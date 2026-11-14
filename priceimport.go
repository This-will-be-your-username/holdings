package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Importing a price history (Vanguard's "Prices and distributions" export, or any date/price table, as .xlsx or
// CSV). Vanguard's export looks like:
//
//	Date         | Buy      | Sell     | NAV
//	12 Nov 2026  | $1.5673  | $1.5671  | $1.5673
//
// The NAV is used when there is one; otherwise a plain unit price, then the sell (exit) price, then the buy (entry)
// price. Dates may be text in any common format or real Excel dates.

type PriceImport struct {
	Prices        map[string]float64
	Column        string // which column the prices came from
	From, To      string
	Rows, Skipped int
}

var priceCols = []struct {
	label string
	re    *regexp.Regexp
}{
	{"NAV", regexp.MustCompile(`^(nav|nav price|nav per unit|net asset value)$`)},
	{"unit price", regexp.MustCompile(`^(unit price|price|unit value|closing price|mid price|close|last price)$`)},
	{"sell (exit) price", regexp.MustCompile(`^(sell|sell price|exit|exit price|redemption price|bid)$`)},
	{"buy (entry) price", regexp.MustCompile(`^(buy|buy price|entry|entry price|application price|ask|offer)$`)},
}

var reDateHeader = regexp.MustCompile(`^(date|as at|as at date|as of|price date|valuation date|effective date|nav date)$`)

func excelSerialToDate(v float64) (string, bool) {
	if v < 20000 || v > 80000 { // roughly 1954 to 2119
		return "", false
	}
	t := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(v))
	return t.Format(dateFmt), true
}

// ParsePriceHistory reads rows (from readTable or readXLSX) into date -> price.
func ParsePriceHistory(rows [][]string) (*PriceImport, error) {
	dateCol, priceCol, hdr := -1, -1, -1
	label := ""
	for i, r := range rows {
		if i > 30 {
			break
		}
		dc, best, bl := -1, 99, ""
		pc := -1
		for c, cell := range r {
			n := normHeader(cell)
			if reDateHeader.MatchString(n) && dc < 0 {
				dc = c
			}
			for rank, p := range priceCols {
				if p.re.MatchString(n) && rank < best {
					best, pc, bl = rank, c, p.label
				}
			}
		}
		if dc >= 0 && pc >= 0 {
			dateCol, priceCol, hdr, label = dc, pc, i, bl
			break
		}
	}
	start := hdr + 1
	if hdr < 0 { // no header: a date column followed by a price column
		for i, r := range rows {
			if i > 30 {
				break
			}
			if len(r) >= 2 {
				if _, ok := parseDateFlex(r[0], ""); ok {
					if _, ok2 := parseNumber(r[1], false); ok2 {
						dateCol, priceCol, start, label = 0, 1, i, "second column"
						break
					}
				}
			}
		}
		if dateCol < 0 {
			return nil, fmt.Errorf("couldn't find a date column and a price column (expected headings like Date and NAV, Unit price, Sell or Buy)")
		}
	}

	var texts []string
	for _, r := range rows[start:] {
		if dateCol < len(r) {
			texts = append(texts, r[dateCol])
		}
	}
	order, _ := detectDateOrder(texts)

	out := &PriceImport{Prices: map[string]float64{}, Column: label}
	for _, r := range rows[start:] {
		if dateCol >= len(r) || priceCol >= len(r) || r[dateCol] == "" {
			continue
		}
		d, ok := parseDateFlex(r[dateCol], order)
		if !ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(r[dateCol]), 64); err == nil {
				d, ok = excelSerialToDate(f)
			}
		}
		p, pok := parseNumber(r[priceCol], false)
		if !ok || !pok || p <= 0 {
			out.Skipped++
			continue
		}
		out.Prices[d] = p
		out.Rows++
		if out.From == "" || d < out.From {
			out.From = d
		}
		if d > out.To {
			out.To = d
		}
	}
	if out.Rows == 0 {
		return nil, fmt.Errorf("found the %s column but no readable date/price rows", label)
	}
	return out, nil
}

// ParsePriceFile accepts .xlsx or CSV/text bytes.
func ParsePriceFile(data []byte) (*PriceImport, error) {
	if isXLSX(data) {
		rows, err := readXLSX(data)
		if err != nil {
			return nil, err
		}
		return ParsePriceHistory(rows)
	}
	rows, _, err := readTable(data)
	if err != nil {
		return nil, fmt.Errorf("could not read the file as CSV: %v", err)
	}
	return ParsePriceHistory(rows)
}
