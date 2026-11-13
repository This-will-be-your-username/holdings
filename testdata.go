package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"
)

// SeedTestData fills a store with a realistic, self-consistent set of simulated
// investments built around VGS, VDGR, VDHG and the unlisted VAN0110AU fund:
//
//   - ~3 years of lump-sum and monthly/quarterly buys with brokerage and contract-note references
//   - quarterly distributions with franking credits, foreign-tax offsets, discountable capital gain
//     and tax-deferred components, some reinvested (DRP), June-quarter payments assigned to the
//     earlier income year
//   - sells that exercise CGT: a discounted gain, a full-rate gain on a young parcel and a loss
//   - ~5 years of daily prices (market-wide factors, so betas and correlations make sense), market
//     benchmarks, weekly unit prices for the managed fund, and one distribution left unrecorded so
//     the "may be missing" suggestions appear
//
// Everything is derived from a fixed random seed, so the shape of the data is the same each time;
// only the dates move forward with "now".

const testSeed = 20260101

type testFund struct {
	sym, name, market string
	a, b, idio        float64 // loadings on the global and local market factors, and own noise
	target            float64 // price at the end of the series
	yield             float64 // approx. quarterly distribution as a share of price (before seasonality)
	franked, ft       float64 // share of a distribution that is franked / foreign-tax offset
	cgd, td           float64 // share that is discountable capital gain / tax-deferred
	drp               bool
	brokerage         float64
}

var testFunds = []testFund{
	{"VGS.AX", "Vanguard MSCI Index International Shares ETF", MarketASX, 1.00, 0.00, 0.0025, 165.00, 0.0042, 0, 0.06, 0, 0.0, false, 9.50},
	{"VDHG.AX", "Vanguard Diversified High Growth Index ETF", MarketASX, 0.55, 0.25, 0.0015, 68.00, 0.0062, 0.12, 0.03, 0.08, 0.04, true, 9.50},
	{"VDGR.AX", "Vanguard Diversified Growth Index ETF", MarketASX, 0.42, 0.20, 0.0015, 78.00, 0.0066, 0.20, 0.02, 0.06, 0.03, false, 9.50},
	{"VAN0110AU", "Vanguard Managed Fund (APIR VAN0110AU)", MarketManaged, 0.40, 0.20, 0.0010, 1.62, 0.0070, 0.25, 0.03, 0.05, 0.0, false, 0},
}

func rd(v float64, dp int) float64 { p := math.Pow(10, float64(dp)); return math.Round(v*p) / p }

