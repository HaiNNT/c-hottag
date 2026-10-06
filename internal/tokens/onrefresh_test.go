package tokens_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// events records every RefreshEvent a manager emits.
type events struct {
	mu  sync.Mutex
	got []tokens.RefreshEvent
	ch  chan tokens.RefreshEvent
}

func newEvents() *events { return &events{ch: make(chan tokens.RefreshEvent, 16)} }

func (e *events) record(ev tokens.RefreshEvent) {
	e.mu.Lock()
	e.got = append(e.got, ev)
	e.mu.Unlock()
	e.ch <- ev
}

func (e *events) all() []tokens.RefreshEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tokens.RefreshEvent(nil), e.got...)
}

func (e *events) last(t *testing.T) tokens.RefreshEvent {
	t.Helper()
	g := e.all()
	if len(g) == 0 {
		t.Fatal("no refresh event")
	}
	return g[len(g)-1]
}

func newObservedManager(f *fakeSlot, clk *syncClock, ev *events) *tokens.Manager {
	return tokens.New(tokens.Config{
		Read:      f.read,
		Refresh:   f,
		LockPath:  func(dir string) string { return dir + "/.lock" },
		TryLock:   func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		Now:       clk.now,
		OnRefresh: ev.record,
	})
}

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func TestOnRefreshReportsRenewed(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(t0.Add(8 * time.Hour)) }
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	m.WarmFor(context.Background(), slot, 15*time.Minute, tokens.TriggerWake)
	got := ev.last(t)
	if got.Account != "C" || got.Trigger != tokens.TriggerWake || got.Outcome != tokens.OutcomeRenewed {
		t.Fatalf("event = %+v", got)
	}
	if got.ExpiresIn != 8*time.Hour || got.NeedsLogin {
		t.Fatalf("event = %+v, want ExpiresIn 8h", got)
	}
}

func TestOnRefreshWarmTriggerAndUnchangedToken(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(14 * time.Minute))} // usable, and the refresh leaves it
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	m.Warm(context.Background(), slot, 15*time.Minute)
	got := ev.last(t)
	if got.Trigger != tokens.TriggerWarm || got.Outcome != tokens.OutcomeNotRenewed {
		t.Fatalf("event = %+v", got)
	}
}

func TestOnRefreshReportsAForcedRefresh(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(time.Hour))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(t0.Add(8 * time.Hour)) }
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	if _, ok := m.ForceRefresh(context.Background(), slot); !ok {
		t.Fatal("ForceRefresh failed")
	}
	if got := ev.last(t); got.Trigger != tokens.TriggerForced || got.Outcome != tokens.OutcomeRenewed {
		t.Fatalf("event = %+v", got)
	}
}

func TestOnRefreshReportsARequestTriggeredRefresh(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(t0.Add(8 * time.Hour)) }
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	m.Token(context.Background(), slot)
	got := <-ev.ch
	if got.Trigger != tokens.TriggerRequest || got.Outcome != tokens.OutcomeRenewed {
		t.Fatalf("event = %+v", got)
	}
}

func TestOnRefreshReportsAFailure(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute)), refreshErr: errors.New("refresh /slots/C: /bin/claude mcp list: exit status 1")}
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	m.Warm(context.Background(), slot, 15*time.Minute)
	got := ev.last(t)
	if got.Outcome != tokens.OutcomeFailed || got.Detail != "exit status 1" || got.RetryIn != 30*time.Second {
		t.Fatalf("event = %+v", got)
	}
}

func TestOnRefreshDetailNeverCarriesASecret(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute)), refreshErr: errors.New("x: Bearer sk-ant-abc123")}
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	m.Warm(context.Background(), slot, 15*time.Minute)
	if d := ev.last(t).Detail; strings.Contains(d, "sk-ant-") || strings.Contains(d, "Bearer") {
		t.Fatalf("detail = %q", d)
	}
}

func TestOnRefreshReportsAPanic(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute))}
	f.refreshFunc = func(context.Context) error { panic("boom") }
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	m.Warm(context.Background(), slot, 15*time.Minute)
	got := ev.last(t)
	if got.Outcome != tokens.OutcomePanicked || got.Account != "C" || got.RetryIn != 30*time.Second {
		t.Fatalf("event = %+v", got)
	}
}

func TestOnRefreshPanicInTheCallbackIsContained(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(t0.Add(time.Hour)) }
	m := tokens.New(tokens.Config{
		Read: f.read, Refresh: f, Now: clk.now,
		LockPath:  func(dir string) string { return dir + "/.lock" },
		TryLock:   func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		OnRefresh: func(tokens.RefreshEvent) { panic("callback") },
	})
	if _, ok := m.Warm(context.Background(), slot, 15*time.Minute); !ok {
		t.Fatal("a panicking callback broke the refresh")
	}
}

