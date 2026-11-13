package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Test mode swaps the whole app onto a separate, persistent dataset of simulated
// investments (VGS, VDGR, VDHG and the VAN0110AU managed fund). The real data file is
// never opened for writing while test mode is on, and the test data is stored in its
// own file, so you can switch back and forth like flipping between two accounts.
//
//	TEST_MODE=1 go run .     start in test mode (TEST_MODE=0 forces real data)
//	the switch in the page header changes it at runtime; the choice is remembered.

type modes struct {
	mu       sync.RWMutex
	real     *Store
	test     *Store
	testOn   bool
	dir      string
	testPath string
	modeFile string
}

func (a *App) initModes(dataFile string) error {
	a.dir = filepath.Dir(dataFile)
	a.testPath = filepath.Join(a.dir, "fundtrack-test.json")
	a.modeFile = filepath.Join(a.dir, "mode")
	real, err := OpenStore(dataFile)
	if err != nil {
		return err
	}
	a.real = real
	on := false
	if b, err := os.ReadFile(a.modeFile); err == nil {
		on = strings.TrimSpace(string(b)) == "test"
	}
	switch os.Getenv("TEST_MODE") {
	case "1", "true", "on":
		on = true
	case "0", "false", "off":
		on = false
	}
	if on {
		return a.setMode(true)
	}
	return nil
}

func (a *App) st() *Store {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.testOn && a.test != nil {
		return a.test
	}
	return a.real
}

func (a *App) inTest() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.testOn
}

// setMode switches stores, creating and seeding the test dataset the first time.
func (a *App) setMode(test bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if test && a.test == nil {
		s, err := OpenStore(a.testPath)
		if err != nil {
			return err
		}
		if len(s.db.Txns) == 0 {
			if err := SeedTestData(s, time.Now()); err != nil {
				return err
			}
		}
		a.test = s
	}
	a.testOn = test
	v := "real"
	if test {
		v = "test"
	}
	_ = os.MkdirAll(a.dir, 0o755)
	_ = os.WriteFile(a.modeFile, []byte(v), 0o644)
	return nil
}

func (a *App) registerTestMode(mux *http.ServeMux) {
	mux.HandleFunc("POST /mode", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		test := r.FormValue("mode") == "test"
		if err := a.setMode(test); err != nil {
			redirect(w, r, "/", "", err.Error())
			return
		}
		msg := "Switched to your real data"
		if test {
			msg = "Test mode on — you're now looking at simulated data. Your real records are untouched."
		}
		redirect(w, r, "/", msg, "")
	})
	mux.HandleFunc("POST /test/reset", func(w http.ResponseWriter, r *http.Request) {
		if !a.inTest() {
			redirect(w, r, "/", "", "test data can only be reset in test mode")
			return
		}
		if err := SeedTestData(a.st(), time.Now()); err != nil {
			redirect(w, r, "/", "", err.Error())
			return
		}
		redirect(w, r, "/", "Test data regenerated", "")
	})
	mux.HandleFunc("GET /test/sample.csv", a.testSampleCSV)
}

