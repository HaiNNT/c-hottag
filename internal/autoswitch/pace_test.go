package autoswitch

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usage"
)

var paceT0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func snap(p5 float64, r5 time.Time, p7 float64, r7 time.Time) usage.Snapshot {
	return usage.Snapshot{
		Known:    true,
		FiveHour: usage.Window{Utilization: p5 / 100, HasUtilization: true, ResetsAt: r5, Known: true},
		SevenDay: usage.Window{Utilization: p7 / 100, HasUtilization: true, ResetsAt: r7, Known: true},
	}
}

func snap5(p5 float64) usage.Snapshot { return snap(p5, time.Time{}, 0, time.Time{}) }

func nearf(t *testing.T, got, want, eps float64) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Fatalf("got %v, want %v (+-%v)", got, want, eps)
	}
}

func TestPaceObserveRiseIsBurn(t *testing.T) {
	var p Pace
	p.Observe("A", 20, snap5(10), paceT0)
	p.Observe("A", 20, snap5(20), paceT0.Add(30*time.Minute))
	r, ok := p.AccountBurn("A", Win5h, paceT0.Add(30*time.Minute))
	if !ok {
		t.Fatal("not observed")
	}
	nearf(t, r, 400, 1e-6) // 10 points of 20 units = 200 units in 0.5h
	if _, ok := p.AccountBurn("B", Win5h, paceT0); ok {
		t.Fatal("never observed must be !ok")
	}
}

func TestPaceRoundingFallIgnored(t *testing.T) {
	var p Pace
	p.Observe("A", 20, snap5(40), paceT0)
	p.Observe("A", 20, snap5(39), paceT0.Add(time.Minute))
	r, _ := p.AccountBurn("A", Win5h, paceT0.Add(time.Minute))
	if r != 0 {
		t.Fatalf("fall produced rate %v", r)
	}
	p.Observe("A", 20, snap5(41), paceT0.Add(2*time.Minute))
	r, _ = p.AccountBurn("A", Win5h, paceT0.Add(2*time.Minute))
	// baseline stayed 40, so the rise is 1 point of 20 units = 20 over 2 min
	nearf(t, r, 20/(2.0/60), 20) // slightly decayed
}

func TestPaceResetRestarts(t *testing.T) {
	var p Pace
	reset := paceT0.Add(time.Hour)
	p.Observe("A", 20, snap(90, reset, 0, time.Time{}), paceT0)
	p.Observe("A", 20, snap(3, reset.Add(5*time.Hour), 0, time.Time{}), paceT0.Add(10*time.Minute))
	r, _ := p.AccountBurn("A", Win5h, paceT0.Add(10*time.Minute))
	if r != 0 {
		t.Fatalf("fall below half is a reset, rate %v", r)
	}
	// a reading after the known reset restarts even when it did not fall by half
	var q Pace
	q.Observe("A", 20, snap(40, reset, 0, time.Time{}), paceT0)
	q.Observe("A", 20, snap(60, reset.Add(5*time.Hour), 0, time.Time{}), reset.Add(time.Minute))
	r, _ = q.AccountBurn("A", Win5h, reset.Add(time.Minute))
	if r != 0 {
		t.Fatalf("reading after reset must not count a rise, rate %v", r)
	}
}

func TestPaceIdleGapDecays(t *testing.T) {
	var p Pace
	p.Observe("A", 20, snap5(10), paceT0)
	p.Observe("A", 20, snap5(20), paceT0.Add(30*time.Minute))
	busy, _ := p.AccountBurn("A", Win5h, paceT0.Add(30*time.Minute))
	later, _ := p.AccountBurn("A", Win5h, paceT0.Add(30*time.Minute+4*time.Hour))
	if later >= busy/4 {
		t.Fatalf("idle did not decay: %v -> %v", busy, later)
	}
	// a rise after a gap > 4 tau is not burn
	p.Observe("A", 20, snap5(60), paceT0.Add(30*time.Minute+4*time.Hour))
	after, _ := p.AccountBurn("A", Win5h, paceT0.Add(30*time.Minute+4*time.Hour))
	if after > later+1e-9 {
		t.Fatalf("rise across an idle gap counted: %v -> %v", later, after)
	}
}

func TestPaceOutOfOrderIgnored(t *testing.T) {
	var p Pace
	p.Observe("A", 20, snap5(10), paceT0.Add(time.Hour))
	p.Observe("A", 20, snap5(50), paceT0)
	r, _ := p.AccountBurn("A", Win5h, paceT0.Add(time.Hour))
	if r != 0 {
		t.Fatalf("out-of-order reading changed rate: %v", r)
	}
}

