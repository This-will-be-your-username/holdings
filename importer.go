package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A flexible CSV reader for broker / fund-manager exports.
//
// Nothing here assumes a particular institution's layout. The importer:
//  1. sniffs the delimiter and finds the header row (statements often have title lines),
//  2. scores every column against every field using header-name regexes AND the content
//     of the values (does it look like dates? tickers? buy/sell words? numbers?),
//  3. interprets each row: day-first vs month-first dates, signed quantities, "B/S" or
//     "Application/Redemption" actions, brokerage + GST, tickers written many ways,
//     currency and FX orientation, franking components, and so on,
//  4. returns the detected mapping so the user can correct anything it got wrong.

type Field string

const (
	FDate        Field = "date"
	FSettle      Field = "settle"
	FType        Field = "type"
	FSymbol      Field = "symbol"
	FName        Field = "name"
	FDesc        Field = "desc"
	FUnits       Field = "units"
	FPrice       Field = "price"
	FAmount      Field = "amount" // gross trade value / gross distribution
	FTotal       Field = "total"  // total cash including fees / net amount
	FFees        Field = "fees"
	FGST         Field = "gst"
	FCurrency    Field = "currency"
	FFX          Field = "fx"
	FMarket      Field = "market"
	FRef         Field = "ref"
	FNotes       Field = "notes"
	FFranked     Field = "franked"
	FUnfranked   Field = "unfranked"
	FFranking    Field = "franking"
	FForeignTax  Field = "foreign_tax"
	FCGDisc      Field = "cg_disc"
	FCGOther     Field = "cg_other"
	FTaxDeferred Field = "tax_deferred"
	FTaxYear     Field = "tax_year"
)

var fieldOrder = []Field{FDate, FSettle, FType, FSymbol, FName, FDesc, FUnits, FPrice, FAmount, FTotal, FFees, FGST,
	FCurrency, FFX, FMarket, FRef, FNotes, FFranked, FUnfranked, FFranking, FForeignTax, FCGDisc, FCGOther, FTaxDeferred, FTaxYear}

var fieldLabel = map[Field]string{
	FDate: "Trade date", FSettle: "Settlement date (fallback)", FType: "Action (buy / sell / distribution)",
	FSymbol: "Ticker / code", FName: "Name", FDesc: "Description (read action, units, price from text)",
	FUnits: "Units / quantity", FPrice: "Unit price", FAmount: "Gross value / gross distribution",
	FTotal: "Total / net amount", FFees: "Brokerage / fees", FGST: "GST (if separate)", FCurrency: "Currency",
	FFX: "FX rate", FMarket: "Market / exchange", FRef: "Reference (contract note / transaction id)", FNotes: "Notes",
	FFranked: "Franked amount", FUnfranked: "Unfranked amount", FFranking: "Franking credits",
	FForeignTax: "Foreign / withholding tax", FCGDisc: "Capital gain (discountable)", FCGOther: "Capital gain (other)",
	FTaxDeferred: "Tax-deferred / return of capital", FTaxYear: "Tax year",
}

// Fields needed before a file can be imported, shown to the user as essential.
var essentialFields = map[Field]bool{FDate: true, FSymbol: true}

// ---------- header recognition ----------

type hp struct {
	f  Field
	w  int
	re *regexp.Regexp
}

func hpat(f Field, w int, re string) hp { return hp{f, w, regexp.MustCompile(`^(?:` + re + `)$`)} }

