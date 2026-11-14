package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Uploads are held in memory for an hour between "upload" and "import" so the user can
// review the detected columns and duplicates and adjust them. Nothing is stored until
// they confirm.

type importSession struct {
	ID, File string
	Data     []byte
	Hash     string
	At       time.Time
}

var sessions = struct {
	sync.Mutex
	m map[string]*importSession
}{m: map[string]*importSession{}}

func newSession(file string, data []byte) *importSession {
	sessions.Lock()
	defer sessions.Unlock()
	for id, s := range sessions.m {
		if time.Since(s.At) > time.Hour {
			delete(sessions.m, id)
		}
	}
	if len(sessions.m) >= 25 { // drop the oldest
		var oldest string
		for id, s := range sessions.m {
			if oldest == "" || s.At.Before(sessions.m[oldest].At) {
				oldest = id
			}
		}
		delete(sessions.m, oldest)
	}
	sum := sha256.Sum256(data)
	s := &importSession{ID: newID() + newID(), File: file, Data: data, Hash: hex.EncodeToString(sum[:]), At: time.Now()}
	sessions.m[s.ID] = s
	return s
}

func getSession(id string) *importSession {
	sessions.Lock()
	defer sessions.Unlock()
	return sessions.m[id]
}

func (a *App) registerImport(mux *http.ServeMux) {
	mux.HandleFunc("POST /import", a.importStart)
	mux.HandleFunc("GET /import/review", a.importReview)
	mux.HandleFunc("POST /import/review", a.importReview)
	mux.HandleFunc("POST /import/commit", a.importCommit)
	mux.HandleFunc("POST /import/undo", a.importUndo)
}

func (a *App) importStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil && err != http.ErrNotMultipart {
		redirect(w, r, "/transactions", "", "upload too large or unreadable (limit 8 MB)")
		return
	}
	var data []byte
	name := "pasted.csv"
	if f, h, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, _ = io.ReadAll(f)
		name = h.Filename
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte(r.FormValue("paste"))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		redirect(w, r, "/transactions", "", "choose a CSV file (or paste CSV text) to import")
		return
	}
	if isXLSX(data) { // a spreadsheet export: read its first sheet as CSV
		csvData, err := xlsxToCSV(data)
		if err != nil {
			redirect(w, r, "/transactions", "", err.Error())
			return
		}
		data = csvData
	}
	s := newSession(name, data)
	http.Redirect(w, r, "/import/review?t="+url.QueryEscape(s.ID), http.StatusSeeOther)
}

func importOptsFrom(r *http.Request) ImportOpts {
	o := ImportOpts{HeaderRow: -1, DateOrder: "auto", DefMarket: "auto"}
	if v := strings.TrimSpace(r.FormValue("header_row")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			o.HeaderRow = n - 1
		}
	}
	if v := r.FormValue("date_order"); v == "DMY" || v == "MDY" || v == "YMD" {
		o.DateOrder = v
	}
	if v := r.FormValue("def_market"); v == MarketASX || v == MarketUS || v == MarketManaged {
		o.DefMarket = v
	}
	o.DefSymbol = strings.TrimSpace(r.FormValue("def_symbol"))
	if r.Method == http.MethodPost { // the form carries the (possibly corrected) column choices
		o.Map = map[Field]int{}
		for _, f := range fieldOrder {
			if v := r.FormValue("map_" + string(f)); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					o.Map[f] = n
				}
			}
		}
	}
	return o
}

type importSummary struct{ New, Dup, Possible, Skipped, Errors int }

