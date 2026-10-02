package tokens_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

const slot = "/slots/C"

type fakeSlot struct {
	mu         sync.Mutex
	tok        creds.Token
	err        error
	reads      int
	refreshes  int
	refreshErr error
	// onRefresh mutates the slot the way a real refresh would. It runs
	// with f.mu held, so it must not call f's own accessors.
	onRefresh func(f *fakeSlot)
	// refreshFunc, when set, replaces the default refresh behaviour
	// entirely and receives the context the manager passed down — which is
	// how a test observes whether a refresh dies with its caller (F37).
	// It runs WITHOUT f.mu held, so it may use the accessors freely.
	refreshFunc func(ctx context.Context) error
}

func (f *fakeSlot) read(string) (creds.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.tok, f.err
}

func (f *fakeSlot) Refresh(ctx context.Context, _ string) error {
	f.mu.Lock()
	f.refreshes++
	fn := f.refreshFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onRefresh != nil {
		f.onRefresh(f)
	}
	return f.refreshErr
}

func (f *fakeSlot) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *fakeSlot) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

// setToken replaces the slot's credential the way a successful refresh
// would, from outside the manager.
func (f *fakeSlot) setToken(tk creds.Token) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tok = tk
}

// token reads the slot's current credential under the mutex. Since Task 2
// a refresh can run on its own goroutine, so any test that triggers one
// and then wants to see the result must use this instead of reading f.tok
// directly — a direct read races the goroutine that may still be writing
// it.
func (f *fakeSlot) token() creds.Token {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tok
}

// setOnRefresh replaces the slot's onRefresh hook under the mutex. Refresh
// releases f.mu right after incrementing refreshes and before
// re-acquiring it to read onRefresh, so a test that waits for
// refreshCount() to become visible and then does a bare `f.onRefresh = ...`
// has no happens-before relationship with that later read: it is a real
// data race, just a narrow one. Any test that reassigns onRefresh after an
// attempt is already under way must go through this instead.
func (f *fakeSlot) setOnRefresh(fn func(f *fakeSlot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRefresh = fn
}

func newManager(f *fakeSlot, now *time.Time, locked bool) *tokens.Manager {
	return tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			if locked {
				return nil, false, nil
			}
			return func() error { return nil }, true, nil
		},
		Now: func() time.Time { return *now },
	})
}

// syncClock is a goroutine-safe wall-clock stand-in. Task 2 made refreshes
// run on their own goroutine, and both doRefresh and refreshDetached's
// panic recovery call Config.Now again near the very end of an attempt —
// after waitFor's refreshCount()==N has already gone true. A test that
// then advances a plain *time.Time (as newManager above uses) races that
// still-running goroutine; syncClock removes the race for the handful of
// tests that need to advance the clock after triggering an async refresh.
type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func newSyncClock(t time.Time) *syncClock {
	return &syncClock{t: t}
}

func (c *syncClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *syncClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newManagerClock is newManager backed by a syncClock instead of a plain
// *time.Time.
func newManagerClock(f *fakeSlot, clk *syncClock, locked bool) *tokens.Manager {
	return tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			if locked {
				return nil, false, nil
			}
			return func() error { return nil }, true, nil
		},
		Now: clk.now,
	})
}

func tok(exp time.Time) creds.Token {
	return creds.Token{AccessToken: "tok-" + exp.Format("150405"), ExpiresAt: exp}
}

// waitFor polls cond for up to two seconds. Task 2 makes a refresh happen
// on its own goroutine, so a test can no longer assert a counter the
// instant Token returns; it must wait for the effect instead.
//
// Never write cond as an exact-equality check on a counter the poll loop
// itself can advance — e.g. `f.refreshCount() == N` where cond also calls
// m.Token, which can spawn another refresh if the previous one has
// already cleared s.refreshing. fakeSlot.Refresh increments the counter
// at its own top, so if a just-spawned attempt's goroutine wins the race
// against this loop's own read, the observable value can step straight
// past N (e.g. 1 to 3) and `== N` becomes permanently unsatisfiable: every
// remaining iteration re-checks a condition that can never hold again,
// and the test times out at exactly the deadline rather than failing
// early. That deadline-exact failure is the tell — a genuinely slow
// refresh still eventually makes cond() true and returns well inside the
// bound, so a timeout that lands on the nose is evidence of an
// unsatisfiable condition, not a slow one, and raising the deadline
// cannot fix it (it only makes the same failure take longer to report).
// Prefer `>= N` ("at least N attempts happened") for any cond that
// repeats the triggering call.
func waitFor(t *testing.T, want string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", want)
}

// stays is waitFor's negative counterpart: it polls cond for d and fails
// the instant cond turns false, instead of checking once. A single
// immediate check right after a waitFor(refreshCount()==N) is a race the
// triggering goroutine almost always wins before anything else can run, so
// it silently proves nothing about whether the production code actually
// held the count steady (e.g. via a backoff) — only that nothing else
// happened to run in the few nanoseconds before the check. stays makes
// that window long enough to be a real assertion.
func stays(t *testing.T, want string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s stopped holding", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTokenServesAFreshTokenAndCachesTheRead(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	m := newManager(f, &now, false)
	for i := 0; i < 3; i++ {
		got, st, ok := m.Token(context.Background(), slot)
		if !ok || got != f.token().AccessToken || st.State != creds.StateOK {
			t.Fatalf("Token = %q, %+v, %v", got, st, ok)
		}
	}
	if f.readCount() != 1 {
		t.Fatalf("reads = %d, want 1 (cached within ReadTTL)", f.readCount())
	}
	if f.refreshCount() != 0 {
		t.Fatalf("refreshed a fresh token %d times", f.refreshCount())
	}
}

func TestExpiredTokenIsRefreshedUnderTheSlotLock(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(now.Add(time.Hour)) }
	m := newManager(f, &now, false)

	// The triggering call passes through by design (F37): the refresh runs
	// on its own goroutine instead of blocking this request.
	if _, st, ok := m.Token(context.Background(), slot); ok || st.State != creds.StateStale {
		t.Fatalf("triggering Token = %+v, %v; want stale pass-through while the refresh runs", st, ok)
	}

	waitFor(t, "the refreshed token to be served", func() bool {
		got, st, ok := m.Token(context.Background(), slot)
		return ok && st.State == creds.StateOK && got == f.token().AccessToken
	})
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshCount())
	}
}

func TestBusySlotLockPassesThroughWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	m := newManager(f, &now, true) // lock held by someone else
	_, st, ok := m.Token(context.Background(), slot)
	if ok || st.State != creds.StateStale {
		t.Fatalf("Token = %+v, %v; want stale pass-through", st, ok)
	}
	if f.refreshCount() != 0 {
		t.Fatal("refreshed while another holder had the slot lock")
	}
}

func TestFailedRefreshBacksOff(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute)), refreshErr: errors.New("network down")}
	m := newManagerClock(f, clk, false)
	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("failed refresh reported ok")
	}
	waitFor(t, "the failed refresh attempt", func() bool { return f.refreshCount() == 1 })

	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("second call reported ok")
	}
	stays(t, "refreshes == 1 (backoff)", 150*time.Millisecond, func() bool { return f.refreshCount() == 1 })

	clk.advance(31 * time.Second)
	waitFor(t, "a second refresh attempt after the backoff elapsed", func() bool {
		m.Token(context.Background(), slot)
		return f.refreshCount() == 2
	})
}

func TestNeedsLoginIsNeverRefreshed(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{err: creds.ErrNoLogin}
	m := newManager(f, &now, false)
	_, st, ok := m.Token(context.Background(), slot)
	if ok || st.State != creds.StateNeedsLogin {
		t.Fatalf("Token = %+v, %v", st, ok)
	}
	if f.refreshCount() != 0 {
		t.Fatal("refreshed a slot with no login")
	}
}

func TestStatusNeverRefreshes(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	m := newManager(f, &now, false)
	if st := m.Status(slot); st.State != creds.StateStale {
		t.Fatalf("Status = %+v", st)
	}
	if f.refreshCount() != 0 {
		t.Fatal("Status triggered a refresh")
	}
}

func TestBusySlotLockRetriesAtAFlatBackoffWithoutGrowing(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute))} // expired
	var mu sync.Mutex
	lockCalls := 0
	m := tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			mu.Lock()
			lockCalls++
			mu.Unlock()
			return nil, false, nil // permanently busy: someone else holds the lock
		},
		Now: clk.now,
	})
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return lockCalls
	}
	for i := 1; i <= 4; i++ {
		_, st, ok := m.Token(context.Background(), slot)
		if ok || st.State != creds.StateStale {
			t.Fatalf("tick %d: Token = %+v, %v; want stale pass-through", i, st, ok)
		}
		// The lock attempt happens on the detached refresh goroutine, so it
		// must be observed through waitFor, not read straight after Token
		// returns, before advancing the clock for the next tick — the
		// goroutine also reads the clock again (setting nextTry) after
		// lockCalls is already visible, which is exactly why this needs a
		// syncClock and not a plain *time.Time.
		waitFor(t, "the busy-lock attempt to register", func() bool { return calls() == i })
		clk.advance(31 * time.Second) // just past the 30s MinBackoff window
	}
	if f.refreshCount() != 0 {
		t.Fatal("refreshed while another holder had the slot lock")
	}
}

// ForceRefresh exists for the proxy safety net (proxy/safetynet.go): a
// swapped request was refused upstream even though the cached token still
// assesses ok/expiring locally (revoked, org changed, clock skew), so the
// safety net needs a real refresh, not a cache hit.
func TestForceRefreshRunsEvenWhenCachedTokenAssessesFine(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))} // valid for another hour
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(now.Add(2 * time.Hour)) }
	m := newManager(f, &now, false)

	// Token() must not refresh a token that still assesses ok.
	if _, st, ok := m.Token(context.Background(), slot); !ok || st.State != creds.StateOK {
		t.Fatalf("Token = %+v, %v", st, ok)
	}
	if f.refreshCount() != 0 {
		t.Fatalf("Token refreshed a token that assesses ok: %d", f.refreshCount())
	}

	got, ok := m.ForceRefresh(context.Background(), slot)
	if !ok || got != f.token().AccessToken {
		t.Fatalf("ForceRefresh = %q, %v", got, ok)
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1 (ForceRefresh must run the Refresher regardless of Assess)", f.refreshCount())
	}
}

// A 401 burst across several in-flight requests for the same slot must
// coalesce onto one refresh, not stampede the real claude binary once per
// request.
//
// The in-flight refresh is held open on release until every other
// concurrent caller has already returned, so this cannot rely on scheduling
// timing: the winner can only finish once we close release, so any result
// collected beforehand is provably a decline from a caller that found the
// refresh already in flight, not a second, independent refresh.
func TestForceRefreshCoalescesConcurrentCalls(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	f.onRefresh = func(f *fakeSlot) {
		<-release
		f.tok = tok(now.Add(2 * time.Hour))
	}
	m := newManager(f, &now, false)

	const n = 5
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() {
			_, ok := m.ForceRefresh(context.Background(), slot)
			results <- ok
		}()
	}
	for i := 0; i < n-1; i++ {
		if ok := <-results; ok {
			t.Fatal("a concurrent ForceRefresh call succeeded while another was still in flight")
		}
	}
	close(release)
	if ok := <-results; !ok {
		t.Fatal("the in-flight ForceRefresh call did not succeed")
	}

	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1 (concurrent ForceRefresh calls must coalesce)", f.refreshCount())
	}
}

// A busy slot lock (another holder mid-login) must retry ForceRefresh at a
// flat MinBackoff, never growing the backoff the way a genuine refresh
// failure does — the same Task 2 rule Token() already honors.
func TestForceRefreshOnBusyLockRetriesAtAFlatBackoff(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	lockCalls := 0
	m := tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			lockCalls++
			return nil, false, nil // permanently busy
		},
		Now: func() time.Time { return now },
	})
	for i := 1; i <= 3; i++ {
		if _, ok := m.ForceRefresh(context.Background(), slot); ok {
			t.Fatalf("tick %d: ForceRefresh reported ok while the slot lock was busy", i)
		}
		if lockCalls != i {
			t.Fatalf("tick %d: lockCalls = %d, want %d (flat MinBackoff never short-circuits a later tick)", i, lockCalls, i)
		}
		now = now.Add(31 * time.Second)
	}
	if f.refreshCount() != 0 {
		t.Fatal("refreshed while another holder had the slot lock")
	}
}

