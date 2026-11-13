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

func (a *App) computeTax() (*Result, *TaxResult, Settings) {
	var res *Result
	var tr *TaxResult
	var set Settings
	a.store.View(func(db *DB) {
		now := time.Now()
		res = Compute(db, now)
		tr = ComputeTax(db, res.Rows, now)
		set = db.Settings
	})
	return res, tr, set
}

func (a *App) taxExport(name string, write func(io.Writer, *TaxResult, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, tr, _ := a.computeTax()
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
	_, tr, set := a.computeTax()
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
	// Show the sale events largest-gain-first is noisy; keep chronological.
	a.render(w, r, "tax", map[string]any{
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
	a.store.Update(func(db *DB) error {
		db.Settings = Settings{
			OtherIncome: other, CGTMethod: m, OpeningLoss: loss,
			ExcludeFranking: r.FormValue("include_franking") == "",
		}
		return nil
	})
	back := "/tax"
	if fy := r.FormValue("fy"); validFY(fy) {
		back += "?fy=" + fy
	}
	redirect(w, r, back, "Tax settings saved", "")
}

type yieldRow struct {
	Symbol, Name                string
	Cash, Franking, Cost, Value float64
	YieldCost, YieldValue       float64 // trailing 12 months, incl. franking
}

func (a *App) distributionsPage(w http.ResponseWriter, r *http.Request) {
	res, tr, _ := a.computeTax()
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
	a.store.View(func(db *DB) { ex = db.Settings.ExcludeFranking })
	return
}
