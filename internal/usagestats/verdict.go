package usagestats

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagecost"
)

// tierOrder is the plan ladder, cheapest first. Only these tiers take part
// in verdicts and fleet math ("max", "team" and "" are unknown, D8).
var tierOrder = []string{"pro", "max5x", "max20x"}

// tierUnits is the default capacity of a tier (autoswitch.Tier.Units()).
var tierUnits = map[string]float64{"pro": 1, "max5x": 5, "max20x": 20}

// normTier maps a tier name onto the ladder; "ultra" is max20x.
func normTier(s string) (string, bool) {
	if s == "ultra" {
		s = "max20x"
	}
	if _, ok := tierUnits[s]; !ok {
		return "", false
	}
	return s, true
}

func rank(tier string) int {
	for i, t := range tierOrder {
		if t == tier {
			return i
		}
	}
	return -1
}

func display(tier string) string {
	if tier == "max20x" {
		return "ultra"
	}
	return tier
}

// fleetAcct is one account with everything the report derives from it.
type fleetAcct struct {
	info         AccountInfo
	tier         string // normalized; "" when not on the ladder
	known        bool   // rotates and has a ladder tier: takes part in verdicts
	units        float64
	price        *float64
	source       string
	weeks        []Week
	unregistered bool      // seen in the ledger only
	first        time.Time // first evidence: earliest attributed ledger day or snapshot; zero when none
}

func buildFleet(in Input, n int) []fleetAcct {
	infos := append([]AccountInfo(nil), in.Accounts...)
	seen := map[string]bool{}
	for _, a := range infos {
		seen[a.Name] = true
	}
	var extra []string
	for _, d := range in.Days {
		if attributed(d) && !seen[d.Acct] {
			seen[d.Acct] = true
			extra = append(extra, d.Acct)
		}
	}
	sort.Strings(extra)
	unreg := map[string]bool{}
	for _, name := range extra {
		infos = append(infos, AccountInfo{Name: name})
		unreg[name] = true
	}

	first := firstEvidence(in)
	out := make([]fleetAcct, 0, len(infos))
	for _, a := range infos {
		f := fleetAcct{info: a, unregistered: unreg[a.Name], first: first[a.Name]}
		f.tier, _ = normTier(a.Tier)
		f.known = a.Rotates && f.tier != ""
		f.units = a.Units
		if f.units <= 0 {
			f.units = tierUnits[f.tier]
		}
		switch {
		case a.PriceSet:
			p := a.Price
			f.price, f.source = &p, "override"
		default:
			if p, ok := usagecost.PlanPrice(a.Tier); ok {
				f.price, f.source = &p, "list"
			}
		}
		if f.known {
			f.weeks = accountWeeks(a.Name, f.tier, f.units, in.Snaps, in.Days, in.Now, n)
		}
		out = append(out, f)
	}
	return out
}

// firstEvidence is each account's earliest attributed ledger day or snapshot.
func firstEvidence(in Input) map[string]time.Time {
	first := map[string]time.Time{}
	note := func(name string, t time.Time) {
		if cur, ok := first[name]; !ok || t.Before(cur) {
			first[name] = t
		}
	}
	for _, d := range in.Days {
		if !attributed(d) {
			continue
		}
		if ds, ok := dayStart(d.Day); ok {
			note(d.Acct, ds)
		}
	}
	for _, s := range in.Snaps {
		if s.Acct != "" && !s.T.IsZero() {
			note(s.Acct, s.T)
		}
	}
	return first
}

// row builds the account's line. The plan price is pro-rated from the later of
// since and the account's first evidence to end, so a new account is not
// priced for months it did not exist.
func (f fleetAcct) row(tok Tok, since, end time.Time) AccountRow {
	row := AccountRow{
		Name: f.info.Name, Tier: f.info.Tier, Units: f.units,
		Price: f.price, PriceSource: f.source, Tok: tok, Weeks: f.weeks,
	}
	if row.Weeks == nil {
		row.Weeks = []Week{}
	}
	if !since.IsZero() {
		from := since
		if f.first.After(from) {
			from = f.first
		}
		if days := end.Sub(from).Hours() / 24; days > 0 {
			row.From = from.In(loc).Format("2006-01-02")
			if f.price != nil && *f.price > 0 {
				// At least one day, so an account first seen hours ago is not
				// credited with a huge ratio.
				v := tok.USD / (*f.price * max(days, 1) / daysPerMonth)
				row.ValueRatio = &v
			}
		}
	}
	var peaks, perPct []float64
	for _, w := range f.weeks {
		peaks = append(peaks, w.PeakPct)
		if w.USDPerPct != nil {
			perPct = append(perPct, *w.USDPerPct)
		}
	}
	row.AvgPeak = mean(peaks)
	row.USDPerPct = mean(perPct)
	if row.USDPerPct != nil && f.units > 0 {
		v := *row.USDPerPct / f.units
		row.USDPerUnitPct = &v
	}
	return row
}

