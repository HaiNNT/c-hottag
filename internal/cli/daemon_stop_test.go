package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/session"
)

// stopHome is a home whose state.json names a closed port, so the
// foreign-daemon probe never dials 47821 or a developer's real daemon.
func stopHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, closedPort(t))
	return home
}

// shortenStopTimings swaps stop's grace periods for the test and restores
// the values it replaced.
func shortenStopTimings(t *testing.T, grace, kill time.Duration) {
	t.Helper()
	oldPoll, oldGrace, oldKill := stopPoll, stopGrace, killGrace
	stopPoll, stopGrace, killGrace = 10*time.Millisecond, grace, kill
	t.Cleanup(func() { stopPoll, stopGrace, killGrace = oldPoll, oldGrace, oldKill })
}

func TestDaemonStopReportsNotRunning(t *testing.T) {
	stopHome(t)
	sigs := stubSignals(t, nil)
	var out, errb bytes.Buffer
	if code := runDaemonCmd([]string{"stop"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "not running") {
		t.Errorf("stdout = %q, want \"not running\"", out.String())
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none", got)
	}
}

// F138: a chottag daemon answering on the port while nothing holds the lock
// (a pre-M1d-c daemon after an upgrade, or another CHOTTAG_HOME's daemon)
// is not "not running".
func TestDaemonStopRefusesWhenAChottagDaemonAnswersWithoutTheLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	d := newHealthDaemon(t, true, "")
	writeStateWithPort(t, home, d.port)
	sigs := stubSignals(t, nil)

	var out, errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, &out, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), strconv.Itoa(d.port)) || !strings.Contains(errb.String(), daemonlock.Path(home)) {
		t.Errorf("stderr = %q, want it to name port %d and %s", errb.String(), d.port, daemonlock.Path(home))
	}
	if strings.Contains(out.String(), "not running") {
		t.Errorf("stdout = %q: a daemon is serving, so it must not say \"not running\"", out.String())
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none", got)
	}
}

func TestDaemonStopRefusesASupervisedDaemon(t *testing.T) {
	for _, c := range []struct {
		name, supervisor, label, want string
	}{
		{"launchd", "launchd", "com.chottag.daemon", fmt.Sprintf("launchctl bootout gui/%d/com.chottag.daemon", os.Getuid())},
		{"systemd", "systemd", "", "systemctl --user stop chottag.service"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := stopHome(t)
			rec := fakeRecord(4242)
			rec.Supervisor, rec.Label = c.supervisor, c.label
			holdLock(t, home, rec)
			sigs := stubSignals(t, nil)

			var out, errb bytes.Buffer
			if code := runDaemonStop(nil, newReporter(false, &out, &errb)); code != 3 {
				t.Fatalf("exit = %d, want 3 (user action); stderr=%q", code, errb.String())
			}
			if !strings.Contains(errb.String(), c.want) {
				t.Errorf("stderr = %q, want the supervisor's own command %q", errb.String(), c.want)
			}
			if got := sigs.list(); len(got) != 0 {
				t.Errorf("signals sent = %v, want none: the supervisor would restart it", got)
			}
		})
	}
}

func TestDaemonStopRefusesWhileClaudeSessionsAreLive(t *testing.T) {
	home := stopHome(t)
	holdLock(t, home, fakeRecord(4242))
	idle := startHelperProcess(t, home, "idle") // a live pid this test started itself
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(idle.pid(), 1); err != nil {
		t.Fatal(err)
	}
	sigs := stubSignals(t, nil)

	var out, errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, &out, &errb)); code != 3 {
		t.Fatalf("exit = %d, want 3; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), fmt.Sprintf("pid %d", idle.pid())) || !strings.Contains(errb.String(), "--force") {
		t.Errorf("stderr = %q, want it to list pid %d and mention --force", errb.String(), idle.pid())
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none", got)
	}
}

