package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

func (a *App) registerTax(mux *http.ServeMux) {
	mux.HandleFunc("GET /tax", a.taxPage)
	mux.HandleFunc("GET /estimator", a.estimatorPage)
	mux.HandleFunc("GET /outlook", a.outlookPage)
	mux.HandleFunc("GET /plan", a.planPage)
	mux.HandleFunc("POST /plan", a.savePlan)
	mux.HandleFunc("POST /settings", a.saveSettings)
	mux.HandleFunc("GET /distributions", a.distributionsPage)
	mux.HandleFunc("GET /export/tax-summary.csv", a.taxExport("tax-summary", func(w io.Writer, tr *TaxResult, fy string) error {
		return WriteTaxSummaryCSV(w, pickFYs(tr, fy))
	}))
	mux.HandleFunc("GET /export/tax-distributions.csv", a.taxExport("tax-distributions", func(w io.Writer, tr *TaxResult, fy string) error {
		return WriteDistCSV(w, pickFYs(tr, fy))
	}))
	mux.HandleFunc("GET /export/tax-cgt.csv", a.taxExport("tax-cgt", func(w io.Writer, tr *TaxResult, fy string) error {
		if fy == "" {
			return WriteCGTCSV(w, tr.All)
		}
		if y := tr.ByFY[fy]; y != nil {
			return WriteCGTCSV(w, y.Events)
		}
		return WriteCGTCSV(w, nil)
	}))
}

func pickFYs(tr *TaxResult, fy string) []*FYTax {
	if fy == "" {
		return tr.FYs
	}
	if y := tr.ByFY[fy]; y != nil {
		return []*FYTax{y}
	}
	return nil
}

// computeTax works out the tax position for one owner ("" = the default owner, or everyone when no owners are set up).
// owner "*" ignores owners and treats everything as one pool (used for distribution listings).
func (a *App) computeTax(owner string) (*Result, *TaxResult, Settings) {
	var res *Result
	var tr *TaxResult
	var set Settings
	a.st().View(func(db *DB) {
		now := time.Now()
		v := db
		if owner != "*" && db.hasOwners() {
			if db.ownerIndex(owner) < 0 {
				owner = ""
			}
			v = db.ownerView(owner)
		}
		res = Compute(v, now)
		tr = ComputeTax(v, res.Rows, now)
		set = v.Settings
	})
	return res, tr, set
}

