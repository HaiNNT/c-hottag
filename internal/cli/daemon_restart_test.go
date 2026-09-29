package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/session"
)

// restartHome is a home whose state.json names port (a test port, never
// 47821).
func restartHome(t *testing.T, port int) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, port)
	return home
}

func TestDaemonRestartRefusesASupervisedDaemon(t *testing.T) {
	for _, c := range []struct {
		name, supervisor, label, want string
	}{
		{"launchd", "launchd", "com.chottag.daemon", fmt.Sprintf("launchctl kickstart -k gui/%d/com.chottag.daemon", os.Getuid())},
		{"systemd", "systemd", "", "systemctl --user restart chottag.service"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := restartHome(t, closedPort(t))
			rec := fakeRecord(4242)
			rec.Supervisor, rec.Label = c.supervisor, c.label
			holdLock(t, home, rec)
			sigs := stubSignals(t, nil)
			stubSpawn(t, func(string, string, string) error { t.Error("spawned under a supervisor"); return nil })

			var errb bytes.Buffer
			if code := runDaemonCmd([]string{"restart"}, newReporter(false, io.Discard, &errb)); code != 3 {
				t.Fatalf("exit = %d, want 3; stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("stderr = %q, want %q", errb.String(), c.want)
			}
			if got := sigs.list(); len(got) != 0 {
				t.Errorf("signals = %v, want none", got)
			}
		})
	}
}

// restartReplaceable wires a stand-in daemon that restart can replace. The old one
// holds the lock as pid 4242 and answers health. On SIGTERM it releases the
// lock and stops answering. The spawn stub then plays the new daemon (lock
// as pid 5151, health back up) and records the upstream it was handed.
func restartReplaceable(t *testing.T, home string, d *healthDaemon) (sigs *signalLog, upstream *string) {
	t.Helper()
	release := holdLock(t, home, fakeRecord(4242))
	sigs = stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			d.up.Store(false)
			release()
		}
		return nil
	})
	got := "unset"
	stubSpawn(t, func(exe, h, up string) error {
		got = up
		holdLock(t, home, fakeRecord(5151))
		d.up.Store(true)
		return nil
	})
	return sigs, &got
}

// relaunchOnWaitNote is a forced restart's stderr, and the stand-in for a
// supervisor's own relaunch (F140). runDaemonRestart writes the
// supervisor-relaunch note ("waiting for <supervisor> to relaunch ...")
// only after terminate has seen the stopped record release the lock, and
// just before waitForSupervisorRelaunch starts polling. That write is the
// event this takes the lock as rec on, synchronously, on the caller's own
// goroutine (runDaemonRestart writes on it), so t is safe to use.
//
// F229: this used to be a 50-100 ms time.AfterFunc. The stop path signals
// 41-52 ms after it starts (two Inspects, each sleeping acquireRetry), so
// the timer and the signal raced: a same-record acquire that landed
// between the release and waitForRelease's first look read as "the old
// daemon still holds the lock", and terminate waited out stopGrace and
// SIGKILL (12 s) and failed with stop_timeout.
type relaunchOnWaitNote struct {
	t    *testing.T
	home string
	rec  daemonlock.Record
	up   *atomic.Bool // set true right after the acquire; nil leaves health alone
	buf  bytes.Buffer
	ran  bool
	err  error
}

func newRelaunchOnWaitNote(t *testing.T, home string, rec daemonlock.Record, up *atomic.Bool) *relaunchOnWaitNote {
	return &relaunchOnWaitNote{t: t, home: home, rec: rec, up: up}
}

func (w *relaunchOnWaitNote) Write(p []byte) (int, error) {
	n, _ := w.buf.Write(p)
	if !w.ran && strings.Contains(w.buf.String(), "to relaunch its own daemon") {
		w.ran = true
		release, err := daemonlock.Acquire(w.home, w.rec)
		if err != nil {
			w.err = err
			return n, nil
		}
		var once sync.Once
		w.t.Cleanup(func() { once.Do(func() { release() }) })
		if w.up != nil {
			w.up.Store(true)
		}
	}
	return n, nil
}

