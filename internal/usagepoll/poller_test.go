package usagepoll

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

var schedBase = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harness drives a Poller with a fake clock, a fake fetch and a fake
// cache. No goroutine, no timer, no network: runDue is called directly.
type harness struct {
	t     *testing.T
	clk   *fakeClock
	p     *Poller
	ctx   context.Context
	stop  context.CancelFunc
	log   bytes.Buffer
	views map[string]CacheView

	mu      sync.Mutex
	fetched []string // dirs, in order
	applied []string // account names, in order
	// fetch decides each fetch's outcome; default: a failed poll.
	fetch func(dir string) Outcome
	// apply decides each Apply's answer; default: written, unlimited.
	apply func(account string) Applied
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clk: &fakeClock{t: schedBase}, views: map[string]CacheView{}}
	h.ctx, h.stop = context.WithCancel(context.Background())
	t.Cleanup(h.stop)
	h.p = New(Config{
		Fetch: func(ctx context.Context, dir string) Outcome {
			h.mu.Lock()
			h.fetched = append(h.fetched, dir)
			n, f := len(h.fetched), h.fetch
			h.mu.Unlock()
			if n > 50 {
				// A scheduler that re-polls at once would spin forever at
				// a frozen clock: fail and stop it instead of hanging.
				t.Errorf("more than 50 fetches at %v: the scheduler is hot-looping", h.clk.Now())
				h.stop()
			}
			if f == nil {
				return Outcome{Status: "error"}
			}
			return f(dir)
		},
		Cached: func(account string, now time.Time) CacheView { return h.views[account] },
		Apply: func(account string, r Result, sent time.Time) Applied {
			h.mu.Lock()
			h.applied = append(h.applied, account)
			a := h.apply
			h.mu.Unlock()
			if a == nil {
				return Applied{Written: true}
			}
			return a(account)
		},
		Log: &h.log,
		Now: h.clk.Now,
		// No jitter by default: 0.5 maps to +0%.
		Rand: func() float64 { return 0.5 },
	})
	return h
}

func (h *harness) fetchedDirs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.fetched...)
}

func (h *harness) runDue() (time.Duration, bool) { return h.p.runDue(h.ctx) }

// pendingOf reads one account's pending poll.
func (h *harness) pendingOf(name string) (entry, bool) {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	st := h.p.accts[key(name)]
	if st == nil || st.pending == nil {
		return entry{}, false
	}
	return *st.pending, true
}

func (h *harness) wantPending(name string, due time.Time, k kind) {
	h.t.Helper()
	e, ok := h.pendingOf(name)
	if !ok || !e.due.Equal(due) || e.kind != k {
		h.t.Fatalf("%s pending = %+v (%v), want a %v poll due %v", name, e, ok, k, due)
	}
}

func (h *harness) wantNone(name string) {
	h.t.Helper()
	if e, ok := h.pendingOf(name); ok {
		h.t.Fatalf("%s pending = %+v, want none", name, e)
	}
}

func accts(names ...string) []Account {
	out := make([]Account, len(names))
	for i, n := range names {
		out[i] = Account{Name: n, Dir: "/slots/" + n, Rotates: true}
	}
	return out
}

func okOutcome() Outcome { return Outcome{OK: true, Status: "200"} }

func TestSyncRosterSeedsStartAndResetPolls(t *testing.T) {
	h := newHarness(t)
	h.views["B"] = CacheView{Fresh: false}
	h.views["C"] = CacheView{Fresh: true, UpdatedAt: schedBase.Add(-5 * time.Minute)}
	h.views["D"] = CacheView{Fresh: true, Limited: true, Until: schedBase.Add(time.Hour)}
	h.views["E"] = CacheView{Limited: true}
	h.views["F"] = CacheView{Limited: true, Until: schedBase.Add(-time.Hour)}
	h.p.SyncRoster(append(accts("A", "B", "C", "D", "E", "F"), Account{Name: "Home"}))

	h.wantPending("A", schedBase, kindStart)                             // no cache row at all
	h.wantPending("B", schedBase.Add(Stagger), kindStart)                // stale
	h.wantPending("C", schedBase.Add(IdleEvery-5*time.Minute), kindIdle) // fresh and unlimited: idle poll
	h.wantPending("D", schedBase.Add(time.Hour+Grace), kindReset)        // known future reset
	h.wantPending("E", schedBase.Add(2*Stagger), kindReset)              // reset unknown: check now
	h.wantPending("F", schedBase.Add(3*Stagger), kindReset)              // reset already passed
	if _, ok := h.p.accts[key("Home")]; ok {
		t.Fatal("an account with no slot dir joined the roster")
	}
}

