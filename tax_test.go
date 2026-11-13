package main

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func TestFYOf(t *testing.T) {
	for d, want := range map[string]string{"2025-06-30": "2024-25", "2025-07-01": "2025-26", "2026-01-15": "2025-26", "2099-12-31": "2099-00"} {
		if got := fyOf(d); got != want {
			t.Errorf("fyOf(%s)=%s want %s", d, got, want)
		}
	}
}

func TestIncomeTaxScales(t *testing.T) {
	if got := incomeTax("2025-26", 135000); !near(got, 31288) {
		t.Errorf("2025-26 tax on 135k = %v, want 31288", got)
	}
	if got := incomeTax("2026-27", 135000); !near(got, 31020) { // 15% bracket
		t.Errorf("2026-27 tax on 135k = %v, want 31020", got)
	}
	if got := incomeTax("2025-26", 18200); got != 0 {
		t.Errorf("tax-free threshold: %v", got)
	}
	if got := incomeTax("2023-24", 120000); !near(got, 29467) {
		t.Errorf("2023-24 tax on 120k = %v, want 29467", got)
	}
}

func TestNetGainOrdering(t *testing.T) {
	// losses hit non-discount gains first, then discount gains
	net, carry, disc := netGain(1000, 200, 300)
	if !near(net, 450) || !near(carry, 0) || !near(disc, 450) {
		t.Errorf("net=%v carry=%v disc=%v", net, carry, disc)
	}
	net, carry, _ = netGain(100, 0, 500)
	if !near(net, 0) || !near(carry, 400) {
		t.Errorf("loss carry: net=%v carry=%v", net, carry)
	}
}

func rowsFor(txns ...Txn) []Row {
	db := newTestDB()
	db.Txns = txns
	return Compute(db, time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)).Rows
}

func TestCGTMethods(t *testing.T) {
	rows := rowsFor(
		Txn{Date: "2024-01-02", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 100},
		Txn{Date: "2025-06-01", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 120},
		Txn{Date: "2025-07-10", Symbol: "VAS.AX", Type: "sell", Units: 10, Price: 200},
	)
	fifo := cgtEvents(rows, "fifo")
	if len(fifo) != 1 || !near(fifo[0].Gain, 1000) || !fifo[0].Discount {
		t.Fatalf("fifo: %+v", fifo)
	}
	hifo := cgtEvents(rows, "hifo")
	if len(hifo) != 1 || !near(hifo[0].Gain, 800) || hifo[0].Discount {
		t.Fatalf("hifo: %+v", hifo)
	}
	opt := cgtEvents(rows, "optimal") // 1000/2=500 beats 800
	if len(opt) != 1 || !near(opt[0].Gain, 1000) || !near(opt[0].DiscountedGain(), 500) {
		t.Fatalf("optimal: %+v", opt)
	}
}

func TestCGTDiscountBoundary(t *testing.T) {
	rows := rowsFor(
		Txn{Date: "2024-07-10", Symbol: "VAS.AX", Type: "buy", Units: 1, Price: 100},
		Txn{Date: "2025-07-10", Symbol: "VAS.AX", Type: "sell", Units: 1, Price: 150}, // exactly 12 months: not eligible
	)
	if ev := cgtEvents(rows, "fifo"); ev[0].Discount {
		t.Error("exactly 12 months should not qualify")
	}
	rows = rowsFor(
		Txn{Date: "2024-07-10", Symbol: "VAS.AX", Type: "buy", Units: 1, Price: 100},
		Txn{Date: "2025-07-11", Symbol: "VAS.AX", Type: "sell", Units: 1, Price: 150},
	)
	if ev := cgtEvents(rows, "fifo"); !ev[0].Discount {
		t.Error("12 months + 1 day should qualify")
	}
}

