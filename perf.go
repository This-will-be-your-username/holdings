package main

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// ---------- price / fx series ----------

type Series struct {
	D []string
	V []float64
}

func newSeries(m map[string]float64) *Series {
	s := &Series{}
	for d := range m {
		s.D = append(s.D, d)
	}
	sort.Strings(s.D)
	s.V = make([]float64, len(s.D))
	for i, d := range s.D {
		s.V[i] = m[d]
	}
	return s
}

// At returns the value on, or the nearest date before, date.
func (s *Series) At(date string) (float64, string, bool) {
	if s == nil || len(s.D) == 0 {
		return 0, "", false
	}
	i := sort.SearchStrings(s.D, date)
	if i < len(s.D) && s.D[i] == date {
		return s.V[i], s.D[i], true
	}
	if i == 0 {
		return 0, "", false
	}
	return s.V[i-1], s.D[i-1], true
}

func (s *Series) Last() (float64, string, bool) {
	if s == nil || len(s.D) == 0 {
		return 0, "", false
	}
	return s.V[len(s.V)-1], s.D[len(s.D)-1], true
}

// netCash is the cash actually received for a distribution, in fund currency.
// For foreign-currency funds the foreign tax is withheld from the gross amount;
// for AUD funds it is an extra offset that never touched the cash.
func netCash(amount, foreignTax float64, ccy string) float64 {
	if ccy != "AUD" && ccy != "" {
		return amount - foreignTax
	}
	return amount
}

// ---------- XIRR (money-weighted return) ----------

type Flow struct {
	T time.Time
	A float64
}

