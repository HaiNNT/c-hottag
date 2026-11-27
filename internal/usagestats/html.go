package usagestats

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

//go:embed report.tmpl.html
var reportTmpl string

var htmlTmpl = template.Must(template.New("report").Parse(reportTmpl))

const nPalette = 6

type legendItem struct {
	Name  string
	Class string
}

type htmlRow struct {
	Name, From, Tier, Units, Price, Tokens, Cost, Value, AvgPeak string
}

type htmlView struct {
	Report
	Period            string
	Empty             bool
	Cost, Best, Worst string
	Unattr            string
	BarSVG, LineSVG   template.HTML
	Legend            []legendItem
	LineLegend        []legendItem
	Rows              []htmlRow
	Thresholds        []string
}

const dash = "—"

func fmtPtr(p *float64, f string) string {
	if p == nil {
		return dash
	}
	return fmt.Sprintf(f, *p)
}

func fmtCost(t Tok) string {
	s := fmt.Sprintf("$%.2f", t.USD)
	if t.USDUnknown {
		s += " + unpriced"
	}
	if t.Estimated {
		s += " (est.)"
	}
	return s
}

func displayTier(t string) string {
	switch t {
	case "max20x":
		return "ultra"
	case "":
		return dash
	}
	return t
}

// RenderHTML writes the self-contained report page: inline CSS and SVG, no
// external URL of any kind.
func RenderHTML(w io.Writer, r Report) error {
	v := htmlView{Report: r}
	v.Empty = r.Total.Total == 0 && r.Total.USD == 0 && len(r.Periods) == 0 && len(r.Accounts) == 0
	if !r.Since.IsZero() || !r.Until.IsZero() {
		since, until := "the start", "today"
		if !r.Since.IsZero() {
			since = r.Since.Format("2006-01-02")
		}
		if !r.Until.IsZero() {
			until = lastDay(r.Until)
		}
		v.Period = fmt.Sprintf("%s to %s, by %s", since, until, r.By)
	}
	v.Cost = fmtCost(r.Total)
	if r.Unattributed.Total > 0 || r.Unattributed.USD > 0 {
		v.Unattr = fmt.Sprintf("%d tokens, %s", r.Unattributed.Total, fmtCost(r.Unattributed))
	}
	var best, worst *float64
	bestN, worstN := "", ""
	for _, a := range r.Accounts {
		if p := a.ValueRatio; p != nil {
			if best == nil || *p > *best {
				best, bestN = p, a.Name
			}
			if worst == nil || *p < *worst {
				worst, worstN = p, a.Name
			}
		}
		from := ""
		if a.From != "" && !r.Since.IsZero() && a.From > r.Since.Format("2006-01-02") {
			from = a.From
		}
		v.Rows = append(v.Rows, htmlRow{
			Name: a.Name, From: from, Tier: displayTier(a.Tier), Units: fmt.Sprintf("%g", a.Units),
			Price: fmtPtr(a.Price, "$%.0f"), Tokens: fmt.Sprint(a.Tok.Total), Cost: fmtCost(a.Tok),
			Value: fmtPtr(a.ValueRatio, "%.2f"), AvgPeak: fmtPtr(a.AvgPeak, "%.0f%%"),
		})
	}
	v.Best, v.Worst = dash, dash
	if best != nil {
		v.Best = fmt.Sprintf("%.2f (%s)", *best, bestN)
		v.Worst = fmt.Sprintf("%.2f (%s)", *worst, worstN)
	}
	keys := make([]string, 0, len(r.Thresholds))
	for k := range r.Thresholds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.Thresholds = append(v.Thresholds, fmt.Sprintf("%s=%g", k, r.Thresholds[k]))
	}
	v.BarSVG, v.Legend = barSVG(r)
	v.LineSVG, v.LineLegend = lineSVG(r)
	return htmlTmpl.Execute(w, v)
}

// lastDay is the last day a report covers: an Until at local midnight is the
// exclusive end of an --until day, so the day before it.
func lastDay(until time.Time) string {
	u := until.In(loc)
	if u.Equal(time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, loc)) {
		u = u.AddDate(0, 0, -1)
	}
	return u.Format("2006-01-02")
}

func cls(i int) string  { return fmt.Sprintf("c%d", i%nPalette) }
func fill(i int) string { return fmt.Sprintf("var(--c%d)", i%nPalette) }

