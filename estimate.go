package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// The sale estimator answers "what would selling these units cost me in tax?"
// by adding hypothetical sells (IDs "est-N") to your real records and
// recomputing the income-year tax, so loss offsets, discounts, your other
// income and existing distributions are all accounted for. The effect of the
// sale is the difference to the same year without it.

type SaleReq struct {
	Symbol                 string
	Units, Price, Fees, FX float64
	All                    bool
	Target                 float64 // alternative to Units: AUD proceeds to raise
	Picks                  []Pick  // exact parcels to sell (set by the withdrawal planner); empty = use the parcel method
}

type SaleLine struct {
	Symbol, Name, Currency                                    string
	Units, Price, FX, Fees, Held, Remaining, GrossAUD, NetAUD float64
}

type MethodResult struct {
	Method, Label                                   string
	Gain, DiscountedGain, NetCapGainDelta, TaxDelta float64
	AfterTax                                        float64
	Best                                            bool
	Events                                          []CGTEvent
}

type WaitResult struct {
	Date             string
	Days, Parcels    int
	TaxDelta, Saving float64
}

type Estimate struct {
	Date, FY                         string
	Lines                            []SaleLine
	GrossProceeds, Fees, NetProceeds float64
	Methods                          []*MethodResult
	Sel                              *MethodResult
	Base, Scen                       *FYTax
	MethodsDiffer                    bool
	Wait                             *WaitResult
	Future                           bool
	NL                               *NewLawView // the same sale under the CGT rules from 1 July 2027 (set by the page handler)
}

var methodOrder = []string{"fifo", "hifo", "optimal"}
var methodLabels = map[string]string{
	"fifo":    "First in, first out",
	"hifo":    "Highest cost first",
	"optimal": "Lowest taxable gain",
	"plan":    "Selected parcels",
}

