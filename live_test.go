package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func TestMarketHoursAcrossTimeZones(t *testing.T) {
	cases := []struct {
		name, market string
		at           time.Time
		want         bool
	}{
		// Tuesday 10 Nov 2026: Sydney is on daylight time (UTC+11)
		{"ASX 09:59 AEDT", MarketASX, utc(2026, 11, 9, 22, 59), false},
		{"ASX 10:00 AEDT", MarketASX, utc(2026, 11, 9, 23, 0), true},
		{"ASX 16:09 AEDT", MarketASX, utc(2026, 11, 10, 5, 9), true},
		{"ASX 16:10 AEDT (closed after the auction)", MarketASX, utc(2026, 11, 10, 5, 10), false},
		// Tuesday 7 July 2026: standard time (UTC+10)
		{"ASX 10:00 AEST", MarketASX, utc(2026, 7, 7, 0, 0), true},
		{"ASX 09:59 AEST", MarketASX, utc(2026, 7, 6, 23, 59), false},
		{"ASX Saturday", MarketASX, utc(2026, 11, 13, 23, 30), false}, // Sat 14 Nov 10:30 Sydney
		{"ASX Sunday", MarketASX, utc(2026, 11, 14, 23, 30), false},
		{"US 09:29 EST", MarketUS, utc(2026, 11, 10, 14, 29), false},
		{"US 09:30 EST", MarketUS, utc(2026, 11, 10, 14, 30), true},
		{"US 15:59 EST", MarketUS, utc(2026, 11, 10, 20, 59), true},
		{"US 16:00 EST", MarketUS, utc(2026, 11, 10, 21, 0), false},
		{"US summer 09:30 EDT", MarketUS, utc(2026, 7, 7, 13, 30), true},
		{"US Saturday", MarketUS, utc(2026, 11, 14, 15, 0), false},
		{"unknown market", MarketManaged, utc(2026, 11, 10, 1, 0), false},
	}
	for _, c := range cases {
		if got := marketOpenByClock(c.market, c.at); got != c.want {
			t.Errorf("%s: open=%v want %v", c.name, got, c.want)
		}
	}
}

func TestPublicHolidayIsRecognisedFromTheQuoteSession(t *testing.T) {
	tue := utc(2026, 11, 9, 23, 30) // 10:30 Sydney on a weekday
	if !marketOpen(MarketASX, nil, tue) {
		t.Fatal("open by the clock")
	}
	// Yahoo reports the session window for the next trading day when today is a holiday
	holiday := &Quote{Start: tue.Add(24 * time.Hour).Unix(), End: tue.Add(30 * time.Hour).Unix()}
	if marketOpen(MarketASX, holiday, tue) {
		t.Error("a quote whose session is another day means today is a holiday")
	}
	today := &Quote{Start: tue.Add(-30 * time.Minute).Unix(), End: tue.Add(5 * time.Hour).Unix()}
	if !marketOpen(MarketASX, today, tue) {
		t.Error("today's own session window should keep it open")
	}
}

// ---------- fakes for Yahoo, ASX and Vanguard ----------

func quoteJSON(price float64, at time.Time, start, end time.Time) string {
	return fmt.Sprintf(`{"chart":{"result":[{"meta":{"regularMarketPrice":%.4f,"regularMarketTime":%d,"chartPreviousClose":%.4f,"gmtoffset":39600,
		"currentTradingPeriod":{"regular":{"start":%d,"end":%d}}},"timestamp":[],"indicators":{"quote":[{"close":[]}]}}],"error":null}}`,
		price, at.Unix(), price-1, start.Unix(), end.Unix())
}

func withBases(t *testing.T, yahoo, asx, vanguard string) {
	t.Helper()
	oy, oa, ov := yahooBase, asxBase, vanguardBase
	t.Cleanup(func() {
		yahooBase, asxBase, vanguardBase = oy, oa, ov
		yahooOK()
		vgCache.Lock()
		vgCache.byAPIR = nil
		vgCache.Unlock()
		liveNow = time.Now
	})
	if yahoo != "" {
		yahooBase = yahoo
	}
	if asx != "" {
		asxBase = asx
	}
	if vanguard != "" {
		vanguardBase = vanguard
	}
	yahooOK()
}

func TestFetchYahooQuote(t *testing.T) {
	at := utc(2026, 11, 10, 1, 0) // 12:00 Sydney
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "VGS.AX") || r.URL.Query().Get("range") != "1d" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprint(w, quoteJSON(165.25, at, at.Add(-2*time.Hour), at.Add(4*time.Hour)))
	}))
	defer srv.Close()
	withBases(t, srv.URL, "", "")
	q, err := fetchYahooQuote("VGS.AX")
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != 165.25 || q.Prev != 164.25 || q.At != at.Unix() || q.Date != "2026-11-10" || q.Source != "yahoo" || q.End <= q.Start {
		t.Errorf("%+v", q)
	}
}

