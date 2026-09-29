package autoswitch

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Account is one registered account as the planner sees it, in
// registration order. The caller fills it from state.json and status.json.
type Account struct {
	Name    string
	Tier    Tier
	Units   float64 // capacity units; 0 means Tier.Units()
	Rotates bool    // not excluded by `chottag rotate NAME off` (R29)
	// NeedsLogin is status.json's token state for the account.
	NeedsLogin bool
	// Limited and LimitedUntil are knownLimit's answer (R54): a known
	// limit, with its reset when known.
	Limited      bool
	LimitedUntil time.Time
	// LimitWindow is the window that limited it ("5h", "7d"), or "".
	LimitWindow Window
	// Fresh reports whether the usage below is newer than
	// status.StaleAfter. Stale or unknown usage counts as below every
	// switch point for the SERVING account's own leave decision (R54,
	// overPoint). For TARGET eligibility it is narrower (item 1, review
	// round 2, targetOverPoint): a stale reading at or above its point
	// still counts as over when the window's reset is known and still
	// ahead, since the reading can only have grown since it was taken.
	Fresh            bool
	Has5h, Has7d     bool    // the server reported a utilization
	Pct5h, Pct7d     float64 // 0-100
	Reset5h, Reset7d time.Time
}

// units is the account's capacity units, defaulting to its tier's.
func (a Account) units() float64 {
	if a.Units > 0 {
		return a.Units
	}
	return a.Tier.Units()
}

func (a Account) pct(w Window) (float64, bool) {
	if w == Win7d {
		return a.Pct7d, a.Has7d
	}
	return a.Pct5h, a.Has5h
}

func (a Account) reset(w Window) time.Time {
	if w == Win7d {
		return a.Reset7d
	}
	return a.Reset5h
}

// effective is a's utilization for w, treating a window whose reset has
// already passed (at or before now) as freshly refilled: 0%, known. A
// switch point exists to leave before a wall; once the wall's own window
// has reset there is nothing left to leave for, so the account counts as
// refilled rather than still over its point.
func (a Account) effective(w Window, now time.Time) (float64, bool) {
	pct, ok := a.pct(w)
	if !ok {
		return pct, ok
	}
	if r := a.reset(w); !r.IsZero() && !r.After(now) {
		return 0, true
	}
	return pct, ok
}

// The reasons Eligible gives, which `chottag next` shows per skipped account.
const (
	ReasonOutOfRotation = "out of rotation"
	ReasonNeedsLogin    = "needs login"
	ReasonLimited       = "limited"
	ReasonAboveSwitch   = "above switch point"
)

// Eligible is the one rule for "may this account take the serving role?",
// shared by `chottag next` and the planner (M4 spec §5, S3): in rotation,
// not needing a login, not known to be limited, and below its own switch
// points — via targetOverPoint, not overPoint, so a stale reading is a
// lower bound, not "unknown": one at or above its point still counts as
// above it when the window's reset is known and still ahead (item 1,
// review round 2). It returns "" when eligible, else the reason.
//
// This is TARGET eligibility only: it says nothing about the fallback
// (item 2, review round 3) that `chottag next` and a LIMIT trigger reach
// for when Eligible rejects every account — an account "above switch
// point" here can still end up serving, picked as the least-bad option by
// FallbackEligible/Fallback instead.
func Eligible(a Account, p Params, now time.Time) string {
	switch {
	case !a.Rotates:
		return ReasonOutOfRotation
	case a.NeedsLogin:
		return ReasonNeedsLogin
	case a.Limited:
		return ReasonLimited
	}
	if _, _, over := targetOverPoint(a, p, now); over {
		return ReasonAboveSwitch
	}
	return ""
}

// overPoint returns the first window (5h, then 7d) whose fresh utilization
// is at or above the account's switch point, or is itself at the 100% wall
// (whatever the point is set to, since a full window can never serve). A
// window whose reset has already passed counts as refilled, not over. It
// decides whether the SERVING account should leave (M4 spec §4), which
// requires fresh usage: stale usage never triggers a leave.
func overPoint(a Account, p Params, now time.Time) (Window, int, bool) {
	if !a.Fresh {
		return "", 0, false
	}
	for _, w := range []Window{Win5h, Win7d} {
		pt := p.SwitchPoint(w, a.Tier)
		pct, ok := a.effective(w, now)
		if !ok {
			continue
		}
		if pct >= 100 || (pt < MaxSwitchPoint && pct >= float64(pt)) {
			return w, pt, true
		}
	}
	return "", 0, false
}