// F175: a case-only rename keeps the account's pending poll (the key is
// the lowercased name) and takes the new spelling for what it writes.
func TestSyncRosterTakesACaseOnlyRenamesSpelling(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return okOutcome() }
	h.p.SyncRoster([]Account{{Name: "b", Dir: "/slots/b"}})
	h.p.SyncRoster([]Account{{Name: "B", Dir: "/slots/b"}})
	h.wantPending("B", schedBase, kindStart)
	h.runDue()
	h.mu.Lock()
	got := append([]string(nil), h.applied...)
	h.mu.Unlock()
	if len(got) != 1 || got[0] != "B" {
		t.Fatalf("applied %v, want one write under the new spelling \"B\"", got)
	}
}

func TestRunDueStaggersStartPolls(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return okOutcome() }
	h.p.SyncRoster(accts("A", "B", "C"))

	wait, ok := h.runDue()
	if got := h.fetchedDirs(); len(got) != 1 || got[0] != "/slots/A" || !ok || wait != Stagger {
		t.Fatalf("at start: fetched %v, next in %v (%v); want only A, next in %v", got, wait, ok, Stagger)
	}
	h.clk.Advance(Stagger)
	h.runDue()
	h.clk.Advance(Stagger)
	wait, ok = h.runDue()
	if got := h.fetchedDirs(); len(got) != 3 || got[1] != "/slots/B" || got[2] != "/slots/C" {
		t.Fatalf("fetched %v, want A, B, C two seconds apart", got)
	}
	if !ok || wait != IdleEvery-2*Stagger {
		t.Fatalf("next in %v (%v), want the first idle poll IdleEvery after A's update", wait, ok)
	}
	for _, n := range []string{"A", "B", "C"} {
		if e, ok := h.pendingOf(n); !ok || e.kind != kindIdle {
			t.Fatalf("%s pending = %+v (%v), want an idle poll after its result", n, e, ok)
		}
	}
	if lines := strings.Count(h.log.String(), "\n"); lines != 3 {
		t.Fatalf("log = %q, want one line per poll", h.log.String())
	}
}

func TestResetPollRunsAtResetPlusGraceAndFollowsANewReset(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Fresh: true, Limited: true, Until: schedBase.Add(time.Hour)}
	h.p.SyncRoster(accts("D"))
	if wait, ok := h.runDue(); !ok || wait != time.Hour+Grace || len(h.fetchedDirs()) != 0 {
		t.Fatalf("next in %v (%v), fetched %v; want nothing yet, next in 1h1m", wait, ok, h.fetchedDirs())
	}
	h.clk.Advance(time.Hour + Grace)
	newReset := schedBase.Add(5 * time.Hour)
	h.fetch = func(string) Outcome { return okOutcome() }
	h.apply = func(string) Applied { return Applied{Written: true, Limited: true, Until: newReset} }
	h.runDue()
	if len(h.fetchedDirs()) != 1 {
		t.Fatalf("fetched %v, want one reset poll", h.fetchedDirs())
	}
	h.wantPending("D", newReset.Add(Grace), kindReset)
}

// TestAResetThatDidNotClearBacksOff: the server still says limited with a
// reset that has already passed. Rescheduling to reset+Grace would be in
// the past and poll again at once, forever.
func TestAResetThatDidNotClearBacksOff(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true, Until: schedBase.Add(-time.Minute)}
	h.p.SyncRoster(accts("D"))
	h.fetch = func(string) Outcome { return okOutcome() }
	h.apply = func(string) Applied { return Applied{Written: true, Limited: true, Until: schedBase.Add(-time.Minute)} }
	wait, ok := h.runDue()
	if got := len(h.fetchedDirs()); got != 1 || !ok || wait != backoff[0] {
		t.Fatalf("fetched %d, next in %v (%v); want one poll, then the first backoff step", got, wait, ok)
	}
}

func TestUnknownResetBacksOff15Then30Then60(t *testing.T) {
	h := newHarness(t)
	h.views["E"] = CacheView{Limited: true}
	h.p.SyncRoster(accts("E"))
	for i, want := range []time.Duration{15 * time.Minute, 30 * time.Minute, 60 * time.Minute, 60 * time.Minute} {
		h.runDue() // the poll fails (harness default)
		h.wantPending("E", h.clk.Now().Add(want), kindReset)
		h.clk.Advance(want)
		if got := len(h.fetchedDirs()); got != i+1 {
			t.Fatalf("step %d: %d fetches, want %d", i, got, i+1)
		}
	}
	// A success that is still limited with no reset keeps backing off too.
	h.fetch = func(string) Outcome { return okOutcome() }
	h.apply = func(string) Applied { return Applied{Written: true, Limited: true} }
	h.runDue()
	h.wantPending("E", h.clk.Now().Add(60*time.Minute), kindReset)
}

func TestAFailedStartPollJoinsTheIdleSchedule(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return Outcome{Status: "429"} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	// A 429 without Retry-After backs off 60 minutes, not the usual 30.
	h.wantPending("A", schedBase.Add(60*time.Minute), kindIdle)
	if len(h.applied) != 0 {
		t.Fatal("a failed poll reached the cache")
	}
}

