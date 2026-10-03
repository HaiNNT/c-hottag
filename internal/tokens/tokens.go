// Package tokens keeps each account slot's access token usable: it caches
// reads, classifies the token with creds.Assess and, when it has expired,
// asks the slot's own Claude Code login to refresh it — under that slot's
// lock, so the single-use refresh token is never used by two holders.
package tokens

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
)

// Refresher makes the login in slotDir refresh itself. It returns when the
// attempt is over; the manager then re-reads the slot.
type Refresher interface {
	Refresh(ctx context.Context, slotDir string) error
}

// Refresh triggers: what started a refresh attempt.
const (
	TriggerRequest = "request" // a request found the token stale
	TriggerWarm    = "warm"    // the warm loop
	TriggerForced  = "forced"  // ForceRefresh, after an upstream refusal
	TriggerWake    = "wake"    // the warm pass that follows a wake from sleep
)

// Refresh outcomes.
const (
	OutcomeRenewed    = "renewed"     // the slot now holds a usable token
	OutcomeNotRenewed = "not-renewed" // claude exited 0 and the token did not renew
	OutcomeFailed     = "failed"      // the attempt itself failed
	OutcomePanicked   = "panicked"    // the refresher panicked
)

// NotRenewingLimit is how many not-renewed outcomes in a row take a slot to
// needs-login (reason ReasonNotRenewing).
const NotRenewingLimit = 3

// LockoutSpan is the least time the not-renewed outcomes in a row must span,
// from the first to the latest, before the slot is locked out. A network that
// takes a few minutes to return after a wake makes claude exit 0 without
// renewing; three such tries in 90 seconds say nothing about the login.
const LockoutSpan = 10 * time.Minute

// KeychainFlatFor is how long a Keychain that stays unavailable is retried on
// the flat MinBackoff; after that the usual doubling applies, so a locked
// screen overnight does not spawn claude every 30 seconds.
const KeychainFlatFor = 5 * time.Minute

// ReasonNotRenewing is the needs-login reason of a slot whose refresh exits
// 0 NotRenewingLimit times in a row yet never renews its token.
const ReasonNotRenewing = "refresh-not-renewing"

// RefreshEvent is the outcome of one refresh attempt. It carries no token
// and no output of the child: Detail is a short reason.
type RefreshEvent struct {
	Account   string
	Trigger   string
	Outcome   string
	Detail    string
	Took      time.Duration
	ExpiresIn time.Duration // the slot's token expiry from now; negative once expired
	// RetryIn is when the next attempt may start, 0 when none is waited for.
	RetryIn time.Duration
	// NeedsLogin is set on the attempt that took the slot to needs-login
	// (ReasonNotRenewing).
	NeedsLogin bool
	// Tries and Span describe the run of not-renewed outcomes that ended
	// with this event (Tries of them, the first Span ago); 0 otherwise.
	Tries int
	Span  time.Duration
}

type Config struct {
	// OnRefresh, when set, is called once at the end of every refresh
	// attempt, outside every manager lock and after the slot is released. A
	// panic in it is contained. Events for one slot may therefore arrive out
	// of order: a later attempt can finish and report before an earlier one's
	// callback has run, so a consumer must not assume ordering.
	OnRefresh func(RefreshEvent)
	// AccountName names the account whose slot is dir, for RefreshEvent. The
	// default is the directory's base name.
	AccountName func(slotDir string) string

	Read     func(slotDir string) (creds.Token, error)
	Refresh  Refresher
	LockPath func(slotDir string) string
	TryLock  func(path string) (unlock func() error, ok bool, err error)
	Now      func() time.Time
	// ReadTTL is how long a read result is reused before the slot's store is
	// read again — a macOS Keychain read is not free. Default 30s.
	ReadTTL time.Duration
	// MinBackoff and MaxBackoff bound the wait between refresh attempts for a
	// slot whose refresh keeps failing. Defaults 30s and 15m.
	MinBackoff, MaxBackoff time.Duration
	// RefreshTimeout bounds one refresh attempt. It is applied to a context
	// DETACHED from the caller's: a refresh spawns the slot's own `claude`
	// and takes seconds, and the request that noticed the stale token has
	// usually given up long before it finishes. Letting the caller's
	// cancellation kill the child let one impatient client take the slot
	// out of service for every later request (F37). Default 2m —
	// comfortably above the 90s refresh.Claude allows itself, so this
	// never pre-empts the refresher's own timeout.
	RefreshTimeout time.Duration
}

