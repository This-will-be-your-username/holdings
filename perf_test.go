package main

import (
	"math"
	"testing"
	"time"
)

func TestXIRRSimple(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	r, ok := xirr([]Flow{{t0, -100}, {t0.AddDate(0, 0, 365), 110}})
	if !ok || math.Abs(r-0.10) > 1e-6 {
		t.Fatalf("got %v %v want 0.10", r, ok)
	}
}

func TestXIRRTooShort(t *testing.T) {
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := xirr([]Flow{{t0, -100}, {t0.AddDate(0, 0, 10), 110}}); ok {
		t.Fatal("expected not ok for <30 days")
	}
}

func newTestDB() *DB {
	db := &DB{}
	db.init()
	db.Instruments["VAS.AX"] = &Instrument{Symbol: "VAS.AX", Name: "VAS", Currency: "AUD", Market: MarketASX}
	db.Instruments["AAPL"] = &Instrument{Symbol: "AAPL", Name: "Apple", Currency: "USD", Market: MarketUS}
	return db
}

func TestAverageCostAndRealised(t *testing.T) {
	db := newTestDB()
	db.Txns = []Txn{
		{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 100, Fees: 10},  // cost 1010
		{Date: "2025-02-03", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 110, Fees: 10},  // cost 1110 -> pool 2120 / 20
		{Date: "2025-03-03", Symbol: "VAS.AX", Type: "sell", Units: 10, Price: 120, Fees: 10}, // net 1190, removed 1060
		{Date: "2025-03-10", Symbol: "VAS.AX", Type: "dividend", Amount: 50},
	}
	db.Prices["VAS.AX"] = map[string]float64{"2025-06-01": 130}
	res := Compute(db, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	h := res.Holdings[0]
	if math.Abs(h.Realised-130) > 1e-6 {
		t.Errorf("realised %v want 130", h.Realised)
	}
	if math.Abs(h.CostAUD-1060) > 1e-6 || h.Units != 10 {
		t.Errorf("cost %v units %v", h.CostAUD, h.Units)
	}
	if math.Abs(h.Unrealised-240) > 1e-6 { // 1300-1060
		t.Errorf("unrealised %v want 240", h.Unrealised)
	}
	if math.Abs(h.TotalReturn-(240+130+50)) > 1e-6 {
		t.Errorf("total %v", h.TotalReturn)
	}
	if !h.HasXIRR || h.XIRR <= 0 {
		t.Errorf("xirr %v %v", h.XIRR, h.HasXIRR)
	}
}

func TestForeignAndDRP(t *testing.T) {
	db := newTestDB()
	db.FX["USD"] = map[string]float64{"2025-01-01": 1.6, "2025-06-01": 1.5}
	db.Txns = []Txn{
		{Date: "2025-01-02", Symbol: "AAPL", Type: "buy", Units: 10, Price: 100, Fees: 5}, // 1000*1.6+5 = 1605
		{Date: "2025-03-02", Symbol: "AAPL", Type: "drp", Units: 1, Price: 100, Amount: 100},
	}
	db.Prices["AAPL"] = map[string]float64{"2025-06-01": 120}
	res := Compute(db, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	h := res.Holdings[0]
	if h.Units != 11 || math.Abs(h.CostAUD-(1605+160)) > 1e-6 {
		t.Errorf("units %v cost %v", h.Units, h.CostAUD)
	}
	if math.Abs(h.ValueAUD-11*120*1.5) > 1e-6 {
		t.Errorf("value %v", h.ValueAUD)
	}
	if math.Abs(h.Income-160) > 1e-6 {
		t.Errorf("income %v", h.Income)
	}
}
