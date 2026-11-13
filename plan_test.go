package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIntentScore(t *testing.T) {
	growth := Intent{Set: true, Expected: 8, Horizon: 10}
	short := Intent{Set: true, Expected: 3, Horizon: 1}
	cases := []struct {
		name                  string
		xirr                  float64
		has                   bool
		years                 float64
		in                    Intent
		gap, conf, tilt, tolt float64
	}{
		// a 10-year growth holding two years in: judged at 20% weight
		{"long-term, early, behind", 2, true, 2, growth, -6, 0.2, -6 * 0.2 * 0.01, 1e-9},
		// a 1-year holding two years in: judged in full
		{"short-term, behind", -3, true, 2, short, -6, 1, -0.06, 1e-9},
		{"capped", -30, true, 5, short, -33, 1, -0.10, 1e-9},
		{"ahead of goal", 14, true, 10, growth, 6, 1, 0.06, 1e-9},
		{"no intent stated", 2, true, 2, Intent{}, 0, 0, 0, 0},
		{"no return measurable yet", 0, false, 0.01, growth, 0, 0, 0, 0},
	}
	for _, c := range cases {
		gap, conf, tilt := intentScore(c.xirr, c.has, c.years, c.in)
		if math.Abs(gap-c.gap) > 1e-9 || math.Abs(conf-c.conf) > 1e-9 || math.Abs(tilt-c.tilt) > c.tolt+1e-12 {
			t.Errorf("%s: gap %v conf %v tilt %v, want %v %v %v", c.name, gap, conf, tilt, c.gap, c.conf, c.tilt)
		}
	}
}

func TestIntentDefaultsAndOverrides(t *testing.T) {
	if intentOf(nil).Set || intentOf(&Instrument{}).Set {
		t.Error("no role and no expected return means no stated intent")
	}
	in := intentOf(&Instrument{Role: "growth"})
	if !in.Set || in.Expected != 8 || in.Horizon != 10 {
		t.Errorf("%+v", in)
	}
	in = intentOf(&Instrument{Role: "growth", ExpectedPct: 9.5, HorizonYrs: 20})
	if in.Expected != 9.5 || in.Horizon != 20 {
		t.Errorf("overrides: %+v", in)
	}
	in = intentOf(&Instrument{ExpectedPct: 5}) // expected return alone is a stated intent
	if !in.Set || in.Expected != 5 || in.Horizon != 7 {
		t.Errorf("%+v", in)
	}
	if intentOf(&Instrument{Role: "bogus"}).Set {
		t.Error("an unknown role isn't an intent")
	}
}

// two holdings, both flat for two years (0% a year), equal size and equal targets
func flatDB(h1Horizon float64, role1 string) *DB {
	db := newTestDB()
	db.Settings.OtherIncome = 100000
	db.Instruments["P.AX"] = &Instrument{Symbol: "P.AX", Name: "P", Currency: "AUD", Market: MarketASX, TargetPct: 50, Role: role1, HorizonYrs: h1Horizon}
	db.Instruments["Q.AX"] = &Instrument{Symbol: "Q.AX", Name: "Q", Currency: "AUD", Market: MarketASX, TargetPct: 50}
	for _, s := range []string{"P.AX", "Q.AX"} {
		db.Prices[s] = map[string]float64{"2026-11-13": 100}
		db.Txns = append(db.Txns, Txn{Date: "2024-11-13", Symbol: s, Type: "buy", Units: 50, Price: 100})
	}
	return db
}

