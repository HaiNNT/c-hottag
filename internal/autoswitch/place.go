package autoswitch

import "time"

// Candidate reports whether a can take a new or moved session now: it
// rotates, needs no login, is not known limited, and is not at or over a
// switch point by the same rule a switch target uses (targetOverPoint).
func Candidate(a Account, p Params, now time.Time) bool {
	return Eligible(a, p, now) == ""
}

// Headroom is the distance to the switch point in the tighter of the 5h and
// 7d windows (0 to 1, as a fraction of the window), times the account's
// units. A window with no reported usage, or whose reset has already passed,
// counts as fresh capacity: point/100.
func Headroom(a Account, p Params, now time.Time) float64 {
	h := 1.0
	for _, w := range []Window{Win5h, Win7d} {
		point := float64(p.SwitchPoint(w, a.Tier))
		var wh float64
		pct, ok := a.pct(w)
		if r := a.reset(w); !ok || (!r.IsZero() && !r.After(now)) {
			wh = point / 100
		} else {
			wh = min(max((point-pct)/100, 0), 1)
		}
		h = min(h, wh)
	}
	return h * a.units()
}

// Place picks the account for a session: among the accounts that are
// Candidates and not in exclude, the highest Headroom/(1+load[name]); a tie
// goes to the earlier account in accounts. A pin that names a candidate not
// in exclude wins outright. ok is false when there is no candidate. A
// negative load counts as 0.
func Place(accounts []Account, load map[string]int, pin string, exclude map[string]bool, p Params, now time.Time) (string, bool) {
	best, bestScore, found := "", 0.0, false
	for _, a := range accounts {
		if exclude[a.Name] || !Candidate(a, p, now) {
			continue
		}
		if pin != "" && a.Name == pin {
			return a.Name, true
		}
		score := Headroom(a, p, now) / float64(1+max(load[a.Name], 0))
		if !found || score > bestScore {
			best, bestScore, found = a.Name, score, true
		}
	}
	return best, found
}
