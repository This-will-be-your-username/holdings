package main

import (
	"math"
	"strings"
	"testing"
	"time"
)

var wNow = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

func addInst(db *DB, sym string, price float64) {
	db.Instruments[sym] = &Instrument{Symbol: sym, Name: sym, Currency: "AUD", Market: MarketASX}
	db.Prices[sym] = map[string]float64{"2026-11-13": price}
}

func setTargets(db *DB, t map[string]float64) {
	for sym, pct := range t {
		db.Instruments[sym].TargetPct = pct
	}
}

// A: 5000, B: 4000, C: 2500 (C has a young parcel and is loss-making). Weights 43.5 / 34.8 / 21.7.
func withdrawDB() *DB {
	db := newTestDB()
	db.Settings.OtherIncome = 100000 // 32% marginal incl. Medicare
	addInst(db, "A.AX", 50)
	addInst(db, "B.AX", 20)
	addInst(db, "C.AX", 10)
	db.Txns = []Txn{
		{Date: "2023-03-01", Symbol: "A.AX", Type: "buy", Units: 100, Price: 30},
		{Date: "2023-01-10", Symbol: "B.AX", Type: "buy", Units: 100, Price: 18},
		{Date: "2023-06-10", Symbol: "B.AX", Type: "buy", Units: 100, Price: 12},
		{Date: "2023-02-01", Symbol: "C.AX", Type: "buy", Units: 100, Price: 14},
		{Date: "2024-01-15", Symbol: "C.AX", Type: "buy", Units: 100, Price: 11},
		{Date: "2026-10-01", Symbol: "C.AX", Type: "buy", Units: 50, Price: 9}, // under 12 months
	}
	return db
}

func picksOf(pe *PlanEval) []string {
	var out []string
	for _, p := range pe.Plan.Picks {
		out = append(out, short(p.Symbol)+"#"+trimFloat(float64(p.Seq))+"x"+trimFloat(p.Units))
	}
	return out
}

func soldBy(pe *PlanEval) map[string]float64 {
	m := map[string]float64{}
	for _, pk := range pe.Plan.Picks {
		m[pk.Symbol] += pk.Units * pk.PricePU
	}
	return m
}

// ---------- the balancing solver ----------

func TestSolveSellsRebalancesTowardTarget(t *testing.T) {
	v := []float64{5000, 4000, 2500}
	tg := []float64{.40, .30, .30}
	caps := []float64{5000, 4000, 2500}
	s, ok := solveSells(v, tg, caps, 1000)
	if !ok {
		t.Fatal("feasible")
	}
	var sum float64
	for _, x := range s {
		sum += x
		if x < -1e-9 {
			t.Errorf("negative sale %v", s)
		}
	}
	if math.Abs(sum-1000) > 1e-6 {
		t.Errorf("sold %v", sum)
	}
	// the underweight third holding is untouched; the two overweight ones are sold down (475 / 525 by hand)
	if s[2] != 0 || math.Abs(s[0]-475) > 0.01 || math.Abs(s[1]-525) > 0.01 {
		t.Errorf("sells %v", s)
	}
	if maxDrift(v, tg, s) >= maxDrift(v, tg, []float64{0, 0, 0}) {
		t.Error("selling should reduce the drift")
	}
}

func TestSolveSellsProRataWhenAlreadyOnTarget(t *testing.T) {
	s, _ := solveSells([]float64{500, 300, 200}, []float64{.5, .3, .2}, []float64{500, 300, 200}, 100)
	if math.Abs(s[0]-50) > 1e-6 || math.Abs(s[1]-30) > 1e-6 || math.Abs(s[2]-20) > 1e-6 {
		t.Errorf("%v", s)
	}
}

func TestSolveSellsRespectsCapsAndInfeasible(t *testing.T) {
	v := []float64{5000, 4000, 2500}
	tg := []float64{.40, .30, .30}
	s, ok := solveSells(v, tg, []float64{300, 4000, 2500}, 1000)
	if !ok || s[0] > 300+1e-9 {
		t.Fatalf("cap ignored: %v", s)
	}
	if sum := s[0] + s[1] + s[2]; math.Abs(sum-1000) > 1e-6 {
		t.Errorf("sum %v", sum)
	}
	if _, ok := solveSells(v, tg, []float64{100, 100, 100}, 1000); ok {
		t.Error("should be infeasible")
	}
	if s, ok := solveSells(v, tg, []float64{1, 1, 1}, 0); !ok || s[0]+s[1]+s[2] != 0 {
		t.Error("zero amount")
	}
}

// ---------- the planner ----------