// TestAStartPollSkippedForNoTokenRetriesOnce: the first attempt is skipped
// because the token was refreshing; the retry fires no earlier than
// +TokenRetry, succeeds, and nothing polls again after that (F159).
func TestAStartPollSkippedForNoTokenRetriesOnce(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.fetch = func(string) Outcome {
		calls++
		if calls == 1 {
			return Outcome{Status: "no-token", NoToken: true}
		}
		return okOutcome()
	}
	h.p.SyncRoster(accts("A"))
	wait, ok := h.runDue()
	if got := len(h.fetchedDirs()); got != 1 || !ok || wait != TokenRetry {
		t.Fatalf("fetched %d, next in %v (%v); want one skipped attempt, next retry in %v", got, wait, ok, TokenRetry)
	}
	h.wantPending("A", schedBase.Add(TokenRetry), kindStart)

	// Not before +TokenRetry: the deadline has not passed yet.
	h.runDue()
	if got := len(h.fetchedDirs()); got != 1 {
		t.Fatalf("fetched %d before +TokenRetry, want still 1", got)
	}

	h.clk.Advance(TokenRetry)
	if wait, ok := h.runDue(); !ok || wait != IdleEvery {
		t.Fatalf("next in %v (%v), want the idle poll IdleEvery after the retry succeeded", wait, ok)
	}
	if got := len(h.fetchedDirs()); got != 2 {
		t.Fatalf("fetched %d, want exactly 2", got)
	}
	if lines := strings.Count(h.log.String(), " start "); lines != 2 {
		t.Fatalf("log = %q, want both poll lines to show kind start", h.log.String())
	}

	// A third attempt never happens after the OK, before the idle poll.
	h.clk.Advance(20 * time.Minute)
	h.runDue()
	if got := len(h.fetchedDirs()); got != 2 {
		t.Fatalf("fetched %d after advancing far past the retry, want still 2", got)
	}
}

// TestAStartPollRetriesNoTokenOnlyOnce: a second no-token gives up, exactly
// like an ordinary failed start poll, instead of retrying forever.
func TestAStartPollRetriesNoTokenOnlyOnce(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return Outcome{Status: "no-token", NoToken: true} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", schedBase.Add(TokenRetry), kindStart)
	h.clk.Advance(TokenRetry)
	h.runDue()
	h.wantPending("A", schedBase.Add(TokenRetry+IdleEvery), kindIdle)
	h.clk.Advance(20 * time.Minute)
	h.runDue()
	if got := len(h.fetchedDirs()); got != 2 {
		t.Fatalf("fetched %d, want exactly 2 attempts even hours later", got)
	}
}

// TestAResetPollSkippedForNoTokenRetriesOnceThenBacksOff: a reset poll
// skipped for no-token gets the same one-time retry a start poll gets
// (final review), so a limited account is not stuck for a full 15-minute backoff
// step just because the retry raced a background refresh. A second
// no-token on it backs off 15 minutes like an ordinary failed reset poll.
func TestAResetPollSkippedForNoTokenRetriesOnceThenBacksOff(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true}
	h.fetch = func(string) Outcome { return Outcome{Status: "no-token", NoToken: true} }
	h.p.SyncRoster(accts("D"))
	h.runDue()
	h.wantPending("D", schedBase.Add(TokenRetry), kindReset)
	if got := len(h.fetchedDirs()); got != 1 {
		t.Fatalf("fetched %d, want exactly 1 before the retry", got)
	}

	h.clk.Advance(TokenRetry)
	h.runDue()
	h.wantPending("D", h.clk.Now().Add(backoff[0]), kindReset)
	if got := len(h.fetchedDirs()); got != 2 {
		t.Fatalf("fetched %d, want exactly 2 attempts before backing off", got)
	}
}

// TestAnObservationCancelsTheNoTokenRetry: traffic observed before the
// +TokenRetry deadline cancels the pending retry, like it cancels any
// other pending start poll.
func TestAnObservationCancelsTheNoTokenRetry(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return Outcome{Status: "no-token", NoToken: true} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", schedBase.Add(TokenRetry), kindStart)

	h.p.Observed("A", false, time.Time{})
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle)

	h.clk.Advance(TokenRetry)
	h.runDue()
	if got := len(h.fetchedDirs()); got != 1 {
		t.Fatalf("fetched %d after the observation cancelled the retry, want still 1", got)
	}
}