func TestDaemonStopForceOverridesTheSupervisorAndLiveSessions(t *testing.T) {
	home := stopHome(t)
	rec := fakeRecord(4242)
	rec.Supervisor, rec.Label = "launchd", "com.chottag.daemon"
	release := holdLock(t, home, rec)
	idle := startHelperProcess(t, home, "idle")
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(idle.pid(), 1); err != nil {
		t.Fatal(err)
	}
	sigs := stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			release()
		}
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonStop([]string{"--force"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want %v", sigs.list(), want)
	}
	// --force kills it past the supervisor check, but launchd's own
	// KeepAlive restarts it immediately: stop must not claim this was
	// temporary until the next `claude` launch (item 5, fix round 1).
	if strings.Contains(out.String(), "next `claude` launch") {
		t.Errorf("stdout = %q, must not claim this is temporary: launchd restarts it right away", out.String())
	}
	// The result line ("stopped…") stays on stdout; the supervisor note
	// itself is on stderr, unified with restart's own note (fix round 5).
	want := "launchctl bootout gui/" + strconv.Itoa(os.Getuid()) + "/com.chottag.daemon"
	if !strings.Contains(errb.String(), "launchd") || !strings.Contains(errb.String(), want) {
		t.Errorf("stderr = %q, want it to say launchd will relaunch it and name %q", errb.String(), want)
	}
	if strings.Contains(out.String(), "launchd") {
		t.Errorf("stdout = %q, want the supervisor note on stderr, not stdout", out.String())
	}
}

// The daemon releases its lock some time after SIGTERM (its graceful drain).
// stop must wait for that, so that by the time it returns the lock is free.
func TestDaemonStopSendsSIGTERMAndWaitsForTheLockToBeReleased(t *testing.T) {
	home := stopHome(t)
	release := holdLock(t, home, fakeRecord(4242))
	sigs := stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			time.AfterFunc(150*time.Millisecond, release)
		}
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if st, err := daemonlock.Inspect(home); err != nil || st.Running {
		t.Fatalf("Inspect when stop returned = %+v, %v; want not running: stop must wait for the release", st, err)
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want %v", sigs.list(), want)
	}
	for _, s := range []string{"stopped the daemon (pid 4242)", "next `claude` launch"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("stdout = %q, want it to contain %q (spec §5: stop is temporary, and says so)", out.String(), s)
		}
	}
}

func TestDaemonStopEscalatesToSIGKILLAfterTheGracePeriod(t *testing.T) {
	shortenStopTimings(t, 100*time.Millisecond, 2*time.Second)
	home := stopHome(t)
	release := holdLock(t, home, fakeRecord(4242))
	sigs := stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			release()
		}
		return nil
	})

	if code := runDaemonStop(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}, {4242, syscall.SIGKILL}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want %v", sigs.list(), want)
	}
}

func TestDaemonStopFailsWhenTheLockOutlivesSIGKILL(t *testing.T) {
	shortenStopTimings(t, 50*time.Millisecond, 50*time.Millisecond)
	home := stopHome(t)
	holdLock(t, home, fakeRecord(4242))
	sigs := stubSignals(t, nil)

	var errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "still holds") {
		t.Errorf("stderr = %q, want it to say the lock is still held", errb.String())
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}, {4242, syscall.SIGKILL}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want %v", sigs.list(), want)
	}
}

// A `claude` launch can start a new daemon the moment the old one exits.
// The lock then names someone else, and nobody may be signalled on the old
// record's behalf: neither the old pid (gone, possibly reused) nor the new
// holder.
func TestDaemonStopNeverSignalsAReplacedHolder(t *testing.T) {
	shortenStopTimings(t, 100*time.Millisecond, 100*time.Millisecond)
	home := stopHome(t)
	release := holdLock(t, home, fakeRecord(4242))
	replacement := fakeRecord(5151)
	sigs := stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			release()
			holdLock(t, home, replacement)
		}
		return nil
	})

	var out bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0: the daemon stop targeted is gone", code)
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want only %v", sigs.list(), want)
	}
	if !strings.Contains(out.String(), "pid 5151") {
		t.Errorf("stdout = %q, want a note that a new daemon (pid 5151) has started", out.String())
	}
	if st, err := daemonlock.Inspect(home); err != nil || !st.Record.Same(replacement) {
		t.Errorf("Inspect = %+v, %v; the replacement must be untouched", st, err)
	}
}