func TestBalancePlanSellsOverweightAndProtectsUnderweight(t *testing.T) {
	db := withdrawDB()
	setTargets(db, map[string]float64{"A.AX": 40, "B.AX": 30, "C.AX": 30})
	res, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	pe := res.Sel
	if pe.Key != "balance" {
		t.Fatalf("default strategy %s", pe.Key)
	}
	sold := soldBy(pe)
	if sold["C.AX"] != 0 {
		t.Errorf("C is underweight (21.7%% vs 30%%) and must not be sold: %v", sold)
	}
	if sold["A.AX"] == 0 || sold["B.AX"] == 0 {
		t.Errorf("A and B are overweight: %v", sold)
	}
	// 475 / 525 by hand -> whole units: A 10 x 50, B 27 x 20
	if got := strings.Join(picksOf(pe), ","); got != "A#0x10,B#0x27" {
		t.Errorf("picks %s", got)
	}
	if pe.Raised < 1000 || pe.Raised > 1000+50 {
		t.Errorf("raised %v", pe.Raised)
	}
	if pe.Plan.MaxDriftAfter >= pe.Plan.MaxDriftBefore {
		t.Errorf("drift %v -> %v", pe.Plan.MaxDriftBefore, pe.Plan.MaxDriftAfter)
	}
	for _, p := range pe.Plan.Picks {
		if !p.Eligible || !strings.Contains(p.Reason, "Overweight") {
			t.Errorf("pick %+v", p)
		}
	}
	// allocation table accounts for everything
	var afterSum float64
	for _, a := range pe.Plan.Alloc {
		afterSum += a.After
	}
	if len(pe.Plan.Alloc) != 3 || math.Abs(afterSum-1) > 1e-6 {
		t.Errorf("alloc %+v sum %v", pe.Plan.Alloc, afterSum)
	}
}

func TestNoTargetsMeansProRataAndSaysSo(t *testing.T) {
	res, err := WithdrawalPlans(withdrawDB(), WithdrawOpts{Target: 1000}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	sold := soldBy(res.Sel)
	// roughly in proportion to 5000 : 4000 : 2500 (within a unit's rounding)
	for sym, want := range map[string]float64{"A.AX": 435, "B.AX": 348, "C.AX": 217} {
		if math.Abs(sold[sym]-want) > 60 {
			t.Errorf("%s sold %v, want about %v (all %v)", sym, sold[sym], want, sold)
		}
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "haven't set a target") {
			found = true
		}
	}
	if !found || !res.Sel.Plan.NoTargets {
		t.Errorf("notes %v", res.Notes)
	}
}

func TestPartialTargetsAndNormalisation(t *testing.T) {
	db := withdrawDB()
	setTargets(db, map[string]float64{"A.AX": 40, "B.AX": 30}) // C unset takes the rest
	w := ComputeWeights(db, Compute(db, wNow))
	if !w.AnySet || math.Abs(w.Info["C.AX"].Target-0.30) > 1e-9 || math.Abs(w.Info["A.AX"].Target-0.40) > 1e-9 {
		t.Errorf("targets %+v %+v", w.Info["A.AX"], w.Info["C.AX"])
	}
	setTargets(db, map[string]float64{"A.AX": 70, "B.AX": 60}) // over 100: scaled, C gets nothing
	w = ComputeWeights(db, Compute(db, wNow))
	var sum float64
	for _, hi := range w.Info {
		sum += hi.Target
	}
	if math.Abs(sum-1) > 1e-9 || w.Info["C.AX"].Target != 0 || math.Abs(w.Info["A.AX"].Target-70.0/130) > 1e-9 {
		t.Errorf("over-allocated: %+v sum %v", w.Info, sum)
	}
	if math.Abs(w.SumSet-130) > 1e-9 {
		t.Errorf("sum set %v", w.SumSet)
	}
	// nothing set: the current mix is the target
	db2 := withdrawDB()
	w = ComputeWeights(db2, Compute(db2, wNow))
	if w.AnySet || math.Abs(w.Info["A.AX"].Target-w.Info["A.AX"].Cur) > 1e-12 {
		t.Errorf("no targets: %+v", w.Info["A.AX"])
	}
}