// xirr returns the annualised internal rate of return for dated cash flows.
// ok=false if it can't be computed or the period is under 30 days (annualising
// short periods is meaningless).
func xirr(flows []Flow) (float64, bool) {
	if len(flows) < 2 {
		return 0, false
	}
	fl := append([]Flow(nil), flows...)
	sort.SliceStable(fl, func(i, j int) bool { return fl[i].T.Before(fl[j].T) })
	var pos, neg bool
	for _, f := range fl {
		if f.A > 0 {
			pos = true
		} else if f.A < 0 {
			neg = true
		}
	}
	t0 := fl[0].T
	if !pos || !neg || fl[len(fl)-1].T.Sub(t0) < 30*24*time.Hour {
		return 0, false
	}
	npv := func(r float64) float64 {
		var s float64
		for _, f := range fl {
			s += f.A / math.Pow(1+r, f.T.Sub(t0).Hours()/24/365)
		}
		return s
	}
	lo, hi := -0.9999, 10.0
	for npv(lo)*npv(hi) > 0 && hi < 1e6 {
		hi *= 10
	}
	if npv(lo)*npv(hi) > 0 {
		return 0, false
	}
	for i := 0; i < 300; i++ {
		mid := (lo + hi) / 2
		if npv(lo)*npv(mid) <= 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return (lo + hi) / 2, true
}

// ---------- results ----------

type Row struct {
	Txn
	Name, Market, Currency string
	FXUsed                 float64
	GrossAUD               float64 // buy: cost incl. fees; sell: net proceeds; dividend/drp: income
	CashFlowAUD            float64 // investor cash: buy -, sell +, dividend +, drp 0
	UnitsAfter             float64
	AvgCostAfter           float64
	Realised               float64
	Warn                   string
}

type Holding struct {
	Private                        bool   // unlisted company, valued from your recorded checkpoints
	Live                           bool   // priced from a live quote (market open, quote under 15 minutes old)
	PriceAt                        int64  // unix time of the quote that set Price, if it came from a live quote
	PriceSource                    string // yahoo | asx, for live quotes
	Symbol, Name, Market, Currency string
	YearsHeld                      float64 // years since the first purchase
	Units, CostAUD, AvgCost        float64
	Price                          float64
	PriceDate                      string
	PriceNote                      string // "" ok, else why the price is questionable
	FXNow                          float64
	ValueAUD, Unrealised           float64
	Realised, Income               float64 // Income = cash distributions received (net of foreign tax withheld)
	Franking                       float64 // franking credits received
	Invested                       float64 // total new money put in (buys incl. fees)
	TotalReturn, ReturnPct         float64
	XIRR                           float64
	HasXIRR                        bool
	Closed                         bool
	Weight                         float64
	flows                          []Flow
}

type ChartPt struct {
	Date            string
	Value, Invested float64
}

type Suggest struct {
	Symbol, Name, Date, Currency string
	PayDate                      string // from the ASX when known
	PerUnit, Units, Amount       float64
	FrankPct, Franked, Franking  float64 // estimated from the ASX franking %
}

type Result struct {
	Rows      []Row
	Holdings  []*Holding // open positions
	Closed    []*Holding
	Total     *Holding
	Chart     []ChartPt
	Warnings  []string
	Suggested []Suggest
	FX        []FXRate // rates used to convert foreign holdings to AUD
}

// FXRate is the latest AUD-per-unit rate for a currency held, and the day it applies to.
type FXRate struct {
	Ccy, Date string
	Rate      float64
}

var typeOrder = map[string]int{"buy": 0, "drp": 1, "dividend": 2, "sell": 3}

type posState struct {
	units, cost, lastPx float64
	h                   *Holding
	sawBuy, warnedPre   bool
}

// Compute replays all transactions and derives personal performance:
// average-cost basis, realised/unrealised gains, income and XIRR.
func Compute(db *DB, now time.Time) *Result {
	today := now.Format(dateFmt)
	px := map[string]*Series{}
	for sym := range db.Instruments {
		m := map[string]float64{}
		for d, v := range db.Prices[sym] {
			m[d] = v
		}
		for d, v := range db.Manual[sym] {
			m[d] = v
		}
		// today's live quote, if it is newer than the saved daily history
		if q, ok := db.Quotes[sym]; ok && q.Price > 0 && q.Date != "" {
			latest := ""
			for d := range m {
				if d > latest {
					latest = d
				}
			}
			if q.Date >= latest {
				m[q.Date] = q.Price
			}
		}
		px[sym] = newSeries(m)
	}
	fxs := map[string]*Series{}
	for c, m := range db.FX {
		fxs[c] = newSeries(m)
	}
	fxAt := func(ccy, date string) (float64, bool) {
		if ccy == "" || ccy == "AUD" {
			return 1, true
		}
		s := fxs[ccy]
		if s == nil || len(s.D) == 0 {
			return 0, false
		}
		if v, _, ok := s.At(date); ok {
			return v, true
		}
		return s.V[0], true
	}

	txns := append([]Txn(nil), db.Txns...)
	sort.SliceStable(txns, func(i, j int) bool {
		if txns[i].Date != txns[j].Date {
			return txns[i].Date < txns[j].Date
		}
		return typeOrder[txns[i].Type] < typeOrder[txns[j].Type]
	})

	incl := !db.Settings.ExcludeFranking
	res := &Result{}
	st := map[string]*posState{}
	var order []string
	allFlows := []Flow{}

	for _, t := range txns {
		inst := db.Instruments[t.Symbol]
		if inst == nil {
			continue
		}
		s := st[t.Symbol]
		if s == nil {
			s = &posState{h: &Holding{Symbol: t.Symbol, Name: inst.Name, Market: inst.Market, Currency: inst.Currency, Private: inst.Private}}
			st[t.Symbol] = s
			order = append(order, t.Symbol)
		}
		h := s.h
		when, _ := time.Parse(dateFmt, t.Date)
		fx := t.FX
		row := Row{Txn: t, Name: inst.Name, Market: inst.Market, Currency: inst.Currency}
		if fx <= 0 {
			v, ok := fxAt(inst.Currency, t.Date)
			if !ok {
				row.Warn = "no FX rate yet - hit Refresh"
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s %s: no %s/AUD rate available (Refresh, or enter the rate manually)", t.Date, t.Symbol, inst.Currency))
			}
			fx = v
		}
		row.FXUsed = fx
		s.lastPx = t.Price

		switch t.Type {
		case "buy":
			s.sawBuy = true
			g := t.Units*t.Price*fx + t.Fees
			row.GrossAUD, row.CashFlowAUD = g, -g
			s.units += t.Units
			s.cost += g
			h.Invested += g
			h.flows = append(h.flows, Flow{when, -g})
		case "drp":
			if s.units == 0 && !s.sawBuy && !s.warnedPre {
				s.warnedPre = true
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: the first record on %s is a reinvestment, so earlier purchases are probably missing from the import. Units and value may be understated - import the earlier history, or compare with your provider at /reconcile.", inst.Name, t.Date))
			}
			a := t.Amount
			if a <= 0 {
				a = t.Units * t.Price
			}
			g := netCash(a, t.ForeignTax, inst.Currency) * fx
			row.GrossAUD = g
			s.units += t.Units
			s.cost += g
			h.Income += g // reinvested income: no net cash movement for XIRR
			h.Franking += t.Franking
			if incl && t.Franking > 0 {
				h.flows = append(h.flows, Flow{when, t.Franking})
			}
		case "dividend":
			g := netCash(t.Amount, t.ForeignTax, inst.Currency) * fx
			row.GrossAUD, row.CashFlowAUD = g, g
			h.Income += g
			h.Franking += t.Franking
			flow := g
			if incl {
				flow += t.Franking
			}
			h.flows = append(h.flows, Flow{when, flow})
		case "sell":
			net := t.Units*t.Price*fx - t.Fees
			u := t.Units
			if u > s.units+1e-9 {
				row.Warn = "sells more than held"
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s %s: sold %.4f units but only %.4f held", t.Date, t.Symbol, t.Units, s.units))
				u = s.units
			}
			avg := 0.0
			if s.units > 0 {
				avg = s.cost / s.units
			}
			removed := avg * u
			row.GrossAUD, row.CashFlowAUD = net, net
			row.Realised = net - removed
			h.Realised += row.Realised
			s.cost -= removed
			s.units -= u
			if s.units < 1e-9 {
				s.units, s.cost = 0, 0
			}
			h.flows = append(h.flows, Flow{when, net})
		}
		row.UnitsAfter = s.units
		if s.units > 0 {
			row.AvgCostAfter = s.cost / s.units
		}
		res.Rows = append(res.Rows, row)
	}

	total := &Holding{Symbol: "TOTAL", Name: "Portfolio"}
	nowT, _ := time.Parse(dateFmt, today)
	var totalValue float64
	for _, sym := range order {
		s := st[sym]
		h := s.h
		h.Units, h.CostAUD = s.units, s.cost
		if s.units > 0 {
			h.AvgCost = s.cost / s.units
		}
		h.FXNow, _ = fxAt(h.Currency, today)
		if p, d, ok := px[sym].Last(); ok {
			h.Price, h.PriceDate = p, d
			if q, ok := db.Quotes[sym]; ok && q.Date == d && q.Price == p {
				h.PriceAt, h.PriceSource = q.At, q.Source
				h.Live = now.Unix()-q.At < 15*60 && marketOpenByClock(db.Instruments[sym].Market, now)
			}
			if d < now.AddDate(0, 0, -7).Format(dateFmt) {
				h.PriceNote = "price is over a week old"
			}
		} else {
			h.Price, h.PriceNote = s.lastPx, "no market price - using your last trade price"
		}
		h.ValueAUD = h.Units * h.Price * h.FXNow
		if h.Units > 0 {
			h.Unrealised = h.ValueAUD - h.CostAUD
		}
		h.TotalReturn = h.Unrealised + h.Realised + h.Income
		if incl {
			h.TotalReturn += h.Franking
		}
		if h.Invested > 0 {
			h.ReturnPct = h.TotalReturn / h.Invested * 100
		}
		if len(h.flows) > 0 {
			h.YearsHeld = nowT.Sub(h.flows[0].T).Hours() / 24 / 365.25
		}
		fl := append([]Flow(nil), h.flows...)
		if h.Units > 0 {
			fl = append(fl, Flow{nowT, h.ValueAUD})
		}
		if r, ok := xirr(fl); ok {
			h.XIRR, h.HasXIRR = r*100, true
		}
		allFlows = append(allFlows, h.flows...)
		total.ValueAUD += h.ValueAUD
		total.CostAUD += h.CostAUD
		total.Unrealised += h.Unrealised
		total.Realised += h.Realised
		total.Income += h.Income
		total.Franking += h.Franking
		total.Invested += h.Invested
		totalValue += h.ValueAUD
		h.Closed = h.Units <= 0
		if h.Closed {
			res.Closed = append(res.Closed, h)
		} else {
			res.Holdings = append(res.Holdings, h)
		}
		if h.PriceNote != "" && !h.Closed {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %s", sym, h.PriceNote))
		}
		if h.Private && !h.Closed {
			if d, err := time.Parse(dateFmt, h.PriceDate); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s is valued at your last trade price - record a value checkpoint on the Private page.", h.Name))
			} else if now.Sub(d) > staleAfterDays*24*time.Hour {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s was last valued on %s - check for a newer valuation on the Private page.", h.Name, h.PriceDate))
			}
		}
	}
	fxSeen := map[string]bool{}
	for _, h := range res.Holdings {
		if c := h.Currency; c != "" && c != "AUD" && !fxSeen[c] {
			fxSeen[c] = true
			if s := fxs[c]; s != nil {
				if v, d, ok := s.Last(); ok {
					res.FX = append(res.FX, FXRate{Ccy: c, Date: d, Rate: v})
					if d < now.AddDate(0, 0, -7).Format(dateFmt) {
						res.Warnings = append(res.Warnings, fmt.Sprintf("The latest %s/AUD rate is from %s - Refresh to update currency conversion.", c, d))
					}
				}
			}
		}
	}
	sort.Slice(res.FX, func(i, j int) bool { return res.FX[i].Ccy < res.FX[j].Ccy })
	for _, h := range res.Holdings {
		if totalValue > 0 {
			h.Weight = h.ValueAUD / totalValue * 100
		}
	}
	sort.Slice(res.Holdings, func(i, j int) bool { return res.Holdings[i].ValueAUD > res.Holdings[j].ValueAUD })
	total.TotalReturn = total.Unrealised + total.Realised + total.Income
	if incl {
		total.TotalReturn += total.Franking
	}
	if total.Invested > 0 {
		total.ReturnPct = total.TotalReturn / total.Invested * 100
	}
	fl := append([]Flow(nil), allFlows...)
	if totalValue > 0 {
		fl = append(fl, Flow{nowT, totalValue})
	}
	if r, ok := xirr(fl); ok {
		total.XIRR, total.HasXIRR = r*100, true
	}
	res.Total = total

	res.Chart = buildChart(res.Rows, px, fxs, db, now)
	res.Suggested = suggestions(db, res.Rows)
	return res
}

