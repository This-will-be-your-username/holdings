package main

import (
	"fmt"
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"time"
)

// Outlook: trend analysis and a probabilistic six-month projection.
//
// This is a statistical projection from past behaviour, NOT a prediction. Prices
// are projected by resampling each holding's own historical daily moves (the
// same days across holdings, so correlations are kept) around an assumed drift.
// The output is a range of outcomes with probabilities. Distributions you
// receive are not included - these are price projections.

const (
	horizon   = 126  // trading days, about six months
	minObs    = 120  // minimum daily observations to model a holding
	nSimsPort = 3000 // portfolio paths
	nSimsAsst = 2000 // per-holding paths
	longRunPA = 0.06 // assumed long-run price growth, per year (equities, price only)
	driftCap  = 0.30 // clamp on assumed annual log drift
	recStep   = 3    // record every n-th step for the fan
)

var scenarios = []struct{ Key, Label, Desc string }{
	{"blend", "Balanced", "Average of the last year's trend, the full-history trend and a long-run assumption, plus a small market-trend tilt"},
	{"trend", "Recent trend continues", "Assumes the last 12 months' growth rate carries on (capped) - optimistic after a strong year, pessimistic after a weak one"},
	{"longrun", "Long-run average", "Assumes about 6% a year price growth regardless of recent performance"},
	{"flat", "No growth", "Zero expected drift - only volatility moves the range"},
}

type Pt struct {
	T int64   `json:"t"` // unix ms
	V float64 `json:"v"`
}

type FanData struct {
	T   []int64   `json:"t"`
	P10 []float64 `json:"p10"`
	P25 []float64 `json:"p25"`
	P50 []float64 `json:"p50"`
	P75 []float64 `json:"p75"`
	P90 []float64 `json:"p90"`
}

type LineCfg struct {
	Name string `json:"name"`
	Cls  string `json:"cls"`
	Pts  []Pt   `json:"pts"`
}

// ChartCfg describes one chart for web/static/charts.js: a history line, optional
// extra lines (moving averages, trend, net invested) and an optional forecast fan.
type ChartCfg struct {
	Name     string    `json:"name"`
	HistName string    `json:"histName"`
	Hist     []Pt      `json:"hist"`
	Lines    []LineCfg `json:"lines,omitempty"`
	Fan      *FanData  `json:"fan,omitempty"`
	Now      int64     `json:"now"`
	Mini     bool      `json:"mini,omitempty"`
	Invert   bool      `json:"invert,omitempty"` // falling is good (VIX)
	Fmt      string    `json:"fmt"`              // money | num | fx
}

type MarketCard struct {
	Sym, Name, Trend, Level      string
	Last, R1M, R3M, R12M         float64
	Vs50, Vs200, Vol30, DD, Tilt float64
}

type AssetOutlook struct {
	Symbol, Name, Market, BenchName, Trend string
	Units, ValueNow                        float64
	R1M, R3M, R6M, R12M, BenchR12          float64
	Vol60, VolAll, DD, Beta, Corr          float64
	SlopePA, R2                            float64 // regression trend over the last year, % p.a. and fit
	Mu, Tilt                               float64 // assumed drift and market tilt, % p.a.
	Median, P10, P90, ProbUp, ProbDown10   float64 // six-month outcomes for the position (AUD / %)
	Enough                                 bool
	Note                                   string
}

type PortfolioOutlook struct {
	Start, Median, P10, P25, P75, P90 float64
	ProbUp, ProbDown10, ProbUp10      float64
	Included, Flat                    []string
	FlatValue                         float64
	EndDate                           string
}

type Outlook struct {
	Scenario                    string
	ScenarioLabel, ScenarioDesc string
	Market                      []*MarketCard
	Headline, Detail, Tone      string
	Assets                      []*AssetOutlook
	Portfolio                   *PortfolioOutlook
	Charts                      map[string]*ChartCfg
	AsOf                        string
}

// ---------- small maths helpers ----------

