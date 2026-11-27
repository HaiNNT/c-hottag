package usagelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeDaily(t *testing.T, l Ledger, rows ...Daily) {
	t.Helper()
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	for _, r := range rows {
		b, _ := json.Marshal(r)
		buf = append(append(buf, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(l.Dir, "daily.jsonl"), buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDaysMergesRollupAndRaw(t *testing.T) {
	l := Open(t.TempDir())
	writeDaily(t, l, Daily{Day: "2026-07-01", Acct: "A", Model: "m", Src: "live", N: 3, Out: 30})
	l.AppendEvents([]Event{
		{T: at(2026, 11, 20, 1), Src: "live", Acct: "A", Model: "m", In: 1, Out: 2, CR: 3, CW5m: 4, CW1h: 5, Web: 6},
		{T: at(2026, 11, 20, 9), Src: "live", Acct: "A", Model: "m", In: 1, Out: 2},
		{T: at(2026, 11, 20, 9), Src: "slot", Model: "m", Out: 7},
	})
	got, err := l.Days(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := []Daily{
		{Day: "2026-07-01", Acct: "A", Model: "m", Src: "live", N: 3, Out: 30},
		{Day: "2026-11-20", Model: "m", Src: "slot", N: 1, Out: 7},
		{Day: "2026-11-20", Acct: "A", Model: "m", Src: "live", N: 2, In: 2, Out: 4, CR: 3, CW5m: 4, CW1h: 5, Web: 6},
	}
	// sort order: Day, Acct, ... so unknown acct ("") comes first
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	got, _ = l.Days(at(2026, 11, 1, 0), time.Time{})
	if len(got) != 2 {
		t.Fatalf("ranged %+v", got)
	}
}

func sumDays(ds []Daily) (n, out int64) {
	for _, d := range ds {
		n += d.N
		out += d.Out
	}
	return
}

func TestCompactRollsUpOldMonths(t *testing.T) {
	home := t.TempDir()
	l := Open(home)
	now := at(2026, 11, 26, 12)
	l.AppendEvents([]Event{
		{T: at(2026, 7, 1, 10), MID: "a", Src: "live", Acct: "A", Model: "m", Out: 5},
		{T: at(2026, 7, 31, 23), MID: "b", Src: "live", Acct: "A", Model: "m", Out: 6},
		{T: at(2026, 11, 20, 10), MID: "c", Src: "live", Acct: "A", Model: "m", Out: 7},
	})
	before, _ := l.Days(time.Time{}, time.Time{})
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(l.Dir, "events-2026-07.jsonl")) {
		t.Fatal("old month not deleted")
	}
	if !fileExists(filepath.Join(l.Dir, "events-2026-11.jsonl")) {
		t.Fatal("recent month deleted")
	}
	rows, _ := readLines[Daily](filepath.Join(l.Dir, "daily.jsonl"))
	if len(rows) != 2 {
		t.Fatalf("daily rows %+v", rows)
	}
	after, _ := l.Days(time.Time{}, time.Time{})
	bn, bo := sumDays(before)
	an, ao := sumDays(after)
	if bn != an || bo != ao || an != 3 || ao != 18 {
		t.Fatalf("before %d/%d after %d/%d", bn, bo, an, ao)
	}
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	rows, _ = readLines[Daily](filepath.Join(l.Dir, "daily.jsonl"))
	again, _ := l.Days(time.Time{}, time.Time{})
	if len(rows) != 2 || !reflect.DeepEqual(after, again) {
		t.Fatalf("second compact changed things: %+v", rows)
	}
}

func TestCompactKeepsMonthWithRecentEvent(t *testing.T) {
	l := Open(t.TempDir())
	now := at(2026, 11, 26, 12) // cut = 2026-08-28
	l.AppendEvents([]Event{
		{T: at(2026, 8, 10, 10), Src: "live", Model: "m"},
		{T: at(2026, 8, 30, 10), Src: "live", Model: "m"},
	})
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(l.Dir, "events-2026-08.jsonl")) {
		t.Fatal("month with a recent event was rolled up")
	}
}

func TestCompactCrashBeforeDeleteDoesNotDoubleCount(t *testing.T) {
	l := Open(t.TempDir())
	now := at(2026, 11, 26, 12)
	l.AppendEvents([]Event{{T: at(2026, 7, 1, 10), Src: "live", Model: "m", Out: 5}})
	// simulate: rows already in daily.jsonl but raw file still there
	writeDaily(t, l, Daily{Day: "2026-07-01", Model: "m", Src: "live", N: 1, Out: 5})
	if err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	days, _ := l.Days(time.Time{}, time.Time{})
	if n, _ := sumDays(days); n != 1 {
		t.Fatalf("double counted: %+v", days)
	}
	if fileExists(filepath.Join(l.Dir, "events-2026-07.jsonl")) {
		t.Fatal("raw not deleted")
	}
}
