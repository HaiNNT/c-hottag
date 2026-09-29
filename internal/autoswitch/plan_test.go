package autoswitch

import (
	"math"
	"strings"
	"testing"
	"time"
)

// acct is a fresh, rotating, logged-in account with both windows known.
func acct(name string, tier Tier, pct5 float64, reset5 time.Duration, pct7 float64, reset7 time.Duration) Account {
	return Account{
		Name: name, Tier: tier, Rotates: true, Fresh: true,
		Has5h: true, Pct5h: pct5, Reset5h: t0.Add(reset5),
		Has7d: true, Pct7d: pct7, Reset7d: t0.Add(reset7),
	}
}

func input(mode Mode, serving string, accts ...Account) Input {
	return Input{Now: t0, Enabled: true, Params: Preset(mode), Serving: serving, Accounts: accts}
}

func TestPlanOffDoesNothing(t *testing.T) {
	in := input(ModeBalanced, "A", acct("A", TierMax5x, 99, 3*time.Hour, 10, 72*time.Hour), acct("B", TierMax5x, 0, time.Hour, 0, 72*time.Hour))
	in.Enabled = false
	in.Accounts[0].Limited = true
	if d := Plan(in); d.Action != ActionOff || d.Reason != "off" {
		t.Fatalf("Plan = %+v, want off", d)
	}
}

func TestPlanStaysBelowTheSwitchPoint(t *testing.T) {
	d := Plan(input(ModeBalanced, "A",
		acct("A", TierMax20x, 91, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierMax5x, 0, time.Hour, 0, 72*time.Hour)))
	if d.Action != ActionStay || d.Reason != "staying on A (5h 91% < 98%)" {
		t.Fatalf("Plan = %+v", d)
	}
}

