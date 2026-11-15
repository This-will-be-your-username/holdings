package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The withdrawal planner answers "I need $X out - what should I sell?".
//
// Priorities, in order (see plan.go for the model behind 1 and 2):
//
//  1. Diversification: end up as close as possible to your target mix. Overweight holdings are
//     sold down first; underweight ones are protected. (With no targets set, your current mix
//     is treated as the target, so the sale is roughly pro-rata.)
//  2. Intent-aware performance: each holding's target is nudged (at most +/-10%) by how it is
//     doing against what it was meant to do, discounted by how far through its intended horizon
//     it is, so long-term holdings aren't marked down for a short patch.
//  3. Tax: within each holding, sell parcels held over 12 months first (50% CGT discount), lowest
//     taxable gain first. If keeping the portfolio balanced would need a parcel under 12 months,
//     it is used only when avoiding it would leave you more than 2.5 points further from target.
//
// A second strategy, "tax", ignores balance and sells whatever costs least tax, for comparison.

const driftTolerance = 0.025 // how much extra imbalance is accepted to avoid selling a young parcel

type WithdrawOpts struct {
	Target    float64         // AUD to receive (after brokerage; after tax too if AfterTax)
	AfterTax  bool            // true: the amount is what you want left after estimated CGT
	Brokerage float64         // AUD per holding sold
	Date      string          // sale date
	Strategy  string          // balance | tax
	Only      map[string]bool // restrict sales to these holdings (empty = all)
}

var strategyLabels = map[string]string{
	"balance": "Keep my portfolio balanced",
	"tax":     "Lowest tax overall",
}

type PlanPick struct {
	Symbol                string
	Seq                   int
	Units                 float64
	Acquired              string
	Days                  int
	Eligible              bool
	CostPU, PricePU, Gain float64 // AUD per unit (Gain = PricePU - CostPU)
	Reason                string
}

// AllocRow is one holding's place in the portfolio before and after the sale.
type AllocRow struct {
	HoldInfo
	Excluded                bool    // not available to sell from
	Sell                    float64 // gross AUD planned
	After                   float64 // share of the portfolio afterwards
	DriftBefore, DriftAfter float64 // percentage points away from the (intent-adjusted) target
}

type Plan struct {
	Strategy, Label               string
	Picks                         []PlanPick
	Reqs                          []SaleReq
	Gross, Fees                   float64
	Alloc                         []AllocRow
	MaxDriftBefore, MaxDriftAfter float64 // percentage points
	UsedIneligible                bool
	NoTargets                     bool
	Notes                         []string
}

type ParcelRow struct {
	Event  CGTEvent
	Reason string
}

type PlanEval struct {
	Key, Label   string
	Plan         *Plan
	Est          *Estimate
	Rows         []ParcelRow
	Raised       float64 // net proceeds
	Tax          float64 // estimated extra tax
	AfterTax     float64 // net proceeds less tax
	Selected     bool
	Best         bool // lowest tax of the plans
	MostBalanced bool // smallest drift from target afterwards
	Converged    bool
	HoldingsHit  int
}

type WithdrawResult struct {
	Opts    WithdrawOpts
	Plans   []*PlanEval
	Sel     *PlanEval
	Weights *Weights
	Notes   []string
}

type cpar struct {
	h                         *Holding
	p                         *parcel
	pu, costPU, gainPU, taxPU float64
	tpd                       float64 // taxable gain per $ of proceeds
	elig                      bool
	days                      int
}

type holdCands struct {
	h          *Holding
	pu         float64
	elig, inel []*cpar
	eligV      float64 // value of parcels held over 12 months
	totV       float64
	allowed    bool
}

func byTax(in []*cpar) []*cpar {
	s := append([]*cpar(nil), in...)
	sort.SliceStable(s, func(i, j int) bool {
		if math.Abs(s[i].tpd-s[j].tpd) > 1e-12 {
			return s[i].tpd < s[j].tpd
		}
		return s[i].p.acq.Before(s[j].p.acq)
	})
	return s
}

