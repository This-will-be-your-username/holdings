package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Fingerprints let a transaction be recognised when it shows up again in a later
// upload, even from a different institution's export format.
//
// A fingerprint is a hash of the transaction's *interpreted* content (date, symbol,
// type, units, price and, for distributions, amount), not of the raw CSV text, so the
// same trade matches whatever the column order, date format or number formatting of
// the file it came from. Brokerage, FX rate and notes are left out on purpose:
// institutions disagree on those.
//
// Matching is count-aware: two genuinely identical trades on one day are two
// transactions, so an upload only dedupes as many copies as are already recorded.
// Where a file carries an institution reference (contract note number etc.), the
// reference decides: same reference = same transaction, different references = distinct.

const fpVersion = "v1"

var reNonAlnum = regexp.MustCompile(`[^A-Z0-9]`)

// normRef canonicalises an institution reference for comparison.
func normRef(s string) string {
	return reNonAlnum.ReplaceAllString(strings.ToUpper(s), "")
}

func r4(v float64) string { return fmt.Sprintf("%.4f", math.Round(v*1e4)/1e4+0) }
func r2(v float64) string { return fmt.Sprintf("%.2f", math.Round(v*1e2)/1e2+0) }

// canonical returns the fields the fingerprint is made from.
func canonical(t Txn) string {
	units, price, amt := t.Units, t.Price, 0.0
	switch t.Type {
	case "dividend":
		units, price, amt = 0, 0, t.Amount
	case "drp":
		amt = t.Amount
		if amt <= 0 {
			amt = t.Units * t.Price
		}
	}
	return strings.Join([]string{fpVersion, t.Date, strings.ToUpper(strings.TrimSpace(t.Symbol)), t.Type,
		r4(units), r4(price), r2(amt)}, "|")
}

// Fingerprint is a short stable hash of the transaction's content.
func Fingerprint(t Txn) string {
	sum := sha256.Sum256([]byte(canonical(t)))
	return hex.EncodeToString(sum[:])[:24]
}

// looseMatch reports whether two transactions look like the same event reported with
// small differences: dates a few days apart (trade vs settlement date), or a price or
// amount rounded differently. These are flagged for the user rather than skipped.
func looseMatch(a, b Txn) bool {
	if a.Symbol != b.Symbol || a.Type != b.Type {
		return false
	}
	if ra, rb := normRef(a.Ref), normRef(b.Ref); ra != "" && rb != "" && ra != rb {
		return false // different reference numbers: different transactions
	}
	da, e1 := time.Parse(dateFmt, a.Date)
	db, e2 := time.Parse(dateFmt, b.Date)
	if e1 != nil || e2 != nil {
		return false
	}
	gap := da.Sub(db).Abs()
	near := func(x, y, tol float64) bool { return math.Abs(x-y) <= tol*math.Max(math.Abs(x), math.Abs(y))+0.005 }
	switch a.Type {
	case "dividend":
		return gap <= 5*24*time.Hour && near(a.Amount, b.Amount, 0.02)
	case "drp":
		return gap <= 5*24*time.Hour && near(a.Units, b.Units, 0.005)
	default: // buy, sell
		return gap <= 3*24*time.Hour && near(a.Units, b.Units, 0.005) && near(a.Price, b.Price, 0.02)
	}
}

type DupMatch struct {
	Class    string // new | dup | possible
	Existing int    // index into the existing slice, -1 for new
	Why      string
}

// ClassifyDuplicates compares incoming transactions with those already recorded.
//   - dup:      already recorded (same reference, or same fingerprint) - skip it
//   - possible: probably the same event with slightly different details - ask the user
//   - new:      not seen before
func ClassifyDuplicates(existing, incoming []Txn) []DupMatch {
	used := make([]bool, len(existing))
	byRef := map[string][]int{}
	byFP := map[string][]int{}
	for i, e := range existing {
		byFP[Fingerprint(e)] = append(byFP[Fingerprint(e)], i)
		if r := normRef(e.Ref); r != "" {
			k := r + "|" + e.Symbol
			byRef[k] = append(byRef[k], i)
		}
	}
	out := make([]DupMatch, len(incoming))
	for k := range out {
		out[k] = DupMatch{Class: "new", Existing: -1}
	}

	// 1) institution references are the strongest signal
	for k, in := range incoming {
		r := normRef(in.Ref)
		if r == "" {
			continue
		}
		for _, i := range byRef[r+"|"+in.Symbol] {
			if used[i] || existing[i].Type != in.Type {
				continue
			}
			used[i] = true
			if Fingerprint(existing[i]) == Fingerprint(in) {
				out[k] = DupMatch{"dup", i, "same reference number and details"}
			} else {
				out[k] = DupMatch{"possible", i, "same reference number but the details differ"}
			}
			break
		}
	}
	// 2) identical content, one existing copy consumed per incoming copy
	for k, in := range incoming {
		if out[k].Class != "new" {
			continue
		}
		ir := normRef(in.Ref)
		for _, i := range byFP[Fingerprint(in)] {
			if used[i] {
				continue
			}
			if er := normRef(existing[i].Ref); er != "" && ir != "" && er != ir {
				continue // both carry references and they differ: two separate transactions
			}
			used[i] = true
			out[k] = DupMatch{"dup", i, "identical transaction already recorded"}
			break
		}
	}
	// 3) near matches, for the user to decide
	for k, in := range incoming {
		if out[k].Class != "new" {
			continue
		}
		for i, e := range existing {
			if !used[i] && looseMatch(e, in) {
				used[i] = true
				out[k] = DupMatch{"possible", i, "similar transaction recorded (date or amount differs slightly)"}
				break
			}
		}
	}
	return out
}
