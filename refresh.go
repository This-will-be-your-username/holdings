package main

import (
	"fmt"
	"strings"
	"time"
)

// Refresh brings the saved data up to date. It is built to be cheap and honest:
//
//   - History is INCREMENTAL. Daily prices are saved in the data file, so after the first download only the last
//     ~10 days are requested (a few KB per holding) instead of five years every time.
//   - Secondary lookups (ASX distribution details, Vanguard fund prices) are throttled to a few times a day.
//   - Intraday prices are not handled here; see live.go.
//   - The outcome is recorded truthfully: "last refreshed" only moves when something actually updated, and any
//     failure is saved and shown on the dashboard rather than only logged.

func (a *App) Refresh() (string, error)    { return a.refresh(false) }
func (a *App) RefreshNow() (string, error) { return a.refresh(true) }

func storedRange(m map[string]float64) (first, last string) {
	for d := range m {
		if first == "" || d < first {
			first = d
		}
		if d > last {
			last = d
		}
	}
	return
}

// incrementalFrom decides where to start fetching: just after what is saved, or the full wanted range if the
// saved history doesn't reach back far enough yet.
func incrementalFrom(stored map[string]float64, want time.Time) time.Time {
	first, last := storedRange(stored)
	if last == "" || first > want.AddDate(0, 0, 7).Format(dateFmt) {
		return want
	}
	l, err := time.Parse(dateFmt, last)
	if err != nil {
		return want
	}
	return l.AddDate(0, 0, -10) // a short overlap picks up corrections and any days we missed
}