// business days ending at end, ascending
func bizDaysEnding(end time.Time, n int) []string {
	var out []string
	for d := end; len(out) < n; d = d.AddDate(0, 0, -1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, d.Format(dateFmt))
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// path builds a price series from factor returns and rescales it to end at target.
func path(rets []float64, target float64) []float64 {
	p := make([]float64, len(rets))
	cum := 0.0
	for i, r := range rets {
		cum += r
		p[i] = math.Exp(cum)
	}
	k := target / p[len(p)-1]
	for i := range p {
		p[i] *= k
	}
	return p
}

type testEv struct {
	idx  int
	kind string // buy | sell | dist
	f    *testFund
	amt  float64 // buy: AUD to invest; sell: units
	q    int     // distribution month
}

func SeedTestData(s *Store, now time.Time) error {
	const nDays = 1320
	end := now.AddDate(0, 0, -1)
	dates := bizDaysEnding(end, nDays)
	n := len(dates)
	rng := rand.New(rand.NewSource(testSeed))

	// --- market factors, with a drawdown and rebound partway through ---
	dip0, dipLen := int(0.55*float64(n)), 25
	fg, fa := make([]float64, n), make([]float64, n)
	for i := 0; i < n; i++ {
		vol := 0.0075 * (1 + 0.45*math.Sin(float64(i)/85))
		g := 0.00045 + vol*rng.NormFloat64()
		switch {
		case i >= dip0 && i < dip0+dipLen:
			g -= 0.005
		case i >= dip0+dipLen && i < dip0+dipLen+30:
			g += 0.0025
		}
		fg[i] = g
		fa[i] = 0.55*g + 0.006*rng.NormFloat64() + 0.0002
	}
	series := map[string][]float64{}
	for _, f := range testFunds {
		r := make([]float64, n)
		for i := range r {
			r[i] = f.a*fg[i] + f.b*fa[i] + f.idio*rng.NormFloat64()
		}
		series[f.sym] = path(r, f.target)
	}
	axjo := path(fa, 9080)
	gspc := path(fg, 7590)
	vix, fx := make([]float64, n), make([]float64, n)
	v, x := 17.0, 0.66
	for i := 0; i < n; i++ {
		v += 0.12*(18-v) - 120*fg[i] + 0.5*rng.NormFloat64()
		v = math.Max(10, math.Min(60, v))
		vix[i] = v
		x += 0.0004 * rng.NormFloat64()
		fx[i] = x
	}
	fxScale := 0.68 / fx[n-1]
	for i := range fx {
		fx[i] *= fxScale
	}

	price := func(sym string, i int) float64 {
		dp := 2
		if sym == "VAN0110AU" {
			dp = 4
		}
		return rd(series[sym][i], dp)
	}
	fund := map[string]*testFund{}
	for i := range testFunds {
		fund[testFunds[i].sym] = &testFunds[i]
	}
	vgs, vdhg, vdgr, van := fund["VGS.AX"], fund["VDHG.AX"], fund["VDGR.AX"], fund["VAN0110AU"]

	// --- plan the events ---
	s0 := n - 780 // about three years ago
	last := n - 15
	var evs []testEv
	add := func(i int, kind string, f *testFund, amt float64) {
		if i > 0 && i < last {
			evs = append(evs, testEv{idx: i, kind: kind, f: f, amt: amt})
		}
	}
	add(s0, "buy", vgs, 8000)
	for i := s0 + 21; i < last; i += 21 {
		add(i, "buy", vgs, 500)
	}
	add(s0+63, "buy", vdhg, 12000)
	for k, i := 0, s0+126; i < last; k, i = k+1, i+63 {
		add(i, "buy", vdhg, 2000)
	}
	add(s0+126, "buy", vdgr, 6000)
	add(dip0-5, "buy", vdgr, 5000) // bought just before the drawdown
	for i := s0; i < last; i += 21 {
		add(i, "buy", van, 400)
	}
	add(s0+360, "sell", vgs, 40)            // old parcel: discounted gain
	add(dip0+22, "sell", vdgr, 30)          // sold at the bottom of the drawdown: a capital loss
	add(s0+126+63*5+120, "sell", vdhg, -25) // negative: all parcels over a year old, plus 25 units of a younger one
	add(s0+500, "sell", van, 600)

	// distribution pay dates: the 12th of Jan/Apr/Jul/Oct (next business day)
	idxOnOrAfter := func(d time.Time) int {
		ds := d.Format(dateFmt)
		return sort.SearchStrings(dates, ds)
	}
	var payIdx []int
	var payMonth []int
	for y := now.Year() - 4; y <= now.Year(); y++ {
		for _, m := range []int{1, 4, 7, 10} {
			i := idxOnOrAfter(time.Date(y, time.Month(m), 12, 0, 0, 0, 0, time.UTC))
			if i > s0+30 && i < n {
				payIdx = append(payIdx, i)
				payMonth = append(payMonth, m)
			}
		}
	}
	unrecorded := len(payIdx) - 1 // the latest payment is left for the "may be missing" suggestions
	for k, i := range payIdx {
		for j := range testFunds {
			if k == unrecorded {
				continue
			}
			evs = append(evs, testEv{idx: i, kind: "dist", f: &testFunds[j], q: payMonth[k]})
		}
	}
	order := map[string]int{"buy": 0, "dist": 1, "sell": 2}
	sort.SliceStable(evs, func(i, j int) bool {
		if evs[i].idx != evs[j].idx {
			return evs[i].idx < evs[j].idx
		}
		return order[evs[i].kind] < order[evs[j].kind]
	})

	// --- turn events into transactions ---
	units := map[string]float64{}
	type lot struct {
		date  string
		units float64
	}
	lots := map[string][]lot{} // FIFO parcels, mirroring how the tax engine will match sales
	var txns []Txn
	var imported []int // indexes (into txns) that came "from" the sample broker file
	ref := 0
	season := map[int]float64{1: 1.0, 4: 0.55, 7: 1.7, 10: 0.75}
	for _, e := range evs {
		d := dates[e.idx]
		p := price(e.f.sym, e.idx)
		switch e.kind {
		case "buy":
			var u float64
			if e.f.market == MarketManaged {
				u = rd(e.amt/p, 4)
			} else {
				u = math.Floor(e.amt / p)
			}
			if u <= 0 {
				continue
			}
			t := Txn{Date: d, Symbol: e.f.sym, Type: "buy", Units: u, Price: p, Fees: e.f.brokerage}
			if e.f.market == MarketASX && e.idx < s0+330 { // the first year came with contract notes
				ref++
				t.Ref = fmt.Sprintf("CN%08d", 41000000+ref)
				imported = append(imported, len(txns))
			}
			units[e.f.sym] += u
			lots[e.f.sym] = append(lots[e.f.sym], lot{d, u})
			txns = append(txns, t)
		case "sell":
			want := e.amt
			if e.amt < 0 { // "everything held over a year, plus -amt units"
				cut := time.Time{}
				if dt, err := time.Parse(dateFmt, d); err == nil {
					cut = dt.AddDate(-1, 0, -1)
				}
				want = -e.amt
				for _, l := range lots[e.f.sym] {
					if ld, _ := time.Parse(dateFmt, l.date); ld.Before(cut) {
						want += l.units
					}
				}
			}
			u := math.Min(want, units[e.f.sym])
			if u <= 0 {
				continue
			}
			units[e.f.sym] -= u
			for rem, i := u, 0; rem > 1e-9 && i < len(lots[e.f.sym]); i++ {
				use := math.Min(rem, lots[e.f.sym][i].units)
				lots[e.f.sym][i].units -= use
				rem -= use
			}
			txns = append(txns, Txn{Date: d, Symbol: e.f.sym, Type: "sell", Units: u, Price: p, Fees: e.f.brokerage,
				Notes: map[string]string{"VGS.AX": "Trim - parcel held over a year", "VDGR.AX": "Sold into the drawdown", "VDHG.AX": "Short-term sale", "VAN0110AU": "Redemption"}[e.f.sym]})
		case "dist":
			held := units[e.f.sym]
			if held <= 0 {
				continue
			}
			amt := rd(held*p*e.f.yield*season[e.q], 2)
			if amt < 1 {
				continue
			}
			t := Txn{Date: d, Symbol: e.f.sym, Type: "dividend", Amount: amt}
			if e.f.drp {
				u := math.Floor(amt / p)
				if u < 1 {
					continue
				}
				amt = rd(u*p, 2)
				t = Txn{Date: d, Symbol: e.f.sym, Type: "drp", Units: u, Price: p, Amount: amt}
				units[e.f.sym] += u
				lots[e.f.sym] = append(lots[e.f.sym], lot{d, u})
			}
			t.Franked = rd(amt*e.f.franked, 2)
			t.Franking = rd(t.Franked*30/70, 2)
			t.ForeignTax = rd(amt*e.f.ft, 2)
			t.TaxDeferred = rd(amt*e.f.td, 2)
			if e.q == 7 { // the June-quarter payment lands in July but belongs to the earlier income year
				t.CGDisc = rd(amt*e.f.cgd, 2)
				dt, _ := time.Parse(dateFmt, d)
				t.TaxYear = fyOf(dt.AddDate(0, -1, 0).Format(dateFmt))
			}
			txns = append(txns, t)
		}
	}

	// --- store ---
	return s.Update(func(db *DB) error {
		*db = DB{}
		db.init()
		db.Settings = Settings{OtherIncome: 110000, CGTMethod: "fifo"}
		impID := "sample-import"
		for i := range txns {
			txns[i].ID = newID()
			txns[i].FP = Fingerprint(txns[i])
		}
		for _, i := range imported {
			txns[i].Import = impID
		}
		db.Txns = txns
		db.Imports = []ImportRec{{ID: impID, At: now.AddDate(0, 0, -30).Format(time.RFC3339), File: "sample-commsec-first-year.csv",
			Hash: "test-data", Rows: len(imported), Added: len(imported)}}

		for _, f := range testFunds {
			db.Instruments[f.sym] = &Instrument{Symbol: f.sym, Name: f.name, Currency: "AUD", Market: f.market}
		}
		for _, f := range testFunds {
			if f.market == MarketManaged {
				m := map[string]float64{}
				for i := 0; i < n; i += 5 {
					m[dates[i]] = price(f.sym, i)
				}
				for _, t := range txns { // a unit price on every transaction date
					if t.Symbol == f.sym && t.Price > 0 {
						m[t.Date] = t.Price
					}
				}
				m[dates[n-1]] = price(f.sym, n-1)
				db.Manual[f.sym] = m
				continue
			}
			m := make(map[string]float64, n)
			for i := range dates {
				m[dates[i]] = price(f.sym, i)
			}
			db.Prices[f.sym] = m
			// ex-dividend dates and the latest (unrecorded) distribution, as Yahoo / the ASX would report them
			db.Divs[f.sym] = map[string]float64{}
			for k, i := range payIdx {
				exI := i - 8
				if exI < 0 {
					continue
				}
				pu := rd(price(f.sym, i)*f.yield*season[payMonth[k]], 4)
				db.Divs[f.sym][dates[exI]] = pu
				if k == unrecorded {
					db.DivInfo[f.sym] = map[string]DivInfo{dates[exI]: {Pay: dates[i], PerUnit: pu, Franking: f.franked * 100}}
				}
			}
		}
		for sym, ser := range map[string][]float64{"^AXJO": axjo, "^GSPC": gspc, "^VIX": vix, "AUDUSD=X": fx} {
			m := make(map[string]float64, n)
			for i := range dates {
				m[dates[i]] = rd(ser[i], 4)
			}
			db.Bench[sym] = m
		}
		db.LastRefresh = now.Format(time.RFC3339)
		return nil
	})
}
