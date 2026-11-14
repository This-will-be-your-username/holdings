package main

import (
	"math"
	"strings"
	"testing"
)

func sellG(db *DB, units float64) *Estimate {
	est, err := EstimateSale(db, []SaleReq{{Symbol: "G.AX", Units: units}}, "2026-11-13", "fifo", wNow)
	if err != nil {
		panic(err)
	}
	return est
}

func compare(t *testing.T, db *DB, est *Estimate, a NewLawAssump) *NewLawView {
	t.Helper()
	v, err := CompareNewLaw(db, est, nil, "fifo", a, wNow)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNewLawWithNoGrowthMatchesCurrentRules(t *testing.T) {
	db := fgDB() // G: bought 2023 at $1/unit, now $100 -> deferred gain of $99/unit, over 12 months
	est := sellG(db, 10)
	if !near(est.Sel.TaxDelta, 158.4) { // 990 gain, 50% discount, 32% marginal incl. Medicare
		t.Fatalf("current rules: %v", est.Sel.TaxDelta)
	}
	v := compare(t, db, est, DefaultNewLaw())
	b := v.B
	// the gain up to 1 July 2027 keeps the discount and there is no growth since: same tax
	if !near(b.DeferredPre, 990) || !near(b.DeferredGross, 990) || !near(b.Discount, 495) || !near(b.DeferredTaxable, 495) || !near(b.PostGain, 0) || !near(b.TopUp, 0) {
		t.Errorf("%+v", b)
	}
	if !near(b.TaxDelta, est.Sel.TaxDelta) || !near(v.Diff, 0) {
		t.Errorf("new %v vs current %v", b.TaxDelta, est.Sel.TaxDelta)
	}
	if b.FY != "2027-28" || !near(b.AfterTax, est.NetProceeds-b.TaxDelta) || v.AOutOfRange {
		t.Errorf("fy %s after-tax %v", b.FY, b.AfterTax)
	}
	if len(b.Parcels) != 1 || !b.Parcels[0].Held12 || !near(b.Parcels[0].V0, 1000) {
		t.Errorf("parcels %+v", b.Parcels)
	}
}

func TestNewLawIndexesOnlyGrowthAfterTheTransition(t *testing.T) {
	db := fgDB()
	est := sellG(db, 10)
	// a year on, +10% growth: sells at $110/unit = $1,100 against a reset basis of $1,000 indexed at 2.5%
	b := compare(t, db, est, NewLawAssump{SaleDate: "2028-07-01", GrowthPA: 10, CPI: 2.5}).B
	if math.Abs(b.Factor-1.025) > 0.001 || math.Abs(b.PriceFactor-1.10) > 0.001 {
		t.Fatalf("factors %v %v", b.Factor, b.PriceFactor)
	}
	if math.Abs(b.Parcels[0].Proceeds-1100) > 0.5 { // (1 July to 1 July spans a leap day: 366 days)
		t.Fatalf("proceeds %v", b.Parcels[0].Proceeds)
	}
	if math.Abs(b.PostNominal-100) > 0.5 || math.Abs(b.PostGain-75) > 0.5 || math.Abs(b.IndexRelief-25) > 0.5 {
		t.Errorf("post-change growth 100, indexation relief 25, taxable 75: %+v", b)
	}
	if !near(b.DeferredTaxable, 495) {
		t.Errorf("deferred taxable %v", b.DeferredTaxable)
	}
	// 570 taxable at 32% = 182.40; a 100k income already pays 30% so the 30% floor adds nothing
	if math.Abs(b.TaxDelta-182.4) > 0.5 || b.TopUp > 0.005 {
		t.Errorf("tax %v topup %v", b.TaxDelta, b.TopUp)
	}
	// under today's rules the same sale at the same price pays tax on 50% of (1100 - 10): more deferral benefit, so the
	// new regime costs more once real growth is taxed with indexation instead of a discount
	estA := estimateAt(t, db, "G.AX", 10, 110)
	if !(b.TaxDelta > estA.Sel.TaxDelta) {
		t.Errorf("new rules %v should cost more than current rules %v on a rising price", b.TaxDelta, estA.Sel.TaxDelta)
	}
}

func estimateAt(t *testing.T, db *DB, sym string, units, price float64) *Estimate {
	t.Helper()
	est, err := EstimateSale(db, []SaleReq{{Symbol: sym, Units: units, Price: price}}, "2026-11-13", "fifo", wNow)
	if err != nil {
		t.Fatal(err)
	}
	return est
}

func TestMinimumTaxTopsUpLowMarginalRates(t *testing.T) {
	db := fgDB()
	db.Settings.OtherIncome = 20000 // marginal rate 14% + 2% Medicare in 2028-29: below the 30% floor
	est := sellG(db, 10)
	b := compare(t, db, est, NewLawAssump{SaleDate: "2028-07-01", GrowthPA: 10, CPI: 2.5}).B
	// income tax on the 75 post-change gain is 14% = 10.50; the floor is 30% = 22.50; top-up 12.00
	if math.Abs(b.TopUp-12) > 0.2 {
		t.Errorf("top-up %v", b.TopUp)
	}
	if b.TaxDelta <= b.MarginalTax {
		t.Error("tax should include the top-up")
	}
	// the deferred (pre-change) gain is not exposed to the floor: no growth, no top-up
	b0 := compare(t, db, est, DefaultNewLaw()).B
	if b0.TopUp != 0 || b0.PostGain != 0 {
		t.Errorf("no post-change gain, no floor: %+v", b0)
	}
}

func TestLossAfterTheTransitionIsNotIndexedAndOffsetsDeferredGainFirst(t *testing.T) {
	db := fgDB()
	est := sellG(db, 10)
	b := compare(t, db, est, NewLawAssump{SaleDate: "2028-07-01", GrowthPA: -10, CPI: 2.5}).B
	// sells at $90: a $100 nominal loss since the reset (indexation can't enlarge it)
	if math.Abs(b.Parcels[0].PostNominal+100) > 0.5 || b.Parcels[0].PostIndexed != b.Parcels[0].PostNominal || b.IndexRelief != 0 {
		t.Errorf("%+v", b.Parcels[0])
	}
	// the loss reduces the deferred gain (990 -> 890) before it could touch new gains; the discount then applies
	if math.Abs(b.DeferredGross-890) > 0.5 || math.Abs(b.DeferredTaxable-445) > 0.5 || math.Abs(b.TaxDelta-142.4) > 0.5 || math.Abs(b.LossesUsed-100) > 0.5 {
		t.Errorf("%+v", b)
	}
}

func TestLossOrderingProtectsNewGainsFromSmallLosses(t *testing.T) {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	addInst(db, "L.AX", 80)
	addInst(db, "W.AX", 100)
	db.Txns = []Txn{
		{Date: "2023-01-10", Symbol: "L.AX", Type: "buy", Units: 10, Price: 130}, // a $500 loss today
		{Date: "2023-01-10", Symbol: "W.AX", Type: "buy", Units: 10, Price: 1},   // a deferred gain
	}
	est, err := EstimateSale(db, []SaleReq{{Symbol: "L.AX", All: true}, {Symbol: "W.AX", All: true}}, "2026-11-13", "fifo", wNow)
	if err != nil {
		t.Fatal(err)
	}
	b := compare(t, db, est, NewLawAssump{SaleDate: "2028-07-01", GrowthPA: 20, CPI: 2.5}).B
	// pool of 500 (L's pre-change loss) is smaller than W's 990 deferred gain, so it all goes there;
	// the new (indexed) gains are untouched
	var postSum float64
	for _, p := range b.Parcels {
		if p.PostIndexed > 0 {
			postSum += p.PostIndexed
		}
	}
	if b.PostGain != postSum || b.PostGain <= 0 {
		t.Errorf("post gain %v vs %v", b.PostGain, postSum)
	}
	if !near(b.LossesUsed, 500) || b.LossCarried != 0 {
		t.Errorf("losses used %v carried %v", b.LossesUsed, b.LossCarried)
	}
}

func youngDB() *DB {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	addInst(db, "Z.AX", 100)
	db.Txns = []Txn{{Date: "2026-06-01", Symbol: "Z.AX", Type: "buy", Units: 100, Price: 90}}
	return db
}

func TestWaitingPastJulyMakesAParcelDiscountEligible(t *testing.T) {
	db := youngDB() // bought 1 June 2026: under 12 months now, over 12 months on 1 July 2027
	est := estimateAt(t, db, "Z.AX", 10, 100)
	if !near(est.Sel.TaxDelta, 32) { // gain 100 at full rate, 32%
		t.Fatalf("current rules: %v", est.Sel.TaxDelta)
	}
	b := compare(t, db, est, DefaultNewLaw()).B
	if !b.Parcels[0].Held12 || !near(b.Discount, 50) || !near(b.TaxDelta, 16) {
		t.Errorf("by 1 July 2027 the parcel is over 12 months: %+v", b)
	}
	// a parcel still under 12 months at the post-change sale gets neither discount nor indexation
	db2 := youngDB()
	db2.Txns[0].Date = "2026-10-01"
	est2 := estimateAt(t, db2, "Z.AX", 10, 100)
	b2 := compare(t, db2, est2, NewLawAssump{SaleDate: "2027-07-01", GrowthPA: 0, CPI: 2.5}).B
	if b2.Parcels[0].Held12 || b2.Discount != 0 || !near(b2.TaxDelta, 32) {
		t.Errorf("%+v", b2)
	}
}

func TestEarlierLossesCarryIntoTheNewRulesYear(t *testing.T) {
	db := fgDB()
	addInst(db, "H.AX", 50)
	db.Txns = append(db.Txns,
		Txn{Date: "2023-01-10", Symbol: "H.AX", Type: "buy", Units: 100, Price: 60},
		Txn{Date: "2025-09-01", Symbol: "H.AX", Type: "sell", Units: 100, Price: 50}) // a $1,000 loss in 2025-26 with no gains to use it
	est := sellG(db, 10)
	b := compare(t, db, est, DefaultNewLaw()).B
	if !near(b.LossesUsed, 990) || !near(b.LossCarried, 10) || !near(b.DeferredGross, 0) || b.TaxDelta > 0.005 {
		t.Errorf("a carried-forward loss should absorb the deferred gain: %+v", b)
	}
	// ...while the current-rules estimate for 2026-27 also applies it
	if sellG(db, 10).Sel.TaxDelta >= 158 {
		t.Error("loss should reduce current-rules tax too")
	}
}

func TestCompareNewLawValidatesInput(t *testing.T) {
	db := fgDB()
	est := sellG(db, 10)
	if _, err := CompareNewLaw(db, est, nil, "fifo", NewLawAssump{SaleDate: "2027-06-30"}, wNow); err == nil || !strings.Contains(err.Error(), "1 July 2027") {
		t.Errorf("date before the change: %v", err)
	}
	if _, err := CompareNewLaw(db, est, nil, "fifo", NewLawAssump{SaleDate: "garbage"}, wNow); err == nil {
		t.Error("bad date")
	}
	if _, err := CompareNewLaw(db, est, nil, "fifo", NewLawAssump{SaleDate: "2027-07-01", CPI: 99}, wNow); err == nil {
		t.Error("absurd CPI")
	}
	// blank date defaults to the first day of the new rules
	v, err := CompareNewLaw(db, est, nil, "fifo", NewLawAssump{CPI: 2.5}, wNow)
	if err != nil || v.B.A.SaleDate != "2027-07-01" {
		t.Errorf("%v %+v", err, v)
	}
}

func TestNewLawKeepsTheSameParcelsForAWithdrawalPlan(t *testing.T) {
	db := withdrawDB()
	setTargets(db, map[string]float64{"A.AX": 40, "B.AX": 30, "C.AX": 30})
	wr, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	picks := map[string][]Pick{}
	for _, rq := range wr.Sel.Plan.Reqs {
		picks[rq.Symbol] = rq.Picks
	}
	v, err := CompareNewLaw(db, wr.Sel.Est, picks, "plan", DefaultNewLaw(), wNow)
	if err != nil {
		t.Fatal(err)
	}
	// same units and parcels as the plan, and with no growth the same tax
	if len(v.B.Parcels) != len(wr.Sel.Est.Sel.Events) {
		t.Errorf("parcels %d vs %d", len(v.B.Parcels), len(wr.Sel.Est.Sel.Events))
	}
	if math.Abs(v.B.TaxDelta-wr.Sel.Tax) > 0.01 {
		t.Errorf("new rules %v vs plan %v", v.B.TaxDelta, wr.Sel.Tax)
	}
}
