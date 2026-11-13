package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const dateFmt = "2006-01-02"

// Market identifies where an instrument's prices come from.
const (
	MarketASX     = "ASX"     // Yahoo Finance, symbol suffixed .AX
	MarketUS      = "US"      // Yahoo Finance, bare ticker
	MarketManaged = "MANAGED" // unlisted fund (e.g. Vanguard managed fund): manual unit prices
)

type Instrument struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Market   string `json:"market"`
}

// Txn is a single user transaction. Price/Amount are in the instrument's
// currency; Fees are always AUD (brokerage is normally charged in AUD).
// FX is AUD per 1 unit of instrument currency; 0 means "look it up".
type Txn struct {
	ID     string  `json:"id"`
	Date   string  `json:"date"`
	Symbol string  `json:"symbol"`
	Type   string  `json:"type"` // buy | sell | dividend | drp
	Units  float64 `json:"units"`
	Price  float64 `json:"price"`
	Fees   float64 `json:"fees"`
	Amount float64 `json:"amount"`
	FX     float64 `json:"fx"`
	Notes  string  `json:"notes"`

	// Distribution tax details (dividend / drp only). All AUD except
	// ForeignTax, which is in the fund's currency.
	Franked     float64 `json:"franked,omitempty"`      // franked portion of the distribution
	Franking    float64 `json:"franking,omitempty"`     // franking credits attached
	ForeignTax  float64 `json:"foreign_tax,omitempty"`  // foreign tax: withheld from Amount (foreign-currency funds) or offset gross-up (AUD funds)
	CGDisc      float64 `json:"cg_disc,omitempty"`      // capital gain component eligible for the 50% discount (gross)
	CGOther     float64 `json:"cg_other,omitempty"`     // other capital gain component (gross)
	TaxDeferred float64 `json:"tax_deferred,omitempty"` // tax-deferred / return of capital (reduces cost base)
	TaxYear     string  `json:"tax_year,omitempty"`     // override income year, e.g. 2025-26

	// Provenance, used to recognise the same transaction in later uploads.
	FP     string `json:"fp,omitempty"`     // content fingerprint (see fingerprint.go)
	Ref    string `json:"ref,omitempty"`    // institution reference (contract note / transaction id), if the file had one
	Import string `json:"import,omitempty"` // id of the CSV import batch that created it
}

// ImportRec records one CSV import so it can be listed, recognised if uploaded again, and undone.
type ImportRec struct {
	ID       string `json:"id"`
	At       string `json:"at"`
	File     string `json:"file"`
	Hash     string `json:"hash"` // sha256 of the file bytes
	Rows     int    `json:"rows"`
	Added    int    `json:"added"`
	Dupes    int    `json:"dupes"`
	Possible int    `json:"possible"`
	Skipped  int    `json:"skipped"`
}

// Settings are the user's tax assumptions.
type Settings struct {
	OtherIncome     float64           `json:"other_income"`     // taxable income from everything else (salary etc.)
	CGTMethod       string            `json:"cgt_method"`       // fifo | hifo | optimal
	ExcludeFranking bool              `json:"exclude_franking"` // leave franking credits out of return figures
	OpeningLoss     float64           `json:"opening_loss"`     // net capital losses brought forward before your first year of data
	EstMethod       string            `json:"-"`                // runtime only: parcel method for hypothetical sales (sale estimator)
	EstPicks        map[string][]Pick `json:"-"`                // runtime only: exact parcels for hypothetical sales, by transaction id
}

// Pick says "sell this many units from parcel number Seq of the holding" (Seq is the
// order the holding's parcels were created in). Used by the withdrawal planner.
type Pick struct {
	Seq   int
	Units float64
}

// DivInfo is distribution detail from the ASX (pay date, franking %).
type DivInfo struct {
	Pay      string  `json:"pay"`
	PerUnit  float64 `json:"per_unit"`
	Franking float64 `json:"franking"` // percent
}

type DB struct {
	Imports     []ImportRec                   `json:"imports"`
	Settings    Settings                      `json:"settings"`
	DivInfo     map[string]map[string]DivInfo `json:"div_info"` // symbol -> ex-date -> info
	Bench       map[string]map[string]float64 `json:"bench"`    // market benchmarks (indices, FX, VIX): symbol -> date -> close
	Instruments map[string]*Instrument        `json:"instruments"`
	Txns        []Txn                         `json:"txns"`
	Prices      map[string]map[string]float64 `json:"prices"` // fetched closes: symbol -> date -> price
	Manual      map[string]map[string]float64 `json:"manual"` // user-entered prices (override fetched)
	Divs        map[string]map[string]float64 `json:"divs"`   // symbol -> ex-date -> per-unit dividend
	FX          map[string]map[string]float64 `json:"fx"`     // currency -> date -> AUD per unit
	LastRefresh string                        `json:"last_refresh"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	db   DB
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	s.db.init()
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.db); err != nil {
			return nil, err
		}
		s.db.init()
		// fingerprint anything recorded before fingerprints existed (manual entries, older imports)
		for i := range s.db.Txns {
			if s.db.Txns[i].FP == "" {
				s.db.Txns[i].FP = Fingerprint(s.db.Txns[i])
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (d *DB) init() {
	if d.Instruments == nil {
		d.Instruments = map[string]*Instrument{}
	}
	if d.Prices == nil {
		d.Prices = map[string]map[string]float64{}
	}
	if d.Manual == nil {
		d.Manual = map[string]map[string]float64{}
	}
	if d.Divs == nil {
		d.Divs = map[string]map[string]float64{}
	}
	if d.FX == nil {
		d.FX = map[string]map[string]float64{}
	}
	if d.Bench == nil {
		d.Bench = map[string]map[string]float64{}
	}
	if d.DivInfo == nil {
		d.DivInfo = map[string]map[string]DivInfo{}
	}
	if d.Settings.CGTMethod == "" {
		d.Settings.CGTMethod = "fifo"
	}
}

// View runs fn with a read lock. fn must not retain the *DB.
func (s *Store) View(fn func(*DB)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.db)
}

// Update runs fn under the write lock and persists on success.
func (s *Store) Update(fn func(*DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.db); err != nil {
		return err
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.db, "", " ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
