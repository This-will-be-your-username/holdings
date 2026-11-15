package main

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func (a *App) registerOwners(mux *http.ServeMux) {
	mux.HandleFunc("POST /owners", a.addOwner)
	mux.HandleFunc("POST /owners/delete", a.deleteOwner)
	mux.HandleFunc("POST /owners/assign", a.assignOwners)
}

func taxBack(owner, fy string) string {
	q := url.Values{}
	if owner != "" {
		q.Set("owner", owner)
	}
	if validFY(fy) {
		q.Set("fy", fy)
	}
	if len(q) == 0 {
		return "/tax"
	}
	return "/tax?" + q.Encode()
}

func (a *App) addOwner(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 40 {
		redirect(w, r, "/tax", "", "enter a name for the owner (up to 40 characters)")
		return
	}
	if strings.EqualFold(name, "unassigned") || name == "*" {
		redirect(w, r, "/tax", "", "choose a different name: that one is reserved")
		return
	}
	err := a.st().Update(func(db *DB) error {
		if db.ownerIndex(name) >= 0 {
			return fmt.Errorf("there is already an owner called %s", name)
		}
		o := Owner{Name: name, CGTMethod: db.Settings.CGTMethod}
		db.Owners = append(db.Owners, o)
		return nil
	})
	if err != nil {
		redirect(w, r, "/tax", "", err.Error())
		return
	}
	redirect(w, r, taxBack(name, ""), "Added "+name+". Set their income below and assign their investments.", "")
}

func (a *App) deleteOwner(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	err := a.st().Update(func(db *DB) error {
		i := db.ownerIndex(name)
		if i < 0 {
			return fmt.Errorf("no such owner")
		}
		real := db.Owners[i].Name
		db.Owners = append(db.Owners[:i], db.Owners[i+1:]...)
		for _, in := range db.Instruments { // their investments go back to the default owner
			if strings.EqualFold(in.Owner, real) {
				in.Owner = ""
			}
		}
		return nil
	})
	if err != nil {
		redirect(w, r, "/tax", "", err.Error())
		return
	}
	redirect(w, r, "/tax", "Removed "+name+"; their investments are unassigned again.", "")
}

// assignOwners saves which owner holds each investment (form fields owner_<symbol>).
func (a *App) assignOwners(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	n := 0
	err := a.st().Update(func(db *DB) error {
		for sym, in := range db.Instruments {
			v, ok := r.Form["owner_"+sym]
			if !ok || len(v) == 0 {
				continue
			}
			want := ""
			if v[0] != "" {
				i := db.ownerIndex(v[0])
				if i < 0 {
					return fmt.Errorf("no such owner %q", v[0])
				}
				want = db.Owners[i].Name
			}
			if in.Owner != want {
				in.Owner = want
				n++
			}
		}
		return nil
	})
	if err != nil {
		redirect(w, r, "/tax", "", err.Error())
		return
	}
	redirect(w, r, taxBack(r.FormValue("owner"), r.FormValue("fy")), fmt.Sprintf("Ownership saved (%d changed)", n), "")
}

// ---------- tax page data ----------

type ownerTab struct {
	Name, Label string
	On          bool
	Count       int // investments tagged to them
}

type ownerSummary struct {
	Label                          string
	OtherIncome                    float64
	Assessable, NetCapGain, NetTax float64
	Holdings                       int
}

type assignRow struct {
	Symbol, Name, Owner string
	Held                bool
}

// ownerTaxData builds what the tax page needs about owners for the chosen income year.
func ownerTaxData(db *DB, owner, fy string, now time.Time) (tabs []ownerTab, sum []ownerSummary, assign []assignRow) {
	if !db.hasOwners() {
		return
	}
	counts := map[string]int{}
	for sym := range db.Instruments {
		counts[db.ownerOf(sym)]++
	}
	for _, o := range db.ownerList() {
		tabs = append(tabs, ownerTab{Name: o, Label: ownerLabel(o, true), On: o == owner, Count: counts[o]})
		view := db.ownerView(o)
		res := Compute(view, now)
		tr := ComputeTax(view, res.Rows, now)
		row := ownerSummary{Label: ownerLabel(o, true), OtherIncome: view.Settings.OtherIncome, Holdings: len(res.Holdings)}
		if y := tr.ByFY[fy]; y != nil {
			row.Assessable, row.NetCapGain, row.NetTax = y.Assessable, y.NetCapGain, y.NetPayable
		}
		sum = append(sum, row)
	}
	held := map[string]bool{}
	for _, t := range db.Txns {
		held[t.Symbol] = true
	}
	for _, sym := range sortedSymbols(db.Instruments) {
		in := db.Instruments[sym]
		assign = append(assign, assignRow{Symbol: sym, Name: in.Name, Owner: db.ownerOf(sym), Held: held[sym]})
	}
	sort.SliceStable(assign, func(i, j int) bool { return assign[i].Held && !assign[j].Held })
	return
}
