package main

import (
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sellAllDB(now time.Time) *DB {
	db := &DB{}
	db.init()
	for _, s := range []string{"AAA.AX", "BBB.AX", "CCC.AX"} {
		db.Instruments[s] = &Instrument{Symbol: s, Name: s, Currency: "AUD", Market: MarketASX}
	}
	old := now.AddDate(-2, 0, 0).Format(dateFmt)
	recent := now.AddDate(0, -3, 0).Format(dateFmt)
	db.Txns = []Txn{
		{ID: "1", Date: old, Symbol: "AAA.AX", Type: "buy", Units: 100, Price: 10},    // long-term gain of 1000
		{ID: "2", Date: recent, Symbol: "BBB.AX", Type: "buy", Units: 100, Price: 10}, // short-term gain of 500
		{ID: "3", Date: old, Symbol: "CCC.AX", Type: "buy", Units: 100, Price: 20},    // long-term loss of 500
	}
	d := now.AddDate(0, 0, -1).Format(dateFmt)
	db.Prices["AAA.AX"] = map[string]float64{d: 20}
	db.Prices["BBB.AX"] = map[string]float64{d: 15}
	db.Prices["CCC.AX"] = map[string]float64{d: 15}
	return db
}

func TestSellAllAtTopBracket(t *testing.T) {
	now := time.Now()
	db := sellAllDB(now)
	db.Settings.OtherIncome = 0 // the user's own income is deliberately ignored
	sa, err := SellAllEstimate(db, now.Format(dateFmt), now, DefaultNewLaw(), true)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sa.Rate-0.47) > 1e-6 {
		t.Fatalf("top marginal rate %v, want 0.47", sa.Rate)
	}
	if sa.LongGain != 1000 || sa.ShortGain != 500 || sa.Losses != 500 {
		t.Fatalf("gains: long %v short %v losses %v", sa.LongGain, sa.ShortGain, sa.Losses)
	}
	// the 500 loss wipes out the full-rate gain first, leaving 1000 of long-term gain; half of it is taxed
	if math.Abs(sa.Taxable-500) > 0.01 || math.Abs(sa.Tax-235) > 0.01 {
		t.Fatalf("taxable %v tax %v, want 500 and 235", sa.Taxable, sa.Tax)
	}
	if math.Abs(sa.Est.NetProceeds-5000) > 0.01 || math.Abs(sa.Pocket-4765) > 0.01 {
		t.Fatalf("proceeds %v pocket %v, want 5000 and 4765", sa.Est.NetProceeds, sa.Pocket)
	}
	if len(sa.Rows) != 3 {
		t.Fatalf("rows %+v", sa.Rows)
	}
	if db.Settings.OtherIncome != 0 {
		t.Fatal("the stored settings must not change")
	}
	// holdings are left untouched
	if len(db.Txns) != 3 {
		t.Fatal("estimate must not add transactions")
	}
}

func TestSellAllIgnoresTheUsersOwnBracket(t *testing.T) {
	now := time.Now()
	lowDB, highDB := sellAllDB(now), sellAllDB(now)
	lowDB.Settings.OtherIncome, highDB.Settings.OtherIncome = 20000, 400000
	a, _ := SellAllEstimate(lowDB, now.Format(dateFmt), now, DefaultNewLaw(), true)
	b, _ := SellAllEstimate(highDB, now.Format(dateFmt), now, DefaultNewLaw(), true)
	if math.Abs(a.Tax-b.Tax) > 0.01 {
		t.Fatalf("tax should not depend on the user's income: %v vs %v", a.Tax, b.Tax)
	}
}