// targetOverPoint is overPoint's TARGET-eligibility variant (item 1, review
// round 2): usage never falls before a reset, so a stale reading is a lower
// bound. A fresh account behaves exactly like overPoint. A stale account's
// reading at or above its point still counts as over when the window's
// reset is known and still ahead (non-zero and after now) — the reading can
// only have grown since it was taken. A stale reading whose reset is
// unknown, or has already passed, stays a "below" (R54's "stale = unknown"
// narrows to this case for target eligibility and scoring; it still holds
// unchanged for LIMITS). Without this, balanced mode treats a stale
// over-point account as an eligible target and ping-pongs onto it once the
// cooldown ends.
func targetOverPoint(a Account, p Params, now time.Time) (Window, int, bool) {
	if a.Fresh {
		return overPoint(a, p, now)
	}
	for _, w := range []Window{Win5h, Win7d} {
		pct, ok := a.pct(w)
		if !ok {
			continue
		}
		if r := a.reset(w); r.IsZero() || !r.After(now) {
			continue // unknown, or already refilled: not a lower bound we can use
		}
		pt := p.SwitchPoint(w, a.Tier)
		if pct >= 100 || (pt < MaxSwitchPoint && pct >= float64(pt)) {
			return w, pt, true
		}
	}
	return "", 0, false
}

// Input is everything one planning decision needs (M4 spec §2, §9).
type Input struct {
	Now     time.Time
	Enabled bool
	Params  Params
	Serving string
	// Accounts is every registered account, in registration order.
	Accounts []Account
	// LastSoftSwitch is when the planner last made a threshold switch.
	LastSoftSwitch time.Time
	// UserChosen reports that the user put Serving there (tag/next) while
	// it was already at or above a switch point (S6).
	UserChosen bool
	// FallbackTarget reports that Serving holds the role only because a
	// LIMIT trigger's fallback put it there (item 1, review round 3): no
	// candidate was below its own switch point, so Serving was picked
	// despite being above its. The daemon remembers this across
	// evaluations until Serving changes by any means. A THRESHOLD
	// evaluation that finds no candidate while this holds must not report
	// ActionNoCandidate — every other account is in exactly the same
	// above-point state Serving itself is already tolerating, so nothing
	// has actually gotten worse — and a hard limit still finds its way to
	// this same fallback, unaffected (M4 spec review round 3, item 1).
	FallbackTarget bool
	// Burn is the measured burn rate in units per active hour; 0 uses
	// DefaultBurn.
	Burn float64
}

// Action is what the planner decided.
type Action string

const (
	ActionOff         Action = "off"
	ActionStay        Action = "stay"
	ActionHold        Action = "hold"
	ActionSwitch      Action = "switch"
	ActionNoCandidate Action = "no-candidate"
)

// Hold kinds, for ActionHold.
const (
	HoldCache    = "cache"
	HoldCooldown = "cooldown"
	HoldUser     = "user"
)

// Triggers, for ActionSwitch and ActionNoCandidate.
const (
	TriggerLimit     = "limit"
	TriggerThreshold = "threshold"
)

// Decision is one planning result.
type Decision struct {
	Action  Action
	From    string // the serving account the decision is about
	Target  string // ActionSwitch only
	Trigger string // TriggerLimit | TriggerThreshold, for a switch or no candidate
	Hold    string // HoldCache | HoldCooldown | HoldUser, for ActionHold
	Window  Window // the window that triggered, or the one reported for a stay
	Pct     float64
	Point   int
	// ResetIn is how long until Window resets (cache hold), and CooldownLeft
	// how much of the cooldown is left (cooldown hold).
	ResetIn      time.Duration
	CooldownLeft time.Duration
	// Continuity is the target's continuity score, and Gain how much it
	// beats the registration-order candidate by (balanced only).
	Continuity time.Duration
	Gain       time.Duration
	// Fallback reports that ActionSwitch's target was picked by the LIMIT
	// trigger's fallback (item 1, review round 3): no candidate was below
	// its own switch point, so Target was picked despite being above it.
	Fallback bool
	// Reason is the one-line summary `chottag auto` and `chottag status`
	// show, e.g. "holding A (5h 96%, resets in 9m)".
	Reason string
}

