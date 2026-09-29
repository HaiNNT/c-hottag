package autoswitch

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)

// near reports whether a and b are within a second: Continuity spends
// fractionally inside a step, so a closed-form answer matches to float
// rounding.
func near(a, b time.Duration) bool {
	d := a - b
	return d < time.Second && d > -time.Second
}

// TestContinuityOfOneAccountIsItsTimeToLimit is §9's single-account
// property. A max5x account (5 units per 1%) at 40% has 5 × 60 = 300 units;
// at 60 units an hour that is 5h, and neither window resets before then
// (5h reset in 6h, 7d reset in 3 days).
func TestContinuityOfOneAccountIsItsTimeToLimit(t *testing.T) {
	a := SimAccount{Units: 5, Pct5h: 40, Pct7d: 10, Reset5h: t0.Add(6 * time.Hour), Reset7d: t0.Add(72 * time.Hour)}
	if got := Continuity([]SimAccount{a}, 60, t0, Horizon); !near(got, 5*time.Hour) {
		t.Fatalf("Continuity = %v, want 5h", got)
	}
}

// The 7-day window binds when it is the tighter one: its capacity is
// 5 units × (100 − 98) × WeekScale 5 = 50 units, and 50 units at 60 an
// hour is 50 minutes (the 5h window holds 500).
func TestContinuityRespectsTheTighterWindow(t *testing.T) {
	a := SimAccount{Units: 5, Pct5h: 0, Pct7d: 98, Reset5h: t0.Add(5 * time.Hour), Reset7d: t0.Add(72 * time.Hour)}
	if got := Continuity([]SimAccount{a}, 60, t0, Horizon); !near(got, 50*time.Minute) {
		t.Fatalf("Continuity = %v, want 50m", got)
	}
}

func TestContinuityNeverExceedsTheHorizon(t *testing.T) {
	a := SimAccount{Units: 20, Reset5h: t0.Add(time.Hour), Reset7d: t0.Add(time.Hour)}
	if got := Continuity([]SimAccount{a}, 0.001, t0, Horizon); got != Horizon {
		t.Fatalf("Continuity = %v, want the horizon %v", got, Horizon)
	}
	if got := Continuity([]SimAccount{a}, 0.001, t0, 90*time.Minute); got != 90*time.Minute {
		t.Fatalf("Continuity = %v, want a 90m horizon", got)
	}
}

func TestContinuityWithNoAccountsIsZero(t *testing.T) {
	if got := Continuity(nil, 10, t0, Horizon); got != 0 {
		t.Fatalf("Continuity(nil) = %v", got)
	}
}

// A blocked account adds nothing until it is unblocked: one pro account at
// 0% (100 units) blocked for 2h, at 100 units an hour, runs out 1h after
// it unblocks.
func TestContinuityWaitsOutABlockedAccount(t *testing.T) {
	blocked := SimAccount{Units: 1, Reset5h: t0.Add(10 * time.Hour), Reset7d: t0.Add(100 * time.Hour), BlockedUntil: t0.Add(2 * time.Hour)}
	if got := Continuity([]SimAccount{blocked}, 100, t0, Horizon); got != 0 {
		t.Fatalf("Continuity = %v, want 0: nothing can serve at the start", got)
	}
	// free has 100 units of 5h, gone after 1h; nothing can serve until 2h,
	// so continuity stops at 1h.
	free := SimAccount{Units: 1, Reset5h: t0.Add(10 * time.Hour), Reset7d: t0.Add(100 * time.Hour)}
	if got := Continuity([]SimAccount{free, blocked}, 100, t0, Horizon); !near(got, time.Hour) {
		t.Fatalf("Continuity = %v, want 1h", got)
	}
}

// A reset refills a window. A pro account with 5h at 90% (10 units) and
// 7d at 90% (10 × WeekScale 5 = 50 units), whose 5h window resets in 15
// minutes, burning 20 an hour:
//   - 0-15m: 5 units spent; c5 = 5, c7 = 45.
//   - at 15m the 5h window refills: c5 = 100, and c7's 45 units last
//     45 / 20 = 2h15m.
//
// Total 2h30m. Without the refill it would stop at 30m.
func TestContinuityRefillsAtAReset(t *testing.T) {
	a := SimAccount{Units: 1, Pct5h: 90, Pct7d: 90, Reset5h: t0.Add(15 * time.Minute), Reset7d: t0.Add(100 * time.Hour)}
	if got := Continuity([]SimAccount{a}, 20, t0, Horizon); !near(got, 150*time.Minute) {
		t.Fatalf("Continuity = %v, want 2h30m", got)
	}
}

