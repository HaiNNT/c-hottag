package journal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)

func open(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return j, dir
}

func TestPutListRoundTripAndMode(t *testing.T) {
	j, dir := open(t)
	e := Entry{PID: 10, PPID: 9, Started: t0, SID: "s1", Pool: "default", Dir: "/Users/alice/repo", CmuxSurface: "SURF-1", CmuxWorkspace: "WS-1"}
	if err := j.Put(e); err != nil {
		t.Fatal(err)
	}
	got, err := j.List()
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %+v, %v; want [%+v]", got, err, e)
	}
	g := got[0]
	if !g.Started.Equal(e.Started) {
		t.Fatalf("Started = %v, want %v", g.Started, e.Started)
	}
	g.Started = e.Started // times compared with Equal above
	if g != e {
		t.Fatalf("List = %+v; want %+v", g, e)
	}
	fi, err := os.Stat(filepath.Join(dir, e.Key()+".json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("entry file: %v mode %v, want 0600", err, fi.Mode().Perm())
	}
}

func TestPutReplacesOpenEntryWithSamePID(t *testing.T) {
	// The cmux hand-off runs the shim twice in one pid (F243); one entry.
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, Started: t0, Dir: "/a"})
	_ = j.Put(Entry{PID: 10, Started: t0.Add(time.Second), Dir: "/a"})
	got, _ := j.List()
	if len(got) != 1 || !got[0].Started.Equal(t0.Add(time.Second)) {
		t.Fatalf("got %+v, want only the second entry", got)
	}
}

func TestPutKeepsEndedEntryWithReusedPID(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, Started: t0, Ended: t0.Add(time.Minute), Outcome: OutcomeLost})
	_ = j.Put(Entry{PID: 10, Started: t0.Add(time.Hour)})
	if got, _ := j.List(); len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
}

func TestListSkipsUnreadableAndForeignFiles(t *testing.T) {
	j, dir := open(t)
	_ = j.Put(Entry{PID: 10, Started: t0})
	os.WriteFile(filepath.Join(dir, "11-1.json"), []byte("{not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "journal.lock"), nil, 0o600)
	if got, err := j.List(); err != nil || len(got) != 1 {
		t.Fatalf("got %d, %v; want 1 entry", len(got), err)
	}
}

func TestSweepOutcomes(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, PPID: 9, Started: t0})  // claude dead, shell alive -> exited
	_ = j.Put(Entry{PID: 20, PPID: 19, Started: t0}) // both dead -> lost
	_ = j.Put(Entry{PID: 30, PPID: 29, Started: t0}) // alive -> untouched
	alive := map[int]bool{9: true, 30: true, 29: true}
	now := t0.Add(time.Hour)
	n, err := j.Sweep(now, time.Time{}, func(pid int) bool { return alive[pid] })
	if err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want 2", n, err)
	}
	by := map[int]Entry{}
	es, _ := j.List()
	for _, e := range es {
		by[e.PID] = e
	}
	if by[10].Outcome != OutcomeExited || !by[10].Ended.Equal(now) {
		t.Errorf("pid 10 = %+v, want exited at now", by[10])
	}
	if by[20].Outcome != OutcomeLost {
		t.Errorf("pid 20 = %+v, want lost", by[20])
	}
	if !by[30].Open() {
		t.Errorf("pid 30 = %+v, want open", by[30])
	}
	// A second sweep changes nothing.
	if n, _ := j.Sweep(now.Add(time.Hour), time.Time{}, func(int) bool { return false }); n != 1 {
		t.Fatalf("second sweep ended %d, want 1 (only pid 30)", n)
	}
}

func TestSetNativesOnlyOpenEntriesAndOnlyChanges(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, Started: t0, SID: "s1"})
	_ = j.Put(Entry{PID: 20, Started: t0, SID: "s2", Ended: t0, Outcome: OutcomeLost, Native: "old"})
	n, err := j.SetNatives(map[string]string{"s1": nat1, "s2": u("N")})
	if err != nil || n != 1 {
		t.Fatalf("SetNatives = %d, %v; want 1", n, err)
	}
	if n, _ := j.SetNatives(map[string]string{"s1": nat1}); n != 0 {
		t.Fatalf("unchanged native rewrote %d entries", n)
	}
}

func TestPruneAgeAndCount(t *testing.T) {
	j, _ := open(t)
	now := t0.Add(30 * 24 * time.Hour)
	_ = j.Put(Entry{PID: 1, Started: t0, Ended: t0, Outcome: OutcomeLost})                   // too old
	_ = j.Put(Entry{PID: 2, Started: now.Add(-time.Hour), Ended: now, Outcome: OutcomeLost}) // kept
	_ = j.Put(Entry{PID: 3, Started: t0})                                                    // open: never pruned by age
	if err := j.Prune(now); err != nil {
		t.Fatal(err)
	}
	es, _ := j.List()
	if len(es) != 2 {
		t.Fatalf("after prune %d entries, want 2: %+v", len(es), es)
	}
}