func logRets(v []float64) []float64 {
	if len(v) < 2 {
		return nil
	}
	r := make([]float64, 0, len(v)-1)
	for i := 1; i < len(v); i++ {
		if v[i] > 0 && v[i-1] > 0 {
			r = append(r, math.Log(v[i]/v[i-1]))
		}
	}
	return r
}

func meanStd(x []float64) (m, s float64) {
	if len(x) == 0 {
		return math.NaN(), math.NaN()
	}
	for _, v := range x {
		m += v
	}
	m /= float64(len(x))
	if len(x) < 2 {
		return m, math.NaN()
	}
	for _, v := range x {
		s += (v - m) * (v - m)
	}
	return m, math.Sqrt(s / float64(len(x)-1))
}

func lastN(v []float64, n int) []float64 {
	if len(v) > n {
		return v[len(v)-n:]
	}
	return v
}

func maLast(v []float64, n int) float64 {
	if len(v) < n {
		return math.NaN()
	}
	m, _ := meanStd(v[len(v)-n:])
	return m
}

func rollMA(v []float64, n int) []float64 {
	out := make([]float64, len(v))
	var sum float64
	for i, x := range v {
		sum += x
		if i >= n {
			sum -= v[i-n]
		}
		if i >= n-1 {
			out[i] = sum / float64(n)
		} else {
			out[i] = math.NaN()
		}
	}
	return out
}

// regress fits y = a + b*x with x = index/252 (years). Returns slope per year and R².
func regress(y []float64) (a, b, r2, sd float64) {
	n := float64(len(y))
	if n < 3 {
		return math.NaN(), math.NaN(), math.NaN(), math.NaN()
	}
	var sx, sy, sxx, sxy float64
	for i, v := range y {
		x := float64(i) / 252
		sx += x
		sy += v
		sxx += x * x
		sxy += x * v
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN(), math.NaN(), math.NaN(), math.NaN()
	}
	b = (n*sxy - sx*sy) / den
	a = (sy - b*sx) / n
	var ssRes, ssTot float64
	my := sy / n
	for i, v := range y {
		f := a + b*float64(i)/252
		ssRes += (v - f) * (v - f)
		ssTot += (v - my) * (v - my)
	}
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	return a, b, r2, math.Sqrt(ssRes / (n - 2))
}

func pctChange(s *Series, lastDate string, days int) float64 {
	t, err := time.Parse(dateFmt, lastDate)
	if err != nil || len(s.V) == 0 {
		return math.NaN()
	}
	then := t.AddDate(0, 0, -days).Format(dateFmt)
	p0, d0, ok := s.At(then)
	if !ok || d0 < t.AddDate(0, 0, -days-10).Format(dateFmt) { // no data near that date
		return math.NaN()
	}
	return (s.V[len(s.V)-1]/p0 - 1) * 100
}

func trendLabel(price, ma50, ma200 float64) string {
	if math.IsNaN(ma200) {
		if math.IsNaN(ma50) {
			return "Short history"
		}
		if price >= ma50 {
			return "Uptrend"
		}
		return "Downtrend"
	}
	switch {
	case price >= ma200 && price >= ma50 && ma50 >= ma200:
		return "Uptrend"
	case price >= ma200 && price < ma50:
		return "Pullback"
	case price < ma200 && price >= ma50:
		return "Recovering"
	case price < ma200 && price < ma50:
		return "Downtrend"
	}
	return "Sideways"
}

func dayMs(d string) int64 {
	t, _ := time.Parse(dateFmt, d)
	return t.UnixMilli()
}

func pts(s *Series, v []float64, from string) []Pt {
	var out []Pt
	for i, d := range s.D {
		if d >= from && !math.IsNaN(v[i]) {
			out = append(out, Pt{dayMs(d), rnd(v[i])})
		}
	}
	return out
}

func bizDates(last string, steps int) []string {
	t, _ := time.Parse(dateFmt, last)
	out := make([]string, 0, steps)
	for len(out) < steps {
		t = t.AddDate(0, 0, 1)
		if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, t.Format(dateFmt))
		}
	}
	return out
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// ---------- simulation ----------