// classify runs the duplicate check over the importable rows.
func classifyRows(an *Analysis, existing []Txn) importSummary {
	var idx []int
	var txns []Txn
	for i := range an.Rows {
		if an.Rows[i].Status == "ok" {
			idx = append(idx, i)
			txns = append(txns, an.Rows[i].Txn)
		}
	}
	matches := ClassifyDuplicates(existing, txns)
	for k, i := range idx {
		m := matches[k]
		an.Rows[i].Class, an.Rows[i].Why = m.Class, m.Why
		if m.Existing >= 0 {
			ex := existing[m.Existing]
			an.Rows[i].Existing = &ex
		}
	}
	var s importSummary
	for _, p := range an.Rows {
		switch {
		case p.Status == "skip":
			s.Skipped++
		case p.Status == "error":
			s.Errors++
		case p.Class == "dup":
			s.Dup++
		case p.Class == "possible":
			s.Possible++
		case p.Class == "new":
			s.New++
		}
	}
	return s
}

type colOpt struct {
	Idx   int
	Label string
}

type fieldRow struct {
	Key       string
	Label     string
	Sel, Conf int
	Essential bool
}

func (a *App) importReview(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sess := getSession(r.FormValue("t"))
	if sess == nil {
		redirect(w, r, "/transactions", "", "that upload has expired - please upload the file again")
		return
	}
	a.renderReview(w, r, sess, importOptsFrom(r), "")
}

func (a *App) renderReview(w http.ResponseWriter, r *http.Request, sess *importSession, o ImportOpts, errMsg string) {
	an, err := Analyze(sess.Data, o)
	if err != nil {
		redirect(w, r, "/transactions", "", err.Error())
		return
	}
	var sum importSummary
	var prev *ImportRec
	a.st().View(func(db *DB) {
		sum = classifyRows(an, db.Txns)
		for i := range db.Imports {
			if db.Imports[i].Hash == sess.Hash {
				p := db.Imports[i]
				prev = &p
			}
		}
	})
	cols := make([]colOpt, len(an.Headers))
	for i, h := range an.Headers {
		lbl := strings.TrimSpace(h)
		if lbl == "" {
			lbl = "(blank)"
		}
		cols[i] = colOpt{i, fmt.Sprintf("%s: %s", colLetter(i), lbl)}
	}
	var fields []fieldRow
	for _, f := range fieldOrder {
		sel := -1
		if c, ok := an.Map[f]; ok {
			sel = c
		}
		fields = append(fields, fieldRow{string(f), fieldLabel[f], sel, an.Conf[f], essentialFields[f]})
	}
	missing := []string{}
	if _, ok := an.Map[FDate]; !ok {
		missing = append(missing, "a date column")
	}
	_, hasSym := an.Map[FSymbol]
	_, hasName := an.Map[FName]
	_, hasDesc := an.Map[FDesc]
	if !hasSym && !hasName && !hasDesc && an.DefSymbol == "" {
		missing = append(missing, "a ticker/code column (or enter a symbol for the whole file below)")
	}
	sort.SliceStable(an.Rows, func(i, j int) bool { return an.Rows[i].Line < an.Rows[j].Line })
	data := map[string]any{
		"T": sess.ID, "File": sess.File, "An": an, "Sum": sum, "Cols": cols, "Fields": fields, "Prev": prev,
		"HeaderRow": an.HeaderRow + 1, "DateOrder": an.DateOrder, "DefMarket": o.DefMarket, "DefSymbol": an.DefSymbol,
		"Missing": missing, "Importable": sum.New + sum.Possible, "DateOrderPicked": o.DateOrder,
	}
	if errMsg != "" {
		data["Err"] = errMsg
	}
	a.render(w, r, "import", data)
}