func (w *relaunchOnWaitNote) String() string { return w.buf.String() }

// check fails the test unless the relaunch ran and took the lock.
func (w *relaunchOnWaitNote) check() {
	w.t.Helper()
	if w.err != nil {
		w.t.Fatalf("relaunch: %v", w.err)
	}
	if !w.ran {
		w.t.Fatalf("the supervisor-relaunch note was never written, so the relaunch never ran; stderr=%q", w.buf.String())
	}
}

// TestDaemonRestartForceWaitsForSupervisorRelaunchThenReportsIt pins F140:
// spawning here, the way an unsupervised restart does, would itself take
// the lock and beat the supervisor's own relaunch to it — the unit's
// relaunch then finds "already running" and gives up, leaving the daemon
// this command started running unsupervised. Instead restart must wait for
// the supervisor's OWN relaunch. That is simulated here by
// relaunchOnWaitNote once restart starts waiting, with nothing routed
// through the spawn seam at all: it is never stubbed in this test, so
// TestMain's panicking default is what would fail it if restart reached it.
// restartSupervisorWait is only a hang guard here: success returns as soon
// as the relaunch is seen.
func TestDaemonRestartForceWaitsForSupervisorRelaunchThenReportsIt(t *testing.T) {
	old := restartSupervisorWait
	restartSupervisorWait = 10 * time.Second
	t.Cleanup(func() { restartSupervisorWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	rec := fakeRecord(4242)
	rec.Supervisor = "systemd"
	release := holdLock(t, home, rec)
	errb := newRelaunchOnWaitNote(t, home, fakeRecord(9999), &d.up)
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		d.up.Store(false)
		release()
		return nil
	})

	var out bytes.Buffer
	code := runDaemonRestart([]string{"--force"}, newReporter(false, &out, errb))
	errb.check()
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "the supervisor relaunched the daemon (pid 9999)") {
		t.Errorf("stdout = %q, want it to report the supervisor's relaunch and pid", out.String())
	}
	if !strings.Contains(errb.String(), "waiting for") || !strings.Contains(errb.String(), "systemd") {
		t.Errorf("stderr = %q, want a note that restart is waiting for systemd to relaunch its own daemon", errb.String())
	}
}

// TestDaemonRestartTreatsAReusedPidWithADifferentStartedAsANewHolder pins
// the review residual in F140's new-holder check: comparing pid alone would
// wrongly call the OS reusing the stopped daemon's own pid number for the
// relaunch "still the old daemon", and wait out the whole budget. Comparing
// the full record (daemonlock.Record.Same: pid AND Started) recognizes it
// as a genuinely new holder instead.
func TestDaemonRestartTreatsAReusedPidWithADifferentStartedAsANewHolder(t *testing.T) {
	old := restartSupervisorWait
	restartSupervisorWait = 10 * time.Second // a hang guard: success returns at once
	t.Cleanup(func() { restartSupervisorWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	rec := fakeRecord(4242)
	rec.Supervisor = "systemd"
	release := holdLock(t, home, rec)
	relaunched := rec
	relaunched.Started = rec.Started.Add(time.Second) // same pid, different Started
	errb := newRelaunchOnWaitNote(t, home, relaunched, &d.up)
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		d.up.Store(false)
		release()
		return nil
	})

	var out bytes.Buffer
	code := runDaemonRestart([]string{"--force"}, newReporter(false, &out, errb))
	errb.check()
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "the supervisor relaunched the daemon (pid 4242)") {
		t.Errorf("stdout = %q, want it to report pid 4242 even though that pid number was reused", out.String())
	}
}