func unitsFor(c *cpar, dollars float64) float64 {
	need := dollars / c.pu
	var u float64
	if c.h.Market == MarketManaged {
		u = math.Ceil(need*1e4-1e-6) / 1e4
	} else {
		u = math.Ceil(need - 1e-9) // listed securities trade in whole units
	}
	return math.Min(u, c.p.units)
}

func collectCands(base *Result, parcels map[string][]*parcel, o WithdrawOpts, saleDate time.Time) (map[string]*holdCands, []string, []string) {
	hcs := map[string]*holdCands{}
	var order, notes []string
	for _, h := range base.Holdings {
		hc := &holdCands{h: h, allowed: len(o.Only) == 0 || o.Only[h.Symbol]}
		hcs[h.Symbol] = hc
		order = append(order, h.Symbol)
		if h.Private { // illiquid: can't be sold on demand, so never suggested
			if hc.allowed && len(o.Only) > 0 {
				notes = append(notes, fmt.Sprintf("%s is a private holding and can't be sold on demand, so it was left out", short(h.Symbol)))
			}
			hc.allowed = false
			continue
		}
		hc.pu = h.Price * h.FXNow
		if hc.pu <= 0 {
			if hc.allowed {
				notes = append(notes, fmt.Sprintf("%s has no usable price, so it was left out", short(h.Symbol)))
			}
			hc.allowed = false
			continue
		}
		if !hc.allowed {
			continue
		}
		for _, p := range parcels[h.Symbol] {
			if p.units <= 1e-9 {
				continue
			}
			c := &cpar{h: h, p: p, pu: hc.pu, costPU: p.cost / p.units}
			c.gainPU = hc.pu - c.costPU
			c.elig = discountEligible(p.acq, saleDate)
			c.taxPU = c.gainPU
			if c.gainPU > 0 && c.elig {
				c.taxPU = c.gainPU / 2
			}
			c.tpd = c.taxPU / hc.pu
			c.days = int(saleDate.Sub(p.acq).Hours() / 24)
			hc.totV += p.units * hc.pu
			if c.elig {
				hc.eligV += p.units * hc.pu
				hc.elig = append(hc.elig, c)
			} else {
				hc.inel = append(hc.inel, c)
			}
		}
	}
	return hcs, order, notes
}

func (p *Plan) addPick(c *cpar, u float64, reason string, young bool) {
	p.Gross += u * c.pu
	if young {
		p.UsedIneligible = true
	}
	p.Picks = append(p.Picks, PlanPick{Symbol: c.h.Symbol, Seq: c.p.seq, Units: u, Acquired: c.p.acqS, Days: c.days, Eligible: c.elig,
		CostPU: c.costPU, PricePU: c.pu, Gain: c.gainPU, Reason: reason})
}

func parcelText(c *cpar) string {
	age := fmt.Sprintf("%d-month-old", c.days/30)
	if c.days < 60 {
		age = fmt.Sprintf("%d-day-old", c.days)
	}
	switch {
	case c.gainPU < 0:
		return age + " parcel, sold at a loss, so no tax and the loss can offset other gains"
	case c.elig:
		return age + " parcel, 50% CGT discount applies"
	}
	return age + " parcel, under 12 months old so the full gain is taxed"
}