// A persistently-refused swapped route (spec §4.4) calls ForceRefresh on
// every single request. Without a throttle, a token that always assesses
// fine locally means every one of those calls succeeds, and success used to
// reset the shared backoff to zero — so the very next refusal would force a
// real `claude` subprocess again, forever, unthrottled.
func TestForceRefreshThrottlesRepeatedForcingAfterSuccess(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))} // always assesses fine locally
	m := newManager(f, &now, false)

	if _, ok := m.ForceRefresh(context.Background(), slot); !ok {
		t.Fatal("first ForceRefresh did not succeed")
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshCount())
	}

	// A second refusal on the very same route, an instant later: must not
	// spawn a second subprocess.
	if _, ok := m.ForceRefresh(context.Background(), slot); ok {
		t.Fatal("second ForceRefresh succeeded inside the cooldown window")
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes after the second call = %d, want still 1 (throttled)", f.refreshCount())
	}

	// Past the cooldown, forcing again is allowed once more.
	now = now.Add(31 * time.Second)
	if _, ok := m.ForceRefresh(context.Background(), slot); !ok {
		t.Fatal("ForceRefresh past the cooldown window did not succeed")
	}
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes after the cooldown = %d, want 2", f.refreshCount())
	}
}

// The ForceRefresh cooldown must never delay Token's own refresh of a
// genuinely expired token: they are gated by separate fields precisely so a
// route stuck forcing refreshes on a bad token does not also stall a
// different, real, expired-token refresh for the same slot.
func TestForceRefreshCooldownDoesNotDelayANaturalRefresh(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	m := tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			return func() error { return nil }, true, nil
		},
		Now:        func() time.Time { return now },
		ReadTTL:    time.Second,
		MinBackoff: 60 * time.Second, // long enough that ReadTTL elapses well inside it
	})

	if _, ok := m.ForceRefresh(context.Background(), slot); !ok {
		t.Fatal("ForceRefresh did not succeed")
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshCount())
	}

	// The token expires (revoked, clock skew, whatever) moments later, well
	// inside the 60s force cooldown but past the 1s read TTL.
	f.setToken(tok(now.Add(-time.Minute)))
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(now.Add(time.Hour)) }
	now = now.Add(2 * time.Second)

	if _, st, ok := m.Token(context.Background(), slot); ok || st.State != creds.StateStale {
		t.Fatalf("triggering Token = %+v, %v; want stale pass-through despite the force cooldown", st, ok)
	}
	waitFor(t, "a prompt natural refresh despite the force cooldown", func() bool {
		_, st, ok := m.Token(context.Background(), slot)
		return ok && st.State == creds.StateOK
	})
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d, want 2 (Token must refresh a genuinely stale token even inside the force cooldown)", f.refreshCount())
	}
}

// A panic inside the Refresher must not strand a slot as permanently
// "refreshing": without a defer resetting it, every future call for this
// slot for the life of the process would see s.refreshing still true and
// pass through instead of ever refreshing again.
//
// Since Task 2 moved the refresh onto its own goroutine (refreshDetached),
// the panic no longer propagates to this test's own stack — refreshDetached
// recovers it there instead (see
// TestPanickingRefresherDoesNotCrashTheProcessAndSetsABackoff for that
// guarantee directly) — so this test observes the recovery's effects
// through waitFor rather than wrapping m.Token in its own recover().
func TestPanicInRefreshDoesNotStrandTheSlotAsRefreshingForever(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute))} // stale, so Token enters doRefresh
	f.onRefresh = func(*fakeSlot) { panic("boom") }
	m := newManagerClock(f, clk, false)

	m.Token(context.Background(), slot)
	waitFor(t, "the panicking refresh attempt", func() bool { return f.refreshCount() == 1 })

	// refreshDetached's recover also starts the normal backoff (see the
	// guarantee test below), so the next attempt needs the clock to move
	// past it — not because s.refreshing is stuck true. Advancing it here
	// (rather than a plain *time.Time) is what keeps this race-free: the
	// recover handler reads the clock again after refreshCount is already
	// visible.
	clk.advance(31 * time.Second)
	// setOnRefresh, not a bare assignment: the first attempt's goroutine may
	// still be between incrementing refreshes and reading onRefresh when
	// refreshCount() == 1 becomes visible above, so an unguarded write here
	// would race it.
	f.setOnRefresh(func(f *fakeSlot) { f.tok = tok(clk.now().Add(time.Hour)) })
	waitFor(t, "a recovered refresh attempt after the panic", func() bool {
		m.Token(context.Background(), slot)
		return f.refreshCount() == 2
	})
	waitFor(t, "the refreshed token to be served", func() bool {
		got, st, ok := m.Token(context.Background(), slot)
		return ok && st.State == creds.StateOK && got == f.token().AccessToken
	})
}

// TestPanickingRefresherDoesNotCrashTheProcessAndSetsABackoff pins the
// guarantee refreshDetached exists for. Since Task 2 moved refreshes onto
// their own goroutine, an unrecovered panic there would take the whole
// daemon down instead of failing just this one attempt: net/http's
// per-connection recover, which used to contain a panic bubbling up from
// doRefresh, never sees a goroutine detached from the request. This test
// running to completion at all (instead of crashing the test binary, the
// way it did before refreshDetached existed) is half the proof; the other
// half is that the recovered panic still starts the normal backoff, so a
// persistently panicking refresher is not respawned on every single
// request.
func TestPanickingRefresherDoesNotCrashTheProcessAndSetsABackoff(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute))}
	f.onRefresh = func(*fakeSlot) { panic("boom") }
	m := newManagerClock(f, clk, false)

	m.Token(context.Background(), slot)
	waitFor(t, "the panicking refresh attempt", func() bool { return f.refreshCount() == 1 })

	// The clock is frozen, so a second attempt here can only mean the
	// recovered panic failed to start a backoff.
	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("Token reported ok right after a panicking refresh")
	}
	stays(t, "refreshes == 1 (a panic must start a backoff, not be retried immediately)", 150*time.Millisecond, func() bool {
		return f.refreshCount() == 1
	})

	clk.advance(31 * time.Second)
	// >= 2, not == 2: this second attempt panics too (onRefresh is never
	// reset), and on the panic path refreshDetached's own recover sets the
	// backoff strictly AFTER doRefresh's defer has already cleared
	// s.refreshing (see refreshDetached's comment on the ordering). A poll
	// landing in that window sees refreshing == false and nextTry already
	// elapsed and spawns a third attempt, so the counter can step straight
	// past 2 — see waitFor's own comment for why == would make this
	// unsatisfiable rather than just occasionally flaky.
	waitFor(t, "a refresh attempt once the backoff elapsed", func() bool {
		m.Token(context.Background(), slot)
		return f.refreshCount() >= 2
	})
}