var headerPats = []hp{
	hpat(FDate, 100, `trade date( time)?|date traded|trade dt|execution date|exec date|order date|deal date`),
	hpat(FDate, 95, `(?:transaction|txn|trans|contract|business|activity|booking|processed|process|posting|posted|post) ?date( time)?`),
	hpat(FDate, 90, `date|date time|datetime|dt|time`),
	hpat(FDate, 72, `(?:payment|pay|paid|distribution|dividend|payout|entitlement) date`),
	hpat(FDate, 30, `(?:ex|record|ex dividend) date`),
	hpat(FSettle, 100, `settle(?:ment)? ?date|sett(?:le)? dt|date settled|value date|settled`),

	hpat(FSymbol, 100, `(?:asx |stock |security |share |instrument |investment |product |fund |sec |ticker |market )?(?:code|symbol|ticker)(?: symbol)?`),
	hpat(FSymbol, 92, `asx code|asx ticker|ticker symbol|ric|bloomberg(?: ticker)?|apir(?: code)?`),
	hpat(FSymbol, 60, `security|stock|instrument|investment|product|asset|holding|isin`),
	hpat(FName, 90, `company(?: name)?|security name|stock name|instrument name|investment name|fund name|product name|asset name|name|issuer|security description|description of security|stock description|instrument description`),
	hpat(FName, 50, `security|stock|instrument|investment|product|asset|holding|fund|fund description|scheme`),
	hpat(FDesc, 55, `description|details|narrative|particulars|transaction description|memo|transaction details|activity description`),

	hpat(FType, 100, `transaction type|trans type|txn type|trade type|activity type|buy sell|buy sell indicator|buy or sell|action|side|b s|bs|trade action|order action|order side`),
	hpat(FType, 85, `type|direction|operation|activity|event|transaction|kind|category`),

	hpat(FUnits, 100, `units|quantity|qty|no of units|number of units|no of shares|number of shares|shares|volume|filled quantity|filled qty|executed quantity|exec qty|securities|units quantity|unit quantity|trade quantity|share quantity`),
	hpat(FPrice, 100, `unit price|price|avg price|average price|trade price|execution price|exec price|price per unit|price per share|share price|fill price|avg trade price|average trade price|tradeprice|unit cost|cost per share|cost per unit`),
	hpat(FPrice, 55, `average cost|rate|net price`),

	hpat(FAmount, 95, `trade value|gross amount|gross value|gross consideration|consideration|gross proceeds|trade amount|value of trade|transaction value|gross payment|gross dividend|gross distribution|total dividend|dividend amount|distribution amount`),
	hpat(FTotal, 90, `total|total value|total amount|total cost|total proceeds|total consideration|net amount|net value|net proceeds|net payment|net dividend|net distribution|settlement amount|cash amount|net cash|net total|amount|value|payment|credit|debit|credit debit|debit credit|amount paid|amount received`),

	hpat(FFees, 95, `brokerage|commission|commissions|fees|fee|charges|charge|total fees|total charges|trading fees?|transaction fees?|brokerage fee|brokerage commission|brokerage (?:incl|inc|and|plus) gst|brokerage gst|ibcommission|costs|cost`),
	hpat(FGST, 90, `gst|gst amount|vat|gst on brokerage`),

	hpat(FCurrency, 95, `currency|ccy|curr|currency primary|trade currency|price currency|settlement currency|currency code|cur`),
	hpat(FFX, 95, `fx|fx rate|fx to|fx rate to|exchange rate|exch rate|forex rate|forex|conversion rate|rate to|aud rate|currency rate|fx conversion rate`),
	hpat(FMarket, 90, `market|exchange|exch|venue|listing exchange|primary exchange|mkt|market exchange|exchange code`),
	hpat(FRef, 95, `contract note(?: no| number| ref| reference| id)?|cn no|cn number|reference(?: no| number| id)?|ref(?: no| number| id)?|source ref(?:erence)?|transaction id|transaction no|transaction number|trade id|trade no|trade number|trade ref|order id|order no|order number|confirmation(?: no| number| id)?|txn id|exec id|execution id|document no|document number|receipt(?: no| number)?|broker ref|statement ref|id`),
	hpat(FNotes, 80, `notes?|comments?|remarks?`),

	hpat(FFranked, 95, `franked|franked amount|franked dividend|franked dividends|franked amt|franked distribution`),
	hpat(FUnfranked, 95, `unfranked|unfranked amount|unfranked dividend|unfranked dividends|unfranked amt|unfranked distribution`),
	hpat(FFranking, 95, `franking credits?|franking credit amount|imputation credits?|franking|franking tax credit|franking credits received|franking credit amt`),
	hpat(FForeignTax, 95, `withholding tax|withholding|foreign tax|foreign tax withheld|foreign tax paid|tax withheld|foreign income tax offset|fito|nra tax|us tax withheld|foreign withholding tax`),
	hpat(FCGDisc, 95, `discounted capital gain|discount capital gain|capital gain discount(?:ed)?|cgt concession amount|cg discount|capital gains discount|cg discountable`),
	hpat(FCGOther, 95, `other capital gain|capital gain other|capital gains other|cg other|non discount capital gain|non discounted capital gain`),
	hpat(FTaxDeferred, 95, `tax deferred|tax deferred amount|return of capital|non assessable|non assessable amount|tax free|tax free amount|cost base reduction|tax exempted`),
	hpat(FTaxYear, 90, `tax year|income year|financial year|fy`),
}

var (
	reCamel   = regexp.MustCompile(`(\p{Ll})(\p{Lu})`)
	reSplit   = regexp.MustCompile(`[^a-z0-9]+`)
	reCcyTail = regexp.MustCompile(`( (?:aud|usd|nzd|gbp|eur|a|us))+$`)
)

// normHeader lowercases, splits camelCase, strips punctuation and trailing currency tags:
// "Brokerage+GST ($)" -> "brokerage plus gst", "TradePrice" -> "trade price".
func normHeader(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
	h = reCamel.ReplaceAllString(h, "$1 $2")
	h = strings.ToLower(h)
	h = strings.ReplaceAll(h, "&", " and ")
	h = strings.ReplaceAll(h, "+", " plus ")
	h = strings.TrimSpace(reSplit.ReplaceAllString(h, " "))
	return reCcyTail.ReplaceAllString(h, "")
}

// headerScores returns the best weight each field gets from a header cell.
func headerScores(h string) map[Field]int {
	n := normHeader(h)
	out := map[Field]int{}
	if n == "" {
		return out
	}
	for _, p := range headerPats {
		if p.re.MatchString(n) && p.w > out[p.f] {
			out[p.f] = p.w
		}
	}
	return out
}

// ---------- values: numbers, dates, types, symbols ----------

var (
	reNumJunk       = regexp.MustCompile(`(?i)(a\$|us\$|nz\$|\$|aud|usd|nzd|gbp|eur|€|£|\s|\x{00a0}|')`)
	reThousandsComm = regexp.MustCompile(`^\d{1,3}(,\d{3})+$`)
	reDecComma      = regexp.MustCompile(`^\d+,\d{1,}$`)
)