// stuckSlot is a slot whose refresh exits 0 and leaves the token expired.
func stuckSlot() *fakeSlot { return &fakeSlot{tok: tok(t0.Add(-time.Minute))} }

// tryWarm advances the clock by gap, then runs one warm pass.
func tryWarm(m *tokens.Manager, clk *syncClock, gap time.Duration) (creds.Status, bool) {
	clk.advance(gap)
	return m.Warm(context.Background(), slot, 15*time.Minute)
}

func anyNeedsLogin(ev *events) bool {
	for _, e := range ev.all() {
		if e.NeedsLogin {
			return true
		}
	}
	return false
}

func TestThreeQuickNotRenewedDoNotLockOut(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	// A post-wake network delay: tries at +1m, +2m, +4m, +8m, all inside 10 minutes.
	for _, gap := range []time.Duration{time.Minute, time.Minute, 2 * time.Minute, 4 * time.Minute} {
		st, _ := tryWarm(m, clk, gap)
		if st.State == creds.StateNeedsLogin {
			t.Fatalf("locked out after a run shorter than %v: %+v", tokens.LockoutSpan, st)
		}
	}
	if anyNeedsLogin(ev) || f.refreshCount() != 4 {
		t.Fatalf("NeedsLogin or wrong count: %d refreshes", f.refreshCount())
	}
}

func TestThreeNotRenewedSpanningTenMinutesIsNeedsLogin(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	for i := 1; i <= 3; i++ {
		st, ok := tryWarm(m, clk, 6*time.Minute) // first to third: 12 minutes
		got := ev.last(t)
		if ok || got.Outcome != tokens.OutcomeNotRenewed {
			t.Fatalf("try %d: %+v, ok=%v", i, got, ok)
		}
		wantLogin := i == 3
		if got.NeedsLogin != wantLogin || (st.State == creds.StateNeedsLogin) != wantLogin {
			t.Fatalf("try %d: NeedsLogin = %v, status = %+v, want lockout %v", i, got.NeedsLogin, st, wantLogin)
		}
	}
	st := m.Status(slot)
	if st.State != creds.StateNeedsLogin || st.Reason != "refresh-not-renewing" {
		t.Fatalf("status = %+v", st)
	}
	if _, st, ok := m.Token(context.Background(), slot); ok || st.State != creds.StateNeedsLogin {
		t.Fatalf("Token = %+v, %v", st, ok)
	}
}

// A locked-out slot is probed once per MaxBackoff, and a renewal lifts the
// lockout without a re-login.
func TestLockoutIsProbedAndARenewalClearsIt(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	for i := 0; i < 3; i++ {
		tryWarm(m, clk, 6*time.Minute)
	}
	if f.refreshCount() != 3 || m.Status(slot).State != creds.StateNeedsLogin {
		t.Fatal("setup: not locked out")
	}
	tryWarm(m, clk, 14*time.Minute)
	if f.refreshCount() != 3 {
		t.Fatalf("probed inside MaxBackoff: %d refreshes", f.refreshCount())
	}
	// Still dead at the probe: stays locked out, no second NeedsLogin event.
	tryWarm(m, clk, time.Minute)
	if f.refreshCount() != 4 || m.Status(slot).State != creds.StateNeedsLogin {
		t.Fatalf("probe: %d refreshes, status %+v", f.refreshCount(), m.Status(slot))
	}
	n := 0
	for _, e := range ev.all() {
		if e.NeedsLogin {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d NeedsLogin events, want 1", n)
	}
	// The network is back: the next probe renews.
	f.setOnRefresh(func(f *fakeSlot) { f.tok = tok(clk.now().Add(8 * time.Hour)) })
	st, ok := tryWarm(m, clk, 16*time.Minute)
	if !ok || st.State != creds.StateOK {
		t.Fatalf("probe = %+v, %v", st, ok)
	}
	if got := ev.last(t); got.Outcome != tokens.OutcomeRenewed {
		t.Fatalf("event = %+v, want renewed", got)
	}
	if _, st, ok := m.Token(context.Background(), slot); !ok || st.State != creds.StateOK {
		t.Fatalf("Token = %+v, %v after the probe renewed", st, ok)
	}
}

// bigSpan is a gap that, with two of them, already spans LockoutSpan.
const bigSpan = 6 * time.Minute

func TestARenewalResetsTheNotRenewedCount(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan) // two in a row
	f.setOnRefresh(func(f *fakeSlot) { f.tok = tok(clk.now().Add(30 * time.Minute)) })
	tryWarm(m, clk, bigSpan) // renewed
	if got := ev.last(t); got.Outcome != tokens.OutcomeRenewed {
		t.Fatalf("event = %+v", got)
	}
	f.setOnRefresh(nil)
	clk.advance(time.Hour) // the token is expired again
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan) // two more, not three
	if st := m.Status(slot); st.State == creds.StateNeedsLogin || anyNeedsLogin(ev) {
		t.Fatalf("status = %+v: the count was not reset", st)
	}
}