func TestAStartPollThatFindsALimitSchedulesTheReset(t *testing.T) {
	h := newHarness(t)
	reset := schedBase.Add(3 * time.Hour)
	h.fetch = func(string) Outcome { return okOutcome() }
	h.apply = func(string) Applied { return Applied{Written: true, Limited: true, Until: reset} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", reset.Add(Grace), kindReset)
}

func TestA401GiveUpWaitsForTheNextEvent(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true}
	h.fetch = func(string) Outcome { return Outcome{Status: "401>401", GaveUp: true} }
	h.p.SyncRoster(accts("D"))
	if _, ok := h.runDue(); ok {
		t.Fatal("a reset poll was rescheduled after a 401 gave up")
	}
	h.wantNone("D")
	// The next event: a wake re-queues it, because its reset is unknown.
	h.p.Wake()
	h.wantPending("D", schedBase, kindReset)
}

func TestObservedCancelsNoDataPollAndMovesResetPoll(t *testing.T) {
	h := newHarness(t)
	h.p.SyncRoster(accts("A"))
	h.wantPending("A", schedBase, kindStart)

	h.p.Observed("A", false, time.Time{})
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle)

	until := schedBase.Add(2 * time.Hour)
	h.p.Observed("a", true, until) // case-folded, like every account lookup
	h.wantPending("A", until.Add(Grace), kindReset)

	h.p.Observed("A", true, time.Time{}) // same limit, reset unknown: keep the reset poll
	h.wantPending("A", until.Add(Grace), kindReset)

	h.p.Observed("A", false, time.Time{})
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle)
	h.p.Observed("A", true, time.Time{}) // limited, reset unknown: the idle entry gives way to the backoff
	h.wantPending("A", schedBase.Add(backoff[0]), kindReset)
	h.p.Observed("A", false, time.Time{})
	h.p.mu.Lock()
	h.p.accts["a"].pending = nil
	h.p.mu.Unlock()
	h.p.Observed("A", true, time.Time{}) // limited, reset unknown, nothing pending: back off
	h.wantPending("A", schedBase.Add(backoff[0]), kindReset)

	h.p.Observed("Home", true, until) // not on the roster: ignored, no panic
}

// TestAnObservationDuringAPollWins: the observe lands while the fetch is
// in flight. Its decision stands; the finishing poll schedules nothing.
func TestAnObservationDuringAPollWins(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome {
		h.p.Observed("A", false, time.Time{})
		return okOutcome()
	}
	h.apply = func(string) Applied { return Applied{Written: true, Limited: true, Until: schedBase.Add(time.Hour)} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle) // Observed's own schedule
}

func TestADroppedPollIsNotRescheduled(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true}
	h.fetch = func(string) Outcome { return okOutcome() }
	h.apply = func(string) Applied { return Applied{Written: false, Limited: true} }
	h.p.SyncRoster(accts("D"))
	h.runDue()
	h.wantNone("D")
}

func TestWakeRequeuesOnlyPassedResets(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true, Until: schedBase.Add(time.Hour)}
	h.views["G"] = CacheView{Limited: true, Until: schedBase.Add(9 * time.Hour)}
	h.p.SyncRoster(accts("D", "G"))
	// Both give up on a 401 at their reset poll, leaving nothing pending.
	h.fetch = func(string) Outcome { return Outcome{Status: "401>401", GaveUp: true} }
	h.clk.Advance(time.Hour + Grace)
	h.runDue()
	h.wantNone("D")
	h.p.mu.Lock()
	h.p.accts["g"].pending = nil
	h.p.mu.Unlock()

	h.clk.Advance(3 * time.Hour) // asleep past D's reset, not G's
	h.p.Wake()
	h.wantPending("D", h.clk.Now(), kindReset)
	h.wantNone("G")
}

// TestWakeDoesNotRequeueAResetPollParkedInFlight: Wake's own !st.inflight
// guard must hold while a reset poll is running, even with its reset past
// or unknown (which would otherwise make Wake re-queue it). Without that
// guard, Wake would sneak in a second, immediate pending entry behind
// finish's back, so the poll that is already running gets duplicated
// instead of following its ordinary backoff once it completes.
func TestWakeDoesNotRequeueAResetPollParkedInFlight(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true} // reset unknown: due immediately
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.fetch = func(string) Outcome {
		once.Do(func() { close(entered) })
		<-release // closed once, so a duplicate call does not block
		return Outcome{Status: "error"}
	}
	h.p.SyncRoster(accts("D"))
	runDone := make(chan struct{})
	go func() { h.runDue(); close(runDone) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never started")
	}

	// D is limited with its reset unknown, so Wake would otherwise queue a
	// second, immediate reset poll for it right now.
	h.p.Wake()

	close(release)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("runDue did not return after release")
	}

	if got := h.fetchedDirs(); len(got) != 1 {
		t.Fatalf("fetched %v, want exactly one fetch: the parked poll, with Wake a no-op while it was inflight", got)
	}
	h.wantPending("D", h.clk.Now().Add(backoff[0]), kindReset)
}