// TestDaemonRestartDoesNotTreatTheStoppedRecordItselfAsARelaunch is the
// other side of the Same() fix above: the exact record just stopped
// reappearing (same pid AND Started) is not a relaunch at all, so the wait
// must time out rather than declare success on stale or misread state.
// The reappearance happens once restart is already waiting
// (relaunchOnWaitNote), so it is in place for the whole wait, however
// slowly the machine runs: the timeout is this test's expected outcome,
// not a bound it races (F229).
func TestDaemonRestartDoesNotTreatTheStoppedRecordItselfAsARelaunch(t *testing.T) {
	old := restartSupervisorWait
	restartSupervisorWait = 300 * time.Millisecond
	t.Cleanup(func() { restartSupervisorWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	rec := fakeRecord(4242)
	rec.Supervisor = "systemd"
	release := holdLock(t, home, rec)
	errb := newRelaunchOnWaitNote(t, home, rec, &d.up)
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		d.up.Store(false)
		release()
		return nil
	})

	if code := runDaemonRestart([]string{"--force"}, newReporter(false, io.Discard, errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	errb.check()
	if !strings.Contains(errb.String(), "systemctl --user restart chottag.service") {
		t.Errorf("stderr = %q, want the supervisor's own restart command", errb.String())
	}
}

// TestDaemonRestartForceExitsWhenTheSupervisorDoesNotRelaunch is F140's
// timeout side: nothing ever takes the lock back over, so restart must give
// up rather than wait forever, and it must still never spawn.
func TestDaemonRestartForceExitsWhenTheSupervisorDoesNotRelaunch(t *testing.T) {
	old := restartSupervisorWait
	restartSupervisorWait = 100 * time.Millisecond
	t.Cleanup(func() { restartSupervisorWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	rec := fakeRecord(4242)
	rec.Supervisor = "systemd"
	release := holdLock(t, home, rec)
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		d.up.Store(false)
		release()
		return nil
	})
	// No stubSpawn: TestMain's panicking default is what would fail this
	// test if restart ever reached the spawn seam here.

	var errb bytes.Buffer
	if code := runDaemonRestart([]string{"--force"}, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "systemctl --user restart chottag.service") {
		t.Errorf("stderr = %q, want the supervisor's own restart command", errb.String())
	}
}

// TestDaemonRestartForceOnAnUnsupervisedDaemonStillSpawns pins that F140's
// fix is scoped to a supervised daemon: --force with no supervisor at all
// takes the ordinary stop-then-spawn path, unchanged.
func TestDaemonRestartForceOnAnUnsupervisedDaemonStillSpawns(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	sigs, _ := restartReplaceable(t, home, d)

	var out bytes.Buffer
	if code := runDaemonRestart([]string{"--force"}, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "started the daemon (pid 5151)") {
		t.Errorf("stdout = %q, want restart to spawn as usual when unsupervised", out.String())
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want %v", sigs.list(), want)
	}
}

func TestDaemonRestartStopsThenStartsAndReportsTheNewPid(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	sigs, _ := restartReplaceable(t, home, d)

	var out, errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	stopped := strings.Index(out.String(), "stopped the daemon (pid 4242)")
	started := strings.Index(out.String(), "started the daemon (pid 5151)")
	if stopped < 0 || started < 0 || stopped > started {
		t.Errorf("stdout = %q, want \"stopped … 4242\" and then \"started … 5151\"", out.String())
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want %v", sigs.list(), want)
	}
}

// Unlike stop, live sessions do not refuse a restart: they only see a brief
// interruption, and restart says so.
func TestDaemonRestartDoesNotRefuseLiveSessions(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	restartReplaceable(t, home, d)
	idle := startHelperProcess(t, home, "idle")
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(idle.pid(), d.port); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "1 live claude session(s) will see a brief interruption") {
		t.Errorf("stdout = %q, want the interruption notice", out.String())
	}
}

// A registry restart cannot read is informational here, not a reason to
// refuse: unlike `stop`, nothing asks the operator for permission on the
// strength of the live-session count, so restart warns and continues
// rather than aborting.
func TestDaemonRestartWarnsButContinuesWhenTheSessionRegistryIsUnreadable(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	restartReplaceable(t, home, d) // creates home/run as a directory first
	runDir := filepath.Join(home, "run")
	if err := os.Chmod(runDir, 0o300); err != nil { // -wx: os.ReadDir fails, a known path still opens
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(runDir, 0o700) }) // restore before t.TempDir()'s own cleanup removes it

	var out, errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "could not read the session registry") {
		t.Errorf("stderr = %q, want a warning about the unreadable session registry", errb.String())
	}
	if !strings.Contains(out.String(), "started the daemon (pid 5151)") {
		t.Errorf("stdout = %q, want restart to continue despite the warning", out.String())
	}
}