func balanceReason(hi *HoldInfo, c *cpar, young, first bool) string {
	if !first { // the holding-level explanation is given once, on its first parcel
		if young {
			return fmt.Sprintf("%s; used because parcels over 12 months couldn't cover the amount without leaving the portfolio less balanced.", parcelText(c))
		}
		return fmt.Sprintf("%s; next-lowest taxable gain in this holding.", parcelText(c))
	}
	drift := (hi.Cur - hi.Eff) * 100
	var lead string
	switch {
	case drift > 0.5:
		lead = fmt.Sprintf("Overweight: %.1f%% of your portfolio against a %.1f%% target (%+.1f pts), so selling it moves you toward balance.", hi.Cur*100, hi.Eff*100, drift)
	case drift < -0.5:
		lead = fmt.Sprintf("Underweight (%.1f%% vs %.1f%% target), but needed so the full amount is raised without leaving other holdings further from target.", hi.Cur*100, hi.Eff*100)
	default:
		lead = fmt.Sprintf("Close to its %.1f%% target; a modest amount is taken to cover the withdrawal while staying balanced.", hi.Eff*100)
	}
	if hi.IntentSet && hi.HasXIRR && math.Abs(hi.Gap) >= 0.25 {
		lead += fmt.Sprintf(" It has returned %.1f%% a year against its %.1f%% goal, but that counts at only %.0f%% weight (%.1f years into a %.0f-year horizon).",
			hi.XIRR, hi.Expected, hi.Conf*100, hi.Years, hi.Horizon)
	}
	tail := parcelText(c)
	if young {
		return fmt.Sprintf("%s %s; used because parcels over 12 months couldn't cover the amount without leaving the portfolio less balanced.", lead, tail)
	}
	return fmt.Sprintf("%s %s; lowest taxable gain in this holding.", lead, tail)
}

// planTax: global lowest-tax-per-dollar order, ignoring balance.
func planTax(plan *Plan, hcs map[string]*holdCands, order []string, o WithdrawOpts, target float64) error {
	var elig, inel []*cpar
	var avail float64
	for _, sym := range order {
		hc := hcs[sym]
		elig, inel = append(elig, hc.elig...), append(inel, hc.inel...)
		avail += hc.totV
	}
	elig, inel = byTax(elig), byTax(inel)
	seq := append(append([]*cpar(nil), elig...), inel...)

	remaining := target
	started := map[string]bool{}
	for i, c := range seq {
		if remaining <= 0.005 {
			break
		}
		sym := c.h.Symbol
		if !started[sym] {
			started[sym] = true
			remaining += o.Brokerage
			plan.Fees += o.Brokerage
		}
		u := unitsFor(c, remaining)
		if u <= 0 {
			continue
		}
		remaining -= u * c.pu
		young := i >= len(elig)
		reason := fmt.Sprintf("%s. Lowest taxable gain per dollar raised across your holdings.", parcelText(c))
		if young {
			reason = fmt.Sprintf("%s. Used because parcels over 12 months didn't cover the amount; lowest tax per dollar of those left.", parcelText(c))
		}
		plan.addPick(c, u, reason, young)
	}
	if remaining > 0.005 {
		return fmt.Errorf("the selected holdings can only raise about $%s in total (before tax) - less than the $%s you asked for",
			commas(math.Max(avail-o.Brokerage*float64(len(started)), 0), 0), commas(target, 0))
	}
	return nil
}

