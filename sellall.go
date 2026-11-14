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
	OtherIncomeSet      float64
}

func topMarginalRate(fy string) float64 {
	return (taxAndMedicare(fy, topBracketIncome+1000) - taxAndMedicare(fy, topBracketIncome)) / 1000
}

func SellAllEstimate(db *DB, date string, now time.Time, nl NewLawAssump) (*SellAll, error) {
	if _, err := time.Parse(dateFmt, date); err != nil {
		return nil, fmt.Errorf("bad sale date")
	}
	s := *db // shallow copy: only the settings change, nothing is written back
	s.Settings.OtherIncome = topBracketIncome
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
	if est.Scen != nil && est.Base != nil {
		sa.Discount = est.Scen.DiscountApplied - est.Base.DiscountApplied
		sa.ExistingLossesUsed = est.Base.LossCarriedFwd - est.Scen.LossCarriedFwd // losses already on record that this sale soaks up
		if sa.ExistingLossesUsed < 0 {
			sa.ExistingLossesUsed = 0
		}
	}
	sa.Taxable = est.Sel.NetCapGainDelta
	sa.Tax = est.Sel.TaxDelta
	sa.Pocket = est.Sel.AfterTax
	if nlv, nerr := CompareNewLaw(&s, est, nil, est.Sel.Method, nl, now); nerr == nil {
		est.NL = nlv
	}
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