func TestDaemonRestartDoesNotStartWhenTheStopFails(t *testing.T) {
	shortenStopTimings(t, 50*time.Millisecond, 50*time.Millisecond)
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	holdLock(t, home, fakeRecord(4242)) // never released: survives SIGKILL
	stubSignals(t, nil)
	stubSpawn(t, func(string, string, string) error {
		t.Error("spawned although the old daemon never stopped")
		return nil
	})

	if code := runDaemonRestart(nil, newReporter(false, io.Discard, io.Discard)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestDaemonRestartOfAStoppedDaemonJustStarts(t *testing.T) {
	d := newHealthDaemon(t, false, "")
	home := restartHome(t, d.port)
	sigs := stubSignals(t, nil)
	stubSpawn(t, func(string, string, string) error {
		holdLock(t, home, fakeRecord(5151))
		d.up.Store(true)
		return nil
	})

	var out bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "started the daemon (pid 5151)") {
		t.Errorf("stdout = %q", out.String())
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals = %v, want none", got)
	}
}

// The lock is free a moment before the old listener has gone: the kernel
// closes a dying process's descriptors in no promised order. restart must
// wait for the port to stop answering before it spawns. Otherwise
// ensureStarted sees a lock-less daemon answering (F138) and refuses.
func TestDaemonRestartWaitsForTheOldListenerToClose(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	release := holdLock(t, home, fakeRecord(4242))
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		release()                                                          // the lock goes first...
		time.AfterFunc(150*time.Millisecond, func() { d.up.Store(false) }) // ...the listener a moment later
		return nil
	})
	stubSpawn(t, func(string, string, string) error {
		holdLock(t, home, fakeRecord(5151))
		d.up.Store(true)
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "started the daemon (pid 5151)") {
		t.Errorf("stdout = %q", out.String())
	}
}

// The health probe alone cannot tell "the old listener is still up" apart
// from "a `claude` launch already raced in and started the next daemon":
// both answer. waitForPortToClose's lock check is what tells them apart, so
// it must end the wait the instant a new holder takes the lock, rather than
// waiting out the full budget just because health never goes down.
func TestDaemonRestartStopsWaitingAsSoonAsANewDaemonTakesTheLock(t *testing.T) {
	old := restartPortWait
	restartPortWait = 500 * time.Millisecond
	t.Cleanup(func() { restartPortWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	release := holdLock(t, home, fakeRecord(4242))
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		release()
		holdLock(t, home, fakeRecord(5151)) // a `claude` launch already raced in
		return nil
	})
	spawned := false
	stubSpawn(t, func(string, string, string) error { spawned = true; return nil })

	start := time.Now()
	var out bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if elapsed := time.Since(start); elapsed >= restartPortWait {
		t.Errorf("elapsed = %v, want well under restartPortWait (%v): a new holder's lock should end the wait immediately", elapsed, restartPortWait)
	}
	if spawned {
		t.Error("spawned although a new daemon already held the lock")
	}
	if !strings.Contains(out.String(), "daemon already running (pid 5151)") {
		t.Errorf("stdout = %q, want ensureStarted to report the already-running new daemon", out.String())
	}
}

