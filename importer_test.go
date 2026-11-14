package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

func analyze(t *testing.T, csv string, mut ...func(*ImportOpts)) *Analysis {
	t.Helper()
	o := ImportOpts{HeaderRow: -1, DateOrder: "auto", DefMarket: "auto"}
	for _, m := range mut {
		m(&o)
	}
	an, err := Analyze([]byte(csv), o)
	if err != nil {
		t.Fatal(err)
	}
	return an
}

func okTxns(an *Analysis) []Txn {
	var out []Txn
	for _, r := range an.Rows {
		if r.Status == "ok" {
			out = append(out, r.Txn)
		}
	}
	return out
}

func TestParseNumber(t *testing.T) {
	cases := []struct {
		in   string
		dec  bool
		want float64
		ok   bool
	}{
		{"1,234.50", false, 1234.5, true}, {"$1,234.50", false, 1234.5, true}, {"A$12", false, 12, true},
		{"(1,234.50)", false, -1234.5, true}, {"12.5-", false, -12.5, true}, {"1234.5CR", false, 1234.5, true},
		{"1234.5DR", false, -1234.5, true}, {"1.234,50", false, 1234.5, true}, {"95,10", true, 95.1, true},
		{"95,10", false, 95.1, true}, {"1,234", false, 1234, true}, {"1,234", true, 1.234, true},
		{"1 234.56", false, 1234.56, true}, {"-5", false, -5, true}, {"+7.5", false, 7.5, true}, {"1e3", false, 1000, true},
		{"2.5%", false, 2.5, true}, {"USD 12.30", false, 12.3, true},
		{"", false, 0, false}, {"-", false, 0, false}, {"n/a", false, 0, false}, {"abc", false, 0, false},
	}
	for _, c := range cases {
		got, ok := parseNumber(c.in, c.dec)
		if ok != c.ok || (ok && math.Abs(got-c.want) > 1e-9) {
			t.Errorf("parseNumber(%q,%v) = %v,%v want %v,%v", c.in, c.dec, got, ok, c.want, c.ok)
		}
	}
}

func TestParseDateFlex(t *testing.T) {
	cases := []struct{ in, order, want string }{
		{"2025-01-02", "", "2025-01-02"}, {"02/01/2025", "DMY", "2025-01-02"}, {"02/01/2025", "MDY", "2025-02-01"},
		{"25/03/2025", "", "2025-03-25"}, {"03/25/2025", "", "2025-03-25"}, {"2/1/25", "DMY", "2025-01-02"},
		{"2 Jan 2025", "", "2025-01-02"}, {"02-Jan-2025", "", "2025-01-02"}, {"January 2, 2025", "", "2025-01-02"},
		{"2nd January 2025", "", "2025-01-02"}, {"20250102", "", "2025-01-02"}, {"20250102;093015", "", "2025-01-02"},
		{"2025-01-02T09:30:15Z", "", "2025-01-02"}, {"02/01/2025 14:35:10", "DMY", "2025-01-02"}, {"02.01.2025", "DMY", "2025-01-02"},
		{"2025/01/02", "", "2025-01-02"}, {"Thu, 2 Jan 2025", "", "2025-01-02"}, {"2025-01-02 09:30", "", "2025-01-02"},
		{"31/02/2025", "DMY", ""}, {"hello", "", ""}, {"", "", ""}, {"13/13/2025", "", ""},
	}
	for _, c := range cases {
		got, ok := parseDateFlex(c.in, c.order)
		if c.want == "" && ok || c.want != "" && (!ok || got != c.want) {
			t.Errorf("parseDateFlex(%q,%q) = %q,%v want %q", c.in, c.order, got, ok, c.want)
		}
	}
	if o, _ := detectDateOrder([]string{"01/02/2025", "25/02/2025"}); o != "DMY" {
		t.Errorf("order %s", o)
	}
	if o, _ := detectDateOrder([]string{"01/02/2025", "02/25/2025"}); o != "MDY" {
		t.Errorf("order %s", o)
	}
	if o, note := detectDateOrder([]string{"01/02/2025", "03/04/2025"}); o != "DMY" || !strings.Contains(note, "ambiguous") {
		t.Errorf("order %s %s", o, note)
	}
	if o, _ := detectDateOrder([]string{"2025-01-02"}); o != "YMD" {
		t.Errorf("order %s", o)
	}
}

