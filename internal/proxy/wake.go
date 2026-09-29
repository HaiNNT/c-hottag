package proxy

import (
	"context"
	"time"
)

// DefaultWakeInterval and DefaultWakeGap are spec §6's values: check every
// 5s, and treat more than 30s of unexplained wall-clock movement as a
// suspend.
const (
	DefaultWakeInterval = 5 * time.Second
	DefaultWakeGap      = 30 * time.Second
)

type WakerConfig struct {
	// Interval is how often to check. Default DefaultWakeInterval.
	Interval time.Duration
	// Gap is how much MORE wall-clock time than monotonic time must pass
	// between checks before the machine is judged to have slept. Default
	// DefaultWakeGap. The comparison is strictly greater than Gap, so a
	// gap exactly equal to it is not a wake.
	Gap time.Duration
	// Wall and Mono are the two clocks. The detector fires when the
	// monotonic clock has frozen through a suspend while the wall clock
	// kept moving — the real behaviour on both of chottag's targets
	// (Darwin's mach_absolute_time and Linux's CLOCK_MONOTONIC both
	// exclude suspended time, so this is not a hedge against uncertain
	// platform behaviour; it is what actually happens). It is safe in the
	// other direction too: if a platform's monotonic clock kept advancing
	// through the suspend instead, the two deltas would stay close and the
	// gap would never exceed Gap, so the detector simply would not fire —
	// no false wake, just a missed one on that platform. The remaining
	// failure mode is a wall-clock-only forward step with no monotonic
	// gap (e.g. an NTP correction over 30s), which fires a false-positive
	// wake; that costs only a round of closed idle upstream connections
	// and a token re-read, not a correctness bug. Defaults use time.Now.
	Wall func() time.Time
	Mono func() time.Duration
	// Ticks replaces the internal ticker, for tests. nil = a real ticker.
	Ticks <-chan time.Time
	// OnWake runs on the detector's own goroutine, so it must return
	// promptly. Required.
	OnWake func()
}

// Waker holds its comparison baseline as unexported fields, not a Run-local
// variable, so baseline and check below can be driven directly — one call
// each, on one goroutine — by a test, instead of needing a second goroutine
// (Run) synchronized against a fake clock. A test mutating a fake clock
// while Run reads it concurrently is a real, non-theoretical data race: an
// unbuffered channel send only orders the value it carries, not a later,
// separate call to Wall/Mono made after the receive.
type Waker struct {
	cfg      WakerConfig
	lastWall time.Time
	lastMono time.Duration
}

func NewWaker(cfg WakerConfig) *Waker {
	if cfg.Interval == 0 {
		cfg.Interval = DefaultWakeInterval
	}
	if cfg.Gap == 0 {
		cfg.Gap = DefaultWakeGap
	}
	if cfg.Wall == nil {
		// Round(0) strips the monotonic reading, leaving a pure wall
		// clock: without it, both cfg.Wall() and cfg.Mono() would carry a
		// monotonic reading, wall.Sub(lastWall) would track the SAME
		// monotonic clock as mono-lastMono, the two would stay in lock
		// step even through a real suspend, and the detector could never
		// fire in production. This is not a no-op tidy-up.
		cfg.Wall = func() time.Time { return time.Now().Round(0) }
	}
	if cfg.Mono == nil {
		start := time.Now()
		cfg.Mono = func() time.Duration { return time.Since(start) }
	}
	if cfg.OnWake == nil {
		// check calls cfg.OnWake with no nil guard: a caller that leaves
		// this unset (or a wiring change that drops the assignment)
		// crashes the first time a real wake actually fires, not at
		// construction where it would be caught immediately. Every other
		// field here gets a default; OnWake is the one a caller is most
		// likely to forget, since the zero value compiles and every test
		// that never triggers an actual wake stays green.
		cfg.OnWake = func() {}
	}
	return &Waker{cfg: cfg}
}

// baseline captures the starting point for check's comparisons.
func (w *Waker) baseline() {
	w.lastWall, w.lastMono = w.cfg.Wall(), w.cfg.Mono()
}

// check compares the current clocks against the last baseline, fires
// OnWake if the gap says the machine slept, and resets the baseline
// unconditionally, so one suspend fires exactly one wake.
func (w *Waker) check() {
	wall, mono := w.cfg.Wall(), w.cfg.Mono()
	// Unexplained wall-clock movement: time the wall clock recorded that
	// the monotonic clock did not.
	if wall.Sub(w.lastWall)-(mono-w.lastMono) > w.cfg.Gap {
		w.cfg.OnWake()
	}
	w.lastWall, w.lastMono = wall, mono
}

// Run checks for a suspend until ctx is cancelled.
func (w *Waker) Run(ctx context.Context) {
	ticks := w.cfg.Ticks
	if ticks == nil {
		t := time.NewTicker(w.cfg.Interval)
		defer t.Stop()
		ticks = t.C
	}
	w.baseline()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			w.check()
		}
	}
}
