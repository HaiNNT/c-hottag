package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/notify"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
)

// autoSwitcher is the daemon's auto-switch (M4 spec §4, §4a, §5). It asks
// autoswitch.Plan after every serving response (the usage hook), on every
// roster tick, and synchronously when a serving request hits the wall (the
// proxy's WallRetry hook); it writes serving with a compare-and-swap
// (store.SwapServing, S10), publishes status.json's auto object, logs to
// daemon.log and posts notices through the M2b dispatcher.
//
// Every decision runs under mu, so the hook, the tick and a wall retry
// never interleave: after one switches, the next sees the new serving
// account and cannot switch again off the old one.
type autoSwitcher struct {
	store      store.Store
	state      func() (store.State, error)
	invalidate func() // drops the state cache after a write of our own
	sink       *statusSink
	notify     *daemonNotify
	log        io.Writer
	now        func() time.Time
	// fake is the fake-util knob's re-applier (fakeutil_on.go): a no-op in
	// a release build. It runs before every decision.
	fake func(time.Time)

	// spread, if non-nil, is the spread policy's placement engine: while
	// state.json's policy is spread, evaluate marks accounts and keeps
	// serving at the new-session choice instead of switching (M7).
	spread *spreadEngine
	// spreadSeeded: the first spread evaluation after a start has run (it
	// only seeds marks). spreadLimitNoticed: accounts whose limit-hit retry
	// has been announced, until they are candidates again. Both under mu.
	spreadSeeded       bool
	spreadLimitNoticed map[string]bool

	mu   sync.Mutex
	burn autoswitch.Burn
	// poolAuto is the default pool's serial auto-switch state; others holds
	// every other pool's, created at its first evaluation (M8: each serial
	// pool is evaluated on its own).
	poolAuto
	others    map[string]*poolAuto
	published status.Auto // what the last decision wrote to status.json
}

// poolAuto is one pool's auto-switch memory. Under mu.
type poolAuto struct {
	lastSoft time.Time
	// lastSwitch is the pool's latest switch (status.json's auto view).
	lastSwitch *status.AutoSwitch
	// known is the pool's serving account as this switcher last saw or wrote
	// it. A different one in state.json is the user's choice (ruling 8). Not
	// seeded across a restart (controller ruling 2): it depends on THIS
	// switcher having itself last seen or written the current serving
	// account, which a fresh process has not.
	known string
	// knownDir is known's slot dir, as of the last evaluation: a serving
	// account whose name changed but whose dir did not was renamed, not
	// chosen (F171 review), so it must not set userChosen.
	knownDir   string
	userChosen bool
	// fallbackTarget reports that known holds the role only because a
	// LIMIT trigger's fallback put it there (item 1, review round 3): it
	// is fed into autoswitch.Input.FallbackTarget on every evaluation, set
	// from Decision.Fallback whenever trySwitch succeeds, and cleared
	// whenever known changes for any other reason (the same place
	// userChosen is recomputed, just above).
	fallbackTarget bool
	lastLogKey     string

	// swapBackoffKey/swapBackoffUntil/swapLastErr back off a repeated
	// swap failure that is not a lost compare-and-swap (state.json
	// itself unwritable, say, review round 1 item 5): re-attempting the
	// write, and re-running continuity to pick a target, on every response
	// or tick would otherwise busy-loop against a store that just failed.
	// swapBackoffKey is "From->Target"; a decision for a different pair
	// retries at once (the state changed), and the same pair retries once
	// swapBackoffUntil (the injected clock) has passed. swapLastErr dedupes
	// the failure log line to once per distinct message.
	swapBackoffKey   string
	swapBackoffUntil time.Time
	swapLastErr      string
}

// poolState is pool's auto-switch memory. Caller holds a.mu.
func (a *autoSwitcher) poolState(pool string) *poolAuto {
	if pool == store.DefaultPool {
		return &a.poolAuto
	}
	if a.others == nil {
		a.others = map[string]*poolAuto{}
	}
	p := a.others[pool]
	if p == nil {
		p = &poolAuto{}
		a.others[pool] = p
	}
	return p
}