const (
	svgW, svgH = 720, 240
	padL, padR = 48, 12
	padT, padB = 12, 28
)

func barSVG(r Report) (template.HTML, []legendItem) {
	set := map[string]bool{}
	var max float64
	for _, p := range r.Periods {
		for m := range p.ByModel {
			set[m] = true
		}
		max = math.Max(max, p.Tok.USD)
	}
	models := make([]string, 0, len(set))
	for m := range set {
		models = append(models, m)
	}
	sort.Strings(models)
	var legend []legendItem
	for i, m := range models {
		legend = append(legend, legendItem{m, cls(i)})
	}
	if len(r.Periods) == 0 || max <= 0 {
		return template.HTML(`<p class="mut">no priced cost in this period</p>`), legend
	}
	var b strings.Builder
	plotW, plotH := float64(svgW-padL-padR), float64(svgH-padT-padB)
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" role="img" aria-label="cost per period by model">`, svgW, svgH)
	fmt.Fprintf(&b, `<line class="ax" x1="%d" y1="%d" x2="%d" y2="%d"/>`, padL, svgH-padB, svgW-padR, svgH-padB)
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end">$%.2f</text><text x="%d" y="%d" text-anchor="end">$0</text>`, padL-4, padT+8, max, padL-4, svgH-padB)
	slot := plotW / float64(len(r.Periods))
	bw := slot * 0.7
	for i, p := range r.Periods {
		x := float64(padL) + slot*float64(i) + (slot-bw)/2
		y := float64(svgH - padB)
		for j, m := range models {
			h := p.ByModel[m].USD / max * plotH
			if h <= 0 {
				continue
			}
			y -= h
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"><title>%s %s $%.2f</title></rect>`,
				x, y, bw, h, fill(j), template.HTMLEscapeString(p.Key), template.HTMLEscapeString(m), p.ByModel[m].USD)
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" text-anchor="middle">%s</text>`, x+bw/2, svgH-padB+14, template.HTMLEscapeString(p.Key))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), legend
}

func lineSVG(r Report) (template.HTML, []legendItem) {
	n := 0
	for _, a := range r.Accounts {
		if len(a.Weeks) > n {
			n = len(a.Weeks)
		}
	}
	if n == 0 {
		return template.HTML(`<p class="mut">no complete weekly windows yet</p>`), nil
	}
	maxPct := 100.0
	for _, a := range r.Accounts {
		for _, w := range a.Weeks {
			maxPct = math.Max(maxPct, w.PeakPct)
		}
	}
	plotW, plotH := float64(svgW-padL-padR), float64(svgH-padT-padB)
	xAt := func(i int) float64 {
		if n == 1 {
			return float64(padL) + plotW/2
		}
		return float64(padL) + plotW*float64(i)/float64(n-1)
	}
	yAt := func(p float64) float64 { return float64(padT) + plotH*(1-p/maxPct) }
	var legend []legendItem
	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" role="img" aria-label="weekly peak percent per account">`, svgW, svgH)
	fmt.Fprintf(&b, `<line class="ax" x1="%d" y1="%d" x2="%d" y2="%d"/>`, padL, svgH-padB, svgW-padR, svgH-padB)
	fmt.Fprintf(&b, `<line class="rule" x1="%d" y1="%.1f" x2="%d" y2="%.1f"/><text x="%d" y="%.1f" text-anchor="end">100%%</text>`,
		padL, yAt(100), svgW-padR, yAt(100), padL-4, yAt(100)+4)
	fmt.Fprintf(&b, `<text x="%d" y="%d" text-anchor="end">0%%</text>`, padL-4, svgH-padB)
	for ai, a := range r.Accounts {
		legend = append(legend, legendItem{a.Name, cls(ai)})
		if len(a.Weeks) == 0 {
			continue
		}
		pts := make([]string, 0, len(a.Weeks))
		for i, w := range a.Weeks {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", xAt(i), yAt(w.PeakPct)))
		}
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`, fill(ai), strings.Join(pts, " "))
		for i, w := range a.Weeks {
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" fill="%s"><title>%s %s %.0f%%</title></circle>`,
				xAt(i), yAt(w.PeakPct), fill(ai), template.HTMLEscapeString(a.Name), w.Reset.Format("2006-01-02"), w.PeakPct)
		}
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()), legend
}
