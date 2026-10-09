package shim

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/journal"
	"github.com/HaiNNT/c-hottag/internal/proxy"
)

func TestResumeArg(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, ""},
		{[]string{"--resume", "abc"}, "abc"},
		{[]string{"--resume=abc"}, "abc"},
		{[]string{"-r", "abc"}, "abc"},
		{[]string{"-r=abc"}, "abc"},
		{[]string{"--resume"}, ""},
		{[]string{"--resume", "--model", "x"}, ""},
		{[]string{"-p", "hi", "--resume", "abc"}, "abc"},
		{[]string{"--continue"}, ""},
	} {
		if got := resumeArg(c.args); got != c.want {
			t.Errorf("resumeArg(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestJournalEntryFields(t *testing.T) {
	started := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	env := []string{"CMUX_WORKSPACE_ID=WS-1", "CMUX_SURFACE_ID=S-1"}
	e := journalEntry(10, 5, started, "s1", "work", "/Users/alice/repo", env, []string{"--resume", "N0"})
	want := journal.Entry{PID: 10, PPID: 5, Started: started, SID: "s1", Pool: "work",
		Dir: "/Users/alice/repo", CmuxWorkspace: "WS-1", CmuxSurface: "S-1", ResumeOf: "N0"}
	if e != want {
		t.Errorf("entry = %+v, want %+v", e, want)
	}
	e = journalEntry(10, 5, started, "", "work", "/Users/alice/repo", nil, nil)
	if e.CmuxWorkspace != "" || e.CmuxSurface != "" || e.Pool != "" {
		t.Errorf("entry = %+v, want no cmux fields and no pool without a sid", e)
	}
}

func TestJournalEntryRecordsFork(t *testing.T) {
	started := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	e := journalEntry(10, 5, started, "s1", "work", "/Users/alice/repo", nil, []string{"--resume", "N0", "--fork-session"})
	if !e.Fork || e.ResumeOf != "N0" {
		t.Errorf("entry = %+v, want Fork and ResumeOf N0", e)
	}
	e = journalEntry(10, 5, started, "s1", "work", "/Users/alice/repo", nil, []string{"--resume", "N0"})
	if e.Fork {
		t.Errorf("entry = %+v, want no Fork", e)
	}
}

func journalFixture(t *testing.T) (home string, env []string) {
	t.Helper()
	home = t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()})
	t.Cleanup(daemon.Close)
	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	return home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}
}

func TestRunWritesJournalEntry(t *testing.T) {
	home, env := journalFixture(t)
	var got execCall
	defer swapExec(home, &got)()
	if code := Run(nil, home, env, ownVersion, io.Discard, io.Discard); code != 0 {
		t.Fatalf("Run = %d", code)
	}
	if len(got.live) != 1 {
		t.Fatalf("registry = %v, want one entry", got.live)
	}
	j, err := journal.Open(filepath.Join(home, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	es, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if len(es) != 1 {
		t.Fatalf("journal = %+v, want one entry", es)
	}
	e := es[0]
	if e.PID != os.Getpid() || e.Dir != wd || e.SID != got.live[0].SID || e.Pool != got.live[0].Pool {
		t.Errorf("entry = %+v, registry = %+v, want this pid, dir %q and the registry's sid and pool", e, got.live[0], wd)
	}
}

func TestRunLaunchesWhenJournalUnwritable(t *testing.T) {
	home, env := journalFixture(t)
	if err := os.WriteFile(filepath.Join(home, "sessions"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got execCall
	defer swapExec(home, &got)()
	var stderr strings.Builder
	if code := Run(nil, home, env, ownVersion, io.Discard, &stderr); code != 0 {
		t.Fatalf("Run = %d, stderr %s", code, stderr.String())
	}
	if got.bin == "" {
		t.Error("execFn was not reached")
	}
	if !strings.Contains(stderr.String(), "could not record the session") {
		t.Errorf("stderr = %q, want could not record the session", stderr.String())
	}
}