// fakeFlock simulates a real flock closely enough to prove a leak, unlike
// newManager's TryLock (which always grants and so can never observe one):
// it refuses a second TryLock while the previous holder's unlock() has not
// been called, exactly like a real flock refuses a second locker while the
// holding fd stays open.
type fakeFlock struct {
	mu     sync.Mutex
	locked bool
}

func (f *fakeFlock) TryLock(string) (func() error, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.locked {
		return nil, false, nil
	}
	f.locked = true
	return func() error {
		f.mu.Lock()
		f.locked = false
		f.mu.Unlock()
		return nil
	}, true, nil
}

// isLocked reads locked under the mutex. Since Task 2 the unlock closure
// above can run on the detached refresh goroutine, so a direct, unguarded
// read of f.locked from a test would race it.
func (f *fakeFlock) isLocked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locked
}

// TestPanicInRefreshReleasesTheSlotLock proves the flock itself is released
// after a panic mid-refresh, not merely that s.refreshing is reset (that
// half is covered by TestPanicInRefreshDoesNotStrandTheSlotAsRefreshingForever
// above, whose fake TryLock always grants and so cannot catch a leaked
// flock). Without `defer unlock()`, a panic between acquiring the lock and
// the inline unlock() call after Refresh returns leaks it for the life of
// the process: every later refresh for this slot would see errSlotBusy
// forever, even though s.refreshing correctly looks idle.
func TestPanicInRefreshReleasesTheSlotLock(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute))} // stale, so Token enters doRefresh
	f.onRefresh = func(*fakeSlot) { panic("boom") }
	flock := &fakeFlock{}
	m := tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  flock.TryLock,
		Now:      clk.now,
	})

	m.Token(context.Background(), slot)
	waitFor(t, "the panicking refresh attempt", func() bool { return f.refreshCount() == 1 })
	// unlock() is one of doRefresh's own defers, which unwind (releasing
	// the flock) before the panic ever reaches refreshDetached's recover
	// on this goroutine — so it has already run by the time the flock is
	// observed free. That is exactly what's under test: the discriminating
	// assertion is that TryLock genuinely succeeds again below, not merely
	// that this flag looks clear (a fake that always grants would pass
	// that check too, proving nothing).
	waitFor(t, "the flock to be released", func() bool { return !flock.isLocked() })

	// The real proof: a later refresh attempt for this same slot must be
	// able to take the lock again. If the panic leaked it, fakeFlock (like
	// a real flock) stays held forever and TryLock keeps refusing, so
	// Refresh would fall into the errSlotBusy path instead of ever running
	// the Refresher again.
	clk.advance(31 * time.Second) // past the backoff the recovered panic started
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(clk.now().Add(time.Hour)) }
	waitFor(t, "a recovered refresh attempt after the panic", func() bool {
		m.Token(context.Background(), slot)
		return f.refreshCount() == 2
	})
	waitFor(t, "the refreshed token to be served", func() bool {
		got, st, ok := m.Token(context.Background(), slot)
		return ok && st.State == creds.StateOK && got == f.token().AccessToken
	})
	if flock.isLocked() {
		t.Fatal("the fake flock is still held after a successful refresh")
	}
}

// TestRefreshSurvivesACancelledRequest reproduces F37: the request that
// noticed a stale token used to block on the refresh, so a client that
// hung up killed the `claude` child and the slot fell out of service.
func TestRefreshSurvivesACancelledRequest(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	fresh := tok(now.Add(time.Hour))

	started := make(chan struct{})
	finished := make(chan error, 1)
	f := &fakeSlot{tok: tok(now.Add(-time.Hour))}
	f.refreshFunc = func(ctx context.Context) error {
		close(started)
		select {
		case <-time.After(300 * time.Millisecond):
			f.setToken(fresh)
			finished <- nil
			return nil
		case <-ctx.Done():
			finished <- ctx.Err()
			return ctx.Err()
		}
	}
	m := newManager(f, &now, false)

	// The client hangs up as soon as the refresh is under way.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	defer cancel()

	if _, _, ok := m.Token(ctx, slot); ok {
		t.Fatal("Token returned a credential while the refresh was still running; it should pass this request through")
	}

	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("the refresh died with the request that triggered it: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the refresh never finished")
	}

	// The next request gets the refreshed credential, with no backoff in
	// the way.
	waitFor(t, "the refreshed token to be served", func() bool {
		got, _, ok := m.Token(context.Background(), slot)
		return ok && got == fresh.AccessToken
	})
}

// TestCancellationDoesNotStartABackoff pins the second half of F37: an
// attempt cut short by cancellation says nothing about whether the slot's
// login works, so it must not start the doubling backoff that keeps later
// requests on passthrough.
func TestCancellationDoesNotStartABackoff(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(-time.Hour))}
	f.refreshFunc = func(ctx context.Context) error { return context.Canceled }
	m := newManager(f, &now, false)

	m.Token(context.Background(), slot)
	waitFor(t, "the first refresh attempt", func() bool { return f.refreshCount() == 1 })

	// The clock is frozen, so if the cancelled attempt started a backoff
	// this second request can never attempt a refresh. The Token call is
	// inside the condition, not before it: refreshCount()==1 goes true
	// before doRefresh's own defer clears s.refreshing, so a single call
	// made right after waitFor can land in that window and be a silent
	// no-op. Coalescing makes repeating the call inside the poll safe.
	//
	// >= 2, not == 2: cancellation deliberately leaves backoff/nextTry
	// untouched (F37 — the next request must retry immediately, not back
	// off), so nothing here ever stops a poll from spawning yet another
	// refresh attempt once the previous one has cleared s.refreshing.
	// Every iteration of this loop that finds refreshing == false spawns
	// one, and fakeSlot.Refresh increments the counter at its own top —
	// so once a freshly spawned attempt's goroutine wins the race against
	// this loop's own read, the observable count can step straight past 2
	// (e.g. 1 to 3), and == 2 would then never hold again for the rest of
	// the deadline. See waitFor's own comment for why that failure mode
	// looks like a slow machine but isn't one.
	waitFor(t, "a second refresh attempt after a cancelled one", func() bool {
		m.Token(context.Background(), slot)
		return f.refreshCount() >= 2
	})
}