// planBalance: decide how much to take from each holding to land closest to the target mix,
// then choose the parcels inside each holding for tax.
func planBalance(plan *Plan, hcs map[string]*holdCands, order []string, w *Weights, o WithdrawOpts, target float64) error {
	n := len(order)
	v, t, capAll, capEl := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	var avail float64
	allowed := 0
	for i, sym := range order {
		hi, hc := w.Info[sym], hcs[sym]
		v[i], t[i] = hi.Value, hi.Eff
		if hc.allowed {
			capAll[i], capEl[i] = hc.totV, hc.eligV
			avail += hc.totV
			allowed++
		}
	}
	fail := func() error {
		return fmt.Errorf("the selected holdings can only raise about $%s in total (before tax) - less than the $%s you asked for",
			commas(math.Max(avail-o.Brokerage*float64(allowed), 0), 0), commas(target, 0))
	}

	goal := target
	for attempt := 0; attempt < 4; attempt++ {
		nSold := 1
		var s []float64
		for iter := 0; iter < 8; iter++ { // brokerage depends on how many holdings are sold, which depends on the amount
			S := goal + float64(nSold)*o.Brokerage
			ideal, ok := solveSells(v, t, capAll, S)
			if !ok {
				return fail()
			}
			s = ideal
			// prefer a plan using only parcels over 12 months if it costs little balance
			if el, ok := solveSells(v, t, capEl, S); ok && maxDrift(v, t, el) <= maxDrift(v, t, ideal)+driftTolerance {
				s = el
			}
			cnt := 0
			for _, x := range s {
				if x > 0.01 {
					cnt++
				}
			}
			if cnt == 0 {
				cnt = 1
			}
			if cnt == nSold {
				break
			}
			nSold = cnt
		}

		plan.Picks, plan.Gross, plan.Fees, plan.UsedIneligible = nil, 0, 0, false
		for i, sym := range order {
			if s[i] <= 0.01 {
				continue
			}
			hc := hcs[sym]
			plan.Fees += o.Brokerage
			rem := s[i]
			seq := append(byTax(hc.elig), byTax(hc.inel)...)
			first := true
			for k, c := range seq {
				if rem <= 0.005 {
					break
				}
				u := unitsFor(c, rem)
				if u <= 0 {
					continue
				}
				rem -= u * c.pu
				young := k >= len(hc.elig)
				plan.addPick(c, u, balanceReason(w.Info[sym], c, young, first), young)
				first = false
			}
		}
		short := target - (plan.Gross - plan.Fees)
		if short <= 0.005 {
			return nil
		}
		goal += short // brokerage count shifted while rounding; ask for a little more
	}
	return nil
}

func finishPlan(plan *Plan, hcs map[string]*holdCands, w *Weights, brokerage float64) {
	idx := map[string]int{}
	sold := map[string]float64{}
	var S float64
	for _, pk := range plan.Picks {
		i, ok := idx[pk.Symbol]
		if !ok {
			i = len(plan.Reqs)
			idx[pk.Symbol] = i
			plan.Reqs = append(plan.Reqs, SaleReq{Symbol: pk.Symbol, Fees: brokerage})
		}
		plan.Reqs[i].Units += pk.Units
		plan.Reqs[i].Picks = append(plan.Reqs[i].Picks, Pick{Seq: pk.Seq, Units: pk.Units})
		sold[pk.Symbol] += pk.Units * pk.PricePU
		S += pk.Units * pk.PricePU
	}
	rest := w.Total - S
	for _, sym := range w.Order {
		hi := w.Info[sym]
		ar := AllocRow{HoldInfo: *hi, Excluded: !hcs[sym].allowed, Sell: sold[sym]}
		if rest > 0 {
			ar.After = (hi.Value - sold[sym]) / rest
		}
		ar.DriftBefore, ar.DriftAfter = (hi.Cur-hi.Eff)*100, (ar.After-hi.Eff)*100
		plan.MaxDriftBefore = math.Max(plan.MaxDriftBefore, math.Abs(ar.DriftBefore))
		plan.MaxDriftAfter = math.Max(plan.MaxDriftAfter, math.Abs(ar.DriftAfter))
		plan.Alloc = append(plan.Alloc, ar)
	}
}

func buildPlan(base *Result, parcels map[string][]*parcel, o WithdrawOpts, strategy string, target float64, w *Weights) (*Plan, error) {
	saleDate, err := time.Parse(dateFmt, o.Date)
	if err != nil {
		return nil, fmt.Errorf("bad sale date")
	}
	plan := &Plan{Strategy: strategy, Label: strategyLabels[strategy], NoTargets: !w.AnySet}
	hcs, order, notes := collectCands(base, parcels, o, saleDate)
	plan.Notes = notes
	if strategy == "tax" {
		err = planTax(plan, hcs, order, o, target)
	} else {
		err = planBalance(plan, hcs, order, w, o, target)
	}
	if err != nil {
		return nil, err
	}
	finishPlan(plan, hcs, w, o.Brokerage)
	return plan, nil
}

