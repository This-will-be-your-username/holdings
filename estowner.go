package main

import (
	"fmt"
	"math"
	"time"
)

// EstimateSale prices a sale. With no owners set up it is a single estimate. With owners, the sale is split by who
// holds each investment; every owner's part is taxed on their own income, losses and parcels, and the parts are
// added up. A sale that touches one owner is simply that owner's estimate.
func EstimateSale(db *DB, reqs []SaleReq, date, selected string, now time.Time) (*Estimate, error) {
	if !db.hasOwners() {
		est, err := estimateCore(db, reqs, date, selected, now)
		if est != nil {
			est.Owner, est.Label = db.viewOwner, ownerLabel(db.viewOwner, false)
		}
		return est, err
	}
	groups := map[string][]SaleReq{}
	var order []string
	for _, rq := range reqs {
		o := db.ownerOf(rq.Symbol)
		if _, ok := groups[o]; !ok {
			order = append(order, o)
		}
		groups[o] = append(groups[o], rq)
	}
	if len(order) == 0 {
		return nil, fmt.Errorf("add at least one holding to sell")
	}
	var parts []*Estimate
	for _, o := range order {
		e, err := estimateCore(db.ownerView(o), groups[o], date, selected, now)
		if err != nil {
			return nil, err
		}
		e.Owner, e.Label = o, ownerLabel(o, true)
		parts = append(parts, e)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return mergeEstimates(parts), nil
}

// mergeEstimates adds up per-owner estimates of the same sale. Each owner's tax is computed separately, so the totals
// are the sum of what each person would actually owe.
func mergeEstimates(parts []*Estimate) *Estimate {
	first := parts[0]
	m := &Estimate{Date: first.Date, FY: first.FY, Future: first.Future, Parts: parts, Label: "All owners"}
	for _, p := range parts {
		m.Lines = append(m.Lines, p.Lines...)
		m.GrossProceeds += p.GrossProceeds
		m.Fees += p.Fees
		m.NetProceeds += p.NetProceeds
	}
	for i, fm := range first.Methods {
		mr := &MethodResult{Method: fm.Method, Label: fm.Label}
		for _, p := range parts {
			pm := p.Methods[i]
			mr.Gain += pm.Gain
			mr.ESSIncome += pm.ESSIncome
			mr.DiscountedGain += pm.DiscountedGain
			mr.NetCapGainDelta += pm.NetCapGainDelta
			mr.TaxDelta += pm.TaxDelta
			mr.Events = append(mr.Events, pm.Events...)
		}
		mr.AfterTax = m.NetProceeds - mr.TaxDelta
		m.Methods = append(m.Methods, mr)
		if fm.Method == first.Sel.Method {
			m.Sel = mr
		}
	}
	best, worst := math.Inf(1), math.Inf(-1)
	for _, mr := range m.Methods {
		best, worst = math.Min(best, mr.TaxDelta), math.Max(worst, mr.TaxDelta)
	}
	m.MethodsDiffer = worst-best > 0.005
	for _, mr := range m.Methods {
		mr.Best = m.MethodsDiffer && mr.TaxDelta-best < 0.005
	}
	return m
}

// CompareNewLawAll prices the sale under the 1 July 2027 rules. For a sale split by owner each owner's part is
// compared on their own assumptions (shown per owner); otherwise est.NL is set as before.
func CompareNewLawAll(db *DB, est *Estimate, picks map[string][]Pick, selMethod string, a NewLawAssump, now time.Time) error {
	one := func(e *Estimate) error {
		view := db
		if db.hasOwners() {
			view = db.ownerView(e.Owner)
		}
		nl, err := CompareNewLaw(view, e, picks, selMethod, a, now)
		if err != nil {
			return err
		}
		e.NL = nl
		return nil
	}
	if len(est.Parts) == 0 {
		return one(est)
	}
	for _, p := range est.Parts {
		if err := one(p); err != nil {
			return err
		}
	}
	return nil
}