func (f fleetAcct) maxPeak() float64 {
	var m float64
	for _, w := range f.weeks {
		m = max(m, w.PeakPct)
	}
	return m
}

// maxDemand is the busiest window's demand in units, each window at the
// units its plan had then.
func (f fleetAcct) maxDemand() float64 {
	var m float64
	for _, w := range f.weeks {
		m = max(m, w.PeakPct*w.Units/100)
	}
	return m
}

func verdicts(fleet []fleetAcct, n int) []Verdict {
	out := make([]Verdict, 0, len(fleet))
	for _, f := range fleet {
		out = append(out, verdictFor(f, fleet, n))
	}
	return out
}

func verdictFor(f fleetAcct, fleet []fleetAcct, n int) Verdict {
	v := Verdict{Account: f.info.Name, Basis: map[string]float64{}}
	if !f.known {
		v.Kind = "none"
		if f.unregistered {
			v.Text = "No verdict: not a registered account (history only). Run chottag plan if it is still in use."
		} else if !f.info.Rotates {
			v.Text = "No verdict: rotation is off for this account, so it stays out of the fleet math. Run chottag plan to set its tier or price."
		} else {
			v.Text = "No verdict: the tier is not one chottag can size (pro, max5x, ultra). Run chottag plan to set its tier."
		}
		return v
	}
	v.Basis["windows"] = float64(len(f.weeks))
	if len(f.weeks) < n {
		v.Kind = "insufficient"
		v.Text = fmt.Sprintf("Not enough history: %d of %d complete 7d windows.", len(f.weeks), n)
		return v
	}
	var sum float64
	limited := 0
	for _, w := range f.weeks {
		sum += w.PeakPct
		if w.Limited {
			limited++
		}
	}
	avg := sum / float64(len(f.weeks))
	demand := f.maxDemand()
	v.Basis["avgPeak"] = avg
	v.Basis["limitedWindows"] = float64(limited)
	v.Basis["maxPeak"] = f.maxPeak()
	v.Basis["maxDemandUnits"] = demand
	v.Basis["units"] = f.units
	facts := fmt.Sprintf("Weekly peak averaged %.0f%% over %d windows, limited in %d", avg, len(f.weeks), limited)

	if avg >= upgradeAvg || limited >= upgradeLimit {
		if next := rank(f.tier) + 1; next < len(tierOrder) {
			v.Kind = "upgrade"
			v.Text = fmt.Sprintf("Consider upgrade to %s. %s (upgrade at average >= %.0f%% or limited in >= %d windows).",
				display(tierOrder[next]), facts, upgradeAvg, upgradeLimit)
			return v
		}
		v.Kind = "keep"
		v.Text = fmt.Sprintf("Keep. %s; already the top tier, so more capacity means another account.", facts)
		return v
	}

	if avg < cancelAvg {
		var spare float64
		for _, g := range fleet {
			if g.info.Name != f.info.Name && g.known && len(g.weeks) > 0 {
				spare += g.units * (1 - g.maxPeak()/100)
			}
		}
		v.Basis["spareUnits"] = spare
		if spare >= demand {
			v.Kind = "cancel"
			v.Text = fmt.Sprintf("Cancel candidate. %s (below %.0f%%) and the other accounts have %.1f spare units against its %.1f-unit peak.",
				facts, cancelAvg, spare, demand)
			return v
		}
	}

	for _, t := range tierOrder[:rank(f.tier)] {
		if demand <= downgradeFill*tierUnits[t] {
			v.Kind = "downgrade"
			v.Text = fmt.Sprintf("Consider downgrade to %s. Peak demand %.1f units fits %.0f units at <= %.0f%% in every window.",
				display(t), demand, tierUnits[t], downgradeFill*100)
			return v
		}
	}
	v.Kind = "keep"
	v.Text = fmt.Sprintf("Keep. %s; peak demand %.1f of %.0f units.", facts, demand, f.units)
	return v
}

