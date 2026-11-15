package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Editing a recorded transaction - mainly to put in the real cost basis for shares that came in with a looked-up or
// estimated price. The original fingerprint is kept so that importing the same broker row again is still recognised as
// a duplicate and doesn't add the old figures back.

func (a *App) registerTxnEdit(mux *http.ServeMux) {
	mux.HandleFunc("GET /transactions/edit", a.txnEditPage)
	mux.HandleFunc("POST /transactions/edit", a.txnEditSave)
}

func (a *App) findTxn(id string) (t Txn, inst Instrument, ok bool) {
	a.st().View(func(db *DB) {
		for _, x := range db.Txns {
			if x.ID == id {
				t, ok = x, true
				if i := db.Instruments[x.Symbol]; i != nil {
					inst = *i
				}
				return
			}
		}
	})
	return
}

func (a *App) txnEditPage(w http.ResponseWriter, r *http.Request) {
	t, inst, ok := a.findTxn(r.FormValue("id"))
	if !ok {
		redirect(w, r, "/transactions", "", "that transaction no longer exists")
		return
	}
	a.render(w, r, "txnedit", map[string]any{"T": t, "Inst": inst,
		"Acquires": t.Type == "buy" || t.Type == "award", "Prices": t.Type == "buy" || t.Type == "sell" || t.Type == "award" || t.Type == "drp",
		"Total": round6(t.Units * t.Price)})
}

func (a *App) txnEditSave(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	id := r.FormValue("id")
	old, inst, ok := a.findTxn(id)
	if !ok {
		redirect(w, r, "/transactions", "", "that transaction no longer exists")
		return
	}
	back := "/transactions/edit?id=" + id
	t := old
	if s := strings.TrimSpace(r.FormValue("date")); s != "" {
		d, err := parseDate(s)
		if err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		t.Date = d
	}
	if r.Form.Has("notes") {
		t.Notes = strings.TrimSpace(r.FormValue("notes"))
	}
	if t.Type != "dividend" {
		if v := strings.TrimSpace(r.FormValue("units")); v != "" {
			t.Units = pnum(v)
		}
	}
	if v := strings.TrimSpace(r.FormValue("price")); v != "" && t.Type != "dividend" {
		p := pnum(v)
		if p != t.Price { // the old provenance no longer describes the figure
			t.Notes = dropPriceProvenance(t.Notes)
		}
		if r.FormValue("total_cost") != "" {
			if t.Units <= 0 {
				redirect(w, r, back, "", "enter the units to work out a price from a total cost basis")
				return
			}
			p = round6(p / t.Units)
			t.Notes = appendNote(t.Notes, fmt.Sprintf("cost basis edited: total %s %s", inst.Currency, commas(pnum(v), 2)))
		} else {
			t.Notes = appendNote(t.Notes, fmt.Sprintf("cost basis edited: %s %s per unit", inst.Currency, commas(p, 4)))
		}
		t.Price = p
	}
	if v := strings.TrimSpace(r.FormValue("fees")); v != "" || r.Form.Has("fees") {
		t.Fees = pnum(v)
	}
	if t.Type == "dividend" || t.Type == "drp" {
		if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
			t.Amount = pnum(v)
		}
	}
	if r.Form.Has("fx") {
		t.FX = pnum(r.FormValue("fx"))
	}
	if t.Type == "buy" || t.Type == "award" {
		t.LongTerm = r.FormValue("long_term") != ""
	}

	if err := validateTxn(&t); err != nil {
		redirect(w, r, back, "", err.Error())
		return
	}
	err := a.st().Update(func(db *DB) error {
		for i := range db.Txns {
			if db.Txns[i].ID == id {
				if t.FP == "" {
					t.FP = Fingerprint(old)
				}
				db.Txns[i] = t // FP is deliberately unchanged
				return nil
			}
		}
		return fmt.Errorf("that transaction no longer exists")
	})
	if err != nil {
		redirect(w, r, "/transactions", "", err.Error())
		return
	}
	redirect(w, r, "/transactions", fmt.Sprintf("Updated %s %s on %s: %s %s per unit", t.Type, short(t.Symbol), t.Date, inst.Currency, commas(t.Price, 4)), "")
}

// dropPriceProvenance removes notes that say where the previous price came from.
func dropPriceProvenance(n string) string {
	var keep []string
	for _, part := range strings.Split(n, "; ") {
		if strings.HasPrefix(part, "price looked up") || strings.HasPrefix(part, "price worked out") || strings.HasPrefix(part, "cost basis edited") {
			continue
		}
		keep = append(keep, part)
	}
	return strings.Join(keep, "; ")
}

func appendNote(n, add string) string {
	n = strings.TrimSpace(n)
	if n == "" {
		return add
	}
	if strings.Contains(n, add) {
		return n
	}
	return n + "; " + add
}