type Manager struct {
	cfg   Config
	mu    sync.Mutex
	slots map[string]*slot
}

type slot struct {
	mu         sync.Mutex
	tok        creds.Token
	err        error
	readAt     time.Time
	status     creds.Status
	refreshing bool
	// done wakes anyone waiting in Await for the refresh in flight when it
	// started. A fresh channel is made (under s.mu) every time refreshing
	// flips to true — in both Token and ForceRefresh — and doRefresh's
	// first-registered defer closes it unconditionally, so a panic in the
	// refresher still wakes waiters instead of hanging them forever (see
	// that defer's own comment on why it runs last).
	done    chan struct{}
	backoff time.Duration
	nextTry time.Time
	// forceNextTry throttles repeated ForceRefresh calls, independently of
	// nextTry/backoff: it is set only after a SUCCESSFUL forced refresh and
	// is never read by Token's natural (StateStale) refresh path, so an
	// ordinary expired token is refreshed immediately regardless of it.
	forceNextTry time.Time
	// awaitNextTry throttles the refresh Await forces through a backoff
	// (R147): one attempt per MinBackoff, so a burst of remote requests for
	// a slot whose login is broken does not spawn a claude per request: at most
	// one forced attempt per forceEvery, measured from the last one.
	awaitNextTry time.Time
	// warmHold: Warm leaves the slot alone until then (see Warm).
	warmHold time.Time
	// notRenewing counts refreshes that exited 0 and left the token expired,
	// in a row. A usable token resets it; NotRenewingLimit of them set
	// lockedOut.
	notRenewing int
	// firstNotRenewed is when the current run of not-renewed outcomes began.
	firstNotRenewed time.Time
	// keychainSince is when the Keychain first failed in the current run of
	// keychain-unavailable outcomes; zero outside one.
	keychainSince time.Time
	// lockedOut: the slot reads as needs-login (ReasonNotRenewing) until its
	// token is usable again or Invalidate says it was logged in anew.
	lockedOut bool
}

// forceEvery is the least time between two refreshes Await forces through a
// backoff for one slot.
const forceEvery = time.Minute

// errSlotBusy means another holder (usually `chottag login`) has the slot
// lock, so this refresh attempt did not happen.
var errSlotBusy = errors.New("slot is locked by another holder")

func New(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReadTTL == 0 {
		cfg.ReadTTL = 30 * time.Second
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = 30 * time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 15 * time.Minute
	}
	if cfg.RefreshTimeout == 0 {
		cfg.RefreshTimeout = 2 * time.Minute
	}
	return &Manager{cfg: cfg, slots: map[string]*slot{}}
}

func (m *Manager) slotFor(dir string) *slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.slots[dir]
	if s == nil {
		s = &slot{}
		m.slots[dir] = s
	}
	return s
}

// Token returns a usable access token for slotDir. ok=false means the caller
// must pass the request through unchanged; the Status says why.
func (m *Manager) Token(ctx context.Context, slotDir string) (string, creds.Status, bool) {
	s := m.slotFor(slotDir)
	s.mu.Lock()
	now := m.cfg.Now()
	m.read(s, slotDir, now, false)
	st := s.status
	switch st.State {
	case creds.StateOK, creds.StateExpiring:
		token := s.tok.AccessToken
		s.mu.Unlock()
		return token, st, true
	case creds.StateNeedsLogin:
		// A locked-out slot (refresh-not-renewing) is still probed, one
		// refresh per MaxBackoff: a renewal lifts the lockout. Any other
		// needs-login is left alone.
		if !(s.lockedOut && !s.refreshing && !now.Before(s.nextTry)) {
			s.mu.Unlock()
			return "", st, false
		}
	}
	if s.refreshing || now.Before(s.nextTry) {
		s.mu.Unlock()
		return "", st, false
	}
	s.refreshing = true
	s.done = make(chan struct{})
	s.mu.Unlock()
	// Refresh in the background and pass THIS request through. doRefresh's
	// own doc already says requests for this slot "should pass through
	// meanwhile instead of queueing behind it" — the request that
	// triggered the refresh was the one exception, and that exception is
	// what F37 cost: a ~9s stall, and a client that gave up first killed
	// the child and took the slot out of service.
	go m.refreshDetached(context.WithoutCancel(ctx), s, slotDir, TriggerRequest)
	return "", st, false
}

