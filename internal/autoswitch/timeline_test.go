package autoswitch

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// world is a scripted usage timeline (spec §9): accounts with staggered
// resets, a serving account that burns at a fixed rate while the user
// works, and the planner deciding after every step, exactly as the daemon's
// tick would.
type world struct {
	now        time.Time
	accts      []Account
	serving    string
	lastSoft   time.Time
	userChosen bool
	enabled    bool
	params     Params
	burn       float64 // units per hour while working
	log        []string
}

func newWorld(mode Mode, serving string, burn float64, accts ...Account) *world {
	return &world{now: t0, accts: accts, serving: serving, enabled: true, params: Preset(mode), burn: burn}
}

func (w *world) idx(name string) int {
	for i := range w.accts {
		if strings.EqualFold(w.accts[i].Name, name) {
			return i
		}
	}
	return -1
}

// step advances the clock by dt, refills windows that reset, burns on the
// serving account (limiting it at 100%), then asks the planner and applies
// its decision.
func (w *world) step(dt time.Duration, working bool) Decision {
	w.now = w.now.Add(dt)
	for i := range w.accts {
		a := &w.accts[i]
		for !w.now.Before(a.Reset5h) {
			a.Pct5h, a.Reset5h = 0, a.Reset5h.Add(Win5h.Length())
			if a.LimitWindow == Win5h {
				a.Limited, a.LimitWindow, a.LimitedUntil = false, "", time.Time{}
			}
		}
		for !w.now.Before(a.Reset7d) {
			a.Pct7d, a.Reset7d = 0, a.Reset7d.Add(Win7d.Length())
			if a.LimitWindow == Win7d {
				a.Limited, a.LimitWindow, a.LimitedUntil = false, "", time.Time{}
			}
		}
	}
	if i := w.idx(w.serving); working && i >= 0 && !w.accts[i].Limited {
		a := &w.accts[i]
		used := w.burn * dt.Hours()
		a.Pct5h = min(100, a.Pct5h+used/a.units())
		a.Pct7d = min(100, a.Pct7d+used/(a.units()*WeekScale))
		switch {
		case a.Pct5h >= 100:
			a.Limited, a.LimitWindow, a.LimitedUntil = true, Win5h, a.Reset5h
		case a.Pct7d >= 100:
			a.Limited, a.LimitWindow, a.LimitedUntil = true, Win7d, a.Reset7d
		}
	}
	d := Plan(Input{
		Now: w.now, Enabled: w.enabled, Params: w.params, Serving: w.serving,
		Accounts: w.accts, LastSoftSwitch: w.lastSoft, UserChosen: w.userChosen, Burn: w.burn,
	})
	if d.Action == ActionSwitch {
		w.log = append(w.log, fmt.Sprintf("%s %s->%s %s", w.now.Sub(t0), d.From, d.Target, d.Trigger))
		w.serving, w.userChosen = d.Target, false
		if d.Trigger == TriggerThreshold {
			w.lastSoft = w.now
		}
	}
	return d
}

// run steps every 5 minutes for total, the user working throughout.
func (w *world) run(total time.Duration) {
	for t := time.Duration(0); t < total; t += 5 * time.Minute {
		w.step(5*time.Minute, true)
	}
}

// fourAccounts is the shared timeline (spec §9: 3-4 accounts, mixed tiers,
// staggered resets). The user burns 300 units an hour for 12 hours: 60
// points an hour on a max5x, 15 on the max20x, 300 on the pro.
func fourAccounts() []Account {
	return []Account{
		acct("A", TierMax5x, 70, 3*time.Hour, 30, 50*time.Hour),
		acct("B", TierMax20x, 40, 270*time.Minute, 60, 30*time.Hour),
		acct("C", TierPro, 10, 1*time.Hour, 20, 80*time.Hour),
		acct("D", TierMax5x, 0, 2*time.Hour, 50, 100*time.Hour),
	}
}