// Plan decides for the serving account, in the order of M4 spec §4:
// a limit leaves at once; a switch point leaves unless a hold applies;
// otherwise stay.
func Plan(in Input) Decision {
	d := Decision{From: in.Serving}
	if !in.Enabled {
		d.Action, d.Reason = ActionOff, "off"
		return d
	}
	s, ok := find(in.Accounts, in.Serving)
	if !ok {
		d.Action, d.Reason = ActionStay, "no serving account"
		return d
	}
	if s.Limited {
		d.Trigger, d.Window = TriggerLimit, s.LimitWindow
		return in.leave(d, s)
	}
	w, pt, over := overPoint(s, in.Params, in.Now)
	if !over {
		d.Action = ActionStay
		d.Window, d.Pct, d.Point = stayWindow(s, in.Params, in.Now)
		d.Reason = stayReason(s, d)
		return d
	}
	pct, _ := s.pct(w)
	d.Trigger, d.Window, d.Pct, d.Point = TriggerThreshold, w, pct, pt
	if in.UserChosen {
		d.Action, d.Hold = ActionHold, HoldUser
		d.Reason = fmt.Sprintf("holding %s (%s %.0f%%, chosen by you)", s.Name, w, pct)
		return d
	}
	if resetIn, ok := cacheHold(s, in.Params, in.Now); ok {
		d.Action, d.Hold, d.ResetIn = ActionHold, HoldCache, resetIn
		d.Reason = fmt.Sprintf("holding %s (%s %.0f%%, resets in %s)", s.Name, w, pct, shortDur(resetIn))
		return d
	}
	if !in.LastSoftSwitch.IsZero() {
		// A LastSoftSwitch in the future (a clock hiccup, or a caller that
		// has not happened yet in a test) has already, trivially, elapsed:
		// since < 0 never extends the cooldown.
		if since := in.Now.Sub(in.LastSoftSwitch); since >= 0 && since < in.Params.Cooldown {
			d.Action, d.Hold, d.CooldownLeft = ActionHold, HoldCooldown, in.Params.Cooldown-since
			d.Reason = fmt.Sprintf("holding %s (%s %.0f%%, cooldown %s left)", s.Name, w, pct, shortDur(d.CooldownLeft))
			return d
		}
	}
	d = in.leave(d, s)
	if d.Action == ActionNoCandidate && d.Trigger == TriggerThreshold && in.FallbackTarget {
		// s itself is already above its own point (that's how we got
		// here), and it holds the role only because a LIMIT trigger's
		// fallback put it there: every other account is in exactly the
		// same above-point state s is already tolerating, so nothing has
		// actually gotten worse (item 1, review round 3). Read as a stay,
		// not a no-candidate: no notice, and no daemon.log line (an
		// ordinary stay gets neither).
		d.Action = ActionStay
		d.Reason = fmt.Sprintf("staying on %s (fallback; no account below its switch point)", s.Name)
	}
	return d
}

// cacheHold applies when every window at or above its switch point resets
// within the mode's hold time for that window (R70).
func cacheHold(s Account, p Params, now time.Time) (time.Duration, bool) {
	var longest time.Duration
	held := false
	for _, w := range []Window{Win5h, Win7d} {
		pt := p.SwitchPoint(w, s.Tier)
		pct, ok := s.effective(w, now)
		if !ok || pt >= MaxSwitchPoint || pct < float64(pt) {
			continue
		}
		r := s.reset(w)
		in := r.Sub(now)
		if r.IsZero() || in <= 0 || in > p.Hold(w) {
			return 0, false
		}
		held = true
		longest = max(longest, in)
	}
	return longest, held
}

