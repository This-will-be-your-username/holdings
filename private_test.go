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

func privDB() *DB {
	db := &DB{}
	db.init()
	db.Instruments["PVT-ACME"] = &Instrument{Symbol: "PVT-ACME", Name: "Acme Robotics Pty Ltd", Currency: "AUD", Market: MarketManaged, Private: true, SharesOut: 1000000}
	db.Txns = []Txn{{ID: "1", Date: "2024-03-01", Symbol: "PVT-ACME", Type: "buy", Units: 2000, Price: 1.00}}
	return db
}

func TestCheckpointFromPriceAndValuation(t *testing.T) {
	db := privDB()
	if _, err := AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2025-01-10", Price: 1.5}); err != nil {
		t.Fatal(err)
	}
	cp, err := AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2026-02-01", Valuation: 3000000})
	if err != nil || cp.Price != 3 {
		t.Fatalf("valuation / shares: %v %+v", err, cp)
	}
	if db.Manual["PVT-ACME"]["2026-02-01"] != 3 || db.Manual["PVT-ACME"]["2025-01-10"] != 1.5 {
		t.Fatalf("price series %v", db.Manual["PVT-ACME"])
	}
	// the same date replaces rather than duplicates
	AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2026-02-01", Price: 2.5})
	if len(db.Checkpoints["PVT-ACME"]) != 2 || db.Manual["PVT-ACME"]["2026-02-01"] != 2.5 {
		t.Fatalf("replace: %+v", db.Checkpoints["PVT-ACME"])
	}
	// the holding is valued at the latest checkpoint
	res := Compute(db, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if len(res.Holdings) != 1 || res.Holdings[0].ValueAUD != 5000 || !res.Holdings[0].Private {
		t.Fatalf("holding %+v", res.Holdings)
	}
	id := db.Checkpoints["PVT-ACME"][1].ID
	if !DeleteCheckpoint(db, "PVT-ACME", id) || db.Manual["PVT-ACME"]["2026-02-01"] != 0 || len(db.Manual["PVT-ACME"]) != 1 {
		t.Fatalf("delete: %v", db.Manual["PVT-ACME"])
	}
}

func TestCheckpointValuationNeedsShares(t *testing.T) {
	db := privDB()
	db.Instruments["PVT-ACME"].SharesOut = 0
	if _, err := AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2026-02-01", Valuation: 3000000}); err == nil || !strings.Contains(err.Error(), "shares on issue") {
		t.Fatalf("expected a shares-on-issue error, got %v", err)
	}
	// once an earlier checkpoint has both price and valuation, a later valuation can be scaled from it
	db.Checkpoints["PVT-ACME"] = []Checkpoint{{ID: "x", Date: "2025-01-01", Price: 2, Valuation: 1000000}}
	cp, err := AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2026-01-01", Valuation: 1500000})
	if err != nil || cp.Price != 3 || !strings.Contains(cp.Note, "scaled") {
		t.Fatalf("scaled: %v %+v", err, cp)
	}
	if _, err := AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2026-01-01"}); err == nil {
		t.Fatal("empty checkpoint should fail")
	}
	if _, err := AddCheckpoint(db, "NOPE", Checkpoint{Date: "2026-01-01", Price: 1}); err == nil {
		t.Fatal("unknown company should fail")
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]float64{"1,200,000": 1.2e6, "350m": 350e6, "4.2b": 4.2e9, "$4.2 billion": 4.2e9, "1.5bn": 1.5e9, "": 0, "abc": 0} {
		if got := parseAmount(in); got != want {
			t.Errorf("parseAmount(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExtractValuationsAndDates(t *testing.T) {
	c := extractValuations("Acme raised $40 million, valuing the firm at A$1.2 billion.", "news.com.au")
	if len(c) != 1 || c[0].Amount != 1.2e9 || c[0].Currency != "AUD" {
		t.Fatalf("A: %+v", c)
	}
	c = extractValuations("Acme closes round at a US$350m valuation", "example.com")
	if len(c) != 1 || c[0].Amount != 350e6 || c[0].Currency != "USD" {
		t.Fatalf("B: %+v", c)
	}
	if c := extractValuations("Acme valued at $50 and rising", "x.com"); len(c) != 0 {
		t.Fatalf("tiny figure should be ignored: %+v", c)
	}
	for in, want := range map[string]string{"on 5 March 2026 the": "2026-03-05", "Mar 5, 2026": "2026-03-05", "2026-03-05": "2026-03-05", "in September 2025": "2025-09-01", "no date": ""} {
		if got := findDate(in); got != want {
			t.Errorf("findDate(%q) = %q, want %q", in, got, want)
		}
	}
	if !mentions("Acme Robotics raises funds", "Acme Robotics Pty Ltd") || mentions("Other Robotics raises funds", "Acme Robotics Pty Ltd") {
		t.Fatal("name matching")
	}
}

const fakeResults = `<html><body>
<div class="result"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fwww.news.example%2Facme&rut=x">Acme Robotics raises $40m</a>
<a class="result__snippet" href="x">On 5 March 2026, <b>Acme Robotics</b> said it raised $40 million at a US$350 million valuation.</a></div>
<div class="result"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fother.example%2Fz&rut=x">Unrelated Corp valued at $9 billion</a>
<a class="result__snippet" href="x">Unrelated Corp is valued at $9 billion.</a></div>
<div class="result"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fold.example%2Facme&rut=x">Acme Robotics in 2024</a>
<a class="result__snippet" href="x">In June 2024 Acme Robotics was valued at US$120 million after its seed round.</a></div>
</body></html>`

func fakeSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fakeResults)
	}))
	old := searchBase
	searchBase = srv.URL
	t.Cleanup(func() { searchBase = old; srv.Close() })
}