// poolTag is the pool name a notice carries: only when the install has more
// than one pool, so a default-only install's notices are unchanged.
func poolTag(st store.State, pool string) string {
	if len(st.PoolNames()) > 1 {
		return pool
	}
	return ""
}

// sessionPool is the pool a request's session is in: its identity's pool, or
// default when it has none or state.json no longer has it.
func sessionPool(ctx context.Context, st store.State) string {
	if id, ok := proxy.IdentityFrom(ctx); ok && st.HasPool(id.Caller.Pool) {
		return id.Caller.Pool
	}
	return store.DefaultPool
}

// swapBackoff is how long a repeated SwapServing failure (not a lost
// compare-and-swap) is left alone before the next attempt (review round 1
// item 5).
const swapBackoff = time.Minute

func newAutoSwitcher(s store.Store, cache *store.Cache, sink *statusSink, dn *daemonNotify, log io.Writer, fake func(time.Time)) *autoSwitcher {
	if fake == nil {
		fake = func(time.Time) {}
	}
	a := &autoSwitcher{
		store: s, state: cache.State, invalidate: cache.Invalidate,
		sink: sink, notify: dn, log: log, now: time.Now, fake: fake,
	}
	// Restart (review round 1 item 4): seed lastSwitch, and — for a soft
	// (threshold) switch — lastSoft, from the sink's already-loaded
	// status.json. Without this a cooldown a soft switch started just
	// before the daemon restarted is silently forgotten, and status.json's
	// own view of the last switch reads as "none" for a moment even though
	// the switch itself is still standing. The first evaluate leaves both
	// alone unless it makes a switch of its own.
	if auto := sink.auto(); auto != nil {
		seed := func(p *poolAuto, ls *status.AutoSwitch) {
			if ls == nil {
				return
			}
			p.lastSwitch = ls
			if ls.Trigger == autoswitch.TriggerThreshold {
				p.lastSoft = ls.At
			}
		}
		seed(&a.poolAuto, auto.LastSwitch)
		for name, pa := range auto.Pools {
			seed(a.poolState(name), pa.LastSwitch)
		}
	}
	return a
}

// autoUsageHook wraps the usage hook (newUsageHook): after next has folded
// the response into the status cache, a 2xx clears a stale needs-login
// mark (ruling 7), and a response for the serving account feeds the burn
// rate and runs the planner. OnUsage fires only for a swapped response
// answered on the account's own credential (!Drift, F26), so nothing here
// is attributed to the wrong account. A nil a returns next unchanged.
func autoUsageHook(a *autoSwitcher, next func(string, int, http.Header)) func(string, int, http.Header) {
	if a == nil {
		return next
	}
	return func(account string, code int, h http.Header) {
		next(account, code, h)
		a.onUsage(account, code, h)
	}
}

func (a *autoSwitcher) onUsage(account string, code int, h http.Header) {
	if code >= 200 && code < 300 {
		a.sink.setNeedsLoginCleared(account, a.now())
	}
	st, err := a.state()
	if err != nil {
		return
	}
	if a.spread != nil && anySpread(st) {
		// Sessions sit on many accounts: any account's response can push it
		// over a point. Only the marks are refreshed here: serving is kept on
		// the roster tick, never by a write on the response path.
		a.spreadOnUsage(a.now())
	}
	if !servesASerialPool(st, account, a.spread != nil) {
		return
	}
	now := a.now()
	if w := usagehdr.Parse(h, now).FiveHour; w.HasUtilization {
		if acct, ok := findExact(&st, account); ok {
			a.mu.Lock()
			a.burn.Observe(account, unitsOf(*acct), w.Utilization*100, now)
			a.mu.Unlock()
		}
	}
	a.evaluate(now, false)
}

// anySpread reports whether some pool's policy is spread.
func anySpread(st store.State) bool {
	for _, p := range st.PoolNames() {
		if poolSpread(st, p) {
			return true
		}
	}
	return false
}

