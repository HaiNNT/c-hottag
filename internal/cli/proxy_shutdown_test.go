package cli

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestAwaitShutdownSignalCancelsOnFirstSignal pins the graceful half of
// F4: a single signal calls cancel exactly once and does not force an
// exit.
func TestAwaitShutdownSignalCancelsOnFirstSignal(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forceCalls atomic.Int32
	cancelled := make(chan struct{})
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		awaitShutdownSignal(sig, func() { close(cancelled) }, done, func() { forceCalls.Add(1) })
		close(finished)
	}()

	sig <- os.Interrupt
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel was never called after a signal arrived")
	}
	// done only closes once cancel has definitely already run: closing it
	// any earlier would let awaitShutdownSignal's first select race sig
	// against done and — legitimately, since both would be ready — return
	// without ever calling cancel, which is not what this test pins.
	close(done) // simulate the daemon finishing its graceful shutdown

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("awaitShutdownSignal did not return once the daemon finished")
	}
	if forceCalls.Load() != 0 {
		t.Fatalf("forceExit calls = %d, want 0: only one signal ever arrived", forceCalls.Load())
	}
}

// TestAwaitShutdownSignalForcesExitOnASecondSignalBeforeDone pins F4's
// actual fix: measured, a second and third SIGINT arriving during the 5s
// grace window were silently absorbed (rc=0 after 5.02s). A second signal
// arriving before the daemon has finished shutting down must force an
// immediate exit instead of waiting out the rest of the grace period.
func TestAwaitShutdownSignalForcesExitOnASecondSignalBeforeDone(t *testing.T) {
	sig := make(chan os.Signal, 2)
	done := make(chan struct{}) // never closed: the daemon is still "shutting down"
	var forceCalls atomic.Int32
	finished := make(chan struct{})

	go func() {
		awaitShutdownSignal(sig, func() {}, done, func() { forceCalls.Add(1); close(finished) })
	}()

	sig <- os.Interrupt
	sig <- os.Interrupt

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("forceExit was not called after a second signal arrived before the daemon finished")
	}
	if forceCalls.Load() != 1 {
		t.Fatalf("forceExit calls = %d, want 1", forceCalls.Load())
	}
}

// TestForceExitUnlessDoneSkipsWhenDoneIsAlreadyClosed and
// TestForceExitUnlessDoneRunsWhenDoneIsStillOpen pin F3b's recheck
// directly, deterministically: forceExitUnlessDone was pulled out of
// awaitShutdownSignal's final select specifically so this could be
// asserted without racing the goroutine scheduler (see
// TestAwaitShutdownSignalNeverCallsCancelWhenDoneIsAlreadyClosed's
// comment for why the wider race it defends against cannot be
// constructed deterministically). Together they pin both directions: a
// mutation dropping the check entirely (always calling forceExit) fails
// the first; a mutation inverting it (never calling forceExit) fails the
// second.
func TestForceExitUnlessDoneSkipsWhenDoneIsAlreadyClosed(t *testing.T) {
	done := make(chan struct{})
	close(done)
	var forceCalls atomic.Int32
	forceExitUnlessDone(done, func() { forceCalls.Add(1) })
	if forceCalls.Load() != 0 {
		t.Fatalf("forceExit calls = %d, want 0: done was already closed", forceCalls.Load())
	}
}

func TestForceExitUnlessDoneRunsWhenDoneIsStillOpen(t *testing.T) {
	done := make(chan struct{}) // never closed
	var forceCalls atomic.Int32
	forceExitUnlessDone(done, func() { forceCalls.Add(1) })
	if forceCalls.Load() != 1 {
		t.Fatalf("forceExit calls = %d, want 1: done was still open", forceCalls.Load())
	}
}