// TestForceRefreshSurvivesACancelledCaller pins Step 7: ForceRefresh keeps
// blocking its caller (the proxy safety net needs its result to retry the
// request in flight), but the underlying refresh runs on a context the
// caller cannot kill. Without that, the exact F37 mechanism would apply to
// the safety net's own refresh: a client giving up mid-ForceRefresh would
// kill the `claude` child and take the slot out of service.
func TestForceRefreshSurvivesACancelledCaller(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	fresh := tok(now.Add(time.Hour))

	started := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	f.refreshFunc = func(ctx context.Context) error {
		close(started)
		select {
		case <-time.After(300 * time.Millisecond):
			f.setToken(fresh)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m := newManager(f, &now, false)

	// The caller (the safety net's own request goroutine) hangs up as soon
	// as the refresh is under way.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	defer cancel()

	got, ok := m.ForceRefresh(ctx, slot)
	if !ok || got != fresh.AccessToken {
		t.Fatalf("ForceRefresh = %q, %v; want it to survive the caller cancelling mid-refresh", got, ok)
	}
}

// TestRefreshTimeoutBoundsAnAttempt pins Step 8: doRefresh wraps its
// (already-detached) context with Config.RefreshTimeout, so a refresher
// that never returns on its own is still cut off instead of hanging the
// slot — and, via ForceRefresh, its caller — forever.
func TestRefreshTimeoutBoundsAnAttempt(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	f.refreshFunc = func(ctx context.Context) error {
		<-ctx.Done() // only returns once doRefresh's own timeout fires
		return ctx.Err()
	}
	m := tokens.New(tokens.Config{
		Read:     f.read,
		Refresh:  f,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			return func() error { return nil }, true, nil
		},
		Now:            func() time.Time { return now },
		RefreshTimeout: 50 * time.Millisecond,
	})

	done := make(chan bool, 1)
	go func() {
		_, ok := m.ForceRefresh(context.Background(), slot)
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("ForceRefresh succeeded against a refresher that only returns when its context is done")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ForceRefresh did not return; RefreshTimeout should have cut the attempt off")
	}
}

func TestTokenExpiringInsideTheReadTTLIsNoticed(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(2 * time.Second))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(now.Add(time.Hour)) }
	m := newManager(f, &now, false)
	if _, st, ok := m.Token(context.Background(), slot); !ok || st.State != creds.StateExpiring {
		t.Fatalf("first Token = %+v, %v; want usable+expiring", st, ok)
	}
	now = now.Add(5 * time.Second) // expired, still inside the 30s read TTL
	if _, st, ok := m.Token(context.Background(), slot); ok || st.State != creds.StateStale {
		t.Fatalf("triggering second Token = %+v, %v; want stale pass-through while the refresh runs", st, ok)
	}
	waitFor(t, "the refreshed token to be served", func() bool {
		_, st, ok := m.Token(context.Background(), slot)
		return ok && st.State == creds.StateOK
	})
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshCount())
	}
}

// TestInvalidateAllForcesAFreshRead: after a wake, a cached credential may
// have expired while the machine slept, so the next use must re-read the
// slot rather than trust the cache.
func TestInvalidateAllForcesAFreshRead(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(time.Hour))}
	m := newManager(f, &now, false)

	if _, _, ok := m.Token(context.Background(), slot); !ok {
		t.Fatal("expected a usable token")
	}
	first := f.readCount()

	// Without invalidation the ReadTTL cache would serve this.
	if _, _, ok := m.Token(context.Background(), slot); !ok {
		t.Fatal("expected a usable token")
	}
	if got := f.readCount(); got != first {
		t.Fatalf("read count = %d, want %d — the cache should have served this", got, first)
	}

	m.InvalidateAll()
	if _, _, ok := m.Token(context.Background(), slot); !ok {
		t.Fatal("expected a usable token")
	}
	if got := f.readCount(); got != first+1 {
		t.Errorf("read count = %d, want %d — InvalidateAll did not force a re-read", got, first+1)
	}
}

// TestInvalidateAllDoesNotClearBackoff: a slot already backing off from a
// genuine failure before the sleep must not be hammered the instant the
// wake detector clears its read cache. InvalidateAll clears only readAt.
func TestInvalidateAllDoesNotClearBackoff(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute)), refreshErr: errors.New("network down")}
	m := newManagerClock(f, clk, false)

	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("failed refresh reported ok")
	}
	waitFor(t, "the failed refresh attempt to back off", func() bool { return f.refreshCount() == 1 })
	readsBeforeInvalidate := f.readCount()

	m.InvalidateAll()

	// readAt was cleared: the very next Token call re-reads the slot store
	// even though ReadTTL has not elapsed.
	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("Token reported ok for a still-expired, still-erroring slot")
	}
	if got := f.readCount(); got != readsBeforeInvalidate+1 {
		t.Fatalf("read count = %d, want %d — InvalidateAll should have forced a re-read", got, readsBeforeInvalidate+1)
	}

	// backoff/nextTry were NOT cleared: the clock hasn't moved past the
	// backoff window, so this must not have triggered a second refresh
	// attempt. If it had, InvalidateAll would hammer a slot that was
	// already failing the instant the machine woke up.
	stays(t, "refreshes == 1 (backoff still in effect)", 150*time.Millisecond, func() bool {
		return f.refreshCount() == 1
	})
}

// dirCountingReader is item 6's own fixture (review round 3): it counts
// reads per slot dir, unlike fakeSlot's single shared counter, so a test can
// tell Invalidate forced a fresh read on the one slot it named and left
// every other slot's ReadTTL cache alone. exp is always in the future, so
// Assess never reports StateStale and Refresh is never expected to run.
type dirCountingReader struct {
	mu    sync.Mutex
	reads map[string]int
	exp   time.Time
}

func (d *dirCountingReader) read(dir string) (creds.Token, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reads == nil {
		d.reads = map[string]int{}
	}
	d.reads[dir]++
	return tok(d.exp), nil
}

func (d *dirCountingReader) count(dir string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads[dir]
}

func (d *dirCountingReader) Refresh(context.Context, string) error {
	return errors.New("dirCountingReader: unexpected refresh; exp is always in the future")
}