// evalStrategy plans and prices one strategy. In "after tax" mode it widens the target
// until the cash left after the estimated tax reaches the amount asked for.
func evalStrategy(db *DB, base *Result, parcels map[string][]*parcel, o WithdrawOpts, strategy string, now time.Time, w *Weights) (*PlanEval, error) {
	target := o.Target
	pe := &PlanEval{Key: strategy, Label: strategyLabels[strategy], Converged: true}
	for iter := 0; iter < 14; iter++ {
		plan, err := buildPlan(base, parcels, o, strategy, target, w)
		if err != nil {
			if o.AfterTax {
				return nil, fmt.Errorf("it isn't possible to end up with $%s after tax from the selected holdings (%v)", commas(o.Target, 0), err)
			}
			return nil, err
		}
		est, err := EstimateSale(db, plan.Reqs, o.Date, "plan", now)
		if err != nil {
			return nil, err
		}
		pe.Plan, pe.Est = plan, est
		pe.Raised, pe.Tax = est.NetProceeds, est.Sel.TaxDelta
		pe.AfterTax = est.NetProceeds - est.Sel.TaxDelta
		if !o.AfterTax {
			break
		}
		short := o.Target - pe.AfterTax
		if short <= 0 {
			break
		}
		target += short + 0.01
		pe.Converged = iter < 13
	}
	// pair each CGT event with the reason its parcel was chosen (events follow pick order per holding)
	reasons := map[string][]string{}
	for _, pk := range pe.Plan.Picks {
		reasons[pk.Symbol] = append(reasons[pk.Symbol], pk.Reason)
	}
	used := map[string]int{}
	for _, e := range pe.Est.Sel.Events {
		r := ""
		if rs := reasons[e.Symbol]; used[e.Symbol] < len(rs) {
			r = rs[used[e.Symbol]]
		}
		used[e.Symbol]++
		pe.Rows = append(pe.Rows, ParcelRow{e, r})
	}
	pe.HoldingsHit = len(pe.Plan.Reqs)
	return pe, nil
}

// WithdrawalPlans builds the chosen strategy's plan and, for comparison, the other one.
func WithdrawalPlans(db *DB, o WithdrawOpts, now time.Time) (*WithdrawResult, error) {
	if o.Target <= 0 {
		return nil, fmt.Errorf("enter the amount you want to withdraw")
	}
	if o.Brokerage < 0 {
		return nil, fmt.Errorf("brokerage can't be negative")
	}
	if o.Date == "" {
		o.Date = now.Format(dateFmt)
	}
	if _, err := time.Parse(dateFmt, o.Date); err != nil {
		return nil, fmt.Errorf("bad sale date")
	}
	if o.Strategy != "tax" {
		o.Strategy = "balance"
	}
	base := Compute(db, now)
	if len(base.Holdings) == 0 {
		return nil, fmt.Errorf("you have no open holdings to sell")
	}
	w := ComputeWeights(db, base)
	_, parcels := cgtReplay(base.Rows, db.Settings.CGTMethod, "", nil)
	res := &WithdrawResult{Opts: o, Weights: w}
	for _, st := range []string{o.Strategy, map[string]string{"balance": "tax", "tax": "balance"}[o.Strategy]} {
		pe, err := evalStrategy(db, base, parcels, o, st, now, w)
		if err != nil {
			if st == o.Strategy {
				return nil, err
			}
			continue
		}
		pe.Selected = st == o.Strategy
		res.Plans = append(res.Plans, pe)
		if pe.Selected {
			res.Sel = pe
		}
	}
	if len(res.Plans) == 2 {
		a, b := res.Plans[0], res.Plans[1]
		switch {
		case a.Tax < b.Tax-0.005:
			a.Best = true
		case b.Tax < a.Tax-0.005:
			b.Best = true
		}
		switch {
		case a.Plan.MaxDriftAfter < b.Plan.MaxDriftAfter-0.05:
			a.MostBalanced = true
		case b.Plan.MaxDriftAfter < a.Plan.MaxDriftAfter-0.05:
			b.MostBalanced = true
		}
	}
	if !w.AnySet {
		res.Notes = append(res.Notes, "You haven't set a target allocation, so your current mix is treated as the target and the sale is spread roughly in proportion. Set your target mix to have the planner rebalance toward it.")
	}
	if res.Sel.Plan.UsedIneligible {
		res.Notes = append(res.Notes, "Keeping the portfolio balanced needed some parcels under 12 months old, which are taxed at the full rate. Selling later, or asking for less, would lower the tax.")
	}
	res.Notes = append(res.Notes, res.Sel.Plan.Notes...)
	if o.AfterTax && !res.Sel.Converged {
		res.Notes = append(res.Notes, "The after-tax amount didn't settle exactly; the figures shown are the closest found.")
	}
	return res, nil
}

