package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Portfolio plan: what each holding is *for*.
//
// Every holding can carry a role (long-term growth, balanced, defensive, short-term), a
// target share of the portfolio, a time horizon and an expected return. The withdrawal
// planner uses them in this order of priority:
//
//  1. DIVERSIFICATION - end up as close to the target mix as possible: sell what is overweight.
//  2. INTENT-AWARE PERFORMANCE - judge each holding against what it was meant to do, and
//     discount that verdict by how far through its intended horizon it is. A 10-year growth
//     holding that is two years in is judged at 20% weight, so a slow patch isn't punished.
//     A short-term holding that has had its full horizon is judged in full. The judgement only
//     nudges the target weight (at most +/-10% of it), so it never overrides balance.
//  3. TAX - within the holdings chosen, use parcels over 12 months old first, lowest tax first.

type RoleDef struct {
	Key, Label string
	Expected   float64 // default expected return, % a year
	Horizon    float64 // default intended holding period, years
}

var roles = []RoleDef{
	{"growth", "Long-term growth", 8.0, 10},
	{"balanced", "Balanced / diversified", 6.5, 7},
	{"defensive", "Defensive / income", 4.0, 3},
	{"short", "Short-term / liquidity", 3.0, 1},
}

func roleDef(key string) (RoleDef, bool) {
	for _, r := range roles {
		if r.Key == key {
			return r, true
		}
	}
	return RoleDef{}, false
}

type Intent struct {
	Set               bool
	Expected, Horizon float64
}

// intentOf is what the investor expects of this holding. No role and no expected return
// means no intention was stated, and the holding is never marked down for performance.
func intentOf(inst *Instrument) Intent {
	if inst == nil {
		return Intent{}
	}
	rd, hasRole := roleDef(inst.Role)
	if !hasRole && inst.ExpectedPct <= 0 {
		return Intent{}
	}
	in := Intent{Set: true, Expected: inst.ExpectedPct, Horizon: inst.HorizonYrs}
	if in.Expected <= 0 {
		if hasRole {
			in.Expected = rd.Expected
		} else {
			in.Expected = 6.5
		}
	}
	if in.Horizon <= 0 {
		if hasRole {
			in.Horizon = rd.Horizon
		} else {
			in.Horizon = 7
		}
	}
	return in
}

const (
	tiltPerPoint = 0.01 // relative change in target weight per percentage point of intent-adjusted gap
	tiltCap      = 0.10 // never move a target by more than 10% of itself
)

// intentScore compares a holding's annualised return with what it was meant to deliver.
//
//	gap  = actual return - expected return, in percentage points a year
//	conf = how much that is worth believing: the share of the intended horizon already elapsed (max 1)
//	tilt = gap x conf x 1% per point, capped at +/-10%: negative -> hold a little less than target
func intentScore(xirr float64, hasXIRR bool, yearsHeld float64, in Intent) (gap, conf, tilt float64) {
	if !in.Set || !hasXIRR {
		return 0, 0, 0
	}
	gap = xirr - in.Expected
	conf = math.Min(1, math.Max(0, yearsHeld)/math.Max(in.Horizon, 0.25))
	tilt = clamp(gap*conf*tiltPerPoint, -tiltCap, tiltCap)
	return
}

type HoldInfo struct {
	Symbol, Name, Role, RoleLabel string
	Value                         float64
	Cur, Target, Eff              float64 // fractions of the portfolio
	TargetSet, IntentSet, HasXIRR bool
	Expected, Horizon, Years      float64
	XIRR, Gap, Conf, Tilt         float64
}

type Weights struct {
	Info   map[string]*HoldInfo
	Order  []string // symbols, largest holding first
	Total  float64
	AnySet bool    // the investor has set at least one target
	SumSet float64 // percent
}

