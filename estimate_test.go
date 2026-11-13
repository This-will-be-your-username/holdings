package main

import (
	"strings"
	"testing"
	"time"
)

var estNow = time.Date(2025, 7, 10, 0, 0, 0, 0, time.UTC)

func estDB() *DB {
	db := newTestDB()
	db.Settings.OtherIncome = 100000 // 30% bracket + 2% Medicare = 32% marginal
	db.Txns = []Txn{
		{Date: "2024-01-02", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 100}, // >12m by the sale date
		{Date: "2025-06-01", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 120}, // recent
	}
	db.Prices["VAS.AX"] = map[string]float64{"2025-07-10": 200}
	return db
}

func TestEstimateMethodsAndTax(t *testing.T) {
	est, err := EstimateSale(estDB(), []SaleReq{{Symbol: "VAS.AX", Units: 10}}, "2025-07-10", "fifo", estNow)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*MethodResult{}
	for _, m := range est.Methods {
		by[m.Method] = m
	}
	// fifo: old parcel, gain 1000, discounted 500 -> 32% = 160
	if m := by["fifo"]; !near(m.Gain, 1000) || !near(m.DiscountedGain, 500) || !near(m.TaxDelta, 160) {
		t.Errorf("fifo %+v", m)
	}
	// hifo: recent parcel, gain 800, no discount -> 256
	if m := by["hifo"]; !near(m.Gain, 800) || !near(m.TaxDelta, 256) {
		t.Errorf("hifo %+v", m)
	}
	// optimal: picks the discounted parcel (500 < 800)
	if m := by["optimal"]; !near(m.TaxDelta, 160) || !m.Best {
		t.Errorf("optimal %+v", m)
	}
	if by["hifo"].Best || !est.MethodsDiffer {
		t.Error("hifo should not be best; methods differ")
	}
	if !near(est.NetProceeds, 2000) || !near(est.Sel.AfterTax, 2000-160) {
		t.Errorf("proceeds %v after-tax %v", est.NetProceeds, est.Sel.AfterTax)
	}
}

func TestEstimateWaitForDiscount(t *testing.T) {
	// hifo sells the 2025-06-01 parcel: full-rate now, eligible from 2026-06-02 (same income year)
	est, err := EstimateSale(estDB(), []SaleReq{{Symbol: "VAS.AX", Units: 10, Fees: 10}}, "2025-07-10", "hifo", estNow)
	if err != nil {
		t.Fatal(err)
	}
	if est.Wait == nil || est.Wait.Date != "2026-06-02" || est.Wait.Parcels != 1 {
		t.Fatalf("wait: %+v", est.Wait)
	}
	// gain 790 after $10 brokerage: 790*.32=252.80 now vs 395*.32=126.40 waiting
	if !near(est.Sel.TaxDelta, 252.8) || !near(est.Wait.TaxDelta, 126.4) || !near(est.Wait.Saving, 126.4) {
		t.Errorf("sel %v wait %+v", est.Sel.TaxDelta, est.Wait)
	}
	// fifo sells only discount-eligible units -> nothing to wait for
	est, _ = EstimateSale(estDB(), []SaleReq{{Symbol: "VAS.AX", Units: 10}}, "2025-07-10", "fifo", estNow)
	if est.Wait != nil {
		t.Errorf("unexpected wait: %+v", est.Wait)
	}
}

func TestEstimateOffsetsExistingGainsAndLosses(t *testing.T) {
	db := estDB()
	db.Instruments["BHP.AX"] = &Instrument{Symbol: "BHP.AX", Name: "BHP", Currency: "AUD", Market: MarketASX}
	db.Prices["BHP.AX"] = map[string]float64{"2025-07-10": 40}
	db.Txns = append(db.Txns,
		Txn{Date: "2023-01-02", Symbol: "BHP.AX", Type: "buy", Units: 100, Price: 50}, // cost 5000
		Txn{Date: "2024-12-01", Symbol: "VAS.AX", Type: "sell", Units: 2, Price: 150}) // already realised gain (FY24-25)
	// Selling BHP at a loss in 2025-26 has no 2025-26 gains to offset -> tax delta 0, loss carried forward
	est, err := EstimateSale(db, []SaleReq{{Symbol: "BHP.AX", Units: 100}}, "2025-07-10", "fifo", estNow)
	if err != nil {
		t.Fatal(err)
	}
	if !near(est.Sel.Gain, -1000) || !near(est.Sel.TaxDelta, 0) || !near(est.Scen.LossCarriedFwd, 1000) {
		t.Errorf("loss: %+v carry=%v", est.Sel, est.Scen.LossCarriedFwd)
	}
	// Now add a gain in the same year first; the loss should shelter it and cut tax.
	db.Txns = append(db.Txns, Txn{Date: "2025-07-01", Symbol: "VAS.AX", Type: "sell", Units: 8, Price: 200})
	est, _ = EstimateSale(db, []SaleReq{{Symbol: "BHP.AX", Units: 100}}, "2025-07-10", "fifo", estNow)
	if est.Sel.TaxDelta >= 0 {
		t.Errorf("a loss against existing same-year gains should reduce tax, got %v", est.Sel.TaxDelta)
	}
}

