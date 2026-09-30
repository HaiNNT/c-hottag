package daemonlock

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/session"
)

// setDuration swaps one of this package's timing vars for the test and
// restores the value it replaced (never a named default, F130).
func setDuration(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func testRecord(pid int) Record {
	return Record{PID: pid, Started: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
}

// TestInspectReportsTheHolderAndItsRecord also pins that the record is
// written IN PLACE: were it written by rename (fsutil.WriteFileAtomic), the
// path would name a new inode while the flock stayed on the old, unlinked
// one, and Inspect would lock the new file and report "not running". The
// record's pid is this test's own: Inspect now also requires the pid to be
// alive (I2c), and a fixed placeholder pid would not be.
func TestInspectReportsTheHolderAndItsRecord(t *testing.T) {
	home := t.TempDir()
	rec := Record{PID: os.Getpid(), Started: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC), Supervisor: "launchd", Label: "com.chottag.daemon"}
	release, err := Acquire(home, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	st, err := Inspect(home)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatal("Inspect reports not running while the lock is held")
	}
	if !st.Record.Same(rec) || st.Record.Supervisor != "launchd" || st.Record.Label != "com.chottag.daemon" {
		t.Fatalf("Inspect record = %+v, want %+v", st.Record, rec)
	}
}

func TestInspectReportsNotRunningOnceReleased(t *testing.T) {
	home := t.TempDir()
	release, err := Acquire(home, testRecord(4242))
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(home)
	if err != nil || st.Running {
		t.Fatalf("Inspect after release = %+v, %v; want not running", st, err)
	}
	again, err := Acquire(home, testRecord(5151))
	if err != nil {
		t.Fatalf("a fresh Acquire after release failed: %v", err)
	}
	again()
}

// Inspect is what `daemon stop` runs on a machine where no daemon ever ran:
// it must answer "not running" without creating run/ or the lock file.
func TestInspectDoesNotCreateAnythingWhenNoDaemonEverRan(t *testing.T) {
	home := t.TempDir()
	st, err := Inspect(home)
	if err != nil || st.Running {
		t.Fatalf("Inspect on an empty home = %+v, %v; want not running and no error", st, err)
	}
	if _, err := os.Stat(filepath.Join(home, "run")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Inspect created %s (stat err = %v)", filepath.Join(home, "run"), err)
	}
}

// Only the lock decides. The stale record names THIS test's own pid, which
// is certainly alive, so an Inspect that trusted the record instead of the
// lock would report a running daemon.
func TestInspectIgnoresAStaleRecordNobodyHolds(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home), b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(home)
	if err != nil || st.Running {
		t.Fatalf("Inspect with a stale record and no holder = %+v, %v; want not running", st, err)
	}
}

// The holder's record names this test's own pid (a real, live one): the
// trailing Inspect call below now also requires a live pid (I2c), and a
// fixed placeholder like 4242 would not be.
func TestAcquireFailsFastWhileAnotherHolderHasTheLock(t *testing.T) {
	setDuration(t, &acquireWait, 100*time.Millisecond)
	home := t.TempDir()
	holderPID := os.Getpid()
	release, err := Acquire(home, testRecord(holderPID))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = Acquire(home, testRecord(5151))
	var are *AlreadyRunningError
	if !errors.As(err, &are) || are.PID != holderPID {
		t.Fatalf("second Acquire err = %v, want *AlreadyRunningError naming pid %d (the holder, not the caller)", err, holderPID)
	}
	if got, want := err.Error(), fmt.Sprintf("daemon already running (pid %d)", holderPID); got != want {
		t.Errorf("err = %q, want %q (spec §4.6's exact message)", got, want)
	}
	// The refused caller must not have touched the holder's record.
	st, err := Inspect(home)
	if err != nil || st.Record.PID != holderPID {
		t.Fatalf("after a refused Acquire, Inspect = %+v, %v; want the holder's pid %d", st, err, holderPID)
	}
}