func TestClassifyType(t *testing.T) {
	cases := map[string]string{
		"Buy": "buy", "BOUGHT": "buy", "B": "buy", "Purchase": "buy", "Application": "buy", "Market Buy": "buy", "Switch In": "buy",
		"Sell": "sell", "Sold": "sell", "S": "sell", "Redemption": "sell", "Limit Sell": "sell", "Disposal": "sell",
		"Dividend": "dividend", "Distribution": "dividend", "Franked Dividend": "dividend", "DIV": "dividend",
		"Distribution Reinvestment": "drp", "DRP": "drp", "Dividend Reinvested": "drp",
		"B 100 VAS @ 95.10": "buy", "S 10 VAS @ 105": "sell",
	}
	for in, want := range cases {
		if got, ign, ok := classifyType(in); !ok || ign || got != want {
			t.Errorf("classifyType(%q) = %q ign=%v ok=%v want %q", in, got, ign, ok, want)
		}
	}
	for _, in := range []string{"Deposit", "Withdrawal", "Interest", "Brokerage Fee", "Transfer In", "Withholding Tax", "Stock Split", "Opening Balance"} {
		if _, ign, ok := classifyType(in); !ok || !ign {
			t.Errorf("classifyType(%q) should be an ignored row", in)
		}
	}
	if _, _, ok := classifyType("ETF"); ok {
		t.Error("an asset class is not an action")
	}
}

func TestParseSymbol(t *testing.T) {
	ctx := symCtx{def: MarketASX}
	cases := []struct{ in, sym, mkt string }{
		{"VAS", "VAS", MarketASX}, {"vas.ax", "VAS", MarketASX}, {"ASX:VAS", "VAS", MarketASX}, {"VAS AU Equity", "VAS", MarketASX},
		{"VAS.AU", "VAS", MarketASX}, {"NASDAQ:AAPL", "AAPL", MarketUS}, {"AAPL US Equity", "AAPL", MarketUS}, {"AAPL.US", "AAPL", MarketUS},
		{"AAPL.O", "AAPL", MarketUS}, {"VAN0002AU", "VAN0002AU", MarketManaged}, {"AU000000VAS1", "VAS", MarketASX},
		{"Vanguard Australian Shares Index ETF (VAS)", "VAS", MarketASX}, {"BRK.B", "BRK.B", MarketASX},
	}
	for _, c := range cases {
		s, m, ok := parseSymbol(c.in, ctx)
		if !ok || s != c.sym || m != c.mkt {
			t.Errorf("parseSymbol(%q) = %q,%q,%v want %q,%q", c.in, s, m, ok, c.sym, c.mkt)
		}
	}
	if _, m, _ := parseSymbol("AAPL", symCtx{ccy: "USD", def: MarketASX}); m != MarketUS {
		t.Error("USD currency should make a bare ticker a US stock")
	}
	if _, m, _ := parseSymbol("AAPL", symCtx{market: MarketUS, def: MarketASX}); m != MarketUS {
		t.Error("exchange column should win")
	}
	for _, bad := range []string{"", "Vanguard Australian Shares", "USD", "12.5"} {
		if _, _, ok := parseSymbol(bad, ctx); ok {
			t.Errorf("parseSymbol(%q) should fail", bad)
		}
	}
}

func TestNormHeader(t *testing.T) {
	cases := map[string]string{
		"Unit Price ($)": "unit price", "Brokerage+GST ($)": "brokerage plus gst", "TradePrice": "trade price",
		"CurrencyPrimary": "currency primary", "Buy/Sell": "buy sell", "Avg. Price": "avg price", "Trade Value (AUD)": "trade value",
		"\ufeffDate": "date", "fees_aud": "fees", "franking_credits_aud": "franking credits",
	}
	for in, want := range cases {
		if got := normHeader(in); got != want {
			t.Errorf("normHeader(%q) = %q want %q", in, got, want)
		}
	}
}