// TestInvalidateForcesAFreshReadOnOnlyThatSlot is item 6 (review round 3):
// Invalidate must force a re-read of the one slot named, the same as
// InvalidateAll does for every slot, and must not touch a different slot's
// still-valid ReadTTL cache.
func TestInvalidateForcesAFreshReadOnOnlyThatSlot(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	r := &dirCountingReader{exp: now.Add(time.Hour)}
	m := tokens.New(tokens.Config{
		Read:     r.read,
		Refresh:  r,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock: func(string) (func() error, bool, error) {
			return func() error { return nil }, true, nil
		},
		Now: func() time.Time { return now },
	})
	const dirA, dirB = "/slots/A", "/slots/B"
	for _, d := range []string{dirA, dirB} {
		if _, _, ok := m.Token(context.Background(), d); !ok {
			t.Fatalf("expected a usable token for %s", d)
		}
	}
	firstA, firstB := r.count(dirA), r.count(dirB)

	// Without Invalidate, the ReadTTL cache serves both.
	for _, d := range []string{dirA, dirB} {
		if _, _, ok := m.Token(context.Background(), d); !ok {
			t.Fatalf("expected a usable token for %s", d)
		}
	}
	if r.count(dirA) != firstA || r.count(dirB) != firstB {
		t.Fatalf("reads = %d/%d, want %d/%d — the cache should have served both", r.count(dirA), r.count(dirB), firstA, firstB)
	}

	m.Invalidate(dirA)
	if _, _, ok := m.Token(context.Background(), dirA); !ok {
		t.Fatal("expected a usable token for dirA")
	}
	if _, _, ok := m.Token(context.Background(), dirB); !ok {
		t.Fatal("expected a usable token for dirB")
	}
	if got := r.count(dirA); got != firstA+1 {
		t.Errorf("dirA reads = %d, want %d — Invalidate did not force a re-read", got, firstA+1)
	}
	if got := r.count(dirB); got != firstB {
		t.Errorf("dirB reads = %d, want %d — Invalidate must not touch a different slot", got, firstB)
	}
}

// TestAwaitWaitsOutAnInFlightRefreshAndReturnsTheFreshToken pins F166's fix:
// unlike Token, Await does not settle for the stale passthrough while a
// refresh it triggered is still running — it waits for that refresh (the
// refresher here blocks on release, standing in for the real `claude`
// subprocess) and returns the fresh token once it lands.
func TestAwaitWaitsOutAnInFlightRefreshAndReturnsTheFreshToken(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	fresh := tok(now.Add(time.Hour))
	release := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.refreshFunc = func(ctx context.Context) error {
		<-release
		f.setToken(fresh)
		return nil
	}
	m := newManager(f, &now, false)

	type result struct {
		tok string
		ok  bool
	}
	results := make(chan result, 1)
	go func() {
		got, _, ok := m.Await(context.Background(), slot)
		results <- result{got, ok}
	}()

	waitFor(t, "the refresh attempt to start", func() bool { return f.refreshCount() == 1 })
	close(release)

	select {
	case r := <-results:
		if !r.ok || r.tok != fresh.AccessToken {
			t.Fatalf("Await = %q, %v; want the fresh token", r.tok, r.ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await never returned")
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshCount())
	}
}

// TestAwaitCoalescesConcurrentCallers proves 10 concurrent Awaits on one
// stale slot share the single refresh Token's triggering call started (the
// same coalescing ForceRefresh already relies on), and every one of them
// gets the fresh token once it lands, not just the caller who happened to
// trigger the refresh.
func TestAwaitCoalescesConcurrentCallers(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	fresh := tok(now.Add(time.Hour))
	release := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.refreshFunc = func(ctx context.Context) error {
		<-release
		f.setToken(fresh)
		return nil
	}
	m := newManager(f, &now, false)

	const n = 10
	results := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			got, _, ok := m.Await(context.Background(), slot)
			if !ok {
				results <- ""
				return
			}
			results <- got
		}()
	}

	waitFor(t, "the refresh attempt to start", func() bool { return f.refreshCount() == 1 })
	close(release)

	for i := 0; i < n; i++ {
		select {
		case got := <-results:
			if got != fresh.AccessToken {
				t.Fatalf("Await result = %q, want %q", got, fresh.AccessToken)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("not all 10 Awaits returned")
		}
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1 (concurrent Await calls must coalesce)", f.refreshCount())
	}
}

