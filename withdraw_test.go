package main

import (
	"strings"
	"testing"
	"time"
)

var wNow = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

func addInst(db *DB, sym string, price float64) {
	db.Instruments[sym] = &Instrument{Symbol: sym, Name: sym, Currency: "AUD", Market: MarketASX}
	db.Prices[sym] = map[string]float64{"2026-11-13": price}
}

// A: big winner. B: moderate winner. C: loss-making, with a young parcel.
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

func TestWithdrawTakesFromLeastProfitableEligibleParcelFirst(t *testing.T) {
	db := withdrawDB()
	res, err := WithdrawalPlans(db, WithdrawOpts{Target: 500, Strategy: "least"}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	pe := res.Sel
	// C is the least profitable holding; its biggest-loss eligible parcel (cost 14) goes first, 500/10 = 50 units
	if got := strings.Join(picksOf(pe), ","); got != "C#0x50" {
		t.Fatalf("picks %s", got)
	}
	if !near(pe.Raised, 500) || !near(pe.Est.Sel.Gain, -200) || pe.Tax > 0.005 {
		t.Errorf("raised %v gain %v tax %v", pe.Raised, pe.Est.Sel.Gain, pe.Tax)
	}
	if pe.Plan.UsedIneligible {
		t.Error("no need for under-12-month parcels")
	}
	if len(pe.Rows) != 1 || pe.Rows[0].Reason == "" || !strings.Contains(pe.Rows[0].Reason, "least profitable") || !strings.Contains(pe.Rows[0].Reason, "loss") {
		t.Errorf("reason: %+v", pe.Rows)
	}
}

func TestWithdrawMovesToNextHoldingAndSkipsYoungParcels(t *testing.T) {
	res, err := WithdrawalPlans(withdrawDB(), WithdrawOpts{Target: 2600, Strategy: "least"}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	pe := res.Sel
	// C's two eligible parcels (2,000) then B (next least profitable): its lowest-gain parcel (cost 18) for the remaining 600
	if got := strings.Join(picksOf(pe), ","); got != "C#0x100,C#1x100,B#0x30" {
		t.Fatalf("picks %s", got)
	}
	if pe.Plan.UsedIneligible {
		t.Error("eligible parcels covered it")
	}
	for _, p := range pe.Plan.Picks {
		if !p.Eligible {
			t.Errorf("under-12-month parcel used: %+v", p)
		}
	}
	// ranking: C, B, A with order numbers
	r := pe.Plan.Ranking
	if r[0].Symbol != "C.AX" || r[1].Symbol != "B.AX" || r[2].Symbol != "A.AX" || r[0].Order != 1 || r[2].Order != 3 {
		t.Errorf("ranking %+v", r)
	}
	if !near(r[0].Taken, 2000) || !near(r[1].Taken, 600) || r[2].Taken != 0 {
		t.Errorf("taken %+v", r)
	}
	// B's parcel: gain 2/unit * 30 = 60, discounted to 30, at 32% = 9.60; C's losses (-500-... ) offset it
	if pe.Est.Sel.Gain >= 60 {
		t.Errorf("gain %v should include C's losses", pe.Est.Sel.Gain)
	}
}

func TestWithdrawFallsBackToYoungParcelsAndSaysSo(t *testing.T) {
	db := withdrawDB()
	res, err := WithdrawalPlans(db, WithdrawOpts{Target: 2300, Strategy: "least", Only: map[string]bool{"C.AX": true}}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	pe := res.Sel
	if got := strings.Join(picksOf(pe), ","); got != "C#0x100,C#1x100,C#2x30" {
		t.Fatalf("picks %s", got)
	}
	if !pe.Plan.UsedIneligible || pe.Plan.Picks[2].Eligible {
		t.Error("should flag the under-12-month parcel")
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
	if !strings.Contains(pe.Plan.Picks[2].Reason, "didn't cover") {
		t.Errorf("reason: %s", pe.Plan.Picks[2].Reason)
	}
}

func TestWithdrawRoundsUpToWholeUnitsAndCoversBrokerage(t *testing.T) {
	db := withdrawDB()
	res, err := WithdrawalPlans(db, WithdrawOpts{Target: 505, Strategy: "least", Only: map[string]bool{"C.AX": true}}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := picksOf(res.Sel); len(got) != 1 || got[0] != "C#0x51" || !near(res.Sel.Raised, 510) {
		t.Errorf("whole units: %v raised %v", got, res.Sel.Raised)
	}
	res, err = WithdrawalPlans(db, WithdrawOpts{Target: 500, Brokerage: 10, Strategy: "least", Only: map[string]bool{"C.AX": true}}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := picksOf(res.Sel); len(got) != 1 || got[0] != "C#0x51" || !near(res.Sel.Raised, 500) || !near(res.Sel.Est.Fees, 10) {
		t.Errorf("brokerage: %v raised %v fees %v", got, res.Sel.Raised, res.Sel.Est.Fees)
	}
}

func TestWithdrawStrategiesDiffer(t *testing.T) {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	addInst(db, "F.AX", 100)
	addInst(db, "G.AX", 100)
	db.Txns = []Txn{
		{Date: "2023-01-10", Symbol: "F.AX", Type: "buy", Units: 100, Price: 90}, // +11% overall, gain 10/unit
		{Date: "2023-01-10", Symbol: "G.AX", Type: "buy", Units: 100, Price: 1},  // huge gain...
		{Date: "2024-01-10", Symbol: "G.AX", Type: "buy", Units: 100, Price: 99}, // ...but this parcel's gain is only 1/unit
	}
	least, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000, Strategy: "least"}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(picksOf(least.Sel), ","); got != "F#0x10" {
		t.Errorf("least-profitable picks %s", got)
	}
	var tax *PlanEval
	for _, p := range least.Plans {
		if p.Key == "tax" {
			tax = p
		}
	}
	if tax == nil || strings.Join(picksOf(tax), ",") != "G#1x10" {
		t.Fatalf("tax strategy should take the cheap G parcel: %+v", tax)
	}
	// F: gain 100, discounted 50, 32% -> 16.00; G: gain 10, discounted 5 -> 1.60
	if !near(least.Sel.Tax, 16) || !near(tax.Tax, 1.6) {
		t.Errorf("taxes %v %v", least.Sel.Tax, tax.Tax)
	}
	if !tax.Best || least.Sel.Best {
		t.Error("the lower-tax plan should be flagged best")
	}
	// choosing "tax" as the primary strategy flips which one is selected
	res, _ := WithdrawalPlans(db, WithdrawOpts{Target: 1000, Strategy: "tax"}, wNow)
	if res.Sel.Key != "tax" || res.Plans[0].Key != "tax" {
		t.Errorf("selected %s", res.Sel.Key)
	}
}

func TestWithdrawAfterTaxTarget(t *testing.T) {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	addInst(db, "F.AX", 100)
	db.Txns = []Txn{{Date: "2023-01-10", Symbol: "F.AX", Type: "buy", Units: 100, Price: 90}}
	pre, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000, Strategy: "least"}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Sel.AfterTax >= 1000 { // 10 units, tax 16 -> 984 in hand
		t.Fatalf("before-tax plan should leave less than 1000 after tax: %v", pre.Sel.AfterTax)
	}
	post, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000, AfterTax: true, Strategy: "least"}, wNow)
	if err != nil {
		t.Fatal(err)
	}
	if post.Sel.AfterTax < 1000 || post.Sel.Raised <= pre.Sel.Raised {
		t.Errorf("after-tax: raised %v, after tax %v", post.Sel.Raised, post.Sel.AfterTax)
	}
	if post.Sel.AfterTax > 1000+100+1 { // overshoot is bounded by one whole unit
		t.Errorf("overshoot too large: %v", post.Sel.AfterTax)
	}
}

func TestWithdrawErrors(t *testing.T) {
	db := withdrawDB()
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 0}, wNow); err == nil {
		t.Error("zero amount")
	}
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 1e9}, wNow); err == nil || !strings.Contains(err.Error(), "can only raise") {
		t.Errorf("too much: %v", err)
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
	n := len(db.Txns)
	if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 2600}, wNow); err != nil {
		t.Fatal(err)
	}
	if len(db.Txns) != n || db.Settings.EstPicks != nil || db.Settings.EstMethod != "" {
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
	// a pick for fewer units than the sale: the rest follows the method
	rows[len(rows)-1].Units = 15
	rows[len(rows)-1].GrossAUD = 300
	ev, _ = cgtReplay(rows, "fifo", "fifo", map[string][]Pick{"est-0": {{Seq: 1, Units: 10}}})
	if len(ev) != 2 || ev[0].Acquired != "2023-06-10" || ev[1].Acquired != "2023-01-10" || !near(ev[1].Units, 5) {
		t.Errorf("%+v", ev)
	}
}
