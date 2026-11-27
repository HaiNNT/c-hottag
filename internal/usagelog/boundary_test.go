package usagelog

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCompactCrossMonthDuplicate(t *testing.T) {
	l := Open(t.TempDir())
	now := at(2026, 11, 26, 12)
	l.AppendEvents([]Event{
		{T: time.Date(2026, 8, 31, 23, 59, 59, 0, loc), MID: "x", Src: "live", Acct: "B", Model: "m", Out: 100},
		{T: time.Date(2026, 9, 1, 0, 0, 1, 0, loc), MID: "x", Src: "slot", Model: "m", Out: 100},
		{T: at(2026, 9, 1, 5), MID: "y", Src: "live", Acct: "A", Model: "m", Out: 10},
		{T: at(2026, 8, 5, 5), MID: "z", Src: "live", Acct: "A", Model: "m", Out: 1},
	})
	before, _ := l.Days(time.Time{}, time.Time{})
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	after, _ := l.Days(time.Time{}, time.Time{})
	bn, bo := sumDays(before)
	an, ao := sumDays(after)
	if bn != 3 || bn != an || bo != ao {
		t.Fatalf("before %d/%d after %d/%d: %+v", bn, bo, an, ao, after)
	}
	for _, d := range after {
		if d.Acct == "" {
			t.Fatalf("unattributed row: %+v", after)
		}
	}
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	again, _ := l.Days(time.Time{}, time.Time{})
	if !reflect.DeepEqual(after, again) {
		t.Fatalf("rerun changed: %+v", again)
	}
}

func TestCompactRepairsAfterCrashBeforeRewrite(t *testing.T) {
	l := Open(t.TempDir())
	now := at(2026, 11, 26, 12)
	l.AppendEvents([]Event{
		{T: time.Date(2026, 8, 31, 23, 59, 59, 0, loc), MID: "x", Src: "live", Acct: "B", Model: "m", Out: 100},
		{T: time.Date(2026, 9, 1, 0, 0, 1, 0, loc), MID: "x", Src: "slot", Model: "m", Out: 100},
	})
	// daily written, nothing else done yet
	writeDaily(t, l, Daily{Day: "2026-08-31", Acct: "B", Model: "m", Src: "live", N: 1, Out: 100})
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	days, _ := l.Days(time.Time{}, time.Time{})
	if n, o := sumDays(days); n != 1 || o != 100 {
		t.Fatalf("got %+v", days)
	}
}

func TestEventsDedupAcrossRangeBoundary(t *testing.T) {
	l := Open(t.TempDir())
	l.AppendEvents([]Event{
		{T: time.Date(2026, 10, 31, 23, 59, 59, 0, loc), MID: "x", Src: "live", Acct: "B", Model: "m", Out: 5},
		{T: time.Date(2026, 11, 1, 0, 0, 1, 0, loc), MID: "x", Src: "slot", Model: "m", Out: 5},
	})
	whole, _ := l.Events(time.Time{}, time.Time{})
	if len(whole) != 1 {
		t.Fatalf("whole %+v", whole)
	}
	got, _ := l.Events(at(2026, 11, 1, 0), time.Time{})
	if len(got) != 0 {
		t.Fatalf("range returned the losing copy: %+v", got)
	}
}

func TestAppendAfterTornLine(t *testing.T) {
	l := Open(t.TempDir())
	l.AppendEvents([]Event{{T: at(2026, 11, 5, 10), Src: "live", Model: "m", Out: 1}})
	p := filepath.Join(l.Dir, "events-2026-11.jsonl")
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"t":"2026-11-05T10:00`)
	f.Close()
	l.AppendEvents([]Event{{T: at(2026, 11, 6, 10), Src: "live", Model: "m", Out: 2}})
	got, err := l.Events(time.Time{}, time.Time{})
	if err != nil || len(got) != 2 || got[1].Out != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
}