// If the old port keeps answering past restartPortWait, whatever answers
// is not a daemon of this home (F138): restart refuses, and never spawns
// into a port it cannot bind.
func TestDaemonRestartDoesNotSpawnWhileTheOldListenerStillAnswers(t *testing.T) {
	old := restartPortWait
	restartPortWait = 100 * time.Millisecond
	t.Cleanup(func() { restartPortWait = old })
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	release := holdLock(t, home, fakeRecord(4242))
	stubSignals(t, func(int, syscall.Signal) error { release(); return nil }) // health never goes down
	stubSpawn(t, func(string, string, string) error { t.Error("spawned while the old port still answers"); return nil })

	var errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), daemonlock.Path(home)) {
		t.Errorf("stderr = %q, want the F138 message naming %s", errb.String(), daemonlock.Path(home))
	}
}

// The new daemon's upstream comes from THIS shell (spec §5). When that
// differs from what the old daemon reported, most commonly because
// restart was run inside a chottag session whose HTTPS_PROXY is chottag
// itself, the operator is told, and no credential is ever shown — not a
// password (redact.UpstreamProxy's usual job) and not a bare, username-only
// token either (a real shape for a bearer credential, and the one
// redact.UpstreamProxy alone would miss: it only ever masks a URL's
// password).
func TestDaemonRestartNotesAnUpstreamChange(t *testing.T) {
	for _, c := range []struct {
		name, newUpstream, wantNew, leak string
	}{
		{"password credential", "http://bob:hunter2@corp-b:8080", "corp-b:8080", "hunter2"},
		{"username-only credential", "http://sk-SECRETTOKEN@corp-b:8080", "corp-b:8080", "sk-SECRETTOKEN"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newHealthDaemon(t, true, "http://corp-a:8080")
			home := restartHome(t, d.port)
			_, upstream := restartReplaceable(t, home, d)
			t.Setenv("HTTPS_PROXY", c.newUpstream)

			var errb bytes.Buffer
			if code := runDaemonRestart(nil, newReporter(false, io.Discard, &errb)); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
			}
			for _, s := range []string{"http://corp-a:8080", c.wantNew} {
				if !strings.Contains(errb.String(), s) {
					t.Errorf("stderr = %q, want the note to name %q", errb.String(), s)
				}
			}
			if strings.Contains(errb.String(), c.leak) {
				t.Errorf("stderr = %q leaks the proxy credential %q", errb.String(), c.leak)
			}
			if *upstream != c.newUpstream {
				t.Errorf("spawn upstream = %q, want this shell's HTTPS_PROXY verbatim (it travels by env, never argv)", *upstream)
			}
		})
	}
}

// A scheme-less upstream ("bob:hunter2@proxy:8080", the shape http_proxy
// env vars and `curl -x` accept) is where net/url reads the first token as
// a SCHEME rather than userinfo, so redact.WithoutUserinfo reduces it to ""
// — indistinguishable, by naive comparison, from "no upstream configured"
// when the old daemon had none either. Both are "", so a bare
// `reduced == health.Upstream` check would wrongly call that "no change"
// and stay silent about a credential that is about to travel to a new
// daemon. The note must still fire, falling back to
// redact.UpstreamProxy's "<redacted>" rather than nothing.
func TestDaemonRestartNotesAnUnparsableUpstreamChange(t *testing.T) {
	d := newHealthDaemon(t, true, "") // the old daemon has no upstream
	home := restartHome(t, d.port)
	restartReplaceable(t, home, d)
	t.Setenv("HTTPS_PROXY", "bob:hunter2@proxy:8080") // scheme-less: opaque to net/url

	var errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, io.Discard, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "<redacted>") {
		t.Errorf("stderr = %q, want the note to fall back to <redacted> rather than staying silent", errb.String())
	}
	if strings.Contains(errb.String(), "hunter2") {
		t.Errorf("stderr = %q leaks the proxy password", errb.String())
	}
}

