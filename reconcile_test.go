package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const valCSV = `Account name,Account number,Investment,Product ID,Price date,Price,Quantity,Value
Test Person,00000000,holdings.export.csv.vanguardCashAccount,,14-Nov-2026,,,50.00
Test Person,00000000,Alpha Index Fund,1,12-Nov-2026,2.0000,1000,2000.00
Test Person,00000000,Beta Index ETF,2,13-Nov-2026,10,50,500.00
Test Person,00000000,Gamma Fund,3,13-Nov-2026,1,10,10.00
,,,,,,holdings.export.csv.totalPortfolioValue,2560.00
`

func reconDB() *DB {
	db := &DB{}
	db.init()
	db.Instruments["ALPHA"] = &Instrument{Symbol: "ALPHA", Name: "Alpha Index Fund", Currency: "AUD", Market: MarketManaged}
	db.Instruments["BETA"] = &Instrument{Symbol: "BETA", Name: "Beta Index ETF", Currency: "AUD", Market: MarketManaged}
	db.Instruments["DELTA"] = &Instrument{Symbol: "DELTA", Name: "Delta Fund", Currency: "AUD", Market: MarketManaged}
	db.Txns = []Txn{
		{ID: "1", Date: "2025-01-02", Symbol: "ALPHA", Type: "buy", Units: 400, Price: 1.5},
		{ID: "2", Date: "2025-01-02", Symbol: "BETA", Type: "buy", Units: 50, Price: 9},
		{ID: "3", Date: "2025-02-02", Symbol: "DELTA", Type: "buy", Units: 5, Price: 1},
	}
	db.Prices["ALPHA"] = map[string]float64{"2026-11-12": 2}
	db.Prices["BETA"] = map[string]float64{"2026-11-13": 10}
	db.Prices["DELTA"] = map[string]float64{"2026-11-13": 1}
	return db
}

func TestParseValuation(t *testing.T) {
	v, err := ParseValuation([]byte(valCSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Holdings) != 3 || v.Cash != 50 || v.Total != 2560 || v.AsAt != "2026-11-13" {
		t.Fatalf("parsed %+v", v)
	}
	if v.Holdings[0].Qty != 1000 || v.Holdings[0].Price != 2 {
		t.Fatalf("holding %+v", v.Holdings[0])
	}
	if _, err := ParseValuation([]byte("a,b\n1,2\n")); err == nil {
		t.Fatal("expected an error for a file with no holdings")
	}
}

func TestReconcileFindsMissingUnits(t *testing.T) {
	v, _ := ParseValuation([]byte(valCSV))
	rc := Reconcile(reconDB(), v, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	st := map[string]string{}
	for _, r := range rc.Rows {
		st[r.Name] = r.Status
	}
	if st["Alpha Index Fund"] != "missing" || st["Beta Index ETF"] != "ok" || st["Gamma Fund"] != "unmatched" {
		t.Fatalf("statuses %v", st)
	}
	if rc.NMissing != 1 || rc.NOK != 1 || rc.NUnmatched != 1 {
		t.Fatalf("counts %+v", rc)
	}
	if math.Abs(rc.MissingValue-1200) > 0.01 { // 600 units at $2
		t.Fatalf("missing value %v", rc.MissingValue)
	}
	if len(rc.NotInVanguard) != 1 || rc.NotInVanguard[0].Symbol != "DELTA" {
		t.Fatalf("not in provider: %+v", rc.NotInVanguard)
	}
	if rc.VGTotal != 2560 || rc.EarliestTxn != "2025-01-02" {
		t.Fatalf("totals %+v", rc)
	}
}

func TestReconcileAgreesOnceHistoryIsComplete(t *testing.T) {
	db := reconDB()
	db.Txns = append(db.Txns, Txn{ID: "4", Date: "2024-01-02", Symbol: "ALPHA", Type: "buy", Units: 600, Price: 1})
	v, _ := ParseValuation([]byte(valCSV))
	rc := Reconcile(db, v, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	if rc.NMissing != 0 || rc.NOK != 2 {
		t.Fatalf("expected the imported history to close the gap: %+v", rc)
	}
}

func TestReconcilePageAndUpload(t *testing.T) {
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/reconcile", nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Upload the holdings export") {
		t.Fatalf("form page: %d", r.Code)
	}
	req := httptest.NewRequest("POST", "/reconcile", strings.NewReader("paste="+strings.ReplaceAll(strings.ReplaceAll(valCSV, "\n", "%0A"), ",", "%2C")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = httptest.NewRecorder()
	h.ServeHTTP(r, req)
	loc := r.Header().Get("Location")
	if r.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/reconcile?t=") {
		t.Fatalf("upload: %d %q", r.Code, loc)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", loc, nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "Alpha Index Fund") {
		t.Fatalf("result page: %d", r.Code)
	}
}

func TestFirstRecordReinvestmentWarns(t *testing.T) {
	db := reconDB()
	db.Txns = []Txn{{ID: "1", Date: "2025-03-01", Symbol: "ALPHA", Type: "drp", Units: 3, Price: 1.5, Amount: 4.5}}
	res := Compute(db, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "reinvestment") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a missing-history warning, got %v", res.Warnings)
	}
}