func TestBalanceBeatsYoungParcelsUnlessItCostsLittle(t *testing.T) {
	db := withdrawDB()
	// C is hugely overweight (target 1%) but only $2,000 of it is over 12 months old
	setTargets(db, map[string]float64{"A.AX": 49, "B.AX": 50, "C.AX": 1})
	res, err := WithdrawalPlans(db, WithdrawOpts{Target: 3500}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Sel.Plan.UsedIneligible {
		t.Error("avoiding C's young parcel would leave the portfolio ~5 points off target, more than the tolerance: it should be used")
	}
	// the invariant, computed independently
	base := Compute(db, wNow)
	w := ComputeWeights(db, base)
	var v, tg, capAll, capEl []float64
	_, parcels := cgtReplay(base.Rows, db.Settings.CGTMethod, "", nil)
	hcs, order, _ := collectCands(base, parcels, WithdrawOpts{}, wNow)
	for _, sym := range order {
		v, tg = append(v, w.Info[sym].Value), append(tg, w.Info[sym].Eff)
		capAll, capEl = append(capAll, hcs[sym].totV), append(capEl, hcs[sym].eligV)
	}
	ideal, _ := solveSells(v, tg, capAll, 3500)
	el, ok := solveSells(v, tg, capEl, 3500)
	want := !(ok && maxDrift(v, tg, el) <= maxDrift(v, tg, ideal)+driftTolerance)
	if res.Sel.Plan.UsedIneligible != want {
		t.Errorf("UsedIneligible=%v, expected %v", res.Sel.Plan.UsedIneligible, want)
	}
	// a request the over-12-month parcels cover with little imbalance avoids the young parcel
	res, err = WithdrawalPlans(db, WithdrawOpts{Target: 2100}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sel.Plan.UsedIneligible {
		t.Error("a smaller withdrawal should not need young parcels")
	}
}

func TestFallsBackToYoungParcelsWhenNothingElseCoversIt(t *testing.T) {
	res, err := WithdrawalPlans(withdrawDB(), WithdrawOpts{Target: 2300, Only: map[string]bool{"C.AX": true}}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	pe := res.Sel
	if got := strings.Join(picksOf(pe), ","); got != "C#0x100,C#1x100,C#2x30" {
		t.Fatalf("picks %s", got)
	}
	if !pe.Plan.UsedIneligible || pe.Plan.Picks[2].Eligible || !strings.Contains(pe.Plan.Picks[2].Reason, "couldn't cover") {
		t.Errorf("young parcel not flagged: %+v", pe.Plan.Picks[2])
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "12 months") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes: %v", res.Notes)
	}
}

func TestWithinAHoldingLowestTaxParcelsGoFirst(t *testing.T) {
	// only C is sellable: parcels over 12 months, biggest loss first
	res, err := WithdrawalPlans(withdrawDB(), WithdrawOpts{Target: 500, Only: map[string]bool{"C.AX": true}}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(picksOf(res.Sel), ","); got != "C#0x50" {
		t.Fatalf("picks %s", got)
	}
	if !near(res.Sel.Est.Sel.Gain, -200) || res.Sel.Tax > 0.005 {
		t.Errorf("gain %v tax %v", res.Sel.Est.Sel.Gain, res.Sel.Tax)
	}
}

func TestRoundsUpToWholeUnitsAndCoversBrokerage(t *testing.T) {
	db := withdrawDB()
	only := map[string]bool{"C.AX": true}
	res, err := WithdrawalPlans(db, WithdrawOpts{Target: 505, Only: only}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := picksOf(res.Sel); len(got) != 1 || got[0] != "C#0x51" || !near(res.Sel.Raised, 510) {
		t.Errorf("whole units: %v raised %v", got, res.Sel.Raised)
	}
	res, err = WithdrawalPlans(db, WithdrawOpts{Target: 500, Brokerage: 10, Only: only}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := picksOf(res.Sel); len(got) != 1 || got[0] != "C#0x51" || !near(res.Sel.Raised, 500) || !near(res.Sel.Est.Fees, 10) {
		t.Errorf("brokerage: %v raised %v fees %v", got, res.Sel.Raised, res.Sel.Est.Fees)
	}
}

func fgDB() *DB {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	addInst(db, "F.AX", 100)
	addInst(db, "G.AX", 100)
	db.Txns = []Txn{
		{Date: "2023-01-10", Symbol: "F.AX", Type: "buy", Units: 100, Price: 99}, // gain 1/unit
		{Date: "2023-01-10", Symbol: "G.AX", Type: "buy", Units: 100, Price: 1},  // gain 99/unit
	}
	return db
}

func TestBalanceFirstTaxSecond(t *testing.T) {
	db := fgDB()
	setTargets(db, map[string]float64{"F.AX": 70, "G.AX": 30}) // equal values now, so G is the overweight one
	bal, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000, Strategy: "balance"}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(picksOf(bal.Sel), ","); got != "G#0x10" {
		t.Errorf("balance picks %s", got)
	}
	var tax *PlanEval
	for _, p := range bal.Plans {
		if p.Key == "tax" {
			tax = p
		}
	}
	if tax == nil || strings.Join(picksOf(tax), ",") != "F#0x10" {
		t.Fatalf("tax strategy should take the cheap F parcel: %+v", tax)
	}
	// G: gain 990, discounted 495, 32% = 158.40;  F: gain 10 -> 5 -> 1.60
	if !near(bal.Sel.Tax, 158.4) || !near(tax.Tax, 1.6) {
		t.Errorf("taxes %v %v", bal.Sel.Tax, tax.Tax)
	}
	// ...but the tax plan leaves the portfolio further from target
	if tax.Plan.MaxDriftAfter <= bal.Sel.Plan.MaxDriftAfter {
		t.Errorf("drift after: balance %v, tax %v", bal.Sel.Plan.MaxDriftAfter, tax.Plan.MaxDriftAfter)
	}
	if !tax.Best || bal.Sel.Best || !bal.Sel.MostBalanced || tax.MostBalanced {
		t.Errorf("flags: balance best=%v balanced=%v / tax best=%v balanced=%v", bal.Sel.Best, bal.Sel.MostBalanced, tax.Best, tax.MostBalanced)
	}
	// choosing tax as the primary strategy flips the selection
	res, _ := WithdrawalPlans(db, WithdrawOpts{Target: 1000, Strategy: "tax"}, wNow)
	if res.Sel.Key != "tax" || res.Plans[0].Key != "tax" {
		t.Errorf("selected %s", res.Sel.Key)
	}
}

func TestAfterTaxTarget(t *testing.T) {
	db := fgDB()
	delete(db.Instruments, "G.AX")
	db.Txns = db.Txns[:1]
	pre, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Sel.AfterTax >= 1000 { // 10 units, gain 10/unit... tax 1.6; after tax slightly under 1000
		t.Fatalf("before-tax plan should leave less than 1000 after tax: %v", pre.Sel.AfterTax)
	}
	post, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000, AfterTax: true}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if post.Sel.AfterTax < 1000 || post.Sel.Raised < pre.Sel.Raised {
		t.Errorf("after-tax: raised %v, after tax %v", post.Sel.Raised, post.Sel.AfterTax)
	}
	if post.Sel.AfterTax > 1000+100+1 {
		t.Errorf("overshoot too large: %v", post.Sel.AfterTax)
	}
}

func TestWithdrawErrors(t *testing.T) {
	db := withdrawDB()
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 0}, wNow); err == nil {
		t.Error("zero amount")
	}
	for _, st := range []string{"balance", "tax"} {
		if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 1e9, Strategy: st}, wNow); err == nil || !strings.Contains(err.Error(), "can only raise") {
			t.Errorf("%s too much: %v", st, err)
		}
	}
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 100, Date: "nonsense"}, wNow); err == nil {
		t.Error("bad date")
	}
	if _, err := WithdrawalPlans(newTestDB(), WithdrawOpts{Target: 100}, wNow); err == nil {
		t.Error("no holdings")
	}
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 100, Brokerage: -1}, wNow); err == nil {
		t.Error("negative brokerage")
	}
}