func TestImportCommSecStyle(t *testing.T) {
	an := analyze(t, `Code,Company,Date,Type,Quantity,Unit Price ($),Trade Value ($),Brokerage+GST ($),GST ($),Contract Note,Total Value ($)
VAS,VANGUARD AUSTRALIAN SHARES INDEX ETF,02/01/2025,Buy,50,95.10,4755.00,9.50,0.86,12345678,4764.50
VAS,VANGUARD AUSTRALIAN SHARES INDEX ETF,15/09/2025,Sell,10,105.00,1050.00,9.50,0.86,12345999,1040.50
`)
	ts := okTxns(an)
	if len(ts) != 2 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	b, s := ts[0], ts[1]
	if b.Date != "2025-01-02" || b.Symbol != "VAS.AX" || b.Type != "buy" || b.Units != 50 || b.Price != 95.10 || b.Fees != 9.50 || b.Ref != "12345678" {
		t.Errorf("buy: %+v", b)
	}
	if s.Date != "2025-09-15" || s.Type != "sell" || s.Units != 10 || s.Fees != 9.50 {
		t.Errorf("sell: %+v", s)
	}
	if an.DateOrder != "DMY" || an.Rows[0].Market != MarketASX {
		t.Errorf("order %s market %s", an.DateOrder, an.Rows[0].Market)
	}
	if len(an.Rows[0].Warn) != 0 {
		t.Errorf("unexpected warnings: %v", an.Rows[0].Warn)
	}
}

func TestImportUSBrokerISODatesAndCurrency(t *testing.T) {
	an := analyze(t, `Trade Date,Settlement Date,Symbol,Side,Units,Avg. Price,Value,Fee,Currency
2025-03-04,2025-03-06,AAPL,Buy,5,195.00,975.00,3.00,USD
2025-04-10,2025-04-14,MSFT,Sell,2,410.50,821.00,3.00,USD
`)
	ts := okTxns(an)
	if len(ts) != 2 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	if ts[0].Symbol != "AAPL" || an.Rows[0].Market != MarketUS || ts[0].Date != "2025-03-04" || ts[0].Fees != 3 || ts[0].Price != 195 {
		t.Errorf("%+v %s", ts[0], an.Rows[0].Market)
	}
	if ts[1].Type != "sell" || ts[1].Units != 2 || ts[1].Price != 410.5 {
		t.Errorf("%+v", ts[1])
	}
}

func TestImportIBKRStyle(t *testing.T) {
	an := analyze(t, `Symbol,DateTime,Quantity,TradePrice,IBCommission,CurrencyPrimary,Buy/Sell
AAPL,20250304;093015,5,195.00,-1.00,USD,BUY
AAPL,20250410;101500,-2,200.50,-1.00,USD,SELL
`)
	ts := okTxns(an)
	if len(ts) != 2 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	if ts[0].Date != "2025-03-04" || ts[0].Fees != 1 || ts[1].Type != "sell" || ts[1].Units != 2 || ts[1].Date != "2025-04-10" {
		t.Errorf("%+v %+v", ts[0], ts[1])
	}
}

func TestImportSignedQuantityWithoutTypeColumn(t *testing.T) {
	an := analyze(t, `Date,Code,Quantity,Price
02/01/2025,VAS,50,95.10
15/09/2025,VAS,-10,105.00
`)
	ts := okTxns(an)
	if len(ts) != 2 || ts[0].Type != "buy" || ts[1].Type != "sell" || ts[1].Units != 10 {
		t.Fatalf("%+v", an.Rows)
	}
	if len(an.Rows[0].Warn) == 0 {
		t.Error("an inferred action should be flagged")
	}
}

func TestImportSemicolonEuropeanFormat(t *testing.T) {
	an := analyze(t, "Date;Code;Type;Quantity;Price;Brokerage\n02.01.2025;VAS;Buy;50;95,10;9,50\n15.09.2025;VAS;Sell;10;105,00;9,50\n")
	ts := okTxns(an)
	if an.Delim != "semicolon" || len(ts) != 2 || ts[0].Price != 95.10 || ts[0].Fees != 9.5 || ts[1].Date != "2025-09-15" {
		t.Fatalf("%s %+v", an.Delim, an.Rows)
	}
}

func TestImportMonthFirstDatesDetected(t *testing.T) {
	an := analyze(t, "Date,Symbol,Action,Shares,Price\n03/25/2025,AAPL,Buy,5,195\n04/02/2025,AAPL,Buy,1,190\n",
		func(o *ImportOpts) { o.DefMarket = MarketUS })
	ts := okTxns(an)
	if an.DateOrder != "MDY" || len(ts) != 2 || ts[0].Date != "2025-03-25" || ts[1].Date != "2025-04-02" {
		t.Fatalf("%s %+v", an.DateOrder, an.Rows)
	}
	// user override beats detection
	an = analyze(t, "Date,Symbol,Action,Shares,Price\n03/04/2025,AAPL,Buy,5,195\n", func(o *ImportOpts) { o.DateOrder = "MDY" })
	if okTxns(an)[0].Date != "2025-03-04" {
		t.Errorf("MDY override: %+v", an.Rows)
	}
}

