package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// The CGT reform from 1 July 2027 (Treasury Laws Amendment (Tax Reform No. 1) Act 2026,
// Royal Assent 26 June 2026; more tranches are still being made), as modelled here for an
// Australian-resident individual holding listed investments:
//
//   - A holder at 30 June 2027 is treated as re-acquiring each asset at its market value just
//     before 1 July 2027. The gain that built up to then is DEFERRED until sale and keeps the
//     50% discount (if the asset is owned more than 12 months). This estimator assumes that
//     market value is TODAY'S price, i.e. "the new cost base sits at the current value".
//   - Growth after 1 July 2027 is taxed on a CPI-INDEXED basis (the cost base rises with
//     inflation; indexation can't create or increase a loss) with no discount.
//   - A 30% MINIMUM TAX applies to that post-July-2027 gain: it is a floor, topped up where
//     the tax at your marginal rate would be lower.
//   - Capital losses are applied to the deferred (pre-change) gains first, then to the new gains.
//
// Where the legislation or ATO guidance has not been finalised the assumptions are shown to the
// user and can be changed (sale date, growth after 1 July 2027, CPI). Not modelled: the exemption
// from the minimum tax for income-support recipients, trusts, and a valuation by the apportionment
// method (which is not available for listed shares).

const (
	newRegimeStart = "2027-07-01"
	minTaxRate     = 0.30
)

type NewLawAssump struct {
	SaleDate string  // when the sale happens (on or after 1 July 2027)
	GrowthPA float64 // price growth after 1 July 2027, % a year
	CPI      float64 // inflation used for cost-base indexation, % a year
}

func DefaultNewLaw() NewLawAssump {
	return NewLawAssump{SaleDate: newRegimeStart, GrowthPA: 0, CPI: 2.5}
}

type NLParcel struct {
	Symbol, Acquired          string
	Units, Cost, V0, Proceeds float64
	Deferred                  float64 // gain (loss) that accrued up to 1 July 2027: V0 - cost base
	PostNominal               float64 // growth after 1 July 2027 before indexation
	PostIndexed               float64 // ...after indexation (>= 0 when there is growth)
	Held12                    bool    // owned more than 12 months at the sale
}

type NewLawResult struct {
	A                            NewLawAssump
	FY                           string
	Factor, PriceFactor          float64 // indexation factor and the price growth applied
	Est                          *Estimate
	Parcels                      []NLParcel
	DeferredPre                  float64 // pre-change gain before any losses
	DeferredGross                float64 // pre-change gain after losses, before the discount
	Discount                     float64 // 50% discount on the discount-eligible deferred gain
	DeferredTaxable              float64
	PostNominal, IndexRelief     float64
	PostGain                     float64 // post-change gain after indexation and losses
	LossesUsed, LossCarried      float64
	TotalGain                    float64 // proceeds less original cost base, all parcels
	MarginalTax, TopUp, TaxDelta float64
	AfterTax                     float64
}

// NewLawCompute prices the sale in est (already run at the post-change sale date and price) under
// the new rules. v0 is each holding's AUD value per unit that is assumed to be its 1 July 2027 value.
func NewLawCompute(est *Estimate, v0 map[string]float64, a NewLawAssump) *NewLawResult {
	r := &NewLawResult{A: a, FY: est.FY, Est: est, PriceFactor: 1}
	sale, _ := time.Parse(dateFmt, a.SaleDate)
	start, _ := time.Parse(dateFmt, newRegimeStart)
	years := math.Max(0, sale.Sub(start).Hours()/24/365.25)
	r.Factor = math.Pow(1+a.CPI/100, years)
	r.PriceFactor = math.Pow(1+a.GrowthPA/100, years)

	var dElig, dPlain, dLoss, postGain, postLoss float64
	for _, e := range est.Sel.Events {
		if e.Kind != "sale" {
			continue
		}
		u0 := v0[e.Symbol] * e.Units
		pre := u0 - e.Cost
		nom := e.Proceeds - u0
		post := nom
		if nom > 0 && e.Discount { // owned over 12 months: index the reset cost base
			post = math.Max(0, e.Proceeds-u0*r.Factor)
			r.IndexRelief += nom - post
		}
		r.PostNominal += nom
		r.TotalGain += e.Proceeds - e.Cost
		r.Parcels = append(r.Parcels, NLParcel{Symbol: e.Symbol, Acquired: e.Acquired, Units: e.Units, Cost: e.Cost, V0: u0,
			Proceeds: e.Proceeds, Deferred: pre, PostNominal: nom, PostIndexed: post, Held12: e.Discount})
		switch {
		case pre > 0 && e.Discount:
			dElig += pre
		case pre > 0:
			dPlain += pre
		default:
			dLoss += -pre
		}
		if post > 0 {
			postGain += post
		} else {
			postLoss += -post
		}
	}

	// losses: this sale's losses plus anything already carried into the year, applied to the
	// deferred gains first (so the choice that protects the discount no longer exists), then new gains
	pool := dLoss + postLoss + est.Base.LossBroughtFwd + est.Base.Losses
	r.LossesUsed = pool
	use := func(g *float64) {
		u := math.Min(pool, *g)
		*g -= u
		pool -= u
	}
	use(&dPlain)
	use(&dElig)
	use(&postGain)
	r.DeferredPre = dElig + dPlain + 0
	r.LossCarried = pool
	r.LossesUsed -= pool
	r.DeferredGross = dPlain + dElig
	r.Discount = dElig / 2
	r.DeferredTaxable = dPlain + dElig - r.Discount
	r.PostGain = postGain

	// tax: gains stacked on top of your other income, with the 30% floor on the post-change part
	fy := est.FY
	ordinary := est.Base.OtherIncome + est.Base.Franked + est.Base.Unfranked + est.Base.Franking
	total := r.DeferredTaxable + r.PostGain
	r.MarginalTax = taxAndMedicare(fy, ordinary+total) - taxAndMedicare(fy, ordinary)
	incomeOnPost := incomeTax(fy, ordinary+total) - incomeTax(fy, ordinary+r.DeferredTaxable)
	r.TopUp = math.Max(0, minTaxRate*r.PostGain-incomeOnPost)
	r.TaxDelta = r.MarginalTax + r.TopUp
	r.AfterTax = est.NetProceeds - r.TaxDelta
	return r
}