// The daemon may exit on its own between stop's inspection and its signal.
// terminate re-checks the lock first and signals nothing — and, since it
// never actually stopped anything, says so plainly (item 7, fix round 1)
// rather than claiming credit for a stop it didn't perform.
func TestTerminateDoesNotSignalADaemonThatAlreadyExited(t *testing.T) {
	home := stopHome(t) // nothing holds the lock
	sigs := stubSignals(t, nil)
	var out bytes.Buffer
	if _, fail := terminate(home, fakeRecord(4242), newReporter(false, &out, io.Discard)); fail != nil {
		t.Fatalf("terminate failed: %+v, want success", fail)
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none: the holder had already gone", got)
	}
	if !strings.Contains(out.String(), "had already exited") || strings.Contains(out.String(), "stopped") {
		t.Errorf("stdout = %q, want \"had already exited\", not a claim of having stopped it", out.String())
	}
}

// The lock can already name a different daemon by the time terminate
// re-checks it: the one stop inspected exited, and a `claude` launch
// started another. The old pid may have been reused, and the new holder is
// not stop's target, so nothing is sent at all.
func TestTerminateNeverSignalsWhenTheLockNamesAnotherDaemon(t *testing.T) {
	home := stopHome(t)
	holdLock(t, home, fakeRecord(5151))
	sigs := stubSignals(t, nil)
	var out bytes.Buffer
	if _, fail := terminate(home, fakeRecord(4242), newReporter(false, &out, io.Discard)); fail != nil {
		t.Fatalf("terminate failed: %+v, want success: the daemon it was asked to stop is gone", fail)
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none", got)
	}
	if !strings.Contains(out.String(), "pid 5151") {
		t.Errorf("stdout = %q, want a note naming the new daemon (pid 5151)", out.String())
	}
}

// The process can vanish between the re-check and the kill(2), in which
// case kill reports ESRCH. That is a successful stop, not an error.
func TestDaemonStopTreatsAVanishedProcessAsStopped(t *testing.T) {
	home := stopHome(t)
	release := holdLock(t, home, fakeRecord(4242))
	stubSignals(t, func(pid int, sig syscall.Signal) error {
		release()
		return syscall.ESRCH
	})
	if code := runDaemonStop(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestDaemonStopRefusesAnUnreadableRecord(t *testing.T) {
	home := stopHome(t)
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(daemonlock.Path(home)) // held, with an empty record
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	sigs := stubSignals(t, nil)

	var errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "pid is unknown") {
		t.Errorf("stderr = %q, want it to say the pid is unknown", errb.String())
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none", got)
	}
}

// I1 (fix round 1): the lock can go from a readable record to an
// unreadable one mid-escalation — SIGTERM releases it, and something else
// (an unrelated writer, or a torn write) takes it with no record. Neither
// mutation the reviewer found may survive: signalling rec.PID once its
// record can no longer be read (M2), or reporting that as a successful
// stop (M2').
func TestDaemonStopStopsEscalatingWhenTheRecordBecomesUnreadable(t *testing.T) {
	shortenStopTimings(t, 100*time.Millisecond, 100*time.Millisecond)
	home := stopHome(t)
	release := holdLock(t, home, fakeRecord(4242))
	sigs := stubSignals(t, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			release()
			unlock, ok, err := fsutil.TryLock(daemonlock.Path(home)) // held, with an empty record
			if err != nil || !ok {
				t.Fatalf("TryLock = %v, %v", ok, err)
			}
			t.Cleanup(func() { unlock() })
		}
		return nil
	})

	var out, errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, &out, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if want := []sentSignal{{4242, syscall.SIGTERM}}; !reflect.DeepEqual(sigs.list(), want) {
		t.Errorf("signals = %v, want only %v: an unreadable record's pid is unknown, so SIGKILL must never be sent (M2)", sigs.list(), want)
	}
	if strings.Contains(out.String(), "stopped") {
		t.Errorf("stdout = %q, must not claim a stop: the record is unreadable, not confirmed released (M2')", out.String())
	}
	if !strings.Contains(errb.String(), "pid is unknown") {
		t.Errorf("stderr = %q, want unreadableRecordMessage (\"pid is unknown\"), not the generic error text (item 6)", errb.String())
	}
}