// leave picks a target for a limit or threshold trigger. A LIMIT trigger
// that finds no candidate below its own switch points falls back to any
// account with capacity at all (item 2, review round 2): a THRESHOLD
// switch never falls back, since there was no wall forcing the move.
func (in Input) leave(d Decision, s Account) Decision {
	target, score, gain, ok := in.pickTarget(s)
	fellBack := false
	if !ok && d.Trigger == TriggerLimit {
		var cands []Account
		for _, a := range afterServing(in.Accounts, s.Name) {
			if FallbackEligible(a, in.Now) {
				cands = append(cands, a)
			}
		}
		target, score, gain, ok = Fallback(cands, in.Accounts, in.Params, in.Burn, in.Now)
		fellBack = ok
	}
	if !ok {
		d.Action = ActionNoCandidate
		d.Reason = fmt.Sprintf("no account to switch to from %s (%s)", s.Name, triggerText(d))
		return d
	}
	d.Action, d.Target, d.Continuity, d.Gain, d.Fallback = ActionSwitch, target, score, gain, fellBack
	if fellBack {
		d.Reason = fmt.Sprintf("switched %s -> %s (%s; %s above its switch point)", s.Name, target, triggerText(d), target)
	} else {
		d.Reason = fmt.Sprintf("switched %s -> %s (%s)", s.Name, target, triggerText(d))
	}
	return d
}

// FallbackEligible is item 2's fallback rule (review round 2): when a hard
// request to move — the planner's own LIMIT trigger, or `next` without
// --force — finds no candidate below its own switch points, any account
// that still has capacity at all is better than leaving the user stuck on
// one that cannot serve. It ignores switch points entirely (that is the
// point of the fallback): in rotation, not needing a login, not known
// limited, and below 100% (effective, reset-aware) in every window.
func FallbackEligible(a Account, now time.Time) bool {
	if !a.Rotates || a.NeedsLogin || a.Limited {
		return false
	}
	for _, w := range []Window{Win5h, Win7d} {
		if pct, ok := a.effective(w, now); ok && pct >= 100 {
			return false
		}
	}
	return true
}

// Fallback picks item 2's fallback target among cands (FallbackEligible
// accounts, in the order to prefer them on a tie — registration order
// after the account leaving, wrapping). roster is every registered
// account (the account leaving included), used to build the continuity
// simulation for balanced mode; it may equal cands. cache-optimize keeps
// cands[0] (registration order); balanced picks the highest continuity
// score, ties broken by remaining capacity then position in cands — the
// same rule as pickTarget/pickBest, shared rather than copied (S3-like).
func Fallback(cands, roster []Account, p Params, burn float64, now time.Time) (string, time.Duration, time.Duration, bool) {
	if len(cands) == 0 {
		return "", 0, 0, false
	}
	if p.Order == OrderRegistration {
		return cands[0].Name, 0, 0, true
	}
	scores := make([]time.Duration, len(cands))
	for i, c := range cands {
		scores[i] = Continuity(simOrderFor(c, roster, now), burn, now, Horizon)
	}
	best := pickBest(cands, scores, now)
	return cands[best].Name, scores[best], gain(scores, best), true
}

func triggerText(d Decision) string {
	if d.Trigger == TriggerLimit {
		if d.Window == "" {
			return "limit"
		}
		return "limit " + string(d.Window)
	}
	return fmt.Sprintf("threshold %s %.0f%% >= %d%%", d.Window, d.Pct, d.Point)
}

