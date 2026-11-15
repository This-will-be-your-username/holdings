package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestEditCostBasisKeepsFingerprintAndFixesGains(t *testing.T) {
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	a.st().Update(func(db *DB) error {
		db.Instruments["ACME"] = &Instrument{Symbol: "ACME", Name: "Acme Inc", Currency: "USD", Market: MarketUS}
		t1 := Txn{ID: "t1", Date: "2022-03-01", Symbol: "ACME", Type: "award", Units: 100, Price: 50, Notes: "price looked up: market close on 2022-03-01"}
		t1.FP = Fingerprint(t1)
		db.Txns = []Txn{t1}
		db.Prices["ACME"] = map[string]float64{"2026-11-13": 60}
		db.FX["USD"] = map[string]float64{"2022-03-01": 1.3, "2026-11-13": 1.5}
		return nil
	})
	var before Txn
	a.st().View(func(db *DB) { before = db.Txns[0] })

	page := httptest.NewRecorder()
	a.routes().ServeHTTP(page, httptest.NewRequest("GET", "/transactions/edit?id=t1", nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), "total") {
		t.Fatalf("edit page: %d", page.Code)
	}
	hist := httptest.NewRecorder()
	a.routes().ServeHTTP(hist, httptest.NewRequest("GET", "/transactions", nil))
	if !strings.Contains(hist.Body.String(), "/transactions/edit?id=t1") {
		t.Fatal("history should link to the edit page")
	}

	// the real cost basis is a total of USD 4,000 for the 100 units
	req := httptest.NewRequest("POST", "/transactions/edit", strings.NewReader(url.Values{"id": {"t1"}, "date": {"2022-03-01"}, "units": {"100"},
		"price": {"4000"}, "total_cost": {"1"}, "fees": {""}, "fx": {""}, "notes": {"price looked up: market close on 2022-03-01"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, req)
	if r.Code != 303 || strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("save: %d %s", r.Code, r.Header().Get("Location"))
	}
	var after Txn
	a.st().View(func(db *DB) { after = db.Txns[0] })
	if after.Price != 40 || after.FP != before.FP || after.ID != "t1" {
		t.Fatalf("after edit %+v", after)
	}
	if strings.Contains(after.Notes, "looked up") || !strings.Contains(after.Notes, "cost basis edited: total USD 4,000.00") {
		t.Fatalf("notes %q", after.Notes)
	}
	a.st().View(func(db *DB) {
		res := Compute(db, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
		h := res.Holdings[0]
		if h.CostAUD != 4000*1.3 { // cost basis converted at the rate on the acquisition date
			t.Fatalf("cost %v", h.CostAUD)
		}
	})
	// a bad edit is refused and leaves the record alone
	req = httptest.NewRequest("POST", "/transactions/edit", strings.NewReader(url.Values{"id": {"t1"}, "date": {"2022-03-01"}, "units": {"0"}, "price": {"5"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = httptest.NewRecorder()
	a.routes().ServeHTTP(r, req)
	if !strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatal("zero units should be refused")
	}
}
