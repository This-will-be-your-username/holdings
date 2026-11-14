package main

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Reconciling FundTrack with a provider's holdings (valuation) export.
//
// A transaction export only covers a window of time. Anything bought before it is simply absent, so FundTrack's
// units - and therefore value, cost base and returns - come out short. The holdings export lists what the account
// really holds today, so comparing the two shows exactly which holdings are short and by how many units. It never edits
// data: the cure is importing the missing history.
//
// Parsing is header-driven like the transaction importer: it needs an investment name plus units and/or value; price,
// price date and code are optional. Account name/number columns are ignored and never stored.

type VGHolding struct {
	Name, Code, PriceDate string
	Price, Qty, Value     float64
}

type VGValuation struct {
	Holdings []VGHolding
	Cash     float64 // cash held with the provider (not an investment FundTrack tracks)
	Total    float64 // the provider's stated total, if the file has one (0 if not)
	AsAt     string  // latest price date in the file
}

var (
	reValName  = regexp.MustCompile(`^(investment|investment name|holding|holdings|security|security name|fund|fund name|product|product name|description|name|instrument)$`)
	reValQty   = regexp.MustCompile(`^(quantity|units|qty|units held|unit holding|no of units|number of units|shares|holding units)$`)
	reValPrice = regexp.MustCompile(`^(price|unit price|nav|last price|closing price|exit price|current price)$`)
	reValValue = regexp.MustCompile(`^(value|market value|valuation|total value|current value|balance|amount)$`)
	reValDate  = regexp.MustCompile(`^(price date|pricing date|as at|as at date|valuation date|date|price as at)$`)
	reValCode  = regexp.MustCompile(`^(code|symbol|ticker|asx code|apir|apir code|product code|product id|security code)$`)
)

// ParseValuation reads a holdings export (CSV, or .xlsx).
func ParseValuation(data []byte) (*VGValuation, error) {
	var rows [][]string
	var err error
	if isXLSX(data) {
		rows, err = readXLSX(data)
	} else {
		rows, _, err = readTable(data)
	}
	if err != nil {
		return nil, fmt.Errorf("could not read the file: %v", err)
	}
	nameC, qtyC, priceC, valC, dateC, codeC, hdr := -1, -1, -1, -1, -1, -1, -1
	for i, r := range rows {
		if i > 30 {
			break
		}
		n, q, p, v, d, c := -1, -1, -1, -1, -1, -1
		for ci, cell := range r {
			h := normHeader(cell)
			switch {
			case reValName.MatchString(h) && n < 0:
				n = ci
			case reValQty.MatchString(h) && q < 0:
				q = ci
			case reValPrice.MatchString(h) && p < 0:
				p = ci
			case reValValue.MatchString(h) && v < 0:
				v = ci
			case reValDate.MatchString(h) && d < 0:
				d = ci
			case reValCode.MatchString(h) && c < 0:
				c = ci
			}
		}
		if n >= 0 && (q >= 0 || v >= 0) {
			nameC, qtyC, priceC, valC, dateC, codeC, hdr = n, q, p, v, d, c, i
			break
		}
	}
	if hdr < 0 {
		return nil, fmt.Errorf("couldn't find the columns: expected headings like Investment, Quantity (or Units), Price and Value")
	}
	get := func(r []string, c int) string {
		if c >= 0 && c < len(r) {
			return strings.TrimSpace(r[c])
		}
		return ""
	}
	num := func(r []string, c int) float64 { v, _ := parseNumber(get(r, c), false); return v }

	out := &VGValuation{}
	var dates []string
	for _, r := range rows[hdr+1:] {
		name := get(r, nameC)
		if name == "" { // a totals line: label somewhere in the row, amount in the value column
			for _, cell := range r {
				if regexp.MustCompile(`(?i)total`).MatchString(cell) {
					out.Total = num(r, valC)
					break
				}
			}
			continue
		}
		h := VGHolding{Name: name, Qty: num(r, qtyC), Price: num(r, priceC), Value: num(r, valC)}
		if c := get(r, codeC); c != "" && !regexp.MustCompile(`^\d+$`).MatchString(c) { // a numeric product id isn't a code
			h.Code = strings.ToUpper(c)
		}
		if d, ok := parseDateFlex(get(r, dateC), ""); ok {
			h.PriceDate = d
		}
		if regexp.MustCompile(`(?i)cash`).MatchString(name) && h.Qty == 0 && h.Price == 0 {
			out.Cash += h.Value
			continue
		}
		if h.PriceDate != "" {
			dates = append(dates, h.PriceDate)
		}
		if h.Qty == 0 && h.Value == 0 {
			continue
		}
		if h.Price == 0 && h.Qty > 0 {
			h.Price = h.Value / h.Qty
		}
		if h.Qty == 0 && h.Price > 0 {
			h.Qty = h.Value / h.Price
		}
		out.Holdings = append(out.Holdings, h)
	}
	if len(out.Holdings) == 0 {
		return nil, fmt.Errorf("no holdings found under the header row")
	}
	sort.Strings(dates)
	if len(dates) > 0 {
		out.AsAt = dates[len(dates)-1]
	}
	return out, nil
}