// ---------- page ----------

type holdOpt struct {
	Symbol, Name     string
	ReturnPct, Value float64
	Checked          bool
}

func (a *App) raisePage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	var res *Result
	var myIncome float64
	a.st().View(func(db *DB) { res = Compute(db, now); myIncome = db.Settings.OtherIncome })
	bracket := bracketChoice(r, myIncome)

	date := strings.TrimSpace(r.FormValue("date"))
	if d, err := parseDate(date); err == nil {
		date = d
	} else {
		date = now.Format(dateFmt)
	}
	strategy := r.FormValue("strategy")
	if strategy != "tax" {
		strategy = "balance"
	}
	sentOnly := r.FormValue("only_sent") != ""
	only := map[string]bool{}
	for _, s := range r.Form["only"] {
		only[s] = true
	}
	var opts []holdOpt
	for _, h := range res.Holdings {
		opts = append(opts, holdOpt{h.Symbol, h.Name, h.ReturnPct, h.ValueAUD, !sentOnly || only[h.Symbol]})
	}
	data := map[string]any{
		"Mode": "raise", "Holdings": res.Holdings, "HoldOpts": opts, "Date": date, "Strategy": strategy,
		"Amount": strings.TrimSpace(r.FormValue("amount")), "Brokerage": strings.TrimSpace(r.FormValue("brokerage")),
		"AfterTax": r.FormValue("after_tax") == "1",
		"Bracket":  bracket, "MyIncome": myIncome, "TopRate": topMarginalRate(fyOf(date)),
	}
	nl, nlForm := newLawFromForm(r)
	for k, v := range nlForm {
		data[k] = v
	}
	if r.FormValue("run") != "" {
		o := WithdrawOpts{Target: pnum(r.FormValue("amount")), AfterTax: r.FormValue("after_tax") == "1",
			Brokerage: pnum(r.FormValue("brokerage")), Date: date, Strategy: strategy}
		if sentOnly {
			if len(only) == 0 {
				data["Err"] = "select at least one holding to sell from"
				a.render(w, r, "estimator", data)
				return
			}
			o.Only = only
		}
		var wr *WithdrawResult
		var err error
		a.st().View(func(db0 *DB) {
			db := withBracket(db0, bracket)
			wr, err = WithdrawalPlans(db, o, now)
			if err == nil {
				picks := map[string][]Pick{}
				for _, rq := range wr.Sel.Plan.Reqs {
					picks[rq.Symbol] = rq.Picks
				}
				var nerr error
				wr.Sel.Est.NL, nerr = CompareNewLaw(db, wr.Sel.Est, picks, "plan", nl, now)
				if nerr != nil {
					data["NLErr"] = nerr.Error()
				}
			}
		})
		if err != nil {
			data["Err"] = err.Error()
		} else {
			data["Raise"] = wr
		}
	}
	a.render(w, r, "estimator", data)
}
