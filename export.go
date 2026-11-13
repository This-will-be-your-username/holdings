package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

var txnHeader = []string{"date", "symbol", "name", "market", "type", "units", "price", "currency",
	"fees_aud", "amount", "fx_to_aud", "value_aud", "cash_flow_aud", "units_after",
	"avg_cost_aud_after", "realised_gain_aud", "notes",
	"franked_aud", "franking_credits_aud", "foreign_tax", "cg_discount_aud", "cg_other_aud", "tax_deferred_aud", "tax_year"}

func f(v float64, dp int) string { return strconv.FormatFloat(v, 'f', dp, 64) }

// WriteTransactionsCSV exports every transaction with the AUD values the
// performance numbers are derived from, so results can be re-checked in a
// spreadsheet. The leading columns match what ImportCSV reads (round-trips).
func WriteTransactionsCSV(w io.Writer, res *Result) error {
	cw := csv.NewWriter(w)
	_ = cw.Write(txnHeader)
	for _, r := range res.Rows {
		_ = cw.Write([]string{
			r.Date, r.Symbol, r.Name, r.Market, r.Type,
			f(r.Units, 6), f(r.Price, 6), r.Currency,
			f(r.Fees, 2), f(r.Amount, 2), f(r.FXUsed, 6),
			f(r.GrossAUD, 2), f(r.CashFlowAUD, 2), f(r.UnitsAfter, 6),
			f(r.AvgCostAfter, 4), f(r.Realised, 2), r.Notes,
			f(r.Franked, 2), f(r.Franking, 2), f(r.ForeignTax, 2), f(r.CGDisc, 2), f(r.CGOther, 2), f(r.TaxDeferred, 2), r.TaxYear,
		})
	}
	cw.Flush()
	return cw.Error()
}

func WriteHoldingsCSV(w io.Writer, res *Result) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"symbol", "name", "market", "currency", "status", "units", "avg_cost_aud", "cost_basis_aud",
		"price", "price_date", "fx_now", "value_aud", "unrealised_aud", "realised_aud", "income_aud",
		"invested_aud", "total_return_aud", "total_return_pct", "xirr_pct"})
	all := append(append([]*Holding{}, res.Holdings...), res.Closed...)
	all = append(all, res.Total)
	for _, h := range all {
		status := "open"
		if h.Closed {
			status = "closed"
		}
		x := ""
		if h.HasXIRR {
			x = f(h.XIRR, 2)
		}
		_ = cw.Write([]string{h.Symbol, h.Name, h.Market, h.Currency, status,
			f(h.Units, 6), f(h.AvgCost, 4), f(h.CostAUD, 2), f(h.Price, 4), h.PriceDate, f(h.FXNow, 6),
			f(h.ValueAUD, 2), f(h.Unrealised, 2), f(h.Realised, 2), f(h.Income, 2),
			f(h.Invested, 2), f(h.TotalReturn, 2), f(h.ReturnPct, 2), x})
	}
	cw.Flush()
	return cw.Error()
}

func parseDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{dateFmt, "2/1/2006", "02/01/2006", "2-Jan-2006", "2 Jan 2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(dateFmt), nil
		}
	}
	return "", fmt.Errorf("bad date %q (use YYYY-MM-DD or DD/MM/YYYY)", s)
}