func TestEstimateDoesNotAlterPastSales(t *testing.T) {
	db := estDB()
	db.Settings.CGTMethod = "fifo"
	db.Txns = append(db.Txns, Txn{Date: "2025-02-01", Symbol: "VAS.AX", Type: "sell", Units: 5, Price: 150})
	base := Compute(db, estNow)
	baseTR := ComputeTax(db, base.Rows, estNow)
	est, err := EstimateSale(db, []SaleReq{{Symbol: "VAS.AX", Units: 5}}, "2025-07-10", "hifo", estNow)
	if err != nil {
		t.Fatal(err)
	}
	// the real past sale's gain must be identical in baseline regardless of the hypothetical method
	if y := baseTR.ByFY["2024-25"]; len(y.Events) != 1 || !near(y.Events[0].Gain, 250) {
		t.Fatalf("baseline past sale: %+v", y.Events)
	}
	for _, m := range est.Methods {
		for _, e := range m.Events {
			if !strings.HasPrefix(e.TxnID, "est-") {
				t.Errorf("non-hypothetical event leaked into %s: %+v", m.Method, e)
			}
		}
	}
	if len(db.Txns) != 3 { // original db untouched
		t.Errorf("db mutated: %d txns", len(db.Txns))
	}
}

func TestEstimateInputs(t *testing.T) {
	db := estDB()
	est, err := EstimateSale(db, []SaleReq{{Symbol: "VAS.AX", All: true}}, "2025-07-10", "fifo", estNow)
	if err != nil || !near(est.Lines[0].Units, 20) || !near(est.Lines[0].Remaining, 0) {
		t.Fatalf("all: %v %+v", err, est)
	}
	est, err = EstimateSale(db, []SaleReq{{Symbol: "VAS.AX", Target: 1000}}, "2025-07-10", "fifo", estNow)
	if err != nil || !near(est.Lines[0].Units, 5) { // 1000 / $200
		t.Fatalf("target: %v %+v", err, est)
	}
	cases := map[string][]SaleReq{
		"only":  {{Symbol: "VAS.AX", Units: 21}},
		"don't": {{Symbol: "XYZ.AX", Units: 1}},
		"enter": {{Symbol: "VAS.AX"}},
		"only ": {{Symbol: "VAS.AX", Units: 15}, {Symbol: "VAS.AX", Units: 6}}, // two lines can't oversell together
	}
	for want, reqs := range cases {
		if _, err := EstimateSale(db, reqs, "2025-07-10", "fifo", estNow); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
	if _, err := EstimateSale(db, []SaleReq{{Symbol: "VAS.AX", Units: 1}}, "2025-01-01", "fifo", estNow); err == nil {
		t.Error("sale date before last transaction should be rejected")
	}
}

func TestEstimateForeignHolding(t *testing.T) {
	db := estDB()
	db.FX["USD"] = map[string]float64{"2025-01-01": 1.6, "2025-07-10": 1.5}
	db.Txns = append(db.Txns, Txn{Date: "2025-01-02", Symbol: "AAPL", Type: "buy", Units: 10, Price: 100}) // cost A$1600
	db.Prices["AAPL"] = map[string]float64{"2025-07-10": 200}
	est, err := EstimateSale(db, []SaleReq{{Symbol: "AAPL", Units: 10}}, "2025-07-10", "fifo", estNow)
	if err != nil {
		t.Fatal(err)
	}
	// proceeds 10*200*1.5 = 3000, cost 1600 -> gain 1400 (currency move included, as the ATO requires)
	if !near(est.Sel.Gain, 1400) || !near(est.NetProceeds, 3000) {
		t.Errorf("gain %v proceeds %v", est.Sel.Gain, est.NetProceeds)
	}
}