// TestAPollDueDuringSleepRunsOnWake: a monotonic timer does not advance in
// sleep, so the wall-clock deadline is what makes it due.
func TestAPollDueDuringSleepRunsOnWake(t *testing.T) {
	h := newHarness(t)
	h.views["D"] = CacheView{Limited: true, Until: schedBase.Add(time.Hour)}
	h.p.SyncRoster(accts("D"))
	h.runDue()
	h.clk.Advance(5 * time.Hour)
	h.p.Wake()
	h.runDue()
	if got := h.fetchedDirs(); len(got) != 1 {
		t.Fatalf("fetched %v after waking past the reset, want the reset poll", got)
	}
}

func TestRosterRemoveCancelsAndAddSeeds(t *testing.T) {
	h := newHarness(t)
	h.views["A"] = CacheView{Fresh: true, UpdatedAt: schedBase}
	h.p.SyncRoster(accts("A", "B"))
	h.wantPending("B", schedBase, kindStart)
	h.clk.Advance(time.Minute)
	h.p.SyncRoster(accts("A", "C"))
	if _, ok := h.p.accts["b"]; ok {
		t.Fatal("a removed account stayed on the roster")
	}
	h.wantPending("C", h.clk.Now(), kindStart)
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle) // an existing account is not re-seeded
}

func TestAnAccountRemovedDuringItsPollIsNotWritten(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome {
		h.p.SyncRoster(nil)
		return okOutcome()
	}
	h.p.SyncRoster(accts("A"))
	h.runDue()
	if len(h.applied) != 0 {
		t.Fatal("a poll for a removed account was written to the cache")
	}
}

func TestStopDropsTheInFlightPoll(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome {
		h.stop() // the daemon stops mid-request
		return okOutcome()
	}
	h.p.SyncRoster(accts("A", "B"))
	h.runDue()
	if len(h.applied) != 0 || h.log.Len() != 0 {
		t.Fatalf("applied %v, log %q: a poll finished after stop must write and log nothing", h.applied, h.log.String())
	}
	if got := h.fetchedDirs(); len(got) != 1 {
		t.Fatalf("fetched %v, want the worker to stop after the dropped poll", got)
	}
}

// TestStopDoesNotWaitOnABlockedFetch: a cancelled ctx must drop an
// in-flight poll at once, not wait for Fetch to return. On a 401, the
// production Fetch calls ForceRefresh, which runs with
// context.WithoutCancel and its own 2-minute timeout (deliberately, to
// protect token rotation): if poll waited on Fetch itself, daemon stop
// would wait up to 2 minutes past its SIGKILL deadline instead of
// dropping the request. This Fetch ignores ctx entirely and blocks until
// released, standing in for that worst case.
func TestStopDoesNotWaitOnABlockedFetch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	applyCalls := 0
	var logBuf bytes.Buffer
	p := New(Config{
		Fetch: func(ctx context.Context, dir string) Outcome {
			close(entered)
			<-release
			return okOutcome()
		},
		Cached: func(string, time.Time) CacheView { return CacheView{} },
		Apply: func(string, Result, time.Time) Applied {
			mu.Lock()
			applyCalls++
			mu.Unlock()
			return Applied{Written: true}
		},
		Log: &logBuf,
		Now: func() time.Time { return schedBase },
		// Never fires: Run must return via ctx.Done, not this timer.
		After: func(time.Duration) <-chan time.Time { return make(chan time.Time) },
	})
	p.SyncRoster(accts("A"))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { p.Run(ctx); close(stopped) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never started")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s: it waited on the blocked Fetch instead of dropping it on stop")
	}

	mu.Lock()
	n := applyCalls
	mu.Unlock()
	if n != 0 {
		t.Fatalf("Apply called %d times, want 0: nothing is written for a dropped poll", n)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("log = %q, want empty: nothing is logged for a dropped poll", logBuf.String())
	}

	close(release) // let the abandoned Fetch goroutine finish, so nothing leaks
}

