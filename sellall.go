package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// "Sell everything" answers: if I sold every holding on a day, how much would I keep and how much tax would I owe?
// It prices the sale as if the capital gain landed in the top tax bracket, which is the cautious case: the discount
// still applies to anything held over 12 months, and losses and carried-forward losses are used as usual. It is the
// ordinary sale estimator run on all holdings with the investor's other income set far above the top threshold.

// topBracketIncome is "other income" high enough that every extra dollar is taxed at the top marginal rate.
const topBracketIncome = 10_000_000.0

type SellAllRow struct {
	Symbol, Name                       string
	Private                            bool
	Units, Proceeds, Cost, Long, Short float64 // Long and Short are gains (negative = loss), before the discount
	LongLoss, ShortLoss                float64
	ParcelsLong, ParcelsShort          int
}

type SellAll struct {
	Est                 *Estimate
	FY, Date            string
	Rate                float64 // top marginal rate including Medicare
	Rows                []SellAllRow
	LongGain, ShortGain float64 // gains only, before the discount
	Losses              float64 // capital losses made on this sale (positive number)
	Discount            float64 // 50% discount on long-term gains that were not wiped out by losses
	ExistingLossesUsed  float64
	Taxable             float64 // extra net capital gain added to the year
	Tax, Pocket         float64
	PrivateHeld         []string
	ByOwner             []SellAllOwner // set when the sale spans several owners: each is taxed in their own bracket
	OtherIncomeSet      float64
}

// bracketChoice reads which tax assumption an estimate should use: "mine" (the other income set on the Tax page) or
// "top" (the top marginal rate whatever your income). With no other income recorded the estimate would be taxed as if the
// gain were your only income, which is far too low, so the top rate is the default until an income is set.
func bracketChoice(r *http.Request, anyIncome bool) string {
	if v := r.FormValue("bracket"); v == "mine" || v == "top" {
		return v
	}
	if !anyIncome {
		return "top"
	}
	return "mine"
}

// setTopBracket puts every owner's other income above the top threshold, on a copy.
func setTopBracket(s *DB) {
	s.Settings.OtherIncome = topBracketIncome
	if len(s.Owners) > 0 {
		os := make([]Owner, len(s.Owners))
		copy(os, s.Owners)
		for i := range os {
			os[i].OtherIncome = topBracketIncome
		}
		s.Owners = os
	}
}

// withBracket returns a view of the data to estimate against; for "top" the other income is set above the top
// threshold. Nothing is written back.
func withBracket(db *DB, choice string) *DB {
	if choice != "top" {
		return db
	}
	s := *db
	setTopBracket(&s)
	return &s
}

type SellAllOwner struct {
	Owner                          string
	Proceeds, Tax, Pocket, Taxable float64
}

func topMarginalRate(fy string) float64 {
	return (taxAndMedicare(fy, topBracketIncome+1000) - taxAndMedicare(fy, topBracketIncome)) / 1000
}

func SellAllEstimate(db *DB, date string, now time.Time, nl NewLawAssump) (*SellAll, error) {
	if _, err := time.Parse(dateFmt, date); err != nil {
		return nil, fmt.Errorf("bad sale date")
	}
	s := *db // shallow copy: only the settings change, nothing is written back
	setTopBracket(&s)
	base := Compute(&s, now)
	var reqs []SaleReq
	sa := &SellAll{Date: date, FY: fyOf(date), OtherIncomeSet: topBracketIncome}
	for _, h := range base.Holdings {
		reqs = append(reqs, SaleReq{Symbol: h.Symbol, All: true})
		if h.Private {
			sa.PrivateHeld = append(sa.PrivateHeld, h.Name)
		}
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("you don't hold anything to sell")
	}
	est, err := EstimateSale(&s, reqs, date, s.Settings.CGTMethod, now)
	if err != nil {
		return nil, err
	}
	sa.Est = est
	sa.Rate = topMarginalRate(sa.FY)

	rows := map[string]*SellAllRow{}
	var order []string
	for _, l := range est.Lines {
		rows[l.Symbol] = &SellAllRow{Symbol: l.Symbol, Name: l.Name, Units: l.Units, Proceeds: l.NetAUD}
		order = append(order, l.Symbol)
	}
	for _, e := range est.Sel.Events {
		r := rows[e.Symbol]
		if r == nil {
			continue
		}
		r.Cost += e.Cost
		switch {
		case e.Gain >= 0 && e.Discount:
			r.Long += e.Gain
			r.ParcelsLong++
			sa.LongGain += e.Gain
		case e.Gain >= 0:
			r.Short += e.Gain
			r.ParcelsShort++
			sa.ShortGain += e.Gain
		case e.Discount:
			r.LongLoss += -e.Gain
			sa.Losses += -e.Gain
		default:
			r.ShortLoss += -e.Gain
			sa.Losses += -e.Gain
		}
	}
	for _, sym := range order {
		sa.Rows = append(sa.Rows, *rows[sym])
	}
	sort.SliceStable(sa.Rows, func(i, j int) bool { return sa.Rows[i].Proceeds > sa.Rows[j].Proceeds })
	for i := range sa.Rows {
		if in := db.Instruments[sa.Rows[i].Symbol]; in != nil {
			sa.Rows[i].Private = in.Private
		}
	}
	parts := est.Parts
	if len(parts) == 0 {
		parts = []*Estimate{est}
	}
	for _, p := range parts {
		if p.Scen != nil && p.Base != nil {
			sa.Discount += p.Scen.DiscountApplied - p.Base.DiscountApplied
			if used := p.Base.LossCarriedFwd - p.Scen.LossCarriedFwd; used > 0 { // losses already on record that this sale soaks up
				sa.ExistingLossesUsed += used
			}
		}
	}
	if len(est.Parts) > 1 {
		for _, p := range est.Parts {
			sa.ByOwner = append(sa.ByOwner, SellAllOwner{Owner: p.Label, Proceeds: p.NetProceeds, Tax: p.Sel.TaxDelta, Pocket: p.Sel.AfterTax, Taxable: p.Sel.NetCapGainDelta})
		}
	}
	sa.Taxable = est.Sel.NetCapGainDelta
	sa.Tax = est.Sel.TaxDelta
	sa.Pocket = est.Sel.AfterTax
	_ = CompareNewLawAll(&s, est, nil, est.Sel.Method, nl, now)
	return sa, nil
}

func (a *App) sellAllPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	date := strings.TrimSpace(r.FormValue("date"))
	if date == "" {
		date = now.Format(dateFmt)
	} else if d, err := parseDate(date); err == nil {
		date = d
	}
	var res *Result
	a.st().View(func(db *DB) { res = Compute(db, now) })
	data := map[string]any{"Mode": "all", "Holdings": res.Holdings, "Date": date, "Today": now.Format(dateFmt)}
	nl, nlForm := newLawFromForm(r)
	for k, v := range nlForm {
		data[k] = v
	}
	if len(res.Holdings) > 0 {
		var sa *SellAll
		var err error
		a.st().View(func(db *DB) { sa, err = SellAllEstimate(db, date, now, nl) })
		if err != nil {
			data["Err"] = err.Error()
		} else {
			data["SA"] = sa
		}
	}
	a.render(w, r, "estimator", data)
}
