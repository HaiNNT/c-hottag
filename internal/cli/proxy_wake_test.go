package cli

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// TestNewWakeHandlerDoesNotBlockOnASlowInvalidation pins F1: OnWake runs
// synchronously on the waker's own select loop, so a slow invalidateAll —
// modelling tm.InvalidateAll() blocked on a slot's mutex behind a Keychain
// prompt, exactly the state a lid-open resume can leave things in — must
// not stall the handler itself. closeUpstreams must still run
// synchronously (it is genuinely non-blocking in production; see
// newWakeHandler's doc comment), and logWake must still run even though
// the invalidation has not finished.
func TestNewWakeHandlerDoesNotBlockOnASlowInvalidation(t *testing.T) {
	release := make(chan struct{})
	invalidateStarted := make(chan struct{})
	var invalidateCalls, closeCalls, logCalls atomic.Int32
	handler := newWakeHandler(
		func() int { closeCalls.Add(1); return 0 },
		func() {
			invalidateCalls.Add(1)
			close(invalidateStarted)
			<-release
		},
		func() { logCalls.Add(1) },
	)

	done := make(chan struct{})
	go func() { handler(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("OnWake blocked on a slow invalidateAll instead of returning promptly")
	}

	select {
	case <-invalidateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("invalidateAll was never started")
	}
	close(release)

	if closeCalls.Load() != 1 {
		t.Fatalf("closeUpstreams calls = %d, want 1", closeCalls.Load())
	}
	if logCalls.Load() != 1 {
		t.Fatalf("logWake calls = %d, want 1", logCalls.Load())
	}
}

// TestNewWakeHandlerDropsASecondWakeWhileInvalidationIsInFlight pins the
// single-flight half of F1: a wake storm (several rapid resume events)
// must not stack goroutines each parked on the same slot mutex. A second
// call while the first invalidateAll is still running must not start a
// second one; once the first finishes, a third call must start a new one
// (the guard resets, rather than permanently disabling invalidation).
func TestNewWakeHandlerDropsASecondWakeWhileInvalidationIsInFlight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	var invalidateCalls atomic.Int32
	handler := newWakeHandler(
		func() int { return 0 },
		func() {
			invalidateCalls.Add(1)
			started <- struct{}{}
			<-release
		},
		func() {},
	)
	// callBounded runs handler on its own goroutine and fails cleanly,
	// within a few seconds, if it does not return (N7): under a mutation
	// reverting OnWake to call invalidateAll fully synchronously, EVERY
	// call below — not just the first — would otherwise block its calling
	// goroutine on <-release forever, and this test would die only at the
	// package's default multi-minute timeout, even though the sibling
	// TestNewWakeHandlerDoesNotBlockOnASlowInvalidation already catches
	// that same mutation cleanly.
	callBounded := func(fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() { fn(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a call to handler() did not return within the bound")
		}
	}

	callBounded(handler) // first wake: starts invalidateAll, which blocks on release
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first invalidateAll never started")
	}

	callBounded(handler) // second wake while the first is still in flight: must be dropped
	callBounded(handler) // and a third, for good measure

	// Give any wrongly-spawned second/third invalidateAll a chance to
	// register itself in started before asserting there is none.
	select {
	case <-started:
		t.Fatal("a second invalidateAll started while the first was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	if invalidateCalls.Load() != 1 {
		t.Fatalf("invalidateAll calls while the first was in flight = %d, want 1", invalidateCalls.Load())
	}

	close(release) // let the first invalidateAll return, which resets the guard
	// Give the deferred guard reset a moment to run: it happens on the
	// first invalidateAll's own goroutine, after release unblocks it, so
	// there is no channel to synchronize on here.
	time.Sleep(50 * time.Millisecond)

	handler() // fourth wake, after the guard reset: must start a new invalidateAll
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("invalidateAll never ran again after the guard reset")
	}
	if invalidateCalls.Load() != 2 {
		t.Fatalf("invalidateAll calls after the guard reset = %d, want 2", invalidateCalls.Load())
	}
}