// TestAwaitContextTimeoutDoesNotCancelTheRefresh pins the other half of
// F166's fix: the selector bounds its Await call with its own timeout
// (Choose wraps ctx, not the caller's original request context), and that
// timeout must only give up WAITING — it must not reach into the refresh
// itself, for exactly the reason RefreshTimeout's own doc gives (F37): a
// refresh killed by an impatient caller takes the slot out of service for
// everyone else. The refresher here only unblocks on release, well after
// ctx has expired, so a later Token call getting the fresh token is proof
// the refresh kept running uncancelled.
func TestAwaitContextTimeoutDoesNotCancelTheRefresh(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	fresh := tok(now.Add(time.Hour))
	release := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.refreshFunc = func(ctx context.Context) error {
		<-release
		f.setToken(fresh)
		return nil
	}
	m := newManager(f, &now, false)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	type result struct {
		ok      bool
		elapsed time.Duration
	}
	results := make(chan result, 1)
	start := time.Now()
	go func() {
		_, _, ok := m.Await(ctx, slot)
		results <- result{ok, time.Since(start)}
	}()

	select {
	case r := <-results:
		if r.ok {
			t.Fatal("Await succeeded while the refresher was still blocked")
		}
		if r.elapsed > 500*time.Millisecond {
			t.Fatalf("Await took %v to return after its 50ms context expired", r.elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await never returned; its ctx timeout did not stop the wait")
	}

	close(release)
	waitFor(t, "the refreshed token to be served, proving the refresh was not cancelled", func() bool {
		got, _, ok := m.Token(context.Background(), slot)
		return ok && got == fresh.AccessToken
	})
}

// TestAwaitReturnsPromptlyWhenTheRefreshFails proves a failed refresh wakes
// every PARKED waiter — one already blocked in Await's select on s.done, not
// one that merely finds s.refreshing already false by the time it checks —
// promptly instead of leaving them to time out: doRefresh's
// first-registered defer closes s.done on every exit, success or failure.
//
// The refresh is deliberately held open on release until every one of the n
// concurrent Awaits below has started: as long as release stays closed,
// s.refreshing cannot have gone false yet, so every one of them is
// guaranteed to take Await's done-wait branch and block in the select —
// none of them can win the `!s.refreshing` race and skip the channel
// entirely, which is exactly what made the previous version of this test
// (an instantly-failing refresh) not reliably exercise this path.
func TestAwaitReturnsPromptlyWhenTheRefreshFails(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.refreshFunc = func(ctx context.Context) error {
		<-release
		return errors.New("network down")
	}
	m := newManager(f, &now, false)

	const n = 5
	type result struct {
		ok      bool
		elapsed time.Duration
	}
	results := make(chan result, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _, ok := m.Await(ctx, slot)
			results <- result{ok, time.Since(start)}
		}()
	}

	waitFor(t, "the refresh attempt to start", func() bool { return f.refreshCount() == 1 })
	close(release)

	for i := 0; i < n; i++ {
		select {
		case r := <-results:
			if r.ok {
				t.Fatal("an Await succeeded despite a failing refresh")
			}
			if r.elapsed > 500*time.Millisecond {
				t.Fatalf("an Await took %v to return after the refresh failed; want well under its 2s ctx timeout", r.elapsed)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("not all Awaits returned")
		}
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1 (concurrent Await calls must coalesce)", f.refreshCount())
	}
}

// TestAwaitOnANeedsLoginSlotReturnsAtOnce: a slot with no login at all has
// nothing to refresh, so Await must not try to wait on it — it returns
// Token's own StateNeedsLogin result immediately, same as Token does.
func TestAwaitOnANeedsLoginSlotReturnsAtOnce(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{err: creds.ErrNoLogin}
	m := newManager(f, &now, false)

	done := make(chan bool, 1)
	go func() {
		_, st, ok := m.Await(context.Background(), slot)
		done <- ok || st.State == creds.StateNeedsLogin
	}()
	select {
	case gotNeedsLogin := <-done:
		if !gotNeedsLogin {
			t.Fatal("Await did not report StateNeedsLogin")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Await blocked on a needs-login slot")
	}
	if f.refreshCount() != 0 {
		t.Fatal("Await refreshed a slot with no login")
	}
}

// TestAwaitDuringBackoffTriesOneRefreshAndReturnsItsToken (R147): a slot in
// backoff from a failed refresh has nothing in flight, but a remote or owner
// request must not go out on Home's login, so Await tries one refresh
// regardless of nextTry and waits for it.
func TestAwaitDuringBackoffTriesOneRefreshAndReturnsItsToken(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute)), refreshErr: errors.New("network down")}
	m := newManagerClock(f, clk, false)

	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("failed refresh reported ok")
	}
	waitFor(t, "the failed refresh attempt to back off", func() bool { return f.refreshCount() == 1 })

	fresh := tok(clk.now().Add(time.Hour))
	f.setOnRefresh(func(f *fakeSlot) { f.tok = fresh })
	f.mu.Lock()
	f.refreshErr = nil
	f.mu.Unlock()

	got, _, ok := m.Await(context.Background(), slot)
	if !ok || got != fresh.AccessToken {
		t.Fatalf("Await = %q, %v; want the freshly refreshed token", got, ok)
	}
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d, want 2 (one forced past the backoff)", f.refreshCount())
	}
}

// A forced attempt that fails is not repeated by every request: a second
// Await inside MinBackoff returns at once without another refresh.
func TestAwaitDuringBackoffFailsAndThrottlesTheNextForcedAttempt(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute)), refreshErr: errors.New("network down")}
	m := newManagerClock(f, clk, false)

	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("failed refresh reported ok")
	}
	waitFor(t, "the failed refresh attempt to back off", func() bool { return f.refreshCount() == 1 })

	if _, _, ok := m.Await(context.Background(), slot); ok {
		t.Fatal("Await succeeded though the refresh fails")
	}
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d, want 2", f.refreshCount())
	}
	done := make(chan bool, 1)
	go func() {
		_, _, ok := m.Await(context.Background(), slot)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("second Await succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Await blocked")
	}
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d, want 2 (the forced attempt is throttled)", f.refreshCount())
	}
	// Natural refresh after the backoff (60s after the forced failure).
	clk.advance(61 * time.Second)
	if _, _, ok := m.Await(context.Background(), slot); ok {
		t.Fatal("Await succeeded though the refresh fails")
	}
	if f.refreshCount() != 3 {
		t.Fatalf("refreshes = %d, want 3 after the backoff lapsed", f.refreshCount())
	}
	// Backoff is now 120s. 61s later the forced throttle (60s from the last
	// attempt) has lapsed but nextTry has not: only a forced attempt can run.
	clk.advance(61 * time.Second)
	if _, _, ok := m.Await(context.Background(), slot); ok {
		t.Fatal("Await succeeded though the refresh fails")
	}
	if f.refreshCount() != 4 {
		t.Fatalf("refreshes = %d, want 4 (a forced attempt once the 60s throttle lapsed)", f.refreshCount())
	}
	// Less than 60s after that attempt: no forced attempt.
	clk.advance(30 * time.Second)
	m.Await(context.Background(), slot)
	if f.refreshCount() != 4 {
		t.Fatalf("refreshes = %d, want 4 inside the 60s throttle", f.refreshCount())
	}
}

// Concurrent Awaits in backoff coalesce into one forced refresh.
func TestAwaitDuringBackoffCoalescesConcurrentCallers(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(-time.Minute)), refreshErr: errors.New("network down")}
	m := newManagerClock(f, clk, false)
	if _, _, ok := m.Token(context.Background(), slot); ok {
		t.Fatal("failed refresh reported ok")
	}
	waitFor(t, "the failed refresh attempt to back off", func() bool { return f.refreshCount() == 1 })

	release := make(chan struct{})
	fresh := tok(clk.now().Add(time.Hour))
	f.mu.Lock()
	f.refreshFunc = func(ctx context.Context) error {
		<-release
		f.setToken(fresh)
		return nil
	}
	f.mu.Unlock()
	const n = 5
	results := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _, ok := m.Await(context.Background(), slot)
			results <- ok
		}()
	}
	waitFor(t, "the forced refresh to start", func() bool { return f.refreshCount() == 2 })
	close(release)
	for i := 0; i < n; i++ {
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("an Await failed")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("not all Awaits returned")
		}
	}
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d, want 2", f.refreshCount())
	}
}

// Warm (R147) refreshes a token that expires within the window, before any
// request needs it, with an injected clock.
func TestWarmRefreshesATokenExpiringWithinTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(14 * time.Minute))} // OK to Assess, inside 15 minutes
	fresh := tok(now.Add(time.Hour))
	f.onRefresh = func(f *fakeSlot) { f.tok = fresh }
	m := newManager(f, &now, false)

	st, refreshed := m.Warm(context.Background(), slot, 15*time.Minute)
	if !refreshed || st.State != creds.StateOK {
		t.Fatalf("Warm = %+v, %v; want a refresh to an ok token", st, refreshed)
	}
	if f.refreshCount() != 1 || f.token().AccessToken != fresh.AccessToken {
		t.Fatalf("refreshes = %d, token = %q", f.refreshCount(), f.token().AccessToken)
	}
}