// servesASerialPool reports whether account is the serving account of a pool
// the serial auto-switch decides for (a spread pool's serving follows the
// placement engine, which spreadAvailable says exists).
func servesASerialPool(st store.State, account string, spreadAvailable bool) bool {
	for _, p := range st.PoolNames() {
		if spreadAvailable && poolSpread(st, p) {
			continue
		}
		if strings.EqualFold(account, st.PoolOf(p).Serving) {
			return true
		}
	}
	return false
}

// tick runs on the roster tick, before the notification roll-up (so a
// switch at a reset can replace "available again", S8). A nil a does
// nothing.
func (a *autoSwitcher) tick(now time.Time) {
	if a == nil {
		return
	}
	a.evaluate(now, false)
}

// wallRetry is proxy.Config.WallRetry (M4 spec §4a). It classifies the 429
// itself, records the limit, and runs the planner at once (a hard trigger,
// S7). It asks the proxy to re-choose when the 429 is a limit and
// auto-switch is on; the proxy resends only if serving is now another
// account. The switch's own daemon.log line is written at once (review
// round 1 item 2: it must exist even if the resend never confirms
// anything); only the notice, and a second log line about the resend
// itself, wait for done, so they can say what actually happened to the
// request (ruling 5, revised).
func (a *autoSwitcher) wallRetry(ctx context.Context, account string, h http.Header) (bool, func(string, int)) {
	now := a.now()
	v := usagehdr.Classify(http.StatusTooManyRequests, h, now)
	if !v.Limited {
		return false, nil
	}
	a.sink.observe(account, usagehdr.Parse(h, now), v)
	st, err := a.state()
	if err != nil {
		return false, nil
	}
	pool := sessionPool(ctx, st)
	if poolSpread(st, pool) && a.spread != nil {
		if id, ok := proxy.IdentityFrom(ctx); ok && id.Caller.SID != "" {
			return a.spreadWallRetry(pool, poolTag(st, pool), id.Caller.SID, account, now, string(planWindow(v.Window)))
		}
	}
	if !st.AutoOn() {
		return false, nil
	}
	var sw *status.AutoSwitch
	if strings.EqualFold(account, st.PoolOf(pool).Serving) {
		sw = a.evaluateFor(now, true, pool)
	}
	window := string(planWindow(v.Window))
	tag := poolTag(st, pool)
	return true, func(to string, code int) {
		a.retried(account, to, code, window, sw, pool, tag)
	}
}

// retried finishes a wall retry: the wall-retry line, for whatever the
// resend actually did, and the switch's own notice. A concurrent
// `chottag tag` can retarget the resend away from this switch's own target
// before the proxy's Choose ever runs (S10): to is then neither "" nor
// sw.To, and the notice must say where the request actually went, not
// claim this switch's own target (review round 1 item 2). lastSwitch is
// marked retried only when to really is this switch's own target — "count
// the resend as OK only when to equals the switch's target".
func (a *autoSwitcher) retried(from, to string, code int, window string, sw *status.AutoSwitch, pool, tag string) {
	if to != "" {
		fmt.Fprintln(a.log, wallRetryLine(from, to, code, window))
	}
	if sw == nil {
		return
	}
	ok := to != "" && code >= 200 && code < 300
	if ok && strings.EqualFold(to, sw.To) {
		a.mu.Lock()
		if p := a.poolState(pool); p.lastSwitch == sw {
			cp := *sw
			cp.Retried = true
			p.lastSwitch = &cp
			if pool == store.DefaultPool {
				a.published.LastSwitch = p.lastSwitch
			} else if pa, ok := a.published.Pools[pool]; ok {
				pa.LastSwitch = p.lastSwitch
				a.published.Pools[pool] = pa
			}
			a.sink.setAuto(a.published, true)
		}
		a.mu.Unlock()
	}
	dest, retried := sw.To, false
	if ok {
		dest, retried = to, true
	}
	a.notify.switched(notify.Switch{From: sw.From, To: dest, Trigger: sw.Trigger, Window: sw.Window, Pct: sw.Pct, Retried: retried, Pool: tag, Episode: pool})
}