func buildChart(rows []Row, px map[string]*Series, fxs map[string]*Series, db *DB, now time.Time) []ChartPt {
	if len(rows) == 0 {
		return nil
	}
	start, _ := time.Parse(dateFmt, rows[0].Date)
	units := map[string]float64{}
	last := map[string]float64{}
	var invested float64
	var pts []ChartPt
	i := 0
	emit := func(d time.Time) {
		ds := d.Format(dateFmt)
		for i < len(rows) && rows[i].Date <= ds {
			r := rows[i]
			switch r.Type {
			case "buy", "drp":
				units[r.Symbol] += r.Units
			case "sell":
				units[r.Symbol] -= r.Units
			}
			if r.Type == "buy" || r.Type == "sell" {
				invested -= r.CashFlowAUD
			}
			last[r.Symbol] = r.Price
			i++
		}
		var v float64
		for sym, u := range units {
			if u <= 1e-9 {
				continue
			}
			p := last[sym]
			if x, _, ok := px[sym].At(ds); ok {
				p = x
			}
			fx := 1.0
			if c := db.Instruments[sym].Currency; c != "AUD" {
				if s := fxs[c]; s != nil && len(s.D) > 0 {
					if x, _, ok := s.At(ds); ok {
						fx = x
					} else {
						fx = s.V[0]
					}
				}
			}
			v += u * p * fx
		}
		pts = append(pts, ChartPt{ds, v, invested})
	}
	for d := start; d.Before(now); d = d.AddDate(0, 0, 7) {
		emit(d)
	}
	emit(now)
	return pts
}

