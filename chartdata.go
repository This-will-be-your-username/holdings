package main

import (
	"math"
	"net/http"
	"sort"
	"time"
)

// The chart explorer gets one aligned data set - a value, a net-invested figure and an AUD unit price per holding for
// every date - and does the picking, ranges, highlighting and comparisons in the browser.

type ExplorerSeries struct {
	Sym      string     `json:"sym"`
	Name     string     `json:"name"`
	Value    []*float64 `json:"value"`    // market value in AUD (null before the first transaction)
	Invested []*float64 `json:"invested"` // cumulative money put in, net of sales
	Price    []*float64 `json:"price"`    // AUD value of one unit (null before the first transaction)
	Latest   float64    `json:"latest"`   // value on the last date, to order the picker
	Private  bool       `json:"private,omitempty"`
}

type Explorer struct {
	T     []int64          `json:"t"` // UTC milliseconds
	Total ExplorerSeries   `json:"total"`
	Items []ExplorerSeries `json:"items"`
}

func roundPtr(v float64, dp int) *float64 {
	p := math.Pow10(dp)
	x := math.Round(v*p) / p
	return &x
}

// maxExplorerPoints keeps the data set small: longer histories are sampled weekly instead of daily.
const maxExplorerPoints = 2500

func buildExplorer(db *DB, res *Result, now time.Time) *Explorer {
	if len(res.Rows) == 0 {
		return nil
	}
	start, _ := time.Parse(dateFmt, res.Rows[0].Date)
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	step := 1
	if int(end.Sub(start).Hours()/24) > maxExplorerPoints {
		step = 7
	}
	var dates []time.Time
	for d := start; d.Before(end); d = d.AddDate(0, 0, step) {
		dates = append(dates, d)
	}
	dates = append(dates, end)

	var syms []string
	idx := map[string]int{}
	for _, r := range res.Rows {
		if _, ok := idx[r.Symbol]; !ok && db.Instruments[r.Symbol] != nil {
			idx[r.Symbol] = len(syms)
			syms = append(syms, r.Symbol)
		}
	}
	ex := &Explorer{}
	for _, s := range syms {
		in := db.Instruments[s]
		name := in.Name
		if name == "" {
			name = s
		}
		ex.Items = append(ex.Items, ExplorerSeries{Sym: short(s), Name: name, Private: in.Private})
	}
	ex.Total = ExplorerSeries{Sym: "TOTAL", Name: "Total portfolio"}

	units := make([]float64, len(syms))
	inv := make([]float64, len(syms))
	last := make([]float64, len(syms))
	started := make([]bool, len(syms))
	fxFor := func(ccy, ds string) float64 {
		if ccy == "" || ccy == "AUD" {
			return 1
		}
		s := res.fxs[ccy]
		if s == nil || len(s.D) == 0 {
			return 1
		}
		if x, _, ok := s.At(ds); ok {
			return x
		}
		return s.V[0]
	}
	i := 0
	for _, d := range dates {
		ds := d.Format(dateFmt)
		for i < len(res.Rows) && res.Rows[i].Date <= ds {
			r := res.Rows[i]
			if k, ok := idx[r.Symbol]; ok {
				started[k] = true
				switch r.Type {
				case "buy", "drp", "award":
					units[k] += r.Units
				case "sell":
					units[k] -= r.Units
				}
				if r.Type == "buy" || r.Type == "award" || r.Type == "sell" {
					inv[k] -= r.CashFlowAUD
				}
				last[k] = r.Price
			}
			i++
		}
		ex.T = append(ex.T, d.UnixMilli())
		var tv, ti float64
		for k, s := range syms {
			it := &ex.Items[k]
			if !started[k] {
				it.Value, it.Invested, it.Price = append(it.Value, nil), append(it.Invested, nil), append(it.Price, nil)
				continue
			}
			p := last[k]
			if x, _, ok := res.px[s].At(ds); ok {
				p = x
			}
			fx := fxFor(db.Instruments[s].Currency, ds)
			v := 0.0
			if units[k] > 1e-9 {
				v = units[k] * p * fx
			}
			it.Value = append(it.Value, roundPtr(v, 2))
			it.Invested = append(it.Invested, roundPtr(inv[k], 2))
			it.Price = append(it.Price, roundPtr(p*fx, 4))
			tv += v
			ti += inv[k]
		}
		ex.Total.Value = append(ex.Total.Value, roundPtr(tv, 2))
		ex.Total.Invested = append(ex.Total.Invested, roundPtr(ti, 2))
	}
	for k := range ex.Items {
		if n := len(ex.Items[k].Value); n > 0 && ex.Items[k].Value[n-1] != nil {
			ex.Items[k].Latest = *ex.Items[k].Value[n-1]
		}
	}
	sort.SliceStable(ex.Items, func(a, b int) bool { return ex.Items[a].Latest > ex.Items[b].Latest })
	if n := len(ex.Total.Value); n > 0 {
		ex.Total.Latest = *ex.Total.Value[n-1]
	}
	return ex
}

func (a *App) chartsPage(w http.ResponseWriter, r *http.Request) {
	var ex *Explorer
	a.st().View(func(db *DB) {
		now := liveNow()
		ex = buildExplorer(db, Compute(db, now), now)
	})
	a.render(w, r, "charts", map[string]any{"Explorer": ex, "Empty": ex == nil})
}