// ComputeWeights works out current, target and intent-adjusted weights for every open holding.
//   - targets the investor has set are used (unset holdings share what's left, by size);
//   - with no targets at all the current mix is the target, i.e. no rebalancing pressure;
//   - the intent tilt is applied last and everything is renormalised to 100%.
func ComputeWeights(db *DB, base *Result) *Weights {
	w := &Weights{Info: map[string]*HoldInfo{}}
	for _, h := range base.Holdings {
		w.Total += h.ValueAUD
	}
	var sumSet, unsetVal float64
	for _, h := range base.Holdings {
		inst := db.Instruments[h.Symbol]
		hi := &HoldInfo{Symbol: h.Symbol, Name: h.Name, Value: h.ValueAUD, Years: h.YearsHeld, XIRR: h.XIRR, HasXIRR: h.HasXIRR}
		if w.Total > 0 {
			hi.Cur = h.ValueAUD / w.Total
		}
		if inst != nil {
			hi.Role = inst.Role
			if rd, ok := roleDef(inst.Role); ok {
				hi.RoleLabel = rd.Label
			}
			if inst.TargetPct > 0 {
				hi.TargetSet, hi.Target = true, inst.TargetPct/100
				sumSet += hi.Target
			}
		}
		if !hi.TargetSet {
			unsetVal += h.ValueAUD
		}
		w.Info[h.Symbol] = hi
		w.Order = append(w.Order, h.Symbol)
	}
	sort.SliceStable(w.Order, func(i, j int) bool { return w.Info[w.Order[i]].Value > w.Info[w.Order[j]].Value })
	w.AnySet, w.SumSet = sumSet > 0, sumSet*100

	if !w.AnySet {
		for _, hi := range w.Info {
			hi.Target = hi.Cur
		}
	} else {
		remain := math.Max(0, 1-sumSet)
		for _, hi := range w.Info {
			if !hi.TargetSet && unsetVal > 0 {
				hi.Target = remain * hi.Value / unsetVal
			}
		}
		normalise(w, func(h *HoldInfo) *float64 { return &h.Target })
	}

	for _, h := range base.Holdings {
		hi := w.Info[h.Symbol]
		in := intentOf(db.Instruments[h.Symbol])
		hi.IntentSet, hi.Expected, hi.Horizon = in.Set, in.Expected, in.Horizon
		hi.Gap, hi.Conf, hi.Tilt = intentScore(h.XIRR, h.HasXIRR, h.YearsHeld, in)
		hi.Eff = hi.Target * (1 + hi.Tilt)
	}
	normalise(w, func(h *HoldInfo) *float64 { return &h.Eff })
	return w
}

func normalise(w *Weights, get func(*HoldInfo) *float64) {
	var sum float64
	for _, hi := range w.Info {
		sum += *get(hi)
	}
	if sum <= 0 {
		return
	}
	for _, hi := range w.Info {
		*get(hi) /= sum
	}
}

// solveSells finds how much (AUD) to sell from each holding to raise S so the portfolio ends
// as close as possible to the target weights t (least squares on the post-sale weights).
//
// With p_i = v_i - s_i and the post-sale total fixed at V-S, minimising sum (p_i - t_i(V-S))^2
// subject to 0 <= s_i <= cap_i and sum s_i = S gives s_i = clamp(v_i - t_i(V-S) - c, 0, cap_i)
// for one shared shift c, found by bisection: overweight holdings are sold down first, and the
// rest of the amount is spread so nothing ends up further from target than it has to.
// ok=false if the caps can't cover S.
func solveSells(v, t, caps []float64, S float64) (s []float64, ok bool) {
	var V, capSum float64
	for i := range v {
		V += v[i]
		capSum += caps[i]
	}
	if S <= 0 {
		return make([]float64, len(v)), true
	}
	if capSum < S-1e-9 {
		return nil, false
	}
	rest := math.Max(V-S, 0)
	sells := func(c float64) ([]float64, float64) {
		out := make([]float64, len(v))
		var sum float64
		for i := range v {
			out[i] = clamp(v[i]-t[i]*rest-c, 0, caps[i])
			sum += out[i]
		}
		return out, sum
	}
	lo, hi := -2*V-S, 2*V+S
	for it := 0; it < 200; it++ {
		mid := (lo + hi) / 2
		if _, sum := sells(mid); sum > S {
			lo = mid
		} else {
			hi = mid
		}
	}
	out, sum := sells((lo + hi) / 2)
	if sum > 0 && math.Abs(sum-S) > 1e-6 { // remove bisection residue
		k := S / sum
		for i := range out {
			out[i] = math.Min(out[i]*k, caps[i])
		}
	}
	return out, true
}

