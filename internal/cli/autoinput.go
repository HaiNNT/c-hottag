package cli

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// tierOf maps an account's stored plan to the planner's tier. An unknown
// plan ("") and a Max plan of unknown size ("max", F200) count as max5x,
// the safer mistake (S2). The stored value is lower-cased before parsing
// (fix round 1 item 5), so a hand-edited "Max20x" still matches.
func tierOf(a store.Account) autoswitch.Tier {
	if t, ok := autoswitch.ParseTier(strings.ToLower(a.Plan)); ok {
		return t
	}
	return autoswitch.TierMax5x
}

// unitsOf is the account's capacity units: its --units override, else its
// tier's (M4 spec §2).
func unitsOf(a store.Account) float64 {
	if a.Units > 0 {
		return float64(a.Units)
	}
	return tierOf(a).Units()
}

// planLabel is how `chottag status` and `chottag auto` show an account's
// plan: the tier, "max?" for a Max plan of unknown size, "-" when unknown.
// The two special cases are matched case-insensitively (fix round 1 item
// 5); any other value (a real tier name, or an unrecognised one) is shown
// exactly as stored.
func planLabel(a store.Account) string {
	switch strings.ToLower(a.Plan) {
	case "":
		return "-"
	case "max":
		return "max?"
	}
	return a.Plan
}

// planFromSubscription is login/adopt's pre-fill (M4 spec §2, F200):
// `claude auth status --json`'s subscriptionType, where it names a tier
// unambiguously. "max" is kept as "max" (size unknown; doctor's
// plan-unknown row asks the user to set it). Anything else is unknown.
func planFromSubscription(sub string) string {
	switch strings.ToLower(strings.TrimSpace(sub)) {
	case "pro":
		return "pro"
	case "team":
		return "team"
	case "max":
		return "max"
	}
	return ""
}

// prefillPlan sets a's plan from sub only while a has none: a plan the
// user set with `chottag plan` always wins.
func prefillPlan(a *store.Account, sub string) {
	if a.Plan == "" {
		a.Plan = planFromSubscription(sub)
	}
}

// autoParams resolves state.json's auto block into planner parameters: the
// mode's preset (an unknown mode is balanced, S1), then every stored
// override, in a fixed order. An override that fails validation (a hand
// edit) is skipped, so that key keeps its preset value: a bad value must
// not stop the daemon from switching, or `next` from working.
func autoParams(st store.State) autoswitch.Params {
	a := st.AutoSettings()
	// strings.ToLower, same as tierOf (fix round 1 item 5): a hand-edited
	// "Cache-Optimize" must still resolve to ModeCacheOptimize.
	mode, ok := autoswitch.ParseMode(strings.ToLower(a.Mode))
	if !ok {
		mode = autoswitch.ModeBalanced
	}
	p := autoswitch.Preset(mode)
	keys := make([]string, 0, len(a.SwitchPoints))
	for k := range a.SwitchPoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if q, err := p.With(k, strconv.Itoa(a.SwitchPoints[k])); err == nil {
			p = q
		}
	}
	for _, kv := range [][2]string{{"hold5h", a.Hold5h}, {"hold7d", a.Hold7d}, {"cooldown", a.Cooldown}} {
		if kv[1] == "" {
			continue
		}
		if q, err := p.With(kv[0], kv[1]); err == nil {
			p = q
		}
	}
	return p
}

// planAccounts is the planner's view of every registered account, in
// registration order: its tier and units from state.json; its rotation;
// its limit by knownLimit's rule (R54); its freshness, usage, resets and
// token state from the status cache. f may be nil (no cache): every
// account is then unknown, which never skips one.
func planAccounts(st *store.State, f *status.File, now time.Time) []autoswitch.Account {
	out := make([]autoswitch.Account, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		pa := autoswitch.Account{Name: a.Name, Tier: tierOf(a), Units: unitsOf(a), Rotates: a.Rotates()}
		pa.LimitedUntil, pa.Limited = knownLimit(f, a.Name, now)
		if f != nil {
			pa.Fresh = f.Fresh(a.Name, now)
			pa.Recent = f.Recent(a.Name, now)
			for _, row := range f.Accounts {
				if !strings.EqualFold(row.Name, a.Name) {
					continue
				}
				// A stale needs-login must not outlive a re-login (fix
				// round 1 item 1, superseding plan ruling 3): only the
				// daemon's passthrough hook writes Token, so nothing else
				// clears it. row.TokenAt is when that state last actually
				// changed (status.SetToken); a.LoggedInAt is when
				// `chottag login`/`adopt` last confirmed this account
				// logged in. LoggedInAt at or before TokenAt means the
				// needs-login is newer than the last login: still true.
				pa.NeedsLogin = row.Token == creds.StateNeedsLogin && !a.LoggedInAt.After(row.TokenAt)
				pa.LimitWindow = planWindow(row.Window)
				if u := row.Usage; u != nil {
					if u.FiveHourPct != nil {
						pa.Has5h, pa.Pct5h = true, *u.FiveHourPct
					}
					if u.SevenDayPct != nil {
						pa.Has7d, pa.Pct7d = true, *u.SevenDayPct
					}
					pa.Reset5h, pa.Reset7d = u.FiveHourResetsAt, u.SevenDayResetsAt
				}
				break
			}
		}
		out = append(out, pa)
	}
	return out
}

// planWindow maps status.json's limit window name to the planner's.
func planWindow(w string) autoswitch.Window {
	switch w {
	case "five_hour":
		return autoswitch.Win5h
	case "seven_day":
		return autoswitch.Win7d
	}
	return ""
}
