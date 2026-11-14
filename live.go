package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // exchange time zones must work on machines (Windows) with no zoneinfo database
)

// Live prices.
//
// While a market is open the app polls a lightweight quote for every listed holding (one small request
// each, not five years of history) and the dashboard refreshes itself from them. When the market is closed
// nothing is fetched: the last quote and the saved daily history are used. Daily history is saved in the data
// file and only topped up incrementally (see Refresh), so a restart or a refresh never re-downloads it.
//
// Sources: Yahoo Finance for ASX and US prices; for ASX symbols the ASX's own research API is a second,
// indicative source (bid/ask mid) if Yahoo is rate-limiting or down. Failures are recorded and shown on the
// dashboard instead of vanishing into a log.

var (
	yahooBase = "https://query1.finance.yahoo.com"
	asxBase   = "https://asx.api.markitdigital.com"
)

const liveInterval = 30 * time.Second

// liveNow is the clock the live loop uses; tests replace it.
var liveNow = time.Now

type Quote struct {
	Price, Prev float64
	At          int64  // unix time of the price
	Date        string // trading date in the exchange's time zone
	Source      string // yahoo | asx
	Start, End  int64  // the regular session this price belongs to, when the source says
}

type LiveStatus struct {
	OpenMarkets []string
	UpdatedAt   int64 // last time any quote was fetched successfully
	CheckedAt   int64
	Err         string // quotes couldn't be fetched (shown to the user)
	Note        string // informational, e.g. "using ASX indicative prices"
}

// ---------- market hours ----------

type tradingSession struct {
	Name, TZ       string
	OpenH, OpenM   int
	CloseH, CloseM int
}

var tradingSessions = map[string]tradingSession{
	MarketASX: {"ASX", "Australia/Sydney", 10, 0, 16, 10}, // includes the closing auction
	MarketUS:  {"US markets", "America/New_York", 9, 30, 16, 0},
}

func inZone(tz string, t time.Time) time.Time {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return t
	}
	return t.In(loc)
}

// marketOpenByClock says whether the market's regular hours are on (weekdays only). Public holidays are not in
// this table; a quote's own session window corrects for them (see marketOpen).
func marketOpenByClock(market string, now time.Time) bool {
	s, ok := tradingSessions[market]
	if !ok {
		return false
	}
	t := inZone(s.TZ, now)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	return m >= s.OpenH*60+s.OpenM && m < s.CloseH*60+s.CloseM
}

// marketOpen combines the clock with the session window that Yahoo reports with each quote: on a public holiday
// the window belongs to another day, so the market is treated as closed.
func marketOpen(market string, q *Quote, now time.Time) bool {
	if !marketOpenByClock(market, now) {
		return false
	}
	if q != nil && q.End > 0 {
		if n := now.Unix(); n < q.Start || n > q.End+600 {
			return false
		}
	}
	return true
}

// heldListed returns listed instruments the investor holds (or, when all is true, has ever traded).
func heldListed(db *DB, all bool) map[string]*Instrument {
	units := map[string]float64{}
	seen := map[string]bool{}
	for _, t := range db.Txns {
		seen[t.Symbol] = true
		switch t.Type {
		case "buy", "drp", "award":
			units[t.Symbol] += t.Units
		case "sell":
			units[t.Symbol] -= t.Units
		}
	}
	out := map[string]*Instrument{}
	for sym, inst := range db.Instruments {
		if inst.Market == MarketManaged || !seen[sym] {
			continue
		}
		if all || units[sym] > 1e-9 {
			out[sym] = inst
		}
	}
	return out
}

