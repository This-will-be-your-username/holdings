package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// This file models the tax position of an Australian-resident individual
// holding investments directly (not via a company, trust or super fund).
// It is an estimate to support your return, not tax advice.

// ---------- income years & rates ----------

// fyOf returns the Australian income year (1 Jul - 30 Jun) for a date, e.g. "2025-26".
func fyOf(date string) string {
	t, err := time.Parse(dateFmt, date)
	if err != nil {
		return ""
	}
	y := t.Year()
	if t.Month() < time.July {
		y--
	}
	return fmt.Sprintf("%d-%02d", y, (y+1)%100)
}

func fyStart(fy string) int {
	var y int
	fmt.Sscanf(fy, "%d", &y)
	return y
}

type bracket struct{ from, rate float64 }

// scaleFor returns resident marginal rates (excluding Medicare levy).
// 2026-27: first rate cut to 15%; 2027-28 onwards: 14% (legislated).
func scaleFor(fy string) []bracket {
	switch y := fyStart(fy); {
	case y >= 2027:
		return []bracket{{0, 0}, {18200, .14}, {45000, .30}, {135000, .37}, {190000, .45}}
	case y == 2026:
		return []bracket{{0, 0}, {18200, .15}, {45000, .30}, {135000, .37}, {190000, .45}}
	case y >= 2024:
		return []bracket{{0, 0}, {18200, .16}, {45000, .30}, {135000, .37}, {190000, .45}}
	default: // 2022-23 and 2023-24 (earlier years are close enough for estimates)
		return []bracket{{0, 0}, {18200, .19}, {45000, .325}, {120000, .37}, {180000, .45}}
	}
}

func incomeTax(fy string, inc float64) float64 {
	b := scaleFor(fy)
	var tax float64
	for i, br := range b {
		top := math.Inf(1)
		if i+1 < len(b) {
			top = b[i+1].from
		}
		if inc > br.from {
			tax += (math.Min(inc, top) - br.from) * br.rate
		}
	}
	return tax
}

// taxAndMedicare: flat 2% Medicare levy (low-income reductions, surcharge and
// HELP are not modelled).
func taxAndMedicare(fy string, inc float64) float64 {
	if inc <= 0 {
		return 0
	}
	return incomeTax(fy, inc) + 0.02*inc
}

// ---------- capital gains ----------

type parcel struct {
	ess   bool // awarded under a tax-deferred share scheme: a sale is ordinary income, not a capital gain
	acq   time.Time
	acqS  string
	units float64
	cost  float64 // AUD cost base of the remaining units
	seq   int     // order this holding's parcels were created in (stable id for Pick)
}

type CGTEvent struct {
	FY, Symbol, Name, Acquired, Sold, Kind string
	TxnID                                  string // the sell transaction that produced this event
	Units, Cost, Proceeds, Gain            float64
	Days                                   int
	Discount                               bool
}

// DiscountedGain is the gain after the 50% discount (losses are never discounted).
func (e CGTEvent) DiscountedGain() float64 {
	if e.Gain > 0 && e.Discount {
		return e.Gain / 2
	}
	return e.Gain
}

func discountEligible(acq, sold time.Time) bool {
	return sold.After(acq.AddDate(1, 0, 0)) // owned for more than 12 months
}

// cgtEvents replays transactions into tax parcels and returns each disposal
// (one event per parcel consumed) plus any E4 gains from tax-deferred
// distributions that exceed the cost base.
func cgtEvents(rows []Row, method string) []CGTEvent { return cgtEventsM(rows, method, "") }

// cgtEventsM is cgtEvents, but hypothetical sells (IDs starting "est-", used by
// the sale estimator) can use their own parcel method without changing how
// your real past sales were matched.
func cgtEventsM(rows []Row, method, estMethod string) []CGTEvent {
	ev, _ := cgtReplay(rows, method, estMethod, nil)
	return ev
}