// parseNumber reads money/quantity text: $1,234.50  (1,234.50)  1.234,50  12.5-  1234.5CR  A$12
// decimalComma hints that "12,5" means 12.5 (European exports, usually semicolon separated).
func parseNumber(s string, decimalComma bool) (float64, bool) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "-", "--", "n/a", "na", "null", "nan":
		return 0, false
	}
	neg := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		neg, s = true, s[1:len(s)-1]
	}
	if u := strings.ToUpper(s); strings.HasSuffix(u, "DR") {
		neg, s = true, s[:len(s)-2]
	} else if strings.HasSuffix(u, "CR") {
		s = s[:len(s)-2]
	}
	s = reNumJunk.ReplaceAllString(s, "")
	s = strings.TrimSuffix(s, "%")
	if strings.HasSuffix(s, "-") {
		neg, s = true, s[:len(s)-1]
	}
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	s = strings.TrimPrefix(s, "+")
	hasDot, hasComma := strings.Contains(s, "."), strings.Contains(s, ",")
	switch {
	case hasDot && hasComma:
		if strings.LastIndex(s, ".") > strings.LastIndex(s, ",") {
			s = strings.ReplaceAll(s, ",", "")
		} else {
			s = strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", ".")
		}
	case hasComma:
		if reThousandsComm.MatchString(s) && !decimalComma {
			s = strings.ReplaceAll(s, ",", "")
		} else if decimalComma || reDecComma.MatchString(s) && strings.Count(s, ",") == 1 {
			s = strings.ReplaceAll(s, ",", ".")
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case hasDot && strings.Count(s, ".") > 1:
		s = strings.ReplaceAll(s, ".", "")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	if neg {
		v = -v
	}
	return v, true
}

var (
	reTimeTail = regexp.MustCompile(`(?i)[T\s]+\d{1,2}:\d{2}(:\d{2})?(\.\d+)?\s*(am|pm)?\s*(z|utc|gmt|[+-]\d{2}:?\d{2}|[a-z]{3,4})?$`)
	reOrdinal  = regexp.MustCompile(`(?i)(\d)(st|nd|rd|th)\b`)

	reCompactTime = regexp.MustCompile(`[;_]\d{4,6}$`)
	reYMD8        = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})$`)
	reNumDate     = regexp.MustCompile(`^(\d{1,4})[/\-. ](\d{1,2})[/\-. ](\d{1,4})$`)
)

var nameLayouts = []string{"2 Jan 2006", "2 January 2006", "02 Jan 2006", "2-Jan-2006", "02-Jan-06", "2-Jan-06", "Jan 2, 2006",
	"January 2, 2006", "Jan 2 2006", "2006-Jan-02", "Mon, 2 Jan 2006", "Monday, 2 January 2006", "2 Jan, 2006", "Jan-02-2006",
	"02Jan2006", "2Jan2006", "Mon 2 Jan 2006", "2 Jan 06", "January 2 2006"}

func mkDate(y, m, d int) (string, bool) {
	if y < 100 {
		if y < 70 {
			y += 2000
		} else {
			y += 1900
		}
	}
	if y < 1970 || y > 2100 || m < 1 || m > 12 || d < 1 || d > 31 {
		return "", false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d { // e.g. 31 Feb rolled over
		return "", false
	}
	return t.Format(dateFmt), true
}

// parseDateFlex reads a date in many formats. order resolves numeric ambiguity:
// DMY (Australian default), MDY (US) or YMD; "auto"/"" guesses per value (day-first unless impossible).
func parseDateFlex(s, order string) (string, bool) {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	if s == "" {
		return "", false
	}
	s = strings.TrimSpace(reTimeTail.ReplaceAllString(s, ""))
	s = reCompactTime.ReplaceAllString(s, "") // "20250304;093015" (IBKR) -> "20250304"
	s = reOrdinal.ReplaceAllString(s, "$1")
	if m := reYMD8.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return mkDate(y, mo, d)
	}
	if m := reNumDate.FindStringSubmatch(s); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		c, _ := strconv.Atoi(m[3])
		switch {
		case len(m[1]) == 4: // YYYY-MM-DD
			return mkDate(a, b, c)
		case len(m[3]) == 4 || len(m[3]) == 2:
			switch order {
			case "MDY":
				return mkDate(c, a, b)
			case "DMY":
				return mkDate(c, b, a)
			default:
				if a > 12 {
					return mkDate(c, b, a)
				}
				if b > 12 {
					return mkDate(c, a, b)
				}
				return mkDate(c, b, a)
			}
		}
		return "", false
	}
	for _, l := range nameLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format(dateFmt), true
		}
	}
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format(dateFmt), true
		}
	}
	return "", false
}

// detectDateOrder looks across a whole column: any "25/03/2024" proves day-first, any
// "03/25/2024" proves month-first, 4-digit-first means year-first. Fully ambiguous -> day-first.
func detectDateOrder(vals []string) (order, note string) {
	var dmy, mdy, ymd int
	for _, v := range vals {
		v = strings.TrimSpace(reTimeTail.ReplaceAllString(strings.TrimSpace(v), ""))
		m := reNumDate.FindStringSubmatch(v)
		if m == nil {
			continue
		}
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[2])
		switch {
		case len(m[1]) == 4:
			ymd++
		case a > 12:
			dmy++
		case b > 12:
			mdy++
		}
	}
	switch {
	case ymd > 0 && dmy == 0 && mdy == 0:
		return "YMD", "dates are year-first (YYYY-MM-DD)"
	case dmy > 0 && mdy == 0:
		return "DMY", "dates are day-first (DD/MM/YYYY) - confirmed by days above 12"
	case mdy > 0 && dmy == 0:
		return "MDY", "dates are month-first (MM/DD/YYYY) - confirmed by days above 12"
	case dmy > 0 && mdy > 0:
		return "DMY", "dates are mixed day/month-first; assuming day-first"
	}
	return "DMY", "dates are ambiguous (all day and month values are 12 or below); assuming day-first (DD/MM/YYYY) - change it if that's wrong"
}

var (
	reTaxLine = regexp.MustCompile(`(?i)^(non[- ]resident |us |foreign )?(withh?olding )?tax( withh?eld| paid| deducted)?\b|^nra\b|^withh?olding\b`)
	reAward   = regexp.MustCompile(`(?i)\brsus?\b|\brsas?\b|\breleas(e|ed|es)\b|\bvest(ed|s|ing)?\b|\bawards?(ed)?\b|stock award|share award|equity award|performance rights? (vest|releas)`)
	reDRP     = regexp.MustCompile(`(?i)reinvest|\bdrp\b|\bdrip\b|dividend plan|distribution plan`)
	reDiv     = regexp.MustCompile(`(?i)dividend|distribution|\bdivs?\b|payout|franked|\bcoupon\b|^income\b`)
	reSell    = regexp.MustCompile(`(?i)^(s|sl|sell|sold|sale|sales|redemption|redeem|redeemed|disposal|disposed|short sell|units withdrawn|switch out|sell to close)\b`)
	reBuy     = regexp.MustCompile(`(?i)^(b|bu|buy|bought|purchase|purchased|acquisition|application|applic|subscription|investment|contribution|switch in|buy to open|units issued|new units|trade in)\b`)
	reIgnore  = regexp.MustCompile(`(?i)deposit|withdraw|transfer|interest|\bfees?\b|charge|account|\btax\b|\bgst\b|adjust|opening|closing|balance|journal|\bfx\b|currency|conversion|split|consolidat|merger|spin|rights|bonus|corporate action|capital return|buy ?back|\bcash\b|statement|settlement`)
)

// classifyType maps the wording institutions use onto buy / sell / dividend / drp.
// ignore=true means a row that is real but isn't an investment transaction (deposit, fee, ...).
func classifyType(s string) (typ string, ignore, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, false
	}
	switch {
	case reTaxLine.MatchString(s):
		return "", true, true
	case reAward.MatchString(s):
		return "award", false, true
	case reDRP.MatchString(s):
		return "drp", false, true
	case reDiv.MatchString(s):
		return "dividend", false, true
	case reSell.MatchString(s):
		return "sell", false, true
	case reBuy.MatchString(s):
		return "buy", false, true
	case reIgnore.MatchString(s):
		return "", true, true
	}
	// "Market Buy", "Limit Sell", "Bought 100 VAS" - action word anywhere
	words := strings.Fields(strings.ToLower(s))
	for _, w := range words {
		switch strings.Trim(w, ".,:;") {
		case "buy", "bought", "purchase":
			return "buy", false, true
		case "sell", "sold", "sale":
			return "sell", false, true
		}
	}
	return "", false, false
}

var (
	reISINAU   = regexp.MustCompile(`^AU0+([A-Z0-9]{3,5})\d$`)
	reBBG      = regexp.MustCompile(`^([A-Z0-9]{1,6})\s+(AU|AX|US|UQ|UN|UW|UA)(?:\s+EQUITY)?$`)
	rePrefixEx = regexp.MustCompile(`^(ASX|XASX|NASDAQ|NYSE|NYSEARCA|NYSEMKT|AMEX|ARCA|BATS|XNAS|XNYS)\s*[:\-/ ]\s*([A-Z0-9.]{1,6})$`)
	reSuffixAU = regexp.MustCompile(`^([A-Z0-9]{1,6})[.\-](AX|ASX|AU|XA|CHA)$`)
	reSuffixUS = regexp.MustCompile(`^([A-Z][A-Z0-9]{0,4})[.\-](US|O|N|OQ|K|P|A)$`)
	reAPIR     = regexp.MustCompile(`\b[A-Z]{3}\d{4}AU\b`)
	reParenSym = regexp.MustCompile(`\(([A-Z][A-Z0-9]{1,5})\)`)
	reCodeText = regexp.MustCompile(`(?i)\b(?:ASX|CODE|TICKER)[:\s]+([A-Z0-9]{3,5})\b`)
	reBare     = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,5}(\.[A-Z])?$`)
)