// evaluate makes one decision and acts on it, for the default pool's
// deferNotice; see evaluateFor.
func (a *autoSwitcher) evaluate(now time.Time, deferNotice bool) *status.AutoSwitch {
	return a.evaluateFor(now, deferNotice, store.DefaultPool)
}

// evaluateFor decides for every pool, each on its own (M8): a serial pool
// with the planner over its members and its own serving account, a spread
// pool by keeping its serving at what a new session of the pool would get.
// With deferNotice (a wall retry) the switch's own log line is still written
// now, but the notice of pool want's switch is returned for the caller to
// emit once the resend's outcome is known (ruling 5, revised: review round 1
// item 2); the other pools' notices go out at once. status.json's auto
// object is the default pool's decision.
func (a *autoSwitcher) evaluateFor(now time.Time, deferNotice bool, want string) *status.AutoSwitch {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fake(now)
	st, err := a.state()
	if err != nil {
		return nil
	}
	for name := range a.others {
		if !st.HasPool(name) {
			delete(a.others, name) // a removed pool leaves no memory behind
			a.notify.candidateOK(name)
		}
	}
	f := a.sink.fileCopy()
	params := autoParams(st)
	var wantSw *status.AutoSwitch
	var def status.Auto
	var others map[string]status.PoolAuto
	anySw, marked := false, false
	var limited map[string]autoswitch.Account
	for _, pool := range st.PoolNames() {
		v := poolView(st, pool)
		dn := deferNotice && pool == want
		var sw *status.AutoSwitch
		var pub status.Auto
		if poolSpread(st, pool) && a.spread != nil {
			if !marked {
				limited = a.spreadMarks(now, st, planAccounts(&st, &f, now), params)
				marked = true
			}
			sw, pub = a.evaluateSpread(now, dn, pool, v, limited, params, poolTag(st, pool))
		} else {
			sw, pub = a.evaluateSerial(now, dn, pool, v, planAccounts(&v, &f, now), params, poolTag(st, pool))
		}
		if sw != nil {
			anySw = true
			if pool == want {
				wantSw = sw
			}
		}
		if pool == store.DefaultPool {
			def = pub
		} else {
			if others == nil {
				others = map[string]status.PoolAuto{}
			}
			others[pool] = status.PoolAuto{Decision: pub.Decision, UserChosen: pub.UserChosen, LastSwitch: a.poolState(pool).lastSwitch}
		}
	}
	a.published = status.Auto{
		Mode: string(params.Mode), Decision: def.Decision, LastSwitch: a.lastSwitch,
		UserChosen: def.UserChosen, BurnRate: a.burn.Rate(), Pools: others,
	}
	a.sink.setAuto(a.published, anySw)
	return wantSw
}