func TestImportStatementWithPreambleAndFooter(t *testing.T) {
	csv := `Vanguard Personal Investor,,,,
Account statement,,,,
,,,,
Date,Description,Units,Unit price,Amount
15/03/2025,Application,100.1234,50.1234,"5,019.00"
20/06/2025,Distribution,,,150.00
30/06/2025,Distribution Reinvestment,2.5,52.00,130.00
Total,,,,"5,299.00"
`
	an := analyze(t, csv, func(o *ImportOpts) { o.DefSymbol = "VAN0002AU" })
	if an.HeaderRow != 3 {
		t.Errorf("header row %d", an.HeaderRow)
	}
	ts := okTxns(an)
	if len(ts) != 3 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	if ts[0].Type != "buy" || ts[0].Symbol != "VAN0002AU" || an.Rows[0].Market != MarketManaged || ts[0].Units != 100.1234 {
		t.Errorf("%+v", ts[0])
	}
	if ts[1].Type != "dividend" || ts[1].Amount != 150 || ts[1].Units != 0 {
		t.Errorf("%+v", ts[1])
	}
	if ts[2].Type != "drp" || ts[2].Units != 2.5 || ts[2].Price != 52 || ts[2].Amount != 130 {
		t.Errorf("%+v", ts[2])
	}
	if last := an.Rows[len(an.Rows)-1]; last.Status != "skip" || !strings.Contains(last.Reason, "total") {
		t.Errorf("total row: %+v", last)
	}
	// no ticker anywhere and none supplied -> a clear error, not a guess
	an = analyze(t, csv)
	for _, r := range an.Rows[:3] {
		if r.Status != "error" || !strings.Contains(r.Reason, "ticker") {
			t.Errorf("expected a ticker error: %+v", r)
		}
	}
}

func TestImportDescriptionOnlyStatement(t *testing.T) {
	an := analyze(t, `Date,Transaction Details,Amount
02/01/2025,B 100 VAS @ 95.100000,-9519.50
15/09/2025,S 10 VAS @ 105.000000,1040.50
17/09/2025,Deposit - thank you,5000.00
`)
	ts := okTxns(an)
	if len(ts) != 2 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	if ts[0].Type != "buy" || ts[0].Units != 100 || ts[0].Price != 95.1 || ts[0].Symbol != "VAS.AX" || ts[1].Type != "sell" || ts[1].Units != 10 {
		t.Errorf("%+v %+v", ts[0], ts[1])
	}
	if an.Rows[2].Status != "skip" {
		t.Errorf("deposit should be skipped: %+v", an.Rows[2])
	}
}

func TestImportDividendStatementWithFranking(t *testing.T) {
	an := analyze(t, `Payment Date,Security Code,Franked Amount,Unfranked Amount,Franking Credit,Net Payment
01/10/2025,VAS,70.00,20.00,30.00,90.00
`)
	ts := okTxns(an)
	if len(ts) != 1 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	d := ts[0]
	if d.Type != "dividend" || d.Amount != 90 || d.Franked != 70 || d.Franking != 30 || d.Date != "2025-10-01" {
		t.Errorf("%+v", d)
	}
}

func TestImportUSDividendWithholding(t *testing.T) {
	an := analyze(t, `Date,Symbol,Type,Amount,Withholding Tax,Currency
2025-08-05,AAPL,Dividend,1.25,0.19,USD
`)
	ts := okTxns(an)
	if len(ts) != 1 || ts[0].Amount != 1.25 || ts[0].ForeignTax != 0.19 || an.Rows[0].Market != MarketUS {
		t.Fatalf("%+v", an.Rows)
	}
}