func TestQuoteFallsBackToASXWhenYahooRateLimitsAndBacksOff(t *testing.T) {
	var yahooHits, asxHits int32
	y := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&yahooHits, 1)
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}))
	defer y.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&asxHits, 1)
		if strings.Contains(r.URL.Path, "/etfs/vgs/") {
			fmt.Fprint(w, `{"data":{"shareInformation":{"priceAsk":165.40,"priceBid":165.20,"priceClose":164.00}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer a.Close()
	withBases(t, y.URL, a.URL, "")
	liveNow = func() time.Time { return utc(2026, 11, 10, 0, 30) } // Tuesday 11:30 Sydney: the ASX is trading

	q, note, err := fetchQuote("VGS.AX")
	if err != nil || q.Source != "asx" || q.Price != 165.30 || !strings.Contains(note, "ASX indicative") {
		t.Fatalf("%+v %q %v", q, note, err)
	}
	if yahooAllowed() {
		t.Error("a 429 should make us wait before asking Yahoo again")
	}
	// while backing off, Yahoo isn't called at all
	before := atomic.LoadInt32(&yahooHits)
	if _, _, err := fetchQuote("VGS.AX"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&yahooHits) != before {
		t.Error("Yahoo should not be called during backoff")
	}
	// a US symbol has no second source, so the error surfaces
	if _, _, err := fetchQuote("AAPL"); err == nil {
		t.Error("expected an error for a symbol with no fallback")
	}
	if atomic.LoadInt32(&asxHits) == 0 {
		t.Error("ASX fallback never used")
	}
}

func liveApp(t *testing.T) *App {
	t.Helper()
	a, err := newApp(filepath.Join(t.TempDir(), "fundtrack.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = a.st().Update(func(db *DB) error {
		db.Instruments["VGS.AX"] = &Instrument{Symbol: "VGS.AX", Name: "VGS", Currency: "AUD", Market: MarketASX}
		db.Prices["VGS.AX"] = map[string]float64{"2026-11-06": 160, "2026-11-09": 161}
		db.Txns = []Txn{{ID: "1", Date: "2025-01-02", Symbol: "VGS.AX", Type: "buy", Units: 100, Price: 140}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLiveTickOnlyFetchesWhileOpenAndPricesTheHolding(t *testing.T) {
	var hits int32
	open := utc(2026, 11, 10, 0, 30) // Tue 11:30 Sydney, market open
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		at := liveNow().Add(-45 * time.Second)
		fmt.Fprint(w, quoteJSON(170.50, at, at.Add(-90*time.Minute), at.Add(4*time.Hour)))
	}))
	defer srv.Close()
	withBases(t, srv.URL, "", "")
	a := liveApp(t)

	liveNow = func() time.Time { return open }
	n, err := a.LiveTick(false)
	if err != nil || n != 1 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("open: n=%d err=%v hits=%d", n, err, hits)
	}
	var h *Holding
	var st LiveStatus
	a.st().View(func(db *DB) {
		res := Compute(db, open)
		h, st = res.Holdings[0], db.Live
	})
	if h.Price != 170.50 || !h.Live || h.PriceSource != "yahoo" || h.PriceDate != "2026-11-10" || h.ValueAUD != 17050 {
		t.Errorf("holding %+v", h)
	}
	if len(st.OpenMarkets) != 1 || st.OpenMarkets[0] != "ASX" || st.UpdatedAt == 0 || st.Err != "" {
		t.Errorf("status %+v", st)
	}

	// Saturday: closed, so nothing is fetched and the last quote stays in use
	liveNow = func() time.Time { return utc(2026, 11, 13, 23, 30) } // Sat 10:30 Sydney
	n, _ = a.LiveTick(false)
	if n != 0 || atomic.LoadInt32(&hits) != 1 {
		t.Errorf("closed market was polled: n=%d hits=%d", n, hits)
	}
	a.st().View(func(db *DB) {
		if len(db.Live.OpenMarkets) != 0 {
			t.Errorf("open markets %v", db.Live.OpenMarkets)
		}
	})
	// a forced refresh (the user pressed the button) fetches regardless
	if n, _ = a.LiveTick(true); n != 1 || atomic.LoadInt32(&hits) != 2 {
		t.Errorf("forced: n=%d hits=%d", n, hits)
	}
}

func TestStaleQuoteDoesNotOverrideNewerHistory(t *testing.T) {
	db := &DB{}
	db.init()
	db.Instruments["VGS.AX"] = &Instrument{Symbol: "VGS.AX", Currency: "AUD", Market: MarketASX}
	db.Prices["VGS.AX"] = map[string]float64{"2026-11-09": 161, "2026-11-10": 162}
	db.Txns = []Txn{{Date: "2025-01-02", Symbol: "VGS.AX", Type: "buy", Units: 10, Price: 140}}
	db.Quotes["VGS.AX"] = Quote{Price: 150, At: 1, Date: "2026-11-09", Source: "yahoo"} // older than the saved 10 Nov close
	if h := Compute(db, utc(2026, 11, 10, 6, 0)).Holdings[0]; h.Price != 162 || h.Live {
		t.Errorf("stale quote used: %+v", h)
	}
	db.Quotes["VGS.AX"] = Quote{Price: 163, At: utc(2026, 11, 10, 5, 59).Unix(), Date: "2026-11-10", Source: "yahoo"}
	if h := Compute(db, utc(2026, 11, 10, 6, 0)).Holdings[0]; h.Price != 163 {
		t.Errorf("a same-day quote should replace the (possibly partial) daily bar: %+v", h)
	}
}

func TestLiveFailureIsRecordedAndShown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer srv.Close()
	asx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer asx.Close()
	withBases(t, srv.URL, asx.URL, "")
	a := liveApp(t)
	open := utc(2026, 11, 10, 0, 30)
	liveNow = func() time.Time { return open }
	if n, err := a.LiveTick(false); err == nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	a.st().View(func(db *DB) {
		if !strings.Contains(db.Live.Err, "Couldn't get live prices for 1 of 1") {
			t.Errorf("error not recorded: %+v", db.Live)
		}
		res := Compute(db, open)
		lv := buildLiveView(db, false, res, open)
		if lv.State != "stale" || !lv.Open || !strings.Contains(lv.Text, "live prices unavailable") || lv.Err == "" {
			t.Errorf("view %+v", lv)
		}
	})
}

func TestLiveViewStates(t *testing.T) {
	db := &DB{}
	db.init()
	db.Instruments["VGS.AX"] = &Instrument{Symbol: "VGS.AX", Currency: "AUD", Market: MarketASX}
	db.Prices["VGS.AX"] = map[string]float64{"2026-11-13": 165}
	db.Txns = []Txn{{Date: "2025-01-02", Symbol: "VGS.AX", Type: "buy", Units: 10, Price: 140}}
	open, closed := utc(2026, 11, 10, 0, 30), utc(2026, 11, 14, 3, 0)

	db.Live = LiveStatus{UpdatedAt: open.Unix() - 20}
	lv := buildLiveView(db, false, Compute(db, open), open)
	if lv.State != "open" || !strings.HasPrefix(lv.Text, "Live · ASX open · updated ") {
		t.Errorf("open: %+v", lv)
	}
	lv = buildLiveView(db, false, Compute(db, closed), closed)
	if lv.State != "closed" || lv.Open || !strings.Contains(lv.Text, "prices as at Fri 13 Nov") {
		t.Errorf("closed: %+v", lv)
	}
	if lv = buildLiveView(db, true, Compute(db, closed), closed); lv.State != "test" {
		t.Errorf("test: %+v", lv)
	}
	db.RefreshErr = "Yahoo down"
	if lv = buildLiveView(db, false, Compute(db, closed), closed); lv.RefreshErr != "Yahoo down" {
		t.Errorf("refresh error not surfaced: %+v", lv)
	}
}

func TestQuotesPersistInASideFileNotTheMainData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	s, _ := OpenStore(path)
	_ = s.Update(func(db *DB) error {
		db.Instruments["X.AX"] = &Instrument{Symbol: "X.AX", Market: MarketASX}
		return nil
	})
	s.SetLive(map[string]Quote{"X.AX": {Price: 12.34, Date: "2026-11-10", At: 99, Source: "yahoo"}}, LiveStatus{UpdatedAt: 99}, true)
	s2, _ := OpenStore(path)
	s2.View(func(db *DB) {
		if q := db.Quotes["X.AX"]; q.Price != 12.34 || db.Live.UpdatedAt != 99 {
			t.Errorf("quote not restored: %+v %+v", q, db.Live)
		}
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "12.34") || strings.Contains(string(b), `"quotes"`) {
		t.Error("live quotes must not be written to the main data file")
	}
}

func TestASXFallbackIsOnlyUsedWhileTrading(t *testing.T) {
	asx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"shareInformation":{"priceAsk":165.40,"priceBid":165.20,"priceClose":164.00}}}`)
	}))
	defer asx.Close()
	withBases(t, "", asx.URL, "")
	liveNow = func() time.Time { return utc(2026, 11, 13, 23, 30) } // Saturday 10:30 Sydney
	if _, err := fetchASXQuote("VGS"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("a closed market must not produce an indicative quote: %v", err)
	}
	liveNow = func() time.Time { return utc(2026, 11, 10, 0, 30) } // Tuesday 11:30 Sydney
	q, err := fetchASXQuote("VGS")
	if err != nil || q.Price != 165.30 || q.Date != "2026-11-10" {
		t.Errorf("%+v %v", q, err)
	}
}