func TestPruneCountCapDropsOldestStarted(t *testing.T) {
	j, _ := open(t)
	now := t0.Add(time.Hour)
	for i := 0; i < MaxEntries+3; i++ {
		e := Entry{PID: 100 + i, Started: t0.Add(time.Duration(i) * time.Second), Ended: now, Outcome: OutcomeLost}
		if err := j.write(e); err != nil { // seed directly; Put is not under test
			t.Fatal(err)
		}
	}
	if err := j.Prune(now); err != nil {
		t.Fatal(err)
	}
	es, _ := j.List()
	if len(es) != MaxEntries {
		t.Fatalf("got %d entries, want %d", len(es), MaxEntries)
	}
	for _, e := range es {
		if e.PID < 103 {
			t.Fatalf("pid %d should have been pruned", e.PID)
		}
	}
}

func TestPruneCountCapKeepsOpenEntryOverEnded(t *testing.T) {
	j, _ := open(t)
	now := t0.Add(time.Hour)
	// The open entry is the oldest by Started; ended ones must go first.
	if err := j.write(Entry{PID: 1, Started: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxEntries; i++ {
		if err := j.write(Entry{PID: 100 + i, Started: t0.Add(time.Duration(i) * time.Second), Ended: now, Outcome: OutcomeLost}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Prune(now); err != nil {
		t.Fatal(err)
	}
	es, _ := j.List()
	if len(es) != MaxEntries {
		t.Fatalf("got %d entries, want %d", len(es), MaxEntries)
	}
	open, gone := false, false
	for _, e := range es {
		if e.PID == 1 {
			open = true
		}
		if e.PID == 100 {
			gone = true
		}
	}
	if !open || gone {
		t.Fatalf("open kept = %v, oldest ended kept = %v; want true, false", open, gone)
	}
}

func TestSweepPPID1IsLost(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, PPID: 1, Started: t0})
	if n, err := j.Sweep(t0.Add(time.Hour), time.Time{}, func(pid int) bool { return pid == 1 }); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if es, _ := j.List(); es[0].Outcome != OutcomeLost {
		t.Fatalf("outcome = %q, want lost", es[0].Outcome)
	}
}

func TestUpdateWritesToOriginalKey(t *testing.T) {
	j, dir := open(t)
	e := Entry{PID: 10, Started: t0}
	_ = j.Put(e)
	_ = j.Update(e.Key(), func(e *Entry) bool { e.PID = 99; e.Started = t0.Add(time.Hour); return true })
	ents, _ := os.ReadDir(dir)
	files := 0
	for _, d := range ents {
		if d.Name() != "journal.lock" {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("%d entry files, want 1 (original key rewritten)", files)
	}
	if _, err := os.Stat(filepath.Join(dir, e.Key()+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateMissingKeyIsNil(t *testing.T) {
	j, _ := open(t)
	if err := j.Update("99-1", func(*Entry) bool { return true }); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateWritesWhenFnTrue(t *testing.T) {
	j, _ := open(t)
	e := Entry{PID: 10, Started: t0}
	_ = j.Put(e)
	if err := j.Update(e.Key(), func(e *Entry) bool { e.Dir = "/x"; return true }); err != nil {
		t.Fatal(err)
	}
	if got, _ := j.List(); got[0].Dir != "/x" {
		t.Fatalf("got %+v", got)
	}
}

func TestSetNativesIgnoresAnInvalidID(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, Started: t0, SID: "s1"})
	n, err := j.SetNatives(map[string]string{"s1": "x; touch /tmp/p"})
	if err != nil || n != 0 {
		t.Fatalf("SetNatives = %d, %v; want 0", n, err)
	}
	es, _ := j.List()
	if len(es) != 1 || es[0].Native != "" {
		t.Fatalf("stored %+v", es)
	}
}

func TestSweepBeforeBootIsLostEvenIfPidsAreAlive(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, PPID: 9, Started: t0})                     // started before boot
	_ = j.Put(Entry{PID: 20, PPID: 19, Started: t0.Add(2 * time.Hour)}) // started after boot
	allAlive := func(int) bool { return true }
	n, err := j.Sweep(t0.Add(3*time.Hour), t0.Add(time.Hour), allAlive)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1", n, err)
	}
	by := map[int]Entry{}
	es, _ := j.List()
	for _, e := range es {
		by[e.PID] = e
	}
	if by[10].Outcome != OutcomeLost || by[10].Open() {
		t.Errorf("pid 10 = %+v, want lost", by[10])
	}
	if !by[20].Open() {
		t.Errorf("pid 20 = %+v, want open", by[20])
	}
}

func TestSweepZeroBootTimeKeepsTheKillRule(t *testing.T) {
	j, _ := open(t)
	_ = j.Put(Entry{PID: 10, PPID: 9, Started: t0})
	if n, err := j.Sweep(t0.Add(time.Hour), time.Time{}, func(int) bool { return true }); err != nil || n != 0 {
		t.Fatalf("Sweep = %d, %v; want 0", n, err)
	}
}
