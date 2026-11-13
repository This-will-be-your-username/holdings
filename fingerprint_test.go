package main

import "testing"

func tx(date, sym, typ string, units, price float64) Txn {
	return Txn{Date: date, Symbol: sym, Type: typ, Units: units, Price: price}
}

func classes(m []DupMatch) []string {
	out := make([]string, len(m))
	for i, x := range m {
		out[i] = x.Class
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFingerprintIgnoresFormattingFeesAndNotes(t *testing.T) {
	a := Txn{Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 50, Price: 95.1, Fees: 9.5, Notes: "x", FX: 1}
	b := Txn{Date: "2025-01-02", Symbol: "vas.ax", Type: "buy", Units: 50.000000, Price: 95.100000, Fees: 0, Notes: "other"}
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("same trade should fingerprint identically regardless of fees/notes/case/precision")
	}
	for name, c := range map[string]Txn{
		"date":   {Date: "2025-01-03", Symbol: "VAS.AX", Type: "buy", Units: 50, Price: 95.1},
		"symbol": {Date: "2025-01-02", Symbol: "VGS.AX", Type: "buy", Units: 50, Price: 95.1},
		"type":   {Date: "2025-01-02", Symbol: "VAS.AX", Type: "sell", Units: 50, Price: 95.1},
		"units":  {Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 51, Price: 95.1},
		"price":  {Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 50, Price: 95.2},
	} {
		if Fingerprint(a) == Fingerprint(c) {
			t.Errorf("changing %s should change the fingerprint", name)
		}
	}
	// distributions are identified by amount, not by unit/price fields
	d1 := Txn{Date: "2025-10-01", Symbol: "VAS.AX", Type: "dividend", Amount: 90, Franking: 30}
	d2 := Txn{Date: "2025-10-01", Symbol: "VAS.AX", Type: "dividend", Amount: 90.004, Units: 70, Price: 1.29}
	if Fingerprint(d1) != Fingerprint(d2) {
		t.Error("dividend fingerprints should depend on date, symbol and amount only")
	}
	if len(Fingerprint(a)) != 24 {
		t.Error("fingerprint length")
	}
}

func TestDedupeIdenticalReupload(t *testing.T) {
	existing := []Txn{tx("2025-01-02", "VAS.AX", "buy", 50, 95.1), tx("2025-02-03", "VAS.AX", "sell", 10, 100)}
	got := classes(ClassifyDuplicates(existing, []Txn{existing[1], existing[0]}))
	if !eq(got, []string{"dup", "dup"}) {
		t.Errorf("%v", got)
	}
}

func TestDedupeIsCountAware(t *testing.T) {
	one := tx("2025-01-02", "VAS.AX", "buy", 10, 100)
	// two identical trades on one day are two transactions
	if got := classes(ClassifyDuplicates(nil, []Txn{one, one})); !eq(got, []string{"new", "new"}) {
		t.Errorf("%v", got)
	}
	// one already recorded, file has both: only the second is new
	if got := classes(ClassifyDuplicates([]Txn{one}, []Txn{one, one})); !eq(got, []string{"dup", "new"}) {
		t.Errorf("%v", got)
	}
	// both recorded, re-upload of the file: both are duplicates
	if got := classes(ClassifyDuplicates([]Txn{one, one}, []Txn{one, one})); !eq(got, []string{"dup", "dup"}) {
		t.Errorf("%v", got)
	}
	// an overlapping later file containing only one of the pair does not create a new one
	if got := classes(ClassifyDuplicates([]Txn{one, one}, []Txn{one})); !eq(got, []string{"dup"}) {
		t.Errorf("%v", got)
	}
}

func TestDedupeUsesReferences(t *testing.T) {
	a := tx("2025-01-02", "VAS.AX", "buy", 10, 100)
	a.Ref = "CN-1001"
	sameDifferentRef := a
	sameDifferentRef.Ref = "CN-1002"
	noRef := a
	noRef.Ref = ""

	// same content, different contract notes: two genuine trades (the case count-matching alone can't tell)
	if got := classes(ClassifyDuplicates([]Txn{a}, []Txn{sameDifferentRef})); !eq(got, []string{"new"}) {
		t.Errorf("different refs: %v", got)
	}
	// same reference: duplicate even if the file writes it differently
	again := a
	again.Ref = "cn 1001"
	if got := classes(ClassifyDuplicates([]Txn{a}, []Txn{again})); !eq(got, []string{"dup"}) {
		t.Errorf("same ref: %v", got)
	}
	// manual entry (no ref) vs later upload with a ref: recognised, so the ref can be attached
	m := ClassifyDuplicates([]Txn{noRef}, []Txn{a})
	if m[0].Class != "dup" || m[0].Existing != 0 {
		t.Errorf("manual vs ref: %+v", m[0])
	}
	// same reference but the details changed (e.g. a corrected contract note): flag it
	changed := a
	changed.Price = 101
	if m := ClassifyDuplicates([]Txn{a}, []Txn{changed}); m[0].Class != "possible" {
		t.Errorf("changed details: %+v", m[0])
	}
}

func TestDedupePossibleDuplicates(t *testing.T) {
	base := tx("2025-01-02", "VAS.AX", "buy", 50, 95.10)
	cases := []struct {
		name string
		in   Txn
		want string
	}{
		{"settlement date 2 days later", tx("2025-01-04", "VAS.AX", "buy", 50, 95.10), "possible"},
		{"price rounded differently", tx("2025-01-02", "VAS.AX", "buy", 50, 95.12), "possible"},
		{"10 days apart", tx("2025-01-12", "VAS.AX", "buy", 50, 95.10), "new"},
		{"different units", tx("2025-01-02", "VAS.AX", "buy", 40, 95.10), "new"},
		{"different symbol", tx("2025-01-02", "VGS.AX", "buy", 50, 95.10), "new"},
		{"price very different", tx("2025-01-02", "VAS.AX", "buy", 50, 120), "new"},
	}
	for _, c := range cases {
		if got := ClassifyDuplicates([]Txn{base}, []Txn{c.in})[0].Class; got != c.want {
			t.Errorf("%s: %s want %s", c.name, got, c.want)
		}
	}
	div := Txn{Date: "2025-10-01", Symbol: "VAS.AX", Type: "dividend", Amount: 90}
	payDate := Txn{Date: "2025-10-04", Symbol: "VAS.AX", Type: "dividend", Amount: 90.5}
	if got := ClassifyDuplicates([]Txn{div}, []Txn{payDate})[0].Class; got != "possible" {
		t.Errorf("dividend ex vs pay date: %s", got)
	}
	// a single existing transaction can only flag one incoming row
	got := classes(ClassifyDuplicates([]Txn{base}, []Txn{tx("2025-01-03", "VAS.AX", "buy", 50, 95.10), tx("2025-01-03", "VAS.AX", "buy", 50, 95.10)}))
	if !eq(got, []string{"possible", "new"}) {
		t.Errorf("one existing flags one row: %v", got)
	}
}

func TestSameTradeFromTwoDifferentBrokerFormatsIsRecognised(t *testing.T) {
	a := analyze(t, `Code,Company,Date,Type,Quantity,Unit Price ($),Trade Value ($),Brokerage+GST ($),GST ($),Contract Note,Total Value ($)
VAS,VANGUARD,02/01/2025,Buy,50,95.10,4755.00,9.50,0.86,12345678,4764.50
`)
	// the same trade as another platform (different layout, ISO date, side wording, price precision) lists it
	b := analyze(t, `Trade Date,Ticker,Side,Units,Avg. Price,Fee
2025-01-02,VAS.AX,BOUGHT,50,95.100000,6.60
`)
	existing := okTxns(a)
	for i := range existing {
		existing[i].FP = Fingerprint(existing[i])
	}
	m := ClassifyDuplicates(existing, okTxns(b))
	if len(m) != 1 || m[0].Class != "dup" {
		t.Fatalf("%+v (a=%+v b=%+v)", m, existing, okTxns(b))
	}
}

func TestClassifyRowsSummary(t *testing.T) {
	existing := []Txn{tx("2025-01-02", "VAS.AX", "buy", 50, 95.10), tx("2025-01-20", "VAS.AX", "buy", 10, 100)}
	an := analyze(t, `Date,Code,Type,Quantity,Price
20/01/2025,VAS,Buy,10,100
04/01/2025,VAS,Buy,50,95.12
10/02/2025,VAS,Sell,10,100
15/02/2025,VAS,Deposit,,
`)
	s := classifyRows(an, existing)
	if s.Dup != 1 || s.Possible != 1 || s.New != 1 || s.Skipped != 1 {
		t.Errorf("%+v", s)
	}
	if an.Rows[0].Existing == nil || an.Rows[0].Existing.Date != "2025-01-20" {
		t.Errorf("dup should point at the existing transaction: %+v", an.Rows[0])
	}
}
