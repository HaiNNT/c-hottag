package usagestats

import (
	"sort"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagecost"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// loc is the zone of the ledger's day keys and of the calendar weeks.
// Tests set it to a fixed zone.
var loc = time.Local

const (
	limitedPeak = 99.5 // a window peaking here or above hit the limit
	noiseFloor  = 10.0 // windows peaking below this give no $ per 1%
	weekLen     = 7 * 24 * time.Hour
	mergeGap    = 2 * time.Hour // resets this close are one 7d window
)

// dayStart is local midnight of a ledger day key.
func dayStart(day string) (time.Time, bool) {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	return t, err == nil
}

// weekKey names the Monday-start calendar week (in loc) containing t.
func weekKey(t time.Time) string {
	t = t.In(loc)
	back := (int(t.Weekday()) + 6) % 7
	return t.AddDate(0, 0, -back).Format("2006-01-02")
}

// accountWeeks is an account's last n complete 7d windows, oldest first.
// Windows are keyed by the snapshots' reset time; one is complete when its
// reset is not after now. A day belongs to a window when its midday falls in
// [reset-7d, reset). Cost is priced per ledger row.
func accountWeeks(name, curTier string, curUnits float64, snaps []usagelog.Snapshot, days []usagelog.Daily, now time.Time, n int) []Week {
	// Snapshots from the headers and from the poll can name the same reset a
	// second or minutes apart: resets within mergeGap are one window.
	var sel []usagelog.Snapshot
	for _, s := range snaps {
		if s.Acct != name || s.R7 == 0 || s.P7 == nil || s.R7 > now.Unix() {
			continue
		}
		sel = append(sel, s)
	}
	sort.SliceStable(sel, func(i, j int) bool { return sel[i].R7 < sel[j].R7 })
	type group struct {
		reset  int64 // the reset of the latest snapshot
		peak   float64
		latest usagelog.Snapshot
		last   int64 // largest R7 seen, to chain the gap
	}
	var groups []*group
	for _, s := range sel {
		var g *group
		if len(groups) > 0 && s.R7-groups[len(groups)-1].last <= int64(mergeGap/time.Second) {
			g = groups[len(groups)-1]
		} else {
			g = &group{latest: s, reset: s.R7}
			groups = append(groups, g)
		}
		g.last = s.R7
		if !s.T.Before(g.latest.T) {
			g.latest, g.reset = s, s.R7
		}
		g.peak = max(g.peak, *s.P7)
	}
	if n > 0 && len(groups) > n {
		groups = groups[len(groups)-n:]
	}
	out := make([]Week, 0, len(groups))
	for _, g := range groups {
		end := time.Unix(g.reset, 0)
		begin := end.Add(-weekLen)
		w := Week{Reset: end, PeakPct: g.peak, Limited: g.peak >= limitedPeak, Units: windowUnits(g.latest, curTier, curUnits)}
		for _, d := range days {
			if d.Acct != name || d.Src == "home" {
				continue
			}
			ds, ok := dayStart(d.Day)
			if !ok {
				continue
			}
			mid := ds.Add(12 * time.Hour)
			if mid.Before(begin) || !mid.Before(end) {
				continue
			}
			w.USD += usagecost.CostSum(d.Model, tokensOf(d)).USD
		}
		if w.PeakPct >= noiseFloor {
			v := w.USD / w.PeakPct
			w.USDPerPct = &v
		}
		out = append(out, w)
	}
	return out
}

func tokensOf(d usagelog.Daily) usagecost.Tokens {
	return usagecost.Tokens{In: d.In, Out: d.Out, CR: d.CR, CW5m: d.CW5m, CW1h: d.CW1h, Web: d.Web, Speed: d.Speed}
}

func mean(vs []float64) *float64 {
	if len(vs) == 0 {
		return nil
	}
	var s float64
	for _, v := range vs {
		s += v
	}
	m := s / float64(len(vs))
	return &m
}

// windowUnits is the capacity a window ran on: its latest snapshot's units,
// else the units of the tier recorded then (the current units when that is
// still the current tier, so an override holds), else the current units.
func windowUnits(s usagelog.Snapshot, curTier string, curUnits float64) float64 {
	if s.Units > 0 {
		return s.Units
	}
	if t, ok := normTier(s.Tier); ok && t != curTier {
		return tierUnits[t]
	}
	return curUnits
}