func TestWithdrawDoesNotAlterRecords(t *testing.T) {
	db := withdrawDB()
	setTargets(db, map[string]float64{"A.AX": 40, "B.AX": 30, "C.AX": 30})
	n := len(db.Txns)
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 2600}, wNow); err != nil {
		t.Fatal(err)
	}
	if len(db.Txns) != n || db.Settings.EstPicks != nil || db.Settings.EstMethod != "" || db.Instruments["A.AX"].TargetPct != 40 {
		t.Error("planning must not modify the stored data")
	}
}

func TestParcelPicksOverrideMethod(t *testing.T) {
	db := withdrawDB()
	base := Compute(db, wNow)
	rows := append([]Row(nil), base.Rows...)
	rows = append(rows, Row{Txn: Txn{ID: "est-0", Date: "2026-11-13", Symbol: "B.AX", Type: "sell", Units: 10, Price: 20}, GrossAUD: 200, FXUsed: 1})
	// FIFO would use parcel 0 (cost 18); the pick forces parcel 1 (cost 12)
	ev, _ := cgtReplay(rows, "fifo", "fifo", map[string][]Pick{"est-0": {{Seq: 1, Units: 10}}})
	if len(ev) != 1 || !near(ev[0].Cost, 120) || ev[0].Acquired != "2023-06-10" {
		t.Errorf("%+v", ev)
	}
	ev, _ = cgtReplay(rows, "fifo", "fifo", nil)
	if len(ev) != 1 || !near(ev[0].Cost, 180) {
		t.Errorf("fifo without picks: %+v", ev)
	}
	rows[len(rows)-1].Units = 15
	rows[len(rows)-1].GrossAUD = 300
	ev, _ = cgtReplay(rows, "fifo", "fifo", map[string][]Pick{"est-0": {{Seq: 1, Units: 10}}})
	if len(ev) != 2 || ev[0].Acquired != "2023-06-10" || ev[1].Acquired != "2023-01-10" || !near(ev[1].Units, 5) {
		t.Errorf("%+v", ev)
	}
}