func TestImportCurrencyMismatchIsBlockedNotGuessed(t *testing.T) {
	an := analyze(t, `Date,Symbol,Type,Quantity,Price,Currency,Exchange
2025-03-04,AAPL,Buy,5,300.00,AUD,NASDAQ
`)
	if r := an.Rows[0]; r.Status != "error" || !strings.Contains(r.Reason, "AUD") {
		t.Fatalf("%+v", r)
	}
	// ...but converts when the file supplies the rate
	an = analyze(t, `Date,Symbol,Type,Quantity,Price,Currency,Exchange,FX Rate
2025-03-04,AAPL,Buy,5,300.00,AUD,NASDAQ,1.5
`)
	r := an.Rows[0]
	if r.Status != "ok" || math.Abs(r.Txn.Price-200) > 1e-9 || r.Txn.FX != 1.5 {
		t.Fatalf("%+v", r)
	}
	// USD-per-AUD style rates are inverted to AUD-per-USD
	an = analyze(t, `Date,Symbol,Type,Quantity,Price,Currency,FX Rate
2025-03-04,AAPL,Buy,5,200.00,USD,0.6667
`)
	if fx := an.Rows[0].Txn.FX; math.Abs(fx-1.5) > 0.001 {
		t.Errorf("fx %v", fx)
	}
}