func TestPollLogsOneLinePerPoll(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return Outcome{Status: "401>200", OK: true} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	if got, want := h.log.String(), "chottag: usage poll A start 401>200 0s\n"; got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

// TestObservedNeverWaitsForAnInFlightPoll: Observed runs under the status
// sink's lock on the response path, so it must not queue behind a
// 10-second fetch (F69's lesson: the hook is a liveness requirement).
func TestObservedNeverWaitsForAnInFlightPoll(t *testing.T) {
	h := newHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	h.fetch = func(string) Outcome {
		close(entered)
		<-release
		return Outcome{Status: "error"}
	}
	h.p.SyncRoster(accts("A", "B"))
	runDone := make(chan struct{})
	go func() { h.runDue(); close(runDone) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the poll never started")
	}
	done := make(chan struct{})
	go func() {
		h.p.Observed("B", true, schedBase.Add(time.Hour))
		h.p.Wake()
		h.p.SyncRoster(accts("A", "B"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Observed, Wake or SyncRoster blocked behind an in-flight fetch")
	}
	close(release)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("runDue did not return after release")
	}
}

// TestRunWaitsForTheNextDueAndRecomputesOnAnEvent drives the real Run
// loop with a fake timer: After records each wait and returns a channel
// the test fires.
func TestRunWaitsForTheNextDueAndRecomputesOnAnEvent(t *testing.T) {
	clk := &fakeClock{t: schedBase}
	waits := make(chan time.Duration, 8)
	fire := make(chan time.Time)
	fetched := make(chan string, 8)
	p := New(Config{
		Fetch: func(_ context.Context, dir string) Outcome { fetched <- dir; return okOutcome() },
		Cached: func(string, time.Time) CacheView {
			return CacheView{Fresh: true, Limited: true, Until: schedBase.Add(time.Hour)}
		},
		Apply: func(string, Result, time.Time) Applied { return Applied{Written: true} },
		Now:   clk.Now,
		After: func(d time.Duration) <-chan time.Time { waits <- d; return fire },
	})
	p.SyncRoster(accts("D"))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { p.Run(ctx); close(stopped) }()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}()

	nextWait := func() time.Duration {
		t.Helper()
		select {
		case d := <-waits:
			return d
		case <-time.After(5 * time.Second):
			t.Fatal("Run never armed its timer")
			return 0
		}
	}
	if d := nextWait(); d != time.Hour+Grace {
		t.Fatalf("first wait = %v, want 1h1m", d)
	}
	p.Observed("D", true, schedBase.Add(2*time.Hour))
	// SyncRoster's own nudge may still be queued, re-arming 1h1m once
	// more before Run sees the observe. Any other value is wrong.
	for d := nextWait(); d != 2*time.Hour+Grace; d = nextWait() {
		if d != time.Hour+Grace {
			t.Fatalf("wait after the observe = %v, want 2h1m", d)
		}
	}
	clk.Advance(2*time.Hour + Grace)
	select {
	case fire <- clk.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("Run was not waiting on its timer")
	}
	select {
	case dir := <-fetched:
		if dir != "/slots/D" {
			t.Fatalf("fetched %q", dir)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the due poll never ran")
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	fetch := func(context.Context, string) Outcome { return Outcome{} }
	cached := func(string, time.Time) CacheView { return CacheView{} }
	apply := func(string, Result, time.Time) Applied { return Applied{} }
	for name, c := range map[string]Config{
		"no fetch":  {Cached: cached, Apply: apply},
		"no cached": {Fetch: fetch, Apply: apply},
		"no apply":  {Fetch: fetch, Cached: cached},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: New did not panic", name)
				}
			}()
			New(c)
		}()
	}
}

// jitterHigh and jitterLow are Rand values at the two ends of the +-10%
// spread.
func TestIdlePollIsDueAnIntervalAfterTheUpdateWithJitter(t *testing.T) {
	for _, tc := range []struct {
		rand float64
		want time.Duration
	}{{0, 27 * time.Minute}, {0.5, 30 * time.Minute}, {1, 33 * time.Minute}} {
		h := newHarness(t)
		h.p.cfg.Rand = func() float64 { return tc.rand }
		h.views["A"] = CacheView{Fresh: true, UpdatedAt: schedBase}
		h.p.SyncRoster(accts("A"))
		h.wantPending("A", schedBase.Add(tc.want), kindIdle)
	}
}

func TestRotationOffAccountIsPolledEveryTwoHours(t *testing.T) {
	h := newHarness(t)
	h.views["R"] = CacheView{Fresh: true, UpdatedAt: schedBase}
	h.p.SyncRoster([]Account{{Name: "R", Dir: "/slots/R"}})
	h.wantPending("R", schedBase.Add(OffEvery), kindIdle)
}

func TestIdlePollsAreStaggeredAndRescheduledAfterAResult(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return okOutcome() }
	for _, n := range []string{"A", "B", "C"} {
		h.views[n] = CacheView{Fresh: true, UpdatedAt: schedBase}
	}
	h.p.SyncRoster(accts("A", "B", "C"))
	due := map[string]time.Time{}
	for _, n := range []string{"A", "B", "C"} {
		e, _ := h.pendingOf(n)
		due[n] = e.due
	}
	for _, x := range []string{"A", "B", "C"} {
		for _, y := range []string{"A", "B", "C"} {
			if x < y {
				if d := due[x].Sub(due[y]); d > -Stagger && d < Stagger {
					t.Fatalf("%s and %s are due %v apart, want at least %v", x, y, d.Abs(), Stagger)
				}
			}
		}
	}
	h.clk.Advance(IdleEvery + 10*time.Second)
	h.runDue()
	if got := len(h.fetchedDirs()); got != 3 {
		t.Fatalf("fetched %d, want the three idle polls", got)
	}
	// Each result schedules the next idle poll IdleEvery later.
	for _, n := range []string{"A", "B", "C"} {
		if e, ok := h.pendingOf(n); !ok || e.kind != kindIdle || e.due.Before(h.clk.Now().Add(IdleEvery-time.Minute)) {
			t.Fatalf("%s pending = %+v (%v), want another idle poll about IdleEvery away", n, e, ok)
		}
	}
	if !strings.Contains(h.log.String(), " idle 200 ") {
		t.Fatalf("log = %q, want idle polls logged as kind idle", h.log.String())
	}
}

