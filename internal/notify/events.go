package notify

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultFlapWindow is the least time between two all-limited notices.
const DefaultFlapWindow = 10 * time.Minute

// LimitState is the limit roll-up at one moment, as the daemon's roster
// tick sees it.
type LimitState struct {
	AllLimited       bool
	NextReset        time.Time // zero: unknown
	NextResetAccount string
	Available        string // an account that is not limited now; "" if none is known
}

// Config wires Events to the daemon. Every field but Emit may be zero.
type Config struct {
	// Emit receives each notice that fires. The daemon passes
	// Dispatcher.Enqueue. It is called without Events' lock held.
	Emit func(title, body string)
	// Enabled is read each time a notice would fire (D12), so a change to
	// state.json's switch applies without a restart. nil means on. Limits
	// calls it with Events' lock held: it must be quick and must not call
	// back into Events.
	Enabled func() bool
	// Now is the clock for the flap window. nil means time.Now.
	Now func() time.Time
	// FlapWindow is the least time between all-limited notices. 0 means
	// DefaultFlapWindow.
	FlapWindow time.Duration
	// Location formats reset times. nil means time.Local.
	Location *time.Location
}

type limitPhase int

const (
	phaseUnknown limitPhase = iota
	phaseAvailable
	phaseAllLimited
)

// Events is the notification state machine for one daemon generation
// (D11). Its methods are safe for concurrent use: NeedsLogin and AccountOK
// run on request goroutines, Limits and RouteDrift on the roster tick.
//
// A notice that would fire while notifications are off is consumed, not
// deferred: turning them back on never replays it. That is distinct from a
// notice the flap guard suppresses (see Limits): that one is deferred, not
// consumed.
type Events struct {
	cfg Config

	// pendingCount lets AccountOK skip the mutex and the allocation on the
	// common path where nothing is pending. Changed only under mu, in step
	// with needsLogin, mirroring blockedCount in
	// internal/cli/notify_daemon.go.
	pendingCount atomic.Int32
	mu           sync.Mutex
	needsLogin   map[string]bool // lowercased name: notice fired, account not yet seen ok
	phase        limitPhase
	episodeSent  bool      // this all-limited episode's notice was actually emitted
	lastAllSent  time.Time // when the last all-limited notice fired (attempted)
	pendingAll   bool      // the flap guard suppressed this episode's notice; it is still owed
	driftSent    bool
	noCandSent   bool // this no-candidate episode's notice was attempted (M4)
}

