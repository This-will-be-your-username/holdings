package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

func (a *App) registerPrices(mux *http.ServeMux) {
	mux.HandleFunc("GET /prices", a.pricesPage)
	mux.HandleFunc("POST /prices/fetch", a.fetchFundPrices)
	mux.HandleFunc("POST /prices/import", a.importFundPrices)
	mux.HandleFunc("POST /prices/port", a.saveFundPort)
	mux.HandleFunc("GET /api/live", a.liveAPI)
}

type fundRow struct {
	Symbol, Name, Port string
	CanFetch           bool // an APIR code or a saved Vanguard port ID
	Latest             float64
	LatestDate         string
	Total              int // distinct dates saved (fetched and imported prices overlap)
	Saved, Manual      int
	FetchedAt          string
	ManualPrices       []ChartPt
}

func clock(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).Local().Format("15:04:05")
}

func (a *App) pricesPage(w http.ResponseWriter, r *http.Request) {
	var funds []fundRow
	a.st().View(func(db *DB) {
		for _, inst := range db.Instruments {
			if inst.Market != MarketManaged || inst.Private {
				continue
			}
			f := fundRow{Symbol: inst.Symbol, Name: inst.Name, Port: inst.VanguardPort, CanFetch: inst.VanguardPort != "" || isAPIR(inst.Symbol)}
			merged := map[string]float64{}
			for d, v := range db.Prices[inst.Symbol] {
				merged[d] = v
			}
			f.Saved = len(merged)
			for d, v := range db.Manual[inst.Symbol] {
				merged[d] = v
				f.ManualPrices = append(f.ManualPrices, ChartPt{Date: d, Value: v})
			}
			f.Manual = len(db.Manual[inst.Symbol])
			f.Total = len(merged)
			sort.Slice(f.ManualPrices, func(i, j int) bool { return f.ManualPrices[i].Date > f.ManualPrices[j].Date })
			if _, last := storedRange(merged); last != "" {
				f.LatestDate, f.Latest = last, merged[last]
			}
			if at, err := time.Parse(time.RFC3339, db.PriceAt["vg:"+inst.Symbol]); err == nil {
				f.FetchedAt = at.Local().Format("2 Jan 15:04")
			}
			funds = append(funds, f)
		}
	})
	sort.Slice(funds, func(i, j int) bool { return funds[i].Symbol < funds[j].Symbol })
	a.render(w, r, "prices", map[string]any{"Funds": funds, "Today": time.Now().Format(dateFmt), "TestMode": a.inTest()})
}

// fetchFundPrices pulls a fund's unit prices from Vanguard now.
func (a *App) fetchFundPrices(w http.ResponseWriter, r *http.Request) {
	sym := r.FormValue("symbol")
	if a.inTest() {
		redirect(w, r, "/prices", "", "Test mode uses simulated prices; nothing was fetched from Vanguard")
		return
	}
	var inst Instrument
	var found bool
	a.st().View(func(db *DB) {
		if i := db.Instruments[sym]; i != nil && i.Market == MarketManaged {
			inst, found = *i, true
		}
	})
	if !found {
		redirect(w, r, "/prices", "", fmt.Sprintf("%q isn't an unlisted fund in your records", sym))
		return
	}
	f, err := vgResolve(&inst)
	if err != nil {
		redirect(w, r, "/prices", "", err.Error())
		return
	}
	nav, err := fetchVGPrices(f.PortID)
	if err != nil {
		redirect(w, r, "/prices", "", err.Error())
		return
	}
	first, last := storedRange(nav)
	_ = a.st().Update(func(db *DB) error {
		if db.Prices[sym] == nil {
			db.Prices[sym] = map[string]float64{}
		}
		for d, v := range nav {
			db.Prices[sym][d] = v
		}
		if i := db.Instruments[sym]; i != nil {
			i.VanguardPort = f.PortID
			if (i.Name == "" || i.Name == i.Symbol) && f.Name != "" {
				i.Name = f.Name
			}
		}
		db.PriceAt["vg:"+sym] = time.Now().Format(time.RFC3339)
		return nil
	})
	redirect(w, r, "/prices", fmt.Sprintf("Fetched %d unit prices for %s from Vanguard (%s to %s). Latest NAV $%.4f.", len(nav), sym, first, last, nav[last]), "")
}