func TestTaxDeferredCostBase(t *testing.T) {
	rows := rowsFor(
		Txn{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 100},
		Txn{Date: "2025-03-01", Symbol: "VAS.AX", Type: "dividend", Amount: 300, TaxDeferred: 200},
		Txn{Date: "2025-04-01", Symbol: "VAS.AX", Type: "sell", Units: 10, Price: 100},
	)
	ev := cgtEvents(rows, "fifo")
	if len(ev) != 1 || !near(ev[0].Gain, 200) { // cost base 1000-200 = 800
		t.Fatalf("%+v", ev)
	}
	// excess over cost base -> E4 gain
	rows = rowsFor(
		Txn{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 100},
		Txn{Date: "2025-03-01", Symbol: "VAS.AX", Type: "dividend", Amount: 1200, TaxDeferred: 1200},
	)
	ev = cgtEvents(rows, "fifo")
	if len(ev) != 1 || ev[0].Kind != "E4" || !near(ev[0].Gain, 200) {
		t.Fatalf("E4: %+v", ev)
	}
}

func TestFrankingOffsetsTax(t *testing.T) {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	db.Txns = []Txn{
		{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 100, Price: 10},
		{Date: "2025-12-01", Symbol: "VAS.AX", Type: "dividend", Amount: 700, Franked: 700, Franking: 300},
	}
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	tr := ComputeTax(db, Compute(db, now).Rows, now)
	y := tr.ByFY["2025-26"]
	if !near(y.Assessable, 1000) || !near(y.TaxOnTop, 320) || !near(y.Offsets, 300) || !near(y.NetPayable, 20) {
		t.Errorf("%+v", y)
	}
}

func TestTaxYearOverrideAndForeign(t *testing.T) {
	db := newTestDB()
	db.FX["USD"] = map[string]float64{"2025-07-05": 1.5}
	db.Txns = []Txn{
		{Date: "2025-01-02", Symbol: "AAPL", Type: "buy", Units: 10, Price: 100, FX: 1.5},
		// 10 USD gross, 1.5 USD withheld in the US
		{Date: "2025-07-05", Symbol: "AAPL", Type: "dividend", Amount: 10, ForeignTax: 1.5},
		// June-quarter AUD distribution paid in July, assessed in the earlier year
		{Date: "2025-07-20", Symbol: "VAS.AX", Type: "dividend", Amount: 50, TaxYear: "2024-25"},
	}
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	tr := ComputeTax(db, Compute(db, now).Rows, now)
	if y := tr.ByFY["2024-25"]; y == nil || !near(y.Unfranked, 50) {
		t.Errorf("override: %+v", y)
	}
	y := tr.ByFY["2025-26"]
	if !near(y.Unfranked, 15) || !near(y.ForeignTaxOffset, 2.25) || !near(y.CashReceived, 12.75) { // 10*1.5, 1.5*1.5, net
		t.Errorf("foreign: %+v", y)
	}
}

func TestFrankingInReturns(t *testing.T) {
	mk := func(exclude bool) *Holding {
		db := newTestDB()
		db.Settings.ExcludeFranking = exclude
		db.Txns = []Txn{
			{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 100, Price: 10},
			{Date: "2025-06-30", Symbol: "VAS.AX", Type: "dividend", Amount: 50, Franked: 50, Franking: 21.43},
		}
		db.Prices["VAS.AX"] = map[string]float64{"2025-12-31": 10}
		return Compute(db, time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)).Holdings[0]
	}
	in, out := mk(false), mk(true)
	if !near(in.TotalReturn, 71.43) || !near(out.TotalReturn, 50) || !near(in.Franking, 21.43) {
		t.Errorf("incl=%v excl=%v franking=%v", in.TotalReturn, out.TotalReturn, in.Franking)
	}
	if !(in.XIRR > out.XIRR) {
		t.Errorf("xirr incl %v should exceed excl %v", in.XIRR, out.XIRR)
	}
}

func TestValidateDistributionComponents(t *testing.T) {
	ok := Txn{Type: "dividend", Amount: 100, Franked: 60, CGDisc: 30, TaxDeferred: 10}
	if err := validateTxn(&ok); err != nil {
		t.Errorf("valid: %v", err)
	}
	bad := Txn{Type: "dividend", Amount: 100, Franked: 80, CGDisc: 30}
	if err := validateTxn(&bad); err == nil {
		t.Error("components exceed amount should fail")
	}
	buy := Txn{Type: "buy", Units: 1, Price: 1, Franking: 5}
	if err := validateTxn(&buy); err == nil {
		t.Error("tax details on a buy should fail")
	}
	if validFY("2025-27") || !validFY("2025-26") || !validFY("2099-00") {
		t.Error("validFY")
	}
}