func TestSearchValuations(t *testing.T) {
	fakeSearch(t)
	cands, err := SearchValuations("Acme Robotics Pty Ltd")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected the two Acme results (the unrelated company is dropped), got %+v", cands)
	}
	if cands[0].Date != "2026-03-05" || cands[0].Amount != 350e6 || cands[0].Currency != "USD" || cands[0].URL != "https://www.news.example/acme" {
		t.Fatalf("newest first: %+v", cands[0])
	}
	if cands[1].Date != "2024-06-01" {
		t.Fatalf("second: %+v", cands[1])
	}
	if _, err := SearchValuations(" "); err == nil {
		t.Fatal("empty term should fail")
	}
	searchBase = "http://127.0.0.1:1/"
	if _, err := SearchValuations("Acme"); err == nil {
		t.Fatal("unreachable search should return an error")
	}
}

func TestPrivatePagesEndToEnd(t *testing.T) {
	fakeSearch(t)
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		return r
	}
	if r := post("/private/company", url.Values{"name": {"Acme Robotics Pty Ltd"}, "currency": {"AUD"}, "shares_out": {"1m"}}); r.Code != 303 {
		t.Fatalf("add company: %d", r.Code)
	}
	var sym string
	a.st().View(func(db *DB) {
		for s := range db.Instruments {
			sym = s
		}
	})
	if sym == "" {
		t.Fatal("company not created")
	}
	// shares are recorded through the normal transaction form
	if r := post("/transactions", url.Values{"market": {"PRIVATE"}, "symbol": {sym}, "type": {"buy"}, "date": {"2024-03-01"}, "units": {"2000"}, "price": {"1"}}); r.Code != 303 {
		t.Fatalf("add txn: %d %s", r.Code, r.Body.String())
	}
	body := get("/private").Body.String()
	if !strings.Contains(body, "Acme Robotics Pty Ltd") || !strings.Contains(body, "2,000") {
		t.Fatalf("page missing company or holding:\n%s", body)
	}
	r := post("/private/search", url.Values{"symbol": {sym}})
	if r.Code != 200 || !strings.Contains(r.Body.String(), "news.example") {
		t.Fatalf("search page: %d", r.Code)
	}
	if !strings.Contains(r.Body.String(), "convert to AUD first") { // the US$ result is not offered as a ready checkpoint
		t.Fatalf("currency mismatch not flagged")
	}
	if r := post("/private/checkpoint", url.Values{"symbol": {sym}, "date": {"2026-03-05"}, "valuation": {"3m"}, "source": {"annual report"}}); r.Code != 303 || strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("checkpoint: %d %s", r.Code, r.Header().Get("Location"))
	}
	dash := get("/").Body.String()
	if !strings.Contains(dash, "Acme Robotics") {
		t.Fatalf("dashboard should list the private holding")
	}
	var id string
	a.st().View(func(db *DB) { id = db.Checkpoints[sym][0].ID })
	if r := post("/private/checkpoint/delete", url.Values{"symbol": {sym}, "id": {id}}); r.Code != 303 {
		t.Fatalf("delete: %d", r.Code)
	}
	// fund prices page must not list private companies
	if strings.Contains(get("/prices").Body.String(), "Acme Robotics") {
		t.Fatal("private company leaked onto the fund prices page")
	}
}

func TestStalePrivateValuationWarns(t *testing.T) {
	db := privDB()
	AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: "2024-06-01", Price: 1.2})
	res := Compute(db, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "last valued on 2024-06-01") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a stale-valuation warning, got %v", res.Warnings)
	}
}