// NewLawView is the pair shown side by side: the sale under today's rules (before 1 July 2027)
// and under the new rules.
type NewLawView struct {
	A            *Estimate // current rules, at the user's sale date
	B            *NewLawResult
	Diff         float64 // new-rules tax minus current-rules tax
	AOutOfRange  bool    // the user's own sale date is already on/after 1 July 2027, so A isn't a current-rules figure
	DefaultAssum bool
}

// CompareNewLaw re-runs the sale (same units, same parcels) at the post-change date and price and
// prices it under the new rules. estA is the current-rules estimate of the same sale.
func CompareNewLaw(db *DB, estA *Estimate, picks map[string][]Pick, selMethod string, a NewLawAssump, now time.Time) (*NewLawView, error) {
	if a.SaleDate == "" {
		a.SaleDate = newRegimeStart
	}
	d, err := time.Parse(dateFmt, a.SaleDate)
	if err != nil {
		return nil, fmt.Errorf("bad post-change sale date")
	}
	if a.SaleDate < newRegimeStart {
		return nil, fmt.Errorf("the new rules apply to sales on or after 1 July 2027")
	}
	if a.CPI < -5 || a.CPI > 30 || a.GrowthPA < -50 || a.GrowthPA > 100 {
		return nil, fmt.Errorf("growth and CPI assumptions look out of range")
	}
	_ = d
	base := Compute(db, now)
	v0 := map[string]float64{}
	for _, h := range base.Holdings {
		v0[h.Symbol] = h.Price * h.FXNow // assumed value at 1 July 2027: today's price
	}
	start, _ := time.Parse(dateFmt, newRegimeStart)
	years := math.Max(0, d.Sub(start).Hours()/24/365.25)
	pf := math.Pow(1+a.GrowthPA/100, years)

	var reqs []SaleReq
	for _, ln := range estA.Lines {
		reqs = append(reqs, SaleReq{Symbol: ln.Symbol, Units: ln.Units, Fees: ln.Fees, Price: ln.Price * pf, Picks: picks[ln.Symbol]})
	}
	estB, err := EstimateSale(db, reqs, a.SaleDate, selMethod, now)
	if err != nil {
		return nil, err
	}
	res := NewLawCompute(estB, v0, a)
	v := &NewLawView{A: estA, B: res}
	v.Diff = res.TaxDelta - estA.Sel.TaxDelta
	v.AOutOfRange = estA.Date >= newRegimeStart
	return v, nil
}

// newLawFromForm reads the "new rules" assumptions from a request (blank = defaults) and returns
// them with the strings to put back in the form.
func newLawFromForm(r *http.Request) (NewLawAssump, map[string]any) {
	a := DefaultNewLaw()
	rawDate, rawGrowth, rawCPI := strings.TrimSpace(r.FormValue("post_date")), strings.TrimSpace(r.FormValue("post_growth")), strings.TrimSpace(r.FormValue("cpi"))
	if d, err := parseDate(rawDate); err == nil {
		a.SaleDate = d
	}
	if rawGrowth != "" {
		a.GrowthPA = pnum(rawGrowth)
	}
	if rawCPI != "" {
		a.CPI = pnum(rawCPI)
	}
	return a, map[string]any{"PostDate": a.SaleDate, "PostGrowth": rawGrowth, "PostCPI": rawCPI}
}