// overall sizes the whole rotating fleet by calendar week and finds the
// cheapest plan set covering the busiest week with headroom.
func overall(fleet []fleetAcct, n int) Overall {
	o := Overall{Suggest: map[string]int{}, Changes: []string{}}
	var accts []fleetAcct
	for _, f := range fleet {
		if f.known && f.price != nil {
			accts = append(accts, f)
			o.PayUSD += *f.price
			o.Units += f.units
		}
	}
	var short []string
	override := false
	for _, f := range accts {
		if len(f.weeks) < n {
			short = append(short, f.info.Name)
		}
		if f.source == "override" {
			override = true
		}
	}
	if len(short) > 0 {
		o.Kind = "insufficient"
		o.Text = fmt.Sprintf("Not enough history to size the fleet: %s has fewer than %d complete 7d windows.", strings.Join(short, ", "), n)
		return o
	}
	weekly := map[string]float64{}
	for _, f := range accts {
		perWeek := map[string]float64{}
		for _, w := range f.weeks {
			k := weekKey(w.Reset.Add(-1))
			perWeek[k] = max(perWeek[k], w.PeakPct*w.Units/100)
		}
		for k, d := range perWeek {
			weekly[k] += d
		}
	}
	if len(accts) == 0 || len(weekly) == 0 {
		o.Kind = "insufficient"
		o.Text = "Not enough history to size the fleet: no complete 7d window of a rotating account with a known tier."
		return o
	}
	for _, d := range weekly {
		o.PeakDemand = max(o.PeakDemand, d)
	}

	cur := map[string]int{}
	for _, f := range accts {
		cur[f.tier]++
	}
	target := o.PeakDemand * headroom
	type cand struct {
		counts      [3]int
		usd         float64
		n, distance int
	}
	var cands []cand
	cheapest := -1.0
	for p := 0; p <= 6; p++ {
		for m5 := 0; m5 <= 6; m5++ {
			for m20 := 0; m20 <= 6; m20++ {
				c := cand{counts: [3]int{p, m5, m20}, n: p + m5 + m20}
				if c.n == 0 {
					continue
				}
				units := float64(p)*tierUnits["pro"] + float64(m5)*tierUnits["max5x"] + float64(m20)*tierUnits["max20x"]
				if units+1e-9 < target {
					continue
				}
				for i, t := range tierOrder {
					price, _ := usagecost.PlanPrice(t)
					c.usd += float64(c.counts[i]) * price
					d := c.counts[i] - cur[t]
					if d < 0 {
						d = -d
					}
					c.distance += d
				}
				cands = append(cands, c)
				if cheapest < 0 || c.usd < cheapest {
					cheapest = c.usd
				}
			}
		}
	}
	// Sets within tieBand of the cheapest are ties: fewer accounts, then
	// fewer changes, then the lower price.
	var best *cand
	for i := range cands {
		c := cands[i]
		if c.usd > cheapest*tieBand+1e-9 {
			continue
		}
		if best == nil || c.n < best.n || (c.n == best.n && (c.distance < best.distance ||
			(c.distance == best.distance && c.usd < best.usd))) {
			best = &cands[i]
		}
	}
	if best == nil {
		o.Kind = "insufficient"
		o.Text = fmt.Sprintf("Peak demand %.1f units needs more than 6 accounts of each plan; no suggestion.", o.PeakDemand)
		return o
	}
	for i, t := range tierOrder {
		o.Suggest[t] = best.counts[i]
	}
	o.SuggestUSD = best.usd
	o.Changes = changes(accts, o.Suggest)
	if len(o.Changes) == 0 {
		o.Kind = "keep"
	} else {
		o.Kind = "change"
	}

	var parts []string
	for i := len(tierOrder) - 1; i >= 0; i-- {
		if c := best.counts[i]; c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, display(tierOrder[i])))
		}
	}
	o.Text = fmt.Sprintf("You pay $%.0f/mo for %.0f units; the peak week used %.1f units; cheapest set covering it with %.0f%% headroom: %s ($%.0f/mo). ",
		o.PayUSD, o.Units, o.PeakDemand, (headroom-1)*100, strings.Join(parts, " + "), o.SuggestUSD)
	if o.Kind == "keep" {
		o.Text += "Keep."
	} else {
		o.Text += "Change: " + strings.Join(o.Changes, "; ") + "."
	}
	if override {
		o.Text += " Suggested plans are priced at list prices; your --price overrides apply to what you pay now."
	}
	return o
}

// changes turns current tiers and a suggested set into human lines that name
// accounts. Surplus accounts (lowest demand first) are paired with missing
// slots as an upgrade or downgrade; the rest are cancelled or added.
func changes(accts []fleetAcct, suggest map[string]int) []string {
	byTier := map[string][]fleetAcct{}
	for _, f := range accts {
		byTier[f.tier] = append(byTier[f.tier], f)
	}
	var surplus []fleetAcct
	var slots []string
	for _, t := range tierOrder {
		list := byTier[t]
		sort.SliceStable(list, func(i, j int) bool { return list[i].maxDemand() < list[j].maxDemand() })
		if extra := len(list) - suggest[t]; extra > 0 {
			surplus = append(surplus, list[:extra]...)
		}
		for i := len(list); i < suggest[t]; i++ {
			slots = append(slots, t)
		}
	}
	sort.SliceStable(surplus, func(i, j int) bool { return rank(surplus[i].tier) > rank(surplus[j].tier) })
	sort.SliceStable(slots, func(i, j int) bool { return rank(slots[i]) > rank(slots[j]) })
	out := []string{}
	for len(surplus) > 0 && len(slots) > 0 {
		f, t := surplus[0], slots[0]
		verb := "downgrade"
		if rank(t) > rank(f.tier) {
			verb = "upgrade"
		}
		out = append(out, fmt.Sprintf("%s %s to %s", verb, f.info.Name, display(t)))
		surplus, slots = surplus[1:], slots[1:]
	}
	for _, f := range surplus {
		out = append(out, "cancel "+f.info.Name)
	}
	for _, t := range slots {
		out = append(out, "add one "+display(t))
	}
	return out
}