func (a *App) refresh(force bool) (string, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	if a.inTest() { // simulated data: fetching real prices would make the test transactions inconsistent
		return "Test mode: prices are simulated, nothing was fetched", nil
	}
	now := time.Now()

	type job struct {
		sym  string
		from time.Time
	}
	var jobs, benchJobs []job
	var managed []*Instrument
	asxDue := map[string]bool{}
	fxFrom := map[string]time.Time{}
	fxHave := map[string][2]string{}
	a.st().View(func(db *DB) {
		earliest := map[string]string{}
		for _, t := range db.Txns {
			if e, ok := earliest[t.Symbol]; !ok || t.Date < e {
				earliest[t.Symbol] = t.Date
			}
		}
		limit := now.AddDate(-histYears, 0, 0)
		for sym, e := range earliest {
			inst := db.Instruments[sym]
			if inst == nil {
				continue
			}
			d, _ := time.Parse(dateFmt, e)
			if inst.Currency != "AUD" && inst.Currency != "" { // every foreign holding needs rates, listed or not
				if f, ok := fxFrom[inst.Currency]; !ok || d.Before(f) {
					fxFrom[inst.Currency] = d
				}
			}
			if inst.Market == MarketManaged {
				continue
			}
			want := d.AddDate(0, 0, -10)
			if limit.Before(want) {
				want = limit // several years of history for trend analysis
			}
			jobs = append(jobs, job{sym, incrementalFrom(db.Prices[sym], want)})
			if at, err := time.Parse(time.RFC3339, db.PriceAt["asxdiv:"+sym]); strings.HasSuffix(sym, ".AX") && (force || err != nil || now.Sub(at) > 6*time.Hour) {
				asxDue[sym] = true
			}
		}
		if len(jobs) > 0 {
			for sym := range benchmarks {
				benchJobs = append(benchJobs, job{sym, incrementalFrom(db.Bench[sym], limit)})
			}
		}
		for ccy := range fxFrom {
			s := newSeries(db.FX[ccy])
			if len(s.D) > 0 {
				fxHave[ccy] = [2]string{s.D[0], s.D[len(s.D)-1]}
			}
		}
		managed = managedDue(db, force, now)
	})

	var errs []string
	fail := func(err error) { errs = append(errs, err.Error()) }

	hist := map[string]*QuoteData{}
	for _, j := range jobs {
		if !yahooAllowed() {
			fail(errBackoff)
			break
		}
		q, err := fetchYahoo(j.sym, j.from)
		if err != nil {
			yahooFailed(err)
			fail(err)
			continue
		}
		yahooOK()
		hist[j.sym] = q
	}
	bench := map[string]map[string]float64{}
	for _, j := range benchJobs {
		if !yahooAllowed() {
			break // context only; the next refresh catches up
		}
		q, err := fetchYahoo(j.sym, j.from)
		if err != nil {
			yahooFailed(err)
			fail(err)
			continue
		}
		bench[j.sym] = q.Closes
	}

	type asxDiv struct {
		ex   string
		info DivInfo
	}
	asx := map[string]asxDiv{}
	asxTried := map[string]bool{}
	for sym := range asxDue {
		if _, ok := hist[sym]; !ok {
			continue
		}
		asxTried[sym] = true                                                          // remembered even if the lookup fails, so a bad symbol isn't retried every refresh
		if ex, info, err := fetchASXDiv(strings.TrimSuffix(sym, ".AX")); err == nil { // best effort
			asx[sym] = asxDiv{ex, info}
		}
	}

	type vgResult struct {
		port, name string
		nav        map[string]float64
	}
	vg := map[string]vgResult{}
	for _, inst := range managed {
		f, err := vgResolve(inst)
		if err != nil {
			fail(err)
			continue
		}
		nav, err := fetchVGPrices(f.PortID)
		if err != nil {
			fail(err)
			continue
		}
		vg[inst.Symbol] = vgResult{f.PortID, f.Name, nav}
	}

	fxData := map[string]map[string]float64{}
	for ccy, e := range fxFrom {
		from := e.AddDate(0, 0, -10)
		if h, ok := fxHave[ccy]; ok && h[0] <= from.Format(dateFmt) {
			last, _ := time.Parse(dateFmt, h[1])
			from = last.AddDate(0, 0, -10) // incremental
		}
		m, err := fetchFX(ccy, from, now)
		if err != nil {
			fail(err)
		}
		if d, rate, lerr := fetchFXLatest(ccy); lerr == nil { // the newest published rate, even if the range call missed it
			if m == nil {
				m = map[string]float64{}
			}
			m[d] = rate
		} else if len(m) == 0 {
			fail(lerr)
		}
		if len(m) > 0 {
			fxData[ccy] = m
		}
	}

	stamp := now.Format(time.RFC3339)
	updated := len(hist) + len(vg)
	err := a.st().Update(func(db *DB) error {
		for sym, q := range hist {
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
			db.PriceAt[sym] = stamp
		}
		for sym, m := range bench {
			if db.Bench[sym] == nil {
				db.Bench[sym] = map[string]float64{}
			}
			for d, v := range m {
				db.Bench[sym][d] = v
			}
		}
		for sym, a := range asx {
			if db.DivInfo[sym] == nil {
				db.DivInfo[sym] = map[string]DivInfo{}
			}
			db.DivInfo[sym][a.ex] = a.info
		}
		for sym := range asxTried {
			db.PriceAt["asxdiv:"+sym] = stamp
		}
		for sym, r := range vg {
			if db.Prices[sym] == nil {
				db.Prices[sym] = map[string]float64{}
			}
			for d, v := range r.nav {
				db.Prices[sym][d] = v
			}
			if inst := db.Instruments[sym]; inst != nil {
				inst.VanguardPort = r.port
				if (inst.Name == "" || inst.Name == inst.Symbol) && r.name != "" {
					inst.Name = r.name
				}
			}
			db.PriceAt["vg:"+sym] = stamp
		}
		for ccy, m := range fxData {
			if db.FX[ccy] == nil {
				db.FX[ccy] = map[string]float64{}
			}
			for d, v := range m {
				db.FX[ccy][d] = v
			}
		}
		// "last refreshed" only moves when something really updated
		if updated > 0 || len(bench) > 0 || len(fxData) > 0 {
			db.LastRefresh = stamp
		}
		db.RefreshAt = stamp
		db.RefreshErr = shorten(strings.Join(errs, "; "), 600)
		return nil
	})
	if err != nil {
		return "", err
	}
	if force {
		_, _ = a.LiveTick(true) // make today's price current too
	}
	msg := fmt.Sprintf("Updated %d listed holdings and %d Vanguard funds", len(hist), len(vg))
	if len(errs) > 0 {
		return msg, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return msg, nil
}