// feedSessions alternates credits between sids for n rounds, one round per minute.
func feedSessions(p *Pace, account string, units float64, sids []string, start time.Time, rounds int, stepPct float64) time.Time {
	pct := 10.0
	p.Observe(account, units, snap5(pct), start)
	at := start
	for i := 0; i < rounds; i++ {
		at = at.Add(time.Minute)
		pct += stepPct
		p.Observe(account, units, snap5(pct), at)
		p.Credit(sids[i%len(sids)], account, units, at)
	}
	return at
}

func TestPaceCreditToSession(t *testing.T) {
	var p Pace
	end := feedSessions(&p, "A", 20, []string{"s1", "s2"}, paceT0, 40, 0.5)
	r1, m1 := p.SessionBurn("s1", end)
	r2, m2 := p.SessionBurn("s2", end)
	if !m1 || !m2 {
		t.Fatalf("measured = %v %v", m1, m2)
	}
	acct, _ := p.AccountBurn("A", Win5h, end)
	nearf(t, r1+r2, acct, acct*0.2)
	nearf(t, r1, r2, acct*0.2)
	if r1 <= 0 {
		t.Fatalf("rate %v", r1)
	}
}

func TestPaceUnmeasuredUsesPrior(t *testing.T) {
	var p Pace
	end := feedSessions(&p, "A", 20, []string{"s1"}, paceT0, 5, 0.5)
	if _, m := p.SessionBurn("s1", end); m {
		t.Fatal("5 minutes of credit must be unmeasured")
	}
	if _, m := p.SessionBurn("unknown", end); m {
		t.Fatal("unknown sid must be unmeasured")
	}
	// inputs for placement use the prior for it
	prior := p.Prior(func(string) bool { return true }, end)
	if prior != DefaultSessionBurn {
		t.Fatalf("prior with no measured sessions = %v", prior)
	}
}

func TestPacePriorFloorAndDefault(t *testing.T) {
	var p Pace
	live := func(string) bool { return true }
	if got := p.Prior(live, paceT0); got != DefaultSessionBurn {
		t.Fatalf("none: %v", got)
	}
	// two measured sessions credited with nothing: floor
	end := feedSessions(&p, "A", 20, []string{"s1", "s2"}, paceT0, 40, 0)
	if got := p.Prior(live, end); got != MinSessionBurn {
		t.Fatalf("idle measured: %v", got)
	}
	// inactive for more than 30 minutes: excluded, back to the default
	if got := p.Prior(live, end.Add(31*time.Minute)); got != DefaultSessionBurn {
		t.Fatalf("inactive: %v", got)
	}
	// not live: excluded
	if got := p.Prior(func(string) bool { return false }, end); got != DefaultSessionBurn {
		t.Fatalf("not live: %v", got)
	}
}

func TestPaceMedianOfMeasured(t *testing.T) {
	var p Pace
	var end time.Time
	for i, step := range []float64{1, 2, 3} {
		acct := fmt.Sprintf("A%d", i)
		end = feedSessions(&p, acct, 20, []string{"s" + acct}, paceT0, 40, step)
	}
	r := []float64{}
	for _, a := range []string{"sA0", "sA1", "sA2"} {
		v, m := p.SessionBurn(a, end)
		if !m {
			t.Fatal("unmeasured")
		}
		r = append(r, v)
	}
	nearf(t, p.Prior(func(string) bool { return true }, end), r[1], 1e-9)
}

func TestPaceJSONRoundTripAndPrune(t *testing.T) {
	var p Pace
	end := feedSessions(&p, "A", 20, []string{"s1", "s2"}, paceT0, 40, 0.5)
	b, err := json.Marshal(&p)
	if err != nil {
		t.Fatal(err)
	}
	var q Pace
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	a1, _ := p.AccountBurn("A", Win5h, end)
	a2, ok := q.AccountBurn("A", Win5h, end)
	if !ok || a1 != a2 {
		t.Fatalf("account burn %v vs %v", a1, a2)
	}
	s1, _ := p.SessionBurn("s1", end)
	s2, m := q.SessionBurn("s1", end)
	if !m || s1 != s2 {
		t.Fatalf("session burn %v vs %v", s1, s2)
	}
	q.Prune(func(sid string) bool { return sid == "s2" })
	if _, m := q.SessionBurn("s1", end); m {
		t.Fatal("pruned session still measured")
	}
	if _, m := q.SessionBurn("s2", end); !m {
		t.Fatal("kept session lost")
	}
}

