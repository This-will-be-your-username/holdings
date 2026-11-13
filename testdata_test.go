package main

import (
	"math"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var seedNow = time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)

func seededStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SeedTestData(s, seedNow); err != nil {
		t.Fatal(err)
	}
	return s, path
}

func fps(s *Store) []string {
	var out []string
	s.View(func(db *DB) {
		for _, t := range db.Txns {
			out = append(out, t.FP)
		}
	})
	sort.Strings(out)
	return out
}

func TestSeedDataIsComplete(t *testing.T) {
	s, _ := seededStore(t)
	s.View(func(db *DB) {
		if len(db.Instruments) != 4 || db.Instruments["VAN0110AU"].Market != MarketManaged {
			t.Fatalf("instruments: %+v", db.Instruments)
		}
		types, syms, ids := map[string]int{}, map[string]int{}, map[string]bool{}
		for _, tx := range db.Txns {
			types[tx.Type]++
			syms[tx.Symbol]++
			if tx.Date > seedNow.Format(dateFmt) {
				t.Errorf("transaction in the future: %+v", tx)
			}
			if err := validateTxn(&tx); err != nil {
				t.Errorf("invalid transaction %+v: %v", tx, err)
			}
			if tx.FP == "" || tx.FP != Fingerprint(tx) || tx.ID == "" || ids[tx.ID] {
				t.Errorf("bad fingerprint/id: %+v", tx)
			}
			ids[tx.ID] = true
			if db.Instruments[tx.Symbol] == nil {
				t.Errorf("transaction for unknown instrument %s", tx.Symbol)
			}
		}
		for _, ty := range []string{"buy", "sell", "dividend", "drp"} {
			if types[ty] == 0 {
				t.Errorf("no %s transactions", ty)
			}
		}
		for _, s := range []string{"VGS.AX", "VDGR.AX", "VDHG.AX", "VAN0110AU"} {
			if syms[s] < 10 {
				t.Errorf("%s has only %d transactions", s, syms[s])
			}
		}
		if len(db.Txns) < 120 {
			t.Errorf("only %d transactions", len(db.Txns))
		}
		if db.Settings.OtherIncome <= 0 || db.LastRefresh == "" || len(db.Bench) != 4 {
			t.Errorf("settings/bench missing: %+v %d", db.Settings, len(db.Bench))
		}
		// the import batch record matches the transactions tagged with it
		tagged := 0
		for _, tx := range db.Txns {
			if tx.Import == "sample-import" {
				tagged++
				if tx.Ref == "" {
					t.Error("imported rows should carry a contract-note reference")
				}
			}
		}
		if len(db.Imports) != 1 || db.Imports[0].Added != tagged || tagged == 0 {
			t.Errorf("import record %+v vs %d tagged", db.Imports, tagged)
		}
	})
}