// cgtReplay is the parcel engine. picks (by hypothetical-sale id) name exact parcels to sell
// first; anything not covered by picks is matched using the method. It also returns the
// parcels left open at the end, which the withdrawal planner chooses from.
func cgtReplay(rows []Row, method, estMethod string, picks map[string][]Pick) ([]CGTEvent, map[string][]*parcel) {
	parcels := map[string][]*parcel{}
	var out []CGTEvent
	for _, r := range rows {
		switch r.Type {
		case "dividend", "drp":
			if r.TaxDeferred > 0 {
				ps := parcels[r.Symbol]
				var total float64
				for _, p := range ps {
					total += p.cost
				}
				switch {
				case total > 0 && r.TaxDeferred <= total:
					k := 1 - r.TaxDeferred/total
					for _, p := range ps {
						p.cost *= k
					}
				default: // cost base exhausted: excess is a capital gain (CGT event E4)
					excess := r.TaxDeferred - total
					for _, p := range ps {
						p.cost = 0
					}
					fy := r.TaxYear
					if fy == "" {
						fy = fyOf(r.Date)
					}
					out = append(out, CGTEvent{FY: fy, Symbol: r.Symbol, Name: r.Name, Acquired: "", Sold: r.Date,
						Kind: "E4", Gain: excess, Proceeds: excess})
				}
			}
		}
		switch r.Type {
		case "buy", "drp", "award":
			d, _ := time.Parse(dateFmt, r.Date)
			acqS := r.Date
			if r.LongTerm && (r.Type == "buy" || r.Type == "award") {
				d = d.AddDate(-1, 0, -1) // known to be over 12 months old; the exact day isn't
				acqS = d.Format(dateFmt)
			}
			parcels[r.Symbol] = append(parcels[r.Symbol], &parcel{ess: r.Deferred, acq: d, acqS: acqS, units: r.Units, cost: r.GrossAUD, seq: len(parcels[r.Symbol])})
		case "sell":
			sold, _ := time.Parse(dateFmt, r.Date)
			ps := parcels[r.Symbol]
			ppu := 0.0 // net proceeds per unit
			if r.Units > 0 {
				ppu = r.GrossAUD / r.Units
			}
			order := append([]*parcel(nil), ps...)
			m := method
			if estMethod != "" && strings.HasPrefix(r.ID, "est-") {
				m = estMethod
			}
			switch m {
			case "hifo":
				sort.SliceStable(order, func(i, j int) bool { return perUnit(order[i]) > perUnit(order[j]) })
			case "optimal":
				key := func(p *parcel) float64 { // taxable gain per unit after any discount
					g := ppu - perUnit(p)
					if g > 0 && discountEligible(p.acq, sold) {
						g /= 2
					}
					return g
				}
				sort.SliceStable(order, func(i, j int) bool { return key(order[i]) < key(order[j]) })
			}
			need := r.Units
			consume := func(p *parcel, want float64) {
				u := math.Min(math.Min(want, need), p.units)
				if u <= 1e-9 {
					return
				}
				cost := p.cost * u / p.units
				proceeds := ppu * u
				if p.ess {
					// No tax was paid when these shares were awarded, so the whole sale value (you paid nothing for
					// them) is assessable income at the sale. Their cost base for CGT then equals that value, so there
					// is no capital gain or loss and no discount.
					out = append(out, CGTEvent{
						FY: fyOf(r.Date), Symbol: r.Symbol, Name: r.Name, Acquired: p.acqS, Sold: r.Date, Kind: "ess", TxnID: r.ID,
						Units: u, Cost: 0, Proceeds: proceeds, Gain: proceeds,
						Days: int(sold.Sub(p.acq).Hours() / 24),
					})
					p.cost -= cost
					p.units -= u
					need -= u
					return
				}
				out = append(out, CGTEvent{
					FY: fyOf(r.Date), Symbol: r.Symbol, Name: r.Name, Acquired: p.acqS, Sold: r.Date, Kind: "sale", TxnID: r.ID,
					Units: u, Cost: cost, Proceeds: proceeds, Gain: proceeds - cost,
					Days: int(sold.Sub(p.acq).Hours() / 24), Discount: discountEligible(p.acq, sold),
				})
				p.cost -= cost
				p.units -= u
				need -= u
			}
			for _, pk := range picks[r.ID] { // exact parcels chosen by the withdrawal planner
				for _, p := range ps {
					if p.seq == pk.Seq {
						consume(p, pk.Units)
					}
				}
			}
			for _, p := range order {
				if need <= 1e-9 {
					break
				}
				consume(p, need)
			}
		}
	}
	return out, parcels
}

func perUnit(p *parcel) float64 {
	if p.units <= 0 {
		return 0
	}
	return p.cost / p.units
}

// netGain applies capital losses to non-discount gains first, then discount
// gains, then discounts what's left (50%). Returns net gain, losses carried
// forward and the discount amount.
func netGain(disc, other, loss float64) (net, carry, discount float64) {
	l := loss
	if l >= other {
		l -= other
		other = 0
	} else {
		other -= l
		l = 0
	}
	if l >= disc {
		l -= disc
		disc = 0
	} else {
		disc -= l
		l = 0
	}
	return other + disc/2, l, disc / 2
}

