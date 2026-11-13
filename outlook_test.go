package main

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestRegress(t *testing.T) {
	y := make([]float64, 252)
	for i := range y {
		y[i] = 1 + 0.5*float64(i)/252
	}
	a, b, r2, sd := regress(y)
	if !near(a, 1) || math.Abs(b-0.5) > 1e-9 || math.Abs(r2-1) > 1e-9 || sd > 1e-9 {
		t.Errorf("a=%v b=%v r2=%v sd=%v", a, b, r2, sd)
	}
}

func TestTrendLabels(t *testing.T) {
	cases := []struct {
		p, m50, m200 float64
		want         string
	}{
		{110, 105, 100, "Uptrend"},
		{101, 105, 100, "Pullback"},
		{98, 95, 100, "Recovering"},
		{90, 95, 100, "Downtrend"},
		{100, 95, math.NaN(), "Uptrend"},
		{100, math.NaN(), math.NaN(), "Short history"},
	}
	for _, c := range cases {
		if got := trendLabel(c.p, c.m50, c.m200); got != c.want {
			t.Errorf("trendLabel(%v,%v,%v)=%s want %s", c.p, c.m50, c.m200, got, c.want)
		}
	}
}

func gaussRets(n int, sd float64, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	for i := range out {
		out[i] = r.NormFloat64() * sd
	}
	return out
}

func TestSimulateProperties(t *testing.T) {
	rets := [][]float64{gaussRets(500, 0.01, 1)}
	run := func(mu float64, seed int64) simOut {
		return simulate(rets, []float64{mu}, []float64{1}, []float64{10000}, 0, 3000, rand.New(rand.NewSource(seed)))
	}
	so := run(0.10, 7)
	last := so.q[len(so.q)-1]
	if !(last[0] < last[1] && last[1] < last[2] && last[2] < last[3] && last[3] < last[4]) {
		t.Errorf("percentiles not ordered: %v", last)
	}
	want := 10000 * math.Exp(0.10*float64(horizon)/252) // median follows the assumed drift
	if math.Abs(last[2]/want-1) > 0.012 {
		t.Errorf("median %v want about %v", last[2], want)
	}
	if so.q[0][2] != 10000 || so.steps[0] != 0 || so.steps[len(so.steps)-1] != horizon {
		t.Errorf("start/steps wrong: %v %v", so.q[0], so.steps)
	}
	// deterministic for a seed; different for another
	again := run(0.10, 7)
	if again.q[len(again.q)-1] != last {
		t.Error("same seed should reproduce")
	}
	if run(0.10, 8).q[len(so.q)-1] == last {
		t.Error("different seed should differ")
	}
	// higher drift shifts probability of gain up
	if run(-0.2, 7).probUp >= so.probUp {
		t.Error("lower drift should lower the chance of a gain")
	}
}

func TestSimulateZeroVolatility(t *testing.T) {
	flat := make([]float64, 200)
	for i := range flat {
		flat[i] = 0.001 // constant: demeaned to zero, so only drift moves the value
	}
	so := simulate([][]float64{flat}, []float64{0.2}, []float64{1}, []float64{1000}, 500, 200, rand.New(rand.NewSource(1)))
	fin := so.q[len(so.q)-1]
	want := 500 + 1000*math.Exp(0.2*float64(horizon)/252) // the flat 500 never moves
	for _, v := range fin {
		if math.Abs(v-want) > 1e-6 {
			t.Fatalf("got %v want %v", fin, want)
		}
	}
}

func TestMarketCardTiltAndRegime(t *testing.T) {
	up, down := map[string]float64{}, map[string]float64{}
	d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		ds := d.AddDate(0, 0, i).Format(dateFmt)
		up[ds] = 100 * math.Exp(0.001*float64(i))
		down[ds] = 100 * math.Exp(-0.001*float64(i))
	}
	cu, cd := marketCard("^AXJO", newSeries(up)), marketCard("^GSPC", newSeries(down))
	if cu.Trend != "Uptrend" || cu.Tilt != 1.5 || cd.Trend != "Downtrend" || cd.Tilt != -1.5 {
		t.Errorf("up=%+v down=%+v", cu, cd)
	}
	h, _, tone := regimeSummary(map[string]*MarketCard{"^AXJO": cu, "^GSPC": cu})
	if h != "Broad uptrend" || tone != "pos" {
		t.Errorf("headline %s %s", h, tone)
	}
	if h, _, _ := regimeSummary(map[string]*MarketCard{"^AXJO": cu, "^GSPC": cd}); h != "Mixed signals" {
		t.Errorf("headline %s", h)
	}
}

// synthetic price history ending at end: n business days, daily drift/vol
func genPrices(n int, p0, drift, vol float64, seed int64, end time.Time) map[string]float64 {
	r := rand.New(rand.NewSource(seed))
	out := map[string]float64{}
	var days []time.Time
	for d := end; len(days) < n; d = d.AddDate(0, 0, -1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			days = append(days, d)
		}
	}
	p := p0
	for i := len(days) - 1; i >= 0; i-- {
		p *= math.Exp(drift + vol*r.NormFloat64())
		out[days[i].Format(dateFmt)] = p
	}
	return out
}