func TestSeedDataPerformanceAndTaxScenarios(t *testing.T) {
	s, _ := seededStore(t)
	s.View(func(db *DB) {
		res := Compute(db, seedNow)
		if len(res.Warnings) != 0 {
			t.Errorf("warnings: %v", res.Warnings)
		}
		if len(res.Holdings) != 4 {
			t.Fatalf("holdings: %d", len(res.Holdings))
		}
		for _, h := range res.Holdings {
			if h.Units <= 0 || h.PriceNote != "" || h.Price <= 0 || h.XIRR == 0 && !h.HasXIRR {
				t.Errorf("holding %s: %+v", h.Symbol, h)
			}
		}
		if res.Total.Franking <= 0 || res.Total.Income <= 0 || res.Total.Realised == 0 {
			t.Errorf("totals: %+v", res.Total)
		}

		tr := ComputeTax(db, res.Rows, seedNow)
		var loss, disc, full int
		for _, e := range tr.All {
			switch {
			case e.Gain < 0:
				loss++
			case e.Discount:
				disc++
			default:
				full++
			}
		}
		if loss == 0 || disc == 0 || full == 0 {
			t.Errorf("CGT scenarios: loss=%d discounted=%d full-rate=%d", loss, disc, full)
		}
		if len(tr.FYs) < 3 {
			t.Errorf("income years: %d", len(tr.FYs))
		}
		var overrides, withFranking, withFTO, withCG, withDeferred int
		for _, tx := range db.Txns {
			if tx.TaxYear != "" {
				overrides++
				if fyOf(tx.Date) == tx.TaxYear {
					t.Errorf("tax-year override should differ from the payment's own year: %+v", tx)
				}
			}
			if tx.Franking > 0 {
				withFranking++
			}
			if tx.ForeignTax > 0 {
				withFTO++
			}
			if tx.CGDisc > 0 {
				withCG++
			}
			if tx.TaxDeferred > 0 {
				withDeferred++
			}
		}
		if overrides == 0 || withFranking == 0 || withFTO == 0 || withCG == 0 || withDeferred == 0 {
			t.Errorf("distribution components: override=%d franking=%d fto=%d cg=%d deferred=%d", overrides, withFranking, withFTO, withCG, withDeferred)
		}
		// a real estimate falls out of it
		cur := tr.ByFY[fyOf(seedNow.Format(dateFmt))]
		if cur == nil || cur.Franking <= 0 && len(tr.FYs) < 3 {
			t.Errorf("current year: %+v", cur)
		}

		// distributions the user has not recorded yet are suggested, with franking where the ASX would give it
		got := map[string]bool{}
		franked := false
		for _, sg := range res.Suggested {
			got[sg.Symbol] = true
			if sg.FrankPct > 0 {
				franked = true
			}
		}
		for _, sym := range []string{"VGS.AX", "VDGR.AX", "VDHG.AX"} {
			if !got[sym] {
				t.Errorf("no suggested distribution for %s: %+v", sym, res.Suggested)
			}
		}
		if !franked {
			t.Error("expected a franking estimate on at least one suggestion")
		}

		// the sell estimator and withdrawal planner work on it
		if _, err := WithdrawalPlans(db, WithdrawOpts{Target: 4000, Brokerage: 9.5, Strategy: "least"}, seedNow); err != nil {
			t.Errorf("withdrawal: %v", err)
		}
		if _, err := EstimateSale(db, []SaleReq{{Symbol: "VGS.AX", All: true}}, seedNow.Format(dateFmt), "fifo", seedNow); err != nil {
			t.Errorf("estimate: %v", err)
		}

		// outlook can model every holding, including the managed fund (weekly unit prices)
		o := ComputeOutlook(db, res, "blend", seedNow)
		if o.Portfolio == nil || len(o.Portfolio.Included) != 4 || len(o.Portfolio.Flat) != 0 {
			t.Fatalf("portfolio outlook: %+v", o.Portfolio)
		}
		for _, a := range o.Assets {
			if !a.Enough || a.Beta <= 0 || a.Vol60 <= 0 {
				t.Errorf("asset outlook %s: %+v", a.Symbol, a)
			}
		}
		if len(o.Market) != 4 {
			t.Errorf("market cards: %d", len(o.Market))
		}
	})
}