// ---- placement

func paceAcct(name string, p5 float64, reset5 time.Time) Account {
	return Account{Name: name, Tier: TierMax20x, Rotates: true, Fresh: true, Recent: true,
		Has5h: true, Pct5h: p5, Reset5h: reset5}
}

func sids(prefix string, n int) []string {
	var s []string
	for i := 0; i < n; i++ {
		s = append(s, fmt.Sprintf("%s%d", prefix, i))
	}
	return s
}

func TestPlacePaceSoonResetWins(t *testing.T) {
	in := PaceInput{
		Accounts: []Account{
			paceAcct("bob", 7, paceT0.Add(4*time.Hour+15*time.Minute)),
			paceAcct("alice", 24, paceT0.Add(45*time.Minute)),
		},
		Sessions: map[string][]string{"alice": sids("a", 5), "bob": sids("b", 5)},
		Pace:     &Pace{},
		Subject:  "new",
		Params:   Preset(ModeBalanced),
		Now:      paceT0,
	}
	// five sessions at 300 u/h each: the prior would be 200, so measure them.
	for i := range in.Accounts {
		in.Accounts[i].Has7d, in.Accounts[i].Pct7d, in.Accounts[i].Reset7d = true, 30, paceT0.Add(3*time.Hour)
	}
	in.Pace = pacedWith(t, 300, append(sids("a", 5), sids("b", 5)...))
	name, reason, ok := PlacePace(in)
	if !ok || name != "alice" {
		t.Fatalf("got %q %q %v", name, reason, ok)
	}
	rw, _ := PaceRatio(in, "alice")
	rt, _ := PaceRatio(in, "bob")
	if rw-rt <= 0.05 {
		t.Fatalf("must win on score, not tie-break: %v vs %v", rw, rt)
	}
	if reason == "" || reason == "soonest reset" {
		t.Fatalf("reason %q", reason)
	}
}

// pacedWith returns a Pace in which every sid is measured at rate u/h.
func pacedWith(t *testing.T, rate float64, ids []string) *Pace {
	t.Helper()
	p := &Pace{Sessions: map[string]*SessionPace{}}
	for _, id := range ids {
		p.Sessions[id] = &SessionPace{First: paceT0.Add(-time.Hour), At: paceT0, LastCredit: paceT0, Rise: rate, Wall: 3600}
	}
	return p
}

func TestPlacePaceBurstSpreads(t *testing.T) {
	in := PaceInput{
		Accounts: []Account{
			paceAcct("a", 10, paceT0.Add(2*time.Hour)),
			paceAcct("b", 10, paceT0.Add(2*time.Hour)),
			paceAcct("c", 10, paceT0.Add(2*time.Hour)),
		},
		Sessions: map[string][]string{},
		Pace:     &Pace{},
		Params:   Preset(ModeBalanced),
		Now:      paceT0,
	}
	counts := map[string]int{}
	for i := 0; i < 5; i++ {
		in.Subject = fmt.Sprintf("s%d", i)
		n, _, ok := PlacePace(in)
		if !ok {
			t.Fatal("no placement")
		}
		counts[n]++
		in.Sessions[n] = append(in.Sessions[n], in.Subject)
	}
	if len(counts) < 2 {
		t.Fatalf("all on one account: %v", counts)
	}
	for n, c := range counts {
		if c > 3 {
			t.Fatalf("%s took %d: %v", n, c, counts)
		}
	}
}

func TestPlacePaceExcludesImminentWall(t *testing.T) {
	// a is at 95%: little room and a heavy session load, 5h reset far away
	hot := paceAcct("hot", 95, paceT0.Add(4*time.Hour))
	cool := paceAcct("cool", 50, paceT0.Add(4*time.Hour))
	in := PaceInput{
		Accounts: []Account{hot, cool},
		Sessions: map[string][]string{"hot": sids("h", 3)},
		Pace:     pacedWith(t, 400, sids("h", 3)),
		Subject:  "n",
		Params:   Preset(ModeBalanced),
		Now:      paceT0,
	}
	n, _, ok := PlacePace(in)
	if !ok || n != "cool" {
		t.Fatalf("got %q %v", n, ok)
	}
	// every candidate excluded so: they still compete
	in.Accounts = []Account{hot}
	n, _, ok = PlacePace(in)
	if !ok || n != "hot" {
		t.Fatalf("all-excluded fallback: %q %v", n, ok)
	}
}