type simOut struct {
	steps                               []int
	q                                   [][5]float64 // P10 P25 P50 P75 P90 per recorded step
	start, probUp, probDown10, probUp10 float64
}

func quantile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// simulate runs bootstrap paths. rets[i] are the aligned daily log returns of asset i
// (same length for every asset so one random day moves all assets together).
// mu is the annual log drift, vs a volatility scale, w the starting AUD value of each
// asset, flat a constant extra value (holdings we can't model).
func simulate(rets [][]float64, mu, vs, w []float64, flat float64, n int, rng *rand.Rand) simOut {
	k := len(rets)
	T := len(rets[0])
	means := make([]float64, k)
	for i := range rets {
		means[i], _ = meanStd(rets[i])
	}
	var steps []int
	for s := 0; s <= horizon; s += recStep {
		steps = append(steps, s)
	}
	if steps[len(steps)-1] != horizon {
		steps = append(steps, horizon)
	}
	col := map[int]int{}
	for i, s := range steps {
		col[s] = i
	}
	vals := make([][]float64, len(steps))
	for i := range vals {
		vals[i] = make([]float64, n)
	}
	cum := make([]float64, k)
	start := flat
	for _, x := range w {
		start += x
	}
	for p := 0; p < n; p++ {
		for i := range cum {
			cum[i] = 0
		}
		vals[0][p] = start
		for s := 1; s <= horizon; s++ {
			j := rng.Intn(T)
			for i := 0; i < k; i++ {
				cum[i] += (rets[i][j]-means[i])*vs[i] + mu[i]/252
			}
			if c, ok := col[s]; ok {
				tot := flat
				for i := 0; i < k; i++ {
					tot += w[i] * math.Exp(cum[i])
				}
				vals[c][p] = tot
			}
		}
	}
	out := simOut{steps: steps, start: start}
	final := vals[len(vals)-1]
	var up, dn, up10 int
	for _, v := range final {
		if v > start {
			up++
		}
		if v < start*0.9 {
			dn++
		}
		if v > start*1.1 {
			up10++
		}
	}
	out.probUp = float64(up) / float64(n) * 100
	out.probDown10 = float64(dn) / float64(n) * 100
	out.probUp10 = float64(up10) / float64(n) * 100
	for _, c := range vals {
		sort.Float64s(c)
		out.q = append(out.q, [5]float64{quantile(c, .10), quantile(c, .25), quantile(c, .50), quantile(c, .75), quantile(c, .90)})
	}
	return out
}

func (so simOut) fan(last string) *FanData {
	dates := bizDates(last, horizon)
	f := &FanData{}
	for i, s := range so.steps {
		d := last
		if s > 0 {
			d = dates[s-1]
		}
		f.T = append(f.T, dayMs(d))
		q := so.q[i]
		f.P10 = append(f.P10, rnd(q[0]))
		f.P25 = append(f.P25, rnd(q[1]))
		f.P50 = append(f.P50, rnd(q[2]))
		f.P75 = append(f.P75, rnd(q[3]))
		f.P90 = append(f.P90, rnd(q[4]))
	}
	return f
}

func seedFor(parts ...string) int64 {
	h := fnv.New64a()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return int64(h.Sum64() >> 1)
}

// ---------- market context ----------

func benchFor(market string) string {
	if market == MarketUS {
		return "^GSPC"
	}
	return "^AXJO"
}

