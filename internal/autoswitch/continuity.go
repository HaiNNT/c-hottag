package autoswitch

import (
	"math"
	"time"
)

// The continuity simulation's fixed shape (M4 spec §5, S5).
const (
	// Horizon is how far ahead the simulation looks.
	Horizon = 24 * time.Hour
	// Step is the simulation's time step. Spending inside a step is exact;
	// resets are applied at the start of each step.
	Step = 15 * time.Minute
	// DefaultBurn is the burn rate, in units per active hour, used until
	// the daemon has measured one (M4 spec §2).
	DefaultBurn = 10.0
	// WeekScale is how many 5-hour windows a 7-day window holds: 1% of a
	// week is worth WeekScale × 1% of a 5-hour window (capacity model A);
	// with 1:1 the weekly window would bind within the first 5 hours of
	// every simulation.
	WeekScale = 5.0
)

// SimAccount is one account as the continuity simulation sees it.
type SimAccount struct {
	Units float64
	// Pct5h and Pct7d are the used share of each window, 0-100. Unknown
	// counts as 0.
	Pct5h, Pct7d float64
	// Reset5h and Reset7d are each window's next reset. Zero is unknown:
	// the window is taken to reset one full length from now. A reset in
	// the past means the window has already refilled.
	Reset5h, Reset7d time.Time
	// BlockedUntil is when a limited account can serve again. Zero means
	// it is not blocked.
	BlockedUntil time.Time
}

type simState struct {
	c5, c7       float64 // capacity left, in units
	r5, r7       time.Time
	full5, full7 float64 // a refilled window, in units
	blockedUntil time.Time
}

// Continuity is the continuity score (M4 spec §5): starting now, the user
// burns burn units per hour; the simulation spends accounts[0] until it
// runs out, then at every moment the usable account whose capacity expires
// soonest (earliest reset first), refilling each window at its reset. It returns how long it
// takes until no account can serve, capped at horizon. burn <= 0 uses
// DefaultBurn.
func Continuity(accounts []SimAccount, burn float64, now time.Time, horizon time.Duration) time.Duration {
	if len(accounts) == 0 || horizon <= 0 {
		return 0
	}
	if burn <= 0 || math.IsNaN(burn) {
		burn = DefaultBurn
	}
	st := make([]simState, len(accounts))
	for i, a := range accounts {
		// A NaN percentage (a corrupt read) counts as unknown, like stale
		// usage: 0%, not a poisoned capacity that compares false forever.
		pct5, pct7 := a.Pct5h, a.Pct7d
		if math.IsNaN(pct5) {
			pct5 = 0
		}
		if math.IsNaN(pct7) {
			pct7 = 0
		}
		full5, full7 := a.Units*100, a.Units*100*WeekScale
		st[i] = simState{
			c5:           clampCap(a.Units*(100-pct5), full5),
			c7:           clampCap(a.Units*(100-pct7)*WeekScale, full7),
			r5:           resetOr(a.Reset5h, now, Win5h),
			r7:           resetOr(a.Reset7d, now, Win7d),
			full5:        full5,
			full7:        full7,
			blockedUntil: a.BlockedUntil,
		}
	}
	const eps = 1e-9
	usable := func(i int, at time.Time) bool {
		s := st[i]
		return !at.Before(s.blockedUntil) && min(s.c5, s.c7) > eps
	}
	pick := func(at time.Time) int {
		best := -1
		for i := range st {
			if !usable(i, at) {
				continue
			}
			if best < 0 || expiry(st[i]).Before(expiry(st[best])) {
				best = i
			}
		}
		return best
	}
	// The first account serves until it runs out; from then on every
	// step spends from the usable account whose capacity expires soonest.
	first := usable(0, now)
	for t := time.Duration(0); t < horizon; {
		at := now.Add(t)
		for i := range st {
			s := &st[i]
			for !at.Before(s.r5) {
				s.c5, s.r5 = s.full5, s.r5.Add(Win5h.Length())
			}
			for !at.Before(s.r7) {
				s.c7, s.r7 = s.full7, s.r7.Add(Win7d.Length())
			}
		}
		end := min(t+Step, horizon)
		total := burn * (end - t).Hours()
		need := total
		for need > eps {
			cur := 0
			if first = first && usable(0, at); !first {
				if cur = pick(at); cur < 0 {
					spent := total - need
					return t + time.Duration(spent/burn*float64(time.Hour))
				}
			}
			s := &st[cur]
			take := min(s.c5, s.c7, need)
			s.c5 -= take
			s.c7 -= take
			need -= take
		}
		t = end
	}
	return horizon
}

// expiry is when an account's usable capacity is next lost to a reset.
func expiry(s simState) time.Time {
	if s.r7.Before(s.r5) {
		return s.r7
	}
	return s.r5
}

func clampCap(c, full float64) float64 {
	return max(0, min(c, full))
}

func resetOr(t, now time.Time, w Window) time.Time {
	if t.IsZero() {
		return now.Add(w.Length())
	}
	return t
}