// testSampleCSV builds CSV files from the test data for trying the importer: mostly rows
// that are already recorded (to see deduplication), plus a new trade, a near-duplicate with a
// settlement-style date, and a deposit.
func (a *App) testSampleCSV(w http.ResponseWriter, r *http.Request) {
	if !a.inTest() {
		http.Error(w, "sample files are only available in test mode", http.StatusNotFound)
		return
	}
	kind := r.URL.Query().Get("kind")
	var txns []Txn
	a.st().View(func(db *DB) { txns = append(txns, db.Txns...) })
	sort.SliceStable(txns, func(i, j int) bool { return txns[i].Date < txns[j].Date })
	dmy := func(d string) string { t, _ := time.Parse(dateFmt, d); return t.Format("02/01/2006") }

	var buf strings.Builder
	cw := csv.NewWriter(&buf)
	name := "sample-broker.csv"
	switch kind {
	case "statement":
		// a one-fund statement with no ticker column: enter VAN0110AU as "symbol for every row"
		name = "sample-vanguard-statement.csv"
		_ = cw.Write([]string{"Vanguard Investments Australia"})
		_ = cw.Write([]string{"Account statement"})
		_ = cw.Write([]string{})
		_ = cw.Write([]string{"Date", "Description", "Units", "Unit price", "Amount"})
		n := 0
		for i := len(txns) - 1; i >= 0 && n < 6; i-- {
			t := txns[i]
			if t.Symbol != "VAN0110AU" {
				continue
			}
			desc := map[string]string{"buy": "Application", "sell": "Redemption", "dividend": "Distribution", "drp": "Distribution Reinvestment"}[t.Type]
			amt := t.Amount
			if t.Type == "buy" || t.Type == "sell" {
				amt = t.Units * t.Price
			}
			_ = cw.Write([]string{dmy(t.Date), desc, ifPos(t.Units), ifPos(t.Price), fmt.Sprintf("%.2f", amt)})
			n++
		}
		_ = cw.Write([]string{dmy(time.Now().Format(dateFmt)), "Application", "250.0000", "1.6400", "410.00"}) // new
		_ = cw.Write([]string{"Total", "", "", "", "—"})
	default:
		_ = cw.Write([]string{"Code", "Company", "Date", "Type", "Quantity", "Unit Price ($)", "Trade Value ($)", "Brokerage+GST ($)", "GST ($)", "Contract Note", "Total Value ($)"})
		var qual []Txn // listed buys and sells, newest first
		for i := len(txns) - 1; i >= 0; i-- {
			if t := txns[i]; (t.Type == "buy" || t.Type == "sell") && t.Symbol != "VAN0110AU" {
				qual = append(qual, t)
			}
		}
		var last Txn
		for n, t := range qual {
			if n >= 8 {
				last = t // the next-oldest transaction is NOT in the file: used for the near-duplicate row below
				break
			}
			typ := map[string]string{"buy": "Buy", "sell": "Sell"}[t.Type]
			gross := t.Units * t.Price
			total := gross + t.Fees
			if t.Type == "sell" {
				total = gross - t.Fees
			}
			_ = cw.Write([]string{strings.TrimSuffix(t.Symbol, ".AX"), "VANGUARD", dmy(t.Date), typ, trimFloat(t.Units),
				fmt.Sprintf("%.2f", t.Price), fmt.Sprintf("%.2f", gross), fmt.Sprintf("%.2f", t.Fees), "0.86", t.Ref, fmt.Sprintf("%.2f", total)})
		}
		// a near-duplicate of an older transaction that is not in this file: settlement-style date two days later, price rounded differently
		if last.Symbol != "" {
			d, _ := time.Parse(dateFmt, last.Date)
			_ = cw.Write([]string{strings.TrimSuffix(last.Symbol, ".AX"), "VANGUARD", d.AddDate(0, 0, 2).Format("02/01/2006"), map[string]string{"buy": "Buy", "sell": "Sell"}[last.Type],
				trimFloat(last.Units), fmt.Sprintf("%.2f", last.Price+0.01), "", "9.50", "0.86", "", ""})
		}
		// a brand-new trade, and a non-investment row
		_ = cw.Write([]string{"VGS", "VANGUARD MSCI INDEX INTERNATIONAL SHARES ETF", dmy(time.Now().AddDate(0, 0, -1).Format(dateFmt)), "Buy", "5", "165.00", "825.00", "9.50", "0.86", "99900001", "834.50"})
		_ = cw.Write([]string{"", "Cash deposit", dmy(time.Now().AddDate(0, 0, -3).Format(dateFmt)), "Deposit", "", "", "", "", "", "", "5000.00"})
		_ = cw.Write([]string{"Total", "", "", "", "", "", "", "", "", "", ""})
	}
	cw.Flush()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write([]byte(buf.String()))
}

func ifPos(v float64) string {
	if v <= 0 {
		return ""
	}
	return trimFloat(v)
}
