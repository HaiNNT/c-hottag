package usagelog

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

const dayLayout = "2006-01-02"

type dailyKey struct{ day, acct, model, src, speed string }

func rollup(evs []Event) []Daily {
	m := map[dailyKey]*Daily{}
	for _, e := range evs {
		day := e.T.In(loc).Format(dayLayout)
		k := dailyKey{day, e.Acct, e.Model, e.Src, e.Speed}
		d := m[k]
		if d == nil {
			d = &Daily{Day: day, Acct: e.Acct, Model: e.Model, Src: e.Src, Speed: e.Speed}
			m[k] = d
		}
		d.N++
		d.In += e.In
		d.Out += e.Out
		d.CR += e.CR
		d.CW5m += e.CW5m
		d.CW1h += e.CW1h
		d.Web += e.Web
	}
	out := make([]Daily, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sortDaily(out)
	return out
}

func sortDaily(ds []Daily) {
	sort.Slice(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if a.Acct != b.Acct {
			return a.Acct < b.Acct
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		return a.Speed < b.Speed
	})
}

// eventsNotIn returns the events whose local day is not in have.
func eventsNotIn(evs []Event, have map[string]bool) []Event {
	var out []Event
	for _, e := range evs {
		if !have[e.T.In(loc).Format(dayLayout)] {
			out = append(out, e)
		}
	}
	return out
}

func daySet(ds []Daily) map[string]bool {
	s := map[string]bool{}
	for _, d := range ds {
		s[d.Day] = true
	}
	return s
}

// Days returns daily.jsonl rows plus the rollup of raw days not yet in
// daily.jsonl, for local days in [since, until). Zero bounds are open.
func (l Ledger) Days(since, until time.Time) ([]Daily, error) {
	stored, err := readLines[Daily](l.dailyPath())
	if err != nil {
		return nil, err
	}
	raw, err := l.loadRaw(time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	all := append(stored, rollup(eventsNotIn(raw, daySet(stored)))...)
	var lo, hi string
	if !since.IsZero() {
		lo = since.In(loc).Format(dayLayout)
	}
	if !until.IsZero() {
		hi = until.Add(-time.Nanosecond).In(loc).Format(dayLayout)
	}
	out := all[:0]
	for _, d := range all {
		if (lo != "" && d.Day < lo) || (hi != "" && d.Day > hi) {
			continue
		}
		out = append(out, d)
	}
	sortDaily(out)
	return out, nil
}

// Compact rolls raw months wholly older than now-RawRetention into
// daily.jsonl, then deletes them. Days already in daily.jsonl are not
// added again, so a repeat run (or one that crashed before the delete) is
// harmless. The caller holds Lock.
func (l Ledger) Compact(now time.Time) error {
	cut := now.Add(-RawRetention)
	months, err := l.monthFiles("events-")
	if err != nil {
		return err
	}
	oldSet := map[string]bool{}
	var old []string
	for _, m := range months {
		evs, err := readLines[Event](l.eventsPath(m))
		if err != nil {
			return err
		}
		wholly := true
		for _, e := range evs {
			if !e.T.Before(cut) {
				wholly = false
				break
			}
		}
		if wholly {
			old = append(old, m)
			oldSet[m] = true
		}
	}
	if len(old) == 0 {
		return nil
	}
	// Keys (and times) of the messages in the months being rolled up.
	keyT := map[string]time.Time{}
	for _, m := range old {
		evs, err := readLines[Event](l.eventsPath(m))
		if err != nil {
			return err
		}
		for _, e := range evs {
			if k := dedupKey(e); k != "" {
				if t, ok := keyT[k]; !ok || e.T.Before(t) {
					keyT[k] = e.T
				}
			}
		}
	}
	// Dedup across every raw month so a message split over files counts
	// once. A kept copy living in a later month is rolled under its
	// partner's time, so its day is the rolled month's.
	raw, err := l.loadRaw(time.Time{}, time.Time{})
	if err != nil {
		return err
	}
	var roll []Event
	for _, e := range raw {
		if oldSet[monthOf(e.T)] {
			roll = append(roll, e)
		} else if t, ok := keyT[dedupKey(e)]; ok && dedupKey(e) != "" {
			e.T = t
			roll = append(roll, e)
		}
	}
	stored, err := readLines[Daily](l.dailyPath())
	if err != nil {
		return err
	}
	if rows := rollup(eventsNotIn(roll, daySet(stored))); len(rows) > 0 {
		all := append(stored, rows...)
		sortDaily(all)
		var buf []byte
		for _, d := range all {
			b, err := json.Marshal(d)
			if err != nil {
				return err
			}
			buf = append(append(buf, b...), '\n')
		}
		if err := fsutil.WriteFileAtomic(l.dailyPath(), buf, 0o600); err != nil {
			return err
		}
	}
	for _, m := range old {
		if err := l.dropRolledKeys(m, oldSet, keyT); err != nil {
			return err
		}
	}
	for _, m := range old {
		if err := os.Remove(l.eventsPath(m)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// dropRolledKeys removes from the month after m every event whose key was
// rolled up with m, rewriting that file atomically. The key set comes from
// m's own contents, so a rerun after a crash repairs it.
func (l Ledger) dropRolledKeys(m string, oldSet map[string]bool, keyT map[string]time.Time) error {
	t, err := time.ParseInLocation(monthLayout, m, loc)
	if err != nil {
		return err
	}
	next := t.AddDate(0, 1, 0).Format(monthLayout)
	if oldSet[next] {
		return nil // deleted anyway
	}
	path := l.eventsPath(next)
	evs, err := readLines[Event](path)
	if err != nil {
		return err
	}
	var keep []byte
	n := 0
	for _, e := range evs {
		if k := dedupKey(e); k != "" {
			if _, hit := keyT[k]; hit {
				continue
			}
		}
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		keep = append(append(keep, b...), '\n')
		n++
	}
	if n == len(evs) {
		return nil
	}
	if n == 0 {
		return os.Remove(path)
	}
	return fsutil.WriteFileAtomic(path, keep, 0o600)
}