// TestAwaitShutdownSignalNeverCallsCancelWhenDoneIsAlreadyClosed pins
// F3b's own acceptance-bar mutation — "delete both done-priority checks"
// — deterministically, without racing the goroutine scheduler.
//
// A genuinely-racy reproduction (done closing concurrently with a
// buffered signal, right as the final select blocks) was tried first and
// rejected: measured under both go test's default scheduler and -race,
// it hits its target window at a real but small rate even WITH both
// checks present (their own doc comment already says the checks narrow,
// not eliminate, that specific race) — 1-2% without -race, but 24-46%
// with -race, an ordering that flips depending on the flag and so cannot
// be told apart from a correctly-fixed run by any single threshold. That
// gap is reported below rather than hidden behind a threshold tuned to
// pass regardless of correctness.
//
// This test instead exploits a DIFFERENT, fully deterministic route to
// the same mutation: done and sig are put into their final states BEFORE
// awaitShutdownSignal is ever called (done closed, sig carrying a
// buffered value) — no concurrency, no timing, just Go's own spec ("if
// one or more of the communications can proceed, a single one that can
// proceed is chosen via a uniform pseudo-random selection") applied to a
// select whose cases are already resolved by the time it runs.
//
//   - With both checks present, the very first one (done alone, no tie
//     with sig at all) always wins: cancel is never called, deterministically,
//     zero flakiness, on every run.
//   - With both deleted, the first blocking select (now sig-or-done, with
//     sig ALSO ready) ties, and Go's pseudo-random tie-break calls cancel
//     roughly half the time. It never calls forceExit even then (sig had
//     only one buffered value, already consumed by that same tie-break,
//     so the second select has nothing left to tie with done on) — cancel
//     alone is the only mutation-visible symptom this construction has,
//     so that is what this asserts.
//
// Looping rather than running once turns the mutant's ~50%-per-run
// chance into a near-certainty across the run (P(all N misses) = 0.5^N).
func TestAwaitShutdownSignalNeverCallsCancelWhenDoneIsAlreadyClosed(t *testing.T) {
	const iterations = 20
	for i := 0; i < iterations; i++ {
		sig := make(chan os.Signal, 1)
		sig <- os.Interrupt
		done := make(chan struct{})
		close(done)
		var cancelCalls, forceCalls atomic.Int32

		awaitShutdownSignal(sig, func() { cancelCalls.Add(1) }, done, func() { forceCalls.Add(1) })

		if cancelCalls.Load() != 0 {
			t.Fatalf("iteration %d: cancel calls = %d, want 0 — done was already closed before the call; a done-priority check was not run, or was removed", i, cancelCalls.Load())
		}
		if forceCalls.Load() != 0 {
			t.Fatalf("iteration %d: forceExit calls = %d, want 0", i, forceCalls.Load())
		}
	}
}