func TestLongTermHoldingsAreNotPunishedForAShortPatch(t *testing.T) {
	sellP := func(db *DB) float64 {
		res, err := WithdrawalPlans(db, WithdrawOpts{Target: 1000}, wNow)
		if err != nil {
			t.Fatal(err)
		}
		return soldBy(res.Sel)["P.AX"]
	}
	// no stated intent: identical holdings, so the sale splits evenly
	none := flatDB(0, "")
	even := sellP(none)
	if math.Abs(even-500) > 100 {
		t.Fatalf("even split expected, P sold %v", even)
	}
	// P is a 10-year growth holding that has returned 0% a year for 2 years: well behind its 8% goal,
	// but only 20% of the way through its horizon, so the verdict barely counts
	long := sellP(flatDB(10, "growth"))
	// the same shortfall on a 1-year horizon is judged in full and P is trimmed harder
	brief := sellP(flatDB(1, "growth"))
	if !(long < brief) {
		t.Errorf("a long horizon should be sold less than a short one: long %v, brief %v", long, brief)
	}
	if brief <= even {
		t.Errorf("a holding fully behind its goal should be sold a bit more than an even split: %v vs %v", brief, even)
	}
	// the nudge is modest: even the harshest verdict never overrides diversification (target stays within 10%)
	if brief > 750 {
		t.Errorf("performance should only nudge, got %v", brief)
	}
	w := ComputeWeights(flatDB(10, "growth"), Compute(flatDB(10, "growth"), wNow))
	p, q := w.Info["P.AX"], w.Info["Q.AX"]
	if !(p.Eff < q.Eff) || p.Conf < 0.15 || p.Conf > 0.25 || p.Gap > -7.5 {
		t.Errorf("intent info: %+v %+v", p, q)
	}
	if math.Abs(p.Eff+q.Eff-1) > 1e-9 {
		t.Error("effective targets must sum to 1")
	}
}

func TestOutperformingItsGoalRaisesTheTarget(t *testing.T) {
	db := flatDB(2, "short") // expects 3%/yr, has returned 0 -> behind
	db.Prices["P.AX"]["2026-11-13"] = 130
	db.Instruments["P.AX"].ExpectedPct = 5 // 30% over 2 years (~14%/yr) vs 5% expected -> well ahead
	db.Instruments["P.AX"].Role = "balanced"
	w := ComputeWeights(db, Compute(db, wNow))
	if p := w.Info["P.AX"]; p.Gap <= 0 || p.Tilt <= 0 || p.Eff <= p.Target {
		t.Errorf("ahead of its goal should be held a little more: %+v", p)
	}
}

func TestYearsHeldAndHoldingInfoPlumbing(t *testing.T) {
	db := flatDB(0, "")
	res := Compute(db, wNow)
	for _, h := range res.Holdings {
		if math.Abs(h.YearsHeld-2.0) > 0.01 {
			t.Errorf("%s years held %v", h.Symbol, h.YearsHeld)
		}
		if !h.HasXIRR {
			t.Errorf("%s needs an XIRR", h.Symbol)
		}
	}
}

func TestPlanHandlerSavesAndValidates(t *testing.T) {
	s, _ := seededStore(t)
	a := &App{}
	a.real = s
	post := func(v url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/plan", strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		a.savePlan(rec, req)
		return rec
	}
	good := url.Values{"sym": {"VGS.AX", "VDHG.AX"},
		"role_VGS.AX": {"growth"}, "target_VGS.AX": {"35"}, "horizon_VGS.AX": {"15"}, "expected_VGS.AX": {"8.5"},
		"role_VDHG.AX": {"nonsense"}, "target_VDHG.AX": {"30"}}
	rec := post(good)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Location"))
	}
	s.View(func(db *DB) {
		g, d := db.Instruments["VGS.AX"], db.Instruments["VDHG.AX"]
		if g.Role != "growth" || g.TargetPct != 35 || g.HorizonYrs != 15 || g.ExpectedPct != 8.5 {
			t.Errorf("VGS %+v", g)
		}
		if d.Role != "" || d.TargetPct != 30 {
			t.Errorf("an unknown role must be dropped: %+v", d)
		}
	})
	// out-of-range values are rejected and nothing is changed
	rec = post(url.Values{"sym": {"VGS.AX"}, "target_VGS.AX": {"150"}})
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Error("a 150% target should be rejected")
	}
	s.View(func(db *DB) {
		if db.Instruments["VGS.AX"].TargetPct != 35 {
			t.Error("a rejected save must not change data")
		}
	})
}