func EstimateSale(db *DB, reqs []SaleReq, date, selected string, now time.Time) (*Estimate, error) {
	if len(reqs) == 0 {
		return nil, fmt.Errorf("add at least one holding to sell")
	}
	if _, err := time.Parse(dateFmt, date); err != nil {
		return nil, fmt.Errorf("bad sale date")
	}
	if _, ok := methodLabels[selected]; !ok {
		selected = db.Settings.CGTMethod
	}
	base := Compute(db, now)
	held := map[string]*Holding{}
	for _, h := range base.Holdings {
		held[h.Symbol] = h
	}
	last := map[string]string{}
	for _, t := range db.Txns {
		if t.Date > last[t.Symbol] {
			last[t.Symbol] = t.Date
		}
	}

	used := map[string]float64{}
	picks := map[string][]Pick{}
	hasPicks := false
	var txns []Txn
	var lines []SaleLine
	for i, rq := range reqs {
		h := held[rq.Symbol]
		if h == nil {
			return nil, fmt.Errorf("you don't currently hold %s", rq.Symbol)
		}
		if date < last[rq.Symbol] {
			return nil, fmt.Errorf("sale date %s is before your last %s transaction (%s)", date, short(rq.Symbol), last[rq.Symbol])
		}
		price := rq.Price
		if price <= 0 {
			price = h.Price
		}
		avail := h.Units - used[rq.Symbol]
		units := rq.Units
		switch {
		case rq.All:
			units = avail
		case units <= 0 && rq.Target > 0:
			fx := h.FXNow
			if rq.FX > 0 {
				fx = rq.FX
			}
			if price*fx <= 0 {
				return nil, fmt.Errorf("no price available for %s - enter one", short(rq.Symbol))
			}
			units = math.Round(rq.Target/(price*fx)*10000) / 10000
		}
		if units <= 0 {
			return nil, fmt.Errorf("enter units (or \"all\", or a target amount) for %s", short(rq.Symbol))
		}
		if units > avail+1e-9 {
			return nil, fmt.Errorf("can't sell %s %s: only %s available", trimFloat(units), short(rq.Symbol), trimFloat(avail))
		}
		if rq.Fees < 0 || rq.Price < 0 || rq.FX < 0 {
			return nil, fmt.Errorf("price, fees and FX can't be negative")
		}
		used[rq.Symbol] += units
		if len(rq.Picks) > 0 {
			hasPicks = true
			picks[fmt.Sprintf("est-%d", i)] = rq.Picks
		}
		txns = append(txns, Txn{ID: fmt.Sprintf("est-%d", i), Symbol: rq.Symbol, Type: "sell",
			Units: units, Price: price, Fees: rq.Fees, FX: rq.FX})
		lines = append(lines, SaleLine{Symbol: rq.Symbol, Name: h.Name, Currency: h.Currency, Units: units,
			Price: price, Fees: rq.Fees, Held: h.Units})
	}
	for i := range lines {
		lines[i].Remaining = held[lines[i].Symbol].Units - used[lines[i].Symbol]
	}

	run := func(method, dt string) (*Result, *TaxResult) {
		s := *db // shallow copy: Compute/ComputeTax never mutate the maps
		s.Txns = append([]Txn(nil), db.Txns...)
		for _, t := range txns {
			t.Date = dt
			s.Txns = append(s.Txns, t)
		}
		s.Settings.EstMethod = method
		s.Settings.EstPicks = picks
		res := Compute(&s, now)
		return res, ComputeTax(&s, res.Rows, now)
	}
	isEst := func(id string) bool { return strings.HasPrefix(id, "est-") }

	fy := fyOf(date)
	baseTR := ComputeTax(db, base.Rows, now)
	baseFY := baseTR.ByFY[fy]
	if baseFY == nil {
		baseFY = ComputeTaxEmptyAfter(baseTR, db, fy)
	}

	est := &Estimate{Date: date, FY: fy, Base: baseFY, Lines: lines, Future: date > now.Format(dateFmt)}
	order := methodOrder
	if hasPicks { // parcels were chosen explicitly: there is only one way to read the sale
		order, selected = []string{"plan"}, "plan"
	}
	for _, m := range order {
		res, tr := run(m, date)
		y := tr.ByFY[fy]
		if y == nil {
			y = ComputeTaxEmptyAfter(tr, db, fy)
		}
		mr := &MethodResult{Method: m, Label: methodLabels[m]}
		for _, e := range tr.All {
			if isEst(e.TxnID) {
				mr.Events = append(mr.Events, e)
				mr.Gain += e.Gain
				mr.DiscountedGain += e.DiscountedGain()
			}
		}
		mr.NetCapGainDelta = y.NetCapGain - baseFY.NetCapGain
		mr.TaxDelta = y.NetPayable - baseFY.NetPayable
		if len(est.Methods) == 0 { // proceeds don't depend on the parcel method
			k := 0
			for _, row := range res.Rows {
				if isEst(row.ID) {
					l := &est.Lines[k]
					k++
					l.FX = row.FXUsed
					l.GrossAUD = row.Units * row.Price * row.FXUsed
					l.NetAUD = row.GrossAUD
					est.GrossProceeds += l.GrossAUD
					est.Fees += row.Fees
					est.NetProceeds += row.GrossAUD
				}
			}
		}
		mr.AfterTax = est.NetProceeds - mr.TaxDelta
		est.Methods = append(est.Methods, mr)
		if m == selected {
			est.Sel, est.Scen = mr, y
		}
	}
	best := math.Inf(1)
	worst := math.Inf(-1)
	for _, m := range est.Methods {
		best = math.Min(best, m.TaxDelta)
		worst = math.Max(worst, m.TaxDelta)
	}
	est.MethodsDiffer = worst-best > 0.005
	for _, m := range est.Methods {
		m.Best = est.MethodsDiffer && m.TaxDelta-best < 0.005
	}

	// Would waiting for the 12-month discount help? Re-run at the earliest date a
	// full-rate gain becomes discount-eligible, assuming the same prices.
	var waitDate time.Time
	n := 0
	for _, e := range est.Sel.Events {
		if e.Kind == "sale" && e.Gain > 0 && !e.Discount {
			acq, _ := time.Parse(dateFmt, e.Acquired)
			el := acq.AddDate(1, 0, 1)
			if waitDate.IsZero() || el.Before(waitDate) {
				waitDate = el
			}
			n++
		}
	}
	if !waitDate.IsZero() {
		wd := waitDate.Format(dateFmt)
		_, tr := run(selected, wd)
		wfy := fyOf(wd)
		y := tr.ByFY[wfy]
		if y == nil {
			y = ComputeTaxEmptyAfter(tr, db, wfy)
		}
		b := baseTR.ByFY[wfy]
		if b == nil {
			b = ComputeTaxEmptyAfter(baseTR, db, wfy)
		}
		delta := y.NetPayable - b.NetPayable
		est.Wait = &WaitResult{Date: wd, Days: int(waitDate.Sub(parseDay(date)).Hours() / 24), Parcels: n,
			TaxDelta: delta, Saving: est.Sel.TaxDelta - delta}
	}
	return est, nil
}

