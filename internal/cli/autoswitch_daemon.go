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
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
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

	mu       sync.Mutex
	burn     autoswitch.Burn
	lastSoft time.Time
	// known is the serving account as this switcher last saw or wrote it.
	// A different one in state.json is the user's choice (ruling 8). Not
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
	lastSwitch     *status.AutoSwitch
	published      status.Auto // what the last decision wrote to status.json

	// swapBackoffKey/swapBackoffUntil/swapLastErr back off a repeated
	// SwapServing failure that is not a lost compare-and-swap (state.json
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
	if auto := sink.auto(); auto != nil && auto.LastSwitch != nil {
		a.lastSwitch = auto.LastSwitch
		if auto.LastSwitch.Trigger == autoswitch.TriggerThreshold {
			a.lastSoft = auto.LastSwitch.At
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
	if err != nil || !strings.EqualFold(account, st.Serving) {
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
func (a *autoSwitcher) wallRetry(_ context.Context, account string, h http.Header) (bool, func(string, int)) {
	now := a.now()
	v := usagehdr.Classify(http.StatusTooManyRequests, h, now)
	if !v.Limited {
		return false, nil
	}
	a.sink.observe(account, usagehdr.Parse(h, now), v)
	st, err := a.state()
	if err != nil || !st.AutoOn() {
		return false, nil
	}
	var sw *status.AutoSwitch
	if strings.EqualFold(account, st.Serving) {
		sw = a.evaluate(now, true)
	}
	window := string(planWindow(v.Window))
	return true, func(to string, code int) {
		a.retried(account, to, code, window, sw)
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
func (a *autoSwitcher) retried(from, to string, code int, window string, sw *status.AutoSwitch) {
	if to != "" {
		fmt.Fprintln(a.log, wallRetryLine(from, to, code, window))
	}
	if sw == nil {
		return
	}
	ok := to != "" && code >= 200 && code < 300
	if ok && strings.EqualFold(to, sw.To) {
		a.mu.Lock()
		if a.lastSwitch == sw {
			cp := *sw
			cp.Retried = true
			a.lastSwitch = &cp
			a.published.LastSwitch = a.lastSwitch
			a.sink.setAuto(a.published, true)
		}
		a.mu.Unlock()
	}
	dest, retried := sw.To, false
	if ok {
		dest, retried = to, true
	}
	a.notify.switched(notify.Switch{From: sw.From, To: dest, Trigger: sw.Trigger, Window: sw.Window, Pct: sw.Pct, Retried: retried})
}

// evaluate makes one decision and acts on it. With deferNotice (a wall
// retry) the switch's own log line is still written now, but its notice is
// returned for the caller to emit once the resend's outcome is known
// (ruling 5, revised: review round 1 item 2).
func (a *autoSwitcher) evaluate(now time.Time, deferNotice bool) *status.AutoSwitch {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fake(now)
	st, err := a.state()
	if err != nil {
		return nil
	}
	f := a.sink.fileCopy()
	params := autoParams(st)
	accts := planAccounts(&st, &f, now)

	// The user's choice (S6, ruling 8): a serving account this switcher did
	// not write, at or above a switch point when chosen. The guard lasts
	// until serving changes again or the account drops below every point.
	dir := ""
	if sv, ok := findExact(&st, st.Serving); ok {
		dir = sv.Dir
	}
	switch {
	case !strings.EqualFold(st.Serving, a.known) && dir != "" && dir == a.knownDir:
		// A rename of the account this switcher already knew: same slot,
		// new name. Nothing was chosen, so the guards stand as they were,
		// except that a guard whose account dropped below every point
		// clears now, as it would have without the rename.
		a.known = st.Serving
		if a.userChosen && !overAnyPoint(accts, st.Serving, params, now) {
			a.userChosen = false
		}
	case !strings.EqualFold(st.Serving, a.known):
		a.userChosen = a.known != "" && overAnyPoint(accts, st.Serving, params, now)
		a.fallbackTarget = false
		a.known = st.Serving
	case a.userChosen && !overAnyPoint(accts, st.Serving, params, now):
		a.userChosen = false
	}
	a.knownDir = dir
	// fallbackTarget must not outlive the condition it records (item 1,
	// review round 4): it silences a THRESHOLD no-candidate only because
	// serving is ITSELF above its own point — the moment serving drops
	// below every point (its window resets, say), that silence has no
	// justification left, even though serving has not changed. Without
	// this, a later re-crossing with every other account still over its
	// own point stays silent forever, since the flag only ever cleared on
	// a serving change.
	if a.fallbackTarget && !overAnyPoint(accts, st.Serving, params, now) {
		a.fallbackTarget = false
	}

	d := autoswitch.Plan(autoswitch.Input{
		Now: now, Enabled: st.AutoOn(), Params: params, Serving: st.Serving, Accounts: accts,
		LastSoftSwitch: a.lastSoft, UserChosen: a.userChosen, FallbackTarget: a.fallbackTarget, Burn: a.burn.Rate(),
	})
	var sw *status.AutoSwitch
	switch d.Action {
	case autoswitch.ActionSwitch:
		sw = a.trySwitch(now, deferNotice, &d, params)
	case autoswitch.ActionNoCandidate:
		a.notify.noCandidate(d.From, a.sink.limitsAt(now).AllLimited)
		a.logChange(d)
	case autoswitch.ActionStay, autoswitch.ActionHold:
		a.notify.candidateOK()
		a.logChange(d)
	case autoswitch.ActionOff:
		a.lastLogKey = ""
	}
	a.published = status.Auto{
		Mode: string(params.Mode), Decision: d.Reason, LastSwitch: a.lastSwitch,
		UserChosen: a.userChosen, BurnRate: a.burn.Rate(),
	}
	a.sink.setAuto(a.published, sw != nil)
	return sw
}

// trySwitch is evaluate's ActionSwitch case: it writes serving through the
// compare-and-swap, backing off a repeated non-CAS failure (review round 1
// item 5), and on success logs the switch, ends any all-limited/
// no-candidate episode at once (review round 1 item 1) and — unless the
// notice itself is deferred — posts it. Caller holds a.mu; d is mutated
// (Reason) for the caller to publish.
func (a *autoSwitcher) trySwitch(now time.Time, deferNotice bool, d *autoswitch.Decision, params autoswitch.Params) *status.AutoSwitch {
	key := d.From + "->" + d.Target
	if key == a.swapBackoffKey && now.Before(a.swapBackoffUntil) {
		// Still backing off a non-CAS failure to write this exact switch:
		// skip the write (and re-running continuity to pick it) until the
		// decision changes or the backoff elapses.
		d.Reason = fmt.Sprintf("staying on %s (switch to %s not made: could not write state)", d.From, d.Target)
		return nil
	}
	swapped, err := a.store.SwapServing(d.From, d.Target)
	if err != nil {
		a.invalidate()
		if errors.Is(err, store.ErrServingChanged) {
			// A concurrent `chottag tag` won (S10), and the next decision
			// sees the new serving account as the user's choice. Nothing is
			// announced, and this is not the kind of failure that needs a
			// backoff: it resolves itself on the very next decision.
			a.swapBackoffKey, a.swapBackoffUntil, a.swapLastErr = "", time.Time{}, ""
			d.Reason = fmt.Sprintf("staying on %s (switch to %s not made: serving changed)", d.From, d.Target)
			return nil
		}
		a.swapBackoffKey, a.swapBackoffUntil = key, now.Add(swapBackoff)
		if msg := err.Error(); msg != a.swapLastErr {
			fmt.Fprintf(a.log, "chottag: auto-switch %s -> %s failed: %v\n", d.From, d.Target, err)
			a.swapLastErr = msg
		}
		d.Reason = fmt.Sprintf("staying on %s (switch to %s not made: could not write state)", d.From, d.Target)
		return nil
	}
	a.invalidate()
	a.swapBackoffKey, a.swapLastErr = "", ""
	a.known, a.userChosen, a.fallbackTarget = d.Target, false, d.Fallback
	a.knownDir = ""
	if t, ok := findExact(&swapped, d.Target); ok {
		a.knownDir = t.Dir
	}
	if d.Trigger == autoswitch.TriggerThreshold {
		a.lastSoft = now
	}
	sw := &status.AutoSwitch{From: d.From, To: d.Target, Trigger: d.Trigger, Window: string(d.Window), At: now}
	if d.Trigger == autoswitch.TriggerThreshold {
		sw.Pct = d.Pct
	}
	a.lastSwitch = sw
	a.lastLogKey = ""
	fmt.Fprintln(a.log, autoSwitchLine(*d, params))
	// S8 (review round 1 item 1): end the episode the moment the switch
	// happens, not when its own (possibly deferred) notice fires.
	a.notify.endAllLimitedEpisode()
	if !deferNotice {
		a.notify.switched(notify.Switch{From: sw.From, To: sw.To, Trigger: sw.Trigger, Window: sw.Window, Pct: sw.Pct})
	}
	return sw
}

// logChange writes one daemon.log line when the planner starts holding or
// finds no candidate (spec §7: one line per hold decision change), and
// nothing for a plain stay. Caller holds a.mu.
func (a *autoSwitcher) logChange(d autoswitch.Decision) {
	key := string(d.Action) + "|" + d.Hold + "|" + strings.ToLower(d.From)
	if key == a.lastLogKey {
		return
	}
	a.lastLogKey = key
	if d.Action == autoswitch.ActionHold || d.Action == autoswitch.ActionNoCandidate {
		fmt.Fprintf(a.log, "chottag: auto: %s\n", d.Reason)
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

func (dn *daemonNotify) noCandidate(from string, allLimited bool) {
	if dn != nil {
		dn.events.NoCandidate(from, allLimited)
	}
}

func (dn *daemonNotify) candidateOK() {
	if dn != nil {
		dn.events.CandidateOK()
	}
}

// endAllLimitedEpisode forwards to notify.Events.EndAllLimitedEpisode (S8,
// review round 1 item 1): called the moment a switch happens, even one
// whose own notice is deferred (a wall retry), so a roster tick landing
// before the deferred notice does not re-post "available again".
func (dn *daemonNotify) endAllLimitedEpisode() {
	if dn != nil {
		dn.events.EndAllLimitedEpisode()
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
// owns (Latest, PublishedAt, CheckedAt, Available, Error): Notified and
// Auto stay the daemon's, since a CLI copy of them can predate the
// daemon's own newer write. Call with c.mu held. It reports whether it
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
