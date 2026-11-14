package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Exchange rates are refreshed on their own schedule: the ECB publishes once per business day, so rates are pulled
// again only when the last pull is more than a day old (or when you press the button on the Fund prices page).

const fxMaxAge = 24 * time.Hour

const fxStamp = "fx" // PriceAt key for the last successful pull

type fxNeed struct {
	ccy  string
	from time.Time // earliest date that holding needs a rate for
	have [2]string // first and last saved date, if any
}

// fxNeeds lists every foreign currency held (listed, unlisted or private) with how far back its rates must reach.
func fxNeeds(db *DB) []fxNeed {
	earliest := map[string]time.Time{}
	for _, t := range db.Txns {
		inst := db.Instruments[t.Symbol]
		if inst == nil || inst.Currency == "" || inst.Currency == "AUD" {
			continue
		}
		d, err := time.Parse(dateFmt, t.Date)
		if err != nil {
			continue
		}
		if e, ok := earliest[inst.Currency]; !ok || d.Before(e) {
			earliest[inst.Currency] = d
		}
	}
	var out []fxNeed
	for ccy, d := range earliest {
		n := fxNeed{ccy: ccy, from: d}
		if s := newSeries(db.FX[ccy]); len(s.D) > 0 {
			n.have = [2]string{s.D[0], s.D[len(s.D)-1]}
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ccy < out[j].ccy })
	return out
}

// fxDue reports whether rates should be pulled now.
func fxDue(db *DB, now time.Time) bool {
	needs := fxNeeds(db)
	if len(needs) == 0 {
		return false
	}
	at, err := time.Parse(time.RFC3339, db.PriceAt[fxStamp])
	if err != nil || now.Sub(at) > fxMaxAge {
		return true
	}
	for _, n := range needs {
		if n.have[0] == "" {
			return true // a currency with no rates at all
		}
	}
	return false
}

// RefreshFX pulls exchange rates now (force) or only if the last pull is over 24 hours old.
func (a *App) RefreshFX(force bool) (string, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	return a.refreshFXLocked(force)
}

func (a *App) refreshFXLocked(force bool) (string, error) {
	if a.inTest() {
		return "Test mode: exchange rates are simulated, nothing was fetched", nil
	}
	now := time.Now()
	var needs []fxNeed
	a.st().View(func(db *DB) {
		if force || fxDue(db, now) {
			needs = fxNeeds(db)
		}
	})
	if len(needs) == 0 {
		return "", nil
	}
	data := map[string]map[string]float64{}
	var errs []string
	for _, n := range needs {
		from := n.from.AddDate(0, 0, -10)
		if n.have[0] != "" && n.have[0] <= from.Format(dateFmt) {
			last, _ := time.Parse(dateFmt, n.have[1])
			from = last.AddDate(0, 0, -10) // incremental: only the recent days
		}
		m, err := fetchFX(n.ccy, from, now)
		if err != nil {
			errs = append(errs, err.Error())
		}
		if d, rate, lerr := fetchFXLatest(n.ccy); lerr == nil { // the newest published rate, even if the range call missed it
			if m == nil {
				m = map[string]float64{}
			}
			m[d] = rate
		} else if len(m) == 0 {
			errs = append(errs, lerr.Error())
		}
		if len(m) > 0 {
			data[n.ccy] = m
		}
	}
	_ = a.st().Update(func(db *DB) error {
		for ccy, m := range data {
			if db.FX[ccy] == nil {
				db.FX[ccy] = map[string]float64{}
			}
			for d, v := range m {
				db.FX[ccy][d] = v
			}
		}
		if len(data) == len(needs) { // only a fully successful pull resets the 24-hour clock
			db.PriceAt[fxStamp] = now.Format(time.RFC3339)
		}
		return nil
	})
	msg := fmt.Sprintf("Updated exchange rates for %d currenc%s", len(data), map[bool]string{true: "y", false: "ies"}[len(data) == 1])
	if len(errs) > 0 {
		return msg, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return msg, nil
}

// fxView is what the Fund prices page shows about exchange rates.
type fxRow struct {
	Ccy, Date string
	Rate      float64
}

type fxStatus struct {
	Rows    []fxRow
	At      string // when rates were last pulled
	Due     bool
	NoneYet bool
}

func buildFXStatus(db *DB, now time.Time) fxStatus {
	st := fxStatus{Due: fxDue(db, now)}
	for _, n := range fxNeeds(db) {
		if v, d, ok := newSeries(db.FX[n.ccy]).Last(); ok {
			st.Rows = append(st.Rows, fxRow{n.ccy, d, v})
		}
	}
	if t, err := time.Parse(time.RFC3339, db.PriceAt[fxStamp]); err == nil {
		st.At = t.Local().Format("2 Jan 15:04")
	}
	return st
}

func (a *App) refreshFXHandler(w http.ResponseWriter, r *http.Request) {
	if a.inTest() {
		redirect(w, r, "/prices", "Test mode uses simulated exchange rates; nothing was fetched", "")
		return
	}
	msg, err := a.RefreshFX(true)
	switch {
	case err != nil:
		redirect(w, r, "/prices", "", "Exchange rates: "+err.Error())
	case msg == "":
		redirect(w, r, "/prices", "No foreign-currency holdings, so there are no exchange rates to refresh", "")
	default:
		redirect(w, r, "/prices", msg, "")
	}
}
