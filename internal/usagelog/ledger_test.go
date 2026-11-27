package usagelog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	loc = time.FixedZone("test", 7*3600)
	os.Exit(m.Run())
}

func at(y int, mo time.Month, d, h int) time.Time {
	return time.Date(y, mo, d, h, 0, 0, 0, loc)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestAppendAndReadRoundTrip(t *testing.T) {
	home := t.TempDir()
	l := Open(home)
	nov := Event{T: at(2026, 11, 5, 10), Src: "live", Model: "m", Out: 2}
	oct := Event{T: at(2026, 10, 5, 10), Src: "live", Model: "m", Out: 1}
	if err := l.AppendEvents([]Event{nov, oct}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"events-2026-10.jsonl", "events-2026-11.jsonl"} {
		fi, err := os.Stat(filepath.Join(home, "usage", f))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, fi.Mode().Perm())
		}
	}
	got, err := l.Events(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Out != 1 || got[1].Out != 2 {
		t.Fatalf("got %+v", got)
	}
	got, _ = l.Events(at(2026, 11, 1, 0), at(2026, 12, 1, 0))
	if len(got) != 1 || got[0].Out != 2 {
		t.Fatalf("ranged %+v", got)
	}
}

func TestEventsDedupPrefersAccount(t *testing.T) {
	slot := Event{T: at(2026, 11, 5, 10), MID: "m1", Src: "slot", Model: "m", Out: 84}
	live := Event{T: at(2026, 11, 5, 10), MID: "m1", Src: "live", Acct: "B", Model: "m", Out: 84}
	for _, order := range [][]Event{{slot, live}, {live, slot}} {
		l := Open(t.TempDir())
		for _, e := range order {
			if err := l.AppendEvents([]Event{e}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := l.Events(time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Acct != "B" {
			t.Fatalf("got %+v", got)
		}
	}
	// the kept event takes Acct from a duplicate even if it loses elsewhere
	l := Open(t.TempDir())
	a := Event{T: at(2026, 11, 5, 10), MID: "m2", Src: "slot", Acct: "A", Model: "m", Out: 1}
	b := Event{T: at(2026, 11, 5, 10), MID: "m2", Src: "slot", Model: "m", Out: 9}
	l.AppendEvents([]Event{a, b})
	got, _ := l.Events(time.Time{}, time.Time{})
	if len(got) != 1 || got[0].Acct != "A" {
		t.Fatalf("got %+v", got)
	}
	// larger Out wins among transcript events
	l = Open(t.TempDir())
	l.AppendEvents([]Event{
		{T: at(2026, 11, 5, 10), MID: "m3", Src: "slot", Model: "m", Out: 1},
		{T: at(2026, 11, 5, 10), MID: "m3", Src: "slot", Model: "m", Out: 84},
	})
	got, _ = l.Events(time.Time{}, time.Time{})
	if len(got) != 1 || got[0].Out != 84 {
		t.Fatalf("got %+v", got)
	}
	// RID fallback, and no key means no dedup
	l = Open(t.TempDir())
	l.AppendEvents([]Event{
		{T: at(2026, 11, 5, 10), RID: "r", Src: "live", Model: "m"},
		{T: at(2026, 11, 5, 10), RID: "r", Src: "live", Model: "m"},
		{T: at(2026, 11, 5, 10), Src: "live", Model: "m"},
		{T: at(2026, 11, 5, 10), Src: "live", Model: "m"},
	})
	got, _ = l.Events(time.Time{}, time.Time{})
	if len(got) != 3 {
		t.Fatalf("got %d", len(got))
	}
}

func TestCorruptLineSkipped(t *testing.T) {
	home := t.TempDir()
	l := Open(home)
	l.AppendEvents([]Event{{T: at(2026, 11, 5, 10), Src: "live", Model: "m", Out: 1}})
	f, _ := os.OpenFile(filepath.Join(home, "usage", "events-2026-11.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("garbage{\n")
	f.Close()
	l.AppendEvents([]Event{{T: at(2026, 11, 6, 10), Src: "live", Model: "m", Out: 2}})
	got, err := l.Events(time.Time{}, time.Time{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestRawCutoff(t *testing.T) {
	l := Open(t.TempDir())
	now := at(2026, 11, 26, 12)
	if got := l.RawCutoff(now); !got.Equal(now.Add(-RawRetention)) {
		t.Fatalf("empty: %v", got)
	}
	l.AppendEvents([]Event{
		{T: at(2026, 9, 12, 15), Src: "live", Model: "m"},
		{T: at(2026, 10, 3, 1), Src: "live", Model: "m"},
		{T: at(2026, 11, 3, 1), Src: "live", Model: "m"},
	})
	if got := l.RawCutoff(now); !got.Equal(at(2026, 9, 12, 0)) {
		t.Fatalf("got %v", got)
	}
	// an unrolled event older than now-90d is clamped
	l.AppendEvents([]Event{{T: at(2026, 7, 1, 1), Src: "live", Model: "m"}})
	if got := l.RawCutoff(now); !got.Equal(now.Add(-RawRetention)) {
		t.Fatalf("clamp: %v", got)
	}
}

func TestImportStateRoundTrip(t *testing.T) {
	l := Open(t.TempDir())
	m, err := l.ImportState()
	if err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	in := map[string]FileMark{"/Users/alice/a.jsonl": {Size: 10, ModTime: at(2026, 11, 5, 1).UTC(), Offset: 7}}
	if err := l.SaveImportState(in); err != nil {
		t.Fatal(err)
	}
	out, err := l.ImportState()
	if err != nil {
		t.Fatal(err)
	}
	got := out["/Users/alice/a.jsonl"]
	if got.Size != 10 || got.Offset != 7 || !got.ModTime.Equal(in["/Users/alice/a.jsonl"].ModTime) {
		t.Fatalf("got %+v", got)
	}
}

func TestSnapshotsRoundTrip(t *testing.T) {
	l := Open(t.TempDir())
	p7 := 42.5
	s1 := Snapshot{T: at(2026, 11, 5, 10), Acct: "A", Tier: "pro", Units: 1, P7: &p7, R7: 99}
	s2 := Snapshot{T: at(2026, 10, 5, 10), Acct: "B"}
	if err := l.AppendSnapshot(s1); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendSnapshot(s2); err != nil {
		t.Fatal(err)
	}
	got, err := l.Snapshots(time.Time{}, time.Time{})
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v %v", got, err)
	}
	if got[0].Acct != "B" || got[0].P5 != nil || got[0].P7 != nil {
		t.Fatalf("nil lost: %+v", got[0])
	}
	if got[1].P5 != nil || got[1].P7 == nil || *got[1].P7 != 42.5 || got[1].R7 != 99 {
		t.Fatalf("got %+v", got[1])
	}
	got, _ = l.Snapshots(at(2026, 11, 1, 0), time.Time{})
	if len(got) != 1 || got[0].Acct != "A" {
		t.Fatalf("ranged %+v", got)
	}
}

func TestConcurrentAppend(t *testing.T) {
	home := t.TempDir()
	l := Open(home)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if err := l.AppendEvents([]Event{{T: at(2026, 11, 5, 10), Src: "live", Model: "m", Out: 1}}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	f, err := os.Open(filepath.Join(home, "usage", "events-2026-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		n++
	}
	if n != 1600 {
		t.Fatalf("lines %d", n)
	}
}

func TestLockExcludes(t *testing.T) {
	l := Open(t.TempDir())
	u, err := l.Lock()
	if err != nil {
		t.Fatal(err)
	}
	u()
	u2, err := l.Lock()
	if err != nil {
		t.Fatal(err)
	}
	u2()
}