func TestSeedDataPersistsAndIsDeterministic(t *testing.T) {
	s, path := seededStore(t)
	before := fps(s)
	// reopen from disk, as a restart would
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	after := fps(s2)
	if len(before) == 0 || len(before) != len(after) {
		t.Fatalf("persisted %d of %d", len(after), len(before))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatal("transactions changed across a reload")
		}
	}
	// user edits persist like real data...
	if err := s2.Update(func(db *DB) error {
		db.Txns = append(db.Txns, Txn{ID: "x", Date: "2026-11-01", Symbol: "VGS.AX", Type: "buy", Units: 1, Price: 160})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s3, _ := OpenStore(path)
	if len(fps(s3)) != len(before)+1 {
		t.Error("an added transaction should persist")
	}
	// ...and a reset brings back exactly the original dataset
	if err := SeedTestData(s3, seedNow); err != nil {
		t.Fatal(err)
	}
	again := fps(s3)
	if len(again) != len(before) {
		t.Fatalf("reset: %d vs %d", len(again), len(before))
	}
	for i := range before {
		if before[i] != again[i] {
			t.Fatal("reseeding should be deterministic")
		}
	}
}

func TestSeedDataDoesNotTouchOtherStores(t *testing.T) {
	realPath := filepath.Join(t.TempDir(), "real.json")
	real, _ := OpenStore(realPath)
	_ = real.Update(func(db *DB) error {
		db.Txns = []Txn{{ID: "r1", Date: "2025-01-02", Symbol: "VAS.AX", Type: "buy", Units: 1, Price: 100}}
		return nil
	})
	test, _ := seededStore(t)
	_ = test
	real2, _ := OpenStore(realPath)
	real2.View(func(db *DB) {
		if len(db.Txns) != 1 || db.Txns[0].ID != "r1" {
			t.Error("real data must be unaffected by seeding a different store")
		}
	})
}

func TestSeedPricesAreAllPositiveAndChartsSerialise(t *testing.T) {
	s, _ := seededStore(t)
	s.View(func(db *DB) {
		for sym, m := range db.Manual {
			for d, v := range m {
				if v <= 0 {
					t.Errorf("manual price %s %s = %v", sym, d, v)
				}
			}
		}
		for sym, m := range db.Prices {
			for d, v := range m {
				if v <= 0 {
					t.Errorf("price %s %s = %v", sym, d, v)
				}
			}
		}
		res := Compute(db, seedNow)
		o := ComputeOutlook(db, res, "blend", seedNow)
		if js := string(chartJS(o.Charts)); js == "{}" {
			t.Error("chart data failed to serialise (NaN/Inf)")
		}
	})
}

// a bad (zero) price entered by hand must not take the Outlook page down
func TestOutlookToleratesZeroPrices(t *testing.T) {
	s, _ := seededStore(t)
	s.View(func(db *DB) {
		db.Manual["VAN0110AU"]["2025-03-03"] = 0
		db.Prices["VGS.AX"]["2025-03-03"] = 0
		res := Compute(db, seedNow)
		o := ComputeOutlook(db, res, "blend", seedNow)
		if js := string(chartJS(o.Charts)); js == "{}" || o.Portfolio == nil {
			t.Error("zero prices should be ignored, not break the outlook")
		}
		delete(db.Manual["VAN0110AU"], "2025-03-03")
		delete(db.Prices["VGS.AX"], "2025-03-03")
	})
}

func TestSeedDataHasATargetAllocationWorthRebalancing(t *testing.T) {
	s, _ := seededStore(t)
	s.View(func(db *DB) {
		var sum float64
		for _, in := range db.Instruments {
			if in.Role == "" || in.TargetPct <= 0 {
				t.Errorf("%s has no purpose/target", in.Symbol)
			}
			sum += in.TargetPct
		}
		if math.Abs(sum-100) > 1e-9 {
			t.Errorf("targets add to %v", sum)
		}
		res := Compute(db, seedNow)
		w := ComputeWeights(db, res)
		if !w.AnySet {
			t.Fatal("targets should be in force")
		}
		// the managed fund has outgrown its 15% target and VDGR sits under its 25%
		if van, vdgr := w.Info["VAN0110AU"], w.Info["VDGR.AX"]; van.Cur <= van.Target+0.03 || vdgr.Cur >= vdgr.Target-0.03 {
			t.Errorf("expected drift: VAN %v vs %v, VDGR %v vs %v", van.Cur, van.Target, vdgr.Cur, vdgr.Target)
		}
		// a withdrawal sells the overweight fund and leaves the underweight one alone
		wr, err := WithdrawalPlans(db, WithdrawOpts{Target: 3000, Brokerage: 9.5}, seedNow)
		if err != nil {
			t.Fatal(err)
		}
		sold := soldBy(wr.Sel)
		if sold["VAN0110AU"] == 0 || sold["VDGR.AX"] != 0 {
			t.Errorf("sold %v", sold)
		}
		if wr.Sel.Plan.MaxDriftAfter >= wr.Sel.Plan.MaxDriftBefore {
			t.Errorf("drift %v -> %v", wr.Sel.Plan.MaxDriftBefore, wr.Sel.Plan.MaxDriftAfter)
		}
	})
}