// item 3 (fix round 1): defense in depth against kill(0)/kill(-1). rec
// always comes from a just-Inspect'd record in production, never a 0 or
// negative pid, but terminate refuses one outright rather than trusting
// that invariant implicitly.
func TestTerminateRefusesToSignalPIDZero(t *testing.T) {
	home := stopHome(t)
	sigs := stubSignals(t, nil)
	var errb bytes.Buffer
	if _, fail := terminate(home, daemonlock.Record{}, newReporter(false, io.Discard, &errb)); fail == nil || fail.exit != 1 || fail.code != codeInternal {
		t.Fatalf("terminate failure = %+v, want exit 1, code internal", fail)
	}
	if got := sigs.list(); len(got) != 0 {
		t.Errorf("signals sent = %v, want none", got)
	}
}

func TestDaemonStopRejectsAPositionalArgument(t *testing.T) {
	stopHome(t)
	stubSignals(t, nil)
	if code := runDaemonStop([]string{"now"}, newReporter(false, io.Discard, io.Discard)); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// End to end against a real process: the helper holds the lock as a daemon
// does and exits on SIGTERM. The signal seam lets a real kill(2) through
// ONLY to that helper's pid.
func TestDaemonStopTerminatesARealHelperProcess(t *testing.T) {
	home := stopHome(t)
	helper := startHelperProcess(t, home, "hold")
	t.Cleanup(SetSignalForTest(func(pid int, sig syscall.Signal) error {
		select {
		case <-helper.done:
			t.Errorf("stop tried to signal pid %d after this test's helper had already been reaped", pid)
			return errors.New("test: refusing to signal an already-reaped process")
		default:
		}
		if pid != helper.pid() {
			t.Errorf("stop tried to signal pid %d; only this test's helper (pid %d) may be signalled", pid, helper.pid())
			return errors.New("test: refusing to signal a process this test did not start")
		}
		return syscall.Kill(pid, sig)
	}))

	var out, errb bytes.Buffer
	if code := runDaemonStop(nil, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if err := helper.wait(t); err != nil {
		t.Errorf("helper exited with %v, want a clean exit on SIGTERM", err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("stopped the daemon (pid %d)", helper.pid())) {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestDaemonStopKillsARealHelperThatIgnoresSIGTERM(t *testing.T) {
	shortenStopTimings(t, 300*time.Millisecond, 5*time.Second)
	home := stopHome(t)
	helper := startHelperProcess(t, home, "hold-ignore-term")
	t.Cleanup(SetSignalForTest(func(pid int, sig syscall.Signal) error {
		select {
		case <-helper.done:
			t.Errorf("stop tried to signal pid %d after this test's helper had already been reaped", pid)
			return errors.New("test: refusing to signal an already-reaped process")
		default:
		}
		if pid != helper.pid() {
			t.Errorf("stop tried to signal pid %d; only this test's helper (pid %d) may be signalled", pid, helper.pid())
			return errors.New("test: refusing to signal a process this test did not start")
		}
		return syscall.Kill(pid, sig)
	}))

	if code := runDaemonStop(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	helper.wait(t)
	ws, ok := helper.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("helper state = %v, want killed by SIGKILL", helper.cmd.ProcessState)
	}
}