// ---------- per-year summary ----------

type DistLine struct {
	FY, Date, Symbol, Name, Type, Currency string
	Amount, FX                             float64 // gross, fund currency
	GrossAUD, CashAUD                      float64
	Franked, Unfranked, Franking           float64
	ForeignTaxAUD                          float64
	CGDisc, CGOther, TaxDeferred           float64
	Detailed                               bool // user supplied a breakdown
}

type FYTax struct {
	FY string
	// distributions
	Dists                                      []DistLine
	CashReceived, Franked, Unfranked, Franking float64
	ForeignTaxOffset, DistCGDisc, DistCGOther  float64
	TaxDeferred                                float64
	Undetailed                                 int // distributions with no tax breakdown (treated as unfranked income)
	// capital gains
	Events                                      []CGTEvent
	GainsDisc, GainsOther, Losses               float64 // from disposals (gross, before losses/discount)
	LossBroughtFwd                              float64
	NetCapGain, LossCarriedFwd, DiscountApplied float64
	ESSIncome                                   float64 // sales of tax-deferred awarded shares: ordinary income, taxed in full
	// estimate
	Assessable, OtherIncome                      float64
	TaxOnTop, Offsets, NetPayable, EffectiveRate float64
	FrankingRefund                               float64
	FTOClaimed                                   float64
	Current                                      bool
}

type TaxResult struct {
	FYs  []*FYTax // ascending
	ByFY map[string]*FYTax
	All  []CGTEvent
}

func ComputeTax(db *DB, rows []Row, now time.Time) *TaxResult {
	set := db.Settings
	tr := &TaxResult{ByFY: map[string]*FYTax{}}
	get := func(fy string) *FYTax {
		f := tr.ByFY[fy]
		if f == nil {
			f = &FYTax{FY: fy}
			tr.ByFY[fy] = f
		}
		return f
	}
	cur := fyOf(now.Format(dateFmt))
	get(cur)

	for _, r := range rows {
		if r.Type != "dividend" && r.Type != "drp" {
			continue
		}
		fy := r.TaxYear
		if fy == "" {
			fy = fyOf(r.Date)
		}
		f := get(fy)
		fx := r.FXUsed
		amt := r.Amount
		if amt <= 0 {
			amt = r.Units * r.Price // DRP without a stated amount
		}
		gross := amt * fx
		ft := r.ForeignTax * fx
		ordinary := math.Max(gross-r.CGDisc-r.CGOther-r.TaxDeferred, 0)
		franked := math.Min(r.Franked, ordinary)
		unfranked := ordinary - franked
		cash := gross
		if r.Currency != "AUD" && r.Currency != "" {
			cash -= ft // withheld at source; already inside gross
		} else {
			unfranked += ft // AUD fund: foreign tax offset is an extra gross-up
		}
		detailed := r.Franked > 0 || r.Franking > 0 || r.ForeignTax > 0 || r.CGDisc > 0 || r.CGOther > 0 || r.TaxDeferred > 0
		l := DistLine{FY: fy, Date: r.Date, Symbol: r.Symbol, Name: r.Name, Type: r.Type, Currency: r.Currency,
			Amount: amt, FX: fx, GrossAUD: gross, CashAUD: cash, Franked: franked, Unfranked: unfranked,
			Franking: r.Franking, ForeignTaxAUD: ft, CGDisc: r.CGDisc, CGOther: r.CGOther, TaxDeferred: r.TaxDeferred, Detailed: detailed}
		f.Dists = append(f.Dists, l)
		f.CashReceived += cash
		f.Franked += franked
		f.Unfranked += unfranked
		f.Franking += r.Franking
		f.ForeignTaxOffset += ft
		f.DistCGDisc += r.CGDisc
		f.DistCGOther += r.CGOther
		f.TaxDeferred += r.TaxDeferred
		if !detailed {
			f.Undetailed++
		}
	}

	tr.All, _ = cgtReplay(rows, set.CGTMethod, set.EstMethod, set.EstPicks)
	for _, e := range tr.All {
		f := get(e.FY)
		f.Events = append(f.Events, e)
		switch {
		case e.Kind == "ess":
			f.ESSIncome += e.Gain
		case e.Gain < 0:
			f.Losses += -e.Gain
		case e.Discount:
			f.GainsDisc += e.Gain
		default:
			f.GainsOther += e.Gain
		}
	}

	for _, f := range tr.ByFY {
		tr.FYs = append(tr.FYs, f)
	}
	sort.Slice(tr.FYs, func(i, j int) bool { return tr.FYs[i].FY < tr.FYs[j].FY })

	carry := set.OpeningLoss
	for _, f := range tr.FYs {
		f.Current = f.FY == cur
		f.LossBroughtFwd = carry
		f.NetCapGain, f.LossCarriedFwd, f.DiscountApplied = netGain(
			f.GainsDisc+f.DistCGDisc, f.GainsOther+f.DistCGOther, f.Losses+carry)
		carry = f.LossCarriedFwd

		f.Assessable = f.Franked + f.Unfranked + f.Franking + f.NetCapGain + f.ESSIncome
		f.OtherIncome = set.OtherIncome
		f.TaxOnTop = taxAndMedicare(f.FY, set.OtherIncome+f.Assessable) - taxAndMedicare(f.FY, set.OtherIncome)
		f.FTOClaimed = f.ForeignTaxOffset
		if f.FTOClaimed > 1000 { // above the $1,000 de minimis the offset is capped at Australian tax on the income
			f.FTOClaimed = math.Min(f.FTOClaimed, math.Max(f.TaxOnTop, 0))
		}
		f.Offsets = f.Franking + f.FTOClaimed
		f.NetPayable = f.TaxOnTop - f.Offsets
		if f.Assessable > 0 {
			f.EffectiveRate = f.TaxOnTop / f.Assessable * 100
		}
	}
	return tr
}

