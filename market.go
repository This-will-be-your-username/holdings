package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var httpc = &http.Client{Timeout: 25 * time.Second}

func httpGet(u string) ([]byte, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; fundtrack/1.0)")
	req.Header.Set("Accept", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	return b, nil
}

// QuoteData is what we pull from Yahoo Finance's public chart endpoint.
type QuoteData struct {
	Name     string
	Currency string
	Closes   map[string]float64
	Divs     map[string]float64 // ex-date -> per-unit amount
}

// fetchYahoo pulls daily closes + dividend events for a symbol (ASX symbols
// carry the .AX suffix). Endpoint is unofficial but keyless.
func fetchYahoo(symbol string, from time.Time) (*QuoteData, error) {
	u := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&events=div",
		url.PathEscape(symbol), from.Unix(), time.Now().Add(24*time.Hour).Unix())
	b, err := httpGet(u)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", symbol, err)
	}
	var r struct {
		Chart struct {
			Result []struct {
				Meta struct {
					Currency  string `json:"currency"`
					LongName  string `json:"longName"`
					ShortName string `json:"shortName"`
					Gmtoffset int    `json:"gmtoffset"`
				} `json:"meta"`
				Timestamp []int64 `json:"timestamp"`
				Events    struct {
					Dividends map[string]struct {
						Amount float64 `json:"amount"`
						Date   int64   `json:"date"`
					} `json:"dividends"`
				} `json:"events"`
				Indicators struct {
					Quote []struct {
						Close []*float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error *struct {
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: bad response: %w", symbol, err)
	}
	if len(r.Chart.Result) == 0 {
		msg := "no data"
		if r.Chart.Error != nil {
			msg = r.Chart.Error.Description
		}
		return nil, fmt.Errorf("%s: %s", symbol, msg)
	}
	res := r.Chart.Result[0]
	off := time.Duration(res.Meta.Gmtoffset) * time.Second
	day := func(ts int64) string { return time.Unix(ts, 0).UTC().Add(off).Format(dateFmt) }
	q := &QuoteData{
		Name:     res.Meta.LongName,
		Currency: strings.ToUpper(res.Meta.Currency),
		Closes:   map[string]float64{},
		Divs:     map[string]float64{},
	}
	if q.Name == "" {
		q.Name = res.Meta.ShortName
	}
	if len(res.Indicators.Quote) > 0 {
		cl := res.Indicators.Quote[0].Close
		for i, ts := range res.Timestamp {
			if i < len(cl) && cl[i] != nil && *cl[i] > 0 {
				q.Closes[day(ts)] = *cl[i]
			}
		}
	}
	for _, d := range res.Events.Dividends {
		q.Divs[day(d.Date)] = d.Amount
	}
	return q, nil
}

// fetchASXDiv returns the latest distribution (ex/pay date, per-unit amount,
// franking %) from the ASX's public research API. ETFs and companies use
// different paths and shapes.
func fetchASXDiv(code string) (string, DivInfo, error) {
	type d struct {
		Ex      string  `json:"dateExDate"`
		Pay     string  `json:"datePayDate"`
		Amount  float64 `json:"dividend"`
		Franked float64 `json:"frankingPercent"`
	}
	var r struct {
		Data struct {
			d
			Dividends *d `json:"dividends"`
		} `json:"data"`
	}
	var lastErr error
	for _, kind := range []string{"etfs", "companies"} {
		b, err := httpGet(fmt.Sprintf("https://asx.api.markitdigital.com/asx-research/1.0/%s/%s/key-statistics", kind, url.PathEscape(strings.ToLower(code))))
		if err != nil {
			lastErr = err
			continue
		}
		r.Data.Dividends = nil
		r.Data.d = d{}
		if err := json.Unmarshal(b, &r); err != nil {
			lastErr = err
			continue
		}
		x := r.Data.d
		if r.Data.Dividends != nil {
			x = *r.Data.Dividends
		}
		if x.Ex == "" {
			lastErr = fmt.Errorf("no distribution data")
			continue
		}
		cut := func(s string) string {
			if len(s) >= 10 {
				return s[:10]
			}
			return s
		}
		return cut(x.Ex), DivInfo{Pay: cut(x.Pay), PerUnit: x.Amount, Franking: x.Franked}, nil
	}
	return "", DivInfo{}, lastErr
}

// fetchFX returns AUD per 1 unit of ccy from the ECB via Frankfurter (keyless).
// Long ranges are returned weekly by the API, so we request in <90-day chunks
// to keep daily resolution.
func fetchFX(ccy string, from, to time.Time) (map[string]float64, error) {
	out := map[string]float64{}
	for start := from; !start.After(to); start = start.AddDate(0, 0, 80) {
		end := start.AddDate(0, 0, 79)
		if end.After(to) {
			end = to
		}
		u := fmt.Sprintf("https://api.frankfurter.dev/v1/%s..%s?base=%s&symbols=AUD",
			start.Format(dateFmt), end.Format(dateFmt), url.QueryEscape(ccy))
		b, err := httpGet(u)
		if err != nil {
			return out, fmt.Errorf("FX %s: %w", ccy, err)
		}
		var r struct {
			Rates map[string]map[string]float64 `json:"rates"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return out, fmt.Errorf("FX %s: %w", ccy, err)
		}
		for d, m := range r.Rates {
			if v := m["AUD"]; v > 0 {
				out[d] = v
			}
		}
	}
	return out, nil
}

// Refresh pulls prices, dividends and FX for every held instrument.
func (a *App) Refresh() (string, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()

	type job struct {
		sym  string
		from time.Time
	}
	var jobs []job
	fxFrom := map[string]time.Time{}
	fxHave := map[string][2]string{} // ccy -> first,last stored date
	a.store.View(func(db *DB) {
		earliest := map[string]string{}
		for _, t := range db.Txns {
			if e, ok := earliest[t.Symbol]; !ok || t.Date < e {
				earliest[t.Symbol] = t.Date
			}
		}
		for sym, e := range earliest {
			inst := db.Instruments[sym]
			if inst == nil || inst.Market == MarketManaged {
				continue
			}
			d, _ := time.Parse(dateFmt, e)
			jobs = append(jobs, job{sym, d.AddDate(0, 0, -10)})
			if inst.Currency != "AUD" {
				if f, ok := fxFrom[inst.Currency]; !ok || d.Before(f) {
					fxFrom[inst.Currency] = d
				}
			}
		}
		for ccy := range fxFrom {
			s := newSeries(db.FX[ccy])
			if len(s.D) > 0 {
				fxHave[ccy] = [2]string{s.D[0], s.D[len(s.D)-1]}
			}
		}
	})

	var errs []string
	quotes := map[string]*QuoteData{}
	for _, j := range jobs {
		q, err := fetchYahoo(j.sym, j.from)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		quotes[j.sym] = q
	}
	type asxDiv struct {
		ex   string
		info DivInfo
	}
	asx := map[string]asxDiv{}
	for _, j := range jobs {
		if _, ok := quotes[j.sym]; ok && strings.HasSuffix(j.sym, ".AX") {
			// best effort: franking % and pay date for the latest distribution
			if ex, info, err := fetchASXDiv(strings.TrimSuffix(j.sym, ".AX")); err == nil {
				asx[j.sym] = asxDiv{ex, info}
			}
		}
	}
	fxData := map[string]map[string]float64{}
	for ccy, e := range fxFrom {
		from := e.AddDate(0, 0, -10)
		if h, ok := fxHave[ccy]; ok && h[0] <= from.Format(dateFmt) {
			last, _ := time.Parse(dateFmt, h[1])
			from = last.AddDate(0, 0, -10) // incremental
		}
		m, err := fetchFX(ccy, from, time.Now())
		if err != nil {
			errs = append(errs, err.Error())
		}
		if len(m) > 0 {
			fxData[ccy] = m
		}
	}

	err := a.store.Update(func(db *DB) error {
		for sym, q := range quotes {
			if db.Prices[sym] == nil {
				db.Prices[sym] = map[string]float64{}
			}
			for d, v := range q.Closes {
				db.Prices[sym][d] = v
			}
			if db.Divs[sym] == nil {
				db.Divs[sym] = map[string]float64{}
			}
			for d, v := range q.Divs {
				db.Divs[sym][d] = v
			}
		}
		for sym, a := range asx {
			if db.DivInfo[sym] == nil {
				db.DivInfo[sym] = map[string]DivInfo{}
			}
			db.DivInfo[sym][a.ex] = a.info
		}
		for ccy, m := range fxData {
			if db.FX[ccy] == nil {
				db.FX[ccy] = map[string]float64{}
			}
			for d, v := range m {
				db.FX[ccy][d] = v
			}
		}
		db.LastRefresh = time.Now().Format(time.RFC3339)
		return nil
	})
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Refreshed %d instruments", len(quotes))
	if len(errs) > 0 {
		return msg, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return msg, nil
}

// resolveInstrument builds an Instrument, validating listed symbols against Yahoo.
func resolveInstrument(sym, market, name string) (*Instrument, error) {
	if market == MarketManaged {
		if name == "" {
			name = sym
		}
		return &Instrument{Symbol: sym, Name: name, Currency: "AUD", Market: market}, nil
	}
	q, err := fetchYahoo(sym, time.Now().AddDate(0, 0, -7))
	if err != nil {
		return nil, fmt.Errorf("could not find %q on Yahoo Finance (%v)", sym, err)
	}
	if q.Currency == "" {
		q.Currency = "AUD"
		if market == MarketUS {
			q.Currency = "USD"
		}
	}
	if name == "" {
		name = q.Name
	}
	return &Instrument{Symbol: sym, Name: name, Currency: q.Currency, Market: market}, nil
}

func normSymbol(sym, market string) string {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if market == MarketASX && sym != "" && !strings.Contains(sym, ".") {
		sym += ".AX"
	}
	return sym
}