// evaluateSerial is one serial pool's decision. v is the pool's view
// (poolView) and accts the planner's view of its members. tag is the pool
// name for notices ("" with a single pool). Caller holds a.mu.
func (a *autoSwitcher) evaluateSerial(now time.Time, deferNotice bool, pool string, v store.State, accts []autoswitch.Account, params autoswitch.Params, tag string) (*status.AutoSwitch, status.Auto) {
	p := a.poolState(pool)
	// The user's choice (S6, ruling 8): a serving account this switcher did
	// not write, at or above a switch point when chosen. The guard lasts
	// until serving changes again or the account drops below every point.
	dir := ""
	if sv, ok := findExact(&v, v.Serving); ok {
		dir = sv.Dir
	}
	switch {
	case !strings.EqualFold(v.Serving, p.known) && dir != "" && dir == p.knownDir:
		// A rename of the account this switcher already knew: same slot,
		// new name. Nothing was chosen, so the guards stand as they were,
		// except that a guard whose account dropped below every point
		// clears now, as it would have without the rename.
		p.known = v.Serving
		if p.userChosen && !overAnyPoint(accts, v.Serving, params, now) {
			p.userChosen = false
		}
	case !strings.EqualFold(v.Serving, p.known):
		p.userChosen = p.known != "" && overAnyPoint(accts, v.Serving, params, now)
		p.fallbackTarget = false
		p.known = v.Serving
	case p.userChosen && !overAnyPoint(accts, v.Serving, params, now):
		p.userChosen = false
	}
	p.knownDir = dir
	// fallbackTarget must not outlive the condition it records (item 1,
	// review round 4): it silences a THRESHOLD no-candidate only because
	// serving is ITSELF above its own point — the moment serving drops
	// below every point (its window resets, say), that silence has no
	// justification left, even though serving has not changed. Without
	// this, a later re-crossing with every other account still over its
	// own point stays silent forever, since the flag only ever cleared on
	// a serving change.
	if p.fallbackTarget && !overAnyPoint(accts, v.Serving, params, now) {
		p.fallbackTarget = false
	}

	d := autoswitch.Plan(autoswitch.Input{
		Now: now, Enabled: v.AutoOn(), Params: params, Serving: v.Serving, Accounts: accts,
		LastSoftSwitch: p.lastSoft, UserChosen: p.userChosen, FallbackTarget: p.fallbackTarget, Burn: a.burn.Rate(),
	})
	var sw *status.AutoSwitch
	switch d.Action {
	case autoswitch.ActionSwitch:
		sw = a.trySwitch(now, deferNotice, pool, p, &d, params, tag)
	case autoswitch.ActionNoCandidate:
		a.notify.noCandidate(pool, tag, d.From, a.sink.limitsAt(now).AllLimited)
		a.logChange(p, pool, d)
	case autoswitch.ActionStay, autoswitch.ActionHold:
		a.notify.candidateOK(pool)
		a.logChange(p, pool, d)
	case autoswitch.ActionOff:
		p.lastLogKey = ""
	}
	return sw, status.Auto{Decision: d.Reason, UserChosen: p.userChosen}
}

// swapFailedHarmlessly reports a swap error that resolves itself on the next
// decision and needs no backoff: a concurrent change of the pool's serving,
// or of its membership (a racing `pool leave`, `pool rm`).
func swapFailedHarmlessly(err error) bool {
	return errors.Is(err, store.ErrServingChanged) || errors.Is(err, store.ErrNoPool) || errors.Is(err, store.ErrNotInPool)
}