// ---------- CSV ----------

func WriteCGTCSV(w io.Writer, evs []CGTEvent) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"income_year", "symbol", "name", "event", "acquired", "sold", "days_held", "units",
		"cost_base_aud", "proceeds_aud", "capital_gain_aud", "discount_eligible", "gain_after_discount_aud"})
	for _, e := range evs {
		d := "no"
		if e.Discount {
			d = "yes"
		}
		_ = cw.Write([]string{e.FY, e.Symbol, e.Name, e.Kind, e.Acquired, e.Sold, fmt.Sprint(e.Days), f(e.Units, 6),
			f(e.Cost, 2), f(e.Proceeds, 2), f(e.Gain, 2), d, f(e.DiscountedGain(), 2)})
	}
	cw.Flush()
	return cw.Error()
}

func WriteDistCSV(w io.Writer, fys []*FYTax) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"income_year", "date", "symbol", "name", "type", "currency", "gross_amount", "fx_to_aud",
		"gross_aud", "cash_received_aud", "franked_aud", "unfranked_aud", "franking_credits_aud",
		"foreign_tax_offset_aud", "cg_discountable_aud", "cg_other_aud", "tax_deferred_aud"})
	for _, fy := range fys {
		for _, d := range fy.Dists {
			_ = cw.Write([]string{d.FY, d.Date, d.Symbol, d.Name, d.Type, d.Currency, f(d.Amount, 2), f(d.FX, 6),
				f(d.GrossAUD, 2), f(d.CashAUD, 2), f(d.Franked, 2), f(d.Unfranked, 2), f(d.Franking, 2),
				f(d.ForeignTaxAUD, 2), f(d.CGDisc, 2), f(d.CGOther, 2), f(d.TaxDeferred, 2)})
		}
	}
	cw.Flush()
	return cw.Error()
}

func WriteTaxSummaryCSV(w io.Writer, fys []*FYTax) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"income_year", "franked_dividends", "unfranked_and_other_income", "franking_credits",
		"foreign_tax_offset", "distributed_cg_discountable", "distributed_cg_other", "tax_deferred_non_assessable",
		"disposal_gains_discountable", "disposal_gains_other", "capital_losses_current", "losses_brought_forward",
		"discount_applied", "net_capital_gain", "losses_carried_forward", "assessable_investment_income",
		"est_tax_on_investment_income", "franking_and_foreign_offsets", "est_net_tax_payable_or_refund"})
	for _, y := range fys {
		_ = cw.Write([]string{y.FY, f(y.Franked, 2), f(y.Unfranked, 2), f(y.Franking, 2), f(y.ForeignTaxOffset, 2),
			f(y.DistCGDisc, 2), f(y.DistCGOther, 2), f(y.TaxDeferred, 2), f(y.GainsDisc, 2), f(y.GainsOther, 2),
			f(y.Losses, 2), f(y.LossBroughtFwd, 2), f(y.DiscountApplied, 2), f(y.NetCapGain, 2), f(y.LossCarriedFwd, 2),
			f(y.Assessable, 2), f(y.TaxOnTop, 2), f(y.Offsets, 2), f(y.NetPayable, 2)})
	}
	cw.Flush()
	return cw.Error()
}
