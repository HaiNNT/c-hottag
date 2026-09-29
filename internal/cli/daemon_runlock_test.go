package cli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
)

// refuseListen stubs listenTCP so that `daemon run` fails at the bind
// instead of serving. onListen, if non-nil, runs at that moment, while
// runDaemonRun is mid-flight and holds the lock.
func refuseListen(t *testing.T, onListen func()) {
	t.Helper()
	orig := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) {
		if onListen != nil {
			onListen()
		}
		return nil, errors.New("test: refusing to bind")
	}
	t.Cleanup(func() { listenTCP = orig })
}

// Spec §4.6: a second `daemon run` fails fast with "daemon already running
// (pid N)". It must do so before it opens daemon.log. Otherwise it would
// hold a second rotate.Writer on the running daemon's own log, which is the
// corruption daemonLogSibling exists to prevent, just to report that it
// cannot start.
func TestDaemonRunRefusesWhileAnotherDaemonHoldsTheLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, closedPort(t))
	holdLock(t, home, fakeRecord(4242))
	reached := false
	refuseListen(t, func() { reached = true })

	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "daemon already running (pid 4242)") {
		t.Errorf("stderr = %q, want spec §4.6's \"daemon already running (pid 4242)\"", errb.String())
	}
	if reached {
		t.Error("a refused daemon went on to the bind")
	}
	if _, err := os.Stat(filepath.Join(home, "daemon.log")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused daemon opened daemon.log (stat err = %v): it must take the lock before opening the running daemon's log", err)
	}
}

// At the moment of the bind, the lock is held and its record is this
// process's own.
func TestDaemonRunHoldsTheLockWithItsOwnRecordAtTheBind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, closedPort(t))
	var st daemonlock.Status
	var inspectErr error
	called := false
	refuseListen(t, func() {
		called = true
		st, inspectErr = daemonlock.Inspect(home)
	})

	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, io.Discard)); code != 1 {
		t.Fatalf("exit = %d, want 1 (the stubbed bind refuses)", code)
	}
	if !called {
		t.Fatal("runDaemonCmd never reached the bind")
	}
	if inspectErr != nil || !st.Running || st.Record.PID != os.Getpid() {
		t.Fatalf("Inspect at the bind = %+v, %v; want Running with this process's pid %d", st, inspectErr, os.Getpid())
	}
	if d := time.Since(st.Record.Started); d < 0 || d > time.Minute {
		t.Errorf("record Started = %v, want about now", st.Record.Started)
	}
}

// In production, runDaemonRun returning means the process is exiting, and
// the kernel drops the lock. In-process (every test here), the lock must
// go when the function returns, or the next test's daemon in any home this
// process reuses would be refused.
func TestDaemonRunReleasesTheLockWhenItReturns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, closedPort(t))
	refuseListen(t, nil)

	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, io.Discard)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	st, err := daemonlock.Inspect(home)
	if err != nil || st.Running {
		t.Fatalf("Inspect after runDaemonCmd returned = %+v, %v; want not running", st, err)
	}
}

func TestDaemonRejectsAnUnknownVerb(t *testing.T) {
	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"frobnicate"}, newReporter(false, io.Discard, &errb)); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), `unknown daemon command "frobnicate"`) || !strings.Contains(errb.String(), "daemon run") {
		t.Errorf("stderr = %q, want it to name the unknown verb and show the usage", errb.String())
	}
}
