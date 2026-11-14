package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExplorerSeriesAlign(t *testing.T) {
	db := &DB{}
	db.init()
	db.Instruments["AAA"] = &Instrument{Symbol: "AAA", Name: "Alpha", Currency: "AUD", Market: MarketManaged}
	db.Instruments["BBB"] = &Instrument{Symbol: "BBB", Name: "Beta", Currency: "USD", Market: MarketUS}
	db.Txns = []Txn{
		{ID: "1", Date: "2026-01-01", Symbol: "AAA", Type: "buy", Units: 10, Price: 10},
		{ID: "2", Date: "2026-01-05", Symbol: "BBB", Type: "buy", Units: 2, Price: 100},
		{ID: "3", Date: "2026-01-08", Symbol: "AAA", Type: "sell", Units: 10, Price: 12},
	}
	db.Prices["AAA"] = map[string]float64{"2026-01-01": 10, "2026-01-10": 12}
	db.Prices["BBB"] = map[string]float64{"2026-01-05": 100, "2026-01-10": 110}
	db.FX["USD"] = map[string]float64{"2026-01-01": 1.5}
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	ex := buildExplorer(db, Compute(db, now), now)
	if ex == nil || len(ex.T) != 10 {
		t.Fatalf("expected 10 daily points, got %+v", ex)
	}
	for _, s := range append([]ExplorerSeries{ex.Total}, ex.Items...) {
		if len(s.Value) != len(ex.T) || len(s.Invested) != len(ex.T) {
			t.Fatalf("%s misaligned", s.Sym)
		}
	}
	if len(ex.Items) != 2 || ex.Items[0].Sym != "BBB" { // ordered by latest value: 2*110*1.5 = 330 vs 0 (sold)
		t.Fatalf("order: %+v", ex.Items)
	}
	var a ExplorerSeries
	for _, it := range ex.Items {
		if it.Sym == "AAA" {
			a = it
		}
	}
	if *a.Value[0] != 100 || *a.Value[len(a.Value)-1] != 0 || *a.Invested[len(a.Invested)-1] != -20 { // bought 100, sold 120
		t.Fatalf("AAA: first %v last %v invested %v", *a.Value[0], *a.Value[len(a.Value)-1], *a.Invested[len(a.Invested)-1])
	}
	b := ex.Items[0]
	if b.Value[0] != nil || *b.Value[4] != 300 || *b.Price[len(b.Price)-1] != 165 {
		t.Fatalf("BBB should start on its first transaction: %v %v", b.Value[0], b.Price[len(b.Price)-1])
	}
	if *ex.Total.Value[9] != 330 {
		t.Fatalf("total %v", *ex.Total.Value[9])
	}
	if buildExplorer(&DB{Instruments: map[string]*Instrument{}}, &Result{}, now) != nil {
		t.Fatal("no transactions should give no explorer data")
	}
}

func TestChartsPageRenders(t *testing.T) {
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, httptest.NewRequest("GET", "/charts", nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Nothing to chart yet") {
		t.Fatalf("empty state: %d", r.Code)
	}
	a.st().Update(func(db *DB) error { *db = *privDB(); return nil })
	r = httptest.NewRecorder()
	a.routes().ServeHTTP(r, httptest.NewRequest("GET", "/charts", nil))
	if !strings.Contains(r.Body.String(), "window.__EXPLORER__=") || !strings.Contains(r.Body.String(), "explorer.js") {
		t.Fatalf("explorer data missing from page")
	}
}