// pickTarget chooses among the eligible accounts other than s: the next in
// registration order (cache-optimize), or the highest continuity score,
// ties to the most unit-weighted remaining capacity, then registration
// order (balanced).
func (in Input) pickTarget(s Account) (string, time.Duration, time.Duration, bool) {
	var cands []Account
	for _, a := range afterServing(in.Accounts, s.Name) {
		if Eligible(a, in.Params, in.Now) == "" {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return "", 0, 0, false
	}
	if in.Params.Order == OrderRegistration {
		return cands[0].Name, 0, 0, true
	}
	scores := make([]time.Duration, len(cands))
	for i, c := range cands {
		scores[i] = Continuity(in.simOrder(c), in.Burn, in.Now, Horizon)
	}
	best := pickBest(cands, scores, in.Now)
	return cands[best].Name, scores[best], gain(scores, best), true
}

// gain is how much best's score beats scores[0] (the registration-order
// candidate) by, floored at 0: pickBest may choose a candidate slightly
// below the band's maximum (still within a minute of it) because it has
// more remaining capacity, and that must never read as a loss.
func gain(scores []time.Duration, best int) time.Duration {
	return max(scores[best]-scores[0], 0)
}

// pickBest picks the winner among cands (in registration order) and their
// matching continuity scores: the highest score, ties within a minute of
// it broken by the most unit-weighted remaining capacity, a further tie
// keeping registration order. Finding the band's true maximum first, rather
// than comparing each candidate only to the current best, keeps the choice
// transitive: a chain of near-ties can't walk it away from the top score.
func pickBest(cands []Account, scores []time.Duration, now time.Time) int {
	maxScore := scores[0]
	for _, sc := range scores[1:] {
		if sc > maxScore {
			maxScore = sc
		}
	}
	best := -1
	for i := range cands {
		if scores[i] < maxScore-time.Minute {
			continue
		}
		if best < 0 || remaining(cands[i], now) > remaining(cands[best], now) {
			best = i
		}
	}
	return best
}

// remaining is unit-weighted capacity left in the tighter window, using
// each window's effective (refilled-aware) utilization rather than a
// possibly stale raw reading: a candidate whose window has already reset
// must not be under-rated against one that is genuinely still using it.
func remaining(a Account, now time.Time) float64 {
	pct5, _ := a.effective(Win5h, now)
	pct7, _ := a.effective(Win7d, now)
	return a.units() * (100 - max(pct5, pct7))
}

// simOrder puts first at the front of the simulation, followed by every
// other account that could serve (in rotation, logged in), the serving
// account included.
func (in Input) simOrder(first Account) []SimAccount {
	return simOrderFor(first, in.Accounts, in.Now)
}

// simOrderFor is simOrder's free-function form, shared with Fallback
// (item 2, review round 2): first at the front, followed by every other
// account in roster that could serve (in rotation, logged in).
func simOrderFor(first Account, roster []Account, now time.Time) []SimAccount {
	out := []SimAccount{simAccount(first, now)}
	for _, a := range roster {
		if strings.EqualFold(a.Name, first.Name) || !a.Rotates || a.NeedsLogin {
			continue
		}
		out = append(out, simAccount(a, now))
	}
	return out
}

func (in Input) sim(a Account) SimAccount { return simAccount(a, in.Now) }

func simAccount(a Account, now time.Time) SimAccount {
	sa := SimAccount{Units: a.units(), Reset5h: a.Reset5h, Reset7d: a.Reset7d}
	// A NaN percentage (a corrupt read) counts as unknown, like a window
	// with no data at all: unconstrained rather than poisoning the sim.
	if a.Has5h && !math.IsNaN(a.Pct5h) {
		sa.Pct5h = a.Pct5h
	}
	if a.Has7d && !math.IsNaN(a.Pct7d) {
		sa.Pct7d = a.Pct7d
	}
	if a.Limited {
		sa.BlockedUntil = a.LimitedUntil
		if sa.BlockedUntil.IsZero() {
			sa.BlockedUntil = a.reset(a.LimitWindow)
		}
		if sa.BlockedUntil.IsZero() {
			sa.BlockedUntil = now.Add(Horizon)
		}
	}
	return sa
}

// afterServing lists the accounts in registration order starting after
// serving, wrapping, and leaves serving itself out.
func afterServing(accts []Account, serving string) []Account {
	start := 0
	for i, a := range accts {
		if strings.EqualFold(a.Name, serving) {
			start = i + 1
			break
		}
	}
	var out []Account
	for i := 0; i < len(accts); i++ {
		a := accts[(start+i)%len(accts)]
		if !strings.EqualFold(a.Name, serving) {
			out = append(out, a)
		}
	}
	return out
}

func find(accts []Account, name string) (Account, bool) {
	if name == "" {
		return Account{}, false
	}
	for _, a := range accts {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Account{}, false
}

// stayWindow picks the window to report for a stay: the fresh window
// closest to its switch point.
func stayWindow(s Account, p Params, now time.Time) (Window, float64, int) {
	var bw Window
	var bpct float64
	var bpt int
	best := -1.0
	for _, w := range []Window{Win5h, Win7d} {
		pct, ok := s.effective(w, now)
		if !ok || !s.Fresh {
			continue
		}
		pt := p.SwitchPoint(w, s.Tier)
		if r := pct / float64(pt); r > best {
			best, bw, bpct, bpt = r, w, pct, pt
		}
	}
	return bw, bpct, bpt
}

func stayReason(s Account, d Decision) string {
	if d.Window == "" {
		return fmt.Sprintf("staying on %s (usage unknown)", s.Name)
	}
	return fmt.Sprintf("staying on %s (%s %.0f%% < %d%%)", s.Name, d.Window, d.Pct, d.Point)
}

// shortDur renders a duration as "9m", "2h5m" or "45s".
func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
