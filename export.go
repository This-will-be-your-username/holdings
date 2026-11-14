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
	"franked_aud", "franking_credits_aud", "foreign_tax", "cg_discount_aud", "cg_other_aud", "tax_deferred_aud", "tax_year",
	"source_ref", "fingerprint"}

func f(v float64, dp int) string { return strconv.FormatFloat(v, 'f', dp, 64) }

// WriteTransactionsCSV exports every transaction with the AUD values the
// performance numbers are derived from, so results can be re-checked in a
// spreadsheet. Re-uploading this file is safe: the fingerprint and source_ref columns let the importer recognise every row.
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
			r.Ref, firstNonEmpty(r.FP, Fingerprint(r.Txn)),
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
	case "buy", "sell", "drp", "award":
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
		return fmt.Errorf("type must be buy, sell, award, dividend or drp")
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

func validFY(s string) bool {
	var y, n int
	if _, err := fmt.Sscanf(s, "%4d-%2d", &y, &n); err != nil || len(s) != 7 {
		return false
	}
	return y >= 1990 && y < 2100 && n == (y+1)%100
}