// TestTimelineBalancedSwitchesEarlyAndInPlannedOrder pins balanced's
// decisions on the shared timeline. The first two, checked by hand:
//   - 25m A->B threshold: A (max5x) climbs 70% + 60/h and reads 95% at the
//     25m step, over its 93% point; its 5h reset is 2h35m away, past the
//     30m hold. B's 20 × 60 = 1200 units of 5h give the best continuity,
//     ahead of C and D.
//   - 4h25m B->A limit: B climbs 40% + 15/h from 25m and reads 98.75% at
//     4h20m, over its 98% point, but its reset is 10m away, so it holds
//     (R70); at 4h25m it is at 100% and leaves at once (S7). A's 5h window
//     reset at 3h, so A is back at 0%.
//
// The rest are pinned as computed: most of balanced's switches come before
// the wall, and two of the six are limits it chose to risk by holding.
func TestTimelineBalancedSwitchesEarlyAndInPlannedOrder(t *testing.T) {
	w := newWorld(ModeBalanced, "A", 300, fourAccounts()...)
	w.run(12 * time.Hour)
	got := strings.Join(w.log, "; ")
	want := "25m0s A->B threshold; 4h25m0s B->A limit; 6h0m0s A->D threshold; " +
		"8h30m0s D->A threshold; 10h5m0s A->C threshold; 10h25m0s C->B limit"
	if got != want {
		t.Fatalf("balanced switches:\n got %s\nwant %s", got, want)
	}
	if threshold := strings.Count(got, "threshold"); threshold*2 <= len(w.log) {
		t.Fatalf("%d of %d switches were before the wall; balanced should make most of them early", threshold, len(w.log))
	}
}

// TestTimelineCacheOptimizeMakesTheFewestSwitchesOnlyAtTheWall runs the
// same timeline in cache-optimize: A runs to its wall (70% + 60/h is 100%
// at 30m) and moves to B, the next in registration order; B's 1200 units
// of 5h last exactly to its 4h30m reset and its week holds out, so there is
// no second switch. Never a threshold switch, and fewer than balanced.
func TestTimelineCacheOptimizeMakesTheFewestSwitchesOnlyAtTheWall(t *testing.T) {
	co := newWorld(ModeCacheOptimize, "A", 300, fourAccounts()...)
	co.run(12 * time.Hour)
	bal := newWorld(ModeBalanced, "A", 300, fourAccounts()...)
	bal.run(12 * time.Hour)
	if got := strings.Join(co.log, "; "); got != "30m0s A->B limit" {
		t.Fatalf("cache-optimize switches = %s, want only 30m0s A->B limit", got)
	}
	if len(co.log) >= len(bal.log) {
		t.Fatalf("cache-optimize made %d switches, balanced %d: cache-optimize must make the fewest", len(co.log), len(bal.log))
	}
}

// TestTimelineCacheHoldThenHardLimitEscape: A is over its switch point
// with its 5h reset 20 minutes away, so balanced holds; burning hard, A
// hits the wall before the reset and leaves at once (one refused request
// at most).
func TestTimelineCacheHoldThenHardLimitEscape(t *testing.T) {
	w := newWorld(ModeBalanced, "A", 120, // 120 units/h on max5x = 24 points an hour
		acct("A", TierMax5x, 94, 20*time.Minute, 20, 50*time.Hour),
		acct("B", TierMax5x, 0, 3*time.Hour, 0, 60*time.Hour))
	if d := w.step(time.Minute, true); d.Action != ActionHold || d.Hold != HoldCache {
		t.Fatalf("first decision = %+v, want a cache hold", d)
	}
	var d Decision
	for i := 0; i < 20 && w.serving == "A"; i++ {
		d = w.step(time.Minute, true)
	}
	if w.serving != "B" || d.Trigger != TriggerLimit {
		t.Fatalf("serving %s after %v (last %+v), want B by a limit before A's reset", w.serving, w.now.Sub(t0), d)
	}
	if w.now.Sub(t0) >= 20*time.Minute {
		t.Fatalf("A left at %v, not before its reset", w.now.Sub(t0))
	}
}

