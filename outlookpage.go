package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"time"
)

func chartJS(charts map[string]*ChartCfg) template.JS {
	b, err := json.Marshal(charts)
	if err != nil {
		log.Printf("chart json: %v", err)
		return template.JS("{}")
	}
	return template.JS(b) // json.Marshal escapes <, > and & so this is safe inside <script>
}

// portfolioChart is the dashboard's value-vs-net-invested chart.
func portfolioChart(pts []ChartPt) *ChartCfg {
	if len(pts) < 2 {
		return nil
	}
	c := &ChartCfg{Name: "Portfolio", HistName: "Market value", Fmt: "money"}
	inv := LineCfg{Name: "Net invested", Cls: "inv"}
	for _, p := range pts {
		t := dayMs(p.Date)
		c.Hist = append(c.Hist, Pt{t, p.Value})
		inv.Pts = append(inv.Pts, Pt{t, p.Invested})
	}
	c.Lines = []LineCfg{inv}
	c.Now = c.Hist[len(c.Hist)-1].T
	return c
}

func (a *App) outlookPage(w http.ResponseWriter, r *http.Request) {
	var res *Result
	var o *Outlook
	a.st().View(func(db *DB) {
		now := time.Now()
		res = Compute(db, now)
		o = ComputeOutlook(db, res, r.URL.Query().Get("scenario"), now)
	})
	a.render(w, r, "outlook", map[string]any{
		"O": o, "Charts": chartJS(o.Charts), "Scenarios": scenarios, "Empty": len(res.Holdings) == 0,
	})
}
