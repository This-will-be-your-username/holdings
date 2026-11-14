package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const vgFundsJSON = `{"statusCode":200,"data":[
 {"portId":"8121","fundName":"Vanguard Balanced Index Fund","productType":"mf","tradingSymbols":{"TICKER":null,"APIR":"VAN0108AU","ASX":null,"ABN":null}},
 {"portId":"8133","fundName":"Vanguard Growth Index Fund","productType":"mf","tradingSymbols":{"TICKER":null,"APIR":"VAN0110AU","ASX":null,"ABN":null}},
 {"portId":"5001","fundName":"Vanguard Australian Shares Index ETF","productType":"etf","tradingSymbols":{"TICKER":"VAS","APIR":null,"ASX":"VAS","ABN":null}}]}`

const vgPricesJSON = `{"statusCode":200,"data":[{"portId":"8121","tradingSymbols":{"APIR":null},
 "navPrices":[
  {"measureTypeCode":"NAV","asOfDate":"2026-11-12","currencyCode":"AUD","price":1.5673},
  {"measureTypeCode":"NAV","asOfDate":"2026-11-10","currencyCode":"AUD","price":1.5641},
  {"measureTypeCode":"NAV","asOfDate":"2026-11-09","currencyCode":"AUD","price":0},
  {"measureTypeCode":"NAV","asOfDate":"not-a-date","currencyCode":"AUD","price":1.5},
  {"measureTypeCode":"MID","asOfDate":"2026-11-06","currencyCode":"AUD","price":1.5508}],
 "buyPrices":[{"asOfDate":"2026-11-12","price":1.5673}],"sellPrices":[{"asOfDate":"2026-11-12","price":1.5671}]}]}`

func TestVanguardFundListMapsAPIRToPortIDAndIsCached(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/products/personal/funds/allfunds" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, vgFundsJSON)
	}))
	defer srv.Close()
	withBases(t, "", "", srv.URL)
	m, err := vgFunds()
	if err != nil || len(m) != 2 || m["VAN0108AU"].PortID != "8121" || m["VAN0110AU"].Name != "Vanguard Growth Index Fund" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := vgFunds(); err != nil || atomic.LoadInt32(&hits) != 1 {
		t.Errorf("the list should be cached: %d hits", hits)
	}

	f, err := vgResolve(&Instrument{Symbol: "van0108au"})
	if err != nil || f.PortID != "8121" {
		t.Errorf("%+v %v", f, err)
	}
	if f, err = vgResolve(&Instrument{Symbol: "WHATEVER", VanguardPort: "8133"}); err != nil || f.PortID != "8133" {
		t.Errorf("a saved port ID wins over the symbol: %+v %v", f, err)
	}
	if _, err := vgResolve(&Instrument{Symbol: "MY-FUND"}); err == nil || !strings.Contains(err.Error(), "not an APIR code") {
		t.Errorf("non-APIR: %v", err)
	}
	if _, err := vgResolve(&Instrument{Symbol: "AAA0001AU"}); err == nil || !strings.Contains(err.Error(), "isn't a Vanguard") {
		t.Errorf("unknown APIR: %v", err)
	}
}

func TestFetchVanguardPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/products/personal/fund/8121/prices" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, vgPricesJSON)
	}))
	defer srv.Close()
	withBases(t, "", "", srv.URL)
	nav, err := fetchVGPrices("8121")
	if err != nil {
		t.Fatal(err)
	}
	// only valid NAV rows: zero prices, bad dates and non-NAV measures are dropped
	if len(nav) != 2 || nav["2026-11-12"] != 1.5673 || nav["2026-11-10"] != 1.5641 {
		t.Errorf("%v", nav)
	}
	if _, err := fetchVGPrices("../etc"); err == nil {
		t.Error("port IDs must be numeric")
	}
	if _, err := fetchVGPrices("9999"); err == nil { // 404 from the fake
		t.Error("expected an error for an unknown port")
	}
}

// ---------- a fake of all three services, for refresh ----------

type fakeWorld struct {
	mu                   sync.Mutex
	yahooPeriod1         map[string]int64
	yahooCalls, vgCalls  int
	asxCalls, quoteCalls int
	failYahoo            bool
}