// TestNewWakeHandlerRecoversFromAPanicInInvalidateAllAndReleasesTheGuard
// pins two things a panicking invalidateAll must not do: kill the daemon
// (an unrecovered panic on a goroutine takes the whole process down —
// exactly why refreshDetached in internal/tokens/tokens.go carries a
// recover of its own), and strand the single-flight guard at true forever,
// which would silently drop every wake for the rest of the process's life.
// That second failure mode is the one worth a dedicated test: this project
// already shipped that exact bug class once, a panicking refresher whose
// recover skipped its own backoff reset. Two wakes are driven through the
// SAME handler here — a panic on the first, then a second call — so a
// mutation that recovers but forgets to release the guard fails this test
// even though the process itself survives.
func TestNewWakeHandlerRecoversFromAPanicInInvalidateAllAndReleasesTheGuard(t *testing.T) {
	started := make(chan struct{}, 2) // bounded: invalidateAll must never block trying to report in
	var invalidateCalls atomic.Int32
	handler := newWakeHandler(
		func() int { return 0 },
		func() {
			invalidateCalls.Add(1)
			started <- struct{}{}
			panic("boom: invalidateAll panicked")
		},
		func() {},
	)

	callBounded := func() {
		t.Helper()
		done := make(chan struct{})
		go func() { handler(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("handler() did not return within the bound")
		}
	}

	callBounded() // first wake: invalidateAll panics on its own goroutine
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first invalidateAll never started")
	}
	// The panic recovers on invalidateAll's own goroutine, not handler's
	// (handler already returned once it spawned that goroutine), so there
	// is no channel to synchronize the guard release on: give it a moment
	// to run before the second wake below.
	time.Sleep(50 * time.Millisecond)

	callBounded() // second wake, after the panic: the guard must have been released
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("second invalidateAll never started — the panic stranded the single-flight guard at true")
	}
	if invalidateCalls.Load() != 2 {
		t.Fatalf("invalidateAll calls = %d, want 2", invalidateCalls.Load())
	}
}