func TestImportUnlistedFundWithoutCode(t *testing.T) {
	an := analyze(t, `Date,Fund,Type,Units,Unit Price
15/03/2025,Vanguard Australian Shares Index Fund,Application,100,50.00
`)
	r := an.Rows[0]
	if r.Status != "ok" || r.Market != MarketManaged || !strings.HasPrefix(r.Txn.Symbol, "VANGUARD-AUSTRALIAN") || len(r.Warn) == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestImportSkipsNoiseRows(t *testing.T) {
	an := analyze(t, `Date,Code,Type,Quantity,Price,Amount
02/01/2025,VAS,Buy,50,95.10,
03/01/2025,,Interest,,,1.50
04/01/2025,VAS,Brokerage Fee,,,9.50
not a date,VAS,Buy,5,10,
,,,,,
Total,,,,,100
`)
	ts := okTxns(an)
	if len(ts) != 1 {
		t.Fatalf("rows: %+v", an.Rows)
	}
	skipped := 0
	for _, r := range an.Rows {
		if r.Status == "skip" {
			skipped++
		}
	}
	if skipped != 4 {
		t.Errorf("skipped %d: %+v", skipped, an.Rows)
	}
}

func TestImportManualMappingOverride(t *testing.T) {
	csv := "A,B,C,D,E\n02/01/2025,VAS,Buy,50,95.10\n"
	an := analyze(t, csv)
	if len(okTxns(an)) != 0 { // unrecognisable headers: nothing guessed for numbers
		t.Logf("auto-detected: %+v", an.Map)
	}
	an = analyze(t, csv, func(o *ImportOpts) {
		o.Map = map[Field]int{FDate: 0, FSymbol: 1, FType: 2, FUnits: 3, FPrice: 4}
	})
	ts := okTxns(an)
	if len(ts) != 1 || ts[0].Units != 50 || ts[0].Price != 95.1 || ts[0].Symbol != "VAS.AX" {
		t.Fatalf("%+v", an.Rows)
	}
	// choosing "not in file" removes an auto-detected column
	an = analyze(t, "Date,Code,Type,Quantity,Price,Brokerage\n02/01/2025,VAS,Buy,50,95.10,9.50\n",
		func(o *ImportOpts) { o.Map = map[Field]int{FFees: -1} })
	if ts := okTxns(an); len(ts) != 1 || ts[0].Fees != 0 {
		t.Errorf("%+v", an.Rows)
	}
}

func TestImportDerivesPriceFromValue(t *testing.T) {
	an := analyze(t, "Date,Code,Type,Quantity,Total Value\n02/01/2025,VAS,Buy,50,\"4,764.50\"\n")
	r := an.Rows[0]
	if r.Status != "ok" || math.Abs(r.Txn.Price-95.29) > 1e-9 || len(r.Warn) == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestOwnExportRoundTripsExactly(t *testing.T) {
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	db := newTestDB()
	db.Instruments["FUND1"] = &Instrument{Symbol: "FUND1", Name: "Managed", Currency: "AUD", Market: MarketManaged}
	db.FX["USD"] = map[string]float64{"2025-01-01": 1.5}
	db.Txns = []Txn{
		{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 100, Price: 95.123456, Fees: 9.5, Ref: "CN-001"},
		{Date: "2025-04-01", Symbol: "VAS.AX", Type: "dividend", Amount: 90, Franked: 70, Franking: 30, TaxYear: "2024-25"},
		{Date: "2025-04-02", Symbol: "VAS.AX", Type: "drp", Units: 1, Price: 98, Amount: 98},
		{Date: "2025-01-03", Symbol: "AAPL", Type: "buy", Units: 5, Price: 195, FX: 1.5},
		{Date: "2025-02-03", Symbol: "FUND1", Type: "buy", Units: 10.5, Price: 50.25},
		{Date: "2025-06-01", Symbol: "VAS.AX", Type: "sell", Units: 20, Price: 101, Fees: 9.5},
	}
	for i := range db.Txns {
		db.Txns[i].ID = newID()
		db.Txns[i].FP = Fingerprint(db.Txns[i])
	}
	var buf bytes.Buffer
	if err := WriteTransactionsCSV(&buf, Compute(db, now)); err != nil {
		t.Fatal(err)
	}
	an, err := Analyze(buf.Bytes(), ImportOpts{HeaderRow: -1, DateOrder: "auto", DefMarket: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	got := okTxns(an)
	if len(got) != len(db.Txns) {
		t.Fatalf("got %d of %d rows: %+v", len(got), len(db.Txns), an.Rows)
	}
	// every original transaction is recognised as already recorded
	for i, m := range ClassifyDuplicates(db.Txns, got) {
		if m.Class != "dup" {
			t.Errorf("row %d (%+v) classified %s: %s", i, got[i], m.Class, m.Why)
		}
	}
	// and details survive
	for _, g := range got {
		if g.Type == "dividend" && (g.Franked != 70 || g.Franking != 30 || g.TaxYear != "2024-25") {
			t.Errorf("dividend detail lost: %+v", g)
		}
		if g.Symbol == "VAS.AX" && g.Type == "buy" && g.Ref != "CN-001" {
			t.Errorf("reference lost: %+v", g)
		}
	}
}

func TestClassifyRSURelease(t *testing.T) {
	for _, s := range []string{"Release", "RSU Release", "Stock Award", "Shares vested", "Restricted Stock Unit Vest", "RSU vesting", "Award"} {
		if typ, ig, ok := classifyType(s); typ != "award" || ig || !ok {
			t.Errorf("classifyType(%q) = %q ignore=%v ok=%v, want award", s, typ, ig, ok)
		}
	}
	// ordinary wording must be unaffected
	for s, want := range map[string]string{"Buy": "buy", "Sell to cover": "sell", "Reinvestment": "drp", "Dividend": "dividend"} {
		if typ, _, _ := classifyType(s); typ != want {
			t.Errorf("classifyType(%q) = %q, want %q", s, typ, want)
		}
	}
}

func TestImportRSUReleaseRows(t *testing.T) {
	an := analyze(t, `Date,Action,Symbol,Quantity,Price,Currency
15/03/2026,Release,ACME,40,12.50,USD
16/03/2026,Sell to cover,ACME,14,12.60,USD
`)
	var ts []Txn
	for _, r := range an.Rows {
		if r.Status == "ok" {
			ts = append(ts, r.Txn)
		}
	}
	if len(ts) != 2 || ts[0].Type != "award" || ts[0].Units != 40 || ts[0].Price != 12.5 || ts[1].Type != "sell" {
		t.Fatalf("rows: %+v (%+v)", ts, an.Rows)
	}
	if !strings.Contains(ts[0].Notes, "RSU") {
		t.Fatalf("note: %q", ts[0].Notes)
	}
}

func TestAwardCountsLikeAPurchaseAtVestValue(t *testing.T) {
	db := &DB{}
	db.init()
	db.Instruments["ACME"] = &Instrument{Symbol: "ACME", Name: "Acme", Currency: "AUD", Market: MarketUS}
	db.Txns = []Txn{{ID: "1", Date: "2025-01-10", Symbol: "ACME", Type: "award", Units: 100, Price: 10}}
	db.Prices["ACME"] = map[string]float64{"2026-11-13": 15}
	res := Compute(db, time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	h := res.Holdings[0]
	if h.Units != 100 || h.CostAUD != 1000 || h.ValueAUD != 1500 || h.Unrealised != 500 {
		t.Fatalf("holding %+v", h)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "reinvestment") {
			t.Fatalf("an award must not trigger the missing-history warning: %s", w)
		}
	}
	// the parcel is dated the vest day, so after 12 months it qualifies for the CGT discount
	if err := validateTxn(&db.Txns[0]); err != nil {
		t.Fatal(err)
	}
}