// The upstream-change note is probed while the old daemon can still answer
// (before the stop), but a failed stop must not claim an upstream change
// that never actually happens: nothing was restarted.
func TestDaemonRestartDoesNotNoteAnUpstreamChangeWhenTheStopFails(t *testing.T) {
	shortenStopTimings(t, 50*time.Millisecond, 50*time.Millisecond)
	d := newHealthDaemon(t, true, "http://corp-a:8080")
	home := restartHome(t, d.port)
	holdLock(t, home, fakeRecord(4242)) // never released: survives SIGKILL
	stubSignals(t, nil)
	t.Setenv("HTTPS_PROXY", "http://corp-b:8080")

	var errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "upstream") {
		t.Errorf("stderr = %q, want no upstream note: the stop failed", errb.String())
	}
}

func TestDaemonRestartIsQuietWhenTheUpstreamIsUnchanged(t *testing.T) {
	d := newHealthDaemon(t, true, "http://corp-a:8080")
	home := restartHome(t, d.port)
	restartReplaceable(t, home, d)
	t.Setenv("HTTPS_PROXY", "http://corp-a:8080")

	var errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, io.Discard, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(errb.String(), "upstream") {
		t.Errorf("stderr = %q, want no upstream note", errb.String())
	}
}

// TestDaemonRestartForceOnASupervisedDaemonNeverNotesAnUpstreamChange pins
// a review residual: the upstream-change note is meaningless for a
// supervised daemon's --force restart, because the relaunched daemon comes
// from the supervisor's OWN unit environment, not this shell's — so
// comparing this shell's HTTPS_PROXY against it would be reporting a
// "change" that has nothing to do with what actually happens next.
func TestDaemonRestartForceOnASupervisedDaemonNeverNotesAnUpstreamChange(t *testing.T) {
	old := restartSupervisorWait
	restartSupervisorWait = 10 * time.Second // a hang guard: success returns at once
	t.Cleanup(func() { restartSupervisorWait = old })
	d := newHealthDaemon(t, true, "http://corp-a:8080")
	home := restartHome(t, d.port)
	rec := fakeRecord(4242)
	rec.Supervisor = "systemd"
	release := holdLock(t, home, rec)
	errb := newRelaunchOnWaitNote(t, home, fakeRecord(9999), &d.up)
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		d.up.Store(false)
		release()
		return nil
	})
	t.Setenv("HTTPS_PROXY", "http://corp-b:8080")

	code := runDaemonRestart([]string{"--force"}, newReporter(false, io.Discard, errb))
	errb.check()
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "upstream") {
		t.Errorf("stderr = %q, want no upstream-change note for a supervised restart", errb.String())
	}
}

func TestDaemonRestartRefusesAnUnreadableRecord(t *testing.T) {
	home := restartHome(t, closedPort(t))
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(daemonlock.Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	sigs := stubSignals(t, nil)
	stubSpawn(t, func(string, string, string) error { t.Error("spawned"); return nil })

	var errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "pid is unknown") {
		t.Errorf("stderr = %q, want it to say the pid is unknown", errb.String())
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals = %v, want none", got)
	}
}

// End to end against a real process this test started: the helper holds
// the lock and exits on SIGTERM. The seam lets a real kill(2) through only
// to that helper.
func TestDaemonRestartReplacesARealHelperProcess(t *testing.T) {
	d := newHealthDaemon(t, true, "")
	home := restartHome(t, d.port)
	helper := startHelperProcess(t, home, "hold")
	t.Cleanup(SetSignalForTest(func(pid int, sig syscall.Signal) error {
		if pid != helper.pid() {
			t.Errorf("restart tried to signal pid %d; only this test's helper (pid %d) may be signalled", pid, helper.pid())
			return errors.New("test: refusing to signal a process this test did not start")
		}
		d.up.Store(false)
		return syscall.Kill(pid, sig)
	}))
	stubSpawn(t, func(string, string, string) error {
		holdLock(t, home, fakeRecord(5151))
		d.up.Store(true)
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonRestart(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if err := helper.wait(t); err != nil {
		t.Errorf("helper exited with %v, want a clean exit on SIGTERM", err)
	}
	if !strings.Contains(out.String(), "started the daemon (pid 5151)") {
		t.Errorf("stdout = %q", out.String())
	}
}