func (a *App) taxExport(name string, write func(io.Writer, *TaxResult, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, tr, _ := a.computeTax(r.URL.Query().Get("owner"))
		fy := r.URL.Query().Get("fy")
		label := name
		if fy != "" {
			label += "-" + fy
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fundtrack-%s.csv"`, label))
		w.Write([]byte("\xef\xbb\xbf"))
		_ = write(w, tr, fy)
	}
}

func (a *App) taxPage(w http.ResponseWriter, r *http.Request) {
	owner := r.URL.Query().Get("owner")
	var hasOwners bool
	a.st().View(func(db *DB) {
		hasOwners = len(db.Owners) > 0
		if db.ownerIndex(owner) < 0 {
			owner = ""
		}
	})
	_, tr, set := a.computeTax(owner)
	list := make([]*FYTax, len(tr.FYs))
	for i, y := range tr.FYs { // newest first
		list[len(list)-1-i] = y
	}
	sel := tr.ByFY[r.URL.Query().Get("fy")]
	if sel == nil { // latest year with any activity, else the current year
		for _, y := range tr.FYs {
			if len(y.Dists) > 0 || len(y.Events) > 0 {
				sel = y
			}
		}
	}
	if sel == nil {
		for _, y := range tr.FYs {
			if y.Current {
				sel = y
			}
		}
	}
	var tabs []ownerTab
	var sum []ownerSummary
	var assign []assignRow
	var curOwner Owner
	a.st().View(func(db *DB) {
		tabs, sum, assign = ownerTaxData(db, owner, sel.FY, time.Now())
		if i := db.ownerIndex(owner); i >= 0 {
			curOwner = db.Owners[i]
		}
	})
	// Show the sale events largest-gain-first is noisy; keep chronological.
	a.render(w, r, "tax", map[string]any{
		"Owner": owner, "OwnerLabel": ownerLabel(owner, hasOwners), "HasOwners": hasOwners, "Tabs": tabs, "Summary": sum, "Assign": assign, "CurOwner": curOwner,
		"FY": sel, "FYs": list, "Set": set,
		"FirstRate": scaleFor(sel.FY)[1].rate * 100,
		"Method":    set.CGTMethod,
	})
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	m := r.FormValue("cgt_method")
	if m != "fifo" && m != "hifo" && m != "optimal" {
		m = "fifo"
	}
	other, loss := pnum(r.FormValue("other_income")), pnum(r.FormValue("opening_loss"))
	if other < 0 || loss < 0 {
		redirect(w, r, "/tax", "", "income and losses can't be negative")
		return
	}
	owner := r.FormValue("owner")
	var saved string
	err := a.st().Update(func(db *DB) error {
		if owner != "" { // a named owner's own assumptions
			i := db.ownerIndex(owner)
			if i < 0 {
				return fmt.Errorf("no such owner")
			}
			db.Owners[i].OtherIncome, db.Owners[i].OpeningLoss, db.Owners[i].CGTMethod = other, loss, m
			saved = db.Owners[i].Name
			return nil
		}
		db.Settings = Settings{
			OtherIncome: other, CGTMethod: m, OpeningLoss: loss,
			ExcludeFranking: r.FormValue("include_franking") == "",
		}
		return nil
	})
	if err != nil {
		redirect(w, r, "/tax", "", err.Error())
		return
	}
	msg := "Tax settings saved"
	if saved != "" {
		msg = "Tax settings saved for " + saved
	}
	redirect(w, r, taxBack(saved, r.FormValue("fy")), msg, "")
}

type yieldRow struct {
	Symbol, Name                string
	Cash, Franking, Cost, Value float64
	YieldCost, YieldValue       float64 // trailing 12 months, incl. franking
}

func (a *App) distributionsPage(w http.ResponseWriter, r *http.Request) {
	res, tr, _ := a.computeTax("*")
	since := time.Now().AddDate(-1, 0, 0).Format(dateFmt)
	var yields []yieldRow
	for _, h := range res.Holdings {
		y := yieldRow{Symbol: h.Symbol, Name: h.Name, Cost: h.CostAUD, Value: h.ValueAUD}
		for _, row := range res.Rows {
			if row.Symbol == h.Symbol && (row.Type == "dividend" || row.Type == "drp") && row.Date >= since {
				y.Cash += row.GrossAUD
				y.Franking += row.Franking
			}
		}
		if y.Cost > 0 {
			y.YieldCost = (y.Cash + y.Franking) / y.Cost * 100
		}
		if y.Value > 0 {
			y.YieldValue = (y.Cash + y.Franking) / y.Value * 100
		}
		yields = append(yields, y)
	}
	sort.Slice(yields, func(i, j int) bool { return yields[i].Cash > yields[j].Cash })

	var lines []DistLine
	var years []*FYTax
	for i := len(tr.FYs) - 1; i >= 0; i-- {
		y := tr.FYs[i]
		if len(y.Dists) > 0 {
			years = append(years, y)
		}
		for j := len(y.Dists) - 1; j >= 0; j-- {
			lines = append(lines, y.Dists[j])
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Date > lines[j].Date })
	a.render(w, r, "distributions", map[string]any{
		"Yields": yields, "Years": years, "Lines": lines, "Suggest": res.Suggested,
		"Incl": !a.excludeFranking(),
	})
}

func (a *App) excludeFranking() (ex bool) {
	a.st().View(func(db *DB) { ex = db.Settings.ExcludeFranking })
	return
}
