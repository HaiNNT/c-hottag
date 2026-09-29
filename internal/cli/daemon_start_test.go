package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// startHome is a fresh home with CHOTTAG_HOME pointing at it. The caller
// writes state.json with a test port, never the default 47821.
func startHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	return home
}

func startNoSpawn(t *testing.T) {
	t.Helper()
	stubSpawn(t, func(string, string, string) error {
		t.Error("daemon start spawned a daemon when it must not")
		return nil
	})
}

func TestDaemonStartReportsAnAlreadyRunningHealthyDaemon(t *testing.T) {
	home := startHome(t)
	d := newHealthDaemon(t, true, "")
	writeStateWithPort(t, home, d.port)
	holdLock(t, home, fakeRecord(4242))
	startNoSpawn(t)

	var out, errb bytes.Buffer
	if code := runDaemonCmd([]string{"start"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "already running (pid 4242)") {
		t.Errorf("stdout = %q, want spec §5's \"already running (pid 4242)\"", out.String())
	}
}

// The lock is held but nothing answers the health probe: a daemon wedged
// after taking its lock but before serving. The health probe alone could
// never see this; the lock is what finds it.
func TestDaemonStartRefusesAWedgedDaemon(t *testing.T) {
	home := startHome(t)
	writeStateWithPort(t, home, closedPort(t))
	holdLock(t, home, fakeRecord(4242))
	startNoSpawn(t)

	var errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "pid 4242") || !strings.Contains(errb.String(), "chottag daemon restart") {
		t.Errorf("stderr = %q, want it to name pid 4242 and suggest `chottag daemon restart`", errb.String())
	}
}

// TestDaemonStartReportsAnUnreadableRecordThatIsAlsoWedged pins the minor
// finding: when the lock is held, its record is unreadable, AND nothing
// answers the health probe, ensureStarted's usual "run `chottag daemon
// restart`" suggestion is wrong — restart itself refuses exactly this case
// (its own ErrUnreadableRecord check), so the suggestion is a dead end.
// unreadableRecordMessage is what start must print instead.
func TestDaemonStartReportsAnUnreadableRecordThatIsAlsoWedged(t *testing.T) {
	home := startHome(t)
	writeStateWithPort(t, home, closedPort(t))
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(daemonlock.Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	startNoSpawn(t)

	var errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "pid is unknown") {
		t.Errorf("stderr = %q, want unreadableRecordMessage's \"pid is unknown\"", errb.String())
	}
	if strings.Contains(errb.String(), "chottag daemon restart") {
		t.Errorf("stderr = %q, must not suggest `chottag daemon restart`: restart refuses this exact case too", errb.String())
	}
}

func TestDaemonStartReportsAnUnreadableRecordAsPidUnknown(t *testing.T) {
	home := startHome(t)
	d := newHealthDaemon(t, true, "")
	writeStateWithPort(t, home, d.port)
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(daemonlock.Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	startNoSpawn(t)

	var out bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0: a healthy daemon holds the lock", code)
	}
	if !strings.Contains(out.String(), "already running (pid unknown)") {
		t.Errorf("stdout = %q, want \"already running (pid unknown)\"", out.String())
	}
}

// F138: nothing holds the lock, but a chottag daemon answers on the port.
// Spawning would start a daemon that fails to bind, and the health poll
// would then take the OTHER daemon's answer for success.
func TestDaemonStartRefusesAForeignDaemonOnThePort(t *testing.T) {
	home := startHome(t)
	d := newHealthDaemon(t, true, "")
	writeStateWithPort(t, home, d.port)
	startNoSpawn(t)

	var errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), strconv.Itoa(d.port)) || !strings.Contains(errb.String(), daemonlock.Path(home)) {
		t.Errorf("stderr = %q, want it to name port %d and %s", errb.String(), d.port, daemonlock.Path(home))
	}
}