// importFundPrices reads a price history file (Vanguard's .xlsx or CSV export) into the fund's saved prices.
func (a *App) importFundPrices(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil && err != http.ErrNotMultipart {
		redirect(w, r, "/prices", "", "upload too large or unreadable (limit 16 MB)")
		return
	}
	sym := r.FormValue("symbol")
	var data []byte
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, _ = io.ReadAll(f)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte(r.FormValue("paste"))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		redirect(w, r, "/prices", "", "choose a price history file (.xlsx or .csv) to import")
		return
	}
	pi, err := ParsePriceFile(data)
	if err != nil {
		redirect(w, r, "/prices", "", "import failed: "+err.Error())
		return
	}
	err = a.st().Update(func(db *DB) error {
		inst := db.Instruments[sym]
		if inst == nil || inst.Market != MarketManaged {
			return fmt.Errorf("%q isn't an unlisted fund in your records", sym)
		}
		if db.Manual[sym] == nil {
			db.Manual[sym] = map[string]float64{}
		}
		for d, v := range pi.Prices {
			db.Manual[sym][d] = v
		}
		return nil
	})
	if err != nil {
		redirect(w, r, "/prices", "", err.Error())
		return
	}
	msg := fmt.Sprintf("Imported %d prices for %s (%s to %s) from the %s column", pi.Rows, sym, pi.From, pi.To, pi.Column)
	if pi.Skipped > 0 {
		msg += fmt.Sprintf("; %d unreadable rows skipped", pi.Skipped)
	}
	redirect(w, r, "/prices", msg, "")
}

func (a *App) saveFundPort(w http.ResponseWriter, r *http.Request) {
	sym := r.FormValue("symbol")
	port := strings.TrimSpace(r.FormValue("port_id"))
	if port != "" && !regexp.MustCompile(`^\d{3,6}$`).MatchString(port) {
		redirect(w, r, "/prices", "", "a Vanguard port ID is a number such as 8121 (it's in the fund page's web address after portId=)")
		return
	}
	err := a.st().Update(func(db *DB) error {
		inst := db.Instruments[sym]
		if inst == nil || inst.Market != MarketManaged {
			return fmt.Errorf("%q isn't an unlisted fund in your records", sym)
		}
		inst.VanguardPort = port
		delete(db.PriceAt, "vg:"+sym) // fetch on the next refresh
		return nil
	})
	if err != nil {
		redirect(w, r, "/prices", "", err.Error())
		return
	}
	redirect(w, r, "/prices", "Vanguard link saved for "+sym, "")
}

// ---------- dashboard live status ----------

type liveView struct {
	State, Text, Detail, Err, Note string // State: open | closed | stale | test
	Open                           bool
	RefreshErr                     string
}

func buildLiveView(db *DB, test bool, res *Result, now time.Time) liveView {
	lv := liveView{}
	latest := ""
	for _, h := range res.Holdings {
		if h.PriceDate > latest {
			latest = h.PriceDate
		}
	}
	if t, err := time.Parse(time.RFC3339, db.LastRefresh); err == nil {
		lv.Detail = "History saved " + t.Local().Format("2 Jan 15:04")
	} else {
		lv.Detail = "History not saved yet"
	}
	lv.RefreshErr = db.RefreshErr
	if test {
		lv.State, lv.Text = "test", "Test data · prices are simulated"
		return lv
	}
	open := openMarkets(db, now)
	st := db.Live
	switch {
	case len(open) > 0:
		lv.Open = true
		fresh := st.UpdatedAt > 0 && now.Unix()-st.UpdatedAt < 150
		if !fresh && st.Err != "" {
			lv.State, lv.Err = "stale", st.Err
			lv.Text = strings.Join(open, " & ") + " open · live prices unavailable, showing the last saved prices"
		} else {
			lv.State = "open"
			lv.Text = "Live · " + strings.Join(open, " & ") + " open"
			if st.UpdatedAt > 0 {
				lv.Text += " · updated " + clock(st.UpdatedAt)
			}
			lv.Err = st.Err
		}
	default:
		lv.State = "closed"
		if latest != "" {
			if d, err := time.Parse(dateFmt, latest); err == nil {
				latest = d.Format("Mon 2 Jan")
			}
			lv.Text = "Markets closed · prices as at " + latest
		} else {
			lv.Text = "Markets closed"
		}
	}
	lv.Note = st.Note
	return lv
}