// Await is Token's blocking counterpart. F166: a remote object was
// requested as the Home account because Home's own refresh was still
// running when Token's usual passthrough (StateStale, ok=false) chose it —
// the request landed on the wrong org, upstream returned a 403, and Claude
// Code dropped the artifact's watch for good. That is a real cost a
// passthrough on a serving (inference) request does not have, so the
// selector calls Await instead of Token for a remote- or owner-routed
// request. Waiting on an already-triggered background refresh is
// deliberately NOT what F37 removed: F37 was about a client killing the
// refresh itself by cancelling; Await still lets the refresh run detached
// (via Token's own call below) and only waits, bounded by ctx, for it to
// finish.
//
// R147: a slot backing off from a failed (or busy) refresh has nothing in
// flight, but a remote or owner request must not go out on Home's login
// either, so Await then forces one refresh past nextTry (single-flight, and
// at most one per minute), waits for it under ctx, and re-reads.
func (m *Manager) Await(ctx context.Context, slotDir string) (string, creds.Status, bool) {
	tok, st, ok := m.Token(ctx, slotDir)
	if ok || st.State == creds.StateNeedsLogin {
		return tok, st, ok
	}

	s := m.slotFor(slotDir)
	s.mu.Lock()
	if !s.refreshing && m.cfg.Now().Before(s.nextTry) && !m.cfg.Now().Before(s.awaitNextTry) {
		// R147: a remote or owner request must not go out on Home's login,
		// so a slot backing off from a failed (or busy) refresh gets one
		// forced attempt now, past nextTry: single-flight like any refresh
		// (s.refreshing), detached from this caller, and awaited below under
		// the caller's own bound. Throttled by awaitNextTry.
		s.awaitNextTry = m.cfg.Now().Add(forceEvery)
		s.refreshing = true
		s.done = make(chan struct{})
		go m.refreshDetached(context.WithoutCancel(ctx), s, slotDir, TriggerRequest)
	}
	if !s.refreshing {
		// Nothing (else) to wait for: either backing off from a recent
		// failure, or (the far narrower case) the Token call just above
		// already saw its own freshly-started refresh finish before we
		// re-took s.mu — which is exactly the F166 failure if left
		// unhandled: a fast refresh's fresh token would be dropped in favor
		// of the stale snapshot Token returned a moment earlier. Re-calling
		// Token here (instead of returning that snapshot) closes it: on a
		// completed success it serves the now-cached fresh token; while
		// backing off, or right after errSlotBusy, it returns !ok without
		// spawning (both leave s.nextTry in the future); after a
		// cancelled/no-backoff attempt it may start one more background
		// refresh and return !ok — acceptable, and this is a single extra
		// Token call, not a loop: it never calls back into Await.
		s.mu.Unlock()
		return m.Token(ctx, slotDir)
	}
	done := s.done
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return "", st, false
	case <-done:
		// The refresh that finished forced a re-read (doRefresh's own
		// m.read(..., err == nil)), so this reads the fresh result from
		// cache rather than starting another refresh.
		return m.Token(ctx, slotDir)
	}
}

