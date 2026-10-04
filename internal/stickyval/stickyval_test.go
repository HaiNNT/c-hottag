package stickyval

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) (*Map, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run", "validate-sessions.json")
	m, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func set(t *testing.T, m *Map, sid, account string, at time.Time) {
	t.Helper()
	if err := m.Set(sid, account, "", at); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTripAcrossOpen(t *testing.T) {
	m, p := open(t)
	if err := m.Set("sid1", "C", "c@example.com", t0); err != nil {
		t.Fatal(err)
	}
	m2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	e, st := m2.Lookup("sid1", t0)
	if st != Live || e.Account != "C" || e.Email != "c@example.com" {
		t.Fatalf("got %+v, %v", e, st)
	}
}

func TestMalformedFileIsReportedAndUsable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v.json")
	if err := os.WriteFile(p, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(p)
	if err == nil || m == nil {
		t.Fatalf("Open = %v, %v; want an error and a usable map", m, err)
	}
	set(t, m, "s", "C", t0)
}

func TestWrongVersionFileIsReportedAndUsable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"sessions":{"s":{"account":"C","used":"2026-10-04T12:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Open(p)
	if err == nil || m == nil || m.Len() != 0 {
		t.Fatalf("Open = %v, %v; want an error and an empty usable map", m, err)
	}
	set(t, m, "s", "D", t0)
}

func TestEntryUnusedForSevenDaysIsDroppedButRemembered(t *testing.T) {
	m, p := open(t)
	set(t, m, "old", "C", t0)
	if _, ok := m.Get("old", t0.Add(MaxAge)); !ok {
		t.Fatal("an entry exactly MaxAge old was dropped")
	}
	if e, st := m.Lookup("old", t0.Add(MaxAge+time.Second)); st != Dropped || e.Account != "C" {
		t.Fatalf("an entry unused for over 7 days: %+v, %v; want Dropped with its account", e, st)
	}
	set(t, m, "new", "D", t0.Add(MaxAge+time.Second)) // the write prunes it
	m2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Len() != 1 {
		t.Fatalf("file holds %d live entries, want 1", m2.Len())
	}
	if e, st := m2.Lookup("old", t0.Add(MaxAge+time.Second)); st != Dropped || e.Account != "C" {
		t.Fatalf("after a reopen: %+v, %v; want Dropped with its account", e, st)
	}
	if _, st := m2.Lookup("never", t0); st != Absent {
		t.Fatalf("unknown sid state %v, want Absent", st)
	}
}

func TestMapKeepsTheNewestFiveHundred(t *testing.T) {
	m, p := open(t)
	for i := 0; i < MaxEntries+20; i++ {
		set(t, m, fmt.Sprintf("sid%04d", i), "C", t0.Add(time.Duration(i)*time.Minute))
	}
	now := t0.Add(time.Duration(MaxEntries+20) * time.Minute)
	if m.Len() != MaxEntries {
		t.Fatalf("Len = %d, want %d", m.Len(), MaxEntries)
	}
	if _, st := m.Lookup("sid0019", now); st != Dropped {
		t.Errorf("one of the oldest 20 is %v, want Dropped", st)
	}
	if _, ok := m.Get("sid0020", now); !ok {
		t.Error("the oldest kept entry was dropped")
	}
	m2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Len() != MaxEntries {
		t.Fatalf("file holds %d, want %d", m2.Len(), MaxEntries)
	}
}

func TestUseRefreshesAnEntrySoItOutlivesNewerOnes(t *testing.T) {
	m, _ := open(t)
	set(t, m, "used", "C", t0)
	for i := 0; i < MaxEntries-1; i++ {
		set(t, m, fmt.Sprintf("s%d", i), "C", t0.Add(time.Minute))
	}
	set(t, m, "used", "C", t0.Add(2*time.Hour)) // a hit well past touchEvery
	set(t, m, "extra", "C", t0.Add(3*time.Hour))
	if _, ok := m.Get("used", t0.Add(3*time.Hour)); !ok {
		t.Fatal("a recently used entry was dropped before older ones")
	}
}

func TestAccountsListsRecentlyUsedOnesOnce(t *testing.T) {
	m, _ := open(t)
	set(t, m, "a", "C", t0)
	set(t, m, "b", "C", t0)
	set(t, m, "c", "D", t0.Add(-48*time.Hour))
	got := m.Accounts(t0.Add(time.Hour), WarmWithin)
	if len(got) != 1 || got[0] != "C" {
		t.Fatalf("Accounts = %v, want [C]", got)
	}
}

func TestFileHoldsOnlySessionAccountEmailAndTime(t *testing.T) {
	m, p := open(t)
	if err := m.Set("sid1", "A", "alice@example.com", t0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":2,"sessions":{"sid1":{"account":"A","email":"alice@example.com","used":"2026-10-04T12:00:00Z"}}}`
	if string(data) != want {
		t.Fatalf("file = %s, want %s", data, want)
	}
}

func TestConcurrentUseIsRaceFreeAndTheNewestSnapshotWins(t *testing.T) {
	m, p := open(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if err := m.Set(fmt.Sprintf("g%d", i), "C", "", t0.Add(time.Duration(j)*2*time.Hour)); err != nil {
					t.Error(err)
				}
				m.Get("g0", t0)
				m.Lookup("g1", t0)
			}
		}(i)
	}
	wg.Wait()
	m2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Len() != 8 {
		t.Fatalf("file holds %d, want 8", m2.Len())
	}
	for i := 0; i < 8; i++ {
		e, st := m2.Lookup(fmt.Sprintf("g%d", i), t0)
		if st != Live || !e.Used.Equal(t0.Add(48*time.Hour)) {
			t.Fatalf("g%d on disk = %+v, %v; want the last write", i, e, st)
		}
	}
}

func TestFailedWriteIsRetriedByTheNextSetAndByTheTimer(t *testing.T) {
	m, p := open(t)
	fail := true
	m.writeFile = func(path string, data []byte, perm os.FileMode) error {
		if fail {
			return fmt.Errorf("disk full")
		}
		return os.WriteFile(path, data, perm)
	}
	var timers []func()
	m.after = func(d time.Duration, f func()) {
		if d != retryAfter {
			t.Errorf("retry delay %v, want %v", d, retryAfter)
		}
		timers = append(timers, f)
	}
	if err := m.Set("a", "C", "", t0); err == nil {
		t.Fatal("the failed write was not reported")
	}
	if err := m.Set("b", "D", "", t0); err == nil {
		t.Fatal("the second failed write was not reported")
	}
	if len(timers) != 1 {
		t.Fatalf("%d retries scheduled, want one", len(timers))
	}
	// A hit on the same entry within the hour must retry too: the file lags.
	fail = false
	if err := m.Set("b", "D", "", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m2, err := Open(p)
	if err != nil || m2.Len() != 2 {
		t.Fatalf("after the retry the file holds %d sessions, %v; want 2", m2.Len(), err)
	}
	// The timer firing later finds nothing to do.
	timers[0]()
	// A failure with no validate afterwards is retried by the timer.
	fail = true
	if err := m.Set("c", "C", "", t0.Add(2*time.Hour)); err == nil {
		t.Fatal("expected a failure")
	}
	fail = false
	timers[len(timers)-1]()
	m3, _ := Open(p)
	if m3.Len() != 3 {
		t.Fatalf("after the timer the file holds %d sessions, want 3", m3.Len())
	}
}

func TestOlderSnapshotNeverLandsAfterANewerFailedOne(t *testing.T) {
	m, p := open(t)
	set(t, m, "a", "C", t0)
	m.writeFile = func(string, []byte, os.FileMode) error { return fmt.Errorf("disk full") }
	m.after = func(time.Duration, func()) {}
	_ = m.Set("b", "D", "", t0) // fails: seq 2 attempted
	m.writeFile = os.WriteFile
	if err := m.write(1, []byte(`{"version":2,"sessions":{}}`)); err != nil {
		t.Fatal(err)
	}
	m2, _ := Open(p)
	if m2.Len() != 1 {
		t.Fatalf("an older snapshot overwrote the file: %d sessions", m2.Len())
	}
}