// NewEvents returns a state machine with nothing yet fired.
func NewEvents(cfg Config) *Events {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FlapWindow == 0 {
		cfg.FlapWindow = DefaultFlapWindow
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	return &Events{cfg: cfg, needsLogin: map[string]bool{}}
}

// NeedsLogin reports that the selector found account's token needs a
// login. It fires once per account, and again only after AccountOK.
func (e *Events) NeedsLogin(account string) {
	if account == "" {
		return
	}
	key := strings.ToLower(account)
	e.mu.Lock()
	if e.needsLogin[key] {
		e.mu.Unlock()
		return
	}
	e.needsLogin[key] = true
	e.pendingCount.Add(1)
	e.mu.Unlock()
	e.fire(needsLoginMessage(account))
}

// AccountOK reports that account answered a request on its own credential,
// which re-arms its needs-login notice. The fast path (nothing pending)
// never takes the lock or lowercases account: a lock-free read of 0 just
// orders this call before a concurrent NeedsLogin, which is race-free by
// construction since pendingCount is only ever changed under mu.
func (e *Events) AccountOK(account string) {
	if e.pendingCount.Load() == 0 {
		return
	}
	key := strings.ToLower(account)
	e.mu.Lock()
	if e.needsLogin[key] {
		delete(e.needsLogin, key)
		e.pendingCount.Add(-1)
	}
	e.mu.Unlock()
}

// RouteDrift reports the daemon's route-drift counter. The notice fires the
// first time it is above zero, once per generation.
func (e *Events) RouteDrift(n uint64) {
	if n == 0 {
		return
	}
	e.mu.Lock()
	if e.driftSent {
		e.mu.Unlock()
		return
	}
	e.driftSent = true
	e.mu.Unlock()
	e.fire(routeDriftMessage())
}

// Limits reports the roll-up.
//   - "All limited" fires on a flip to true, unless the last one fired
//     within FlapWindow. A flip the flap guard suppresses is deferred, not
//     dropped: it fires on a later call, still within the same episode,
//     once FlapWindow has elapsed since the last one actually fired and
//     the roll-up is still all-limited. If the episode ends first (a flip
//     to available), the deferred notice is dropped: there is nothing left
//     to say.
//   - "Available again" fires on the flip back, only for an episode whose
//     all-limited notice was actually emitted (not swallowed by Enabled
//     being off): an "all limited" consumed while notifications are off
//     must never be followed by a visible "available again" with no
//     visible "all limited" (PF3).
//   - A first report of "not all limited" only sets the baseline. A first
//     report of "all limited" fires: a restart may repeat a still-true
//     notice once (D11).
//
// The decision (what Enabled() says right now) and the state it leaves
// behind (episodeSent, pendingAll) are settled in one critical section, so
// a concurrent call never sees them disagree; only the Emit callback itself
// runs unlocked. Unlike AccountOK, Enabled() is not hoisted above the lock
// here: whether it is even consulted depends on which switch case runs, so
// calling it upfront would read it on every tick instead of only when a
// notice would fire, changing Config.Enabled's documented contract.
func (e *Events) Limits(s LimitState) {
	var title, body string
	var doEmit, tryAvailable bool
	e.mu.Lock()
	now := e.cfg.Now()
	fireAllLimited := func() {
		e.pendingAll = false
		e.lastAllSent = now
		title, body = allLimitedMessage(s, e.cfg.Location)
		doEmit = e.enabledLocked()
		e.episodeSent = doEmit
	}
	switch {
	case s.AllLimited && e.phase != phaseAllLimited:
		e.phase = phaseAllLimited
		if !e.lastAllSent.IsZero() && now.Sub(e.lastAllSent) < e.cfg.FlapWindow {
			e.episodeSent = false
			e.pendingAll = true
			break
		}
		fireAllLimited()
	case s.AllLimited && e.phase == phaseAllLimited && e.pendingAll && now.Sub(e.lastAllSent) >= e.cfg.FlapWindow:
		fireAllLimited()
	case !s.AllLimited && e.phase == phaseAllLimited:
		e.phase = phaseAvailable
		e.pendingAll = false
		if e.episodeSent {
			e.episodeSent = false
			title, body = availableMessage(s.Available)
			tryAvailable = true
		}
	case !s.AllLimited:
		e.phase = phaseAvailable
		e.pendingAll = false
	}
	if tryAvailable {
		doEmit = e.enabledLocked()
	}
	e.mu.Unlock()

	if doEmit && e.cfg.Emit != nil {
		e.cfg.Emit(title, body)
	}
}

// Switch describes one auto-switch, for its notice (M4 spec §7).
type Switch struct {
	From, To string
	Trigger  string  // "limit" | "threshold"
	Window   string  // "5h" | "7d"; "" when unknown
	Pct      float64 // the utilization that triggered a threshold switch
	// Retried reports that the request that hit the wall was resent on To
	// and went through (§4a): the notice then drops the resend hint.
	Retried bool
}

// Moved describes the sessions an account lost under the spread policy.
type Moved struct {
	From     string
	To       []string // up to three accounts they will most likely move to
	Sessions int      // sessions placed on From when it crossed
	Window   string   // "5h" | "7d"; "" when unknown
	Limited  bool     // From hit its limit, rather than a switch point
}

// Moved posts the spread notice: one per event, never any state kept.
func (e *Events) Moved(m Moved) {
	e.fire(movedMessage(m))
}

// Switched posts an auto-switch's notice and ends any no-candidate
// episode. A switch made while every account is limited replaces that
// episode's "available again" (S8): the switch notice already says an
// account is available, so Limits' next flip to available posts nothing.
// The replacement applies whether or not notifications are on.
func (e *Events) Switched(s Switch) {
	e.EndAllLimitedEpisode()
	e.fire(switchedMessage(s))
}

// EndAllLimitedEpisode clears the "all accounts limited" and no-candidate
// episode bookkeeping Switched would, without emitting anything. A switch
// whose own notice is deferred (a wall retry, M4 spec §4a, waits for the
// resend's outcome before it fires) must still end the episode the moment
// the switch itself happens: a roster tick that lands between the switch
// and the deferred notice calls Limits with the episode still open
// otherwise, and re-posts "available again" a moment before "switched to
// B" (S8, review round 1 item 1).
func (e *Events) EndAllLimitedEpisode() {
	e.mu.Lock()
	e.noCandSent = false
	if e.phase == phaseAllLimited {
		e.episodeSent = false
		e.pendingAll = false
	}
	e.mu.Unlock()
}

// NoCandidate reports that auto-switch had to leave from and found no
// account to go to. It posts once per episode, and never while every
// account is limited: M2b's "all accounts limited" already says so (S8).
// An episode that begins while all are limited and outlasts that (the
// other accounts still above their switch points, say) posts then.
func (e *Events) NoCandidate(from string, allLimited bool) {
	e.mu.Lock()
	if e.noCandSent || allLimited {
		e.mu.Unlock()
		return
	}
	e.noCandSent = true
	e.mu.Unlock()
	e.fire(noCandidateMessage(from))
}

// CandidateOK ends a no-candidate episode: the planner is content to stay,
// or found a target. The next NoCandidate posts again.
func (e *Events) CandidateOK() {
	e.mu.Lock()
	e.noCandSent = false
	e.mu.Unlock()
}

// enabledLocked reads the on/off switch (D12). Call only with e.mu held:
// Limits evaluates it in the same critical section that records
// episodeSent, so the two can never disagree under concurrent access.
func (e *Events) enabledLocked() bool {
	return e.cfg.Enabled == nil || e.cfg.Enabled()
}

// fire emits a notice unless notifications are off, and reports whether it
// did. The decision to fire is already recorded by the caller; an "off"
// here consumes the notice. NeedsLogin and RouteDrift use it; their dedup
// flag has no dependency on whether the notice actually emits, so it needs
// no locked evaluation the way Limits' episodeSent does.
func (e *Events) fire(title, body string) bool {
	if e.cfg.Enabled != nil && !e.cfg.Enabled() {
		return false
	}
	if e.cfg.Emit != nil {
		e.cfg.Emit(title, body)
	}
	return true
}
