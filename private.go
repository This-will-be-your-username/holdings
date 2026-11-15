package main

import (
	"fmt"
	"html"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Private (unlisted) companies have no market price. Their value is whatever you record as a checkpoint: a date and
// either the price per share or the company's reported valuation (turned into a price per share using the shares on
// issue). Between checkpoints the last one is carried forward - nothing is invented.
//
// To help find a newer figure there is a web search for press coverage of funding rounds and valuations. It only
// ever proposes candidates with their source; nothing is saved until you choose to record it.

// Checkpoint is one known value of a private company.
type Checkpoint struct {
	ID        string  `json:"id"`
	Date      string  `json:"date"`
	Price     float64 `json:"price"`               // per share, in the instrument's currency
	Valuation float64 `json:"valuation,omitempty"` // whole-company valuation it was derived from, if given
	Source    string  `json:"source,omitempty"`    // where the figure came from (a link or a description)
	Note      string  `json:"note,omitempty"`
	Origin    string  `json:"origin,omitempty"` // manual | search
}

// AddCheckpoint validates and stores a checkpoint. If only a company valuation is given, the price per share is the
// valuation divided by the shares on issue; failing that it is scaled from an earlier checkpoint that had both.
func AddCheckpoint(db *DB, sym string, cp Checkpoint) (Checkpoint, error) {
	inst := db.Instruments[sym]
	if inst == nil || !inst.Private {
		return cp, fmt.Errorf("%q isn't a private company in your records", sym)
	}
	if _, err := time.Parse(dateFmt, cp.Date); err != nil {
		return cp, fmt.Errorf("enter the date this value applies to")
	}
	if cp.Price < 0 || cp.Valuation < 0 || (cp.Price == 0 && cp.Valuation == 0) {
		return cp, fmt.Errorf("enter a price per share or a company valuation")
	}
	if cp.Price == 0 {
		switch {
		case inst.SharesOut > 0:
			cp.Price = cp.Valuation / inst.SharesOut
		default:
			var ref *Checkpoint
			for i := range db.Checkpoints[sym] {
				c := &db.Checkpoints[sym][i]
				if c.Valuation > 0 && c.Price > 0 && c.Date <= cp.Date && (ref == nil || c.Date > ref.Date) {
					ref = c
				}
			}
			if ref == nil {
				return cp, fmt.Errorf("to use a company valuation, enter the shares on issue for %s (or give the price per share directly)", inst.Name)
			}
			cp.Price = ref.Price * cp.Valuation / ref.Valuation
			cp.Note = strings.TrimSpace(cp.Note + " (price scaled from the " + ref.Date + " checkpoint; ignores dilution)")
		}
	}
	cp.Price = math.Round(cp.Price*1e6) / 1e6
	if cp.Price <= 0 {
		return cp, fmt.Errorf("that works out to a price of zero")
	}
	if cp.ID == "" {
		cp.ID = newID()
	}
	if cp.Origin == "" {
		cp.Origin = "manual"
	}
	cps := db.Checkpoints[sym]
	for i := range cps { // one value per date: a new one replaces it
		if cps[i].Date == cp.Date {
			cps = append(cps[:i], cps[i+1:]...)
			break
		}
	}
	db.Checkpoints[sym] = append(cps, cp)
	syncCheckpoints(db, sym)
	return cp, nil
}

func DeleteCheckpoint(db *DB, sym, id string) bool {
	cps := db.Checkpoints[sym]
	for i := range cps {
		if cps[i].ID == id {
			db.Checkpoints[sym] = append(cps[:i], cps[i+1:]...)
			syncCheckpoints(db, sym)
			return true
		}
	}
	return false
}

// syncCheckpoints makes the price series the rest of the app reads match the checkpoints.
func syncCheckpoints(db *DB, sym string) {
	sort.Slice(db.Checkpoints[sym], func(i, j int) bool { return db.Checkpoints[sym][i].Date < db.Checkpoints[sym][j].Date })
	m := map[string]float64{}
	for _, c := range db.Checkpoints[sym] {
		m[c.Date] = c.Price
	}
	if len(m) == 0 {
		delete(db.Manual, sym)
		return
	}
	db.Manual[sym] = m
}

// parseAmount reads "1,200,000", "350m", "4.2b" or "$4.2 billion".
func parseAmount(s string) float64 {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(",", "", "$", "", " ", "").Replace(s)
	mult := 1.0
	for suf, m := range map[string]float64{"billion": 1e9, "bn": 1e9, "b": 1e9, "million": 1e6, "mn": 1e6, "m": 1e6, "k": 1e3, "thousand": 1e3} {
		if strings.HasSuffix(s, suf) {
			s, mult = strings.TrimSuffix(s, suf), m
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * mult
}

// ---------- web search ----------

var searchBase = "https://html.duckduckgo.com/html/"

// Candidate is a valuation figure found in a public web result. It is a lead to check, not a fact.
type Candidate struct {
	Title, URL, Host, Snippet string
	Date                      string // when the article says it happened, if it could be read
	Amount                    float64
	Currency                  string

	// Set when the figure is in another currency than the company's and a rate was available to convert it.
	Converted            bool
	ConvAmount, ConvRate float64
	ConvDate, ConvNote   string
}

// fxRate returns AUD per 1 unit of ccy on (or just before) date, from the saved rates.
func fxRate(db *DB, ccy, date string) (float64, string, bool) {
	if ccy == "" || ccy == "AUD" {
		return 1, date, true
	}
	s := newSeries(db.FX[ccy])
	if v, d, ok := s.At(date); ok {
		return v, d, true
	}
	if v, d, ok := s.Last(); ok {
		return v, d, true
	}
	return 0, "", false
}

// convertCandidate expresses a found figure in the company's currency, going through AUD.
func convertCandidate(db *DB, c Candidate, to, today string) Candidate {
	if c.Currency == to {
		return c
	}
	on := c.Date
	if on == "" {
		on = today
	}
	from, fd, ok1 := fxRate(db, c.Currency, on)
	dest, _, ok2 := fxRate(db, to, on)
	if !ok1 || !ok2 {
		return c
	}
	c.Converted, c.ConvAmount, c.ConvRate, c.ConvDate = true, c.Amount*from/dest, from/dest, fd
	c.ConvNote = fmt.Sprintf("%s %s converted to %s at %.4f (rate of %s)", c.Currency, commas(c.Amount, 0), to, from/dest, fd)
	return c
}

var (
	reResult = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]*href="([^"]+)"[^>]*>(.*?)</a>.*?<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	reTags   = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpace  = regexp.MustCompile(`\s+`)

	curPre = `((?:US|AU|CA|NZ|A|C)?\s?[$£€])`
	numPat = `(\d[\d,]*(?:\.\d+)?)`
	reValA = regexp.MustCompile(`(?i)valu(?:ed|ation|es|ing)\b[^.$£€]{0,45}?` + curPre + `\s?` + numPat + `\s*(billion|million|trillion|bn|bln|mn|m|b)?\b`)
	reValB = regexp.MustCompile(`(?i)` + curPre + `\s?` + numPat + `\s*(billion|million|trillion|bn|bln|mn|m|b)\b[^.$£€]{0,25}?\bvaluation`)

	months   = `jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec`
	reDateDM = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+(` + months + `)[a-z]*\.?,?\s+(20\d\d)\b`)
	reDateMD = regexp.MustCompile(`(?i)\b(` + months + `)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(20\d\d)\b`)
	reDateIS = regexp.MustCompile(`\b(20\d\d)-(\d\d)-(\d\d)\b`)
	reDateMY = regexp.MustCompile(`(?i)\b(` + months + `)[a-z]*\.?,?\s+(20\d\d)\b`)

	nameStop = map[string]bool{"pty": true, "ltd": true, "limited": true, "inc": true, "corp": true, "the": true, "co": true, "llc": true, "plc": true, "and": true}
)

func monthNum(s string) int {
	i := strings.Index("janfebmaraprmayjunjulaugsepoctnovdec", strings.ToLower(s[:3]))
	return i/3 + 1
}

func findDate(text string) string {
	if m := reDateIS.FindStringSubmatch(text); m != nil {
		return m[0]
	}
	if m := reDateDM.FindStringSubmatch(text); m != nil {
		d, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[3])
		return time.Date(y, time.Month(monthNum(m[2])), d, 0, 0, 0, 0, time.UTC).Format(dateFmt)
	}
	if m := reDateMD.FindStringSubmatch(text); m != nil {
		d, _ := strconv.Atoi(m[2])
		y, _ := strconv.Atoi(m[3])
		return time.Date(y, time.Month(monthNum(m[1])), d, 0, 0, 0, 0, time.UTC).Format(dateFmt)
	}
	if m := reDateMY.FindStringSubmatch(text); m != nil {
		y, _ := strconv.Atoi(m[2])
		return time.Date(y, time.Month(monthNum(m[1])), 1, 0, 0, 0, 0, time.UTC).Format(dateFmt)
	}
	return ""
}

func currencyOf(pre, host string) string {
	pre = strings.ToUpper(strings.ReplaceAll(pre, " ", ""))
	switch {
	case strings.HasPrefix(pre, "US"):
		return "USD"
	case strings.HasPrefix(pre, "AU"), pre == "A$":
		return "AUD"
	case strings.HasPrefix(pre, "CA"), pre == "C$":
		return "CAD"
	case strings.HasPrefix(pre, "NZ"):
		return "NZD"
	case strings.Contains(pre, "£"):
		return "GBP"
	case strings.Contains(pre, "€"):
		return "EUR"
	}
	if strings.HasSuffix(host, ".au") { // a bare "$" in an Australian outlet
		return "AUD"
	}
	return "USD"
}

func scaleOf(s string) float64 {
	switch strings.ToLower(s) {
	case "trillion":
		return 1e12
	case "billion", "bn", "bln", "b":
		return 1e9
	case "million", "mn", "m":
		return 1e6
	}
	return 1
}

// extractValuations pulls "valued at $X" style figures out of a result's text.
func extractValuations(text, host string) []Candidate {
	var out []Candidate
	add := func(pre, num, unit string) {
		n, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", ""), 64)
		amt := n * scaleOf(unit)
		if err != nil || amt < 1e6 { // a company valuation under a million is almost certainly a misread
			return
		}
		out = append(out, Candidate{Amount: amt, Currency: currencyOf(pre, host)})
	}
	for _, m := range reValA.FindAllStringSubmatch(text, -1) {
		add(m[1], m[2], m[3])
	}
	for _, m := range reValB.FindAllStringSubmatch(text, -1) {
		add(m[1], m[2], m[3])
	}
	return out
}

func nameWords(term string) []string {
	var w []string
	for _, t := range strings.Fields(normName(term)) {
		if !nameStop[t] {
			w = append(w, t)
		}
	}
	return w
}

func mentions(text, term string) bool {
	words := nameWords(term)
	if len(words) == 0 {
		return false
	}
	n := " " + normName(text) + " "
	for _, w := range words {
		if !strings.Contains(n, " "+w+" ") && !strings.Contains(n, " "+w+"s ") {
			return false
		}
	}
	return true
}

func cleanText(s string) string {
	return strings.TrimSpace(reSpace.ReplaceAllString(html.UnescapeString(reTags.ReplaceAllString(s, " ")), " "))
}

func resultURL(href string) string {
	href = html.UnescapeString(href)
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	if u, err := url.Parse(href); err == nil {
		if t := u.Query().Get("uddg"); t != "" {
			return t
		}
	}
	return href
}

func httpGetHTML(u string) ([]byte, error) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; fundtrack/1.0)")
	req.Header.Set("Accept", "text/html")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("search service answered %d", resp.StatusCode)
	}
	var buf strings.Builder
	b := make([]byte, 32<<10)
	for buf.Len() < 4<<20 {
		n, e := resp.Body.Read(b)
		buf.Write(b[:n])
		if e != nil {
			break
		}
	}
	return []byte(buf.String()), nil
}

// SearchValuations looks for reported valuations of a company in public web results. Only the company name is sent.
// The newest dated figures come first; figures that can't be dated come last.
func SearchValuations(term string) ([]Candidate, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, fmt.Errorf("enter a company name to search for")
	}
	queries := []string{`"` + term + `" valuation`, `"` + term + `" funding round valued at`}
	var out []Candidate
	seen := map[string]bool{}
	okAny := false
	var lastErr error
	for _, q := range queries {
		b, err := httpGetHTML(searchBase + "?q=" + url.QueryEscape(q))
		if err != nil {
			lastErr = err
			continue
		}
		okAny = true
		for _, m := range reResult.FindAllStringSubmatch(string(b), -1) {
			link := resultURL(m[1])
			title, snip := cleanText(m[2]), cleanText(m[3])
			text := title + ". " + snip
			if !mentions(text, term) {
				continue
			}
			host := ""
			if u, err := url.Parse(link); err == nil {
				host = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
			}
			date := findDate(text)
			for _, c := range extractValuations(text, host) {
				key := fmt.Sprintf("%s|%.0f", host, c.Amount)
				if seen[key] {
					continue
				}
				seen[key] = true
				c.Title, c.URL, c.Host, c.Snippet, c.Date = title, link, host, snip, date
				out = append(out, c)
			}
		}
	}
	if !okAny {
		return nil, fmt.Errorf("the web search didn't respond (%v). You can search yourself and record what you find below", lastErr)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Date == "") != (out[j].Date == "") {
			return out[i].Date != ""
		}
		return out[i].Date > out[j].Date
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out, nil
}

// ---------- pages ----------

func (a *App) registerPrivate(mux *http.ServeMux) {
	mux.HandleFunc("GET /private", func(w http.ResponseWriter, r *http.Request) { a.privatePage(w, r, "", nil, "") })
	mux.HandleFunc("POST /private/company", a.privateCompany)
	mux.HandleFunc("POST /private/checkpoint", a.privateCheckpoint)
	mux.HandleFunc("POST /private/checkpoint/delete", a.privateCheckpointDelete)
	mux.HandleFunc("POST /private/search", a.privateSearch)
	mux.HandleFunc("POST /private/company/delete", a.privateCompanyDelete)
}

// privateCompanyDelete removes a private company. One with no transactions goes straight away; one that has
// transactions is only removed (with them, and its checkpoints) after an explicit confirmation.
func (a *App) privateCompanyDelete(w http.ResponseWriter, r *http.Request) {
	sym := r.FormValue("symbol")
	confirmed := r.FormValue("confirm") != ""
	var name string
	var nTxn int
	err := a.st().Update(func(db *DB) error {
		inst := db.Instruments[sym]
		if inst == nil || !inst.Private {
			return fmt.Errorf("%q isn't a private company in your records", sym)
		}
		name = inst.Name
		for _, t := range db.Txns {
			if t.Symbol == sym {
				nTxn++
			}
		}
		if nTxn > 0 && !confirmed {
			return fmt.Errorf("%s has %d transaction%s. Tick the box to confirm you want them deleted with the company", name, nTxn, plural(nTxn))
		}
		keep := db.Txns[:0:0]
		for _, t := range db.Txns {
			if t.Symbol != sym {
				keep = append(keep, t)
			}
		}
		db.Txns = keep
		delete(db.Instruments, sym)
		delete(db.Checkpoints, sym)
		delete(db.Manual, sym)
		delete(db.Prices, sym)
		delete(db.Divs, sym)
		delete(db.DivInfo, sym)
		delete(db.PriceAt, sym)
		delete(db.Quotes, sym)
		return nil
	})
	if err != nil {
		redirect(w, r, "/private", "", err.Error())
		return
	}
	msg := "Deleted " + name
	if nTxn > 0 {
		msg += fmt.Sprintf(" and its %d transaction%s", nTxn, plural(nTxn))
	}
	redirect(w, r, "/private", msg, "")
}

type privRow struct {
	NTxn                               int // transactions recorded for it
	Symbol, Name, Currency, SearchTerm string
	SharesOut                          float64
	DeferredTax                        bool
	Units, ValueAUD, CostAUD           float64
	Price                              float64
	PriceAUD, FXRate                   float64 // the latest price in AUD and the rate used (foreign companies only)
	ValueNative, RateNow               float64 // holding value in the company's own currency, and the latest AUD rate for it
	RateDate                           string
	FXDate                             string
	PriceDate                          string
	AgeDays                            int
	Stale                              bool
	Checkpoints                        []Checkpoint
	Searched                           bool
	Cands                              []Candidate
	SearchErr                          string
	WebSearch                          string
	Pre                                map[string]string // prefilled checkpoint form
}

// staleAfterDays is how old the newest checkpoint can be before the page (and dashboard) flags it.
const staleAfterDays = 400

func (a *App) privatePage(w http.ResponseWriter, r *http.Request, searchSym string, cands []Candidate, searchErr string) {
	now := liveNow()
	var rows []privRow
	a.st().View(func(db *DB) {
		res := Compute(db, now)
		held := map[string]*Holding{}
		for _, h := range res.Holdings {
			held[h.Symbol] = h
		}
		for _, inst := range db.Instruments {
			if !inst.Private {
				continue
			}
			term := inst.SearchTerm
			if term == "" {
				term = inst.Name
			}
			p := privRow{Symbol: inst.Symbol, Name: inst.Name, Currency: inst.Currency, SearchTerm: term, SharesOut: inst.SharesOut, DeferredTax: inst.DeferredTax,
				WebSearch: "https://duckduckgo.com/?q=" + url.QueryEscape(`"`+term+`" valuation funding round`)}
			for _, t := range db.Txns {
				if t.Symbol == inst.Symbol {
					p.NTxn++
				}
			}
			if h := held[inst.Symbol]; h != nil {
				p.Units, p.ValueAUD, p.CostAUD = h.Units, h.ValueAUD, h.CostAUD
				p.ValueNative = h.Units * h.Price
				if inst.Currency != "AUD" {
					if v, d, ok := newSeries(db.FX[inst.Currency]).Last(); ok {
						p.RateNow, p.RateDate = v, d
					}
				}
			}
			for i := len(db.Checkpoints[inst.Symbol]) - 1; i >= 0; i-- {
				p.Checkpoints = append(p.Checkpoints, db.Checkpoints[inst.Symbol][i])
			}
			if len(p.Checkpoints) > 0 {
				p.Price, p.PriceDate = p.Checkpoints[0].Price, p.Checkpoints[0].Date
				if d, err := time.Parse(dateFmt, p.PriceDate); err == nil {
					p.AgeDays = int(now.Sub(d).Hours() / 24)
					p.Stale = p.AgeDays > staleAfterDays
				}
			}
			p.Pre = map[string]string{"date": now.Format(dateFmt)}
			if q := r.URL.Query(); q.Get("sym") == inst.Symbol {
				for _, k := range []string{"date", "valuation", "price", "source", "note"} {
					if v := q.Get(k); v != "" {
						p.Pre[k] = v
					}
				}
			}
			if inst.Symbol == searchSym {
				p.Searched, p.SearchErr = true, searchErr
				for _, c := range cands {
					p.Cands = append(p.Cands, convertCandidate(db, c, inst.Currency, now.Format(dateFmt)))
				}
			}
			if rate, d, ok := fxRate(db, inst.Currency, p.PriceDate); ok && inst.Currency != "AUD" && p.PriceDate != "" {
				p.PriceAUD, p.FXRate, p.FXDate = p.Price*rate, rate, d
			}
			rows = append(rows, p)
		}
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	a.render(w, r, "private", map[string]any{"Cos": rows, "Today": now.Format(dateFmt), "TestMode": a.inTest()})
}

func (a *App) privateCompany(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	sym := strings.ToUpper(strings.TrimSpace(r.FormValue("symbol")))
	ccy := strings.ToUpper(strings.TrimSpace(r.FormValue("currency")))
	if ccy == "" {
		ccy = "AUD"
	}
	if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(ccy) {
		redirect(w, r, "/private", "", "currency should be a three-letter code such as AUD or USD")
		return
	}
	shares := parseAmount(r.FormValue("shares_out"))
	switched := false
	deferred := r.FormValue("deferred_tax") != ""
	term := strings.TrimSpace(r.FormValue("search_term"))
	err := a.st().Update(func(db *DB) error {
		if sym == "" { // new company
			if name == "" {
				return fmt.Errorf("enter the company's name")
			}
			sym = "PVT-" + strings.Trim(regexp.MustCompile(`[^A-Z0-9]+`).ReplaceAllString(strings.ToUpper(name), "-"), "-")
			if len(sym) > 24 {
				sym = strings.TrimRight(sym[:24], "-")
			}
			if db.Instruments[sym] != nil {
				return fmt.Errorf("%s is already in your records", name)
			}
			db.Instruments[sym] = &Instrument{Symbol: sym, Name: name, Currency: ccy, Market: MarketManaged, Private: true, SharesOut: shares, SearchTerm: term, DeferredTax: deferred}
			return nil
		}
		inst := db.Instruments[sym]
		if inst == nil || !inst.Private {
			return fmt.Errorf("%q isn't a private company in your records", sym)
		}
		if name != "" {
			inst.Name = name
		}
		inst.SharesOut, inst.SearchTerm, inst.DeferredTax = shares, term, deferred
		if ccy != inst.Currency {
			used := len(db.Checkpoints[sym]) > 0
			for _, t := range db.Txns {
				used = used || t.Symbol == sym
			}
			// Recorded prices are plain numbers in the company's currency, so changing it re-labels them. Only do that
			// when the user confirms the numbers really were in the new currency (e.g. entered as USD but saved as AUD).
			if used && r.FormValue("reinterpret") != "1" {
				return fmt.Errorf("your existing prices and checkpoints for %s are recorded in %s. Tick the box to confirm they are really in %s, then save again", inst.Name, inst.Currency, ccy)
			}
			inst.Currency = ccy
			switched = true
		}
		return nil
	})
	if err != nil {
		redirect(w, r, "/private", "", err.Error())
		return
	}
	if switched {
		go a.RefreshFX(true) // make sure rates for the new currency are saved
		redirect(w, r, "/private", "Saved "+name+". Now valued in AUD using the latest "+ccy+" rate.", "")
		return
	}
	redirect(w, r, "/private", "Saved "+name, "")
}

func (a *App) privateCheckpoint(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sym := r.FormValue("symbol")
	date, derr := parseDate(r.FormValue("date"))
	if derr != nil {
		redirect(w, r, "/private", "", derr.Error())
		return
	}
	cp := Checkpoint{Date: date, Price: parseAmount(r.FormValue("price")), Valuation: parseAmount(r.FormValue("valuation")),
		Source: strings.TrimSpace(r.FormValue("source")), Note: strings.TrimSpace(r.FormValue("note")), Origin: "manual"}
	if r.FormValue("origin") == "search" {
		cp.Origin = "search"
	}
	var saved Checkpoint
	err := a.st().Update(func(db *DB) error {
		var e error
		saved, e = AddCheckpoint(db, sym, cp)
		return e
	})
	if err != nil {
		redirect(w, r, "/private", "", err.Error())
		return
	}
	redirect(w, r, "/private", fmt.Sprintf("Recorded %s: $%s per share", saved.Date, commas(saved.Price, 4)), "")
}

func (a *App) privateCheckpointDelete(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	_ = a.st().Update(func(db *DB) error {
		DeleteCheckpoint(db, r.FormValue("symbol"), r.FormValue("id"))
		return nil
	})
	redirect(w, r, "/private", "Checkpoint removed", "")
}

func (a *App) privateSearch(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sym := r.FormValue("symbol")
	var term string
	a.st().View(func(db *DB) {
		if i := db.Instruments[sym]; i != nil && i.Private {
			term = i.SearchTerm
			if term == "" {
				term = i.Name
			}
		}
	})
	if t := strings.TrimSpace(r.FormValue("term")); t != "" {
		term = t
	}
	if a.inTest() {
		a.privatePage(w, r, sym, nil, "Test mode doesn't search the web")
		return
	}
	cands, err := SearchValuations(term)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.privatePage(w, r, sym, cands, msg)
}