func TestAUsableTokenSeenByReadResetsTheCount(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	// Something else (a login, claude itself) leaves a usable token.
	f.setToken(tok(clk.now().Add(8 * time.Hour)))
	clk.advance(time.Minute) // past ReadTTL
	if st := m.Status(slot); st.State != creds.StateOK {
		t.Fatalf("status = %+v", st)
	}
	f.setToken(tok(clk.now().Add(-time.Minute))) // and it expires again
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	if m.Status(slot).State == creds.StateNeedsLogin || anyNeedsLogin(ev) {
		t.Fatal("the count survived a usable token")
	}
}

func TestInvalidateResetsTheCountEvenWithoutALockout(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	m.Invalidate(slot) // `chottag login` finished; the token expires later, again
	tryWarm(m, clk, bigSpan)
	if m.Status(slot).State == creds.StateNeedsLogin || anyNeedsLogin(ev) {
		t.Fatal("the count survived Invalidate")
	}
}

func TestAFailureBreaksTheRun(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	f.mu.Lock()
	f.refreshErr = errors.New("x: exit status 1")
	f.mu.Unlock()
	tryWarm(m, clk, bigSpan)
	if got := ev.last(t); got.Outcome != tokens.OutcomeFailed {
		t.Fatalf("event = %+v", got)
	}
	f.mu.Lock()
	f.refreshErr = nil
	f.mu.Unlock()
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	if m.Status(slot).State == creds.StateNeedsLogin || anyNeedsLogin(ev) {
		t.Fatal("a failure in between did not break the run")
	}
}

func TestAUsableButUnchangedRefreshResetsTheCount(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	f.setToken(tok(clk.now().Add(14 * time.Minute))) // usable, inside the window
	tryWarm(m, clk, time.Minute)
	if got := ev.last(t); got.Outcome != tokens.OutcomeNotRenewed || got.Detail != "token unchanged, not yet due" {
		t.Fatalf("event = %+v", got)
	}
	f.setToken(tok(clk.now().Add(-time.Minute)))
	tryWarm(m, clk, bigSpan)
	tryWarm(m, clk, bigSpan)
	if anyNeedsLogin(ev) {
		t.Fatal("the count survived a usable-but-unchanged refresh")
	}
}

// A read error is no evidence that the token did not renew.
func TestAReadErrorAfterExitZeroIsAFailureNotANonRenewal(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{err: errors.New("unreadable")}
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	for i := 0; i < 5; i++ {
		tryWarm(m, clk, bigSpan)
		got := ev.last(t)
		if got.Outcome != tokens.OutcomeFailed || got.Detail != "no usable login: read-error" || got.NoLogin {
			t.Fatalf("try %d: %+v", i, got)
		}
	}
	if m.Status(slot).State == creds.StateNeedsLogin || anyNeedsLogin(ev) {
		t.Fatal("a read error locked the slot out")
	}
}

// The callback runs after the slot is released: from inside it, the slot
// takes another refresh.
func TestOnRefreshRunsAfterTheSlotIsReleased(t *testing.T) {
	clk := newSyncClock(t0)
	f := stuckSlot()
	var m *tokens.Manager
	var entered atomic.Bool
	var inner int
	m = tokens.New(tokens.Config{
		Read: f.read, Refresh: f, Now: clk.now,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		OnRefresh: func(tokens.RefreshEvent) {
			if entered.CompareAndSwap(false, true) {
				clk.advance(time.Hour)
				m.Warm(context.Background(), slot, 15*time.Minute)
				inner = f.refreshCount()
			}
		},
	})
	m.Warm(context.Background(), slot, 15*time.Minute)
	if inner != 2 {
		t.Fatalf("refreshes seen from the callback = %d, want 2: the slot still read as refreshing", inner)
	}
}