func TestSellAllPage(t *testing.T) {
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, httptest.NewRequest("GET", "/estimator?mode=all", nil))
	if r.Code != 200 || !strings.Contains(r.Body.String(), "No open holdings") {
		t.Fatalf("empty: %d", r.Code)
	}
	a.st().Update(func(db *DB) error { *db = *sellAllDB(time.Now()); return nil })
	r = httptest.NewRecorder()
	a.routes().ServeHTTP(r, httptest.NewRequest("GET", "/estimator?mode=all", nil))
	body := r.Body.String()
	for _, want := range []string{"pocket", "Tax owed", "Long-term gains", "Short-term gains", "4,765.00", "235.00"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestEstimatorDefaultsToTopBracketWhenNoIncomeIsSet(t *testing.T) {
	now := time.Now()
	db := sellAllDB(now)
	db.Settings.OtherIncome = 0
	reqs := []SaleReq{{Symbol: "AAA.AX", All: true}} // a 1000 long-term gain
	date := now.Format(dateFmt)

	req := httptest.NewRequest("GET", "/estimator", nil)
	if got := bracketChoice(req, false); got != "top" {
		t.Fatalf("with no income recorded the default should be the top bracket, got %q", got)
	}
	if got := bracketChoice(req, true); got != "mine" {
		t.Fatalf("with an income recorded the default should be mine, got %q", got)
	}
	if got := bracketChoice(httptest.NewRequest("GET", "/estimator?bracket=mine", nil), false); got != "mine" {
		t.Fatalf("an explicit choice wins, got %q", got)
	}

	top, err := EstimateSale(withBracket(db, "top"), reqs, date, "fifo", now)
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := EstimateSale(withBracket(db, "mine"), reqs, date, "fifo", now)
	// 1000 gain, 50% discount, 47% = 235 at the top bracket; with no other income the 500 sits in the tax-free threshold
	if math.Abs(top.Sel.TaxDelta-235) > 0.01 {
		t.Fatalf("top bracket tax %v, want 235", top.Sel.TaxDelta)
	}
	if mine.Sel.TaxDelta >= top.Sel.TaxDelta/2 {
		t.Fatalf("income of $0 should give a far lower figure (%v vs %v)", mine.Sel.TaxDelta, top.Sel.TaxDelta)
	}
	if db.Settings.OtherIncome != 0 {
		t.Fatal("stored settings must not change")
	}
}

func TestEstimatorPageShowsTheAssumption(t *testing.T) {
	a, _ := newApp(t.TempDir() + "/d.json")
	a.st().Update(func(db *DB) error { *db = *sellAllDB(time.Now()); return nil })
	get := func(u string) string {
		r := httptest.NewRecorder()
		a.routes().ServeHTTP(r, httptest.NewRequest("GET", u, nil))
		return r.Body.String()
	}
	body := get("/estimator?run=1&sym=AAA.AX&units=all")
	if !strings.Contains(body, "top bracket") || strings.Contains(body, "only</em> income") {
		t.Error("default with no income should use the top bracket without the low-tax warning")
	}
	body = get("/estimator?run=1&sym=AAA.AX&units=all&bracket=mine")
	if !strings.Contains(body, "$0</b> on the") {
		t.Error("choosing my income with $0 recorded should warn that the tax will look too low")
	}
}

func sellAllWithPrivate(now time.Time) *DB {
	db := sellAllDB(now)
	db.Instruments["PVT-ACME"] = &Instrument{Symbol: "PVT-ACME", Name: "Acme Pty Ltd", Currency: "AUD", Market: MarketManaged, Private: true}
	db.Txns = append(db.Txns, Txn{ID: "p1", Date: now.AddDate(-2, 0, 0).Format(dateFmt), Symbol: "PVT-ACME", Type: "buy", Units: 1000, Price: 2})
	AddCheckpoint(db, "PVT-ACME", Checkpoint{Date: now.AddDate(0, 0, -30).Format(dateFmt), Price: 10}) // worth 10,000, gain 8,000
	return db
}

func TestSellAllCanIncludeOrLeaveOutPrivateShares(t *testing.T) {
	now := time.Now()
	db := sellAllWithPrivate(now)
	date := now.Format(dateFmt)
	with, err := SellAllEstimate(db, date, now, DefaultNewLaw(), true)
	if err != nil {
		t.Fatal(err)
	}
	without, err := SellAllEstimate(db, date, now, DefaultNewLaw(), false)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(with.Est.NetProceeds-without.Est.NetProceeds-10000) > 0.01 {
		t.Fatalf("proceeds with %v, without %v: private shares should add 10,000", with.Est.NetProceeds, without.Est.NetProceeds)
	}
	// 8,000 long-term gain: half is taxed at 47% = 1,880 more
	if math.Abs((with.Tax-without.Tax)-1880) > 0.01 {
		t.Fatalf("extra tax %v, want 1880", with.Tax-without.Tax)
	}
	if !with.IncludePrivate || with.PrivateProceeds != 10000 || len(with.PrivateHeld) != 1 {
		t.Fatalf("with: %+v", with)
	}
	if without.IncludePrivate || without.PrivateProceeds != 0 || without.PrivateCount != 1 || math.Abs(without.PrivateValue-10000) > 0.01 {
		t.Fatalf("without should still report the private value: %+v", without)
	}
	for _, r := range without.Rows {
		if r.Private {
			t.Fatal("private shares must not be in the sale when left out")
		}
	}
	// only private holdings and the box off: nothing to price
	only := &DB{}
	only.init()
	only.Instruments["PVT-ACME"] = db.Instruments["PVT-ACME"]
	only.Txns = []Txn{db.Txns[len(db.Txns)-1]}
	only.Manual = db.Manual
	only.Checkpoints = db.Checkpoints
	if _, err := SellAllEstimate(only, date, now, DefaultNewLaw(), false); err == nil {
		t.Fatal("a portfolio of only private shares with the box off should explain why there is nothing to price")
	}
}

func TestSellAllPrivateTickBox(t *testing.T) {
	a, _ := newApp(t.TempDir() + "/d.json")
	a.st().Update(func(db *DB) error { *db = *sellAllWithPrivate(time.Now()); return nil })
	get := func(u string) string {
		r := httptest.NewRecorder()
		a.routes().ServeHTTP(r, httptest.NewRequest("GET", u, nil))
		return r.Body.String()
	}
	if body := get("/estimator?mode=all"); !strings.Contains(body, "Includes private holdings") || !strings.Contains(body, `name="private" value="1" checked`) {
		t.Error("a plain visit should include private shares, with the box ticked")
	}
	body := get("/estimator?mode=all&sent=1") // the form was submitted with the box unticked
	if strings.Contains(body, "Includes private holdings") || !strings.Contains(body, "left out of this sale") || strings.Contains(body, `name="private" value="1" checked`) {
		t.Error("with the box unticked private shares are left out and reported")
	}
	if body := get("/estimator?mode=all&sent=1&private=1"); !strings.Contains(body, "Includes private holdings") {
		t.Error("ticking the box includes them again")
	}
}
