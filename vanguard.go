package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Vanguard Australia publishes unit prices on each fund's "Prices and distributions" page. That page loads them
// from Vanguard's own site API, which is what this reads (the same public data the page shows, one request per
// fund):
//
//	GET {base}/products/personal/funds/allfunds      the fund list: portId, name and APIR code
//	GET {base}/products/personal/fund/{portId}/prices daily NAV history (and buy/sell prices)
//
// The unit price used for valuation is the NAV. The APIR code you hold the fund under (e.g. VAN0108AU) is mapped to
// Vanguard's portId automatically, or you can set the portId yourself.

var vanguardBase = "https://www.vanguard.com.au/personal/api"

var reAPIRCode = regexp.MustCompile(`^[A-Z]{3}\d{4}AU$`)

func isAPIR(s string) bool { return reAPIRCode.MatchString(strings.ToUpper(strings.TrimSpace(s))) }

type VGFund struct{ PortID, Name, APIR, Type string }

var vgCache struct {
	sync.Mutex
	at     time.Time
	byAPIR map[string]VGFund
}

// vgFunds returns Vanguard's managed funds keyed by APIR code (cached for 12 hours).
func vgFunds() (map[string]VGFund, error) {
	vgCache.Lock()
	defer vgCache.Unlock()
	if vgCache.byAPIR != nil && time.Since(vgCache.at) < 12*time.Hour {
		return vgCache.byAPIR, nil
	}
	b, err := httpGet(vanguardBase + "/products/personal/funds/allfunds")
	if err != nil {
		return nil, fmt.Errorf("Vanguard fund list: %w", err)
	}
	var r struct {
		Data []struct {
			PortID      string `json:"portId"`
			FundName    string `json:"fundName"`
			ProductType string `json:"productType"`
			Symbols     struct {
				APIR string `json:"APIR"`
			} `json:"tradingSymbols"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("Vanguard fund list: bad response: %w", err)
	}
	m := map[string]VGFund{}
	for _, f := range r.Data {
		if f.Symbols.APIR != "" {
			m[strings.ToUpper(f.Symbols.APIR)] = VGFund{f.PortID, f.FundName, strings.ToUpper(f.Symbols.APIR), f.ProductType}
		}
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("Vanguard fund list contained no funds")
	}
	vgCache.byAPIR, vgCache.at = m, time.Now()
	return m, nil
}

// vgResolve finds the portId for an instrument: the one already saved, or looked up by APIR code.
func vgResolve(inst *Instrument) (VGFund, error) {
	if inst.VanguardPort != "" {
		return VGFund{PortID: inst.VanguardPort, Name: inst.Name}, nil
	}
	if !isAPIR(inst.Symbol) {
		return VGFund{}, fmt.Errorf("%s is not an APIR code, so Vanguard's fund can't be looked up - set its Vanguard port ID", inst.Symbol)
	}
	funds, err := vgFunds()
	if err != nil {
		return VGFund{}, err
	}
	f, ok := funds[strings.ToUpper(inst.Symbol)]
	if !ok {
		return VGFund{}, fmt.Errorf("%s isn't a Vanguard Australia managed fund (not in Vanguard's fund list)", inst.Symbol)
	}
	return f, nil
}

// fetchVGPrices returns the fund's NAV by date (YYYY-MM-DD).
func fetchVGPrices(portID string) (map[string]float64, error) {
	if !regexp.MustCompile(`^\d{3,6}$`).MatchString(portID) {
		return nil, fmt.Errorf("invalid Vanguard port ID %q", portID)
	}
	b, err := httpGet(vanguardBase + "/products/personal/fund/" + url.PathEscape(portID) + "/prices")
	if err != nil {
		return nil, fmt.Errorf("Vanguard prices for port %s: %w", portID, err)
	}
	var r struct {
		Data []struct {
			PortID string `json:"portId"`
			Navs   []struct {
				Date    string  `json:"asOfDate"`
				Price   float64 `json:"price"`
				Measure string  `json:"measureTypeCode"`
			} `json:"navPrices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("Vanguard prices: bad response: %w", err)
	}
	out := map[string]float64{}
	for _, f := range r.Data {
		for _, n := range f.Navs {
			if n.Price > 0 && (n.Measure == "" || n.Measure == "NAV") {
				if d, err := time.Parse(dateFmt, n.Date); err == nil {
					out[d.Format(dateFmt)] = n.Price
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Vanguard returned no prices for port %s", portID)
	}
	return out, nil
}

// managedFundsToUpdate lists unlisted instruments that are held and due for a price update.
func managedDue(db *DB, force bool, now time.Time) []*Instrument {
	var out []*Instrument
	seen := map[string]bool{}
	for _, t := range db.Txns {
		seen[t.Symbol] = true
	}
	for sym, inst := range db.Instruments {
		if inst.Market != MarketManaged || !seen[sym] || (!isAPIR(sym) && inst.VanguardPort == "") {
			continue
		}
		if at, err := time.Parse(time.RFC3339, db.PriceAt["vg:"+sym]); !force && err == nil && now.Sub(at) < 6*time.Hour {
			continue // Vanguard publishes one NAV a day; no need to ask more often than a few times a day
		}
		cp := *inst
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}