// ---------- matching ----------

var reNonAlnumLower = regexp.MustCompile(`[^a-z0-9]+`)

func normName(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "&", " and "))
	return strings.TrimSpace(reNonAlnumLower.ReplaceAllString(s, " "))
}

func nameTokens(s string) map[string]bool {
	m := map[string]bool{}
	for _, t := range strings.Fields(normName(s)) {
		m[t] = true
	}
	return m
}

func matchInstrument(h VGHolding, insts map[string]*Instrument) *Instrument {
	n := normName(h.Name)
	if h.Code != "" {
		for sym, in := range insts {
			if strings.EqualFold(sym, h.Code) || strings.EqualFold(strings.TrimSuffix(sym, ".AX"), h.Code) {
				return in
			}
		}
	}
	for _, in := range insts { // exact name
		if normName(in.Name) == n {
			return in
		}
	}
	for _, in := range insts { // one contains the other
		in2 := normName(in.Name)
		if len(n) >= 12 && len(in2) >= 12 && (strings.Contains(in2, n) || strings.Contains(n, in2)) {
			return in
		}
	}
	best, bestScore := (*Instrument)(nil), 0.0
	a := nameTokens(h.Name)
	for _, in := range insts { // token overlap
		b := nameTokens(in.Name)
		inter := 0
		for t := range a {
			if b[t] {
				inter++
			}
		}
		if u := len(a) + len(b) - inter; u > 0 {
			if s := float64(inter) / float64(u); s > bestScore {
				best, bestScore = in, s
			}
		}
	}
	if bestScore >= 0.85 {
		return best
	}
	return nil
}

// ---------- reconciliation ----------

type ReconRow struct {
	Name, PriceDate  string
	VGPrice, VGUnits float64
	VGValue          float64
	Symbol           string // matched FundTrack holding ("" if none)
	FTUnits          float64
	FTPrice          float64 // the price FundTrack is using
	FTPriceDate      string
	DiffUnits        float64 // Vanguard minus FundTrack
	DiffValue        float64 // at Vanguard's price
	Status           string  // ok | missing | extra | unmatched
	PriceMismatch    bool    // FundTrack's price differs from Vanguard's by more than 1%
}

type Reconciliation struct {
	Rows                  []ReconRow
	NotInVanguard         []ReconRow // FundTrack holds it, the export doesn't list it
	VGTotal, FTTotal      float64    // FundTrack total at FundTrack's own prices
	FTAtVGPrices          float64
	Cash                  float64
	MissingValue          float64 // value of units missing from FundTrack, at Vanguard's prices
	AsAt                  string
	EarliestTxn           string
	NMissing, NExtra, NOK int
	NUnmatched            int
	ExplainedByCash       bool
}

const unitTol = 0.005