// ForceRefresh refreshes slotDir's token unconditionally, ignoring what
// Assess says about the cached token: the proxy safety net calls this after
// a swapped request was refused upstream, when a token that still assesses
// ok/expiring locally may already be rejected there (revoked, org changed,
// clock skew), so a cache hit from Token would just repeat the same failing
// request.
//
// It shares Token's refreshing flag and nextTry backoff, so it coalesces
// with any refresh (forced or natural) already in flight for this slot
// instead of starting a second one: a 401 burst across several concurrent
// requests produces one refresh, not one per request. A forced refresh that
// cannot proceed (already refreshing, or still backing off from a recent
// failure) returns ok=false immediately rather than blocking the caller.
//
// A successful forced refresh also starts its own flat, non-growing
// forceNextTry cooldown (see slot.forceNextTry): a persistently-refused
// swapped route (spec §4.4 — the account genuinely cannot see the object,
// or the route table is wrong; a 404 is refused, and router.go classes any
// unrecognized api.anthropic.com route as Serving) would otherwise force a
// real `claude` subprocess spawn, a Keychain read and three upstream round
// trips on every single request, forever, because a locally-fine token
// always reports success here and success resets the SHARED nextTry to
// zero for Token's benefit. The cooldown is a separate field precisely so
// it never delays Token's own (StateStale) refresh of a genuinely expired
// token.
func (m *Manager) ForceRefresh(ctx context.Context, slotDir string) (string, bool) {
	s := m.slotFor(slotDir)
	s.mu.Lock()
	now := m.cfg.Now()
	if s.refreshing || now.Before(s.nextTry) || now.Before(s.forceNextTry) {
		s.mu.Unlock()
		return "", false
	}
	s.refreshing = true
	s.done = make(chan struct{})
	s.mu.Unlock()
	tok, _, ok := m.doRefresh(context.WithoutCancel(ctx), s, slotDir, true, TriggerForced)
	return tok, ok
}

// Warm refreshes slotDir's token now, in this goroutine, when it is stale or
// expires within the given window, so a remote account's token is fresh
// before a request needs it (R147). It shares Token's single-flight flag and
// failure backoff (a slot already refreshing, or backing off, is left
// alone), never refreshes a needs-login slot, and runs the refresh under the
// same panic guard as the background one. The refresh does not stop with ctx
// (F49): the caller cancels only its own scheduling.
//
// The refresher is Claude Code itself, which renews its token only close to
// expiry: a refresh that leaves ExpiresAt where it was is not a renewal, and
// the slot is then left alone until it is inside creds.ExpiringWithin of
// expiry (slot.warmHold), instead of spawning claude on every pass. It
// reports the slot's status and whether the token was renewed.
func (m *Manager) Warm(ctx context.Context, slotDir string, within time.Duration) (creds.Status, bool) {
	return m.WarmFor(ctx, slotDir, within, TriggerWarm)
}

// WarmFor is Warm with the trigger its RefreshEvent carries: TriggerWarm for
// the periodic loop, TriggerWake for the pass that follows a wake.
func (m *Manager) WarmFor(ctx context.Context, slotDir string, within time.Duration, trigger string) (creds.Status, bool) {
	s := m.slotFor(slotDir)
	s.mu.Lock()
	now := m.cfg.Now()
	m.read(s, slotDir, now, false)
	st := s.status
	if st.State == creds.StateNeedsLogin && !s.lockedOut {
		s.mu.Unlock()
		return st, false
	}
	due := st.State == creds.StateStale || s.lockedOut || s.tok.ExpiresAt.Sub(now) <= within
	if !due || s.refreshing || now.Before(s.nextTry) || now.Before(s.warmHold) {
		s.mu.Unlock()
		return st, false
	}
	before := s.tok.ExpiresAt
	s.refreshing = true
	s.done = make(chan struct{})
	s.mu.Unlock()
	// WithoutCancel (F49, as in refreshDetached): shutdown must not SIGKILL
	// claude mid-refresh, which can tear the token write.
	st, ok := m.runRefresh(context.WithoutCancel(ctx), s, slotDir, trigger)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ok {
		return st, false
	}
	if !s.tok.ExpiresAt.After(before) {
		s.warmHold = s.tok.ExpiresAt.Add(-creds.ExpiringWithin)
		return st, false
	}
	return st, true
}

// LockedOut lists the slot directories locked out for refresh-not-renewing,
// so a caller can probe them (WarmFor) whatever role their account has: a
// locked-out account is taken out of rotation, and nothing else would ever
// try it again.
func (m *Manager) LockedOut() []string {
	m.mu.Lock()
	dirs := make([]string, 0, len(m.slots))
	slots := make([]*slot, 0, len(m.slots))
	for d, s := range m.slots {
		dirs = append(dirs, d)
		slots = append(slots, s)
	}
	m.mu.Unlock()
	var out []string
	for i, s := range slots {
		s.mu.Lock()
		if s.lockedOut {
			out = append(out, dirs[i])
		}
		s.mu.Unlock()
	}
	sort.Strings(out)
	return out
}