func TestAPanickingAccountNameIsContained(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{tok: tok(t0.Add(-time.Minute))}
	f.onRefresh = func(f *fakeSlot) { f.tok = tok(t0.Add(time.Hour)) }
	ev := newEvents()
	m := tokens.New(tokens.Config{
		Read: f.read, Refresh: f, Now: clk.now, OnRefresh: ev.record,
		LockPath:    func(dir string) string { return dir + "/.lock" },
		TryLock:     func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		AccountName: func(string) string { panic("names") },
	})
	if _, ok := m.Warm(context.Background(), slot, 15*time.Minute); !ok {
		t.Fatal("a panicking AccountName broke the refresh")
	}
	if got := ev.last(t); got.Account != "C" {
		t.Fatalf("account = %q, want the directory's name", got.Account)
	}
}

func TestKeychainUnavailableRetriesOnAFlat30sForFiveMinutesThenDoubles(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{err: creds.ErrKeychainUnavailable}
	ev := newEvents()
	m := newObservedManager(f, clk, ev)

	check := func(gap, wantRetry time.Duration) {
		t.Helper()
		tryWarm(m, clk, gap)
		got := ev.last(t)
		if got.Outcome != tokens.OutcomeFailed || got.Detail != "keychain unavailable" || got.RetryIn != wantRetry {
			t.Fatalf("%+v, want retry in %v", got, wantRetry)
		}
	}
	check(time.Hour, 30*time.Second)
	check(31*time.Second, 30*time.Second)
	check(time.Minute, 30*time.Second)
	check(tokens.KeychainFlatFor, 30*time.Second) // past the flat window: the doubling starts at MinBackoff
	check(time.Minute, time.Minute)
	if st := m.Status(slot); st.State == creds.StateNeedsLogin {
		t.Fatalf("keychain-unavailable counted toward needs-login: %+v", st)
	}
}

func lockedOutRig(t *testing.T) (*fakeSlot, *tokens.Manager, *syncClock, *events) {
	t.Helper()
	clk := newSyncClock(t0)
	f := stuckSlot()
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	for i := 0; i < 3; i++ {
		tryWarm(m, clk, 6*time.Minute)
	}
	if got := m.LockedOut(); len(got) != 1 || got[0] != slot {
		t.Fatalf("LockedOut = %v, want [%s]", got, slot)
	}
	return f, m, clk, ev
}

// While locked out, a probe whose outcome is not a renewal still waits
// MaxBackoff before the next one.
func TestLockedOutProbeKeepsTheCadenceAfterAKeychainFailure(t *testing.T) {
	f, m, clk, ev := lockedOutRig(t)
	f.mu.Lock()
	f.err = creds.ErrKeychainUnavailable
	f.mu.Unlock()
	tryWarm(m, clk, 16*time.Minute)
	got := ev.last(t)
	if got.Detail != "keychain unavailable" || got.RetryIn != 15*time.Minute {
		t.Fatalf("event = %+v, want a retry in 15m", got)
	}
	n := f.refreshCount()
	tryWarm(m, clk, time.Minute)
	if f.refreshCount() != n {
		t.Fatal("probed again inside MaxBackoff after a keychain failure")
	}
}

func TestLockedOutProbeKeepsTheCadenceAfterATimeout(t *testing.T) {
	f, m, clk, ev := lockedOutRig(t)
	f.mu.Lock()
	f.refreshFunc = func(context.Context) error { return context.DeadlineExceeded }
	f.mu.Unlock()
	tryWarm(m, clk, 16*time.Minute)
	if got := ev.last(t); got.RetryIn != 15*time.Minute {
		t.Fatalf("event = %+v, want a retry in 15m", got)
	}
	n := f.refreshCount()
	tryWarm(m, clk, time.Minute)
	if f.refreshCount() != n {
		t.Fatal("probed again inside MaxBackoff after a timeout")
	}
}

func TestNotRenewedEventCarriesTheTriesAndSpan(t *testing.T) {
	_, _, _, ev := lockedOutRig(t)
	got := ev.last(t)
	if !got.NeedsLogin || got.Tries != 3 || got.Span != 12*time.Minute {
		t.Fatalf("event = %+v, want 3 tries over 12m", got)
	}
}

// F269: a slot with no login at all says so in the event, so the daemon can
// record needs-login; a transient read error (above) does not.
func TestAMissingLoginIsFlaggedNoLoginOnTheRefreshEvent(t *testing.T) {
	clk := newSyncClock(t0)
	f := &fakeSlot{err: creds.ErrNoLogin}
	ev := newEvents()
	m := newObservedManager(f, clk, ev)
	// A needs-login slot is not warmed, so ForceRefresh is the path that
	// still attempts it.
	m.ForceRefresh(context.Background(), slot)
	got := ev.last(t)
	if got.Outcome != tokens.OutcomeFailed || got.Detail != "no usable login: no-login" || !got.NoLogin {
		t.Fatalf("event = %+v, want a failed refresh flagged NoLogin", got)
	}
}