// The spawn stub plays the daemon: it takes the lock as pid 5151 and starts
// answering health. The upstream comes from this process's own
// HTTPS_PROXY, exactly as the shim resolves it.
func TestDaemonStartSpawnsHandsOverTheUpstreamAndReportsThePid(t *testing.T) {
	home := startHome(t)
	t.Setenv("HTTPS_PROXY", "http://corp:8080")
	d := newHealthDaemon(t, false, "")
	writeStateWithPort(t, home, d.port)
	var calls int
	var gotHome, gotUpstream string
	stubSpawn(t, func(exe, h, upstream string) error {
		calls++
		gotHome, gotUpstream = h, upstream
		holdLock(t, home, fakeRecord(5151))
		d.up.Store(true)
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if calls != 1 || gotHome != home || gotUpstream != "http://corp:8080" {
		t.Errorf("spawn calls=%d home=%q upstream=%q; want 1, %q, and this shell's HTTPS_PROXY", calls, gotHome, gotUpstream, home)
	}
	if !strings.Contains(out.String(), "started the daemon (pid 5151)") {
		t.Errorf("stdout = %q, want \"started the daemon (pid 5151)\"", out.String())
	}
}

// Run from inside a chottag session, HTTPS_PROXY is chottag's own address.
// That is not an upstream, and handing it over would make the new daemon
// dial itself.
func TestDaemonStartNeverHandsTheDaemonItsOwnAddressAsUpstream(t *testing.T) {
	home := startHome(t)
	d := newHealthDaemon(t, false, "")
	writeStateWithPort(t, home, d.port)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:"+strconv.Itoa(d.port))
	gotUpstream := "unset"
	stubSpawn(t, func(exe, h, upstream string) error {
		gotUpstream = upstream
		holdLock(t, home, fakeRecord(5151))
		d.up.Store(true)
		return nil
	})

	if code := runDaemonStart(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if gotUpstream != "" {
		t.Errorf("spawn upstream = %q, want \"\"", gotUpstream)
	}
}

// A spawned daemon's real stderr is /dev/null (spawnDaemon sets none), so
// the only place its startup failure lands is daemon.log. That is what the
// message must name. This takes the shim's 2s poll budget.
func TestDaemonStartFailsNamingDaemonLogWhenTheDaemonNeverAnswers(t *testing.T) {
	home := startHome(t)
	writeStateWithPort(t, home, closedPort(t))
	stubSpawn(t, func(string, string, string) error { return nil })

	var errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), filepath.Join(home, "daemon.log")) {
		t.Errorf("stderr = %q, want it to name %s", errb.String(), filepath.Join(home, "daemon.log"))
	}
}

func TestDaemonStartReportsASpawnFailure(t *testing.T) {
	home := startHome(t)
	writeStateWithPort(t, home, closedPort(t))
	stubSpawn(t, func(string, string, string) error { return errors.New("test: spawn refused") })

	var errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "test: spawn refused") {
		t.Errorf("stderr = %q, want the spawn error", errb.String())
	}
}

// Something became healthy on the port, but it is not a daemon holding this
// home's lock: our own spawn failed (a bind race, for one), and another
// daemon is answering. "started" would be false.
func TestDaemonStartRefusesWhenTheAnsweringDaemonHoldsNoLock(t *testing.T) {
	home := startHome(t)
	d := newHealthDaemon(t, false, "")
	writeStateWithPort(t, home, d.port)
	stubSpawn(t, func(string, string, string) error {
		d.up.Store(true) // answers, but takes no lock
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonStart(nil, newReporter(false, &out, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q", code, out.String())
	}
	if !strings.Contains(errb.String(), daemonlock.Path(home)) {
		t.Errorf("stderr = %q, want it to name %s", errb.String(), daemonlock.Path(home))
	}
}

func TestDaemonStartRejectsAnArgument(t *testing.T) {
	startHome(t)
	if code := runDaemonStart([]string{"now"}, newReporter(false, io.Discard, io.Discard)); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}