// Inspect holds the lock for an instant. A real daemon starting in that
// instant must retry rather than fail (spec §4.6: "retrying for ~1s").
func TestAcquireRetriesThroughAMomentaryHold(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	time.AfterFunc(100*time.Millisecond, func() { unlock() })

	release, err := Acquire(home, testRecord(4242))
	if err != nil {
		t.Fatalf("Acquire during a momentary hold = %v, want it to retry past the hold", err)
	}
	release()
}

// The window between a daemon taking the lock and writing its record, or a
// damaged file: the lock is held and the pid is unknown.
func TestInspectReportsAnUnreadableRecordWhileHeld(t *testing.T) {
	setDuration(t, &recordWait, 50*time.Millisecond)
	for _, c := range []struct{ name, content string }{
		{"empty", ""},
		{"corrupt", "{not json"},
		{"no_pid", `{"pid":0,"started":"2026-09-23T10:00:00Z"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(Path(home), []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			unlock, ok, err := fsutil.TryLock(Path(home))
			if err != nil || !ok {
				t.Fatalf("TryLock = %v, %v", ok, err)
			}
			defer unlock()

			st, err := Inspect(home)
			if !st.Running || !errors.Is(err, ErrUnreadableRecord) {
				t.Fatalf("Inspect = %+v, %v; want Running with ErrUnreadableRecord", st, err)
			}
		})
	}
}

// internal/session's Registry lives in the same run/ directory and reads
// every *.json there as a managed claude session. The record names a live
// pid (this test's own), so a lock file named daemon.json would be listed as
// a session, and `stop` would refuse over it.
func TestLockFileIsInvisibleToTheSessionRegistry(t *testing.T) {
	home := t.TempDir()
	release, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := reg.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("session.Live() = %v, want none: the daemon lock must not read as a session", live)
	}
	if _, err := os.Stat(Path(home)); err != nil {
		t.Errorf("session.Live() pruned the daemon lock: %v", err)
	}
}

// --- fix round 1: the lock's lifetime must not depend on the caller,   -----
// --- and a stale or in-flight record must never read as the holder's. -----

// I1: a caller that discards Acquire's release must still keep the lock.
// Without a package-level registry keeping the unlock closure (and the
// *os.File inside it) reachable, the GC could finalize that file once
// nothing else refers to it, closing its fd and dropping the flock out
// from under a live daemon.
func TestALockSurvivesGCWhenTheCallerDropsRelease(t *testing.T) {
	home := t.TempDir()
	_, err := Acquire(home, testRecord(os.Getpid())) // release deliberately discarded
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	st, err := Inspect(home)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.Record.PID != os.Getpid() {
		t.Fatalf("Inspect after discarding release and GC = %+v, %v; want still Running with our pid (the package must keep the lock alive on its own)", st, err)
	}
}

// I2a: a clean release must not leave a stale record for the next holder
// to misread. release truncates the file before it unlocks.
func TestReleaseEmptiesTheRecordBeforeUnlocking(t *testing.T) {
	home := t.TempDir()
	release, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("record after a clean release = %q, want empty", b)
	}
}

// I2c: a crash can leave a stale record behind, naming a pid that is no
// longer alive. Held here by a plain TryLock (nothing ever overwrites the
// file), Inspect must not report that stale pid as the live holder's.
func TestInspectTreatsAStaleRecordWithADeadPIDAsUnreadable(t *testing.T) {
	setDuration(t, &recordWait, 80*time.Millisecond)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	dead := deadPID(t)
	b, err := json.Marshal(testRecord(dead))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home), b, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()

	st, err := Inspect(home)
	if !st.Running || !errors.Is(err, ErrUnreadableRecord) {
		t.Fatalf("Inspect with a dead-pid record while held = %+v, %v; want Running with ErrUnreadableRecord", st, err)
	}
	if st.Record.PID == dead {
		t.Errorf("Inspect reported the dead pid %d as if it were the live holder's", dead)
	}
}

// SetPIDAliveForTest exists for other packages, which hold this package's
// lock in-process with a fabricated pid; F130 requires nested swaps to
// restore the PREVIOUS value, not fall back to production. This installs A,
// then B, restores B, and checks A — not production's syscall.Kill — came
// back. No other test in this file uses the seam.
func TestSetPIDAliveForTestRestoresThePreviousValueNotProduction(t *testing.T) {
	restoreA := SetPIDAliveForTest(func(pid int) bool { return pid == 111 })
	restoreB := SetPIDAliveForTest(func(pid int) bool { return pid == 222 })

	restoreB()

	if !pidAlive(111) {
		t.Error("after restoring B, pidAlive(111) = false, want true: A should be back")
	}
	if pidAlive(222) {
		t.Error("after restoring B, pidAlive(222) = true, want false: B's own value must not linger")
	}
	if pidAlive(4242) {
		t.Error("after restoring B, pidAlive(4242) = true: production's syscall.Kill must not be reachable yet, A is")
	}

	restoreA()
}

// deadPID returns a pid that was valid a moment ago and is certainly dead
// now: this test binary's own throwaway child, spawned and reaped to match
// no tests (a near-instant, guaranteed-clean exit).
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a throwaway process for a dead pid: %v", err)
	}
	return cmd.Process.Pid
}

// I2b: a record caught mid-write (its two reads disagree) must not be
// accepted; Inspect must keep waiting and return the record once it has
// settled, not the transient one it first saw.
func TestInspectWaitsThroughAChangingRecordThenReturnsTheSettledOne(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()

	stale := testRecord(os.Getpid())
	stale.Started = stale.Started.Add(-time.Hour) // distinct from the final record
	staleBytes, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home), staleBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	final := testRecord(os.Getpid())
	finalBytes, err := json.Marshal(final)
	if err != nil {
		t.Fatal(err)
	}
	old := betweenStableReads
	fired := false
	betweenStableReads = func() {
		if fired {
			return
		}
		fired = true
		if err := os.WriteFile(Path(home), finalBytes, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { betweenStableReads = old })

	st, err := Inspect(home)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || !st.Record.Same(final) {
		t.Fatalf("Inspect = %+v, %v; want the settled, final record %+v", st, err, final)
	}
}

// --- fix round 2: pin the ordering and equality rules a swap could quietly --
// --- defeat, release's idempotency, and Inspect's mid-retry responsiveness -

// I2b (round 2): a record that NEVER settles — every read sees a different
// value — must never be accepted on the strength of a merely-successful
// read. Without the rec1 != rec2 comparison (only checking err != nil),
// each individual read is valid JSON and would be accepted immediately.
func TestInspectNeverAcceptsARecordThatNeverSettles(t *testing.T) {
	// Long enough for several attempts on a normal run; the assertions below
	// never depend on how many fit (a loaded -race CI runner once fitted
	// only one attempt into 80ms).
	setDuration(t, &recordWait, 500*time.Millisecond)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(Path(home))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()

	initial := testRecord(os.Getpid())
	b, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(home), b, 0o600); err != nil {
		t.Fatal(err)
	}

	old := betweenStableReads
	n := 0
	betweenStableReads = func() {
		n++
		rec := testRecord(os.Getpid())
		rec.Started = rec.Started.Add(time.Duration(n) * time.Second) // different every call
		b, err := json.Marshal(rec)
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(Path(home), b, 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { betweenStableReads = old })

	st, err := Inspect(home)
	if !st.Running || !errors.Is(err, ErrUnreadableRecord) {
		t.Fatalf("Inspect with a record that never settles = %+v, %v; want Running with ErrUnreadableRecord", st, err)
	}
	// One attempt is enough to prove the point: its two reads were each
	// valid JSON, and only the rec1 != rec2 comparison rejected them.
	if n < 1 {
		t.Fatalf("betweenStableReads fired %d times, want at least 1 (the test proves nothing otherwise)", n)
	}
}

// I2a (round 2): release must truncate the record BEFORE it unlocks, never
// after. betweenTruncateAndUnlock runs only once truncateRecord has
// returned and only before unlock runs, so reading the file from inside it
// directly pins the order — no need to race a second Acquire against it.
func TestReleaseTruncatesBeforeItUnlocks(t *testing.T) {
	home := t.TempDir()
	release, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}

	old := betweenTruncateAndUnlock
	fired := false
	betweenTruncateAndUnlock = func() {
		fired = true
		b, err := os.ReadFile(Path(home))
		if err != nil {
			t.Error(err)
			return
		}
		if len(b) != 0 {
			t.Errorf("record just before unlock = %q, want empty: release must truncate before it unlocks, not after", b)
		}
	}
	t.Cleanup(func() { betweenTruncateAndUnlock = old })

	if err := release(); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("betweenTruncateAndUnlock never ran")
	}
}

// NB1: release must be idempotent, and a stale release (from a lock this
// caller already gave up) must never reach out and touch whoever holds the
// lock now. This is the exact sequence the reviewer used to prove the bug:
// r1 := Acquire; r1(); r2 := Acquire; r1() again.
func TestReleaseIsIdempotentAndDoesNotTouchALaterHolder(t *testing.T) {
	home := t.TempDir()
	r1, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := r1(); err != nil {
		t.Fatal(err)
	}

	r2, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer r2()

	if err := r1(); err != nil {
		t.Fatalf("a second call to the first release = %v, want nil (idempotent)", err)
	}

	st, err := Inspect(home)
	if err != nil {
		t.Fatalf("Inspect after the stale release = %+v, %v; want r2's record intact, no error", st, err)
	}
	if !st.Running {
		t.Fatal("Inspect after the stale release reports not running; want r2 still holding")
	}
}

// NB2: a holder that releases (truncating, then unlocking) while Inspect is
// mid-retry must not leave Inspect spinning until recordWait: the very next
// iteration re-checks the lock itself and, finding it free, reports "not
// running" at once. betweenStableReads (inside Inspect's first, doomed
// attempt) fires the release synchronously, in the same goroutine, so there
// is nothing racy to drive here.
func TestInspectNoticesAReleaseMidRetryAndReportsNotRunningAtOnce(t *testing.T) {
	setDuration(t, &recordWait, 300*time.Millisecond)
	home := t.TempDir()
	release, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}

	old := betweenStableReads
	fired := false
	betweenStableReads = func() {
		if fired {
			return
		}
		fired = true
		if err := release(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { betweenStableReads = old })

	start := time.Now()
	st, err := Inspect(home)
	elapsed := time.Since(start)
	if err != nil || st.Running {
		t.Fatalf("Inspect after a mid-retry release = %+v, %v; want not running, no error", st, err)
	}
	if elapsed >= recordWait/2 {
		t.Errorf("Inspect took %v to notice the release, want well under recordWait (%v): it must not spin out the full budget", elapsed, recordWait)
	}
}

// NB3: swapping pidAlive from one goroutine while Inspect (on another) is
// reading it must be race-free. Run with -race; a plain, unsynchronized
// package var would be reported as a data race the moment these overlap.
func TestPIDAliveSwapDuringConcurrentInspectIsRaceFree(t *testing.T) {
	home := t.TempDir()
	release, err := Acquire(home, testRecord(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			restore := SetPIDAliveForTest(func(pid int) bool { return true })
			restore()
		}
	}()

	for i := 0; i < 50; i++ {
		if _, err := Inspect(home); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	wg.Wait()
}

// --- cross-process tests: the kernel's behaviour, observed for real --------

const (
	helperModeEnv = "CHOTTAG_DAEMONLOCK_HELPER"
	helperHomeEnv = "CHOTTAG_DAEMONLOCK_HELPER_HOME"
)

// TestHelperProcess is not a test. It skips unless startHelper re-execs this
// test binary with helperModeEnv set. Mode "hold" acquires the lock at once
// and prints "won". Mode "race" prints "armed", waits for a line on stdin,
// then acquires, printing "won" or "lost <holder pid>". A winner holds the
// lock until its stdin closes, with a 60s cap so that a parent which lost
// track of it never leaves it running.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process: runs only when startHelper re-execs this test binary")
	}
	in := bufio.NewReader(os.Stdin)
	if mode == "race" {
		fmt.Println("armed")
		if _, err := in.ReadString('\n'); err != nil {
			os.Exit(3)
		}
	}
	// release is kept explicitly (I1): a caller that discards it must still
	// keep the lock, but a helper deliberately proving that has no business
	// also proving it by accident here.
	release, err := Acquire(os.Getenv(helperHomeEnv), Current())
	var are *AlreadyRunningError
	switch {
	case err == nil:
		fmt.Println("won")
	case errors.As(err, &are):
		fmt.Printf("lost %d\n", are.PID)
		return
	default:
		fmt.Println("error", err)
		os.Exit(1)
	}
	defer release()
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, in); close(closed) }()
	select {
	case <-closed:
	case <-time.After(60 * time.Second):
	}
}

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
}

// startHelper re-execs this test binary as TestHelperProcess. The process
// is this test's own child; cleanup kills it, which is the only signal
// this file ever sends.
func startHelper(t *testing.T, home, mode string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperHomeEnv+"="+home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, stdin: stdin, lines: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	t.Cleanup(func() {
		stdin.Close()
		cmd.Process.Kill() // our own child; an error here only means it already exited
		for range h.lines {
		}
		cmd.Wait()
	})
	return h
}

// next returns the helper's next output line that starts with one of
// prefixes, skipping anything else the test framework prints.
func (h *helper) next(t *testing.T, prefixes ...string) string {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-h.lines:
			if !ok {
				t.Fatalf("helper pid %d exited before printing any of %q", h.cmd.Process.Pid, prefixes)
			}
			for _, p := range prefixes {
				if strings.HasPrefix(line, p) {
					return line
				}
			}
		case <-timeout:
			t.Fatalf("helper pid %d printed none of %q within 30s", h.cmd.Process.Pid, prefixes)
		}
	}
}

// Spec §4.6: "The kernel releases the lock when the process dies — cleanly,
// by crash or by SIGKILL". SIGKILL is the case no cleanup code can help
// with, so it is the one worth proving.
func TestTheKernelReleasesTheLockWhenTheHolderIsKilled(t *testing.T) {
	home := t.TempDir()
	h := startHelper(t, home, "hold")
	h.next(t, "won")

	st, err := Inspect(home)
	if err != nil || !st.Running || st.Record.PID != h.cmd.Process.Pid {
		t.Fatalf("Inspect while the helper holds = %+v, %v; want Running with pid %d", st, err, h.cmd.Process.Pid)
	}

	h.cmd.Process.Kill() // SIGKILL to our own child: nothing in it gets to clean up
	for range h.lines {
	}
	h.cmd.Wait()

	st, err = Inspect(home)
	if err != nil || st.Running {
		t.Fatalf("Inspect after the holder was SIGKILLed = %+v, %v; want not running", st, err)
	}
}

// Two `daemon start`s racing both spawn a daemon. Exactly one of them may
// end up holding the lock, and the loser must name the winner.
func TestConcurrentAcquiresInSeparateProcessesElectExactlyOneHolder(t *testing.T) {
	home := t.TempDir()
	a, b := startHelper(t, home, "race"), startHelper(t, home, "race")
	a.next(t, "armed")
	b.next(t, "armed")
	for _, h := range []*helper{a, b} {
		if _, err := io.WriteString(h.stdin, "go\n"); err != nil {
			t.Fatal(err)
		}
	}
	ra := a.next(t, "won", "lost", "error")
	rb := b.next(t, "won", "lost", "error")
	if (ra == "won") == (rb == "won") {
		t.Fatalf("results %q and %q: exactly one process must win the lock", ra, rb)
	}
	winner, lost := a, rb
	if rb == "won" {
		winner, lost = b, ra
	}
	if want := fmt.Sprintf("lost %d", winner.cmd.Process.Pid); lost != want {
		t.Errorf("loser printed %q, want %q (the winner's pid, read from its record)", lost, want)
	}
}