var ccyWords = map[string]bool{"AUD": true, "USD": true, "NZD": true, "GBP": true, "EUR": true, "CAD": true, "JPY": true, "HKD": true, "SGD": true, "CHF": true}

// marketFromText maps an exchange name to ASX / US ("" if unrecognised).
func marketFromText(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	switch {
	case s == "":
		return ""
	case s == "MANAGED" || s == "UNLISTED" || s == "MANAGED FUND" || s == "UNIT TRUST":
		return MarketManaged
	case regexp.MustCompile(`^(ASX|XASX|AU|AUS|AUSTRALIA|AX|CXA|CHI-?X|XA|ASX LIMITED|AUSTRALIAN SECURITIES EXCHANGE)$`).MatchString(s):
		return MarketASX
	case regexp.MustCompile(`^(NASDAQ|NYSE|NYSEARCA|NYSEMKT|ARCA|AMEX|BATS|US|USA|NMS|NYQ|NGM|NCM|PCX|XNAS|XNYS|ARCX|UNITED STATES|NASDAQ GS|NASDAQGS|NASDAQ GM|SMART|NYSE ARCA|OTC)$`).MatchString(s):
		return MarketUS
	}
	return ""
}

type symCtx struct{ market, ccy, def string }

// parseSymbol extracts a ticker and market from the many ways instruments are written:
// VAS, VAS.AX, ASX:VAS, "VAS AU Equity", NASDAQ:AAPL, AAPL.US, VAN0002AU (APIR), AU000000VAS1 (ISIN),
// and "Vanguard Australian Shares Index ETF (VAS)".
func parseSymbol(raw string, c symCtx) (sym, market string, ok bool) {
	s := strings.ToUpper(strings.TrimSpace(strings.Trim(raw, `"'`)))
	if s == "" {
		return
	}
	bare := func(code string, hint string) (string, string, bool) {
		m := hint
		if m == "" {
			m = c.market
		}
		if m == "" {
			if strings.EqualFold(c.ccy, "USD") {
				m = MarketUS
			} else {
				m = c.def
			}
		}
		if m == "" {
			m = MarketASX
		}
		return code, m, true
	}
	if m := reISINAU.FindStringSubmatch(s); m != nil {
		return m[1], MarketASX, true
	}
	if m := reBBG.FindStringSubmatch(s); m != nil {
		if m[2] == "AU" || m[2] == "AX" {
			return m[1], MarketASX, true
		}
		return m[1], MarketUS, true
	}
	if m := rePrefixEx.FindStringSubmatch(s); m != nil {
		if mk := marketFromText(m[1]); mk != "" {
			return m[2], mk, true
		}
	}
	if m := reSuffixAU.FindStringSubmatch(s); m != nil {
		return m[1], MarketASX, true
	}
	if m := reSuffixUS.FindStringSubmatch(s); m != nil {
		return m[1], MarketUS, true
	}
	if reAPIR.MatchString(s) && len(s) == 9 {
		return s, MarketManaged, true
	}
	if m := reParenSym.FindStringSubmatch(s); m != nil {
		return bare(m[1], "")
	}
	if m := reCodeText.FindStringSubmatch(s); m != nil {
		return bare(m[1], "")
	}
	if reBare.MatchString(s) && !ccyWords[s] {
		return bare(s, "")
	}
	return "", "", false
}

