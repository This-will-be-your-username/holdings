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

const histYears = 5

// benchmarks are fetched for market context (symbol -> display name).
var benchmarks = map[string]string{
	"^AXJO":    "S&P/ASX 200",
	"^GSPC":    "S&P 500",
	"^VIX":     "VIX (US volatility)",
	"AUDUSD=X": "AUD / USD",
}

func httpGet(u string) ([]byte, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; holdings/1.0)")
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
	u := fmt.Sprintf("%s/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&events=div",
		yahooBase, url.PathEscape(symbol), from.Unix(), time.Now().Add(24*time.Hour).Unix())
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
		b, err := httpGet(fmt.Sprintf("%s/asx-research/1.0/%s/%s/key-statistics", asxBase, kind, url.PathEscape(strings.ToLower(code))))
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

var fxBase = "https://api.frankfurter.dev/v1"

// fetchFXLatest returns the most recent published rate (AUD per 1 unit of ccy) and the date it applies to. The ECB
// publishes once each business day, so this is the newest rate that exists.
func fetchFXLatest(ccy string) (string, float64, error) {
	b, err := httpGet(fmt.Sprintf("%s/latest?base=%s&symbols=AUD", fxBase, url.QueryEscape(ccy)))
	if err != nil {
		return "", 0, fmt.Errorf("FX %s: %w", ccy, err)
	}
	var r struct {
		Date  string             `json:"date"`
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Rates["AUD"] <= 0 || r.Date == "" {
		return "", 0, fmt.Errorf("FX %s: unexpected response", ccy)
	}
	return r.Date, r.Rates["AUD"], nil
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
		u := fmt.Sprintf("%s/%s..%s?base=%s&symbols=AUD", fxBase,
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

// resolveInstrument builds an Instrument, validating listed symbols against Yahoo.
func resolveInstrument(sym, market, name string) (*Instrument, error) {
	if market == MarketPrivate {
		if name == "" {
			name = sym
		}
		return &Instrument{Symbol: sym, Name: name, Currency: "AUD", Market: MarketManaged, Private: true}, nil
	}
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