// trySwitch is evaluateSerial's ActionSwitch case: it writes the pool's
// serving through the compare-and-swap, backing off a repeated non-CAS
// failure (review round 1 item 5), and on success logs the switch, ends any
// all-limited/no-candidate episode at once (review round 1 item 1) and,
// unless the notice itself is deferred, posts it. Caller holds a.mu; d is
// mutated (Reason) for the caller to publish.
func (a *autoSwitcher) trySwitch(now time.Time, deferNotice bool, pool string, p *poolAuto, d *autoswitch.Decision, params autoswitch.Params, tag string) *status.AutoSwitch {
	key := d.From + "->" + d.Target
	if key == p.swapBackoffKey && now.Before(p.swapBackoffUntil) {
		// Still backing off a non-CAS failure to write this exact switch:
		// skip the write (and re-running continuity to pick it) until the
		// decision changes or the backoff elapses.
		d.Reason = fmt.Sprintf("staying on %s (switch to %s not made: could not write state)", d.From, d.Target)
		return nil
	}
	swapped, err := a.store.SwapPoolServing(pool, d.From, d.Target)
	if err != nil {
		a.invalidate()
		if swapFailedHarmlessly(err) {
			// A concurrent `chottag tag` won (S10), and the next decision
			// sees the new serving account as the user's choice. Nothing is
			// announced, and this is not the kind of failure that needs a
			// backoff: it resolves itself on the very next decision.
			p.swapBackoffKey, p.swapBackoffUntil, p.swapLastErr = "", time.Time{}, ""
			d.Reason = fmt.Sprintf("staying on %s (switch to %s not made: serving changed)", d.From, d.Target)
			return nil
		}
		p.swapBackoffKey, p.swapBackoffUntil = key, now.Add(swapBackoff)
		if msg := err.Error(); msg != p.swapLastErr {
			fmt.Fprintf(a.log, "chottag: auto-switch %s -> %s failed: %v\n", d.From, d.Target, err)
			p.swapLastErr = msg
		}
		d.Reason = fmt.Sprintf("staying on %s (switch to %s not made: could not write state)", d.From, d.Target)
		return nil
	}
	a.invalidate()
	p.swapBackoffKey, p.swapLastErr = "", ""
	p.known, p.userChosen, p.fallbackTarget = d.Target, false, d.Fallback
	p.knownDir = ""
	if t, ok := findExact(&swapped, d.Target); ok {
		p.knownDir = t.Dir
	}
	if d.Trigger == autoswitch.TriggerThreshold {
		p.lastSoft = now
	}
	sw := &status.AutoSwitch{From: d.From, To: d.Target, Trigger: d.Trigger, Window: string(d.Window), At: now}
	if d.Trigger == autoswitch.TriggerThreshold {
		sw.Pct = d.Pct
	}
	p.lastSwitch = sw
	p.lastLogKey = ""
	fmt.Fprintln(a.log, autoSwitchLine(*d, params)+poolLabel(pool))
	// S8 (review round 1 item 1): end the episode the moment the switch
	// happens, not when its own (possibly deferred) notice fires.
	a.notify.endAllLimitedEpisode(pool)
	if !deferNotice {
		a.notify.switched(notify.Switch{From: sw.From, To: sw.To, Trigger: sw.Trigger, Window: sw.Window, Pct: sw.Pct, Pool: tag, Episode: pool})
	}
	return sw
}

// logChange writes one daemon.log line when the planner starts holding or
// finds no candidate (spec §7: one line per hold decision change), and
// nothing for a plain stay. Caller holds a.mu.
func (a *autoSwitcher) logChange(p *poolAuto, pool string, d autoswitch.Decision) {
	key := string(d.Action) + "|" + d.Hold + "|" + strings.ToLower(d.From)
	if key == p.lastLogKey {
		return
	}
	p.lastLogKey = key
	if d.Action == autoswitch.ActionHold || d.Action == autoswitch.ActionNoCandidate {
		fmt.Fprintf(a.log, "chottag: auto: %s%s\n", d.Reason, poolLabel(pool))
	}
}

// overAnyPoint reports whether name's fresh usage is at or above one of
// its switch points (a window whose reset has passed reads as refilled).
func overAnyPoint(accts []autoswitch.Account, name string, p autoswitch.Params, now time.Time) bool {
	for _, pa := range accts {
		if strings.EqualFold(pa.Name, name) {
			return pa.Fresh && autoswitch.Eligible(autoswitch.Account{
				Rotates: true, Tier: pa.Tier, Fresh: true,
				Has5h: pa.Has5h, Pct5h: pa.Pct5h, Reset5h: pa.Reset5h,
				Has7d: pa.Has7d, Pct7d: pa.Pct7d, Reset7d: pa.Reset7d,
			}, p, now) == autoswitch.ReasonAboveSwitch
		}
	}
	return false
}

// autoSwitchLine is the daemon.log line for a switch (spec §7), e.g.
// "chottag: auto-switch A -> B (threshold 5h 96% >= 93%, balanced,
// continuity +3.5h)". No token, no body.
func autoSwitchLine(d autoswitch.Decision, p autoswitch.Params) string {
	why := "limit"
	if d.Trigger == autoswitch.TriggerThreshold {
		why = fmt.Sprintf("threshold %s %.0f%% >= %d%%", d.Window, d.Pct, d.Point)
	} else if d.Window != "" {
		why += " " + string(d.Window)
	}
	why += ", " + string(p.Mode)
	if p.Order == autoswitch.OrderContinuity {
		why += fmt.Sprintf(", continuity %+.1fh", d.Gain.Hours())
	}
	line := fmt.Sprintf("chottag: auto-switch %s -> %s (%s)", d.From, d.Target, why)
	if d.Fallback {
		// item 7 (review round 3): the target was picked by the LIMIT
		// trigger's fallback (no candidate was below its own switch
		// point), not the ordinary rule — worth a reader's notice.
		line += " (fallback)"
	}
	return line
}