// Reconcile compares the provider's holdings with what FundTrack computes.
func Reconcile(db *DB, v *VGValuation, now time.Time) *Reconciliation {
	res := Compute(db, now)
	held := map[string]*Holding{}
	for _, h := range res.Holdings {
		held[h.Symbol] = h
	}
	rc := &Reconciliation{Cash: v.Cash, AsAt: v.AsAt}
	for _, t := range db.Txns {
		if rc.EarliestTxn == "" || t.Date < rc.EarliestTxn {
			rc.EarliestTxn = t.Date
		}
	}
	used := map[string]bool{}
	for _, h := range v.Holdings {
		r := ReconRow{Name: h.Name, PriceDate: h.PriceDate, VGPrice: h.Price, VGUnits: h.Qty, VGValue: h.Value}
		in := matchInstrument(h, db.Instruments)
		if in == nil {
			r.Status = "unmatched"
			rc.NUnmatched++
			rc.VGTotal += h.Value
			rc.Rows = append(rc.Rows, r)
			continue
		}
		r.Symbol = in.Symbol
		used[in.Symbol] = true
		if ft := held[in.Symbol]; ft != nil {
			r.FTUnits, r.FTPrice, r.FTPriceDate = ft.Units, ft.Price, ft.PriceDate
			rc.FTTotal += ft.ValueAUD
		}
		r.DiffUnits = h.Qty - r.FTUnits
		r.DiffValue = r.DiffUnits * h.Price
		rc.FTAtVGPrices += r.FTUnits * h.Price
		rc.VGTotal += h.Value
		r.PriceMismatch = r.FTPrice > 0 && h.Price > 0 && math.Abs(r.FTPrice/h.Price-1) > 0.01
		switch {
		case math.Abs(r.DiffUnits) <= unitTol:
			r.Status = "ok"
			rc.NOK++
		case r.DiffUnits > 0:
			r.Status = "missing"
			rc.NMissing++
			rc.MissingValue += r.DiffValue
		default:
			r.Status = "extra"
			rc.NExtra++
		}
		rc.Rows = append(rc.Rows, r)
	}
	for _, h := range res.Holdings {
		if !used[h.Symbol] {
			rc.NotInVanguard = append(rc.NotInVanguard, ReconRow{Name: h.Name, Symbol: h.Symbol, FTUnits: h.Units, FTPrice: h.Price, FTPriceDate: h.PriceDate, VGValue: 0, DiffValue: -h.ValueAUD})
			rc.FTTotal += h.ValueAUD
		}
	}
	if v.Total > 0 {
		rc.VGTotal = v.Total
	} else {
		rc.VGTotal += v.Cash
	}
	rc.ExplainedByCash = rc.Cash > 0
	return rc
}

// ---------- pages ----------

func (a *App) registerReconcile(mux *http.ServeMux) {
	mux.HandleFunc("GET /reconcile", a.reconcilePage)
	mux.HandleFunc("POST /reconcile", a.reconcileUpload)
}

func (a *App) reconcileUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil && err != http.ErrNotMultipart {
		redirect(w, r, "/reconcile", "", "upload too large or unreadable (limit 8 MB)")
		return
	}
	var data []byte
	name := "pasted.csv"
	if f, h, err := r.FormFile("file"); err == nil {
		defer f.Close()
		buf := make([]byte, 0, 1<<16)
		tmp := make([]byte, 32<<10)
		for {
			n, e := f.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if e != nil {
				break
			}
		}
		data, name = buf, h.Filename
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte(r.FormValue("paste"))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		redirect(w, r, "/reconcile", "", "choose your provider's holdings export (CSV or .xlsx)")
		return
	}
	if _, err := ParseValuation(data); err != nil {
		redirect(w, r, "/reconcile", "", err.Error())
		return
	}
	s := newSession(name, data)
	http.Redirect(w, r, "/reconcile?t="+s.ID, http.StatusSeeOther)
}

func (a *App) reconcilePage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{}
	if t := r.URL.Query().Get("t"); t != "" {
		sess := getSession(t)
		if sess == nil {
			redirect(w, r, "/reconcile", "", "that upload has expired - please upload the file again")
			return
		}
		v, err := ParseValuation(sess.Data)
		if err != nil {
			redirect(w, r, "/reconcile", "", err.Error())
			return
		}
		var rc *Reconciliation
		a.st().View(func(db *DB) { rc = Reconcile(db, v, liveNow()) })
		data["T"], data["File"], data["RC"], data["V"] = t, sess.File, rc, v
		diff := rc.VGTotal - rc.FTTotal
		data["Diff"] = diff
		data["Residual"] = diff - rc.MissingValue - rc.Cash
	}
	a.render(w, r, "reconcile", data)
}
