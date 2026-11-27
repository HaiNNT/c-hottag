package usagelog

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Ledger is the usage directory, $CHOTTAG_HOME/usage.
type Ledger struct{ Dir string }

// Open returns the ledger under chottagHome. It creates nothing.
func Open(chottagHome string) Ledger {
	return Ledger{Dir: filepath.Join(chottagHome, "usage")}
}

const monthLayout = "2006-01"

func monthOf(t time.Time) string { return t.In(loc).Format(monthLayout) }

func (l Ledger) eventsPath(month string) string {
	return filepath.Join(l.Dir, "events-"+month+".jsonl")
}

func (l Ledger) utilPath(month string) string {
	return filepath.Join(l.Dir, "util-"+month+".jsonl")
}

func (l Ledger) dailyPath() string { return filepath.Join(l.Dir, "daily.jsonl") }

// appendLines writes each item as its own O_APPEND write.
func (l Ledger) appendLines(path string, items []any) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	// A crash can leave a torn last line with no newline; end it first so
	// the next record is not glued onto it.
	prefix := ""
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err == nil && last[0] != '\n' {
			prefix = "\n"
		}
	}
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := f.Write([]byte(prefix + string(b) + "\n")); err != nil {
			f.Close()
			return err
		}
		prefix = ""
	}
	return f.Close()
}

// AppendEvents appends each event to the month file of its T.
func (l Ledger) AppendEvents(evs []Event) error {
	byMonth := map[string][]any{}
	for _, e := range evs {
		m := monthOf(e.T)
		byMonth[m] = append(byMonth[m], e)
	}
	for m, items := range byMonth {
		if err := l.appendLines(l.eventsPath(m), items); err != nil {
			return err
		}
	}
	return nil
}

// AppendSnapshot appends a utilization snapshot to the month file of its T.
func (l Ledger) AppendSnapshot(s Snapshot) error {
	return l.appendLines(l.utilPath(monthOf(s.T)), []any{s})
}

// readLines decodes every parseable line of path into T; corrupt or
// truncated lines are skipped. A missing file yields nothing.
func readLines[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var v T
			if json.Unmarshal(line, &v) == nil {
				out = append(out, v)
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// monthFiles lists the months having a file with the given prefix, sorted.
func (l Ledger) monthFiles(prefix string) ([]string, error) {
	ents, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var months []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		m := strings.TrimSuffix(strings.TrimPrefix(n, prefix), ".jsonl")
		if _, err := time.ParseInLocation(monthLayout, m, loc); err != nil {
			continue
		}
		months = append(months, m)
	}
	sort.Strings(months)
	return months, nil
}

// inMonths reports whether month can hold times in [since, until); zero
// bounds are open.
func inMonths(month string, since, until time.Time) bool {
	if !since.IsZero() && month < monthOf(since) {
		return false
	}
	if !until.IsZero() && month > monthOf(until.Add(-time.Nanosecond)) {
		return false
	}
	return true
}

func inRange(t, since, until time.Time) bool {
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !until.IsZero() && !t.Before(until) {
		return false
	}
	return true
}

// loadRaw reads the raw events of the months overlapping the range,
// deduplicated, sorted by T, filtered to [since, until).
func (l Ledger) loadRaw(since, until time.Time) ([]Event, error) {
	months, err := l.monthFiles("events-")
	if err != nil {
		return nil, err
	}
	var all []Event
	// Every raw month is loaded before dedup so a duplicate pair split
	// across a month boundary collapses regardless of the range.
	for _, m := range months {
		evs, err := readLines[Event](l.eventsPath(m))
		if err != nil {
			return nil, err
		}
		all = append(all, evs...)
	}
	all = dedup(all)
	out := all[:0]
	for _, e := range all {
		if inRange(e.T, since, until) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, nil
}

// better reports whether candidate c should replace kept k.
func better(c, k Event) bool {
	if (c.Acct != "") != (k.Acct != "") {
		return c.Acct != ""
	}
	if (c.Src == "live") != (k.Src == "live") {
		return c.Src == "live"
	}
	return c.Out > k.Out
}

func dedupKey(e Event) string {
	switch {
	case e.MID != "":
		return "m:" + e.MID
	case e.RID != "":
		return "r:" + e.RID
	}
	return ""
}

// dedup collapses events sharing a MID (else RID); events with neither are
// kept as is. Order of first appearance is preserved.
func dedup(evs []Event) []Event {
	idx := map[string]int{}
	var out []Event
	for _, e := range evs {
		key := dedupKey(e)
		if key == "" {
			out = append(out, e)
			continue
		}
		i, seen := idx[key]
		if !seen {
			idx[key] = len(out)
			out = append(out, e)
			continue
		}
		k := out[i]
		acct := k.Acct
		if acct == "" {
			acct = e.Acct
		}
		if better(e, k) {
			k = e
		}
		if k.Acct == "" {
			k.Acct = acct
		}
		out[i] = k
	}
	return out
}

// Events returns the raw, deduplicated events in [since, until), sorted by
// T. A zero bound is open.
func (l Ledger) Events(since, until time.Time) ([]Event, error) {
	return l.loadRaw(since, until)
}

// Snapshots returns the utilization snapshots in [since, until), sorted by T.
func (l Ledger) Snapshots(since, until time.Time) ([]Snapshot, error) {
	months, err := l.monthFiles("util-")
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, m := range months {
		if !inMonths(m, since, until) {
			continue
		}
		ss, err := readLines[Snapshot](l.utilPath(m))
		if err != nil {
			return nil, err
		}
		for _, s := range ss {
			if inRange(s.T, since, until) {
				out = append(out, s)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out, nil
}

// RawCutoff is the start of the oldest local day still held raw, but never
// older than now-RawRetention.
func (l Ledger) RawCutoff(now time.Time) time.Time {
	floor := now.Add(-RawRetention)
	months, err := l.monthFiles("events-")
	if err != nil {
		return floor
	}
	var oldest time.Time
	for _, m := range months {
		evs, err := readLines[Event](l.eventsPath(m))
		if err != nil {
			continue
		}
		for _, e := range evs {
			if oldest.IsZero() || e.T.Before(oldest) {
				oldest = e.T
			}
		}
		if !oldest.IsZero() {
			break
		}
	}
	if oldest.IsZero() {
		return floor
	}
	d := oldest.In(loc)
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	if start.Before(floor) {
		return floor
	}
	return start
}
