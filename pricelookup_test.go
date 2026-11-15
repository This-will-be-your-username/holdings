package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// closes for Thu 2 Jan, Fri 3 Jan 2025 (a Mon-Fri week: 6 Jan is the next trading day)
const fakeChart = `{"chart":{"result":[{"meta":{"currency":"AUD","longName":"Vanguard Australian Shares","gmtoffset":0},
"timestamp":[1735776000,1735862400,1736121600],"indicators":{"quote":[{"close":[95.10,96.20,97.30]}]}}],"error":null}}`

func fakeYahooServer(t *testing.T, body string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	old := yahooBase
	yahooBase = srv.URL
	t.Cleanup(func() { yahooBase = old; srv.Close() })
}

func postTxn(t *testing.T, a *App, v url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/transactions", strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, req)
	return r
}

func lastTxn(a *App) (tx Txn) {
	a.st().View(func(db *DB) { tx = db.Txns[len(db.Txns)-1] })
	return
}

func TestListedPriceIsLookedUpWhenBlank(t *testing.T) {
	fakeYahooServer(t, fakeChart)
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	r := postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2025-01-03"}, "units": {"10"}})
	if r.Code != 303 || strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("add: %d %s", r.Code, r.Header().Get("Location"))
	}
	if tx := lastTxn(a); tx.Price != 96.2 || !strings.Contains(tx.Notes, "market close on 2025-01-03") {
		t.Fatalf("looked-up txn %+v", tx)
	}
	// a Saturday uses Friday's close
	postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2025-01-04"}, "units": {"5"}})
	if tx := lastTxn(a); tx.Price != 96.2 || !strings.Contains(tx.Notes, "2025-01-03") {
		t.Fatalf("weekend txn %+v", tx)
	}
}

func TestEnteredPriceIsNotOverridden(t *testing.T) {
	fakeYahooServer(t, fakeChart)
	a, _ := newApp(t.TempDir() + "/d.json")
	postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2020-03-03"}, "units": {"10"}, "price": {"70.50"}})
	if tx := lastTxn(a); tx.Price != 70.5 || tx.Notes != "" {
		t.Fatalf("explicit cost basis must be kept as typed: %+v", tx)
	}
}

func TestTotalCostBasisBecomesPerUnit(t *testing.T) {
	fakeYahooServer(t, fakeChart)
	a, _ := newApp(t.TempDir() + "/d.json")
	r := postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2019-05-01"}, "units": {"40"}, "price": {"3000"}, "total_cost": {"1"}})
	if r.Code != 303 || strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("add: %s", r.Header().Get("Location"))
	}
	if tx := lastTxn(a); tx.Price != 75 || !strings.Contains(tx.Notes, "total cost basis") {
		t.Fatalf("total cost basis: %+v", tx)
	}
	// no units: can't divide
	r = postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2019-05-01"}, "price": {"3000"}, "total_cost": {"1"}})
	if r.Code == 303 {
		t.Fatal("a total cost without units should be refused")
	}
}

func TestLookupFailureAsksForAPrice(t *testing.T) {
	fakeYahooServer(t, fakeChart)
	a, _ := newApp(t.TempDir() + "/d.json")
	// a first transaction creates the instrument (the fake feed answers), then a date far from any close fails
	postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2025-01-03"}, "units": {"1"}})
	r := postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {"2024-06-03"}, "units": {"1"}})
	if r.Code == 303 || !strings.Contains(r.Body.String(), "Enter a price or your cost basis") {
		t.Fatalf("expected a prompt to enter a price: %d", r.Code)
	}
	r = postTxn(t, a, url.Values{"market": {"ASX"}, "symbol": {"VAS"}, "type": {"buy"}, "date": {time.Now().AddDate(0, 0, 30).Format(dateFmt)}, "units": {"1"}})
	if r.Code == 303 || !strings.Contains(r.Body.String(), "future") {
		t.Fatalf("future date should be refused: %d", r.Code)
	}
}

func TestPrivateCompanyStillNeedsAPrice(t *testing.T) {
	a, _ := newApp(t.TempDir() + "/d.json")
	r := postTxn(t, a, url.Values{"market": {"PRIVATE"}, "symbol": {"ACME"}, "type": {"buy"}, "date": {"2025-01-03"}, "units": {"10"}})
	if r.Code == 303 {
		t.Fatal("unlisted holdings have no market price to look up")
	}
}