// Status reports a slot's state without refreshing, so `chottag status`
// never blocks on a login.
func (m *Manager) Status(slotDir string) creds.Status {
	s := m.slotFor(slotDir)
	s.mu.Lock()
	defer s.mu.Unlock()
	m.read(s, slotDir, m.cfg.Now(), false)
	return s.status
}

// read refreshes the cached credential when the cache is older than ReadTTL
// or force is set, then re-assesses it against now. Caller holds s.mu.
func (m *Manager) read(s *slot, dir string, now time.Time, force bool) {
	if force || s.readAt.IsZero() || now.Sub(s.readAt) >= m.cfg.ReadTTL {
		s.tok, s.err = m.cfg.Read(dir)
		s.readAt = now
	}
	s.status = creds.Assess(s.tok, s.err, now)
	switch s.status.State {
	case creds.StateOK, creds.StateExpiring:
		// A usable token ends any run of non-renewals, and any lockout.
		s.lockedOut, s.notRenewing, s.firstNotRenewed = false, 0, time.Time{}
	case creds.StateStale:
		if s.lockedOut {
			s.status = creds.Status{State: creds.StateNeedsLogin, Reason: ReasonNotRenewing}
		}
	}
}

// refreshDetached runs one refresh on its own goroutine, which is the only
// place a panic from the refresher has nowhere to go: an unrecovered panic
// on a goroutine takes the whole daemon down, and a daemon that dies
// because one slot's login misbehaved fails every other account and every
// in-flight session too. On the synchronous ForceRefresh path a panic
// still propagates to the request goroutine, where net/http contains it —
// that behaviour is deliberately unchanged.
func (m *Manager) refreshDetached(ctx context.Context, s *slot, dir, trigger string) {
	m.runRefresh(ctx, s, dir, trigger)
}

// runRefresh is one refresh under the panic guard: see refreshDetached. On a
// panic it returns the zero Status and false, with the backoff set.
func (m *Manager) runRefresh(ctx context.Context, s *slot, dir, trigger string) (st creds.Status, ok bool) {
	start := m.cfg.Now()
	name := m.nameOf(dir)
	defer func() {
		if recover() == nil {
			return
		}
		// doRefresh's own defers have already cleared s.refreshing and
		// released the flock. What they cannot do is set the backoff: the
		// panic unwound past the error classification. Without this, a
		// refresher that panics every time is respawned on every request.
		//
		// s.mu.Unlock() is deferred, not called inline after setBackoff, so
		// a panic inside m.cfg.Now() or setBackoff itself cannot leave s.mu
		// locked forever — the exact class of bug this wrapper exists to
		// prevent, just one level deeper.
		ev := func() RefreshEvent {
			s.mu.Lock()
			defer s.mu.Unlock()
			now := m.cfg.Now()
			m.setBackoff(s, now)
			return RefreshEvent{
				Account: name, Trigger: trigger, Outcome: OutcomePanicked,
				Detail: "the refresher panicked", Took: now.Sub(start),
				ExpiresIn: expiresIn(s, now), RetryIn: s.nextTry.Sub(now),
			}
		}()
		m.emit(ev)
	}()
	_, st, ok = m.doRefresh(ctx, s, dir, false, trigger)
	return st, ok
}

// doRefresh runs one refresh attempt without holding s.mu: a refresh spawns
// the real claude and can take seconds, and requests for this slot should
// pass through meanwhile instead of queueing behind it. forced records
// whether this attempt came from ForceRefresh, so a success can start its
// own forceNextTry cooldown without touching Token's shared nextTry/backoff.
func (m *Manager) doRefresh(ctx context.Context, s *slot, dir string, forced bool, trigger string) (string, creds.Status, bool) {
	tok, st, ok, ev := m.attempt(ctx, s, dir, forced, trigger)
	// attempt has returned, so its defers have released the slot: the
	// callback's own I/O (status, the log) never holds up an Await or the
	// next refresh.
	m.emit(ev)
	return tok, st, ok
}

