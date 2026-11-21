package autoswitch

import (
	"testing"
	"time"
)

func refused(a Account) Account {
	a.LoginRefused = true
	return a
}

func TestEligibleLoginRefusedAfterNeedsLogin(t *testing.T) {
	p := Preset(ModeBalanced)
	a := refused(acct("A", TierMax5x, 10, time.Hour, 10, 72*time.Hour))
	if got := Eligible(a, p, t0); got != ReasonLoginRefused || ReasonLoginRefused != "login refused" {
		t.Fatalf("Eligible = %q, want %q", got, "login refused")
	}
	a.NeedsLogin = true
	if got := Eligible(a, p, t0); got != ReasonNeedsLogin {
		t.Fatalf("Eligible = %q, want needs login to win", got)
	}
	b := refused(acct("B", TierMax5x, 99, time.Hour, 10, 72*time.Hour))
	b.Limited = true
	if got := Eligible(b, p, t0); got != ReasonLoginRefused {
		t.Fatalf("Eligible = %q, want login refused before limited", got)
	}
	b.Rotates = false
	if got := Eligible(b, p, t0); got != ReasonOutOfRotation {
		t.Fatalf("Eligible = %q, want out of rotation first", got)
	}
}

func TestLoginBlocked(t *testing.T) {
	if (Account{}).LoginBlocked() {
		t.Fatal("zero account blocked")
	}
	if !(Account{NeedsLogin: true}).LoginBlocked() || !(Account{LoginRefused: true}).LoginBlocked() {
		t.Fatal("NeedsLogin and LoginRefused must both block")
	}
}

func TestLoginRefusedIsNotACandidateNorPlaced(t *testing.T) {
	p := Preset(ModeBalanced)
	mod := func(a *Account) { a.LoginRefused = true }
	if Candidate(placeAcct("A", mod), p, placeNow) {
		t.Fatal("refused account is a Candidate")
	}
	if got, ok := Place([]Account{placeAcct("A", mod)}, nil, "A", nil, p, placeNow); ok {
		t.Fatalf("Place = %q, true; want none", got)
	}
	got, ok := Place([]Account{placeAcct("A", mod), placeAcct("B", nil)}, nil, "A", nil, p, placeNow)
	if !ok || got != "B" {
		t.Fatalf("Place = %q, %v; want B", got, ok)
	}
}

func TestPlacePaceSkipsLoginRefused(t *testing.T) {
	a := paceAcct("a", 5, paceT0.Add(time.Hour))
	a.LoginRefused = true
	if _, _, ok := PlacePace(PaceInput{Accounts: []Account{a}, Params: Preset(ModeBalanced), Now: paceT0}); ok {
		t.Fatal("placed on a refused account")
	}
}

func TestFallbackEligibleExcludesLoginRefused(t *testing.T) {
	a := refused(acct("B", TierMax5x, 10, time.Hour, 10, 72*time.Hour))
	if FallbackEligible(a, t0) {
		t.Fatal("refused account is fallback eligible")
	}
}

func TestSimOrderExcludesLoginRefused(t *testing.T) {
	first := acct("A", TierMax5x, 10, time.Hour, 10, 72*time.Hour)
	roster := []Account{first, refused(acct("B", TierMax5x, 10, time.Hour, 10, 72*time.Hour)), acct("C", TierMax5x, 10, time.Hour, 10, 72*time.Hour)}
	if got := simOrderFor(first, roster, t0); len(got) != 2 {
		t.Fatalf("sim order has %d accounts, want 2 (A, C)", len(got))
	}
}

func TestHardTrigger(t *testing.T) {
	for tr, want := range map[string]bool{TriggerLimit: true, TriggerRefused: true, TriggerThreshold: false, "": false} {
		if got := HardTrigger(tr); got != want {
			t.Fatalf("HardTrigger(%q) = %v, want %v", tr, got, want)
		}
	}
	if TriggerRefused != "refused" {
		t.Fatalf("TriggerRefused = %q", TriggerRefused)
	}
}

func TestPlanLeavesARefusedServingAccount(t *testing.T) {
	a := refused(acct("A", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour))
	b := acct("B", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour)
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionSwitch || d.Target != "B" || d.Trigger != TriggerRefused || d.Fallback {
		t.Fatalf("Plan = %+v, want a switch to B on trigger refused", d)
	}
	if d.Reason != "switched A -> B (login refused)" {
		t.Fatalf("Reason = %q", d.Reason)
	}
	// A refusal wins over a limit on the same account.
	a.Limited, a.LimitWindow = true, Win5h
	if d := Plan(input(ModeBalanced, "A", a, b)); d.Trigger != TriggerRefused {
		t.Fatalf("Trigger = %q, want refused before limited", d.Trigger)
	}
}

func TestPlanRefusedFallsBackLikeALimit(t *testing.T) {
	a := refused(acct("A", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour))
	b := acct("B", TierMax5x, 95, 3*time.Hour, 10, 72*time.Hour) // above its switch point
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionSwitch || d.Target != "B" || !d.Fallback || d.Trigger != TriggerRefused {
		t.Fatalf("Plan = %+v, want a fallback switch to B", d)
	}
	if d.Reason != "switched A -> B (login refused; B above its switch point)" {
		t.Fatalf("Reason = %q", d.Reason)
	}
}

func TestPlanRefusedNoCandidate(t *testing.T) {
	a := refused(acct("A", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour))
	b := refused(acct("B", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour))
	d := Plan(input(ModeBalanced, "A", a, b))
	if d.Action != ActionNoCandidate || d.Trigger != TriggerRefused || d.Reason != "no account to switch to from A (login refused)" {
		t.Fatalf("Plan = %+v", d)
	}
	// A FallbackTarget flag never turns a hard refusal into a stay.
	in := input(ModeBalanced, "A", a, b)
	in.FallbackTarget = true
	if d := Plan(in); d.Action != ActionNoCandidate {
		t.Fatalf("Plan = %+v, want no-candidate", d)
	}
}

func TestPlanRefusedOffDoesNothing(t *testing.T) {
	in := input(ModeBalanced, "A", refused(acct("A", TierMax5x, 10, time.Hour, 10, 72*time.Hour)), acct("B", TierMax5x, 0, time.Hour, 0, 72*time.Hour))
	in.Enabled = false
	if d := Plan(in); d.Action != ActionOff {
		t.Fatalf("Plan = %+v, want off", d)
	}
}

func TestNextSkipsLoginRefused(t *testing.T) {
	a := acct("A", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour)
	b := refused(acct("B", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour))
	c := acct("C", TierMax5x, 10, 3*time.Hour, 10, 72*time.Hour)
	if got, ok := Next(input(ModeBalanced, "A", a, b, c)); !ok || got != "C" {
		t.Fatalf("Next = %q, %v; want C", got, ok)
	}
	// Even the fallback skips it.
	c.Pct5h = 95
	if got, ok := Next(input(ModeBalanced, "A", a, b, c)); !ok || got != "C" {
		t.Fatalf("Next = %q, %v; want C via fallback", got, ok)
	}
	if got, ok := Next(input(ModeBalanced, "A", a, b)); ok {
		t.Fatalf("Next = %q, true; want none", got)
	}
}
