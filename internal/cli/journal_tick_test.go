package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/journal"
	"github.com/HaiNNT/c-hottag/internal/sessions"
)

const tickNative = "11111111-2222-3333-4444-555555555555"

func stubAlive(t *testing.T, alive map[int]bool) {
	t.Helper()
	old, oldBoot := journalAlive, journalBootTime
	journalAlive = func(pid int) bool { return alive[pid] }
	journalBootTime = func() time.Time { return time.Time{} }
	t.Cleanup(func() { journalAlive, journalBootTime = old, oldBoot })
}

func openJ(t *testing.T, home string) *journal.Journal {
	t.Helper()
	j, err := journal.Open(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestJournalPassRecordsNativeAndEnds(t *testing.T) {
	home := t.TempDir()
	now := time.Now().Truncate(time.Second)
	j := openJ(t, home)
	for _, e := range []journal.Entry{
		{PID: 10, PPID: 9, SID: "s1", Started: now.Add(-time.Hour)},
		{PID: 20, PPID: 19, SID: "s2", Started: now.Add(-time.Hour)},
	} {
		if err := j.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	tr := sessions.NewTracker()
	tr.Seen("s1", "p", "a", true, tickNative, now)
	stubAlive(t, map[int]bool{10: true, 9: true})
	p := &journalPass{home: home, log: &bytes.Buffer{}}
	p.tick(tr, now)
	es, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		switch e.PID {
		case 10:
			if e.Native != tickNative || !e.Open() {
				t.Fatalf("entry 10: %+v", e)
			}
		case 20:
			if e.Open() || e.Outcome != journal.OutcomeLost || !e.Ended.Equal(now) {
				t.Fatalf("entry 20: %+v", e)
			}
		}
	}
}

func TestJournalPassThrottled(t *testing.T) {
	home := t.TempDir()
	now := time.Now().Truncate(time.Second)
	j := openJ(t, home)
	if err := j.Put(journal.Entry{PID: 10, PPID: 9, SID: "s1", Started: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	alive := map[int]bool{10: true, 9: true}
	stubAlive(t, alive)
	p := &journalPass{home: home, log: &bytes.Buffer{}}
	p.tick(nil, now)
	alive[10] = false
	p.tick(nil, now.Add(5*time.Second))
	es, _ := j.List()
	if len(es) != 1 || !es[0].Open() {
		t.Fatalf("throttled tick acted: %+v", es)
	}
	p.tick(nil, now.Add(16*time.Second))
	es, _ = j.List()
	if es[0].Open() || es[0].Outcome != journal.OutcomeExited {
		t.Fatalf("later tick did not end it: %+v", es[0])
	}
}

func TestJournalPassCrashBatch(t *testing.T) {
	home := t.TempDir()
	now := time.Now().Truncate(time.Second)
	j := openJ(t, home)
	for i, n := range []string{"00000000-0000-0000-0000-00000000000a", "00000000-0000-0000-0000-00000000000b", "00000000-0000-0000-0000-00000000000c"} {
		if err := j.Put(journal.Entry{PID: 10 + i, PPID: 1, SID: "s", Native: n, Started: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	stubAlive(t, map[int]bool{})
	(&journalPass{home: home, log: &bytes.Buffer{}}).tick(nil, now)
	es, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if got := journal.LostBatch(es, now); len(got) != 3 {
		t.Fatalf("LostBatch = %d entries, want 3", len(got))
	}
}

func TestJournalPassLogsErrorOnce(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "sessions"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubAlive(t, map[int]bool{})
	var buf bytes.Buffer
	p := &journalPass{home: home, log: &buf}
	now := time.Now()
	p.tick(nil, now)
	p.tick(nil, now.Add(20*time.Second))
	if n := strings.Count(buf.String(), "journal:"); n != 1 {
		t.Fatalf("logged %d times: %q", n, buf.String())
	}
}

func TestNilJournalPass(t *testing.T) {
	(*journalPass)(nil).tick(nil, time.Now())
}

func TestTickEndsEntriesStartedBeforeBootAsLost(t *testing.T) {
	home := t.TempDir()
	now := time.Now().Truncate(time.Second)
	j := openJ(t, home)
	if err := j.Put(journal.Entry{PID: 10, PPID: 9, SID: "s", Started: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	stubAlive(t, map[int]bool{10: true, 9: true}) // pids reused after the reboot
	journalBootTime = func() time.Time { return now.Add(-time.Hour) }
	(&journalPass{home: home, log: &bytes.Buffer{}}).tick(nil, now)
	es, _ := j.List()
	if len(es) != 1 || es[0].Outcome != journal.OutcomeLost {
		t.Fatalf("entries %+v, want one lost", es)
	}
}