func colLetter(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func (a *App) importCommit(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	sess := getSession(r.FormValue("t"))
	if sess == nil {
		redirect(w, r, "/transactions", "", "that upload has expired - please upload the file again")
		return
	}
	o := importOptsFrom(r)
	an, err := Analyze(sess.Data, o)
	if err != nil {
		redirect(w, r, "/transactions", "", err.Error())
		return
	}
	force := func(line int) bool { return r.FormValue("inc_"+strconv.Itoa(line)) == "1" }

	// work out what would be imported, and which listed instruments are new (network lookups happen outside the lock)
	need := map[string]*ParsedRow{}
	a.st().View(func(db *DB) {
		classifyRows(an, db.Txns)
		for i := range an.Rows {
			p := &an.Rows[i]
			if p.Status == "ok" && (p.Class == "new" || p.Class == "possible" && force(p.Line)) && db.Instruments[p.Txn.Symbol] == nil {
				if _, seen := need[p.Txn.Symbol]; !seen {
					need[p.Txn.Symbol] = p
				}
			}
		}
	})
	resolved := map[string]*Instrument{}
	for sym, p := range need {
		inst, err := resolveInstrument(sym, p.Market, p.Name)
		if err != nil {
			a.renderReview(w, r, sess, o, fmt.Sprintf("Nothing was imported. %v — fix the ticker, or choose a different default market below.", err))
			return
		}
		resolved[sym] = inst
	}

	var rec ImportRec
	err = a.st().Update(func(db *DB) error {
		sum := classifyRows(an, db.Txns) // re-check under the lock
		rec = ImportRec{ID: newID(), At: time.Now().Format(time.RFC3339), File: sess.File, Hash: sess.Hash,
			Rows: len(an.Rows), Dupes: sum.Dup, Skipped: sum.Skipped + sum.Errors}
		for i := range an.Rows {
			p := &an.Rows[i]
			if p.Status != "ok" {
				continue
			}
			switch {
			case p.Class == "dup" && p.Existing != nil:
				// remember the institution's reference on the transaction we already had
				if p.Txn.Ref != "" {
					for j := range db.Txns {
						if db.Txns[j].ID == p.Existing.ID && db.Txns[j].Ref == "" {
							db.Txns[j].Ref = p.Txn.Ref
						}
					}
				}
			case p.Class == "possible" && !force(p.Line):
				rec.Possible++
			case p.Class == "new" || p.Class == "possible":
				if db.Instruments[p.Txn.Symbol] == nil {
					inst := resolved[p.Txn.Symbol]
					if inst == nil {
						return fmt.Errorf("instrument %s could not be resolved", p.Txn.Symbol)
					}
					db.Instruments[p.Txn.Symbol] = inst
				}
				t := p.Txn
				t.ID, t.Import, t.FP = newID(), rec.ID, Fingerprint(t)
				db.Txns = append(db.Txns, t)
				rec.Added++
			}
		}
		if rec.Added > 0 || rec.Dupes > 0 {
			db.Imports = append(db.Imports, rec)
		}
		return nil
	})
	if err != nil {
		a.renderReview(w, r, sess, o, err.Error())
		return
	}
	if len(resolved) > 0 {
		go a.Refresh()
	}
	msg := fmt.Sprintf("Imported %d transaction%s from %s", rec.Added, plural(rec.Added), sess.File)
	var extra []string
	if rec.Dupes > 0 {
		extra = append(extra, fmt.Sprintf("%d already recorded (skipped)", rec.Dupes))
	}
	if rec.Possible > 0 {
		extra = append(extra, fmt.Sprintf("%d possible duplicate%s held back", rec.Possible, plural(rec.Possible)))
	}
	if rec.Skipped > 0 {
		extra = append(extra, fmt.Sprintf("%d row%s skipped", rec.Skipped, plural(rec.Skipped)))
	}
	if len(extra) > 0 {
		msg += " — " + strings.Join(extra, ", ")
	}
	sessions.Lock()
	delete(sessions.m, sess.ID)
	sessions.Unlock()
	redirect(w, r, "/transactions", msg, "")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (a *App) importUndo(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	removed := 0
	a.st().Update(func(db *DB) error {
		kept := db.Txns[:0]
		for _, t := range db.Txns {
			if t.Import == id {
				removed++
				continue
			}
			kept = append(kept, t)
		}
		db.Txns = kept
		imps := db.Imports[:0]
		for _, im := range db.Imports {
			if im.ID != id {
				imps = append(imps, im)
			}
		}
		db.Imports = imps
		return nil
	})
	redirect(w, r, "/transactions", fmt.Sprintf("Undid import: removed %d transaction%s", removed, plural(removed)), "")
}