// TestRunDaemonWakerStopsWithServingContextAndIgnoresTicksAfter pins F2:
// the wake detector must run on a context that is actually tied to ctx
// (not context.Background()), and it must actually be started at all.
// wakerDone closes when Waker.Run returns; Ticks here is a channel this
// test alone controls and never closes, so wakerDone closing within the
// bound below can only be explained by Run having received a cancellation
// derived from ctx.
//
// Since item (iv) added runDaemon's own join (<-wakerStopped) before
// every return, the FIRST select below — waiting on <-daemonDone — is now
// what actually catches both regressions this test names: if the waker
// were ever wired to context.Background(), or never started at all,
// runDaemon itself would hang at its own join and never return, so
// daemonDone would time out there first. The second select, on wakerDone
// directly, can therefore never take its timeout arm on any build that
// reaches the first select's success case at all — with the join in
// place, d.WakerDone is guaranteed already closed by the time
// runDaemon (and hence daemonDone) returns, since the waker goroutine
// closes it before its own deferred close(wakerStopped). It stays as an
// explicit, separate assertion documenting that invariant (WakerDone and
// runDaemon's return must never decouple), not as this test's primary
// catch — if you are debugging a failure here, look at the FIRST select's
// message, not this one's.
func TestRunDaemonWakerStopsWithServingContextAndIgnoresTicksAfter(t *testing.T) {
	ticks := make(chan time.Time)
	var onWakeCalls atomic.Int32
	wakerCfg := proxy.WakerConfig{
		Gap:    30 * time.Second,
		Ticks:  ticks,
		Wall:   func() time.Time { return time.Now() },
		Mono:   func() time.Duration { return 0 }, // constant: any tick's wall delta beats Gap
		OnWake: func() { onWakeCalls.Add(1) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	wakerDone := make(chan struct{})
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:    io.Discard,
			Stderr:    io.Discard,
			Listen:    "127.0.0.1:0",
			Sink:      sink,
			Tokens:    tm,
			Cache:     store.NewCache(store.Store{Dir: t.TempDir()}),
			Owners:    own,
			Waker:     wakerCfg,
			WakerDone: wakerDone,
		})
	}()

	cancel()

	select {
	case code := <-daemonDone:
		if code != 0 {
			t.Fatalf("runDaemon returned %d, want 0 on a clean shutdown", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return after its context was cancelled")
	}

	select {
	case <-wakerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("WakerDone was still open after daemonDone already fired: this should be unreachable given runDaemon's own <-wakerStopped join (item iv) — WakerDone and runDaemon's return have decoupled somehow")
	}

	// A tick sent after Run has already returned must never reach OnWake.
	// The send is non-blocking: Run's goroutine is gone, so a blocking
	// send here would deadlock the test rather than prove anything.
	select {
	case ticks <- time.Now():
	default:
	}
	time.Sleep(50 * time.Millisecond)
	if onWakeCalls.Load() != 0 {
		t.Fatal("OnWake fired for a tick sent after the wake detector had already stopped")
	}
}

// TestRunDaemonDefaultsOnWakeAndLogsWhenItFires pins N1 at the runDaemon
// level: TestRunDaemonWakerStopsWithServingContextAndIgnoresTicksAfter
// above always supplies its own wakerCfg.OnWake, so it never exercises the
// `if wakerCfg.OnWake == nil { wakerCfg.OnWake = newWakeHandler(...) }`
// branch at all — deleting that assignment left OnWake nil in production,
// which proxy.NewWaker's own guard (added this round) now turns into a
// silent no-op rather than a crash, but a silent no-op is still a defect:
// no upstream connections closed and no token invalidation on a real
// wake. This test leaves wakerCfg.OnWake unset, drives a real wake
// through the fake clock, and requires the exact log line to appear —
// which only happens if runDaemon actually assigned the default handler,
// and only with this exact wording (catching the "log-argument mutation"
// alongside the assignment deletion, both with one assertion).
func TestRunDaemonDefaultsOnWakeAndLogsWhenItFires(t *testing.T) {
	ticks := make(chan time.Time)
	var wallNanos atomic.Int64
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	wallNanos.Store(base.UnixNano())
	wakerCfg := proxy.WakerConfig{
		Gap:   30 * time.Second,
		Ticks: ticks,
		Wall:  func() time.Time { return time.Unix(0, wallNanos.Load()).UTC() },
		Mono:  func() time.Duration { return 0 },
		// OnWake deliberately left unset: runDaemon must supply the
		// default.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard,
			Stderr: stderr,
			Listen: "127.0.0.1:0",
			Sink:   sink,
			Tokens: tm,
			Cache:  store.NewCache(store.Store{Dir: t.TempDir()}),
			Owners: own,
			Waker:  wakerCfg,
		})
	}()

	// A first tick with the wall clock untouched: ticks is unbuffered, so
	// this send completing at all proves Run has already called
	// baseline() (using wall=base, since nothing has moved it yet) and is
	// now parked in its select loop — synchronizing with runDaemon's own
	// goroutine without a race on wallNanos. Only THEN is it safe to
	// advance the wall clock and send the tick that should read as a
	// suspend (matching TestRunCapturesItsBaselineOnceBeforeTheLoop's
	// two-tick pattern in internal/proxy/wake_test.go).
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("Run never consumed the warm-up tick")
	}
	wallNanos.Store(base.Add(60 * time.Second).UnixNano())
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("Run never consumed the wake-triggering tick")
	}

	// Contains, not equals: the wake detector runs on its own goroutine,
	// started before net.Listen, so its log line can interleave either
	// side of the "listening on" line runDaemon prints separately.
	const want = "chottag: woke from sleep, closed upstream connections\n"
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(stderr.String(), want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stderr = %q, want it to contain %q — the default OnWake handler either was not assigned or logged something else", stderr.String(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return after its context was cancelled")
	}
}