// TestInstallShutdownCancelsRunsContextAndPropagatesItsCode pins N2's
// extraction directly: installShutdown, not runProxy's own inline body,
// now owns starting the watcher goroutine and handing run the cancellable
// ctx. Bounded (goroutine + select/timeout), not a bare call: under a
// mutation that hands run context.Background() instead, run's own
// <-ctx.Done() below would never fire and this call would hang forever
// (N7's lesson applied here too).
func TestInstallShutdownCancelsRunsContextAndPropagatesItsCode(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forceCalls atomic.Int32

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- installShutdown(sig, func() { forceCalls.Add(1) }, func(ctx context.Context) int {
			sig <- os.Interrupt // the "first Ctrl-C", sent once run has actually started
			<-ctx.Done()        // only returns if installShutdown wired THIS ctx to the signal
			return 7
		})
	}()

	select {
	case code := <-codeCh:
		if code != 7 {
			t.Fatalf("code = %d, want 7 — installShutdown must propagate run's own return code", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("installShutdown did not return — did it hand run a cancellable ctx wired to the first signal?")
	}
	if forceCalls.Load() != 0 {
		t.Fatalf("forceExit calls = %d, want 0: only one signal ever arrived", forceCalls.Load())
	}
}

// TestInstallShutdownForcesExitOnASecondSignalWhileRunIsStillGoing pins
// the other end of N2: a mutation deleting the `go awaitShutdownSignal(...)`
// call inside installShutdown leaves nothing consuming sig at all, so
// neither the first signal's cancel nor a second signal's forceExit would
// ever fire, and run below — blocked on blockRun, standing in for "the
// daemon has not finished shutting down yet" — would never return either;
// installShutdown itself would hang. Bounded the same way.
func TestInstallShutdownForcesExitOnASecondSignalWhileRunIsStillGoing(t *testing.T) {
	sig := make(chan os.Signal, 2)
	blockRun := make(chan struct{}) // never closed: run never returns on its own
	forced := make(chan struct{})

	go func() {
		installShutdown(sig, func() { close(forced) }, func(ctx context.Context) int {
			<-blockRun
			return 0
		})
	}()

	sig <- os.Interrupt
	sig <- os.Interrupt

	select {
	case <-forced:
	case <-time.After(2 * time.Second):
		t.Fatal("forceExit was not called after a second signal arrived while run was still in flight")
	}
}

// TestInstallShutdownStopsWatchingOnceRunReturnsWithoutASignal pins the
// third mutation: dropping installShutdown's `close(done)` right after run
// returns. Without it, the watcher goroutine stays armed on its first
// select (sig or done) forever; a signal sent well after installShutdown
// has already returned would then still be read, call cancel (harmlessly,
// since nothing reads ctx any more) and arm the SECOND select, so one more
// signal after that would wrongly call forceExit for events that have
// nothing to do with a shutdown in progress.
func TestInstallShutdownStopsWatchingOnceRunReturnsWithoutASignal(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var forceCalls atomic.Int32

	code := installShutdown(sig, func() { forceCalls.Add(1) }, func(ctx context.Context) int {
		return 3 // returns cleanly; no signal was ever involved
	})
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}

	// Two signals sent AFTER installShutdown has already returned must
	// never reach forceExit — the watcher must already have stopped
	// (via close(done)) rather than still being parked on its first
	// select, which a signal now would wake into the graceful path and a
	// second into forceExit.
	sig <- os.Interrupt
	sig <- os.Interrupt
	time.Sleep(100 * time.Millisecond)
	if forceCalls.Load() != 0 {
		t.Fatal("forceExit fired for signals sent after installShutdown had already returned — its shutdown watcher was still armed")
	}
}

// TestInstallShutdownRegistersARealSignalHandler is the one case none of
// the three tests above can reach: sig == nil is installShutdown's
// PRODUCTION path, where it must call signal.Notify itself — a mutation
// deleting that one line cannot be caught by any test that supplies its
// own channel (that branch never runs), only by a real signal actually
// reaching the process. It deliberately sends a real SIGINT to this
// process's own pid: under correct code, signal.Notify has claimed
// SIGINT, so this is safe and reliably observable; under the mutation
// being tested for, SIGINT's default disposition (terminate the process)
// applies instead, and the failure is the test binary itself dying rather
// than a clean FAIL — the strongest failure signal there is, reported
// as such rather than hidden.
func TestInstallShutdownRegistersARealSignalHandler(t *testing.T) {
	// F4b: os/signal fans SIGINT out to every channel registered for it, so
	// registering our own guard here alongside installShutdown's real
	// signal.Notify does not change the good path at all. But under the
	// mutation this test exists to catch — installShutdown(nil, ...)
	// dropping its own signal.Notify call — SIGINT's default disposition
	// (terminate the process) no longer applies either, because THIS
	// channel is still claiming it: the test now fails cleanly at its
	// existing 2s bound with its existing diagnosis, instead of the whole
	// test binary dying, which is unattributable and — under -shuffle=on —
	// takes every other concurrently-running test down with it too.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(guard) })

	started := make(chan struct{})
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- installShutdown(nil, func() {
			t.Error("forceExit called on a single real signal")
		}, func(ctx context.Context) int {
			close(started)
			<-ctx.Done()
			return 9
		})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("run was never called")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-codeCh:
		if code != 9 {
			t.Fatalf("code = %d, want 9", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("installShutdown(nil, ...) never cancelled run's ctx after a real SIGINT — is signal.Notify still registered?")
	}
}
