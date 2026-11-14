package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Every page must parse and render against realistic (seeded) data without error. This is the net that
// catches template mistakes, missing helpers and handler panics before they reach a browser.
func TestEveryPageRendersOnTestData(t *testing.T) {
	t.Setenv("TEST_MODE", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "fundtrack.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	raise := "/estimator?mode=raise&run=1&only_sent=1&amount=3000&brokerage=9.5&strategy=balance&only=VGS.AX&only=VDHG.AX&only=VDGR.AX&only=VAN0110AU"
	pages := []struct{ path, want string }{
		{"/", "Your portfolio"},
		{"/transactions", "Import from your broker"},
		{"/distributions", "Distributions"},
		{"/tax", "Tax"},
		{"/tax?fy=2024-25", "Tax"},
		{"/outlook", "Outlook"},
		{"/outlook?scenario=trend", "Outlook"},
		{"/prices", "Fund prices"},
		{"/plan", "Target allocation"},
		{"/estimator", "Sell estimator"},
		{"/estimator?run=1&sym=VGS.AX&units=10&fees=9.5&method=fifo", "Before or after the 1 July 2027 CGT change?"},
		{"/estimator?run=1&sym=VGS.AX&sym=VDHG.AX&units=all&units=5&method=hifo&post_date=2029-07-01&post_growth=8&cpi=3", "Indexation relief"},
		{"/estimator?mode=raise", "Withdraw an amount"},
		{raise, "Before or after the 1 July 2027 CGT change?"},
		{raise + "&strategy=tax&after_tax=1", "Compared with the other way of choosing"},
		{"/export/transactions.csv", "date,symbol"},
		{"/export/holdings.csv", "symbol,name"},
		{"/export/tax-summary.csv", "income_year"},
		{"/export/tax-cgt.csv", "income_year"},
		{"/export/tax-distributions.csv?fy=2025-26", "income_year"},
		{"/test/sample.csv", "Contract Note"},
		{"/static/charts.js", "FundTrack charts"},
	}
	for _, p := range pages {
		resp, err := http.Get(srv.URL + p.path)
		if err != nil {
			t.Fatalf("%s: %v", p.path, err)
		}
		var sb strings.Builder
		buf := make([]byte, 32<<10)
		for {
			n, e := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if e != nil {
				break
			}
		}
		resp.Body.Close()
		body := sb.String()
		if resp.StatusCode != 200 {
			t.Errorf("%s: HTTP %d", p.path, resp.StatusCode)
			continue
		}
		if !strings.Contains(body, p.want) {
			t.Errorf("%s: missing %q", p.path, p.want)
		}
		for _, bad := range []string{"flash bad", "<no value>", "%!", "{{"} {
			if strings.Contains(body, bad) && !strings.HasSuffix(p.path, ".csv") && !strings.HasSuffix(p.path, ".js") {
				t.Errorf("%s: page contains %q", p.path, bad)
			}
		}
	}
}