func marketCard(sym string, s *Series) *MarketCard {
	last, lastDate, ok := s.Last()
	if !ok || len(s.V) < 30 {
		return nil
	}
	c := &MarketCard{Sym: sym, Name: benchmarks[sym], Last: last}
	c.R1M, c.R3M, c.R12M = pctChange(s, lastDate, 30), pctChange(s, lastDate, 91), pctChange(s, lastDate, 365)
	ma50, ma200 := maLast(s.V, 50), maLast(s.V, 200)
	c.Vs50, c.Vs200 = (last/ma50-1)*100, (last/ma200-1)*100
	c.Trend = trendLabel(last, ma50, ma200)
	if _, sd := meanStd(lastN(logRets(s.V), 30)); !math.IsNaN(sd) {
		c.Vol30 = sd * math.Sqrt(252) * 100
	}
	hi := 0.0
	for _, v := range lastN(s.V, 252) {
		hi = math.Max(hi, v)
	}
	c.DD = (last/hi - 1) * 100
	if sym == "^VIX" {
		switch {
		case last < 15:
			c.Level = "calm"
		case last < 25:
			c.Level = "normal"
		case last < 35:
			c.Level = "elevated"
		default:
			c.Level = "stressed"
		}
	}
	// small, transparent tilt used in the Balanced scenario: price above its 200-day
	// average with a positive 6-month return -> +1.5% a year; below with negative -> -1.5%
	r6 := pctChange(s, lastDate, 182)
	switch {
	case last > ma200 && r6 > 0:
		c.Tilt = 1.5
	case last < ma200 && r6 < 0:
		c.Tilt = -1.5
	}
	return c
}

func regimeSummary(cards map[string]*MarketCard) (headline, detail, tone string) {
	var parts []string
	up, down, n := 0, 0, 0
	for _, k := range []string{"^AXJO", "^GSPC"} {
		c := cards[k]
		if c == nil {
			continue
		}
		n++
		switch c.Trend {
		case "Uptrend":
			up++
		case "Downtrend":
			down++
		}
		dir := "above"
		if c.Vs200 < 0 {
			dir = "below"
		}
		parts = append(parts, fmt.Sprintf("%s is %.1f%% %s its 200-day average (%s) and %+.1f%% over 12 months.", c.Name, math.Abs(c.Vs200), dir, c.Trend, c.R12M))
	}
	if v := cards["^VIX"]; v != nil {
		parts = append(parts, fmt.Sprintf("US market volatility (VIX %.1f) is %s.", v.Last, v.Level))
	}
	if a := cards["AUDUSD=X"]; a != nil {
		parts = append(parts, fmt.Sprintf("The Australian dollar (%.4f USD) is %+.1f%% over 3 months (%s) - a stronger AUD lowers the AUD value of US holdings.", a.Last, a.R3M, a.Trend))
	}
	switch {
	case n == 0:
		return "No market data yet", "Refresh prices to load the wider market data.", "neutral"
	case up == n:
		headline, tone = "Broad uptrend", "pos"
	case down == n:
		headline, tone = "Broad downtrend", "neg"
	default:
		headline, tone = "Mixed signals", "neutral"
	}
	for i, p := range parts {
		if i > 0 {
			detail += " "
		}
		detail += p
	}
	return
}

// ---------- main ----------

