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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type App struct {
	liveSaved time.Time // when the live-quote side file was last written
	pages     map[string]*template.Template
	refreshMu sync.Mutex
	modes     // real vs test data stores (see testmode.go)
}

var pageNames = []string{"dashboard", "transactions", "prices", "tax", "distributions", "estimator", "outlook", "import", "plan", "reconcile", "private", "charts"}

// newApp opens the data stores and parses every page template.
func newApp(dataFile string) (*App, error) {
	a := &App{pages: map[string]*template.Template{}}
	if err := a.initModes(dataFile); err != nil {
		return nil, err
	}
	for _, p := range pageNames {
		tpl, err := template.New("").Funcs(funcs).ParseFS(webFS, "web/base.html", "web/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", p, err)
		}
		a.pages[p] = tpl
	}
	return a, nil
}

// routes is the whole HTTP surface.
func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("GET /transactions", func(w http.ResponseWriter, r *http.Request) {
		form := map[string]string{"date": time.Now().Format(dateFmt), "type": "buy", "market": MarketASX}
		for _, k := range []string{"symbol", "market", "type", "date", "amount", "franked", "franking", "currency"} {
			if v := r.URL.Query().Get(k); v != "" {
				form[k] = v
			}
		}
		a.txnPage(w, r, form, "")
	})
	mux.HandleFunc("POST /transactions", a.addTxn)
	mux.HandleFunc("POST /transactions/delete", a.deleteTxn)
	a.registerImport(mux)
	a.registerTestMode(mux)
	a.registerPrices(mux)
	a.registerReconcile(mux)
	a.registerPrivate(mux)
	mux.HandleFunc("GET /charts", a.chartsPage)
	mux.HandleFunc("POST /prices", a.addPrices)
	mux.HandleFunc("POST /refresh", func(w http.ResponseWriter, r *http.Request) {
		msg, err := a.RefreshNow()
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
	return withGzip(mux)
}

func main() {
	addr := env("ADDR", "127.0.0.1:8080")
	dataFile := env("DATA_FILE", "data/fundtrack.json")
	a, err := newApp(dataFile)
	if err != nil {
		log.Fatal(err)
	}

	go a.liveLoop()
	go func() { _, _ = a.LiveTick(false) }()
	go func() {
		for {
			if msg, err := a.Refresh(); err != nil {
				log.Printf("refresh: %s: %v", msg, err)
			}
			time.Sleep(30 * time.Minute)
		}
	}()

	log.Printf("fundtrack listening on http://%s (data: %s)", addr, dataFile)
	log.Fatal(http.ListenAndServe(addr, a.routes()))
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
	data["Test"] = a.inTest()
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
	a.st().View(func(db *DB) {
		res = Compute(db, time.Now())
		last = db.LastRefresh
	})
	return
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	var res *Result
	var last string
	var lv liveView
	a.st().View(func(db *DB) {
		now := liveNow()
		res, last = Compute(db, now), db.LastRefresh
		lv = buildLiveView(db, a.inTest(), res, now)
	})
	lastFmt := "never"
	if t, err := time.Parse(time.RFC3339, last); err == nil {
		lastFmt = t.Local().Format("2 Jan 15:04")
	}
	a.render(w, r, "dashboard", map[string]any{
		"R": res, "Last": lastFmt, "LV": lv, "Empty": len(res.Rows) == 0, "HasChart": len(res.Chart) > 1,
		"Charts": chartJS(map[string]*ChartCfg{"value": portfolioChart(res.Chart)}),
	})
}

func (a *App) txnPage(w http.ResponseWriter, r *http.Request, form map[string]string, errMsg string) {
	res, _ := a.compute()
	rows := make([]Row, len(res.Rows))
	for i, x := range res.Rows { // newest first
		rows[len(rows)-1-i] = x
	}
	var imps []ImportRec
	a.st().View(func(db *DB) {
		for i := len(db.Imports) - 1; i >= 0 && len(imps) < 6; i-- { // newest first
			imps = append(imps, db.Imports[i])
		}
	})
	data := map[string]any{"Rows": rows, "Form": form, "Suggest": res.Suggested, "Imports": imps}
	if errMsg != "" {
		data["Err"] = errMsg
	}
	a.render(w, r, "transactions", data)
}

func (a *App) addTxn(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	form := map[string]string{}
	for _, k := range []string{"market", "symbol", "name", "currency", "type", "date", "units", "price", "total_cost", "fees", "amount", "fx", "notes",
		"franked", "franking", "foreign_tax", "cg_disc", "cg_other", "tax_deferred", "tax_year"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	fail := func(msg string) { a.txnPage(w, r, form, msg) }

	market := strings.ToUpper(form["market"])
	if market != MarketASX && market != MarketUS && market != MarketManaged && market != MarketPrivate {
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
	units, price := pnum(form["units"]), pnum(form["price"])
	var priceNote string
	if r.FormValue("total_cost") != "" && price > 0 { // the figure typed is the whole cost basis, not per share
		if units <= 0 {
			fail("enter the number of units to work out a price from a total cost basis")
			return
		}
		price = round6(price / units)
		priceNote = fmt.Sprintf("price worked out from a total cost basis of %s", commas(pnum(form["price"]), 2))
	}
	if price <= 0 && (market == MarketASX || market == MarketUS) && (form["type"] == "buy" || form["type"] == "sell" || form["type"] == "award") {
		var saved map[string]float64
		a.st().View(func(db *DB) {
			if db.Instruments[sym] != nil {
				saved = map[string]float64{}
				for d, v := range db.Prices[sym] {
					saved[d] = v
				}
				for d, v := range db.Manual[sym] {
					saved[d] = v
				}
			}
		})
		p, used, lerr := lookupClose(sym, date, saved, time.Now())
		if lerr != nil {
			fail("couldn't look up a price for " + short(sym) + " on " + date + " (" + lerr.Error() + "). Enter a price or your cost basis instead.")
			return
		}
		price = p
		priceNote = "price looked up: market close on " + used
	}
	t := Txn{ID: newID(), Date: date, Symbol: sym, Type: form["type"],
		Units: units, Price: price, Fees: pnum(form["fees"]),
		Amount: pnum(form["amount"]), FX: pnum(form["fx"]), Notes: form["notes"],
		Franked: pnum(form["franked"]), Franking: pnum(form["franking"]), ForeignTax: pnum(form["foreign_tax"]),
		CGDisc: pnum(form["cg_disc"]), CGOther: pnum(form["cg_other"]), TaxDeferred: pnum(form["tax_deferred"]),
		TaxYear: form["tax_year"]}
	if priceNote != "" && t.Notes == "" {
		t.Notes = priceNote
	}
	if err := validateTxn(&t); err != nil {
		fail(err.Error())
		return
	}
	var have bool
	a.st().View(func(db *DB) { have = db.Instruments[sym] != nil })
	var inst *Instrument
	if !have {
		if inst, err = resolveInstrument(sym, market, form["name"]); err != nil {
			fail(err.Error())
			return
		}
		if c := strings.ToUpper(form["currency"]); c != "" && (market == MarketPrivate || market == MarketManaged) {
			if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(c) {
				fail("currency should be a three-letter code such as AUD or USD")
				return
			}
			inst.Currency = c
		}
	}
	a.st().Update(func(db *DB) error {
		if inst != nil && db.Instruments[sym] == nil {
			db.Instruments[sym] = inst
		}
		t.FP = Fingerprint(t)
		db.Txns = append(db.Txns, t)
		return nil
	})
	if !have {
		go a.Refresh()
	}
	msg := fmt.Sprintf("Added %s %s", t.Type, short(sym))
	if priceNote != "" {
		msg += " at " + commas(t.Price, 4) + " (" + priceNote + ")"
	}
	redirect(w, r, "/transactions", msg, "")
}

func (a *App) deleteTxn(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	a.st().Update(func(db *DB) error {
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
	err := a.st().Update(func(db *DB) error {
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
	"hm":    clock,
	"sub":   func(a, b float64) float64 { return a - b },
	"pc":    func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) },
	"pp": func(v float64) string { // signed percentage points
		if v > 0.05 {
			return fmt.Sprintf("+%.1f pts", v)
		}
		return fmt.Sprintf("%.1f pts", v)
	},
	"pin": func(v float64) string { // a fraction shown as a plain percent for an input box
		if v <= 0 {
			return ""
		}
		return strconv.FormatFloat(math.Round(v*1000)/10, 'f', -1, 64)
	},
	"pcg": func(v, base float64) float64 { // % change of v over base
		if base == 0 {
			return 0
		}
		return (v/base - 1) * 100
	},
	"pctn": func(v float64) string { // percentage that tolerates "no data"
		switch {
		case math.IsNaN(v):
			return "—"
		case v > 0.005:
			return fmt.Sprintf("+%.1f%%", v)
		}
		return fmt.Sprintf("%.1f%%", v)
	},
	"f1": func(v float64) string {
		if math.IsNaN(v) {
			return "—"
		}
		return fmt.Sprintf("%.1f", v)
	},
	"f2": func(v float64) string {
		if math.IsNaN(v) {
			return "—"
		}
		return fmt.Sprintf("%.2f", v)
	},
	"addf": func(a, b float64) float64 { return a + b },
	"mulf": func(a, b float64) float64 { return a * b },
	// orig shows the original-currency amount in small brackets after an AUD figure; nothing for AUD itself.
	"orig": func(ccy string, v float64) string {
		if ccy == "" || ccy == "AUD" {
			return ""
		}
		return "(" + ccy + " " + commas(v, 2) + ")"
	},
	"add": func(a, b int) int { return a + b },
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"typeLabel": func(t string) string {
		return map[string]string{"buy": "Buy", "sell": "Sell", "dividend": "Distribution", "drp": "Reinvested", "award": "Award (RSU)"}[t]
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
