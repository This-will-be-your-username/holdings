package main

import (
	"math"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// two owners in very different brackets, each with a 1000 long-term gain (100 units, 10 -> 20, held two years)
func ownersDB(now time.Time) *DB {
	db := sellAllDB(now) // AAA, BBB, CCC as before (AAA: 1000 long gain)
	db.Settings.OtherIncome = 0
	db.Owners = []Owner{{Name: "Sam", OtherIncome: 200000}, {Name: "Alex", OtherIncome: 0}}
	db.Instruments["AAA.AX"].Owner = "Sam"
	db.Instruments["BBB.AX"].Owner = "Alex"
	// give BBB the same long-term 1000 gain as AAA so the two are comparable
	old := now.AddDate(-2, 0, 0).Format(dateFmt)
	for i := range db.Txns {
		if db.Txns[i].Symbol == "BBB.AX" {
			db.Txns[i].Date = old
		}
	}
	d := now.AddDate(0, 0, -1).Format(dateFmt)
	db.Prices["BBB.AX"] = map[string]float64{d: 20}
	return db
}

func TestSaleAcrossOwnersIsTaxedInEachOwnersBracket(t *testing.T) {
	now := time.Now()
	db := ownersDB(now)
	date := now.Format(dateFmt)
	both := []SaleReq{{Symbol: "AAA.AX", All: true}, {Symbol: "BBB.AX", All: true}}
	est, err := EstimateSale(db, both, date, "fifo", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(est.Parts) != 2 {
		t.Fatalf("expected one part per owner, got %d", len(est.Parts))
	}
	byOwner := map[string]*Estimate{}
	for _, p := range est.Parts {
		byOwner[p.Owner] = p
	}
	// Sam: 500 taxable on top of 200,000 -> top rate 47% = 235. Alex: 500 taxable, nothing else -> only the 2% Medicare levy = 10
	if got := byOwner["Sam"].Sel.TaxDelta; math.Abs(got-235) > 0.01 {
		t.Fatalf("Sam's tax %v, want 235", got)
	}
	if got := byOwner["Alex"].Sel.TaxDelta; math.Abs(got-10) > 0.01 {
		t.Fatalf("Alex's tax %v, want 10", got)
	}
	if math.Abs(est.Sel.TaxDelta-245) > 0.01 || math.Abs(est.NetProceeds-4000) > 0.01 || math.Abs(est.Sel.AfterTax-3755) > 0.01 {
		t.Fatalf("totals: tax %v proceeds %v after-tax %v", est.Sel.TaxDelta, est.NetProceeds, est.Sel.AfterTax)
	}
	if len(est.Lines) != 2 || est.Lines[0].Owner == est.Lines[1].Owner {
		t.Fatalf("lines should carry their owners: %+v", est.Lines)
	}
	// a sale that touches one owner uses only that owner's assumptions
	one, _ := EstimateSale(db, both[:1], date, "fifo", now)
	if len(one.Parts) != 0 || one.Owner != "Sam" || math.Abs(one.Sel.TaxDelta-235) > 0.01 {
		t.Fatalf("single owner: %+v", one.Sel)
	}
	two, _ := EstimateSale(db, both[1:], date, "fifo", now)
	if math.Abs(two.Sel.TaxDelta-10) > 0.01 {
		t.Fatalf("Alex alone: %v", two.Sel.TaxDelta)
	}
}

func TestTopBracketOverridesEveryOwner(t *testing.T) {
	now := time.Now()
	db := ownersDB(now)
	both := []SaleReq{{Symbol: "AAA.AX", All: true}, {Symbol: "BBB.AX", All: true}}
	est, err := EstimateSale(withBracket(db, "top"), both, now.Format(dateFmt), "fifo", now)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(est.Sel.TaxDelta-470) > 0.01 { // 235 each
		t.Fatalf("top bracket for both owners: %v, want 470", est.Sel.TaxDelta)
	}
	if db.Owners[1].OtherIncome != 0 {
		t.Fatal("the stored owners must not change")
	}
}

func TestOwnerViewsKeepInvestmentsApartAndFallBackToDefault(t *testing.T) {
	now := time.Now()
	db := ownersDB(now)
	view := db.ownerView("Sam")
	res := Compute(view, now)
	if len(res.Holdings) != 1 || res.Holdings[0].Symbol != "AAA.AX" || res.Holdings[0].Owner != "Sam" {
		t.Fatalf("Sam should see only AAA: %+v", res.Holdings)
	}
	if def := Compute(db.ownerView(""), now); len(def.Holdings) != 1 || def.Holdings[0].Symbol != "CCC.AX" {
		t.Fatalf("the default owner keeps the untagged holding: %+v", def.Holdings)
	}
	// deleting an owner returns their investments to the default owner
	db.Owners = db.Owners[1:] // Sam removed
	if db.ownerOf("AAA.AX") != "" {
		t.Fatal("an investment tagged to a deleted owner belongs to the default owner")
	}
	// settings: an owner's own income, the default owner's untouched
	if s := db.settingsFor("Alex"); s.OtherIncome != 0 {
		t.Fatalf("Alex %+v", s)
	}
	if s := ownersDB(now).settingsFor("Sam"); s.OtherIncome != 200000 {
		t.Fatalf("Sam %+v", s)
	}
}

func TestNoOwnersBehavesAsBefore(t *testing.T) {
	now := time.Now()
	db := sellAllDB(now)
	db.Settings.OtherIncome = 200000
	est, err := EstimateSale(db, []SaleReq{{Symbol: "AAA.AX", All: true}}, now.Format(dateFmt), "fifo", now)
	if err != nil || len(est.Parts) != 0 || math.Abs(est.Sel.TaxDelta-235) > 0.01 {
		t.Fatalf("%v %+v", err, est.Sel)
	}
}

func postForm(t *testing.T, a *App, path string, v url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, req)
	return r
}

func getPage(a *App, u string) string {
	r := httptest.NewRecorder()
	a.routes().ServeHTTP(r, httptest.NewRequest("GET", u, nil))
	return r.Body.String()
}

func TestOwnersThroughThePages(t *testing.T) {
	now := time.Now()
	a, err := newApp(t.TempDir() + "/d.json")
	if err != nil {
		t.Fatal(err)
	}
	a.st().Update(func(db *DB) error { *db = *sellAllDB(now); return nil })

	if r := postForm(t, a, "/owners", url.Values{"name": {"Sam"}}); strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatalf("add owner: %s", r.Header().Get("Location"))
	}
	postForm(t, a, "/owners", url.Values{"name": {"Alex"}})
	if r := postForm(t, a, "/owners", url.Values{"name": {"sam"}}); !strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatal("a duplicate owner name (any case) should be refused")
	}
	if r := postForm(t, a, "/owners", url.Values{"name": {"Unassigned"}}); !strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatal("the reserved name should be refused")
	}
	// assign investments, and give each owner their own assumptions
	postForm(t, a, "/owners/assign", url.Values{"owner_AAA.AX": {"Sam"}, "owner_BBB.AX": {"Alex"}})
	postForm(t, a, "/settings", url.Values{"owner": {"Sam"}, "other_income": {"200000"}, "opening_loss": {"0"}, "cgt_method": {"fifo"}})
	postForm(t, a, "/settings", url.Values{"owner": {"Alex"}, "other_income": {"0"}, "opening_loss": {"0"}, "cgt_method": {"fifo"}})
	a.st().View(func(db *DB) {
		if db.Instruments["AAA.AX"].Owner != "Sam" || db.Instruments["BBB.AX"].Owner != "Alex" || db.Owners[0].OtherIncome != 200000 {
			t.Fatalf("saved state: %+v %+v", db.Owners, db.Instruments["AAA.AX"])
		}
		if db.Settings.OtherIncome != 0 {
			t.Fatal("the default owner's income must be unchanged")
		}
	})
	if r := postForm(t, a, "/owners/assign", url.Values{"owner_AAA.AX": {"Nobody"}}); !strings.Contains(r.Header().Get("Location"), "err=") {
		t.Fatal("assigning to an unknown owner should be refused")
	}

	// the tax page shows a tab and an assumptions panel per owner, plus the all-owners summary
	body := getPage(a, "/tax?owner=Sam")
	for _, want := range []string{"Sam&rsquo;s assumptions", "All owners", "Unassigned", "Alex", "Who holds what"} {
		if !strings.Contains(body, want) {
			t.Errorf("tax page missing %q", want)
		}
	}
	if strings.Contains(getPage(a, "/tax?owner=Alex"), "200000") {
		t.Error("Alex's panel must not show Sam's income")
	}

	// the estimator taxes a mixed sale per owner (reusing the same 1,000 long-term gains)
	body = getPage(a, "/estimator?run=1&bracket=mine&sym=AAA.AX&units=all&sym=BBB.AX&units=all")
	for _, want := range []string{"Tax by owner", "Sam", "Alex", "All owners"} {
		if !strings.Contains(body, want) {
			t.Errorf("estimator missing %q", want)
		}
	}
	// the sell-everything page splits by owner too
	if body := getPage(a, "/estimator?mode=all"); !strings.Contains(body, "By owner") {
		t.Error("sell everything should list tax by owner")
	}

	// removing an owner hands their investments back
	postForm(t, a, "/owners/delete", url.Values{"name": {"Sam"}})
	a.st().View(func(db *DB) {
		if db.ownerOf("AAA.AX") != "" || len(db.Owners) != 1 || db.Instruments["AAA.AX"].Owner != "" {
			t.Fatalf("after delete: %+v / %q", db.Owners, db.Instruments["AAA.AX"].Owner)
		}
	})
}