// ComputeTaxEmpty returns a zero-activity year carrying the user's other income,
// for years the real records don't touch yet.
func ComputeTaxEmpty(db *DB, fy string) *FYTax {
	return &FYTax{FY: fy, OtherIncome: db.Settings.OtherIncome}
}

// ComputeTaxEmptyAfter is an untouched income year that still inherits the capital losses carried
// forward from the latest earlier year in tr.
func ComputeTaxEmptyAfter(tr *TaxResult, db *DB, fy string) *FYTax {
	y := ComputeTaxEmpty(db, fy)
	for _, p := range tr.FYs {
		if p.FY < fy {
			y.LossBroughtFwd = p.LossCarriedFwd
		}
	}
	return y
}

func parseDay(s string) time.Time { t, _ := time.Parse(dateFmt, s); return t }
func short(s string) string       { return strings.TrimSuffix(s, ".AX") }
func trimFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")
}

// ---------- page ----------

type formRow struct{ Symbol, Units, Price, Fees, Target string }

func (a *App) estimatorPage(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if r.FormValue("mode") == "raise" {
		a.raisePage(w, r)
		return
	}
	if r.FormValue("mode") == "all" {
		a.sellAllPage(w, r)
		return
	}
	var res *Result
	var set Settings
	a.st().View(func(db *DB) { res = Compute(db, time.Now()); set = db.Settings })
	bracket := bracketChoice(r, set.OtherIncome)

	date := strings.TrimSpace(r.FormValue("date"))
	if date == "" {
		date = time.Now().Format(dateFmt)
	} else if d, err := parseDate(date); err == nil {
		date = d
	}
	method := r.FormValue("method")
	if _, ok := methodLabels[method]; !ok {
		method = set.CGTMethod
	}

	var rows []formRow
	var reqs []SaleReq
	get := func(k string, i int) string {
		if v := r.Form[k]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	for i := range r.Form["sym"] {
		sym := get("sym", i)
		if sym == "" {
			continue
		}
		fr := formRow{sym, get("units", i), get("price", i), get("fees", i), get("target", i)}
		rows = append(rows, fr)
		rq := SaleReq{Symbol: sym, Price: pnum(fr.Price), Fees: pnum(fr.Fees), Target: pnum(fr.Target)}
		if strings.EqualFold(fr.Units, "all") {
			rq.All = true
		} else {
			rq.Units = pnum(fr.Units)
		}
		reqs = append(reqs, rq)
	}
	if len(rows) == 0 {
		rows = []formRow{{}}
	}

	data := map[string]any{"Holdings": res.Holdings, "Rows": rows, "Date": date, "Method": method,
		"Today": time.Now().Format(dateFmt), "Methods": methodOrder, "Labels": methodLabels, "Mode": "sell",
		"Bracket": bracket, "MyIncome": set.OtherIncome, "TopRate": topMarginalRate(fyOf(date))}
	nl, nlForm := newLawFromForm(r)
	for k, v := range nlForm {
		data[k] = v
	}
	if r.FormValue("run") != "" {
		var est *Estimate
		var err error
		a.st().View(func(db0 *DB) {
			now := time.Now()
			db := withBracket(db0, bracket)
			est, err = EstimateSale(db, reqs, date, method, now)
			if err == nil {
				var nerr error
				est.NL, nerr = CompareNewLaw(db, est, nil, est.Sel.Method, nl, now)
				if nerr != nil {
					data["NLErr"] = nerr.Error()
				}
			}
		})
		if err != nil {
			data["Err"] = err.Error()
		} else {
			data["Est"] = est
		}
	}
	a.render(w, r, "estimator", data)
}
