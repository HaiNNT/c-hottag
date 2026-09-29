package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// skip records why one account was passed over. The reasons have
// different remedies — `chottag rotate`, `chottag login`, or waiting — so
// the message names which applied to which account. Reason is one of
// autoswitch's Reason* texts.
type skip struct {
	Name   string
	Reason string // "out of rotation" | "needs login" | "limited" | "above switch point"
	Until  time.Time
}

// nextCandidate picks the account that should take the serving role, in
// registration order starting after st.Serving.
//
// It asks autoswitch.Eligible, the one rule `next`, `logout --force` and
// the auto-switch planner share (M4 spec §5, S3): in rotation, not needing
// a login, not known to be limited, and below its own switch points on
// fresh usage. Two copies of the rule would drift, and the copy that
// drifts is the one that loses the edge case.
//
// ONLY A KNOWN LIMIT SKIPS (§6.3, R54/F161). A missing cache is unknown and
// stays eligible. Usage older than status.StaleAfter is unknown ONLY when
// the reset time is also unknown (zero LimitedUntil) — a known future
// LimitedUntil is honoured however stale the usage that reported it is; see
// knownLimit. For switch points, a stale reading at or above the point still
// counts as above it when the window's reset is known and still ahead — the
// reading can only have grown since it was taken (autoswitch.Account.Fresh,
// item 1, review round 2). A false "limited" silently moves the user off a
// healthy account and is hard to notice; a false "not limited" only means
// they meet the wall themselves.
//
// force ignores limits, switch points and login state but still honours
// rotation: an account the user deliberately excluded is not a fallback.
//
// The third return value reports whether the candidate was picked via the
// fallback below: it is above its own switch point, not literally skipped,
// so the caller must not print it as a skip (item 2, review round 3).
func nextCandidate(st *store.State, f *status.File, now time.Time, force bool) (store.Account, []skip, bool, error) {
	params := autoParams(*st)
	pas := planAccounts(st, f, now)
	view := map[string]autoswitch.Account{}
	for _, pa := range pas {
		view[strings.ToLower(pa.Name)] = pa
	}
	var skips []skip
	var order []store.Account // the walk after serving, in registration order (wrapping)
	cur := st.Serving
	for i := 0; i < len(st.Accounts); i++ {
		a, err := st.Next(cur)
		if err != nil {
			return store.Account{}, skips, false, err
		}
		cur = a.Name
		if strings.EqualFold(a.Name, st.Serving) {
			break
		}
		order = append(order, a)
		pa := view[strings.ToLower(a.Name)]
		why := autoswitch.Eligible(pa, params, now)
		if force && why != autoswitch.ReasonOutOfRotation {
			why = ""
		}
		if why != "" {
			sk := skip{Name: a.Name, Reason: why}
			if why == autoswitch.ReasonLimited {
				sk.Until = pa.LimitedUntil
			}
			skips = append(skips, sk)
			continue
		}
		return a, skips, false, nil
	}
	// A manual `next` is a hard request to move, so it gets the planner's
	// LIMIT-trigger fallback too (item 2, review round 2): any account with
	// capacity at all, rather than being stuck. --force already ignores
	// switch points, so it never needs this.
	if !force {
		if name, ok := fallbackNext(order, pas, view, params, burnOf(f), now); ok {
			for _, a := range order {
				if strings.EqualFold(a.Name, name) {
					return a, withoutSkip(skips, name), true, nil
				}
			}
		}
	}
	sort.SliceStable(skips, func(i, j int) bool {
		if skips[i].Until.IsZero() != skips[j].Until.IsZero() {
			return !skips[i].Until.IsZero()
		}
		return skips[i].Until.Before(skips[j].Until)
	})
	return store.Account{}, skips, false, fmt.Errorf("%w", ErrNoCandidate)
}

// withoutSkip drops name's entry from skips: the fallback's chosen account
// was recorded as a skip during the walk above (it is, after all, above its
// own switch point), but it was picked, not passed over, so it must not
// appear in the caller's skip list (item 2, review round 3).
func withoutSkip(skips []skip, name string) []skip {
	out := make([]skip, 0, len(skips))
	for _, sk := range skips {
		if !strings.EqualFold(sk.Name, name) {
			out = append(out, sk)
		}
	}
	return out
}

// burnOf is the daemon's measured burn rate from status.json (M4 spec §7),
// or 0 (uses autoswitch.DefaultBurn) when there is none.
func burnOf(f *status.File) float64 {
	if f == nil || f.Auto == nil {
		return 0
	}
	return f.Auto.BurnRate
}

// fallbackNext is item 2's fallback, shared with the planner's
// autoswitch.Fallback: order is the walk after serving in registration
// order (wrapping); roster is every registered account, used to build the
// continuity simulation in balanced mode; view looks each name up.
func fallbackNext(order []store.Account, roster []autoswitch.Account, view map[string]autoswitch.Account, p autoswitch.Params, burn float64, now time.Time) (string, bool) {
	var cands []autoswitch.Account
	for _, a := range order {
		if pa := view[strings.ToLower(a.Name)]; autoswitch.FallbackEligible(pa, now) {
			cands = append(cands, pa)
		}
	}
	name, _, _, ok := autoswitch.Fallback(cands, roster, p, burn, now)
	return name, ok
}

// knownLimit reports whether account is limited on evidence we can act on.
// Freshness (status.File.Fresh) only matters when the reset time itself is
// unknown; a KNOWN reset is honoured however stale the usage that reported
// it is (R54/F161: `chottag status` and `chottag next` must agree that an
// account limited until a future HH:MM stays limited, even if nothing has
// polled it since). Three cases, checked in this order:
//   - LimitedUntil is non-zero and not after now: the window has already
//     reset, regardless of freshness — not limited.
//   - LimitedUntil is non-zero and after now: still limited, with no
//     freshness check at all.
//   - LimitedUntil is zero (reset unknown): limited only if the usage that
//     reported it is still fresh; stale, unknown-reset usage cannot say the
//     limit still holds, so it is treated as not limited (today's rule,
//     unchanged).
func knownLimit(f *status.File, account string, now time.Time) (time.Time, bool) {
	if f == nil {
		return time.Time{}, false
	}
	for i := range f.Accounts {
		if !strings.EqualFold(f.Accounts[i].Name, account) {
			continue
		}
		a := f.Accounts[i]
		if !a.Limited {
			return time.Time{}, false
		}
		if !a.LimitedUntil.IsZero() {
			if !a.LimitedUntil.After(now) {
				return time.Time{}, false // the window has already reset
			}
			return a.LimitedUntil, true // known future reset: honoured however stale
		}
		if !f.Fresh(account, now) {
			return time.Time{}, false // unknown reset, stale usage: unknown, not limited
		}
		return time.Time{}, true
	}
	return time.Time{}, false
}