func TestIdle429BacksOffSixtyThenOneTwentyAndHonoursRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.views["A"] = CacheView{Fresh: true, UpdatedAt: schedBase.Add(-time.Hour)}
	h.fetch = func(string) Outcome { return Outcome{Status: "429"} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", schedBase.Add(60*time.Minute), kindIdle)
	h.clk.Advance(60 * time.Minute)
	h.runDue()
	h.wantPending("A", h.clk.Now().Add(120*time.Minute), kindIdle)
	h.clk.Advance(120 * time.Minute)
	h.runDue()
	h.wantPending("A", h.clk.Now().Add(120*time.Minute), kindIdle) // stays at 120

	h.fetch = func(string) Outcome { return Outcome{Status: "429", RetryAfter: 7 * time.Minute} }
	h.clk.Advance(120 * time.Minute)
	h.runDue()
	h.wantPending("A", h.clk.Now().Add(7*time.Minute), kindIdle)

	// Any other failure waits the ordinary interval.
	h.fetch = func(string) Outcome { return Outcome{Status: "timeout"} }
	h.clk.Advance(7 * time.Minute)
	h.runDue()
	h.wantPending("A", h.clk.Now().Add(IdleEvery), kindIdle)
}

func TestNeedsLoginStopsPollingAndAnnouncesOnce(t *testing.T) {
	h := newHarness(t)
	var told []string
	h.p.cfg.OnNeedsLogin = func(a string) {
		told = append(told, a)
		// What the daemon does: the status row says needs-login from now.
		h.views[a] = CacheView{NeedsLogin: true, TokenAt: h.clk.Now()}
	}
	h.fetch = func(string) Outcome { return Outcome{Status: "no-token", NoToken: true, NeedsLogin: true} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.clk.Advance(24 * time.Hour)
	h.p.Wake()
	h.runDue()
	h.p.SyncRoster(accts("A")) // the roster tick: the row still says nothing is wrong
	h.runDue()
	if got := h.fetchedDirs(); len(got) != 1 {
		t.Fatalf("fetched %v, want the one poll that found the missing login", got)
	}
	if len(told) != 1 || told[0] != "A" {
		t.Fatalf("OnNeedsLogin calls = %v, want exactly one for A", told)
	}
	h.wantNone("A")
}

func TestNeedsLoginAccountPollsAgainAfterTheRowClearsOrALogin(t *testing.T) {
	h := newHarness(t)
	h.p.cfg.OnNeedsLogin = func(string) {}
	h.fetch = func(string) Outcome { return Outcome{Status: "no-token", NoToken: true, NeedsLogin: true} }
	h.p.SyncRoster(accts("A", "B"))
	h.runDue()
	h.clk.Advance(Stagger)
	h.runDue()
	h.wantNone("A")
	h.wantNone("B")

	// The status row says needs-login since T; a login after T lifts it.
	tokenAt := h.clk.Now()
	h.views["A"] = CacheView{NeedsLogin: true, TokenAt: tokenAt}
	h.views["B"] = CacheView{NeedsLogin: true, TokenAt: tokenAt}
	h.p.SyncRoster(accts("A", "B"))
	h.wantNone("A")
	h.wantNone("B")
	loggedIn := []Account{{Name: "A", Dir: "/slots/A", Rotates: true, LoggedInAt: tokenAt.Add(time.Minute)}, {Name: "B", Dir: "/slots/B", Rotates: true}}
	h.p.SyncRoster(loggedIn)
	if _, ok := h.pendingOf("A"); !ok {
		t.Fatal("A was not polled again after logging in")
	}
	h.wantNone("B")

	// B's row clears (a refresh renewed it): polled again too.
	h.views["B"] = CacheView{}
	h.p.SyncRoster(loggedIn)
	if _, ok := h.pendingOf("B"); !ok {
		t.Fatal("B was not polled again after its needs-login mark cleared")
	}
}

func TestSeedNeverPollsAnAccountTheRowSaysNeedsLogin(t *testing.T) {
	h := newHarness(t)
	h.views["A"] = CacheView{NeedsLogin: true, TokenAt: schedBase}
	h.p.SyncRoster(accts("A"))
	h.wantNone("A")
	if h.p.PollNow(h.ctx, "A") {
		t.Fatal("PollNow polled a needs-login account")
	}
}

func TestPollNowRunsAheadOfScheduleAndReportsTheResult(t *testing.T) {
	h := newHarness(t)
	h.fetch = func(string) Outcome { return okOutcome() }
	h.views["A"] = CacheView{Fresh: true, UpdatedAt: schedBase}
	h.p.SyncRoster(accts("A"))
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle)

	got := make(chan bool, 1)
	go func() { got <- h.p.PollNow(h.ctx, "A") }()
	// PollNow queues the poll and wakes Run; drive the worker by hand once
	// the poll is queued.
	for {
		<-h.p.kick
		if e, ok := h.pendingOf("A"); ok && e.due.Equal(schedBase) {
			break
		}
	}
	h.runDue()
	if !<-got {
		t.Fatal("PollNow = false, want true for a poll that parsed")
	}
	if n := len(h.fetchedDirs()); n != 1 {
		t.Fatalf("fetched %d, want 1", n)
	}
	h.wantPending("A", schedBase.Add(IdleEvery), kindIdle)

	if h.p.PollNow(h.ctx, "Nobody") {
		t.Fatal("PollNow polled an account that is not on the roster")
	}
}

