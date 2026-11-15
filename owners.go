package main

import (
	"sort"
	"strings"
)

// Owners let investments held by different people (or entities) be taxed separately. Each owner has their own
// assumptions - other income, losses brought forward and parcel method - and an investment is tagged to at most one
// owner. Anything untagged belongs to the default owner, whose assumptions are the original Settings. Tax is always
// worked out per owner, so a sale spanning several owners is taxed in each owner's own bracket and the results added.

type Owner struct {
	Name        string  `json:"name"`
	OtherIncome float64 `json:"other_income"`
	OpeningLoss float64 `json:"opening_loss"`
	CGTMethod   string  `json:"cgt_method,omitempty"` // fifo | hifo | optimal; blank = same as the default owner
}

func (d *DB) ownerIndex(name string) int {
	for i, o := range d.Owners {
		if strings.EqualFold(o.Name, name) {
			return i
		}
	}
	return -1
}

// hasOwners reports whether tax must be split by owner. A per-owner view (see ownerView) is already one owner's data.
func (d *DB) hasOwners() bool { return !d.isView && len(d.Owners) > 0 }

// ownerOf returns the owner an investment is tagged to; "" (the default owner) when untagged or the owner was deleted.
func (d *DB) ownerOf(sym string) string {
	in := d.Instruments[sym]
	if in == nil || in.Owner == "" {
		return ""
	}
	if i := d.ownerIndex(in.Owner); i >= 0 {
		return d.Owners[i].Name
	}
	return ""
}

// ownerList is the default owner ("") followed by the named owners.
func (d *DB) ownerList() []string {
	out := []string{""}
	for _, o := range d.Owners {
		out = append(out, o.Name)
	}
	return out
}

// settingsFor returns the tax assumptions for an owner.
func (d *DB) settingsFor(owner string) Settings {
	s := d.Settings
	if i := d.ownerIndex(owner); owner != "" && i >= 0 {
		o := d.Owners[i]
		s.OtherIncome, s.OpeningLoss = o.OtherIncome, o.OpeningLoss
		if o.CGTMethod == "fifo" || o.CGTMethod == "hifo" || o.CGTMethod == "optimal" {
			s.CGTMethod = o.CGTMethod
		}
	}
	return s
}

// ownerView is the data as one owner sees it: only their investments' transactions, and their own assumptions.
// Nothing is copied deeply and nothing written back.
func (d *DB) ownerView(owner string) *DB {
	s := *d
	s.Settings = d.settingsFor(owner)
	s.Txns = nil
	for _, t := range d.Txns {
		if d.ownerOf(t.Symbol) == owner {
			s.Txns = append(s.Txns, t)
		}
	}
	s.isView, s.viewOwner = true, owner
	return &s
}

// anyIncome reports whether any owner (or the default) has other income recorded.
func (d *DB) anyIncome() bool {
	if d.Settings.OtherIncome > 0 {
		return true
	}
	for _, o := range d.Owners {
		if o.OtherIncome > 0 {
			return true
		}
	}
	return false
}

// ownerLabel is how an owner is named on screen.
func ownerLabel(owner string, hasOwners bool) string {
	switch {
	case owner != "":
		return owner
	case hasOwners:
		return "Unassigned"
	}
	return "You"
}

// sortedOwnerSymbols lists symbols by owner for display.
func sortedSymbols(m map[string]*Instrument) []string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}
