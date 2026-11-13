package main

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type App struct {
	store     *Store
	pages     map[string]*template.Template
	refreshMu sync.Mutex
}

func main() {
	addr := env("ADDR", "127.0.0.1:8080")
	dataFile := env("DATA_FILE", "data/fundtrack.json")
	store, err := OpenStore(dataFile)
	if err != nil {
		log.Fatal(err)
	}
	a := &App{store: store, pages: map[string]*template.Template{}}
	for _, p := range []string{"dashboard", "transactions", "prices", "tax", "distributions", "estimator"} {
		a.pages[p] = template.Must(template.New("").Funcs(funcs).ParseFS(webFS, "web/base.html", "web/"+p+".html"))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /transactions", func(w http.ResponseWriter, r *http.Request) {
		form := map[string]string{"date": time.Now().Format(dateFmt), "type": "buy", "market": MarketASX}
		for _, k := range []string{"symbol", "market", "type", "date", "amount", "franked", "franking"} {
			if v := r.URL.Query().Get(k); v != "" {
				form[k] = v
			}
		}
		a.txnPage(w, r, form, "")
	})
	mux.HandleFunc("POST /transactions", a.addTxn)
	mux.HandleFunc("POST /transactions/delete", a.deleteTxn)
	mux.HandleFunc("POST /import", a.importTxns)
	mux.HandleFunc("GET /prices", a.pricesPage)
	mux.HandleFunc("POST /prices", a.addPrices)
	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		msg, err := a.Refresh()
		back := r.FormValue("back")
		if !strings.HasPrefix(back, "/") {
			back = "/"
		}
		if err != nil {
			redirect(w, r, back, msg, err.Error())
			return
		}
		redirect(w, r, back, msg, "")
	})
	mux.HandleFunc("GET /export/transactions.csv", a.exportCSV(WriteTransactionsCSV, "transactions"))
	mux.HandleFunc("GET /export/holdings.csv", a.exportCSV(WriteHoldingsCSV, "holdings"))
	a.registerTax(mux)
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	go func() {
		for {
			if msg, err := a.Refresh(); err != nil {
				log.Printf("refresh: %s: %v", msg, err)
			}
			time.Sleep(30 * time.Minute)
		}
	}()

	log.Printf("fundtrack listening on http://%s (data: %s)", addr, dataFile)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func redirect(w http.ResponseWriter, r *http.Request, path, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if len(q) > 0 {
		path += sep + q.Encode()
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func (a *App) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	data["Active"] = page
	if _, ok := data["Msg"]; !ok {
		data["Msg"] = r.URL.Query().Get("msg")
	}
	if _, ok := data["Err"]; !ok {
		data["Err"] = r.URL.Query().Get("err")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.pages[page].ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func (a *App) compute() (res *Result, last string) {
	a.store.View(func(db *DB) {
		res = Compute(db, time.Now())
		last = db.LastRefresh
	})
	return
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	res, last := a.compute()
	lastFmt := "never"
	if t, err := time.Parse(time.RFC3339, last); err == nil {
		lastFmt = t.Local().Format("2 Jan 15:04")
	}
	a.render(w, r, "dashboard", map[string]any{
		"R": res, "Chart": chartSVG(res.Chart), "Last": lastFmt, "Empty": len(res.Rows) == 0,
	})
}

func (a *App) txnPage(w http.ResponseWriter, r *http.Request, form map[string]string, errMsg string) {
	res, _ := a.compute()
	rows := make([]Row, len(res.Rows))
	for i, x := range res.Rows { // newest first
		rows[len(rows)-1-i] = x
	}
	data := map[string]any{"Rows": rows, "Form": form, "Suggest": res.Suggested}
	if errMsg != "" {
		data["Err"] = errMsg
	}
	a.render(w, r, "transactions", data)
}

func (a *App) addTxn(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	form := map[string]string{}
	for _, k := range []string{"market", "symbol", "name", "type", "date", "units", "price", "fees", "amount", "fx", "notes",
		"franked", "franking", "foreign_tax", "cg_disc", "cg_other", "tax_deferred", "tax_year"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	fail := func(msg string) { a.txnPage(w, r, form, msg) }

	market := strings.ToUpper(form["market"])
	if market != MarketASX && market != MarketUS && market != MarketManaged {
		fail("choose a market")
		return
	}
	sym := normSymbol(form["symbol"], market)
	if sym == "" {
		fail("symbol is required")
		return
	}
	date, err := parseDate(form["date"])
	if err != nil {
		fail(err.Error())
		return
	}
	t := Txn{ID: newID(), Date: date, Symbol: sym, Type: form["type"],
		Units: pnum(form["units"]), Price: pnum(form["price"]), Fees: pnum(form["fees"]),
		Amount: pnum(form["amount"]), FX: pnum(form["fx"]), Notes: form["notes"],
		Franked: pnum(form["franked"]), Franking: pnum(form["franking"]), ForeignTax: pnum(form["foreign_tax"]),
		CGDisc: pnum(form["cg_disc"]), CGOther: pnum(form["cg_other"]), TaxDeferred: pnum(form["tax_deferred"]),
		TaxYear: form["tax_year"]}
	if err := validateTxn(&t); err != nil {
		fail(err.Error())
		return
	}
	var have bool
	a.store.View(func(db *DB) { have = db.Instruments[sym] != nil })
	var inst *Instrument
	if !have {
		if inst, err = resolveInstrument(sym, market, form["name"]); err != nil {
			fail(err.Error())
			return
		}
	}
	a.store.Update(func(db *DB) error {
		if inst != nil && db.Instruments[sym] == nil {
			db.Instruments[sym] = inst
		}
		db.Txns = append(db.Txns, t)
		return nil
	})
	if !have {
		go a.Refresh()
	}
	redirect(w, r, "/transactions", fmt.Sprintf("Added %s %s", t.Type, sym), "")
}

func (a *App) deleteTxn(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	a.store.Update(func(db *DB) error {
		for i, t := range db.Txns {
			if t.ID == id {
				db.Txns = append(db.Txns[:i], db.Txns[i+1:]...)
				break
			}
		}
		return nil
	})
	redirect(w, r, "/transactions", "Transaction deleted", "")
}

func (a *App) importTxns(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	file, _, err := r.FormFile("file")
	if err != nil {
		redirect(w, r, "/transactions", "", "choose a CSV file to import")
		return
	}
	defer file.Close()
	txns, meta, err := ImportCSV(file)
	if err != nil {
		redirect(w, r, "/transactions", "", "import failed: "+err.Error())
		return
	}
	// resolve any unknown instruments first (network, outside the lock)
	known := map[string]bool{}
	a.store.View(func(db *DB) {
		for s := range db.Instruments {
			known[s] = true
		}
	})
	newInst := map[string]*Instrument{}
	for sym, m := range meta {
		if known[sym] {
			continue
		}
		inst, err := resolveInstrument(sym, m[0], m[1])
		if err != nil {
			redirect(w, r, "/transactions", "", "import failed: "+err.Error())
			return
		}
		newInst[sym] = inst
	}
	added, skipped := 0, 0
	a.store.Update(func(db *DB) error {
		for s, i := range newInst {
			if db.Instruments[s] == nil {
				db.Instruments[s] = i
			}
		}
		for _, t := range txns {
			dup := false
			for _, e := range db.Txns {
				if sameTxn(e, t) {
					dup = true
					break
				}
			}
			if dup {
				skipped++
				continue
			}
			db.Txns = append(db.Txns, t)
			added++
		}
		return nil
	})
	if len(newInst) > 0 {
		go a.Refresh()
	}
	redirect(w, r, "/transactions", fmt.Sprintf("Imported %d transactions (%d duplicates skipped)", added, skipped), "")
}

func (a *App) pricesPage(w http.ResponseWriter, r *http.Request) {
	type fund struct {
		Symbol, Name string
		Prices       []ChartPt // Date/Value reused
	}
	var funds []fund
	a.store.View(func(db *DB) {
		for _, inst := range db.Instruments {
			if inst.Market != MarketManaged {
				continue
			}
			f := fund{Symbol: inst.Symbol, Name: inst.Name}
			for d, v := range db.Manual[inst.Symbol] {
				f.Prices = append(f.Prices, ChartPt{Date: d, Value: v})
			}
			sort.Slice(f.Prices, func(i, j int) bool { return f.Prices[i].Date > f.Prices[j].Date })
			funds = append(funds, f)
		}
	})
	sort.Slice(funds, func(i, j int) bool { return funds[i].Symbol < funds[j].Symbol })
	a.render(w, r, "prices", map[string]any{"Funds": funds, "Today": time.Now().Format(dateFmt)})
}

// addPrices accepts "date,price" lines (or "date price") for a managed fund.
func (a *App) addPrices(w http.ResponseWriter, r *http.Request) {
	sym := r.FormValue("symbol")
	n := 0
	var bad []string
	prices := map[string]float64{}
	for _, ln := range strings.Split(r.FormValue("data"), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		parts := strings.FieldsFunc(ln, func(c rune) bool { return c == ',' || c == '\t' || c == ';' || c == ' ' })
		if len(parts) < 2 {
			bad = append(bad, ln)
			continue
		}
		d, err := parseDate(parts[0])
		p := pnum(parts[1])
		if err != nil || p <= 0 {
			if strings.EqualFold(parts[0], "date") {
				continue // header row
			}
			bad = append(bad, ln)
			continue
		}
		prices[d] = p
	}
	err := a.store.Update(func(db *DB) error {
		inst := db.Instruments[sym]
		if inst == nil || inst.Market != MarketManaged {
			return fmt.Errorf("unknown managed fund %q", sym)
		}
		if db.Manual[sym] == nil {
			db.Manual[sym] = map[string]float64{}
		}
		for d, p := range prices {
			db.Manual[sym][d] = p
			n++
		}
		return nil
	})
	switch {
	case err != nil:
		redirect(w, r, "/prices", "", err.Error())
	case len(bad) > 0:
		redirect(w, r, "/prices", fmt.Sprintf("Saved %d prices", n), "could not read: "+strings.Join(bad, " | "))
	default:
		redirect(w, r, "/prices", fmt.Sprintf("Saved %d prices", n), "")
	}
}

func (a *App) exportCSV(write func(w io.Writer, res *Result) error, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, _ := a.compute()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fundtrack-%s-%s.csv"`, name, time.Now().Format(dateFmt)))
		w.Write([]byte("\ufeff"))
		if err := write(w, res); err != nil {
			log.Printf("export %s: %v", name, err)
		}
	}
}

// ---------- template helpers ----------

var funcs = template.FuncMap{
	"money":  func(v float64) string { return commas(v, 2) },
	"money0": func(v float64) string { return commas(v, 0) },
	"signed": func(v float64) string {
		if v > 0.005 {
			return "+" + commas(v, 2)
		}
		return commas(v, 2)
	},
	"units": func(v float64) string {
		s := fmt.Sprintf("%.4f", v)
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		return commas2(s)
	},
	"num4": func(v float64) string { return commas(v, 4) },
	"pct": func(v float64) string {
		if v > 0.005 {
			return fmt.Sprintf("+%.2f%%", v)
		}
		return fmt.Sprintf("%.2f%%", v)
	},
	"cls": func(v float64) string {
		switch {
		case v > 0.005:
			return "pos"
		case v < -0.005:
			return "neg"
		}
		return ""
	},
	"short": func(s string) string { return strings.TrimSuffix(s, ".AX") },
	"abs":   math.Abs,
	"addf":  func(a, b float64) float64 { return a + b },
	"typeLabel": func(t string) string {
		return map[string]string{"buy": "Buy", "sell": "Sell", "dividend": "Distribution", "drp": "Reinvested"}[t]
	},
	"sel": func(a, b string) string {
		if a == b {
			return "selected"
		}
		return ""
	},
}

func commas(v float64, dp int) string {
	neg := v < 0 && math.Abs(v) >= math.Pow(10, -float64(dp))/2
	s := fmt.Sprintf("%.*f", dp, math.Abs(v))
	s = commas2(s)
	if neg {
		return "-" + s
	}
	return s
}

func commas2(s string) string {
	intPart, frac := s, ""
	if i := strings.Index(s, "."); i >= 0 {
		intPart, frac = s[:i], s[i:]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String() + frac
}

// chartSVG renders portfolio value vs net invested as an inline SVG.
func chartSVG(pts []ChartPt) template.HTML {
	if len(pts) < 2 {
		return ""
	}
	const W, H, L, R, T, B = 900.0, 260.0, 64.0, 12.0, 12.0, 28.0
	maxV := 0.0
	for _, p := range pts {
		maxV = math.Max(maxV, math.Max(p.Value, p.Invested))
	}
	if maxV <= 0 {
		return ""
	}
	maxV *= 1.05
	x := func(i int) float64 { return L + (W-L-R)*float64(i)/float64(len(pts)-1) }
	y := func(v float64) float64 { return T + (H-T-B)*(1-v/maxV) }
	line := func(get func(ChartPt) float64) string {
		var b strings.Builder
		for i, p := range pts {
			c := "L"
			if i == 0 {
				c = "M"
			}
			fmt.Fprintf(&b, "%s%.1f %.1f ", c, x(i), y(get(p)))
		}
		return b.String()
	}
	val := line(func(p ChartPt) float64 { return p.Value })
	inv := line(func(p ChartPt) float64 { return p.Invested })
	var s strings.Builder
	fmt.Fprintf(&s, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="Portfolio value versus net invested">`, W, H)
	for i := 0; i <= 4; i++ {
		v := maxV * float64(i) / 4
		fmt.Fprintf(&s, `<line class="grid" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/><text class="axis" x="%.0f" y="%.1f" text-anchor="end">$%s</text>`,
			L, W-R, y(v), y(v), L-8, y(v)+4, template.HTMLEscapeString(shortMoney(v)))
	}
	fmt.Fprintf(&s, `<path class="area" d="%sL%.1f %.1f L%.1f %.1f Z"/>`, val, x(len(pts)-1), y(0), x(0), y(0))
	fmt.Fprintf(&s, `<path class="inv" d="%s"/><path class="val" d="%s"/>`, inv, val)
	fmt.Fprintf(&s, `<text class="axis" x="%.0f" y="%.0f">%s</text><text class="axis" x="%.0f" y="%.0f" text-anchor="end">%s</text>`,
		L, H-8, pts[0].Date, W-R, H-8, pts[len(pts)-1].Date)
	s.WriteString(`</svg>`)
	return template.HTML(s.String())
}

func shortMoney(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.0fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}
