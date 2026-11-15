package main

import (
	"fmt"
	"math"
	"time"
)

// For listed shares and ETFs a transaction's price is looked up rather than typed: the market close on the
// transaction date (or the last trading day before it, for weekends and holidays). Leave the price blank to use it.

const lookbackDays = 7

// closeOnOrBefore returns the close on date, or on the latest earlier day within a week.
func closeOnOrBefore(closes map[string]float64, date string) (float64, string, bool) {
	v, d, ok := newSeries(closes).At(date)
	if !ok || v <= 0 {
		return 0, "", false
	}
	a, e1 := time.Parse(dateFmt, d)
	b, e2 := time.Parse(dateFmt, date)
	if e1 != nil || e2 != nil || b.Sub(a) > lookbackDays*24*time.Hour {
		return 0, "", false
	}
	return v, d, true
}

// lookupClose finds the price of sym on date: from the saved history if it covers the day, otherwise from Yahoo.
func lookupClose(sym, date string, saved map[string]float64, now time.Time) (price float64, used string, err error) {
	d, perr := time.Parse(dateFmt, date)
	if perr != nil {
		return 0, "", fmt.Errorf("bad date")
	}
	if d.After(now.AddDate(0, 0, 1)) {
		return 0, "", fmt.Errorf("%s is in the future, so there is no market price yet", date)
	}
	if p, u, ok := closeOnOrBefore(saved, date); ok {
		return round6(p), u, nil
	}
	q, qerr := fetchYahoo(sym, d.AddDate(0, 0, -(lookbackDays+3)))
	if qerr != nil {
		return 0, "", qerr
	}
	if p, u, ok := closeOnOrBefore(q.Closes, date); ok {
		return round6(p), u, nil
	}
	return 0, "", fmt.Errorf("no market price for %s around %s", sym, date)
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