// TestTimelineCooldownSpacesSoftSwitches: two accounts both over their
// points; after one soft switch, the cooldown keeps the planner put for
// 15 minutes rather than bouncing back.
func TestTimelineCooldownSpacesSoftSwitches(t *testing.T) {
	w := newWorld(ModeBalanced, "A", 5,
		acct("A", TierMax5x, 94, 4*time.Hour, 20, 50*time.Hour),
		acct("B", TierMax5x, 92, 4*time.Hour, 20, 60*time.Hour))
	w.step(time.Minute, true) // A -> B (threshold)
	if w.serving != "B" {
		t.Fatalf("serving %s, want B", w.serving)
	}
	w.accts[0].Pct5h = 50 // A becomes a candidate again (say a poll corrected it)
	w.accts[1].Pct5h = 95 // B crosses its point
	// The soft switch was at 1m; minutes 2-15 are inside the 15m cooldown
	// (at 15m, 14m have passed), and at 16m it has run out.
	for i := 0; i < 14; i++ {
		if d := w.step(time.Minute, true); d.Action != ActionHold || d.Hold != HoldCooldown {
			t.Fatalf("minute %d: %+v, want a cooldown hold", i+2, d)
		}
	}
	w.step(time.Minute, true)
	if w.serving != "A" {
		t.Fatalf("serving %s at %v, want A once the cooldown has passed", w.serving, w.now.Sub(t0))
	}
}

// TestTimelineManualTagHoldsUntilTheWall: the user tags A while it is at
// 96%; the planner leaves it there until A is limited.
func TestTimelineManualTagHoldsUntilTheWall(t *testing.T) {
	w := newWorld(ModeBalanced, "A", 60,
		acct("A", TierMax5x, 96, 4*time.Hour, 20, 50*time.Hour),
		acct("B", TierMax5x, 0, 3*time.Hour, 0, 60*time.Hour))
	w.userChosen = true
	for i := 0; i < 60 && w.serving == "A"; i++ {
		d := w.step(time.Minute, true)
		if w.serving == "A" && d.Hold != HoldUser {
			t.Fatalf("minute %d: %+v, want a user-choice hold", i+1, d)
		}
	}
	if len(w.log) != 1 || !strings.HasSuffix(w.log[0], "A->B limit") {
		t.Fatalf("switches = %v, want exactly one, at A's wall", w.log)
	}
}

// TestTimelineAllLimitedThenAReset: every account limited, so no candidate
// and serving stays; at the first reset the planner moves there.
func TestTimelineAllLimitedThenAReset(t *testing.T) {
	a := acct("A", TierMax5x, 100, 3*time.Hour, 50, 50*time.Hour)
	a.Limited, a.LimitWindow, a.LimitedUntil = true, Win5h, a.Reset5h
	b := acct("B", TierMax5x, 100, 40*time.Minute, 50, 60*time.Hour)
	b.Limited, b.LimitWindow, b.LimitedUntil = true, Win5h, b.Reset5h
	w := newWorld(ModeBalanced, "A", 60, a, b)
	for i := 0; i < 7; i++ { // 35 minutes
		if d := w.step(5*time.Minute, true); d.Action != ActionNoCandidate {
			t.Fatalf("step %d: %+v, want no candidate", i+1, d)
		}
	}
	w.step(5*time.Minute, true) // 40m: B resets
	if w.serving != "B" || strings.Join(w.log, "") != "40m0s A->B limit" {
		t.Fatalf("switches = %v, want A->B at 40m", w.log)
	}
}

// TestTimelineOffNeverMoves: auto off, A runs into its wall and stays.
func TestTimelineOffNeverMoves(t *testing.T) {
	w := newWorld(ModeBalanced, "A", 300, fourAccounts()...)
	w.enabled = false
	w.run(6 * time.Hour)
	if len(w.log) != 0 || w.serving != "A" {
		t.Fatalf("switches = %v with auto off", w.log)
	}
}