func TestPlacePacePinWins(t *testing.T) {
	in := PaceInput{
		Accounts: []Account{paceAcct("a", 5, paceT0.Add(time.Hour)), paceAcct("b", 90, paceT0.Add(time.Hour))},
		Pace:     &Pace{}, Pin: "b", Subject: "n", Params: Preset(ModeBalanced), Now: paceT0,
		Sessions: map[string][]string{},
	}
	n, _, ok := PlacePace(in)
	if !ok || n != "b" {
		t.Fatalf("got %q %v", n, ok)
	}
	in.Exclude = map[string]bool{"b": true}
	if n, _, _ = PlacePace(in); n != "a" {
		t.Fatalf("excluded pin placed: %q", n)
	}
}

func TestPlacePaceUnknownResetKeepsUsed(t *testing.T) {
	in := PaceInput{
		Accounts: []Account{paceAcct("used", 79, time.Time{}), paceAcct("fresh", 20, time.Time{})},
		Pace:     &Pace{}, Subject: "n", Params: Preset(ModeBalanced), Now: paceT0,
		Sessions: map[string][]string{},
	}
	n, _, ok := PlacePace(in)
	if !ok || n != "fresh" {
		t.Fatalf("got %q %v", n, ok)
	}
}

func TestPlacePaceNoCandidate(t *testing.T) {
	a := paceAcct("a", 5, paceT0.Add(time.Hour))
	a.NeedsLogin = true
	if _, _, ok := PlacePace(PaceInput{Accounts: []Account{a}, Params: Preset(ModeBalanced), Now: paceT0}); ok {
		t.Fatal("placed on an ineligible account")
	}
}

func TestPlacePaceMoveDoesNotDoubleCount(t *testing.T) {
	// two accounts, identical but for where s0 lives; s0 burns 600 u/h.
	ids := []string{"s0"}
	p := pacedWith(t, 600, ids)
	mk := func(from string) PaceInput {
		return PaceInput{
			Accounts: []Account{paceAcct("a", 30, paceT0.Add(3*time.Hour)), paceAcct("b", 30, paceT0.Add(3*time.Hour))},
			Sessions: map[string][]string{from: ids},
			Pace:     p, Subject: "s0", SubjectFrom: from, Params: Preset(ModeBalanced), Now: paceT0,
		}
	}
	inA := mk("a")
	rA, _ := PaceRatio(inA, "a")
	rB, _ := PaceRatio(inA, "b")
	// staying (a) and moving (b) give the same load: the subject counts once either way
	nearf(t, rA, rB, 1e-9)
	// and with the subject not removed the ratio would be lower
	naive := inA
	naive.SubjectFrom, naive.Subject = "", "other"
	rNaive, _ := PaceRatio(naive, "a")
	if rNaive >= rA {
		t.Fatalf("subject counted twice on its own account: %v >= %v", rNaive, rA)
	}
}

func paceAcct7(name string, p5 float64, r5 time.Time, p7 float64, r7 time.Time) Account {
	a := paceAcct(name, p5, r5)
	a.Has7d, a.Pct7d, a.Reset7d = true, p7, r7
	return a
}

// lightInput is a placement of a measured 50 u/h subject "n" onto accounts.
func lightInput(t *testing.T, accts ...Account) PaceInput {
	return PaceInput{Accounts: accts, Pace: pacedWith(t, 50, []string{"n"}), Subject: "n",
		Params: Preset(ModeBalanced), Now: paceT0, Sessions: map[string][]string{}}
}

// measure7 feeds account's meters so its measured 7d burn is about
// stepPct% of its units per 10 minutes, ending at paceT0.
func measure7(p *Pace, account string, units, p7 float64) {
	start := paceT0.Add(-time.Hour)
	for i := 0; i <= 6; i++ {
		p.Observe(account, units, snap(10, time.Time{}, p7+float64(i), time.Time{}), start.Add(time.Duration(i)*10*time.Minute))
	}
}