func (w *fakeWorld) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v8/finance/chart/"):
			if w.failYahoo {
				http.Error(rw, "Too Many Requests", 429)
				return
			}
			sym := strings.TrimPrefix(r.URL.Path, "/v8/finance/chart/")
			if r.URL.Query().Get("range") == "1d" {
				w.quoteCalls++
				now := time.Now()
				fmt.Fprint(rw, quoteJSON(101, now, now.Add(-time.Hour), now.Add(time.Hour)))
				return
			}
			w.yahooCalls++
			p1, _ := strconv.ParseInt(r.URL.Query().Get("period1"), 10, 64)
			p2, _ := strconv.ParseInt(r.URL.Query().Get("period2"), 10, 64)
			w.yahooPeriod1[sym] = p1
			var ts, cl []string
			for d := time.Unix(p1, 0).UTC().Truncate(24 * time.Hour); d.Unix() < p2 && len(ts) < 4000; d = d.AddDate(0, 0, 1) {
				if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
					ts = append(ts, strconv.FormatInt(d.Unix(), 10))
					cl = append(cl, "100.5")
				}
			}
			fmt.Fprintf(rw, `{"chart":{"result":[{"meta":{"currency":"AUD","gmtoffset":39600,"longName":"x"},"timestamp":[%s],"indicators":{"quote":[{"close":[%s]}]},"events":{"dividends":{}}}]}}`,
				strings.Join(ts, ","), strings.Join(cl, ","))
		case strings.HasPrefix(r.URL.Path, "/asx-research/"):
			w.asxCalls++
			http.NotFound(rw, r)
		case r.URL.Path == "/vg/products/personal/funds/allfunds":
			fmt.Fprint(rw, vgFundsJSON)
		case r.URL.Path == "/vg/products/personal/fund/8121/prices":
			w.vgCalls++
			fmt.Fprint(rw, vgPricesJSON)
		default:
			http.NotFound(rw, r)
		}
	})
}