// TestEarliestResetFirstBeatsTheOtherOrder is the rule under the score
// (spec §5): capacity that resets soon is lost if it is not used first.
// Two pro accounts at 50% of 5h and 0% of 7d (500 units of week each):
// C's 5h window resets in 1h, B's in 5h. At 50 units an hour:
//   - C first: C 0-1h (50); C refills at 1h; B (earlier expiry, 5h) 1h-2h
//     (50); C 2h-4h (100); then both 5h windows are empty: 4h.
//   - B first: B 0-1h (50); C refilled at 1h anyway, so C's 50 were lost;
//     C 1h-3h (100); then both are empty: 3h.
func TestEarliestResetFirstBeatsTheOtherOrder(t *testing.T) {
	b := SimAccount{Units: 1, Pct5h: 50, Reset5h: t0.Add(5 * time.Hour), Reset7d: t0.Add(100 * time.Hour)}
	c := SimAccount{Units: 1, Pct5h: 50, Reset5h: t0.Add(1 * time.Hour), Reset7d: t0.Add(100 * time.Hour)}
	cFirst := Continuity([]SimAccount{c, b}, 50, t0, Horizon)
	bFirst := Continuity([]SimAccount{b, c}, 50, t0, Horizon)
	if cFirst <= bFirst {
		t.Fatalf("C first = %v, B first = %v: spending the soon-expiring capacity first must last longer", cFirst, bFirst)
	}
}

// TestAddingCapacityNeverLowersTheScore is §9's monotonicity property,
// over a seeded family of 2-5 account setups: lowering the usage (adding
// capacity) of any account the simulation does not start on never lowers
// the score. The account it starts on is excluded on purpose: the
// simulation stays on it until it runs out (spec §5, "the candidate being
// scored first"), so more capacity there can keep it past another
// account's reset and strand that capacity. A seeded search found such
// cases for the first account only (Planner notes).
// TestContinuityNaNBurnFallsBackToDefault is review round 1's item 6: a
// NaN burn (a corrupt measurement) must not poison the whole simulation;
// it falls back to DefaultBurn exactly like burn <= 0 does.
func TestContinuityNaNBurnFallsBackToDefault(t *testing.T) {
	// A small capacity (10 units) that DefaultBurn (10/h) exhausts in 1h,
	// well short of the horizon: if a NaN burn were not caught, every
	// arithmetic comparison against it is false, the simulation's spend
	// loop would never fire, and Continuity would wrongly return the full
	// (capped) horizon instead of 1h.
	a := SimAccount{Units: 1, Pct5h: 90, Reset5h: t0.Add(20 * time.Hour), Reset7d: t0.Add(72 * time.Hour)}
	nan := Continuity([]SimAccount{a}, math.NaN(), t0, Horizon)
	def := Continuity([]SimAccount{a}, DefaultBurn, t0, Horizon)
	if nan != def || !near(nan, time.Hour) {
		t.Fatalf("Continuity(NaN burn) = %v, want DefaultBurn's %v (1h)", nan, def)
	}
}

// TestContinuityTreatsANaNPercentageAsUnknown is item 6's other half: a
// NaN utilization (a corrupt read) must count as unknown usage (0%), not
// propagate NaN into every downstream comparison.
func TestContinuityTreatsANaNPercentageAsUnknown(t *testing.T) {
	nanAcct := SimAccount{Units: 5, Pct5h: math.NaN(), Reset5h: t0.Add(6 * time.Hour), Reset7d: t0.Add(72 * time.Hour)}
	zeroAcct := SimAccount{Units: 5, Pct5h: 0, Reset5h: t0.Add(6 * time.Hour), Reset7d: t0.Add(72 * time.Hour)}
	got := Continuity([]SimAccount{nanAcct}, 60, t0, Horizon)
	want := Continuity([]SimAccount{zeroAcct}, 60, t0, Horizon)
	if got != want {
		t.Fatalf("Continuity(NaN pct) = %v, want the same as pct 0 (%v)", got, want)
	}
}

func TestAddingCapacityNeverLowersTheScore(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	tiers := []float64{1, 5, 20}
	for n := 0; n < 2000; n++ {
		accts := make([]SimAccount, 2+rng.Intn(4))
		for i := range accts {
			accts[i] = SimAccount{
				Units:   tiers[rng.Intn(3)],
				Pct5h:   float64(rng.Intn(101)),
				Pct7d:   float64(rng.Intn(101)),
				Reset5h: t0.Add(time.Duration(rng.Intn(300)) * time.Minute),
				Reset7d: t0.Add(time.Duration(rng.Intn(7*24*60)) * time.Minute),
			}
		}
		burn := float64(5 + rng.Intn(60))
		base := Continuity(accts, burn, t0, Horizon)
		for i := 1; i < len(accts); i++ {
			more := append([]SimAccount(nil), accts...)
			more[i].Pct5h = max(0, more[i].Pct5h-float64(1+rng.Intn(40)))
			more[i].Pct7d = max(0, more[i].Pct7d-float64(1+rng.Intn(40)))
			if got := Continuity(more, burn, t0, Horizon); got+time.Second < base {
				t.Fatalf("case %d: more capacity on account %d lowered the score %v -> %v\nbase %+v\nmore %+v", n, i, base, got, accts, more)
			}
		}
	}
}