// suggestions lists distributions Yahoo reports for units you held on the
// ex-date that you haven't recorded yet.
func suggestions(db *DB, rows []Row) []Suggest {
	var out []Suggest
	for sym, divs := range db.Divs {
		inst := db.Instruments[sym]
		if inst == nil {
			continue
		}
		var mine []Row
		for _, r := range rows {
			if r.Symbol == sym {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			continue
		}
		for ex, per := range divs {
			if ex < mine[0].Date {
				continue
			}
			var held float64
			for _, r := range mine {
				if r.Date < ex {
					held = r.UnitsAfter
				}
			}
			if held <= 0 {
				continue
			}
			exT, _ := time.Parse(dateFmt, ex)
			lo, hi := exT.AddDate(0, 0, -5).Format(dateFmt), exT.AddDate(0, 0, 75).Format(dateFmt)
			done := false
			for _, r := range mine {
				if (r.Type == "dividend" || r.Type == "drp") && r.Date >= lo && r.Date <= hi {
					done = true
					break
				}
			}
			if !done {
				s := Suggest{Symbol: sym, Name: inst.Name, Date: ex, Currency: inst.Currency, PerUnit: per, Units: held, Amount: per * held}
				// match ASX detail (pay date, franking %) within a few days of the ex-date
				for d, info := range db.DivInfo[sym] {
					dt, err := time.Parse(dateFmt, d)
					if err == nil && dt.Sub(exT).Abs() <= 4*24*time.Hour {
						s.PayDate, s.FrankPct = info.Pay, info.Franking
						s.Franked = s.Amount * info.Franking / 100
						s.Franking = s.Franked * 30 / 70 // 30% company tax rate
						break
					}
				}
				out = append(out, s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}
