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
// Default strategy ("least"), following the rule:
//  1. Only use parcels owned for more than 12 months (so the 50% CGT discount applies) -
//     unless those can't cover the amount, in which case the shortfall is taken from the
//     remaining parcels with the lowest tax per dollar, and flagged.
//  2. Take from your LEAST profitable holdings first (by total return so far), so winners
//     keep compounding.
//  3. Within a holding, sell the parcels with the LOWEST taxable gain first (losses first,
//     then discounted gains from the highest cost base up), i.e. the parcels that minimise CGT.
//
// The alternative strategy ("tax") ignores how profitable each holding has been and simply
// sells whatever costs least in tax per dollar raised, across all holdings.

type WithdrawOpts struct {
	Target    float64         // AUD to receive (after brokerage; after tax too if AfterTax)
	AfterTax  bool            // true: the amount is what you want left after estimated CGT
	Brokerage float64         // AUD per holding sold
	Date      string          // sale date
	Strategy  string          // least | tax
	Only      map[string]bool // restrict to these holdings (empty = all)
}

var strategyLabels = map[string]string{
	"least": "Least profitable first, lowest-tax parcels",
	"tax":   "Lowest tax overall",
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

type HoldingRank struct {
	Symbol, Name                string
	ReturnPct, Value, EligibleV float64
	Parcels, EligibleParcels    int
	Taken                       float64 // gross proceeds planned from this holding
	Excluded                    bool
	Order                       int // position in the "least profitable first" order, 0 = not eligible
}

type Plan struct {
	Strategy, Label string
	Picks           []PlanPick
	Reqs            []SaleReq
	Gross, Fees     float64
	Ranking         []HoldingRank
	UsedIneligible  bool
	Notes           []string
}

type ParcelRow struct {
	Event  CGTEvent
	Reason string
}

type PlanEval struct {
	Key, Label  string
	Plan        *Plan
	Est         *Estimate
	Rows        []ParcelRow
	Raised      float64 // net proceeds
	Tax         float64 // estimated extra tax
	AfterTax    float64 // net proceeds less tax
	Selected    bool
	Best        bool
	Converged   bool
	HoldingsHit int
}

type WithdrawResult struct {
	Opts  WithdrawOpts
	Plans []*PlanEval
	Sel   *PlanEval
	Notes []string
}

type cpar struct {
	h                         *Holding
	p                         *parcel
	pu, costPU, gainPU, taxPU float64
	tpd                       float64 // taxable gain per $ of proceeds
	elig                      bool
	days                      int
}

func buildPlan(base *Result, parcels map[string][]*parcel, o WithdrawOpts, strategy string, target float64) (*Plan, error) {
	saleDate, err := time.Parse(dateFmt, o.Date)
	if err != nil {
		return nil, fmt.Errorf("bad sale date")
	}
	plan := &Plan{Strategy: strategy, Label: strategyLabels[strategy]}
	var elig, inel []*cpar
	rank := map[string]*HoldingRank{}
	var order []string
	totalAvail := 0.0
	for _, h := range base.Holdings {
		hr := &HoldingRank{Symbol: h.Symbol, Name: h.Name, ReturnPct: h.ReturnPct, Value: h.ValueAUD}
		rank[h.Symbol] = hr
		order = append(order, h.Symbol)
		if len(o.Only) > 0 && !o.Only[h.Symbol] {
			hr.Excluded = true
			continue
		}
		pu := h.Price * h.FXNow
		if pu <= 0 {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s has no usable price, so it was left out", short(h.Symbol)))
			continue
		}
		for _, p := range parcels[h.Symbol] {
			if p.units <= 1e-9 {
				continue
			}
			c := &cpar{h: h, p: p, pu: pu, costPU: p.cost / p.units}
			c.gainPU = pu - c.costPU
			c.elig = discountEligible(p.acq, saleDate)
			c.taxPU = c.gainPU
			if c.gainPU > 0 && c.elig {
				c.taxPU = c.gainPU / 2
			}
			c.tpd = c.taxPU / pu
			c.days = int(saleDate.Sub(p.acq).Hours() / 24)
			hr.Parcels++
			totalAvail += p.units * pu
			if c.elig {
				hr.EligibleParcels++
				hr.EligibleV += p.units * pu
				elig = append(elig, c)
			} else {
				inel = append(inel, c)
			}
		}
	}
	byTax := func(s []*cpar) {
		sort.SliceStable(s, func(i, j int) bool {
			if math.Abs(s[i].tpd-s[j].tpd) > 1e-12 {
				return s[i].tpd < s[j].tpd
			}
			return s[i].p.acq.Before(s[j].p.acq)
		})
	}

	var sequence []*cpar
	switch strategy {
	case "tax":
		byTax(elig)
		sequence = append(sequence, elig...)
	default: // least profitable holding first, then lowest-tax parcels within it
		var hs []*Holding
		seen := map[string]bool{}
		for _, c := range elig {
			if !seen[c.h.Symbol] {
				seen[c.h.Symbol] = true
				hs = append(hs, c.h)
			}
		}
		sort.SliceStable(hs, func(i, j int) bool {
			if math.Abs(hs[i].ReturnPct-hs[j].ReturnPct) > 1e-9 {
				return hs[i].ReturnPct < hs[j].ReturnPct
			}
			return hs[i].Symbol < hs[j].Symbol
		})
		for n, h := range hs {
			rank[h.Symbol].Order = n + 1
			var mine []*cpar
			for _, c := range elig {
				if c.h.Symbol == h.Symbol {
					mine = append(mine, c)
				}
			}
			byTax(mine)
			sequence = append(sequence, mine...)
		}
	}
	nElig := len(sequence)
	byTax(inel)
	sequence = append(sequence, inel...)

	remaining := target
	started := map[string]bool{}
	taken := map[string]float64{}
	for i, c := range sequence {
		if remaining <= 0.005 {
			break
		}
		sym := c.h.Symbol
		if !started[sym] {
			started[sym] = true
			remaining += o.Brokerage
			plan.Fees += o.Brokerage
		}
		need := remaining / c.pu
		var u float64
		if c.h.Market == MarketManaged {
			u = math.Ceil(need*1e4-1e-6) / 1e4
		} else {
			u = math.Ceil(need - 1e-9) // listed securities trade in whole units
		}
		u = math.Min(u, c.p.units)
		if u <= 0 {
			continue
		}
		gross := u * c.pu
		remaining -= gross
		plan.Gross += gross
		taken[sym] += u
		pk := PlanPick{Symbol: sym, Seq: c.p.seq, Units: u, Acquired: c.p.acqS, Days: c.days, Eligible: c.elig,
			CostPU: c.costPU, PricePU: c.pu, Gain: c.gainPU}
		pk.Reason = pickReason(strategy, c, rank[sym], i >= nElig)
		if i >= nElig {
			plan.UsedIneligible = true
		}
		plan.Picks = append(plan.Picks, pk)
	}
	if remaining > 0.005 {
		return nil, fmt.Errorf("the selected holdings can only raise about $%s in total (before tax) - less than the $%s you asked for",
			commas(math.Max(plan.Gross-plan.Fees, 0)+0, 0), commas(target, 0))
	}
	_ = totalAvail

	// requests per holding, in the order first used
	idx := map[string]int{}
	for _, pk := range plan.Picks {
		i, ok := idx[pk.Symbol]
		if !ok {
			i = len(plan.Reqs)
			idx[pk.Symbol] = i
			plan.Reqs = append(plan.Reqs, SaleReq{Symbol: pk.Symbol, Fees: o.Brokerage})
		}
		plan.Reqs[i].Units += pk.Units
		plan.Reqs[i].Picks = append(plan.Reqs[i].Picks, Pick{Seq: pk.Seq, Units: pk.Units})
	}
	for _, sym := range order {
		hr := rank[sym]
		if u := taken[sym]; u > 0 {
			for _, pk := range plan.Picks {
				if pk.Symbol == sym {
					hr.Taken += pk.Units * pk.PricePU
				}
			}
		}
		plan.Ranking = append(plan.Ranking, *hr)
	}
	sort.SliceStable(plan.Ranking, func(i, j int) bool {
		a, b := plan.Ranking[i], plan.Ranking[j]
		if a.Excluded != b.Excluded {
			return !a.Excluded
		}
		return a.ReturnPct < b.ReturnPct
	})
	return plan, nil
}

func pickReason(strategy string, c *cpar, hr *HoldingRank, fallback bool) string {
	months := c.days / 30
	age := fmt.Sprintf("%d-month-old", months)
	if c.days < 60 {
		age = fmt.Sprintf("%d-day-old", c.days)
	}
	var tax string
	switch {
	case c.gainPU < 0:
		tax = "sold at a loss, so no tax and the loss can offset other gains"
	case c.elig:
		tax = "50% CGT discount applies"
	default:
		tax = "under 12 months old, so the full gain is taxed"
	}
	switch {
	case fallback:
		return fmt.Sprintf("%s parcel, %s. Used because parcels over 12 months didn't cover the amount; lowest tax per dollar of those left.", age, tax)
	case strategy == "tax":
		return fmt.Sprintf("%s parcel, %s. Lowest taxable gain per dollar raised across your holdings.", age, tax)
	default:
		return fmt.Sprintf("#%d least profitable holding (%+.1f%% return so far). %s parcel, %s; lowest taxable gain in this holding.",
			hr.Order, hr.ReturnPct, age, tax)
	}
}

// evalStrategy plans and prices one strategy. In "after tax" mode it widens the target
// until the cash left after the estimated tax reaches the amount asked for.
func evalStrategy(db *DB, base *Result, parcels map[string][]*parcel, o WithdrawOpts, strategy string, now time.Time) (*PlanEval, error) {
	target := o.Target
	pe := &PlanEval{Key: strategy, Label: strategyLabels[strategy], Converged: true}
	for iter := 0; iter < 14; iter++ {
		plan, err := buildPlan(base, parcels, o, strategy, target)
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
		o.Strategy = "least"
	}
	base := Compute(db, now)
	if len(base.Holdings) == 0 {
		return nil, fmt.Errorf("you have no open holdings to sell")
	}
	_, parcels := cgtReplay(base.Rows, db.Settings.CGTMethod, "", nil)
	res := &WithdrawResult{Opts: o}
	var firstErr error
	for _, st := range []string{o.Strategy, map[string]string{"least": "tax", "tax": "least"}[o.Strategy]} {
		pe, err := evalStrategy(db, base, parcels, o, st, now)
		if err != nil {
			if st == o.Strategy {
				return nil, err
			}
			firstErr = err
			continue
		}
		pe.Selected = st == o.Strategy
		res.Plans = append(res.Plans, pe)
		if pe.Selected {
			res.Sel = pe
		}
	}
	_ = firstErr
	if len(res.Plans) == 2 {
		a, b := res.Plans[0], res.Plans[1]
		switch {
		case a.Tax < b.Tax-0.005:
			a.Best = true
		case b.Tax < a.Tax-0.005:
			b.Best = true
		}
	}
	if res.Sel.Plan.UsedIneligible {
		res.Notes = append(res.Notes, "Parcels held over 12 months couldn't cover this amount, so some parcels under 12 months old are included and taxed at the full rate. Selling later, or asking for less, would lower the tax.")
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
	a.store.View(func(db *DB) { res = Compute(db, now) })

	date := strings.TrimSpace(r.FormValue("date"))
	if d, err := parseDate(date); err == nil {
		date = d
	} else {
		date = now.Format(dateFmt)
	}
	strategy := r.FormValue("strategy")
	if strategy != "tax" {
		strategy = "least"
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
		a.store.View(func(db *DB) { wr, err = WithdrawalPlans(db, o, now) })
		if err != nil {
			data["Err"] = err.Error()
		} else {
			data["Raise"] = wr
		}
	}
	a.render(w, r, "estimator", data)
}
