package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchFXLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/latest" || r.URL.Query().Get("base") != "USD" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"amount":1.0,"base":"USD","date":"2026-11-13","rates":{"AUD":1.5321}}`)
	}))
	defer srv.Close()
	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()
	d, rate, err := fetchFXLatest("USD")
	if err != nil || d != "2026-11-13" || rate != 1.5321 {
		t.Fatalf("got %q %v %v", d, rate, err)
	}
	if _, _, err := fetchFXLatest("XXX"); err == nil {
		t.Fatal("expected an error for an unknown currency")
	}
}

func TestOrigShowsOriginalCurrencyOnly(t *testing.T) {
	f := funcs["orig"].(func(string, float64) string)
	if f("AUD", 5) != "" || f("USD", 1234.5) != "(USD 1,234.50)" {
		t.Fatalf("orig: %q %q", f("AUD", 5), f("USD", 1234.5))
	}
}

func TestForeignHoldingShownInAUDWithOriginal(t *testing.T) {
	db := &DB{}
	db.init()
	db.Instruments["AAPL"] = &Instrument{Symbol: "AAPL", Name: "Apple", Currency: "USD", Market: MarketUS}
	db.Txns = []Txn{{ID: "1", Date: "2026-10-01", Symbol: "AAPL", Type: "buy", Units: 10, Price: 100}}
	db.Prices["AAPL"] = map[string]float64{"2026-11-13": 200}
	db.FX["USD"] = map[string]float64{"2026-10-01": 1.5, "2026-11-13": 1.6}
	res := Compute(db, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	h := res.Holdings[0]
	if h.ValueAUD != 3200 {
		t.Fatalf("value %v", h.ValueAUD)
	}
	if len(res.FX) != 1 || res.FX[0].Ccy != "USD" || res.FX[0].Rate != 1.6 || res.FX[0].Date != "2026-11-13" {
		t.Fatalf("fx info %+v", res.FX)
	}
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	a.st().Update(func(d *DB) error { *d = *db; return nil })
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, httptest.NewRequest("GET", "/", nil))
	body := r.Body.String()
	for _, want := range []string{"(USD 2,000.00)", "3,200.00", "(USD 200.00)", "1 USD = A$1.6000"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestConvertCandidateThroughAUD(t *testing.T) {
	db := &DB{}
	db.init()
	db.FX["USD"] = map[string]float64{"2026-03-01": 1.5}
	c := convertCandidate(db, Candidate{Amount: 100e6, Currency: "USD", Date: "2026-03-05"}, "AUD", "2026-11-14")
	if !c.Converted || c.ConvAmount != 150e6 {
		t.Fatalf("USD->AUD: %+v", c)
	}
	c = convertCandidate(db, Candidate{Amount: 100e6, Currency: "EUR"}, "AUD", "2026-11-14")
	if c.Converted {
		t.Fatal("no EUR rate saved, so it must not be converted")
	}
}

func TestFXRefreshesOnlyWhenOlderThan24Hours(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasPrefix(r.URL.Path, "/latest") {
			fmt.Fprint(w, `{"base":"USD","date":"2026-11-13","rates":{"AUD":1.6}}`)
			return
		}
		fmt.Fprint(w, `{"base":"USD","rates":{"2026-11-12":{"AUD":1.59},"2026-11-13":{"AUD":1.6}}}`)
	}))
	defer srv.Close()
	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()

	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	a.st().Update(func(db *DB) error {
		db.Instruments["AAPL"] = &Instrument{Symbol: "AAPL", Name: "Apple", Currency: "USD", Market: MarketUS}
		db.Txns = []Txn{{ID: "1", Date: time.Now().AddDate(0, 0, -5).Format(dateFmt), Symbol: "AAPL", Type: "buy", Units: 1, Price: 100}}
		return nil
	})
	if _, err := a.RefreshFX(false); err != nil || calls == 0 {
		t.Fatalf("first pull should fetch: %v calls=%d", err, calls)
	}
	n := calls
	if msg, _ := a.RefreshFX(false); msg != "" || calls != n {
		t.Fatalf("a pull within 24h must not call the service (calls %d -> %d)", n, calls)
	}
	a.st().Update(func(db *DB) error {
		db.PriceAt[fxStamp] = time.Now().Add(-25 * time.Hour).Format(time.RFC3339)
		return nil
	})
	if _, err := a.RefreshFX(false); err != nil || calls == n {
		t.Fatalf("a pull over 24h old should fetch again (calls %d)", calls)
	}
	n = calls
	if _, err := a.RefreshFX(true); err != nil || calls == n {
		t.Fatal("force should always fetch")
	}
	// the manual button
	r := httptest.NewRecorder()
	n = calls
	a.routes().ServeHTTP(r, httptest.NewRequest("POST", "/prices/fx", nil))
	if r.Code != 303 || calls == n || strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("button: %d %s calls %d", r.Code, r.Header().Get("Location"), calls)
	}
	body := httptest.NewRecorder()
	a.routes().ServeHTTP(body, httptest.NewRequest("GET", "/prices", nil))
	if !strings.Contains(body.Body.String(), "as at 2026-11-13") || !strings.Contains(body.Body.String(), "1.6000") {
		t.Fatal("prices page should show the saved rate")
	}
}

func TestFXFailureDoesNotResetClock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 500) }))
	defer srv.Close()
	old := fxBase
	fxBase = srv.URL
	defer func() { fxBase = old }()
	a, _ := newApp(t.TempDir() + "/d.json")
	a.st().Update(func(db *DB) error {
		db.Instruments["AAPL"] = &Instrument{Symbol: "AAPL", Currency: "USD", Market: MarketUS}
		db.Txns = []Txn{{ID: "1", Date: "2026-11-01", Symbol: "AAPL", Type: "buy", Units: 1, Price: 1}}
		return nil
	})
	if _, err := a.RefreshFX(false); err == nil {
		t.Fatal("expected an error")
	}
	var stamp string
	a.st().View(func(db *DB) { stamp = db.PriceAt[fxStamp] })
	if stamp != "" {
		t.Fatal("a failed pull must not mark rates as fresh")
	}
}