func TestPollNowReturnsFalseWhenTheContextEnds(t *testing.T) {
	h := newHarness(t)
	h.p.SyncRoster(accts("A"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.p.PollNow(ctx, "A") {
		t.Fatal("PollNow = true with a cancelled context and no poll run")
	}
}

// M3: a pre-switch poll never overrides a 429 backoff (Retry-After included).
func TestPollNowHonoursA429Backoff(t *testing.T) {
	h := newHarness(t)
	h.views["A"] = CacheView{Fresh: true, UpdatedAt: schedBase.Add(-time.Hour)}
	h.fetch = func(string) Outcome { return Outcome{Status: "429", RetryAfter: 40 * time.Minute} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", schedBase.Add(40*time.Minute), kindIdle)
	if h.p.PollNow(h.ctx, "A") {
		t.Fatal("PollNow = true during a Retry-After backoff")
	}
	h.wantPending("A", schedBase.Add(40*time.Minute), kindIdle) // untouched
	if n := len(h.fetchedDirs()); n != 1 {
		t.Fatalf("fetched %d, want only the original poll", n)
	}
}

func TestPollNowSkipsALimitedAccount(t *testing.T) {
	h := newHarness(t)
	h.views["A"] = CacheView{Fresh: true, Limited: true, Until: schedBase.Add(time.Hour)}
	h.p.SyncRoster(accts("A"))
	if h.p.PollNow(h.ctx, "A") {
		t.Fatal("PollNow polled a limited account")
	}
	h.wantPending("A", schedBase.Add(time.Hour+Grace), kindReset)
}

// A 401 that survived a refresh costs a claude spawn per retry: an idle poll
// backs off like a 429.
func TestIdlePollThatGaveUpBacksOffLikeA429(t *testing.T) {
	h := newHarness(t)
	h.views["A"] = CacheView{Fresh: true, UpdatedAt: schedBase.Add(-time.Hour)}
	h.fetch = func(string) Outcome { return Outcome{Status: "401 refresh-failed", GaveUp: true} }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	h.wantPending("A", schedBase.Add(60*time.Minute), kindIdle)
	h.clk.Advance(60 * time.Minute)
	h.runDue()
	h.wantPending("A", h.clk.Now().Add(120*time.Minute), kindIdle)
}

func TestOnOKIsToldOfAPollThatWroteUsage(t *testing.T) {
	h := newHarness(t)
	var got []string
	h.p.SetOnOK(func(a string) { got = append(got, a) })
	h.fetch = func(string) Outcome { return okOutcome() }
	h.p.SyncRoster(accts("A"))
	h.runDue()
	if len(got) != 1 || got[0] != "A" {
		t.Fatalf("OnOK calls = %v, want one for A", got)
	}
	h.fetch = func(string) Outcome { return Outcome{Status: "429"} }
	h.clk.Advance(IdleEvery + time.Minute)
	h.runDue()
	if len(got) != 1 {
		t.Fatalf("OnOK calls = %v after a failed poll, want still one", got)
	}
}

func TestPollSkipSaysWhy(t *testing.T) {
	h := newHarness(t)
	h.views["Lim"] = CacheView{Fresh: true, Limited: true, Until: schedBase.Add(time.Hour)}
	h.views["Bad"] = CacheView{NeedsLogin: true, TokenAt: schedBase}
	h.views["Back"] = CacheView{Fresh: true, UpdatedAt: schedBase.Add(-time.Hour)}
	h.fetch = func(string) Outcome { return Outcome{Status: "429"} }
	h.p.SyncRoster(accts("Lim", "Bad", "Back", "Ok"))
	h.runDue() // Back's idle poll fails with a 429
	for name, want := range map[string]string{"Lim": "limited", "Bad": "needs login", "Back": "in a 429 backoff", "Nobody": "not on the roster"} {
		if got := h.p.PollSkip(name); got != want {
			t.Errorf("PollSkip(%s) = %q, want %q", name, got, want)
		}
	}
}