func TestPlanThresholdSwitch(t *testing.T) {
	d := Plan(input(ModeBalanced, "A",
		acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
	if d.Action != ActionSwitch || d.Target != "B" || d.Trigger != TriggerThreshold || d.Window != Win5h || d.Point != 93 {
		t.Fatalf("Plan = %+v, want a threshold switch A -> B on 5h at 93", d)
	}
	if d.Reason != "switched A -> B (threshold 5h 94% >= 93%)" {
		t.Fatalf("Reason = %q", d.Reason)
	}
}

func TestPlanWeeklyThresholdSwitch(t *testing.T) {
	d := Plan(input(ModeBalanced, "A",
		acct("A", TierPro, 10, 3*time.Hour, 93, 72*time.Hour),
		acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
	if d.Action != ActionSwitch || d.Window != Win7d || d.Point != 93 {
		t.Fatalf("Plan = %+v, want a 7d threshold switch at pro's 93", d)
	}
}

// TestPlanHardLimitIgnoresCooldownHoldAndUserChoice pins S7: a limit leaves
// at once, whatever else applies.
func TestPlanHardLimitIgnoresCooldownHoldAndUserChoice(t *testing.T) {
	a := acct("A", TierMax5x, 100, 5*time.Minute, 40, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(5*time.Minute), Win5h
	in := input(ModeBalanced, "A", a, acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour))
	in.LastSoftSwitch = t0.Add(-time.Minute)
	in.UserChosen = true
	d := Plan(in)
	if d.Action != ActionSwitch || d.Target != "B" || d.Trigger != TriggerLimit || d.Reason != "switched A -> B (limit 5h)" {
		t.Fatalf("Plan = %+v, want an immediate limit switch", d)
	}
}

// TestPlanCacheHold is R70's hold, with its hard-limit escape.
func TestPlanCacheHold(t *testing.T) {
	a := acct("A", TierMax5x, 96, 9*time.Minute, 40, 72*time.Hour)
	b := acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionHold || d.Hold != HoldCache || d.ResetIn != 9*time.Minute || d.Reason != "holding A (5h 96%, resets in 9m)" {
		t.Fatalf("Plan = %+v, want a cache hold", d)
	}
	// A reset just past the hold time (30m) does not hold.
	a.Reset5h = t0.Add(31 * time.Minute)
	if d := Plan(input(ModeBalanced, "A", a, b)); d.Action != ActionSwitch {
		t.Fatalf("Plan = %+v, want a switch: 31m is past the 30m hold", d)
	}
	// The escape: A hits the wall during the hold, and leaves at once.
	a.Reset5h = t0.Add(9 * time.Minute)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, a.Reset5h, Win5h
	if d := Plan(input(ModeBalanced, "A", a, b)); d.Action != ActionSwitch || d.Trigger != TriggerLimit {
		t.Fatalf("Plan = %+v, want the hard limit to break the hold", d)
	}
}

// A hold needs every window over its point to reset soon: a 5h window
// about to reset does not hold an account whose week is also over.
func TestPlanCacheHoldNeedsEveryOverWindowToResetSoon(t *testing.T) {
	a := acct("A", TierMax5x, 96, 9*time.Minute, 99, 72*time.Hour)
	d := Plan(input(ModeBalanced, "A", a, acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
	if d.Action != ActionSwitch {
		t.Fatalf("Plan = %+v, want a switch: the 7d window is over and far from its reset", d)
	}
}

func TestPlanCooldownHoldsASoftSwitch(t *testing.T) {
	in := input(ModeBalanced, "A",
		acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour))
	in.LastSoftSwitch = t0.Add(-5 * time.Minute)
	d := Plan(in)
	if d.Action != ActionHold || d.Hold != HoldCooldown || d.CooldownLeft != 10*time.Minute || d.Reason != "holding A (5h 94%, cooldown 10m left)" {
		t.Fatalf("Plan = %+v, want a cooldown hold with 10m left", d)
	}
	in.LastSoftSwitch = t0.Add(-15 * time.Minute)
	if d := Plan(in); d.Action != ActionSwitch {
		t.Fatalf("Plan = %+v, want a switch once the 15m cooldown has passed", d)
	}
}

// TestPlanUserChoiceHoldsUntilAHardLimit pins S6.
func TestPlanUserChoiceHoldsUntilAHardLimit(t *testing.T) {
	in := input(ModeBalanced, "A",
		acct("A", TierMax5x, 96, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour))
	in.UserChosen = true
	if d := Plan(in); d.Action != ActionHold || d.Hold != HoldUser || d.Reason != "holding A (5h 96%, chosen by you)" {
		t.Fatalf("Plan = %+v, want a user-choice hold", d)
	}
}

// TestPlanDoesNotFlapToAStaleOverPointAccount is item 1 (review round 2):
// A is serving, fresh, at its own switch point. B is stale but reads the
// same over-point percentage, with its reset still ahead. Before the fix,
// B's staleness made it read as "below" and an eligible target, so once
// A's cooldown ended the plan flapped A -> B -> A forever. Now B is not an
// eligible target, so the outcome is no-candidate.
func TestPlanDoesNotFlapToAStaleOverPointAccount(t *testing.T) {
	a := acct("A", TierMax5x, 95, 3*time.Hour, 10, 72*time.Hour)
	b := acct("B", TierMax5x, 95, 3*time.Hour, 10, 72*time.Hour)
	b.Fresh = false
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionNoCandidate {
		t.Fatalf("Plan = %+v, want no-candidate: B's stale 95%% is still over its point (max5x 93)", d)
	}
}

// Same flap, on the 7d window: max5x's weekly point is 98.
func TestPlanDoesNotFlapToAStaleOverPointAccountWeekly(t *testing.T) {
	a := acct("A", TierMax5x, 10, 3*time.Hour, 98.5, 72*time.Hour)
	b := acct("B", TierMax5x, 10, 3*time.Hour, 98.5, 72*time.Hour)
	b.Fresh = false
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionNoCandidate {
		t.Fatalf("Plan = %+v, want no-candidate: B's stale 98.5%% 7d is still over its point (max5x 98)", d)
	}
}

func TestPlanNoCandidate(t *testing.T) {
	b := acct("B", TierMax5x, 95, 4*time.Hour, 20, 72*time.Hour) // above its own point
	c := acct("C", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)
	c.Rotates = false
	d := acct("D", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)
	d.NeedsLogin = true
	e := acct("E", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)
	e.Limited, e.LimitedUntil = true, t0.Add(time.Hour)
	got := Plan(input(ModeBalanced, "A", acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour), b, c, d, e))
	if got.Action != ActionNoCandidate || got.Reason != "no account to switch to from A (threshold 5h 94% >= 93%)" {
		t.Fatalf("Plan = %+v, want no candidate", got)
	}
}

func TestPlanStaleUsageNeverTriggers(t *testing.T) {
	a := acct("A", TierMax5x, 99, 3*time.Hour, 99, 72*time.Hour)
	a.Fresh = false
	d := Plan(input(ModeBalanced, "A", a, acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
	if d.Action != ActionStay || d.Reason != "staying on A (usage unknown)" {
		t.Fatalf("Plan = %+v, want a stay: stale usage counts as below (R54)", d)
	}
}

// An unknown candidate (no data at all) counts as below its switch point
// (R54), so it stays eligible. A stale reading whose reset is itself
// unknown behaves the same way: see
// TestEligibleTreatsAStaleReadingAsOverItsPointWhenTheResetIsStillAhead for
// the case where a known future reset makes a stale reading a lower bound
// instead (item 1, review round 2).
func TestEligibleTreatsUnknownAndStaleWithNoKnownResetAsBelow(t *testing.T) {
	p := Preset(ModeBalanced)
	staleNoReset := Account{Name: "B", Tier: TierMax5x, Rotates: true, Has5h: true, Pct5h: 99, Has7d: true, Pct7d: 99}
	if r := Eligible(staleNoReset, p, t0); r != "" {
		t.Fatalf("stale, unknown reset = %q, want eligible", r)
	}
	unknown := Account{Name: "C", Tier: TierMax5x, Rotates: true, Fresh: true}
	if r := Eligible(unknown, p, t0); r != "" {
		t.Fatalf("unknown = %q, want eligible", r)
	}
}

// TestEligibleTreatsAStaleReadingAsOverItsPointWhenTheResetIsStillAhead is
// item 1 (review round 2): usage never falls before a reset, so a stale
// reading at or above the point, whose reset is still ahead, is a lower
// bound and still counts as over. Without this a stale over-point account
// reads as an eligible target, and balanced mode ping-pongs onto it once
// the cooldown on the fresh account ends.
func TestEligibleTreatsAStaleReadingAsOverItsPointWhenTheResetIsStillAhead(t *testing.T) {
	p := Preset(ModeBalanced)
	stale := acct("B", TierMax5x, 95, time.Hour, 10, 72*time.Hour)
	stale.Fresh = false
	if r := Eligible(stale, p, t0); r != ReasonAboveSwitch {
		t.Fatalf("stale over point, reset ahead = %q, want %q", r, ReasonAboveSwitch)
	}
	// The same reading, but its reset has already passed: refilled, not over.
	past := acct("B", TierMax5x, 95, -time.Minute, 10, 72*time.Hour)
	past.Fresh = false
	if r := Eligible(past, p, t0); r != "" {
		t.Fatalf("stale over point, reset passed = %q, want eligible (refilled)", r)
	}
}

func TestEligibleReasonsInOrder(t *testing.T) {
	p := Preset(ModeBalanced)
	a := acct("A", TierMax5x, 99, time.Hour, 10, 72*time.Hour)
	a.Rotates, a.NeedsLogin, a.Limited = false, true, true
	for _, want := range []string{ReasonOutOfRotation, ReasonNeedsLogin, ReasonLimited, ReasonAboveSwitch, ""} {
		if got := Eligible(a, p, t0); got != want {
			t.Fatalf("Eligible = %q, want %q", got, want)
		}
		switch want {
		case ReasonOutOfRotation:
			a.Rotates = true
		case ReasonNeedsLogin:
			a.NeedsLogin = false
		case ReasonLimited:
			a.Limited = false
		case ReasonAboveSwitch:
			a.Pct5h = 10
		}
	}
	// cache-optimize: 100 is the wall only, so 99% is still eligible.
	a.Pct5h = 99
	if got := Eligible(a, Preset(ModeCacheOptimize), t0); got != "" {
		t.Fatalf("cache-optimize Eligible at 99%% = %q, want eligible", got)
	}
}

// TestCacheOptimizeGoesInRegistrationOrderAtTheWallOnly: never a threshold
// switch, and the target is the next account after serving, wrapping.
func TestCacheOptimizeGoesInRegistrationOrderAtTheWallOnly(t *testing.T) {
	a := acct("A", TierMax5x, 99, 3*time.Hour, 99, 72*time.Hour)
	b := acct("B", TierMax5x, 90, 30*time.Minute, 20, 72*time.Hour)
	c := acct("C", TierMax20x, 0, 4*time.Hour, 0, 72*time.Hour)
	in := input(ModeCacheOptimize, "B", a, b, c)
	in.Accounts[1].Pct5h = 99.9
	if d := Plan(in); d.Action != ActionStay {
		t.Fatalf("Plan = %+v, want a stay below the wall", d)
	}
	in.Accounts[1].Limited, in.Accounts[1].LimitWindow = true, Win5h
	d := Plan(in)
	if d.Action != ActionSwitch || d.Target != "C" || d.Continuity != 0 {
		t.Fatalf("Plan = %+v, want B -> C, the next in registration order", d)
	}
	in.Serving = "C"
	in.Accounts[1].Limited = false
	in.Accounts[2].Limited = true
	if d := Plan(in); d.Target != "A" {
		t.Fatalf("Plan = %+v, want C -> A: registration order wraps", d)
	}
}

// TestBalancedPrefersTheEarliestResetOverRegistrationOrder: A is at its
// weekly wall for days. B is next in registration order; both B and C are
// pro accounts with 50 units of 5h left, but C's 5h window resets in 40
// minutes and B's in 4h50m. At 60 units an hour (resets apply at the start
// of the 15-minute step they fall in, so C's at 45m):
//   - C first: C 0-45m (45); refilled, C 45m-2h25m (100); then B 2h25m-3h15m
//     (50): 3h15m.
//   - B first: B 0-50m (50); C, refilled at 45m, 50m-2h30m (100): 2h30m.
//
// So balanced picks C, with a 45m gain over registration order's B.
func TestBalancedPrefersTheEarliestResetOverRegistrationOrder(t *testing.T) {
	a := acct("A", TierPro, 40, 3*time.Hour, 100, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(72*time.Hour), Win7d
	in := input(ModeBalanced, "A", a,
		acct("B", TierPro, 50, 290*time.Minute, 10, 72*time.Hour),
		acct("C", TierPro, 50, 40*time.Minute, 10, 96*time.Hour))
	in.Burn = 60
	d := Plan(in)
	if d.Action != ActionSwitch || d.Target != "C" || d.Continuity != 195*time.Minute || d.Gain != 45*time.Minute {
		t.Fatalf("Plan = %+v, want C with continuity 3h15m, 45m better than B", d)
	}
}

// A continuity tie goes to the most unit-weighted remaining capacity:
// with every window far from its reset and a tiny burn, both candidates
// last the whole horizon.
func TestBalancedTieGoesToTheMostRemainingCapacity(t *testing.T) {
	in := input(ModeBalanced, "A",
		acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierPro, 10, 20*time.Hour, 10, 150*time.Hour),
		acct("C", TierMax20x, 50, 20*time.Hour, 10, 150*time.Hour))
	in.Burn = 0.01
	d := Plan(in)
	if d.Target != "C" || d.Continuity != Horizon {
		t.Fatalf("Plan = %+v, want C (20 × 50 = 1000 units left against B's 1 × 90)", d)
	}
}

func TestShortDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 9 * time.Minute: "9m", 2*time.Hour + 5*time.Minute: "2h5m", 3 * time.Hour: "3h",
	} {
		if got := shortDur(d); got != want {
			t.Errorf("shortDur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestPlanNamesMatchCaseInsensitively(t *testing.T) {
	d := Plan(input(ModeBalanced, "a",
		acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
	if d.Action != ActionSwitch || !strings.EqualFold(d.From, "A") || d.Target != "B" {
		t.Fatalf("Plan = %+v", d)
	}
}

// TestPlanTreatsAWindowWhoseResetHasPassedAsRefilled is review round 1's
// item 1: a switch point exists to leave before a wall, so once that
// window's own reset has passed (even by a poll's staleness) there is
// nothing left to leave for; the serving account counts as refilled and
// stays, riding into the reset rather than switching away from it.
func TestPlanTreatsAWindowWhoseResetHasPassedAsRefilled(t *testing.T) {
	for _, reset5 := range []time.Duration{0, -time.Minute} {
		a := acct("A", TierMax5x, 96, reset5, 40, 72*time.Hour)
		d := Plan(input(ModeBalanced, "A", a, acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
		if d.Action != ActionStay {
			t.Fatalf("reset5h=%v: Plan = %+v, want a stay: the window has already refilled", reset5, d)
		}
	}
}

// TestEligibleTreatsAWindowWhoseResetHasPassedAsRefilled is the same rule
// (item 1), seen from a candidate: a passed reset makes it eligible again,
// even though its last-known reading was above its switch point.
func TestEligibleTreatsAWindowWhoseResetHasPassedAsRefilled(t *testing.T) {
	p := Preset(ModeBalanced)
	for _, reset5 := range []time.Duration{0, -time.Minute} {
		b := acct("B", TierMax5x, 96, reset5, 40, 72*time.Hour)
		if r := Eligible(b, p, t0); r != "" {
			t.Fatalf("reset5h=%v: Eligible = %q, want eligible: the window has already refilled", reset5, r)
		}
	}
}

// TestPlanStayReasonReflectsTheRefilledWindowNotTheRawReading is
// re-review item 1(a): stayWindow's own use of effective() was untested;
// reverting it to the raw percentage survives the rest of the suite
// because Action alone (already covered) doesn't change. With 5h's raw
// reading at 96% but its reset already passed, the reason must not report
// 96%.
func TestPlanStayReasonReflectsTheRefilledWindowNotTheRawReading(t *testing.T) {
	a := acct("A", TierMax5x, 96, 0, 10, 72*time.Hour) // 5h reset exactly now: refilled
	d := Plan(input(ModeBalanced, "A", a, acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)))
	if d.Action != ActionStay {
		t.Fatalf("Plan = %+v, want a stay", d)
	}
	if strings.Contains(d.Reason, "96%") {
		t.Fatalf("Reason = %q, must not report the raw (refilled) 96%%", d.Reason)
	}
}

// TestPlanCacheHoldIgnoresAWindowWhoseResetHasPassed is item 1(b):
// cacheHold's own use of effective() was likewise untested. 5h is at 96%
// and holds (its reset is 10m away); 7d's raw reading is 99% but its reset
// has already passed, so it must not be counted as still over its point
// (which would otherwise break the hold, since its reset is far away).
func TestPlanCacheHoldIgnoresAWindowWhoseResetHasPassed(t *testing.T) {
	a := acct("A", TierMax5x, 96, 10*time.Minute, 99, 0)
	b := acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)
	if d := Plan(input(ModeBalanced, "A", a, b)); d.Action != ActionHold || d.Hold != HoldCache {
		t.Fatalf("Plan = %+v, want a cache hold: 7d has already refilled, only 5h (10m) counts", d)
	}
}

// TestRemainingUsesTheEffectivePercentageNotTheRawOne is item 2: a
// candidate whose window has already reset must be rated as refilled, not
// under-rated by its last, stale reading.
func TestRemainingUsesTheEffectivePercentageNotTheRawOne(t *testing.T) {
	refilled := Account{Tier: TierPro, Has5h: true, Pct5h: 99, Reset5h: t0, Has7d: true}
	stale := Account{Tier: TierPro, Has5h: true, Pct5h: 50, Has7d: true}
	if got := remaining(refilled, t0); got != 100 {
		t.Fatalf("remaining(refilled) = %v, want 100: its window has already reset", got)
	}
	if got := remaining(stale, t0); got != 50 {
		t.Fatalf("remaining(stale) = %v, want 50", got)
	}
	if remaining(refilled, t0) <= remaining(stale, t0) {
		t.Fatal("a refilled candidate must not be rated below one genuinely at 50%")
	}
}

// TestGainNeverGoesNegative is item 3: pickBest may choose a candidate
// slightly below scores[0] (still within its tie band) for its extra
// remaining capacity, but the reported Gain must floor at 0, not read as
// a loss.
func TestGainNeverGoesNegative(t *testing.T) {
	if got := gain([]time.Duration{100 * time.Second, 90 * time.Second}, 1); got != 0 {
		t.Fatalf("gain = %v, want 0 (clamped)", got)
	}
	if got := gain([]time.Duration{50 * time.Second, 120 * time.Second}, 1); got != 70*time.Second {
		t.Fatalf("gain = %v, want 70s", got)
	}
}

// TestPickBestTieBandIsTransitive is item 2: a chain of near-ties, each
// with more remaining capacity than the last, must not walk the choice
// away from the top score's own band. Only W (150s) and X (100s) are
// within a minute of the maximum; among them X has more capacity left.
func TestPickBestTieBandIsTransitive(t *testing.T) {
	cands := []Account{
		{Name: "W", Tier: TierPro, Pct5h: 90}, // remaining 1*(100-90) = 10
		{Name: "X", Tier: TierPro, Pct5h: 50}, // remaining 50
		{Name: "Y", Tier: TierPro, Pct5h: 10}, // remaining 90
		{Name: "Z", Tier: TierPro, Pct5h: 0},  // remaining 100
	}
	scores := []time.Duration{150 * time.Second, 100 * time.Second, 50 * time.Second, 0}
	if got := pickBest(cands, scores, t0); cands[got].Name != "X" {
		t.Fatalf("pickBest = %s, want X: Y and Z score too far below the 150s maximum to be in its tie band", cands[got].Name)
	}
}

// TestPlanCooldownInTheFutureHasAlreadyElapsed is item 4: a LastSoftSwitch
// after Now (a clock hiccup, or state written slightly ahead) must not
// extend the cooldown past its configured length.
func TestPlanCooldownInTheFutureHasAlreadyElapsed(t *testing.T) {
	in := input(ModeBalanced, "A",
		acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour),
		acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour))
	in.LastSoftSwitch = t0.Add(5 * time.Minute)
	if d := Plan(in); d.Action != ActionSwitch {
		t.Fatalf("Plan = %+v, want a switch: a cooldown timestamp in the future has already elapsed", d)
	}
}

// TestPlanCacheHoldAtExactlyTheHoldTimeStillHolds pins the cacheHold edge
// (item 7): a reset exactly at the mode's hold time still holds, since the
// comparison is a strict "later than the hold", not "at or later".
func TestPlanCacheHoldAtExactlyTheHoldTimeStillHolds(t *testing.T) {
	a := acct("A", TierMax5x, 96, 30*time.Minute, 40, 72*time.Hour)
	b := acct("B", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour)
	if d := Plan(input(ModeBalanced, "A", a, b)); d.Action != ActionHold || d.Hold != HoldCache {
		t.Fatalf("Plan = %+v, want a cache hold: 30m is exactly the hold, not past it", d)
	}
}

// TestEligibleExcludesAFullWindowEvenWhenThePointIsTheWallItself is item 5:
// cache-optimize's switch points are all 100 (the wall only), but a
// candidate already at or over 100% can never actually serve, whatever its
// point is set to.
func TestEligibleExcludesAFullWindowEvenWhenThePointIsTheWallItself(t *testing.T) {
	p := Preset(ModeCacheOptimize)
	full := acct("B", TierMax5x, 100, 4*time.Hour, 20, 72*time.Hour)
	if r := Eligible(full, p, t0); r != ReasonAboveSwitch {
		t.Fatalf("Eligible = %q, want %q: a 100%% window can never serve", r, ReasonAboveSwitch)
	}
}

// TestPlanUsesDefaultBurnWhenNoneMeasured exercises the continuity
// "burn <= 0 uses DefaultBurn" path through Plan itself, not just
// Continuity directly.
func TestPlanUsesDefaultBurnWhenNoneMeasured(t *testing.T) {
	mk := func(burn float64) Input {
		in := input(ModeBalanced, "A",
			acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour),
			acct("B", TierPro, 10, 20*time.Hour, 10, 150*time.Hour),
			acct("C", TierMax20x, 50, 20*time.Hour, 10, 150*time.Hour))
		in.Burn = burn
		return in
	}
	zero, def := Plan(mk(0)), Plan(mk(DefaultBurn))
	if zero.Target != def.Target || zero.Continuity != def.Continuity {
		t.Fatalf("Plan with Burn=0 = %+v, want the same as DefaultBurn: %+v", zero, def)
	}
}

// TestPlanLimitFallsBackWhenNoCandidateIsBelowItsPoint is item 2 (review
// round 2): A is limited, and B and C both have capacity left but are
// above their own (softer) switch points, so the normal candidate search
// finds nothing. Rather than leaving the user stuck on the limited
// account, balanced falls back to whichever still-usable account has the
// best continuity — here a tie (both last the whole horizon at a tiny
// burn), broken by remaining capacity: C (max20x) has 20 units left
// against B's (pro) 10.
func TestPlanLimitFallsBackWhenNoCandidateIsBelowItsPoint(t *testing.T) {
	a := acct("A", TierMax5x, 100, time.Hour, 40, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(time.Hour), Win5h
	b := acct("B", TierPro, 90, 20*time.Hour, 10, 150*time.Hour)
	c := acct("C", TierMax20x, 99, 20*time.Hour, 10, 150*time.Hour)
	in := input(ModeBalanced, "A", a, b, c)
	in.Burn = 0.01
	d := Plan(in)
	if d.Action != ActionSwitch || d.Target != "C" || d.Trigger != TriggerLimit {
		t.Fatalf("Plan = %+v, want a limit fallback switch to C", d)
	}
	if d.Continuity != Horizon {
		t.Fatalf("Continuity = %v, want the horizon (both candidates last it at this burn)", d.Continuity)
	}
	if !strings.Contains(d.Reason, "C above its switch point") {
		t.Fatalf("Reason = %q, want it to name the fallback", d.Reason)
	}
	if !d.Fallback {
		t.Fatal("Fallback = false, want true: the target was picked by the limit fallback")
	}
}

// TestFallbackCacheOptimizePicksRegistrationOrderNotTheLastCandidate is item
// 5 (review round 3): cache-optimize's fallback (OrderRegistration) keeps
// cands[0] — the doc comment's "registration order after the account
// leaving" — not any other position in cands. Called directly rather than
// through Plan(): cache-optimize's Preset maxes out every switch point
// (Preset's "at the wall only"), so pickTarget already succeeds on B before
// leave ever reaches Fallback, and the mutation this test exists for —
// cands[0] swapped for cands[len(cands)-1] — would never be exercised via
// Plan() at all.
func TestFallbackCacheOptimizePicksRegistrationOrderNotTheLastCandidate(t *testing.T) {
	b := acct("B", TierPro, 90, 20*time.Hour, 10, 150*time.Hour)
	c := acct("C", TierMax20x, 99, 20*time.Hour, 10, 150*time.Hour)
	p := Preset(ModeCacheOptimize)
	name, _, _, ok := Fallback([]Account{b, c}, []Account{b, c}, p, 0.01, t0)
	if !ok || name != "B" {
		t.Fatalf("Fallback = %q, %v; want B (cands[0], registration order), not C (cands[len(cands)-1])", name, ok)
	}
}

// TestPlanThresholdNeverFallsBack is item 2: a THRESHOLD trigger (not a
// hard limit) never falls back, even when every other account is above its
// own switch point — there was no wall forcing the move, so waiting out
// the hold/cooldown on the serving account is still better than a switch
// to another over-point account.
func TestPlanThresholdNeverFallsBack(t *testing.T) {
	a := acct("A", TierMax5x, 94, 3*time.Hour, 40, 72*time.Hour) // threshold trigger, not limited
	b := acct("B", TierMax5x, 95, 4*time.Hour, 20, 72*time.Hour) // above its own point, but < 100%
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionNoCandidate {
		t.Fatalf("Plan = %+v, want no-candidate: a threshold trigger must not fall back", d)
	}
}

// TestFallbackNeverPicksAFullOrOutOfRotationAccount is item 2: the
// fallback's own eligibility (FallbackEligible) still excludes an account
// at its wall and one taken out of rotation, even though it ignores switch
// points.
func TestFallbackNeverPicksAFullOrOutOfRotationAccount(t *testing.T) {
	a := acct("A", TierMax5x, 100, time.Hour, 40, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(time.Hour), Win5h
	full := acct("B", TierMax5x, 100, 4*time.Hour, 20, 72*time.Hour)
	outOfRotation := acct("C", TierMax5x, 95, 4*time.Hour, 20, 72*time.Hour)
	outOfRotation.Rotates = false
	d := Plan(input(ModeBalanced, "A", a, full, outOfRotation))
	if d.Action != ActionNoCandidate {
		t.Fatalf("Plan = %+v, want no-candidate: B is at its wall, C is out of rotation", d)
	}
}

// TestPlanFallbackTargetThresholdNoCandidateStaysSilently is item 1
// (review round 3): the reviewer's own repro. B holds the role only
// because a LIMIT trigger's fallback put it there (Input.FallbackTarget),
// and B is itself over its own switch point. A THRESHOLD evaluation that
// finds no candidate must not report ActionNoCandidate — every other
// account is in exactly the above-point state B is already tolerating, so
// nothing has gotten worse — it must read as a silent stay instead.
func TestPlanFallbackTargetThresholdNoCandidateStaysSilently(t *testing.T) {
	a := acct("A", TierMax5x, 100, time.Hour, 40, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(time.Hour), Win5h
	b := acct("B", TierMax5x, 95, 4*time.Hour, 20, 72*time.Hour) // over its own point (93)
	in := input(ModeBalanced, "B", a, b)
	in.FallbackTarget = true
	d := Plan(in)
	if d.Action != ActionStay || d.Reason != "staying on B (fallback; no account below its switch point)" {
		t.Fatalf("Plan = %+v, want a silent fallback stay", d)
	}
}

// TestPlanFallbackTargetSwitchesNormallyWhenACandidateAppears: a candidate
// below its own point still wins the normal switch, even while
// FallbackTarget is set — the silence only ever replaces a genuine
// no-candidate, never a real target.
func TestPlanFallbackTargetSwitchesNormallyWhenACandidateAppears(t *testing.T) {
	a := acct("A", TierMax5x, 100, time.Hour, 40, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(time.Hour), Win5h
	b := acct("B", TierMax5x, 95, 4*time.Hour, 20, 72*time.Hour) // over its own point
	c := acct("C", TierMax5x, 10, 4*time.Hour, 20, 72*time.Hour) // below its own point
	in := input(ModeBalanced, "B", a, b, c)
	in.FallbackTarget = true
	d := Plan(in)
	if d.Action != ActionSwitch || d.Target != "C" {
		t.Fatalf("Plan = %+v, want a normal switch to C", d)
	}
}

// TestPlanFallbackTargetHardLimitStillReportsNoCandidate: a hard limit on
// the fallback target goes through the ordinary LIMIT path, unaffected by
// FallbackTarget — the silence is a THRESHOLD-only carve-out (item 1,
// review round 3).
func TestPlanFallbackTargetHardLimitStillReportsNoCandidate(t *testing.T) {
	a := acct("A", TierMax5x, 100, time.Hour, 40, 72*time.Hour)
	a.Limited, a.LimitedUntil, a.LimitWindow = true, t0.Add(time.Hour), Win5h
	b := acct("B", TierMax5x, 100, time.Hour, 40, 72*time.Hour)
	b.Limited, b.LimitedUntil, b.LimitWindow = true, t0.Add(time.Hour), Win5h
	in := input(ModeBalanced, "B", a, b)
	in.FallbackTarget = true
	d := Plan(in)
	if d.Action != ActionNoCandidate || d.Trigger != TriggerLimit {
		t.Fatalf("Plan = %+v, want a normal (unsilenced) limit no-candidate", d)
	}
}

// TestSimOrderExcludesNonRotatingAccounts: an account taken out of
// rotation never enters the continuity simulation.
func TestSimOrderExcludesNonRotatingAccounts(t *testing.T) {
	in := input(ModeBalanced, "A",
		acct("A", TierMax5x, 10, time.Hour, 10, 72*time.Hour),
		acct("B", TierMax5x, 10, time.Hour, 10, 72*time.Hour))
	in.Accounts[1].Rotates = false
	if sims := in.simOrder(in.Accounts[0]); len(sims) != 1 {
		t.Fatalf("simOrder = %v, want only the first account: B does not rotate", sims)
	}
}

// TestSimLeavesAnAccountUnconstrainedWhenThereIsNo5hData: Has5h false
// means the 5h reading is not to be trusted, so the sim must not carry
// over its raw (stale or zero-value) percentage.
func TestSimLeavesAnAccountUnconstrainedWhenThereIsNo5hData(t *testing.T) {
	in := input(ModeBalanced, "A", acct("A", TierMax5x, 10, time.Hour, 10, 72*time.Hour))
	a := in.Accounts[0]
	a.Has5h, a.Pct5h = false, 99
	if sa := in.sim(a); sa.Pct5h != 0 {
		t.Fatalf("sim.Pct5h = %v, want 0: no 5h data means unconstrained", sa.Pct5h)
	}
}

// TestSimTreatsANaNPercentageAsUnknown is item 6's sim half: a corrupt
// (NaN) reading must not poison the simulation's capacity arithmetic.
func TestSimTreatsANaNPercentageAsUnknown(t *testing.T) {
	in := input(ModeBalanced, "A", acct("A", TierMax5x, 10, time.Hour, 10, 72*time.Hour))
	a := in.Accounts[0]
	a.Pct5h = math.NaN()
	if sa := in.sim(a); sa.Pct5h != 0 || math.IsNaN(sa.Pct5h) {
		t.Fatalf("sim.Pct5h = %v, want 0: a NaN reading counts as unknown", sa.Pct5h)
	}
}