func TestWarmLeavesATokenOutsideTheWindowAlone(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(16 * time.Minute))}
	m := newManager(f, &now, false)
	if _, refreshed := m.Warm(context.Background(), slot, 15*time.Minute); refreshed || f.refreshCount() != 0 {
		t.Fatalf("refreshed a token 16 minutes from expiry: %d refreshes", f.refreshCount())
	}
}

func TestWarmRefreshesAnAlreadyStaleToken(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(now.Add(time.Hour)) }
	m := newManager(f, &now, false)
	if _, refreshed := m.Warm(context.Background(), slot, 15*time.Minute); !refreshed {
		t.Fatal("a stale token was not refreshed")
	}
}

func TestWarmNeverRefreshesANeedsLoginSlot(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	f := &fakeSlot{err: creds.ErrNoLogin}
	m := newManager(f, &now, false)
	st, refreshed := m.Warm(context.Background(), slot, 15*time.Minute)
	if refreshed || st.State != creds.StateNeedsLogin || f.refreshCount() != 0 {
		t.Fatalf("Warm = %+v, %v, %d refreshes; want needs-login and no refresh", st, refreshed, f.refreshCount())
	}
}

// Warm respects the failure backoff and shares the single-flight flag, so a
// failing slot is not respawned on every pass.
func TestWarmRespectsTheBackoffAndSingleFlight(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(time.Minute)), refreshErr: errors.New("network down")}
	m := newManagerClock(f, clk, false)
	if _, refreshed := m.Warm(context.Background(), slot, 15*time.Minute); refreshed {
		t.Fatal("a failed refresh reported refreshed")
	}
	if _, refreshed := m.Warm(context.Background(), slot, 15*time.Minute); refreshed || f.refreshCount() != 1 {
		t.Fatalf("Warm inside the backoff refreshed again: %d refreshes", f.refreshCount())
	}
	clk.advance(31 * time.Second)
	m.Warm(context.Background(), slot, 15*time.Minute)
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d after the backoff, want 2", f.refreshCount())
	}
}

// I1: a panicking refresher does not escape Warm and leaves a backoff.
func TestWarmRecoversFromAPanickingRefresherAndSetsABackoff(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(time.Minute))}
	f.refreshFunc = func(context.Context) error { panic("exec plumbing") }
	m := newManagerClock(f, clk, false)
	if _, refreshed := m.Warm(context.Background(), slot, 6*time.Minute); refreshed {
		t.Fatal("a panicking refresh reported refreshed")
	}
	if _, refreshed := m.Warm(context.Background(), slot, 6*time.Minute); refreshed || f.refreshCount() != 1 {
		t.Fatalf("Warm inside the backoff refreshed again: %d refreshes", f.refreshCount())
	}
}

// I2: a refresh that leaves the token unchanged (Claude Code refreshes only
// close to expiry) is not repeated on every pass: the slot is left alone
// until it is inside creds.ExpiringWithin of expiry.
func TestWarmDoesNotRespawnWhenTheTokenDidNotRenew(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	exp := clk.now().Add(6 * time.Minute)
	f := &fakeSlot{tok: tok(exp)} // the default refresher changes nothing
	m := newManagerClock(f, clk, false)
	win := creds.ExpiringWithin + time.Minute
	if _, refreshed := m.Warm(context.Background(), slot, win); refreshed {
		t.Fatal("an unchanged token reported refreshed")
	}
	for i := 0; i < 2; i++ { // 40s in: 5m20s left, still more than 5m
		clk.advance(20 * time.Second)
		m.Warm(context.Background(), slot, win)
	}
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1: no respawn while more than %v remain", f.refreshCount(), creds.ExpiringWithin)
	}
	clk.advance(2 * time.Minute) // inside the last 5 minutes
	m.Warm(context.Background(), slot, win)
	if f.refreshCount() != 2 {
		t.Fatalf("refreshes = %d, want 2 once inside %v of expiry", f.refreshCount(), creds.ExpiringWithin)
	}
}

// A refresh already in flight (single flight): Warm neither spawns nor blocks.
func TestWarmNeitherSpawnsNorBlocksWhileARefreshIsInFlight(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	f := &fakeSlot{tok: tok(now.Add(-time.Minute))}
	f.refreshFunc = func(context.Context) error { <-release; return errors.New("down") }
	m := newManager(f, &now, false)
	m.Token(context.Background(), slot) // starts the background refresh
	waitFor(t, "the refresh to start", func() bool { return f.refreshCount() == 1 })
	done := make(chan struct{})
	go func() {
		m.Warm(context.Background(), slot, 6*time.Minute)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Warm blocked behind a refresh in flight")
	}
	close(release)
	if f.refreshCount() != 1 {
		t.Fatalf("refreshes = %d, want 1", f.refreshCount())
	}
}

// M8 (F49): cancelling Warm's context (daemon shutdown) never cancels the
// refresher: killing claude mid-refresh can tear the token write. The
// refresh runs to its own end, and no backoff results.
func TestWarmRefreshIsNotCancelledWithItsCallersContext(t *testing.T) {
	clk := newSyncClock(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	f := &fakeSlot{tok: tok(clk.now().Add(time.Minute))}
	release := make(chan struct{})
	refresherCtxErr := make(chan error, 1)
	fresh := tok(clk.now().Add(time.Hour))
	f.refreshFunc = func(ctx context.Context) error {
		<-release
		refresherCtxErr <- ctx.Err()
		f.setToken(fresh)
		return nil
	}
	m := newManagerClock(f, clk, false)
	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		refreshed bool
	}
	done := make(chan res, 1)
	go func() {
		_, r := m.Warm(ctx, slot, 6*time.Minute)
		done <- res{r}
	}()
	waitFor(t, "the refresh to start", func() bool { return f.refreshCount() == 1 })
	cancel()
	close(release)
	if err := <-refresherCtxErr; err != nil {
		t.Fatalf("the refresher's context was cancelled with the caller's: %v", err)
	}
	if r := <-done; !r.refreshed {
		t.Fatal("the refresh did not complete")
	}
}