func outlookDB(now time.Time) *DB {
	db := newTestDB()
	db.Instruments["NEW.AX"] = &Instrument{Symbol: "NEW.AX", Name: "Newly listed", Currency: "AUD", Market: MarketASX}
	db.Instruments["FUND1"] = &Instrument{Symbol: "FUND1", Name: "Managed", Currency: "AUD", Market: MarketManaged}
	db.Prices["VAS.AX"] = genPrices(900, 80, 0.0003, 0.008, 1, now)
	db.Prices["AAPL"] = genPrices(900, 150, 0.0005, 0.015, 2, now)
	db.Prices["NEW.AX"] = genPrices(40, 10, 0, 0.01, 3, now) // too short to model
	db.Manual["FUND1"] = map[string]float64{now.Format(dateFmt): 50}
	db.Bench["^AXJO"] = genPrices(900, 6000, 0.0003, 0.007, 4, now)
	db.Bench["^GSPC"] = genPrices(900, 4000, 0.0004, 0.009, 5, now)
	db.Bench["^VIX"] = genPrices(900, 18, 0, 0.03, 6, now)
	db.Bench["AUDUSD=X"] = genPrices(900, 0.68, 0, 0.004, 7, now)
	start := now.AddDate(-1, 0, 0).Format(dateFmt)
	db.Txns = []Txn{
		{Date: start, Symbol: "VAS.AX", Type: "buy", Units: 100, Price: 90, FX: 1},
		{Date: start, Symbol: "AAPL", Type: "buy", Units: 10, Price: 150},
		{Date: start, Symbol: "NEW.AX", Type: "buy", Units: 100, Price: 10},
		{Date: start, Symbol: "FUND1", Type: "buy", Units: 50, Price: 50},
	}
	db.FX["USD"] = map[string]float64{start: 1.5, now.Format(dateFmt): 1.5}
	return db
}

func TestComputeOutlook(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	db := outlookDB(now)
	res := Compute(db, now)
	o := ComputeOutlook(db, res, "blend", now)

	if len(o.Assets) != 4 || len(o.Market) != 4 || o.Headline == "" {
		t.Fatalf("assets=%d market=%d headline=%q", len(o.Assets), len(o.Market), o.Headline)
	}
	by := map[string]*AssetOutlook{}
	for _, a := range o.Assets {
		by[a.Symbol] = a
	}
	if !by["VAS.AX"].Enough || !by["AAPL"].Enough || by["NEW.AX"].Enough || by["FUND1"].Enough {
		t.Fatalf("enough flags wrong: %+v", by)
	}
	if by["NEW.AX"].Note == "" || by["FUND1"].Note == "" {
		t.Error("unmodelled holdings should explain why")
	}
	v := by["VAS.AX"]
	if !(v.P10 < v.Median && v.Median < v.P90) || v.ProbUp < 0 || v.ProbUp > 100 || math.IsNaN(v.Beta) || v.Beta <= 0 {
		t.Errorf("VAS outlook: %+v", v)
	}
	p := o.Portfolio
	if p == nil || len(p.Included) != 2 || len(p.Flat) != 2 {
		t.Fatalf("portfolio: %+v", p)
	}
	wantFlat := by["NEW.AX"].ValueNow + by["FUND1"].ValueNow
	if math.Abs(p.FlatValue-wantFlat) > 0.01 {
		t.Errorf("flat value %v want %v", p.FlatValue, wantFlat)
	}
	if !(p.P10 < p.P25 && p.P25 < p.Median && p.Median < p.P75 && p.P75 < p.P90) {
		t.Errorf("portfolio bands: %+v", p)
	}
	if math.Abs(p.Start-res.Total.ValueAUD) > 0.01 {
		t.Errorf("start %v should equal portfolio value %v", p.Start, res.Total.ValueAUD)
	}
	for _, k := range []string{"portfolio", "VAS.AX", "AAPL", "m:^AXJO", "m:^VIX", "m:AUDUSD=X"} {
		c := o.Charts[k]
		if c == nil || len(c.Hist) < 2 {
			t.Errorf("chart %s missing", k)
			continue
		}
		if k == "portfolio" || k == "VAS.AX" {
			if c.Fan == nil || len(c.Fan.T) < 10 || c.Fan.T[len(c.Fan.T)-1] <= c.Now {
				t.Errorf("chart %s fan should extend past now", k)
			}
		}
	}
	if o.Charts["NEW.AX"] != nil {
		t.Error("no chart for a holding that can't be modelled")
	}
	// the JSON the page embeds must serialise (no NaN/Inf)
	if s := string(chartJS(o.Charts)); s == "{}" || len(s) < 1000 {
		t.Errorf("chart json suspicious: %d bytes", len(s))
	}

	// deterministic
	o2 := ComputeOutlook(db, res, "blend", now)
	if o2.Portfolio.Median != p.Median || o2.Assets[0].Median != o.Assets[0].Median {
		t.Error("outlook should be reproducible for the same inputs")
	}

	// scenarios behave: no-growth keeps the median near today; unknown key falls back to blend
	flat := ComputeOutlook(db, res, "flat", now).Portfolio
	if math.Abs(flat.Median/flat.Start-1) > 0.03 {
		t.Errorf("flat scenario median %v vs start %v", flat.Median, flat.Start)
	}
	if ComputeOutlook(db, res, "nonsense", now).Scenario != "blend" {
		t.Error("unknown scenario should fall back to blend")
	}
	if tr := ComputeOutlook(db, res, "trend", now); tr.ScenarioLabel == o.ScenarioLabel {
		t.Error("scenario label should change")
	}
}

func TestOutlookNoHistory(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	db := newTestDB()
	db.Txns = []Txn{{Date: "2026-01-02", Symbol: "VAS.AX", Type: "buy", Units: 10, Price: 100}}
	db.Prices["VAS.AX"] = map[string]float64{"2026-11-12": 110}
	o := ComputeOutlook(db, Compute(db, now), "blend", now)
	if o.Portfolio != nil || len(o.Assets) != 1 || o.Assets[0].Enough || o.Assets[0].Note == "" {
		t.Errorf("expected graceful no-history result: %+v", o.Assets[0])
	}
	if o.Headline == "" {
		t.Error("headline should explain missing market data")
	}
}
