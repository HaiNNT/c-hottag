package cli

// L5: `daemon run` chdirs to its own CHOTTAG_HOME before serving, rather than
// inheriting the cwd of whichever shell first ran claude — possibly an
// untrusted project directory — and pinning that for the daemon's whole
// life. See daemonChdir's doc comment in daemon.go.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

func TestDaemonRunChdirsToItsHome(t *testing.T) {
	stubDaemonNotifier(t) // the warm pass may post a needs-login notice (F269)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, 1) // any port: the chdir stub fails the start before any bind
	var got string
	restore := SetDaemonChdirForTest(func(dir string) error { got = dir; return errors.New("stop here") })
	defer restore()
	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, &errb)); code != exit.Error {
		t.Fatalf("exit %d, want 1 (the stub's failure)", code)
	}
	if got != home {
		t.Fatalf("daemon chdir'd to %q, want its home %q (L5)", got, home)
	}
	if !strings.Contains(errb.String(), "stop here") {
		t.Errorf("a chdir failure must reach the real stderr: %q", errb.String())
	}
}

// TestDaemonRunResolvesARelativeLogBeforeChdir pins a review finding (fix
// round 1): a relative --log used to resolve against CHOTTAG_HOME, because
// daemonChdir runs before the raw path reaches runProxyWithSignal, which is
// what actually opens it. It never gets that far here — daemonChdir is
// stubbed to fail immediately, exactly like TestDaemonRunChdirsToItsHome
// above, so nothing is ever opened; daemonRunSubArgsForTest observes the
// already-resolved --log this process is about to hand runProxyWithSignal,
// one step earlier.
//
// It deliberately never chdirs this test process itself (that is
// process-global and would race every other test in this package): the
// "original cwd" a relative --log must resolve against is simply whatever
// this test binary's cwd already is.
func TestDaemonRunResolvesARelativeLogBeforeChdir(t *testing.T) {
	stubDaemonNotifier(t) // the warm pass may post a needs-login notice (F269)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, 1) // any port: the chdir stub fails the start before any bind
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	var sub []string
	daemonRunSubArgsForTest = func(s []string) { sub = s }
	t.Cleanup(func() { daemonRunSubArgsForTest = nil })
	restore := SetDaemonChdirForTest(func(string) error { return errors.New("stop here") })
	defer restore()

	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"run", "--log", "x.jsonl"}, newReporter(false, io.Discard, &errb)); code != exit.Error {
		t.Fatalf("exit %d, want 1 (the chdir stub's failure)", code)
	}
	got, present := flagValue(sub, "--log")
	if !present {
		t.Fatalf("subcommand args %v carry no --log", sub)
	}
	want := filepath.Join(wantCwd, "x.jsonl")
	if got != want {
		t.Fatalf("relative --log resolved to %q, want %q (against this process's own cwd, not CHOTTAG_HOME)", got, want)
	}
}

// TestDaemonRunPinsAnAbsoluteCHOTTAGHOMEBeforeChdir pins M1 (final review)
// and its own restore, NEW-5 (final re-review): a relative CHOTTAG_HOME
// used to re-resolve against h itself, because runProxyWithSignal
// (proxy.go) calls home() again after daemonChdir(h) has already moved the
// cwd there, so it would silently serve a second, empty home nested inside
// the first. It never gets that far here — daemonChdir is stubbed to fail
// immediately, so nothing downstream calls home() again — but the stub
// itself observes CHOTTAG_HOME at the moment daemonChdir runs (still
// inside runDaemonRun, before its restoring defer unwinds), which is the
// same ordering a real downstream home() call would see. After
// runDaemonRun returns, CHOTTAG_HOME must be back to its original,
// relative value: the Setenv that pins it is production's for the
// process's whole remaining life, but must not leak into whatever runs in
// this same test binary next.
func TestDaemonRunPinsAnAbsoluteCHOTTAGHOMEBeforeChdir(t *testing.T) {
	stubDaemonNotifier(t) // the warm pass may post a needs-login notice (F269)
	home := t.TempDir()
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wantCwd, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHOTTAG_HOME", rel)
	writeStateWithPort(t, home, 1) // any port: the chdir stub fails the start before any bind

	var duringChdir string
	restore := SetDaemonChdirForTest(func(dir string) error {
		duringChdir = os.Getenv("CHOTTAG_HOME")
		return errors.New("stop here")
	})
	defer restore()
	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, &errb)); code != exit.Error {
		t.Fatalf("exit %d, want 1 (the stub's failure)", code)
	}
	if duringChdir != home {
		t.Fatalf("CHOTTAG_HOME while daemonChdir ran = %q, want the absolute home %q pinned before it (M1): a later home() call must not re-resolve the original relative value against the new cwd", duringChdir, home)
	}
	if got := os.Getenv("CHOTTAG_HOME"); got != rel {
		t.Fatalf("CHOTTAG_HOME after runDaemonRun returned = %q, want it restored to the original %q (NEW-5): a leaked absolute value must not outlive this call", got, rel)
	}
}

// TestDaemonRunResolvesARelativeClaudePathBeforeChdir pins M1's other half
// (final review): a relative --claude containing a path separator, like
// --log, must resolve against the process's ORIGINAL cwd, not against
// CHOTTAG_HOME after daemonChdir moves it there. Same technique as
// TestDaemonRunResolvesARelativeLogBeforeChdir above.
func TestDaemonRunResolvesARelativeClaudePathBeforeChdir(t *testing.T) {
	stubDaemonNotifier(t) // the warm pass may post a needs-login notice (F269)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, 1) // any port: the chdir stub fails the start before any bind
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	var sub []string
	daemonRunSubArgsForTest = func(s []string) { sub = s }
	t.Cleanup(func() { daemonRunSubArgsForTest = nil })
	restore := SetDaemonChdirForTest(func(string) error { return errors.New("stop here") })
	defer restore()

	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"run", "--claude", "./x"}, newReporter(false, io.Discard, &errb)); code != exit.Error {
		t.Fatalf("exit %d, want 1 (the chdir stub's failure)", code)
	}
	got, present := flagValue(sub, "--claude")
	if !present {
		t.Fatalf("subcommand args %v carry no --claude", sub)
	}
	want := filepath.Join(wantCwd, "x")
	if got != want {
		t.Fatalf("relative --claude resolved to %q, want %q (against this process's own cwd, not CHOTTAG_HOME)", got, want)
	}
}

func TestDaemonChdirSeamRestoresTheCapturedValue(t *testing.T) {
	before := fmt.Sprintf("%p", daemonChdir)
	r1 := SetDaemonChdirForTest(func(string) error { return nil })
	r2 := SetDaemonChdirForTest(func(string) error { return nil })
	r2()
	r1()
	if fmt.Sprintf("%p", daemonChdir) != before {
		t.Fatal("restore must put back the value captured at swap time (F130)")
	}
}