func openMarkets(db *DB, now time.Time) []string {
	set := map[string]bool{}
	for sym, inst := range heldListed(db, false) {
		var q *Quote
		if v, ok := db.Quotes[sym]; ok {
			q = &v
		}
		if marketOpen(inst.Market, q, now) {
			set[tradingSessions[inst.Market].Name] = true
		}
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ---------- fetching ----------

// Yahoo sometimes answers 429 (too many requests) or 5xx; back off so we don't make it worse, and use the
// secondary source meanwhile.
var yb struct {
	sync.Mutex
	until time.Time
	step  time.Duration
}

func yahooAllowed() bool {
	yb.Lock()
	defer yb.Unlock()
	return time.Now().After(yb.until)
}

func yahooFailed(err error) {
	msg := err.Error()
	if !(strings.Contains(msg, "HTTP 429") || strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "HTTP 5")) {
		return
	}
	yb.Lock()
	defer yb.Unlock()
	if yb.step < 30*time.Second {
		yb.step = 30 * time.Second
	} else if yb.step < 5*time.Minute {
		yb.step *= 2
	}
	yb.until = time.Now().Add(yb.step)
}

func yahooOK() {
	yb.Lock()
	yb.step, yb.until = 0, time.Time{}
	yb.Unlock()
}

var errBackoff = errors.New("Yahoo Finance is rate-limiting requests; waiting before retrying")

// fetchYahooQuote gets the latest price with one small request (today's chart only).
func fetchYahooQuote(sym string) (Quote, error) {
	u := fmt.Sprintf("%s/v8/finance/chart/%s?range=1d&interval=1d", yahooBase, url.PathEscape(sym))
	b, err := httpGet(u)
	if err != nil {
		return Quote{}, fmt.Errorf("%s: %w", sym, err)
	}
	var r struct {
		Chart struct {
			Result []struct {
				Meta struct {
					Price     float64 `json:"regularMarketPrice"`
					Time      int64   `json:"regularMarketTime"`
					ChartPrev float64 `json:"chartPreviousClose"`
					Prev      float64 `json:"previousClose"`
					Gmtoffset int     `json:"gmtoffset"`
					Period    struct {
						Regular struct {
							Start int64 `json:"start"`
							End   int64 `json:"end"`
						} `json:"regular"`
					} `json:"currentTradingPeriod"`
				} `json:"meta"`
			} `json:"result"`
			Error *struct {
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Quote{}, fmt.Errorf("%s: bad response: %w", sym, err)
	}
	if len(r.Chart.Result) == 0 {
		msg := "no data"
		if r.Chart.Error != nil {
			msg = r.Chart.Error.Description
		}
		return Quote{}, fmt.Errorf("%s: %s", sym, msg)
	}
	m := r.Chart.Result[0].Meta
	if m.Price <= 0 || m.Time <= 0 {
		return Quote{}, fmt.Errorf("%s: no current price", sym)
	}
	prev := m.ChartPrev
	if prev <= 0 {
		prev = m.Prev
	}
	return Quote{Price: m.Price, Prev: prev, At: m.Time, Source: "yahoo",
		Date:  time.Unix(m.Time, 0).UTC().Add(time.Duration(m.Gmtoffset) * time.Second).Format(dateFmt),
		Start: m.Period.Regular.Start, End: m.Period.Regular.End}, nil
}

// fetchASXQuote is the secondary source for ASX symbols: the ASX's research API, which carries the current
// bid and ask. The midpoint is indicative (a little behind a trade price) but keeps prices moving when Yahoo can't.
func fetchASXQuote(code string) (Quote, error) {
	if !marketOpenByClock(MarketASX, liveNow()) { // bid/ask are only meaningful while trading; when closed the saved close is the price
		return Quote{}, fmt.Errorf("%s: ASX is closed, so there is no indicative price", code)
	}
	var lastErr error
	for _, kind := range []string{"etfs", "companies"} {
		b, err := httpGet(fmt.Sprintf("%s/asx-research/1.0/%s/%s/key-statistics", asxBase, kind, url.PathEscape(strings.ToLower(code))))
		if err != nil {
			lastErr = err
			continue
		}
		type px struct {
			Ask   float64 `json:"priceAsk"`
			Bid   float64 `json:"priceBid"`
			Close float64 `json:"priceClose"`
		}
		var r struct {
			Data struct {
				px
				Info *px `json:"shareInformation"`
			} `json:"data"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			lastErr = err
			continue
		}
		p := r.Data.px
		if r.Data.Info != nil {
			p = *r.Data.Info
		}
		price := p.Close
		if p.Ask > 0 && p.Bid > 0 {
			price = (p.Ask + p.Bid) / 2
		}
		if price <= 0 {
			lastErr = fmt.Errorf("no price")
			continue
		}
		now := liveNow()
		return Quote{Price: price, Prev: p.Close, At: now.Unix(), Date: inZone("Australia/Sydney", now).Format(dateFmt), Source: "asx"}, nil
	}
	return Quote{}, fmt.Errorf("%s: ASX quote unavailable: %v", code, lastErr)
}

func fetchQuote(sym string) (Quote, string, error) {
	var err error
	if yahooAllowed() {
		var q Quote
		if q, err = fetchYahooQuote(sym); err == nil {
			yahooOK()
			return q, "", nil
		}
		yahooFailed(err)
	} else {
		err = errBackoff
	}
	if strings.HasSuffix(sym, ".AX") {
		if q, e2 := fetchASXQuote(strings.TrimSuffix(sym, ".AX")); e2 == nil {
			return q, "Yahoo Finance is unavailable, so ASX indicative prices (bid/ask midpoint) are being used", nil
		}
	}
	return Quote{}, "", err
}

// LiveTick fetches quotes for every listed holding whose market is open (or all of them when force is true,
// e.g. when the user presses Refresh). It returns the number of quotes updated.
func (a *App) LiveTick(force bool) (int, error) {
	if a.inTest() {
		return 0, nil
	}
	now := liveNow()
	type job struct{ sym string }
	var jobs []job
	var open []string
	st := a.st()
	st.View(func(db *DB) {
		open = openMarkets(db, now)
		for sym, inst := range heldListed(db, force) {
			var q *Quote
			if v, ok := db.Quotes[sym]; ok {
				q = &v
			}
			if force || marketOpen(inst.Market, q, now) {
				jobs = append(jobs, job{sym})
			}
		}
	})
	prev := LiveStatus{}
	st.View(func(db *DB) { prev = db.Live })
	if len(jobs) == 0 {
		prev.OpenMarkets, prev.CheckedAt = open, now.Unix()
		st.SetLive(nil, prev, false)
		return 0, nil
	}

	type res struct {
		sym  string
		q    Quote
		note string
		err  error
	}
	out := make(chan res, len(jobs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(sym string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			q, note, err := fetchQuote(sym)
			out <- res{sym, q, note, err}
		}(j.sym)
	}
	wg.Wait()
	close(out)

	quotes := map[string]Quote{}
	var errs []string
	note := ""
	for r := range out {
		if r.err != nil {
			errs = append(errs, r.err.Error())
			continue
		}
		quotes[r.sym] = r.q
		if r.note != "" {
			note = r.note
		}
	}
	status := LiveStatus{OpenMarkets: open, UpdatedAt: prev.UpdatedAt, CheckedAt: now.Unix(), Note: note}
	if len(quotes) > 0 {
		status.UpdatedAt = now.Unix()
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		status.Err = fmt.Sprintf("Couldn't get live prices for %d of %d holdings (%s)", len(errs), len(jobs), shorten(errs[0], 110))
	}
	save := force || now.Sub(a.liveSaved) > time.Minute || len(open) == 0
	if save {
		a.liveSaved = now
	}
	st.SetLive(quotes, status, save)
	if len(quotes) == 0 && len(errs) > 0 {
		return 0, errors.New(strings.Join(errs, "; "))
	}
	return len(quotes), nil
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (a *App) liveLoop() {
	tick := time.NewTicker(liveInterval)
	defer tick.Stop()
	for range tick.C {
		_, _ = a.LiveTick(false) // errors are recorded in the live status and shown on the dashboard
	}
}

// liveAPI feeds the dashboard's auto-refresh.
func (a *App) liveAPI(w http.ResponseWriter, r *http.Request) {
	var st LiveStatus
	var open []string
	test := a.inTest()
	a.st().View(func(db *DB) {
		st = db.Live
		open = openMarkets(db, liveNow())
	})
	if test {
		open = nil
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"open": len(open) > 0, "markets": open, "updatedAt": st.UpdatedAt, "checkedAt": st.CheckedAt,
		"error": st.Err, "note": st.Note, "test": test, "intervalSeconds": int(liveInterval.Seconds()),
	})
}
