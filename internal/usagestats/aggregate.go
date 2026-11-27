package usagestats

import (
	"fmt"
	"sort"

	"github.com/HaiNNT/c-hottag/internal/usagecost"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

const (
	defaultWeeks  = 4
	tieBand       = 1.25 // plan sets priced up to 25% above the cheapest are ties (20% would not tie $80 and $100)
	daysPerMonth  = 30.4375
	headroom      = 1.15
	upgradeAvg    = 90.0
	upgradeLimit  = 2
	downgradeFill = 0.8
	cancelAvg     = 10.0
)

// thresholds are the verdict defaults, printed with every report.
func thresholds(weeks int) map[string]float64 {
	return map[string]float64{
		"windows":               float64(weeks),
		"upgradeAvgPeak":        upgradeAvg,
		"upgradeLimitedWindows": upgradeLimit,
		"limitedPeak":           limitedPeak,
		"downgradeFill":         downgradeFill,
		"cancelAvgPeak":         cancelAvg,
		"headroom":              headroom,
		"noiseFloorPeak":        noiseFloor,
	}
}

// attributed reports whether a ledger row belongs to an account: Home usage
// and rows without an account are unattributed.
func attributed(d usagelog.Daily) bool { return d.Src != "home" && d.Acct != "" }

// add folds one priced ledger row into t.
func (t *Tok) add(d usagelog.Daily, q usagecost.Quote) {
	t.In += d.In
	t.Out += d.Out
	t.CR += d.CR
	t.CW5m += d.CW5m
	t.CW1h += d.CW1h
	t.Web += d.Web
	t.Total += d.In + d.Out + d.CR + d.CW5m + d.CW1h
	switch {
	case q.Known:
		t.USD += q.USD
	case d.In+d.Out+d.CR+d.CW5m+d.CW1h+d.Web > 0:
		t.USDUnknown = true
	}
	if q.Estimated {
		t.Estimated = true
	}
}

// Build aggregates the ledger into a Report.
func Build(in Input) Report {
	weeks := in.Weeks
	if weeks <= 0 {
		weeks = defaultWeeks
	}
	by := in.By
	if by != "day" {
		by = "month"
	}
	r := Report{
		Since: in.Since, Until: in.Until, By: by,
		ByModel: map[string]Tok{}, Thresholds: thresholds(weeks),
		Periods: []Period{}, Accounts: []AccountRow{}, Verdicts: []Verdict{}, Notes: []string{},
	}

	end := in.Until
	if end.IsZero() || end.After(in.Now) {
		end = in.Now
	}
	if r.Until.IsZero() {
		r.Until = in.Now // an open period ends now; never a zero date
	}
	unknown := map[string]bool{}
	estimated := map[string]bool{}
	haiku := false
	badDays := 0
	periods := map[string]*Period{}
	accTok := map[string]*Tok{}

	for _, d := range in.Days {
		ds, ok := dayStart(d.Day)
		if !ok {
			badDays++
			continue
		}
		if !ds.Before(end) || (!in.Since.IsZero() && !ds.AddDate(0, 0, 1).After(in.Since)) {
			continue
		}
		if in.Account != "" && d.Acct != in.Account {
			continue
		}
		q := usagecost.CostSum(d.Model, tokensOf(d))
		model := d.Model
		if model == "" {
			model = "unknown"
		}
		if !q.Known {
			unknown[model] = true
		} else if q.Estimated {
			estimated[model] = true
		}
		if q.Priced == "claude-haiku-5-5" {
			haiku = true
		}

		r.Total.add(d, q)
		bm := r.ByModel[model]
		bm.add(d, q)
		r.ByModel[model] = bm

		key := d.Day
		if by == "month" {
			key = d.Day[:7]
		}
		p := periods[key]
		if p == nil {
			p = &Period{Key: key, ByModel: map[string]Tok{}}
			periods[key] = p
		}
		p.Tok.add(d, q)
		pm := p.ByModel[model]
		pm.add(d, q)
		p.ByModel[model] = pm

		if !attributed(d) {
			r.Unattributed.add(d, q)
			continue
		}
		t := accTok[d.Acct]
		if t == nil {
			t = &Tok{}
			accTok[d.Acct] = t
		}
		t.add(d, q)
	}

	keys := make([]string, 0, len(periods))
	for k := range periods {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.Periods = append(r.Periods, *periods[k])
	}

	fleet := buildFleet(in, weeks)
	for _, f := range fleet {
		if in.Account != "" && f.info.Name != in.Account {
			continue
		}
		var tok Tok
		if t := accTok[f.info.Name]; t != nil {
			tok = *t
		}
		r.Accounts = append(r.Accounts, f.row(tok, in.Since, end))
	}

	for _, v := range verdicts(fleet, weeks) {
		if in.Account == "" || v.Account == in.Account {
			r.Verdicts = append(r.Verdicts, v)
		}
	}
	r.Overall = overall(fleet, weeks)
	r.Notes = notes(unknown, estimated, haiku, badDays)
	return r
}

func notes(unknown, estimated map[string]bool, haiku bool, badDays int) []string {
	n := []string{"Units are chottag's capacity model, not Anthropic's published numbers; $ per 1% (7d) by tier is the evidence for or against it."}
	for _, m := range sortedKeys(unknown) {
		n = append(n, fmt.Sprintf("model %s has no list price: its tokens are counted, its $ is unknown.", m))
	}
	for _, m := range sortedKeys(estimated) {
		n = append(n, fmt.Sprintf("model %s is priced as the newest model of its family (estimate).", m))
	}
	if haiku {
		n = append(n, "claude-haiku-5-5 has a long-prompt price tier per message that daily totals cannot see: its $ can be understated for prompts over 100K tokens.")
	}
	if badDays > 0 {
		n = append(n, fmt.Sprintf("%d ledger rows with an unreadable day were skipped.", badDays))
	}
	return n
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
