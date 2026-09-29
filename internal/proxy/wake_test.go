package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClocks drives the detector deterministically: tick() advances both
// clocks by the same amount (normal running), sleep() advances only the
// wall clock (the machine was suspended). It needs no synchronization, but
// not because nothing here ever runs through Run — TestWakerStopsWithItsContext
// does exactly that. It's safe because nothing mutates a fakeClocks value
// after constructing it in either test that uses one:
// TestWakerFiresOnlyWhenTheWallClockJumps drives baseline()/check()
// directly on a single goroutine and never touches Run, while
// TestWakerStopsWithItsContext does start Run on its own goroutine but
// never changes the clock afterward, so there is nothing for that second
// goroutine's read to race against. A test that both runs Run AND keeps
// mutating the clock afterward needs real synchronization — see
// TestRunCapturesItsBaselineOnceBeforeTheLoop below, which uses atomics
// for exactly that reason.
type fakeClocks struct {
	wall time.Time
	mono time.Duration
}

func (c *fakeClocks) tick(d time.Duration) {
	c.wall = c.wall.Add(d)
	c.mono += d
}

func (c *fakeClocks) sleep(d time.Duration) { c.wall = c.wall.Add(d) }

// TestNewWakerDefaultsOnWakeToANoOp pins the nil guard: check calls
// cfg.OnWake with no nil check of its own (wake.go), so a caller that
// leaves WakerConfig.OnWake unset — every other field here gets a default
// from NewWaker, so this is the one a wiring change is most likely to
// leave out by accident — must not crash the first time a real wake
// fires. Constructing with no OnWake and then firing a wake must not
// panic.
func TestNewWakerDefaultsOnWakeToANoOp(t *testing.T) {
	c := &fakeClocks{wall: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	w := NewWaker(WakerConfig{
		Gap:  30 * time.Second,
		Wall: func() time.Time { return c.wall },
		Mono: func() time.Duration { return c.mono },
		// OnWake deliberately left unset.
	})
	w.baseline()
	c.sleep(31 * time.Second) // past Gap: this check would fire OnWake

	w.check() // must not panic
}

func TestWakerFiresOnlyWhenTheWallClockJumps(t *testing.T) {
	c := &fakeClocks{wall: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	woke := 0

	w := NewWaker(WakerConfig{
		Gap:    30 * time.Second,
		Wall:   func() time.Time { return c.wall },
		Mono:   func() time.Duration { return c.mono },
		OnWake: func() { woke++ },
	})
	w.baseline()

	// Five ordinary ticks: no wake.
	for i := 0; i < 5; i++ {
		c.tick(5 * time.Second)
		w.check()
	}
	if woke != 0 {
		t.Fatalf("woke %d times during normal running, want 0", woke)
	}

	// A gap right at the threshold is not yet a sleep.
	c.tick(5 * time.Second)
	c.sleep(30 * time.Second)
	w.check()
	if woke != 0 {
		t.Errorf("woke %d times on a gap exactly equal to Gap, want 0", woke)
	}

	// Past the threshold: one wake.
	c.tick(5 * time.Second)
	c.sleep(31 * time.Second)
	w.check()
	if woke != 1 {
		t.Errorf("woke %d times after a 31s gap, want 1", woke)
	}

	// Back to normal: no further wakes, and the baseline has been reset.
	for i := 0; i < 3; i++ {
		c.tick(5 * time.Second)
		w.check()
	}
	if woke != 1 {
		t.Errorf("woke %d times in total, want 1 — the baseline did not reset after a wake", woke)
	}
}

func TestWakerStopsWithItsContext(t *testing.T) {
	c := &fakeClocks{wall: time.Now()}
	ticks := make(chan time.Time)
	w := NewWaker(WakerConfig{
		Wall:   func() time.Time { return c.wall },
		Mono:   func() time.Duration { return c.mono },
		Ticks:  ticks,
		OnWake: func() {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}

// TestRunDrivesRealTicksIntoOnWake exercises Run's real-ticker branch
// (WakerConfig.Ticks left nil) with a genuine assertion, not just "Run
// returns when cancelled" — the ctx.Done() arm alone satisfies that even
// if the whole ticker branch, or the rest of Run's body, were deleted.
// Gap is negative, which NewWaker passes through unchanged (it only
// defaults Gap when it is exactly 0), so any tick at all — even a
// near-instant one — fires OnWake. If Run's real-ticker construction, or
// its tick-handling, is missing, OnWake never fires and this times out.
func TestRunDrivesRealTicksIntoOnWake(t *testing.T) {
	woke := make(chan struct{}, 1)
	w := NewWaker(WakerConfig{
		Interval: 5 * time.Millisecond,
		Gap:      -1,
		OnWake:   func() { woke <- struct{}{} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("real ticker never drove a check into OnWake")
	}
}

// TestRunCapturesItsBaselineOnceBeforeTheLoop exercises Run itself, not
// baseline()/check() called directly the way
// TestWakerFiresOnlyWhenTheWallClockJumps does — that test cannot see a
// bug in Run's own orchestration (e.g. Run failing to call baseline()
// before its loop, or calling check() there instead), because it never
// runs Run at all.
//
// The clock is a pair of atomics, not a mutex or a rendezvous hook: Run's
// goroutine and this test's goroutine never touch it at the same instant
// by construction, not by locking. The first tick sent below carries
// exactly the same wall/mono values the clock already had when Run
// started, so whether Run's pre-loop step correctly captured them as a
// baseline, or incorrectly evaluated them as a check, the two are
// distinguishable (only the second fires a wake) — and because an
// unbuffered channel send cannot complete until Run is back at its select
// statement, which requires that pre-loop step to have already finished,
// the assertion right after the send is not a race against it. The second
// tick then advances both clocks by the same 5s, which stays a "no wake"
// tick whichever of Run's two reads (this one, or a stale one Run had not
// yet consumed) it happens to land on, so there is nothing timing-sensitive
// left to get flaky.
func TestRunCapturesItsBaselineOnceBeforeTheLoop(t *testing.T) {
	var wallNanos, monoNanos atomic.Int64
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	wallNanos.Store(base.UnixNano())

	var woke atomic.Int64
	ticks := make(chan time.Time)
	w := NewWaker(WakerConfig{
		Gap:    30 * time.Second,
		Wall:   func() time.Time { return time.Unix(0, wallNanos.Load()).UTC() },
		Mono:   func() time.Duration { return time.Duration(monoNanos.Load()) },
		Ticks:  ticks,
		OnWake: func() { woke.Add(1) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// sendTick bounds the handoff: an unguarded `ticks <- at` blocks
	// forever if Run's body never reads from ticks at all (e.g. it was
	// replaced outright), which would hang this test past any reasonable
	// timeout instead of failing it.
	sendTick := func(at time.Time) {
		t.Helper()
		select {
		case ticks <- at:
		case <-time.After(2 * time.Second):
			t.Fatal("Run never consumed a tick — its select loop is not reading Ticks")
		}
	}

	// Unchanged from what Run's pre-loop step already saw. A wake here
	// means that step evaluated the wake condition (against a zero-value
	// baseline — roughly 2026 years away from "now") instead of merely
	// recording a baseline for later.
	sendTick(base)
	if got := woke.Load(); got != 0 {
		t.Fatalf("woke %d times before any real state change — Run's pre-loop step must capture a baseline, not evaluate one", got)
	}

	// A normal 5s/5s advance. If Run's pre-loop step never ran at all,
	// lastWall/lastMono are still zero-value here too, and this fires for
	// the same reason the first tick would have — proving the loop's own
	// check is using a baseline carried over from before the loop, not a
	// fresh (missing) one.
	wallNanos.Store(base.Add(5 * time.Second).UnixNano())
	monoNanos.Store(int64(5 * time.Second))
	sendTick(base.Add(5 * time.Second))
	if got := woke.Load(); got != 0 {
		t.Fatalf("woke %d times after a normal 5s tick — Run is not carrying its pre-loop baseline into the loop", got)
	}
}

// TestDefaultWallStripsTheMonotonicReading pins that NewWaker's default
// Wall clock calls time.Now().Round(0). Every other test in this file
// injects Wall explicitly, so none of them would notice if that call were
// dropped — but without it, the default Wall and the default Mono would
// both carry the same monotonic reading, the two would stay in lock step
// through a real suspend exactly the way they're designed not to, and the
// detector could never fire in production.
func TestDefaultWallStripsTheMonotonicReading(t *testing.T) {
	w := NewWaker(WakerConfig{OnWake: func() {}})
	if got := w.cfg.Wall(); got != got.Round(0) {
		t.Fatal("default Wall still carries a monotonic reading")
	}
}

// TestDefaultWakeConstantsMatchSpec6 pins the values spec §6 names — every
// other test in this file injects Gap explicitly and would not notice if
// DefaultWakeInterval or DefaultWakeGap drifted from the spec.
func TestDefaultWakeConstantsMatchSpec6(t *testing.T) {
	if DefaultWakeInterval != 5*time.Second {
		t.Errorf("DefaultWakeInterval = %v, want 5s (spec §6)", DefaultWakeInterval)
	}
	if DefaultWakeGap != 30*time.Second {
		t.Errorf("DefaultWakeGap = %v, want 30s (spec §6)", DefaultWakeGap)
	}
}