func pnum(s string) float64 {
	s = strings.NewReplacer(",", "", "$", "", " ", "").Replace(strings.TrimSpace(s))
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func validateTxn(t *Txn) error {
	switch t.Type {
	case "buy", "sell", "drp":
		if t.Units <= 0 {
			return fmt.Errorf("units must be > 0")
		}
		if t.Price <= 0 && !(t.Type == "drp" && t.Amount > 0) {
			return fmt.Errorf("price must be > 0")
		}
	case "dividend":
		if t.Amount <= 0 {
			return fmt.Errorf("dividend amount must be > 0")
		}
	default:
		return fmt.Errorf("type must be buy, sell, dividend or drp")
	}
	if t.Fees < 0 || t.FX < 0 {
		return fmt.Errorf("fees and FX rate can't be negative")
	}
	if t.Type == "dividend" || t.Type == "drp" {
		if t.Franked < 0 || t.Franking < 0 || t.ForeignTax < 0 || t.CGDisc < 0 || t.CGOther < 0 || t.TaxDeferred < 0 {
			return fmt.Errorf("tax components can't be negative")
		}
		amt := t.Amount
		if amt <= 0 {
			amt = t.Units * t.Price
		}
		if t.Franked > amt+0.005 {
			return fmt.Errorf("franked amount (%.2f) can't exceed the distribution (%.2f)", t.Franked, amt)
		}
		if t.Franked+t.CGDisc+t.CGOther+t.TaxDeferred > amt+0.005 {
			return fmt.Errorf("franked + capital gain + tax-deferred components (%.2f) exceed the distribution (%.2f)",
				t.Franked+t.CGDisc+t.CGOther+t.TaxDeferred, amt)
		}
		if t.TaxYear != "" && !validFY(t.TaxYear) {
			return fmt.Errorf("tax year should look like 2025-26")
		}
	} else if t.Franked != 0 || t.Franking != 0 || t.ForeignTax != 0 || t.CGDisc != 0 || t.CGOther != 0 || t.TaxDeferred != 0 || t.TaxYear != "" {
		return fmt.Errorf("tax details only apply to distributions")
	}
	return nil
}

// ImportCSV parses a CSV (header-driven) into transactions. Columns:
// date, symbol, market (ASX|US|MANAGED), type, units, price, fees|fees_aud,
// amount, fx|fx_to_aud, notes, name. Extra columns are ignored.
func ImportCSV(r io.Reader) ([]Txn, map[string][2]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(recs) < 2 {
		return nil, nil, fmt.Errorf("CSV needs a header row and at least one data row")
	}
	idx := map[string]int{}
	for i, h := range recs[0] {
		idx[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	get := func(rec []string, names ...string) string {
		for _, n := range names {
			if i, ok := idx[n]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
		}
		return ""
	}
	for _, need := range []string{"date", "symbol", "type"} {
		if _, ok := idx[need]; !ok {
			return nil, nil, fmt.Errorf("missing required column %q", need)
		}
	}
	var out []Txn
	meta := map[string][2]string{} // symbol -> market,name
	for n, rec := range recs[1:] {
		line := n + 2
		if len(strings.Join(rec, "")) == 0 {
			continue
		}
		d, err := parseDate(get(rec, "date"))
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %v", line, err)
		}
		market := strings.ToUpper(get(rec, "market"))
		sym := strings.ToUpper(get(rec, "symbol"))
		if market == "" {
			if strings.HasSuffix(sym, ".AX") {
				market = MarketASX
			} else {
				market = MarketUS
			}
		}
		if market != MarketASX && market != MarketUS && market != MarketManaged {
			return nil, nil, fmt.Errorf("line %d: market must be ASX, US or MANAGED", line)
		}
		sym = normSymbol(sym, market)
		typ := strings.ToLower(get(rec, "type"))
		switch typ {
		case "distribution", "div", "income":
			typ = "dividend"
		case "reinvest", "reinvestment":
			typ = "drp"
		}
		t := Txn{
			ID: newID(), Date: d, Symbol: sym, Type: typ,
			Units: pnum(get(rec, "units")), Price: pnum(get(rec, "price")),
			Fees: pnum(get(rec, "fees_aud", "fees")), Amount: pnum(get(rec, "amount")),
			FX: pnum(get(rec, "fx_to_aud", "fx")), Notes: get(rec, "notes"),
			Franked: pnum(get(rec, "franked_aud", "franked")), Franking: pnum(get(rec, "franking_credits_aud", "franking")),
			ForeignTax: pnum(get(rec, "foreign_tax")), CGDisc: pnum(get(rec, "cg_discount_aud", "cg_discount", "cg_disc")),
			CGOther: pnum(get(rec, "cg_other_aud", "cg_other")), TaxDeferred: pnum(get(rec, "tax_deferred_aud", "tax_deferred")),
			TaxYear: get(rec, "tax_year"),
		}
		if err := validateTxn(&t); err != nil {
			return nil, nil, fmt.Errorf("line %d: %v", line, err)
		}
		out = append(out, t)
		meta[sym] = [2]string{market, get(rec, "name")}
	}
	return out, meta, nil
}

func validFY(s string) bool {
	var y, n int
	if _, err := fmt.Sscanf(s, "%4d-%2d", &y, &n); err != nil || len(s) != 7 {
		return false
	}
	return y >= 1990 && y < 2100 && n == (y+1)%100
}

func sameTxn(a, b Txn) bool {
	const e = 1e-6
	abs := func(x float64) float64 {
		if x < 0 {
			return -x
		}
		return x
	}
	return a.Date == b.Date && a.Symbol == b.Symbol && a.Type == b.Type &&
		abs(a.Units-b.Units) < e && abs(a.Price-b.Price) < e && abs(a.Amount-b.Amount) < 0.005
}