// attempt is doRefresh's body; it returns the event for doRefresh to emit.
func (m *Manager) attempt(ctx context.Context, s *slot, dir string, forced bool, trigger string) (string, creds.Status, bool, RefreshEvent) {
	// A panic inside m.cfg.Refresh.Refresh (e.g. from the exec plumbing)
	// must not strand this slot as permanently "refreshing": nothing would
	// ever refresh it again for the life of the process. This resets it
	// unconditionally, whether doRefresh returns normally or a panic keeps
	// unwinding past it. Registered first, so by defer LIFO ordering it
	// runs LAST — after both the flock's defer unlock() below and the
	// later defer s.mu.Unlock() — so it never re-locks a mutex this
	// goroutine already holds, and never races the flock release. It also
	// closes s.done, so any Await call waiting on this attempt wakes even
	// if the refresher itself panicked; being the LAST defer to run is
	// exactly why it's safe to close here unconditionally instead of
	// needing its own recover — every other defer above (the flock's
	// unlock, s.mu.Unlock()) has already run first.
	defer func() {
		s.mu.Lock()
		s.refreshing = false
		close(s.done)
		s.mu.Unlock()
	}()

	start := m.cfg.Now()
	name := m.nameOf(dir) // before s.mu: it may read state.json
	s.mu.Lock()
	before := s.tok.ExpiresAt
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, m.cfg.RefreshTimeout)
	defer cancel()

	var err error
	unlock, locked, lockErr := m.cfg.TryLock(m.cfg.LockPath(dir))
	switch {
	case lockErr != nil:
		err = lockErr
	case !locked:
		err = errSlotBusy
	default:
		// Deferred, not called inline after Refresh returns: a panic inside
		// Refresh (e.g. the exec plumbing) must not leak this slot's flock
		// for the life of the process. An inline unlock() after the call
		// only runs on the normal-return path; a panic would skip it, and
		// the kernel only releases a flock when the holding *os.File is
		// closed — which unlock() itself does — so nothing else would ever
		// free it. This is the surviving half of dd12e73's fix: that commit
		// restored s.refreshing on panic, but a leaked flock still made
		// every subsequent refresh for this slot return errSlotBusy
		// forever, with s.refreshing now (wrongly) looking healthy.
		defer unlock()
		err = m.cfg.Refresh.Refresh(ctx, dir)
	}

	// The slot's state is settled under s.mu; doRefresh emits the event once
	// the slot is released.
	return func() (string, creds.Status, bool, RefreshEvent) {
		s.mu.Lock()
		defer s.mu.Unlock()
		now := m.cfg.Now()
		m.read(s, dir, now, err == nil)
		// raw is what the credential says on its own; s.status may be
		// overridden to needs-login for a locked-out slot.
		raw := creds.Assess(s.tok, s.err, now)
		ev := RefreshEvent{
			Account: name, Trigger: trigger, Took: now.Sub(start),
			ExpiresIn: expiresIn(s, now),
		}
		usable := raw.State == creds.StateOK || raw.State == creds.StateExpiring
		if err == nil && usable {
			s.backoff, s.nextTry = 0, time.Time{}
			s.notRenewing, s.firstNotRenewed, s.keychainSince = 0, time.Time{}, time.Time{}
			if forced {
				s.forceNextTry = now.Add(m.cfg.MinBackoff)
			}
			if s.tok.ExpiresAt.After(before) {
				ev.Outcome = OutcomeRenewed
			} else {
				ev.Outcome, ev.Detail = OutcomeNotRenewed, "token unchanged, not yet due"
			}
			return s.tok.AccessToken, s.status, true, ev
		}
		keychain := errors.Is(err, creds.ErrKeychainUnavailable) || raw.Reason == "keychain-unavailable"
		notRenewed := err == nil && raw.State == creds.StateStale && raw.Reason == "expired"
		if !notRenewed {
			// "In a row" means consecutive attempts: any other outcome
			// breaks the run.
			s.notRenewing, s.firstNotRenewed = 0, time.Time{}
		}
		if !keychain {
			s.keychainSince = time.Time{}
		}
		switch {
		case errors.Is(err, errSlotBusy):
			// The lock's holder is `chottag login`, which ends with a fresh
			// token, so look again soon: a flat MinBackoff wait, and leave
			// s.backoff alone so a later genuine failure still starts its own
			// doubling from MinBackoff.
			s.nextTry = now.Add(m.cfg.MinBackoff)
			ev.Outcome, ev.Detail = OutcomeFailed, "slot is locked by another holder"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The attempt was cut short, which is no evidence that this slot's
			// login is broken. Backing off here is what let one impatient
			// client leave a pinned account on passthrough for a whole session
			// (F37). Leave backoff and nextTry untouched so the next request
			// retries immediately.
			ev.Outcome, ev.Detail = OutcomeFailed, "cancelled or timed out"
		case keychain:
			// The Keychain is locked or has no UI to prompt in, typically
			// right after a wake: it clears on its own, so retry on a flat
			// MinBackoff for the first KeychainFlatFor, then double as for
			// any failure.
			if s.keychainSince.IsZero() {
				s.keychainSince = now
			}
			if now.Sub(s.keychainSince) < KeychainFlatFor {
				s.nextTry = now.Add(m.cfg.MinBackoff)
			} else {
				m.setBackoff(s, now)
			}
			ev.Outcome, ev.Detail = OutcomeFailed, "keychain unavailable"
		case notRenewed:
			// claude exited 0, yet the token is still expired: no renewal.
			// NotRenewingLimit of these in a row, spanning LockoutSpan, mean
			// the login cannot renew (a dead refresh token, say), so the
			// slot becomes needs-login. A locked-out slot is probed once per
			// MaxBackoff, and a renewal lifts the lockout.
			if s.notRenewing == 0 {
				s.firstNotRenewed = now
			}
			s.notRenewing++
			m.setBackoff(s, now)
			ev.Outcome, ev.Detail = OutcomeNotRenewed, "claude exited 0 but the token is still expired"
			ev.Tries, ev.Span = s.notRenewing, now.Sub(s.firstNotRenewed)
			if s.lockedOut || (s.notRenewing >= NotRenewingLimit && now.Sub(s.firstNotRenewed) >= LockoutSpan) {
				ev.NeedsLogin = !s.lockedOut
				s.lockedOut = true
				s.backoff, s.nextTry = m.cfg.MaxBackoff, now.Add(m.cfg.MaxBackoff)
				s.status = creds.Status{State: creds.StateNeedsLogin, Reason: ReasonNotRenewing}
			}
		default:
			m.setBackoff(s, now)
			ev.Outcome = OutcomeFailed
			if err != nil {
				ev.Detail = failDetail(err)
			} else {
				ev.Detail = "no usable login: " + raw.Reason
			}
		}
		if s.lockedOut && !errors.Is(err, errSlotBusy) && s.nextTry.Before(now.Add(m.cfg.MaxBackoff)) {
			// A locked-out slot is probed once per MaxBackoff, whatever the
			// probe's outcome (a locked Keychain, a cancel, a timeout).
			s.nextTry = now.Add(m.cfg.MaxBackoff)
		}
		if !s.nextTry.IsZero() {
			ev.RetryIn = s.nextTry.Sub(now)
		}
		return "", s.status, false, ev
	}()
}