// TestRunDaemonWakeInvalidatesTheRealTokenManager pins M15 by its
// verbatim definition — "make tm.InvalidateAll a no-op in the wake
// handler" — at the runDaemon level: newWakeHandler's own unit tests
// (TestNewWakeHandlerDoesNotBlockOnASlowInvalidation and its sibling)
// pass in a FAKE invalidateAll func and so cannot see whether runDaemon
// actually wires the real tm.InvalidateAll into the default handler it
// builds, only that whatever function it was given gets called with the
// right blocking/single-flight semantics. Nothing else exercises that
// wiring: TestRunDaemonDefaultsOnWakeAndLogsWhenItFires drives a real
// tm and a real wake too, but only ever asserts the log line, which
// fires regardless of whether invalidateAll did anything at all.
//
// Observed indirectly, the same way InvalidateAll's own doc comment
// describes its effect: it resets a slot's readAt, so the NEXT read is
// forced even inside a long ReadTTL. A slot is seeded with one Token()
// call (populating readAt and counting one Read), given a ReadTTL far
// longer than this test's own bound so a second Token call would other-
// wise just serve the cache, then a real wake is driven through the fake
// clock; a following Token call must show a second Read — proving the
// slot's cache was actually invalidated, not left alone by a no-op.
func TestRunDaemonWakeInvalidatesTheRealTokenManager(t *testing.T) {
	var reads atomic.Int32
	tm := tokens.New(tokens.Config{
		Read: func(string) (creds.Token, error) {
			reads.Add(1)
			return creds.Token{AccessToken: "tok", ExpiresAt: time.Now().Add(24 * time.Hour)}, nil
		},
		ReadTTL: time.Hour, // far longer than this test: a second Token() call within it must NOT naturally re-read
	})
	const slotDir = "/slots/A"
	if _, _, ok := tm.Token(context.Background(), slotDir); !ok {
		t.Fatal("seeding Token call did not report ok")
	}
	if reads.Load() != 1 {
		t.Fatalf("reads = %d after seeding, want 1", reads.Load())
	}

	ticks := make(chan time.Time)
	var wallNanos atomic.Int64
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	wallNanos.Store(base.UnixNano())
	wakerCfg := proxy.WakerConfig{
		Gap:   30 * time.Second,
		Ticks: ticks,
		Wall:  func() time.Time { return time.Unix(0, wallNanos.Load()).UTC() },
		Mono:  func() time.Duration { return 0 },
		// OnWake deliberately left unset: runDaemon must supply the
		// default, which wires tm.InvalidateAll.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard,
			Stderr: io.Discard,
			Listen: "127.0.0.1:0",
			Sink:   sink,
			Tokens: tm,
			Cache:  store.NewCache(store.Store{Dir: t.TempDir()}),
			Owners: own,
			Waker:  wakerCfg,
		})
	}()

	// Two-tick warm-up/trigger pattern (matching
	// TestRunDaemonDefaultsOnWakeAndLogsWhenItFires): the first tick
	// synchronizes with Run's baseline() call without racing wallNanos,
	// only then is it safe to advance the wall clock and send the tick
	// that reads as a suspend.
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("Run never consumed the warm-up tick")
	}
	wallNanos.Store(base.Add(60 * time.Second).UnixNano())
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("Run never consumed the wake-triggering tick")
	}

	// invalidateAll runs on its own goroutine (F1's fix), so the second
	// read may not be visible the instant the tick send above returns:
	// poll rather than sleep once.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, ok := tm.Token(context.Background(), slotDir); !ok {
			t.Fatal("post-wake Token call did not report ok")
		}
		if reads.Load() >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reads = %d after a wake, want >= 2 — the wake's default OnWake did not invalidate the real tokens.Manager's slot", reads.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return after its context was cancelled")
	}
}

// R149: a wake from sleep schedules the warm pass wakeWarmDelay later, and
// that pass reaches the token manager with the wake trigger's warm func.
func TestRunDaemonWakeSchedulesTheWarmPass(t *testing.T) {
	ticks := make(chan time.Time)
	var wallNanos atomic.Int64
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	wallNanos.Store(base.UnixNano())
	wakerCfg := proxy.WakerConfig{
		Gap:   30 * time.Second,
		Ticks: ticks,
		Wall:  func() time.Time { return time.Unix(0, wallNanos.Load()).UTC() },
		Mono:  func() time.Duration { return 0 },
	}
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "S", Dir: "/slots/S"}); err != nil {
			return err
		}
		st.Serving = "S"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	scheduled := make(chan time.Duration, 1)
	warmed := make(chan string, 4)
	w := &remoteWarmer{
		state: cache.State,
		warm: func(context.Context, string, time.Duration) (creds.Status, bool) {
			return creds.Status{State: creds.StateOK}, false
		},
		wakeWarm: func(_ context.Context, dir string, _ time.Duration) (creds.Status, bool) {
			warmed <- dir
			return creds.Status{State: creds.StateOK}, false
		},
		after: func(d time.Duration, f func()) *time.Timer {
			scheduled <- d
			go f() // the delay is not what is under test
			return time.NewTimer(time.Hour)
		},
		log: io.Discard, now: time.Now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard, Stderr: io.Discard, Listen: "127.0.0.1:0",
			Sink: sink, Tokens: tokens.New(tokens.Config{}), Cache: cache, Owners: own,
			Waker: wakerCfg, Warm: w,
		})
	}()
	ticks <- time.Now() // synchronizes with the waker's baseline, as above
	wallNanos.Store(base.Add(60 * time.Second).UnixNano())
	ticks <- time.Now()
	if d := <-scheduled; d != wakeWarmDelay {
		t.Fatalf("scheduled after %v, want %v", d, wakeWarmDelay)
	}
	if dir := <-warmed; dir != "/slots/S" {
		t.Fatalf("wake pass warmed %q, want the serving slot", dir)
	}
	cancel()
	<-daemonDone
}