func ComputeOutlook(db *DB, res *Result, scenario string, now time.Time) *Outlook {
	o := &Outlook{Scenario: scenario, Charts: map[string]*ChartCfg{}, AsOf: now.Format(dateFmt)}
	found := false
	for _, s := range scenarios {
		if s.Key == scenario {
			o.ScenarioLabel, o.ScenarioDesc, found = s.Label, s.Desc, true
		}
	}
	if !found {
		o.Scenario, o.ScenarioLabel, o.ScenarioDesc = scenarios[0].Key, scenarios[0].Label, scenarios[0].Desc
		scenario = o.Scenario
	}
	oneYear := now.AddDate(-1, 0, 0).Format(dateFmt)

	bs := map[string]*Series{}
	cards := map[string]*MarketCard{}
	for sym := range benchmarks {
		if m := db.Bench[sym]; len(m) > 0 {
			bs[sym] = newSeries(m)
			if c := marketCard(sym, bs[sym]); c != nil {
				cards[sym] = c
			}
		}
	}
	for _, sym := range []string{"^AXJO", "^GSPC", "^VIX", "AUDUSD=X"} {
		if c := cards[sym]; c != nil {
			o.Market = append(o.Market, c)
			s := bs[sym]
			f := "num"
			if sym == "AUDUSD=X" {
				f = "fx"
			}
			o.Charts["m:"+sym] = &ChartCfg{Name: c.Name, HistName: c.Name, Hist: pts(s, s.V, oneYear), Now: dayMs(s.D[len(s.D)-1]), Mini: true, Fmt: f, Invert: sym == "^VIX"}
		}
	}
	o.Headline, o.Detail, o.Tone = regimeSummary(cards)

	// AUD per USD history from AUD/USD
	var usd *Series
	if m := db.Bench["AUDUSD=X"]; len(m) > 0 {
		inv := map[string]float64{}
		for d, v := range m {
			if v > 0 {
				inv[d] = 1 / v
			}
		}
		usd = newSeries(inv)
	}

	type modelled struct {
		a    *AssetOutlook
		ser  *Series
		mu   float64 // annual log drift
		vs   float64
		hold *Holding
	}
	var mods []*modelled
	for _, h := range res.Holdings {
		inst := db.Instruments[h.Symbol]
		if inst == nil {
			continue
		}
		a := &AssetOutlook{Symbol: h.Symbol, Name: h.Name, Market: h.Market, Units: h.Units, ValueNow: h.ValueAUD,
			R1M: math.NaN(), R3M: math.NaN(), R6M: math.NaN(), R12M: math.NaN(), BenchR12: math.NaN(),
			Beta: math.NaN(), Corr: math.NaN(), SlopePA: math.NaN(), R2: math.NaN(), DD: math.NaN()}
		o.Assets = append(o.Assets, a)
		bsym := benchFor(h.Market)
		if c := cards[bsym]; c != nil {
			a.BenchName, a.BenchR12 = c.Name, c.R12M
		}

		m := map[string]float64{}
		for d, v := range db.Prices[h.Symbol] {
			m[d] = v
		}
		for d, v := range db.Manual[h.Symbol] {
			m[d] = v
		}
		ser := newSeries(m)
		if inst.Currency != "AUD" && inst.Currency != "" {
			var fx *Series
			if inst.Currency == "USD" {
				fx = usd
			} else if fm := db.FX[inst.Currency]; len(fm) > 0 {
				fx = newSeries(fm)
			}
			if fx == nil {
				a.Note = "No exchange-rate history for " + inst.Currency
				continue
			}
			cm := map[string]float64{}
			for i, d := range ser.D {
				if f, _, ok := fx.At(d); ok {
					cm[d] = ser.V[i] * f
				}
			}
			ser = newSeries(cm)
		}
		if len(ser.V) < minObs+1 {
			if h.Market == MarketManaged {
				a.Note = fmt.Sprintf("Needs about %d+ unit prices to model (have %d) - add more on the Fund prices page", minObs, len(ser.V))
			} else {
				a.Note = fmt.Sprintf("Only %d days of price history - too short to model yet", len(ser.V))
			}
			continue
		}
		a.Enough = true
		last, lastDate, _ := ser.Last()
		a.R1M, a.R3M, a.R6M, a.R12M = pctChange(ser, lastDate, 30), pctChange(ser, lastDate, 91), pctChange(ser, lastDate, 182), pctChange(ser, lastDate, 365)
		rets := logRets(ser.V)
		_, sdAll := meanStd(rets)
		_, sd60 := meanStd(lastN(rets, 60))
		a.VolAll, a.Vol60 = sdAll*math.Sqrt(252)*100, sd60*math.Sqrt(252)*100
		hi := 0.0
		for _, v := range lastN(ser.V, 252) {
			hi = math.Max(hi, v)
		}
		a.DD = (last/hi - 1) * 100
		ma50, ma200 := maLast(ser.V, 50), maLast(ser.V, 200)
		a.Trend = trendLabel(last, ma50, ma200)

		// regression trend over the last year
		win := lastN(ser.V, 252)
		ly := make([]float64, len(win))
		for i, v := range win {
			ly[i] = math.Log(v)
		}
		ra, rb, r2, _ := regress(ly)
		if !math.IsNaN(rb) {
			a.SlopePA, a.R2 = (math.Exp(rb)-1)*100, r2
		}

		// beta / correlation against the benchmark
		if b := bs[bsym]; b != nil {
			var ar, br []float64
			from := len(ser.D) - 504
			if from < 0 {
				from = 0
			}
			var pa, pb float64
			for i := from; i < len(ser.D); i++ {
				bv, _, ok := b.At(ser.D[i])
				if !ok {
					continue
				}
				if pa > 0 && pb > 0 {
					ar = append(ar, math.Log(ser.V[i]/pa))
					br = append(br, math.Log(bv/pb))
				}
				pa, pb = ser.V[i], bv
			}
			if len(ar) > 60 {
				ma, sa := meanStd(ar)
				mb, sb := meanStd(br)
				var cov float64
				for i := range ar {
					cov += (ar[i] - ma) * (br[i] - mb)
				}
				cov /= float64(len(ar) - 1)
				if sb > 0 && sa > 0 {
					a.Beta, a.Corr = cov/(sb*sb), cov/(sa*sb)
				}
			}
		}

		// drift assumption
		recent := math.Log(last / ser.V[maxInt(0, len(ser.V)-253)])
		full := recent
		if yrs := float64(len(ser.V)) / 252; yrs >= 2 {
			full = math.Log(last/ser.V[0]) / yrs
		}
		lr := math.Log(1 + longRunPA)
		tilt := 0.0
		if c := cards[bsym]; c != nil {
			tilt = c.Tilt / 100
		}
		var mu float64
		switch scenario {
		case "trend":
			mu, tilt = clamp(recent, -0.40, 0.40), 0
		case "longrun":
			mu, tilt = lr, 0
		case "flat":
			mu, tilt = 0, 0
		default:
			mu = clamp((recent+full+lr)/3+tilt, -driftCap, driftCap)
		}
		a.Mu, a.Tilt = mu*100, tilt*100
		vs := 1.0
		if sdAll > 0 && !math.IsNaN(sd60) {
			vs = clamp(sd60/sdAll, 0.8, 1.4) // reflect current volatility conditions
		}

		// per-holding fan
		rr := lastN(rets, 756)
		so := simulate([][]float64{rr}, []float64{mu}, []float64{vs}, []float64{h.ValueAUD}, 0, nSimsAsst,
			rand.New(rand.NewSource(seedFor(scenario, h.Symbol, lastDate))))
		fin := so.q[len(so.q)-1]
		a.P10, a.Median, a.P90 = fin[0], fin[2], fin[4]
		a.ProbUp, a.ProbDown10 = so.probUp, so.probDown10

		// chart: price (AUD) history, moving averages, regression trend line, fan.
		// The line and fan are scaled to the value of your position.
		scale := h.ValueAUD / last
		rawMA50, rawMA200 := rollMA(ser.V, 50), rollMA(ser.V, 200)
		posVals := make([]float64, len(ser.V))
		pm50, pm200 := make([]float64, len(ser.V)), make([]float64, len(ser.V))
		for i, v := range ser.V {
			posVals[i], pm50[i], pm200[i] = v*scale, rawMA50[i]*scale, rawMA200[i]*scale
		}
		cfg := &ChartCfg{Name: a.Name, HistName: "Past value of this holding", Hist: pts(ser, posVals, oneYear), Fmt: "money",
			Lines: []LineCfg{{"50-day average", "ma50", pts(ser, pm50, oneYear)}, {"200-day average", "ma200", pts(ser, pm200, oneYear)}},
			Now:   dayMs(lastDate), Fan: so.fan(lastDate)}
		if !math.IsNaN(rb) {
			var tl []Pt
			n := len(win)
			start := len(ser.D) - n
			for i := 0; i < n; i++ {
				if ser.D[start+i] >= oneYear {
					tl = append(tl, Pt{dayMs(ser.D[start+i]), rnd(math.Exp(ra+rb*float64(i)/252) * scale)})
				}
			}
			for s, d := range bizDates(lastDate, horizon) {
				if (s+1)%recStep == 0 || s+1 == horizon {
					tl = append(tl, Pt{dayMs(d), rnd(math.Exp(ra+rb*float64(n+s)/252) * scale)})
				}
			}
			cfg.Lines = append(cfg.Lines, LineCfg{"1-year trend line", "trend", tl})
		}
		o.Charts[h.Symbol] = cfg
		mods = append(mods, &modelled{a: a, ser: ser, mu: mu, vs: vs, hold: h})
	}

	// portfolio: resample the same historical days for every holding so correlations carry over
	if len(mods) > 0 {
		cut := now.AddDate(-3, 0, 0).Format(dateFmt)
		set := append([]*modelled(nil), mods...)
		var rets [][]float64
		for len(set) > 0 {
			count := map[string]int{}
			for _, m := range set {
				for _, d := range m.ser.D {
					if d >= cut {
						count[d]++
					}
				}
			}
			var common []string
			for d, c := range count {
				if c == len(set) {
					common = append(common, d)
				}
			}
			sort.Strings(common)
			if len(common) > minObs || len(set) == 1 {
				rets = make([][]float64, len(set))
				for i, m := range set {
					idx := map[string]float64{}
					for j, d := range m.ser.D {
						idx[d] = m.ser.V[j]
					}
					for j := 1; j < len(common); j++ {
						rets[i] = append(rets[i], math.Log(idx[common[j]]/idx[common[j-1]]))
					}
				}
				break
			}
			sort.Slice(set, func(i, j int) bool { return len(set[i].ser.D) > len(set[j].ser.D) })
			set = set[:len(set)-1] // drop the shortest history and retry
		}
		if len(set) > 0 && len(rets) > 0 && len(rets[0]) >= minObs/2 {
			in := map[string]bool{}
			mu, vs, w := make([]float64, len(set)), make([]float64, len(set)), make([]float64, len(set))
			var syms []string
			for i, m := range set {
				mu[i], vs[i], w[i] = m.mu, m.vs, m.hold.ValueAUD
				in[m.hold.Symbol] = true
				syms = append(syms, m.hold.Symbol)
			}
			var flat float64
			var flatSyms []string
			for _, h := range res.Holdings {
				if !in[h.Symbol] {
					flat += h.ValueAUD
					flatSyms = append(flatSyms, h.Symbol)
				}
			}
			sort.Strings(syms)
			so := simulate(rets, mu, vs, w, flat, nSimsPort, rand.New(rand.NewSource(seedFor(append([]string{scenario, now.Format(dateFmt)}, syms...)...))))
			fin := so.q[len(so.q)-1]
			last := ""
			for _, m := range set {
				if _, d, ok := m.ser.Last(); ok && d > last {
					last = d
				}
			}
			o.Portfolio = &PortfolioOutlook{Start: so.start, P10: fin[0], P25: fin[1], Median: fin[2], P75: fin[3], P90: fin[4],
				ProbUp: so.probUp, ProbDown10: so.probDown10, ProbUp10: so.probUp10, Included: syms, Flat: flatSyms, FlatValue: flat,
				EndDate: bizDates(last, horizon)[horizon-1]}
			cfg := &ChartCfg{Name: "Your portfolio", HistName: "Your portfolio value", Fmt: "money", Now: dayMs(last), Fan: so.fan(last)}
			for _, p := range res.Chart {
				if p.Date >= oneYear {
					cfg.Hist = append(cfg.Hist, Pt{dayMs(p.Date), rnd(p.Value)})
				}
			}
			o.Charts["portfolio"] = cfg
		}
	}
	return o
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// rnd trims chart values to 4 decimals to keep the embedded JSON small.
func rnd(v float64) float64 { return math.Round(v*1e4) / 1e4 }