// wallRetryLine is spec §4a's daemon.log line, e.g. "chottag: wall retry
// A -> B 200 (limit 5h)". A transport failure on the resend shows status 0.
func wallRetryLine(from, to string, code int, window string) string {
	why := "limit"
	if window != "" {
		why += " " + window
	}
	return fmt.Sprintf("chottag: wall retry %s -> %s %d (%s)", from, to, code, why)
}

// switched, noCandidate, candidateOK and endAllLimitedEpisode forward to
// notify.Events. A nil dn does nothing.
func (dn *daemonNotify) switched(s notify.Switch) {
	if dn != nil {
		dn.events.Switched(s)
	}
}

func (dn *daemonNotify) moved(m notify.Moved) {
	if dn != nil {
		dn.events.Moved(m)
	}
}

func (dn *daemonNotify) noCandidate(pool, tag, from string, allLimited bool) {
	if dn != nil {
		dn.events.NoCandidate(pool, tag, from, allLimited)
	}
}

func (dn *daemonNotify) candidateOK(pool string) {
	if dn != nil {
		dn.events.CandidateOK(pool)
	}
}

// endAllLimitedEpisode forwards to notify.Events.EndAllLimitedEpisode (S8,
// review round 1 item 1): called the moment a switch happens, even one
// whose own notice is deferred (a wall retry), so a roster tick landing
// before the deferred notice does not re-post "available again".
func (dn *daemonNotify) endAllLimitedEpisode(pool string) {
	if dn != nil {
		dn.events.EndAllLimitedEpisode(pool)
	}
}

// fileCopy returns a copy of the cached document that shares nothing
// mutable with it: the accounts slice and each row's usage are copied, so
// the planner can read it without the sink's lock while Observe keeps
// writing (Observe replaces the percentage pointers rather than writing
// through them).
func (c *statusSink) fileCopy() status.File {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.file
	f.Accounts = make([]status.Account, len(c.file.Accounts))
	copy(f.Accounts, c.file.Accounts)
	for i := range f.Accounts {
		if u := f.Accounts[i].Usage; u != nil {
			cp := *u
			f.Accounts[i].Usage = &cp
		}
	}
	f.Auto, f.Daemon, f.Trace = nil, nil, nil
	return f
}

// auto returns a copy of the currently cached auto view, nil if none is
// stored. Unlike fileCopy, it does not clear it: it is the auto-switcher's
// own startup seed (review round 1 item 4), reading the prior daemon
// generation's status.json before the first decision runs.
func (c *statusSink) auto() *status.Auto {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file.Auto == nil {
		return nil
	}
	a := *c.file.Auto
	if a.LastSwitch != nil {
		ls := *a.LastSwitch
		a.LastSwitch = &ls
	}
	return &a
}

// setAuto publishes the daemon's auto-switch view. Only a change is
// written: flushed at once after a switch (it is what the user is about to
// look at), otherwise within the sink's usual coalescing window.
func (c *statusSink) setAuto(a status.Auto, flush bool) {
	c.mu.Lock()
	changed := c.file.SetAuto(a)
	c.mu.Unlock()
	switch {
	case changed && flush:
		c.flush()
	case changed:
		c.maybeSave()
	}
}

// setUpdate publishes the update-check cache (R124). Only a change is
// written, at once: it is rare, and it is what a notice just announced. u is
// copied: the sink never shares a pointer with its caller. A nil u is
// ignored.
func (c *statusSink) setUpdate(u *status.Update) {
	if u == nil {
		return
	}
	cp := *u
	if u.Auto != nil {
		a := *u.Auto
		cp.Auto = &a
	}
	c.mu.Lock()
	changed := c.file.Update == nil || !sameUpdate(*c.file.Update, cp)
	if changed {
		c.file.Update = &cp
	}
	c.mu.Unlock()
	if changed {
		c.flush()
	}
}