// maxDrift is the largest gap (fraction of the portfolio) between any holding's post-sale
// weight and its target.
func maxDrift(v, t, s []float64) float64 {
	var V, S float64
	for i := range v {
		V += v[i]
		S += s[i]
	}
	rest := V - S
	if rest <= 0 {
		return 0
	}
	var m float64
	for i := range v {
		m = math.Max(m, math.Abs((v[i]-s[i])/rest-t[i]))
	}
	return m
}

// ---------- the allocation page ----------

type planRow struct {
	HoldInfo
	ExpectedShown, HorizonShown float64 // what's in force (role default when not overridden)
	ExpectedSet, HorizonSet     bool
}

func (a *App) planPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	var rows []planRow
	var wts *Weights
	a.st().View(func(db *DB) {
		base := Compute(db, now)
		wts = ComputeWeights(db, base)
		for _, sym := range wts.Order {
			hi := wts.Info[sym]
			pr := planRow{HoldInfo: *hi, ExpectedShown: hi.Expected, HorizonShown: hi.Horizon}
			if inst := db.Instruments[sym]; inst != nil {
				pr.ExpectedSet, pr.HorizonSet = inst.ExpectedPct > 0, inst.HorizonYrs > 0
				pr.Target = inst.TargetPct / 100 // show what the user typed, not the implied value
				pr.TargetSet = inst.TargetPct > 0
				if !hi.IntentSet {
					pr.ExpectedShown, pr.HorizonShown = 0, 0
				}
			}
			rows = append(rows, pr)
		}
	})
	a.render(w, r, "plan", map[string]any{"Rows": rows, "W": wts, "Roles": roles})
}

func (a *App) savePlan(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	type edit struct {
		role                      string
		target, horizon, expected float64
	}
	edits := map[string]edit{}
	for _, sym := range r.Form["sym"] {
		e := edit{role: r.FormValue("role_" + sym)}
		if _, ok := roleDef(e.role); !ok {
			e.role = ""
		}
		num := func(k string) float64 { return pnum(r.FormValue(k + "_" + sym)) }
		e.target, e.horizon, e.expected = num("target"), num("horizon"), num("expected")
		if e.target < 0 || e.target > 100 || e.horizon < 0 || e.horizon > 80 || e.expected < 0 || e.expected > 60 {
			redirect(w, r, "/plan", "", fmt.Sprintf("%s: target must be 0-100%%, horizon 0-80 years, expected return 0-60%%", strings.TrimSuffix(sym, ".AX")))
			return
		}
		edits[sym] = e
	}
	var sum float64
	err := a.st().Update(func(db *DB) error {
		for sym, e := range edits {
			inst := db.Instruments[sym]
			if inst == nil {
				continue
			}
			inst.Role, inst.TargetPct, inst.HorizonYrs, inst.ExpectedPct = e.role, e.target, e.horizon, e.expected
			sum += e.target
		}
		return nil
	})
	if err != nil {
		redirect(w, r, "/plan", "", err.Error())
		return
	}
	msg := "Target allocation saved"
	if sum > 0 && math.Abs(sum-100) > 0.5 {
		msg += fmt.Sprintf(" — your targets add up to %s%%; the planner scales them to 100%%", strings.TrimRight(strings.TrimRight(strconv.FormatFloat(sum, 'f', 1, 64), "0"), "."))
	}
	redirect(w, r, "/plan", msg, "")
}