func TestPaceRatio7dBinds(t *testing.T) {
	r5, r7 := paceT0.Add(4*time.Hour), paceT0.Add(48*time.Hour)
	in := lightInput(t, paceAcct7("A", 5, r5, 90, r7), paceAcct7("B", 5, r5, 5, r7))
	measure7(in.Pace, "A", 20, 84)
	ra, wa := PaceRatio(in, "A")
	rb, wb := PaceRatio(in, "B")
	if wa != Win7d || wb != Win5h {
		t.Fatalf("binding a=%v b=%v", wa, wb)
	}
	if ra >= rb {
		t.Fatalf("weekly projected to run out must score below a fresh one: %v >= %v", ra, rb)
	}
	if rb != RCap {
		t.Fatalf("light subject on a fresh account must hit the cap: %v", rb)
	}
	n, reason, _ := PlacePace(in)
	if n != "B" || !strings.HasPrefix(reason, "5h") {
		t.Fatalf("got %q %q", n, reason)
	}
	// the account that is penalised names 7d in its reason when it is the only one
	in.Accounts = in.Accounts[:1]
	if n, reason, _ = PlacePace(in); n != "A" || !strings.HasPrefix(reason, "7d") {
		t.Fatalf("got %q %q", n, reason)
	}
}

func TestPaceWeeklyPenalises(t *testing.T) {
	r5, r7 := paceT0.Add(3*time.Hour), paceT0.Add(100*time.Hour)
	// A is first in order and equal on 5h; only its measured weekly burn runs out before its reset
	in := lightInput(t, paceAcct7("A", 20, r5, 90, r7), paceAcct7("B", 20, r5, 10, r7))
	measure7(in.Pace, "A", 20, 84)
	n, _, ok := PlacePace(in)
	if !ok || n != "B" {
		t.Fatalf("got %q %v", n, ok)
	}
}

func TestPaceWeeklyNeutralWhenUnmeasured(t *testing.T) {
	r5, r7 := paceT0.Add(3*time.Hour), paceT0.Add(100*time.Hour)
	// A's weekly is 97% used but there is no measured 7d burn: neutral, so A wins on order
	in := lightInput(t, paceAcct7("A", 20, r5, 97, r7), paceAcct7("B", 20, r5, 10, r7))
	ra, wa := PaceRatio(in, "A")
	rb, _ := PaceRatio(in, "B")
	if ra != rb || wa != Win5h {
		t.Fatalf("unmeasured weekly must be neutral: %v %v (%v)", ra, rb, wa)
	}
	// session rates (a heavy load) never feed it either
	in.Sessions = map[string][]string{"A": sids("h", 3)}
	in.Pace = pacedWith(t, 400, append(sids("h", 3), "n"))
	if _, w := PaceRatio(in, "A"); w != Win5h {
		t.Fatalf("sessions fed the weekly factor: %v", w)
	}
}

func TestPlacePaceTieFewerSessions(t *testing.T) {
	r5 := paceT0.Add(2 * time.Hour)
	in := PaceInput{
		Accounts: []Account{paceAcct("A", 10, r5), paceAcct("B", 10, r5)},
		// a measured, near-zero session keeps both at the cap
		Sessions: map[string][]string{"A": {"x", "y"}, "B": {"z"}},
		Pace:     pacedWith(t, 0.1, []string{"x", "y", "z", "n"}),
		Subject:  "n", Params: Preset(ModeBalanced), Now: paceT0,
	}
	n, reason, _ := PlacePace(in)
	if n != "B" || reason != "fewest sessions" {
		t.Fatalf("got %q %q", n, reason)
	}
}

func TestPlacePaceReasonColdNames5h(t *testing.T) {
	in := lightInput(t, paceAcct("A", 20, paceT0.Add(2*time.Hour)))
	_, reason, _ := PlacePace(in)
	if reason != "5h 80% left, resets in 2h" {
		t.Fatalf("reason %q", reason)
	}
}

func TestPaceNoCreditAfterIdleGapOrReset(t *testing.T) {
	var p Pace
	p.Observe("A", 20, snap5(10), paceT0)
	p.Observe("A", 20, snap5(60), paceT0.Add(5*time.Hour)) // idle gap
	p.Credit("s", "A", 20, paceT0.Add(5*time.Hour))
	if sp := p.Sessions["s"]; sp.Rise != 0 {
		t.Fatalf("idle-gap rise credited: %v", sp.Rise)
	}
	var q Pace
	q.Observe("A", 20, snap(90, paceT0.Add(time.Hour), 0, time.Time{}), paceT0)
	q.Observe("A", 20, snap5(95), paceT0.Add(2*time.Hour)) // after the known reset
	q.Credit("s", "A", 20, paceT0.Add(2*time.Hour))
	if sp := q.Sessions["s"]; sp.Rise != 0 {
		t.Fatalf("reset-branch rise credited: %v", sp.Rise)
	}
}