// updateCopy returns a copy of the update-check cache the daemon last
// published, with a newer check `chottag update --check` wrote to disk since
// merged in (mergeCLIUpdateLocked). The zero value when there is none. Only
// the update loop calls it: the disk is read without the lock held.
func (c *statusSink) updateCopy() status.Update {
	d := c.loadUpdate(c.path)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mergeCLIUpdateLocked(d)
	if c.file.Update == nil {
		return status.Update{}
	}
	u := *c.file.Update
	if u.Auto != nil {
		a := *u.Auto
		u.Auto = &a
	}
	return u
}

// loadDiskUpdate is the default loadUpdate: the `update` in the status.json
// at path, or nil when there is none or the file cannot be read.
func loadDiskUpdate(path string) *status.Update {
	f, err := status.Load(path)
	if err != nil || f.Update == nil {
		return nil
	}
	return f.Update
}

// mergeCLIUpdateLocked keeps a check `chottag update --check` wrote to
// status.json when it is newer (by CheckedAt) than the sink's own, so the
// sink's next write does not overwrite it. It adopts only the fields the CLI
// owns (Latest, PublishedAt, CheckedAt, Available, Error): Auto stays
// the daemon's, since a CLI copy can predate the daemon's own newer write;
// Notified is adopted only when newer. Call with c.mu held. It reports whether it
// changed c.file.
func (c *statusSink) mergeCLIUpdateLocked(d *status.Update) bool {
	if d == nil {
		return false
	}
	if c.file.Update != nil && !d.CheckedAt.After(c.file.Update.CheckedAt) {
		return false
	}
	u := status.Update{}
	if c.file.Update != nil {
		u = *c.file.Update
	}
	u.Latest, u.PublishedAt, u.CheckedAt, u.Available, u.Error = d.Latest, d.PublishedAt, d.CheckedAt, d.Available, d.Error
	// A notice `update --check` gave shows at once, but never moves Notified
	// back: only a newer version replaces it (run/update-notified decides).
	if d.Notified != "" && (u.Notified == "" || updatecheck.Newer(d.Notified, u.Notified)) {
		u.Notified = d.Notified
	}
	c.file.Update = &u
	return true
}

// sameUpdate reports whether two cache values are equal, comparing times
// by instant.
func sameUpdate(a, b status.Update) bool {
	if (a.Auto == nil) != (b.Auto == nil) {
		return false
	}
	if a.Auto != nil && (a.Auto.Version != b.Auto.Version || !a.Auto.At.Equal(b.Auto.At) || a.Auto.OK != b.Auto.OK || a.Auto.Error != b.Auto.Error) {
		return false
	}
	return a.Latest == b.Latest && a.PublishedAt.Equal(b.PublishedAt) && a.CheckedAt.Equal(b.CheckedAt) &&
		a.Available == b.Available && a.Notified == b.Notified && a.Error == b.Error
}

// setNeedsLoginCleared replaces a recorded needs-login token state with ok
// once the account has answered a request on its own credential (ruling 7):
// eligibility reads this state, and nothing else ever cleared it. It goes
// through status.File.SetToken, not a direct field write, so TokenAt is
// stamped only when the state actually changes (planAccounts' before/after
// comparison against LoggedInAt depends on that).
func (c *statusSink) setNeedsLoginCleared(account string, at time.Time) {
	c.mu.Lock()
	cleared := false
	for _, a := range c.file.Accounts {
		if strings.EqualFold(a.Name, account) && a.Token == creds.StateNeedsLogin {
			cleared = true
			break
		}
	}
	if cleared {
		c.file.SetToken(account, creds.StateOK, at)
	}
	c.mu.Unlock()
	if cleared {
		c.flush()
	}
}