func refreshApp(t *testing.T) (*App, *fakeWorld) {
	t.Helper()
	fw := &fakeWorld{yahooPeriod1: map[string]int64{}}
	srv := httptest.NewServer(fw.handler(t))
	t.Cleanup(srv.Close)
	withBases(t, srv.URL, srv.URL, srv.URL+"/vg")
	a, err := newApp(t.TempDir() + "/fundtrack.json")
	if err != nil {
		t.Fatal(err)
	}
	err = a.st().Update(func(db *DB) error {
		db.Instruments["VGS.AX"] = &Instrument{Symbol: "VGS.AX", Name: "VGS", Currency: "AUD", Market: MarketASX}
		db.Instruments["VAN0108AU"] = &Instrument{Symbol: "VAN0108AU", Name: "VAN0108AU", Currency: "AUD", Market: MarketManaged}
		db.Txns = []Txn{
			{ID: "1", Date: "2025-01-02", Symbol: "VGS.AX", Type: "buy", Units: 10, Price: 100},
			{ID: "2", Date: "2025-02-03", Symbol: "VAN0108AU", Type: "buy", Units: 100, Price: 1.5},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, fw
}

func TestRefreshIsIncrementalAndPullsVanguardFunds(t *testing.T) {
	a, fw := refreshApp(t)
	if msg, err := a.Refresh(); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
	var first int64
	a.st().View(func(db *DB) {
		if len(db.Prices["VGS.AX"]) < 300 {
			t.Errorf("first refresh should download the history: %d points", len(db.Prices["VGS.AX"]))
		}
		nav := db.Prices["VAN0108AU"]
		if len(nav) != 2 || nav["2026-11-12"] != 1.5673 {
			t.Errorf("Vanguard NAV not stored: %v", nav)
		}
		if p := db.Instruments["VAN0108AU"].VanguardPort; p != "8121" {
			t.Errorf("port ID not saved: %q", p)
		}
		if db.Instruments["VAN0108AU"].Name != "Vanguard Balanced Index Fund" {
			t.Errorf("name: %q", db.Instruments["VAN0108AU"].Name)
		}
		if db.PriceAt["VGS.AX"] == "" || db.PriceAt["vg:VAN0108AU"] == "" || db.LastRefresh == "" || db.RefreshErr != "" {
			t.Errorf("bookkeeping: %+v %q", db.PriceAt, db.RefreshErr)
		}
		if len(db.Bench) != len(benchmarks) {
			t.Errorf("benchmarks: %d", len(db.Bench))
		}
	})
	first = fw.yahooPeriod1["VGS.AX"]
	if first > time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("first fetch should start before the first purchase: %d", first)
	}
	vg1, asx1 := fw.vgCalls, fw.asxCalls

	// a second refresh only asks for the last days, and doesn't bother Vanguard or the ASX again
	if _, err := a.Refresh(); err != nil {
		t.Fatal(err)
	}
	second := fw.yahooPeriod1["VGS.AX"]
	if time.Since(time.Unix(second, 0)) > 20*24*time.Hour {
		t.Errorf("second refresh should be incremental (from %s)", time.Unix(second, 0).Format(dateFmt))
	}
	if fw.vgCalls != vg1 || fw.asxCalls != asx1 {
		t.Errorf("throttled sources were hit again: vg %d->%d asx %d->%d", vg1, fw.vgCalls, asx1, fw.asxCalls)
	}
	a.st().View(func(db *DB) {
		if len(db.Prices["VGS.AX"]) < 300 {
			t.Error("incremental refresh must keep the saved history")
		}
	})
	// the user pressing Refresh forces Vanguard and takes a live quote too
	if _, err := a.RefreshNow(); err != nil {
		t.Fatal(err)
	}
	if fw.vgCalls != vg1+1 || fw.quoteCalls == 0 {
		t.Errorf("forced refresh: vg calls %d, quote calls %d", fw.vgCalls, fw.quoteCalls)
	}
}

func TestFailedRefreshIsReportedNotDisguisedAsSuccess(t *testing.T) {
	a, fw := refreshApp(t)
	_ = a.st().Update(func(db *DB) error { db.LastRefresh = "2026-01-01T00:00:00Z"; return nil })
	fw.failYahoo = true
	msg, err := a.Refresh()
	if err == nil {
		t.Fatalf("a refresh where Yahoo fails must return an error (%s)", msg)
	}
	a.st().View(func(db *DB) {
		if !strings.Contains(db.RefreshErr, "429") {
			t.Errorf("error not recorded: %q", db.RefreshErr)
		}
		// Vanguard still worked, so something genuinely updated...
		if len(db.Prices["VAN0108AU"]) == 0 {
			t.Error("Vanguard prices should still have been fetched")
		}
		if db.RefreshAt == "" {
			t.Error("refresh attempt time not recorded")
		}
	})
	// ...but if EVERYTHING fails, "last refreshed" must not move
	a2, fw2 := refreshApp(t)
	_ = a2.st().Update(func(db *DB) error {
		delete(db.Instruments, "VAN0108AU")
		db.Txns = db.Txns[:1]
		db.LastRefresh = "2026-01-01T00:00:00Z"
		return nil
	})
	fw2.failYahoo = true
	yahooOK()
	if _, err := a2.Refresh(); err == nil {
		t.Fatal("expected an error")
	}
	a2.st().View(func(db *DB) {
		if db.LastRefresh != "2026-01-01T00:00:00Z" || db.RefreshErr == "" {
			t.Errorf("a total failure must not look like a refresh: last=%s err=%q", db.LastRefresh, db.RefreshErr)
		}
	})
	// recovery clears the warning
	fw2.failYahoo = false
	yahooOK()
	if _, err := a2.Refresh(); err != nil {
		t.Fatal(err)
	}
	a2.st().View(func(db *DB) {
		if db.RefreshErr != "" || db.LastRefresh == "2026-01-01T00:00:00Z" {
			t.Errorf("recovery: last=%s err=%q", db.LastRefresh, db.RefreshErr)
		}
	})
}

func TestIncrementalFrom(t *testing.T) {
	want := time.Date(2021, 11, 1, 0, 0, 0, 0, time.UTC)
	covers := map[string]float64{"2021-11-02": 1, "2026-11-12": 2}
	if got := incrementalFrom(covers, want); got.Format(dateFmt) != "2026-11-02" {
		t.Errorf("covered history should resume 10 days before the last point, got %s", got.Format(dateFmt))
	}
	short := map[string]float64{"2025-06-01": 1, "2026-11-12": 2} // doesn't reach back far enough
	if got := incrementalFrom(short, want); !got.Equal(want) {
		t.Errorf("history that doesn't reach back should be refetched in full, got %s", got.Format(dateFmt))
	}
	if got := incrementalFrom(nil, want); !got.Equal(want) {
		t.Error("nothing saved: full history")
	}
}

// ---------- Fund prices page handlers ----------

func multipartBody(t *testing.T, fields map[string]string, file string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	if file != "" {
		fw, _ := w.CreateFormFile("file", file)
		_, _ = fw.Write(data)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestFundPricesPageFetchImportAndLink(t *testing.T) {
	a, _ := refreshApp(t)
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	c := noRedirect()
	loc := func(resp *http.Response) string { u, _ := url.QueryUnescape(resp.Header.Get("Location")); return u }

	// the page offers a Vanguard fetch for an APIR-coded fund
	resp, _ := http.Get(srv.URL + "/prices")
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(body.String(), "Fetch from Vanguard now") || !strings.Contains(body.String(), "VAN0108AU") {
		t.Error("prices page should offer a Vanguard fetch")
	}

	// fetch now
	resp, _ = c.PostForm(srv.URL+"/prices/fetch", url.Values{"symbol": {"VAN0108AU"}})
	if l := loc(resp); !strings.Contains(l, "Fetched 2 unit prices for VAN0108AU") || !strings.Contains(l, "Latest NAV $1.5673") {
		t.Errorf("fetch: %s", l)
	}
	// import a Vanguard-style export; the manual import overrides the fetched price on the same date
	xl := makeXLSX(t, vanguardLikeSheet(), "")
	buf, ct := multipartBody(t, map[string]string{"symbol": "VAN0108AU"}, "Prices History.xlsx", xl)
	req, _ := http.NewRequest("POST", srv.URL+"/prices/import", buf)
	req.Header.Set("Content-Type", ct)
	resp, _ = c.Do(req)
	if l := loc(resp); !strings.Contains(l, "Imported 4 prices for VAN0108AU (2014-11-19 to 2026-11-12) from the NAV column") {
		t.Errorf("import: %s", l)
	}
	a.st().View(func(db *DB) {
		if len(db.Manual["VAN0108AU"]) != 4 || db.Manual["VAN0108AU"]["2014-11-19"] != 1.3261 {
			t.Errorf("manual: %v", db.Manual["VAN0108AU"])
		}
	})
	// unknown fund / bad file / bad port
	buf, ct = multipartBody(t, map[string]string{"symbol": "NOPE"}, "x.xlsx", xl)
	req, _ = http.NewRequest("POST", srv.URL+"/prices/import", buf)
	req.Header.Set("Content-Type", ct)
	resp, _ = c.Do(req)
	if !strings.Contains(loc(resp), "err=") {
		t.Errorf("unknown fund should fail: %s", loc(resp))
	}
	buf, ct = multipartBody(t, map[string]string{"symbol": "VAN0108AU"}, "x.csv", []byte("hello"))
	req, _ = http.NewRequest("POST", srv.URL+"/prices/import", buf)
	req.Header.Set("Content-Type", ct)
	resp, _ = c.Do(req)
	if !strings.Contains(loc(resp), "import failed") {
		t.Errorf("bad file: %s", loc(resp))
	}
	resp, _ = c.PostForm(srv.URL+"/prices/port", url.Values{"symbol": {"VAN0108AU"}, "port_id": {"abc"}})
	if !strings.Contains(loc(resp), "err=") {
		t.Errorf("bad port: %s", loc(resp))
	}
	resp, _ = c.PostForm(srv.URL+"/prices/port", url.Values{"symbol": {"VAN0108AU"}, "port_id": {"8133"}})
	a.st().View(func(db *DB) {
		if db.Instruments["VAN0108AU"].VanguardPort != "8133" || db.PriceAt["vg:VAN0108AU"] != "" {
			t.Errorf("link not saved / refetch not scheduled: %q %q", db.Instruments["VAN0108AU"].VanguardPort, db.PriceAt["vg:VAN0108AU"])
		}
	})
	// the live endpoint answers
	resp, _ = http.Get(srv.URL + "/api/live")
	b2 := new(bytes.Buffer)
	_, _ = b2.ReadFrom(resp.Body)
	resp.Body.Close()
	if !strings.Contains(b2.String(), `"open":`) || !strings.Contains(b2.String(), `"intervalSeconds":30`) {
		t.Errorf("/api/live: %s", b2.String())
	}
}

func TestDashboardShowsTheLiveChip(t *testing.T) {
	a := liveApp(t)
	open := utc(2026, 11, 10, 0, 30)
	liveNow = func() time.Time { return open }
	t.Cleanup(func() { liveNow = time.Now })
	srv := httptest.NewServer(a.routes())
	defer srv.Close()
	get := func() string {
		resp, err := http.Get(srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(resp.Body)
		return b.String()
	}
	body := get()
	for _, want := range []string{`id="live-chip"`, `data-open="1"`, "ASX open", `id="live-stats"`, `id="live-holdings"`, "/static/live.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	_ = a.st().Update(func(db *DB) error { db.RefreshErr = "Yahoo down"; return nil })
	if body = get(); !strings.Contains(body, "The last price refresh had problems: Yahoo down") {
		t.Error("a failed refresh should be visible on the dashboard")
	}
	a.st().SetLive(map[string]Quote{"VGS.AX": {Price: 170.5, At: open.Unix() - 30, Date: "2026-11-10", Source: "yahoo"}}, LiveStatus{UpdatedAt: open.Unix() - 30}, false)
	if body = get(); !strings.Contains(body, `class="livedot"`) {
		t.Error("a live-priced holding should carry the live dot")
	}
}