func TestPaceWeeklyNeutralAboveOne(t *testing.T) {
	r5, r7 := paceT0.Add(3*time.Hour), paceT0.Add(100*time.Hour)
	in := lightInput(t, paceAcct7("a", 20, r5, 10, r7), paceAcct7("b", 20, r5, 40, r7))
	ra, _ := PaceRatio(in, "a")
	rb, wb := PaceRatio(in, "b")
	if ra != rb || wb != Win5h {
		t.Fatalf("weekly above 1 must be neutral: %v %v (%v)", ra, rb, wb)
	}
}

// A near reset must not attract a pile-up (spec §10): ten new sessions at the
// prior between a 10%-used account resetting in 4h and a 40%-used one about
// to reset must reach both accounts, roughly balanced.
func TestPlacePaceNearResetNoPileUp(t *testing.T) {
	for _, soon := range []time.Duration{15 * time.Minute, 5 * time.Minute} {
		in := PaceInput{
			Accounts: []Account{
				paceAcct("A", 10, paceT0.Add(4*time.Hour)),
				paceAcct("B", 40, paceT0.Add(soon)),
			},
			Sessions: map[string][]string{},
			Pace:     &Pace{},
			Params:   Preset(ModeBalanced),
			Now:      paceT0,
		}
		counts := map[string]int{}
		for i := 0; i < 10; i++ {
			in.Subject = fmt.Sprintf("s%d", i)
			n, _, ok := PlacePace(in)
			if !ok {
				t.Fatal("no placement")
			}
			counts[n]++
			in.Sessions[n] = append(in.Sessions[n], in.Subject)
		}
		if counts["A"] == 0 || counts["B"] == 0 {
			t.Fatalf("reset in %v: one account took all ten: %v", soon, counts)
		}
		if counts["A"] < 3 || counts["B"] < 3 {
			t.Fatalf("reset in %v: not balanced: %v", soon, counts)
		}
	}
}

// ttl counts the refill: an account that reaches its reset first has a whole
// window after it (r5h = (T + point*units/rate) / 5h), capped by RCap.
func TestPaceRatio5hHorizonCountsRefill(t *testing.T) {
	// room is (switch point - 20)% of the units, at a 400 u/h load, reset in 1h
	in := lightInput(t, paceAcct("A", 20, paceT0.Add(time.Hour)))
	in.Pace = pacedWith(t, 400, []string{"n"})
	pt := float64(in.Params.SwitchPoint(Win5h, TierMax20x))
	units := in.Accounts[0].units()
	room := (pt - 20) * units
	if room/400 < 1 {
		t.Fatalf("fixture: ttl_pre %v must exceed the 1h reset", room/400)
	}
	want := math.Min((1+pt*units/400)/5, RCap)
	got, _ := PaceRatio(in, "A")
	nearf(t, got, want, 1e-9)
	// before the reset: no refill term
	in.Pace = pacedWith(t, 4000, []string{"n"})
	got, _ = PaceRatio(in, "A")
	nearf(t, got, math.Min(room/4000/5, RCap), 1e-9)
}

func TestPaceClockStepBackRestartsBaseline(t *testing.T) {
	var p Pace
	p.Observe("A", 20, snap5(10), paceT0.Add(5*time.Hour))
	// the clock stepped back by more than tau: restart at this reading
	p.Observe("A", 20, snap5(50), paceT0)
	p.Observe("A", 20, snap5(60), paceT0.Add(30*time.Minute))
	r, ok := p.AccountBurn("A", Win5h, paceT0.Add(30*time.Minute))
	if !ok || r <= 0 {
		t.Fatalf("a restarted window must measure again: %v %v", r, ok)
	}
	nearf(t, r, 10*20/0.5, 1e-6) // 10 points of 20 units in 0.5h
	// a small step back (under tau) is still ignored
	var q Pace
	q.Observe("A", 20, snap5(10), paceT0.Add(time.Hour))
	q.Observe("A", 20, snap5(50), paceT0.Add(time.Hour-time.Minute))
	if r, _ := q.AccountBurn("A", Win5h, paceT0.Add(time.Hour)); r != 0 {
		t.Fatalf("small out-of-order reading changed rate: %v", r)
	}
}