// expiresIn is how long the slot's cached token has left, negative once it
// has expired and 0 when there is no token. Caller holds s.mu.
func expiresIn(s *slot, now time.Time) time.Duration {
	if s.tok.ExpiresAt.IsZero() {
		return 0
	}
	return s.tok.ExpiresAt.Sub(now)
}

// nameOf names the account for dir. It runs outside every slot lock, since
// the daemon's AccountName reads state.json, and a panic in it must not
// escape (it can run on refreshDetached's goroutine): the directory's base
// name stands in.
func (m *Manager) nameOf(dir string) (name string) {
	name = filepath.Base(dir)
	if m.cfg.AccountName == nil {
		return name
	}
	defer func() {
		if recover() != nil {
			name = filepath.Base(dir)
		}
	}()
	return m.cfg.AccountName(dir)
}

// emit hands ev to OnRefresh. A panicking callback must not take the
// refresh (or, from refreshDetached's goroutine, the daemon) down with it.
func (m *Manager) emit(ev RefreshEvent) {
	if m.cfg.OnRefresh == nil {
		return
	}
	defer func() { _ = recover() }()
	m.cfg.OnRefresh(ev)
}

// failDetail is a short, secret-free reason for a failed attempt: the
// refresher's wrapped error ends with the child's own exit error ("exit
// status 1"), which is all that is kept. A token or bearer in the text
// (none is expected) is replaced.
func failDetail(err error) string {
	d := err.Error()
	if i := strings.LastIndex(d, ": "); i >= 0 {
		d = d[i+2:]
	}
	if strings.Contains(d, "sk-ant-") || strings.Contains(d, "Bearer") {
		return "refresh command failed"
	}
	if len(d) > 80 {
		d = d[:80]
	}
	return d
}