// embedded trade details inside a free-text description: "B 100 VAS @ 95.10", "Bought 50 AAPL at $190"
var reDesc = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*(?P<act>buy|bought|sell|sold|b|s)\s+(?P<qty>[\d,.]+)\s+(?P<sym>[A-Z0-9.]{1,8})\s*(?:@|at)\s*\$?(?P<px>[\d,.]+)`),
	regexp.MustCompile(`(?i)(?P<act>bought|sold|buy|sell)\s+(?P<qty>[\d,.]+)\s+(?:shares?\s+of\s+|units?\s+of\s+)?(?P<sym>[A-Z0-9.]{1,8})\b.*?(?:@|at)\s*\$?(?P<px>[\d,.]+)`),
	regexp.MustCompile(`(?i)(?P<qty>[\d,.]+)\s+(?:shares?|units?)\s+(?:of\s+)?(?P<sym>[A-Z0-9.]{1,8})\s+(?:@|at)\s*\$?(?P<px>[\d,.]+)`),
}

type embedded struct {
	act, sym   string
	qty, price float64
}

func parseDescription(s string) (e embedded, ok bool) {
	for _, re := range reDesc {
		m := re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		for i, n := range re.SubexpNames() {
			switch n {
			case "act":
				e.act = m[i]
			case "sym":
				e.sym = strings.ToUpper(m[i])
			case "qty":
				e.qty, _ = parseNumber(m[i], false)
			case "px":
				e.price, _ = parseNumber(m[i], false)
			}
		}
		return e, e.qty > 0
	}
	return e, false
}

// ---------- table reading ----------

func sniffDelimiter(text string) rune {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
		if len(lines) >= 25 {
			break
		}
	}
	best, bestScore := ',', 0
	for _, d := range []rune{',', ';', '\t', '|'} {
		counts := map[int]int{}
		for _, l := range lines {
			n, inQ := 0, false
			for _, r := range l {
				switch {
				case r == '"':
					inQ = !inQ
				case r == d && !inQ:
					n++
				}
			}
			if n > 0 {
				counts[n]++
			}
		}
		for n, c := range counts {
			if score := n * c * c; score > bestScore { // many columns, consistently
				best, bestScore = d, score
			}
		}
	}
	return best
}

func readTable(data []byte) (rows [][]string, delim rune, err error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	if strings.HasPrefix(strings.ToLower(text), "sep=") { // Excel hint line
		if i := strings.Index(text, "\n"); i >= 0 {
			text = text[i+1:]
		}
	}
	delim = sniffDelimiter(text)
	cr := csv.NewReader(strings.NewReader(text))
	cr.Comma, cr.LazyQuotes, cr.FieldsPerRecord, cr.TrimLeadingSpace = delim, true, -1, true
	rows, err = cr.ReadAll()
	for _, r := range rows {
		for i := range r {
			r[i] = strings.TrimSpace(r[i])
		}
	}
	return
}

func detectHeaderRow(rows [][]string) int {
	best, bestN := 0, 1
	for i, r := range rows {
		if i >= 40 {
			break
		}
		n := 0
		for _, c := range r {
			if len(headerScores(c)) > 0 {
				n++
			}
		}
		if n > bestN {
			best, bestN = i, n
		}
	}
	return best
}

// ---------- analysis ----------

type ColMap map[Field]int

type ImportOpts struct {
	HeaderRow int           // -1 = detect
	DateOrder string        // auto | DMY | MDY | YMD
	DefMarket string        // auto | ASX | US | MANAGED  (for tickers with no exchange hint)
	DefSymbol string        // symbol to use when the file has no ticker column (one-fund statements)
	Map       map[Field]int // user overrides; nil = auto-detect
}

type ParsedRow struct {
	Line     int
	Raw      []string
	Status   string // ok | skip | error | blank
	Reason   string
	Warn     []string
	Txn      Txn
	Market   string
	Name     string
	Currency string
	Class    string // new | dup | possible  (set by the duplicate check)
	Why      string
	Existing *Txn
}

type Analysis struct {
	Delim          string
	HeaderRow      int
	Headers        []string
	Map            ColMap
	Conf           map[Field]int
	DateOrder      string
	DateNote       string
	DefMarket      string
	DefSymbol      string
	DecimalComma   bool
	FeesInclGST    bool
	TotalIsNet     bool
	Rows           []ParsedRow
	DefaultedMkt   int // rows whose market was assumed from the default
	TotalDataLines int
}

type colStat struct {
	n                                          int
	date, num, sym, typ, text, ccy, mkt, ticky float64
}

func columnStats(rows [][]string, col int) (st colStat, sample []string) {
	var nDate, nNum, nSym, nTyp, nText, nCcy, nMkt int
	for _, r := range rows {
		if col >= len(r) || r[col] == "" {
			continue
		}
		v := r[col]
		st.n++
		sample = append(sample, v)
		if _, ok := parseDateFlex(v, ""); ok {
			nDate++
		}
		if _, ok := parseNumber(v, false); ok {
			nNum++
		} else {
			nText++
		}
		if _, ign, ok := classifyType(v); ok && !ign {
			nTyp++
		} else if _, _, ok := parseSymbol(v, symCtx{}); ok && marketFromText(v) == "" && len(v) <= 12 {
			nSym++
		}
		if len(v) == 3 && ccyWords[strings.ToUpper(v)] {
			nCcy++
		}
		if marketFromText(v) != "" {
			nMkt++
		}
		if st.n >= 300 {
			break
		}
	}
	if st.n > 0 {
		f := float64(st.n)
		st.date, st.num, st.sym, st.typ, st.text, st.ccy, st.mkt =
			float64(nDate)/f, float64(nNum)/f, float64(nSym)/f, float64(nTyp)/f, float64(nText)/f, float64(nCcy)/f, float64(nMkt)/f
	}
	return
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

type cand struct {
	f     Field
	col   int
	score float64
}

// Analyze reads a CSV and works out columns, formats and every row's meaning.
func Analyze(data []byte, o ImportOpts) (*Analysis, error) {
	rows, delim, err := readTable(data)
	if err != nil {
		return nil, fmt.Errorf("could not read the file as CSV: %v", err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("the file needs a header row and at least one data row")
	}
	an := &Analysis{Delim: map[rune]string{',': "comma", ';': "semicolon", '\t': "tab", '|': "pipe"}[delim],
		Map: ColMap{}, Conf: map[Field]int{}, DecimalComma: delim == ';'}
	an.HeaderRow = o.HeaderRow
	if an.HeaderRow < 0 || an.HeaderRow >= len(rows)-1 {
		an.HeaderRow = detectHeaderRow(rows)
	}
	an.Headers = rows[an.HeaderRow]
	body := rows[an.HeaderRow+1:]
	lines := make([]int, len(body))
	for i := range body {
		lines[i] = an.HeaderRow + 2 + i
	}
	an.TotalDataLines = len(body)

	// --- score columns ---
	var cands []cand
	for col, h := range an.Headers {
		st, _ := columnStats(body, col)
		hs := headerScores(h)
		num := func(w int) float64 {
			if st.n == 0 {
				return float64(w) * 0.5
			}
			return float64(w) * (0.3 + 0.7*st.num)
		}
		for f, w := range hs {
			var sc float64
			switch f {
			case FDate, FSettle:
				sc = float64(w) * (0.25 + 0.75*st.date)
				if st.n == 0 {
					sc = float64(w) * 0.5
				}
			case FUnits, FPrice, FAmount, FTotal, FFees, FGST, FFX, FFranked, FUnfranked, FFranking, FForeignTax, FCGDisc, FCGOther, FTaxDeferred:
				sc = num(w)
			case FSymbol:
				sc = float64(w) * (0.4 + 0.6*clamp01(st.sym+st.mkt*0))
			case FName:
				sc = float64(w) * (0.3 + 0.7*st.text) * (1 - 0.6*st.sym)
			case FType:
				sc = float64(w) * clamp01(st.typ/0.7)
			case FCurrency:
				sc = float64(w) * (0.3 + 0.7*st.ccy)
			case FMarket:
				sc = float64(w) * (0.3 + 0.7*st.mkt)
			default:
				sc = float64(w)
			}
			cands = append(cands, cand{f, col, sc})
		}
		// content-only evidence for the essentials
		if st.n >= 3 {
			if st.date >= 0.8 {
				cands = append(cands, cand{FDate, col, 55 * st.date})
			}
			if st.sym >= 0.8 {
				cands = append(cands, cand{FSymbol, col, 58 * st.sym})
			}
			if st.typ >= 0.7 {
				cands = append(cands, cand{FType, col, 62 * st.typ})
			}
			if hs[FDesc] > 0 && st.typ >= 0.4 {
				cands = append(cands, cand{FType, col, 70 * st.typ})
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].col < cands[j].col
	})
	usedCol := map[int]bool{}
	for _, c := range cands {
		if c.score < 22 {
			continue
		}
		if _, have := an.Map[c.f]; have || usedCol[c.col] {
			continue
		}
		an.Map[c.f] = c.col
		an.Conf[c.f] = int(math.Min(100, math.Round(c.score)))
		usedCol[c.col] = true
	}
	// a "Description" column used as the action can still be mined for units/price
	if tc, ok := an.Map[FType]; ok {
		if _, has := an.Map[FDesc]; !has && headerScores(an.Headers[tc])[FDesc] > 0 {
			an.Map[FDesc] = tc
			an.Conf[FDesc] = an.Conf[FType]
		}
	}

	// --- user overrides ---
	for f, col := range o.Map {
		if col < 0 || col >= len(an.Headers) {
			delete(an.Map, f)
			delete(an.Conf, f)
			continue
		}
		an.Map[f] = col
		an.Conf[f] = 100
	}

	// --- formats ---
	an.DefSymbol = strings.TrimSpace(o.DefSymbol)
	an.DefMarket = o.DefMarket
	if an.DefMarket == "" || an.DefMarket == "auto" {
		an.DefMarket = MarketASX
	}
	an.DateOrder, an.DateNote = o.DateOrder, "date format chosen by you"
	if an.DateOrder == "" || an.DateOrder == "auto" {
		var vals []string
		if dc, ok := an.Map[FDate]; ok {
			for _, r := range body {
				if dc < len(r) {
					vals = append(vals, r[dc])
				}
			}
		}
		an.DateOrder, an.DateNote = detectDateOrder(vals)
	}
	if fc, ok := an.Map[FFees]; ok && fc < len(an.Headers) {
		an.FeesInclGST = strings.Contains(normHeader(an.Headers[fc]), "gst")
	}
	if tc, ok := an.Map[FTotal]; ok && tc < len(an.Headers) {
		n := normHeader(an.Headers[tc])
		an.TotalIsNet = strings.Contains(n, "net") || strings.Contains(n, "payment")
	}

	// --- interpret rows ---
	for i, r := range body {
		pr := an.interpret(lines[i], r)
		if pr.Status != "blank" {
			an.Rows = append(an.Rows, pr)
		}
	}
	return an, nil
}

var reTotalRow = regexp.MustCompile(`(?i)^(total|subtotal|sub-total|grand total|balance|summary|closing|opening|net|report|generated|page \d)`)

func (an *Analysis) cell(row []string, f Field) string {
	if i, ok := an.Map[f]; ok && i >= 0 && i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

func (an *Analysis) num(row []string, f Field) (float64, bool) {
	s := an.cell(row, f)
	if s == "" {
		return 0, false
	}
	return parseNumber(s, an.DecimalComma)
}

func abs(v float64) float64 { return math.Abs(v) }

func slug(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^A-Z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// interpret turns one CSV row into a transaction (or says why it can't).
func (an *Analysis) interpret(line int, row []string) ParsedRow {
	pr := ParsedRow{Line: line, Raw: row}
	joined := strings.TrimSpace(strings.Join(row, ""))
	if joined == "" {
		pr.Status = "blank"
		return pr
	}
	first := ""
	for _, c := range row {
		if c != "" {
			first = c
			break
		}
	}
	skip := func(reason string) ParsedRow { pr.Status, pr.Reason = "skip", reason; return pr }
	fail := func(reason string) ParsedRow { pr.Status, pr.Reason = "error", reason; return pr }

	dateStr := an.cell(row, FDate)
	if dateStr == "" {
		dateStr = an.cell(row, FSettle)
	}
	if reTotalRow.MatchString(first) && (dateStr == "" || !reNumDate.MatchString(dateStr)) {
		return skip("summary / total row")
	}
	if dateStr == "" {
		return skip("no date")
	}
	date, ok := parseDateFlex(dateStr, an.DateOrder)
	if !ok {
		return skip(fmt.Sprintf("date %q not recognised", dateStr))
	}

	// what kind of transaction?
	desc := an.cell(row, FDesc)
	typeText := an.cell(row, FType)
	if typeText == "" {
		typeText = desc
	}
	emb, hasEmb := parseDescription(desc)
	if !hasEmb {
		emb, hasEmb = parseDescription(typeText)
	}
	typ, ignore, known := classifyType(typeText)
	if ignore {
		return skip(fmt.Sprintf("not an investment transaction (%q)", typeText))
	}
	if !known && hasEmb {
		if t, ig, ok := classifyType(emb.act); ok && !ig {
			typ, known = t, true
		}
	}

	units, hasUnits := an.num(row, FUnits)
	if !hasUnits && hasEmb {
		units, hasUnits = emb.qty, true
	}
	unitsSigned := units
	units = abs(units)
	price, hasPrice := an.num(row, FPrice)
	if !hasPrice && hasEmb && emb.price > 0 {
		price, hasPrice = emb.price, true
	}
	price = abs(price)
	gross, hasGross := an.num(row, FAmount)
	total, hasTotal := an.num(row, FTotal)
	franked, hasFranked := an.num(row, FFranked)
	unfranked, hasUnfranked := an.num(row, FUnfranked)
	credit, _ := an.num(row, FFranking)

	if !known { // infer from the shape of the row
		switch {
		case hasFranked || hasUnfranked || credit != 0:
			typ = "dividend"
		case hasUnits && unitsSigned < 0:
			typ = "sell"
		case hasUnits && (hasPrice || hasGross || hasTotal):
			typ = "buy"
		default:
			return skip("can't tell whether this is a buy, sell or distribution")
		}
		pr.Warn = append(pr.Warn, "action inferred from the signs/columns")
	}

	// instrument
	mktCell := marketFromText(an.cell(row, FMarket))
	ccyCell := strings.ToUpper(an.cell(row, FCurrency))
	if len(ccyCell) != 3 {
		ccyCell = ""
	}
	ctx := symCtx{market: mktCell, ccy: ccyCell, def: an.DefMarket}
	symRaw := an.cell(row, FSymbol)
	var sym, market string
	ok = false // (ok was last set by the date parse)
	if symRaw != "" {
		sym, market, ok = parseSymbol(symRaw, ctx)
	}
	if !ok && hasEmb && emb.sym != "" {
		sym, market, ok = parseSymbol(emb.sym, ctx)
	}
	name := an.cell(row, FName)
	if !ok {
		for _, t := range []string{name, desc, symRaw} {
			if t == "" {
				continue
			}
			if m := reAPIR.FindString(strings.ToUpper(t)); m != "" {
				sym, market, ok = m, MarketManaged, true
				break
			}
			if s, mk, k := parseSymbol(t, ctx); k && (reParenSym.MatchString(strings.ToUpper(t)) || reCodeText.MatchString(t)) {
				sym, market, ok = s, mk, k
				break
			}
		}
	}
	if !ok && an.DefSymbol != "" { // one-fund statement: the user told us which fund it is
		sym, market, ok = parseSymbol(an.DefSymbol, ctx)
		if !ok {
			sym, market, ok = slug(an.DefSymbol), MarketManaged, true
		}
		if name == "" {
			name = an.DefSymbol
		}
	}
	if !ok { // any APIR code elsewhere in the row?
		if m := reAPIR.FindString(strings.ToUpper(strings.Join(row, " "))); m != "" {
			sym, market, ok = m, MarketManaged, true
		}
	}
	if !ok {
		if label := firstNonEmpty(symRaw, name); label != "" && !regexp.MustCompile(`^[\d.,\-\s$]+$`).MatchString(label) {
			sym, market, ok = slug(label), MarketManaged, true
			pr.Warn = append(pr.Warn, "no ticker or APIR code found - saved as an unlisted fund named from the text (prices need entering by hand)")
		}
	}
	if !ok {
		return fail("no ticker / fund code found in this row")
	}
	if mktCell == "" && market != MarketManaged && !strings.Contains(strings.ToUpper(symRaw), ".") && !reSuffixAU.MatchString(strings.ToUpper(symRaw)) &&
		ccyCell == "" && !regexp.MustCompile(`(?i)(asx|nasdaq|nyse)`).MatchString(symRaw) {
		an.DefaultedMkt++ // reported once on the review page rather than on every row
	}
	sym = normSymbol(sym, market)
	if name == "" {
		name = sym
	}
	inst := "AUD"
	if market == MarketUS {
		inst = "USD"
	}

	fx, hasFX := an.num(row, FFX)
	if hasFX && fx > 0 && market == MarketUS && fx < 1 {
		fx = 1 / fx // given as USD per AUD; we store AUD per USD
	}

	// fees: brokerage (+ GST unless already included), else the gap between total and gross
	fees := 0.0
	feeV, hasFee := an.num(row, FFees)
	gstV, hasGST := an.num(row, FGST)
	switch {
	case hasFee:
		fees = abs(feeV)
		if hasGST && !an.FeesInclGST {
			fees += abs(gstV)
		}
	case hasGST:
		fees = abs(gstV)
	case hasGross && hasTotal && (typ == "buy" || typ == "award" || typ == "sell"):
		fees = math.Abs(abs(total) - abs(gross))
	}

	t := Txn{Date: date, Symbol: sym, Type: typ, Fees: math.Round(fees*100) / 100, Ref: an.cell(row, FRef), Notes: an.cell(row, FNotes)}
	if typ == "award" && t.Notes == "" {
		t.Notes = "Shares awarded (RSU release) - cost base is the market value on the vest date"
	}
	if t.Notes == "" && desc != "" && desc != typeText {
		t.Notes = desc
	}
	if len(t.Notes) > 160 {
		t.Notes = t.Notes[:160]
	}

	switch typ {
	case "buy", "sell", "award":
		if units <= 0 {
			return fail("no quantity")
		}
		if !hasPrice || price <= 0 {
			switch {
			case hasGross && gross != 0:
				price = abs(gross) / units
			case hasTotal && total != 0 && (typ == "buy" || typ == "award"):
				price = (abs(total) - fees) / units
			case hasTotal && total != 0:
				price = (abs(total) + fees) / units
			default:
				return fail("no price (and no value to work it out from)")
			}
			pr.Warn = append(pr.Warn, "unit price worked out from the value")
		}
		if hasTotal && hasPrice && total != 0 {
			want := units * price
			if typ == "buy" || typ == "award" {
				want += fees
			} else {
				want -= fees
			}
			if math.Abs(abs(total)-want) > 0.01*want+0.05 && math.Abs(abs(total)-units*price) > 0.01*units*price+0.05 {
				pr.Warn = append(pr.Warn, fmt.Sprintf("total %.2f doesn't match units × price ± fees (%.2f)", abs(total), want))
			}
		}
		t.Units, t.Price = units, price
	case "dividend", "drp":
		amt := 0.0
		switch {
		case hasFranked && hasUnfranked: // both components stated: their sum is the distribution
			amt = abs(franked) + abs(unfranked)
		case hasGross && gross != 0:
			amt = abs(gross)
		case hasTotal && total != 0:
			amt = abs(total)
			if an.TotalIsNet && market == MarketUS {
				if ft, ok := an.num(row, FForeignTax); ok {
					amt += abs(ft)
					pr.Warn = append(pr.Warn, "amount treated as net of withholding tax; gross worked out")
				}
			}
		case hasFranked || hasUnfranked:
			amt = abs(franked) + abs(unfranked)
		case typ == "drp" && hasUnits && hasPrice:
			amt = units * price
		}
		if typ == "dividend" {
			if amt <= 0 {
				return fail("no distribution amount")
			}
		} else {
			if units <= 0 {
				return fail("reinvestment needs units")
			}
			if !hasPrice || price <= 0 {
				if amt > 0 {
					price = amt / units
				} else {
					return fail("reinvestment needs a price or amount")
				}
			}
			t.Units, t.Price = units, price
		}
		t.Amount = math.Round(amt*100) / 100
		if hasFranked {
			t.Franked = abs(franked)
		}
		t.Franking = abs(credit)
		if v, ok := an.num(row, FForeignTax); ok {
			t.ForeignTax = abs(v)
		}
		if v, ok := an.num(row, FCGDisc); ok {
			t.CGDisc = abs(v)
		}
		if v, ok := an.num(row, FCGOther); ok {
			t.CGOther = abs(v)
		}
		if v, ok := an.num(row, FTaxDeferred); ok {
			t.TaxDeferred = abs(v)
		}
		if ty := an.cell(row, FTaxYear); ty != "" {
			if m := regexp.MustCompile(`^(\d{4})\s*[-/]\s*(\d{2,4})$`).FindStringSubmatch(ty); m != nil && validFY(m[1]+"-"+m[2][len(m[2])-2:]) {
				t.TaxYear = m[1] + "-" + m[2][len(m[2])-2:]
			}
		}
	}

	// currency: our price/amount must be in the instrument's own currency
	if ccyCell != "" && ccyCell != inst {
		if ccyCell == "AUD" && inst == "USD" && hasFX && fx > 0 {
			t.Price /= fx
			t.Amount /= fx
			t.ForeignTax /= fx
			pr.Warn = append(pr.Warn, fmt.Sprintf("converted from AUD to USD at the file's FX rate %.4f", fx))
		} else {
			return fail(fmt.Sprintf("amounts are in %s but this %s holding is in %s - add an FX rate column or enter it manually", ccyCell, market, inst))
		}
	}
	if hasFX && fx > 0 && market == MarketUS {
		t.FX = fx
	}
	if err := validateTxn(&t); err != nil {
		return fail(err.Error())
	}
	pr.Status, pr.Txn, pr.Market, pr.Name, pr.Currency = "ok", t, market, name, inst
	return pr
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