// InvalidateAll marks every slot's cached credential for re-reading before
// next use. The wake detector calls it: a token that was fine when the
// machine went to sleep may have expired during the suspend, and the
// ReadTTL cache would otherwise serve it until the TTL happened to lapse.
//
// It does not refresh anything, but it CAN block: it takes each slot's
// mu, and Manager.Token/Status hold that same mu across creds.ExecRunner's
// exec.Command(...).Output() (internal/creds/creds.go), which has no
// context and no timeout — on darwin that is `security
// find-generic-password`, which can sit waiting on a Keychain-unlock
// prompt. A caller on a latency-sensitive path (e.g. the wake detector's
// own OnWake, which runs synchronously on its select loop — see
// internal/cli/proxy.go's newWakeHandler) must not call this inline; run
// it on its own goroutine instead. The next Token call re-reads,
// re-assesses, and refreshes only if that says it must.
func (m *Manager) InvalidateAll() {
	m.mu.Lock()
	slots := make([]*slot, 0, len(m.slots))
	for _, s := range m.slots {
		slots = append(slots, s)
	}
	m.mu.Unlock()

	for _, s := range slots {
		s.mu.Lock()
		s.readAt = time.Time{}
		s.mu.Unlock()
	}
}

// Invalidate marks slotDir's cached credential for re-reading before its
// next use (item 6, review round 3): the daemon calls this the moment it
// sees that slot's LoggedInAt advance in the roster it already reloads, so a
// request within ReadTTL of a re-login is not served a needs-login read the
// ReadTTL cache took before the login completed.
//
// It is NOT safe to call inline (item 2, review round 4, correcting the
// earlier claim here): it takes slotDir's own s.mu, the same mutex
// Manager.Token/Status hold across creds.ExecRunner's exec.Command(...).
// Output() (internal/creds/creds.go) — no context, no timeout, and on
// darwin that is `security find-generic-password`, which can sit waiting on
// a Keychain-unlock prompt. It is one slot's own share of InvalidateAll's
// same loop and blocks exactly the way that does; InvalidateAll's own doc
// comment names the callers that must not call either of these inline (the
// wake detector's OnWake, and — since this was added — watchRoster's roster
// goroutine).
func (m *Manager) Invalidate(slotDir string) {
	s := m.slotFor(slotDir)
	s.mu.Lock()
	s.readAt = time.Time{}
	// A re-login (the caller's reason) gives the slot a new refresh token,
	// so a needs-login for refresh-not-renewing, and the retry wait that
	// went with it, no longer apply.
	s.notRenewing, s.firstNotRenewed = 0, time.Time{}
	if s.lockedOut {
		s.lockedOut = false
		s.backoff, s.nextTry = 0, time.Time{}
	}
	s.mu.Unlock()
}

// setBackoff doubles this slot's retry wait, bounded by MaxBackoff. Caller
// holds s.mu.
func (m *Manager) setBackoff(s *slot, now time.Time) {
	switch {
	case s.backoff == 0:
		s.backoff = m.cfg.MinBackoff
	case s.backoff < m.cfg.MaxBackoff:
		s.backoff *= 2
	}
	if s.backoff > m.cfg.MaxBackoff {
		s.backoff = m.cfg.MaxBackoff
	}
	s.nextTry = now.Add(s.backoff)
}
