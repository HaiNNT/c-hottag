package notify

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

type emitted struct {
	mu  sync.Mutex
	got [][2]string
}

func (e *emitted) emit(title, body string) {
	e.mu.Lock()
	e.got = append(e.got, [2]string{title, body})
	e.mu.Unlock()
}

func (e *emitted) all() [][2]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][2]string(nil), e.got...)
}

// newTestEvents returns Events on a fixed clock (Friday 2026-09-25 09:00
// UTC) and a switch, both settable through the returned pointers.
func newTestEvents() (*Events, *emitted, *time.Time, *bool) {
	em := &emitted{}
	now := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	on := true
	ev := NewEvents(Config{
		Emit:     em.emit,
		Enabled:  func() bool { return on },
		Now:      func() time.Time { return now },
		Location: time.UTC,
	})
	return ev, em, &now, &on
}

func TestNeedsLoginFiresOncePerAccountUntilSeenOK(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NeedsLogin("B")
	ev.NeedsLogin("B")
	ev.NeedsLogin("B")
	ev.AccountOK("B")
	ev.NeedsLogin("B")
	want := [][2]string{
		{"chottag: B needs login", "Its requests use your own Claude login until you run: chottag login B"},
		{"chottag: B needs login", "Its requests use your own Claude login until you run: chottag login B"},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
}

func TestNeedsLoginIsPerAccountAndCaseInsensitive(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NeedsLogin("B")
	ev.NeedsLogin("b")
	ev.NeedsLogin("C")
	if got := em.all(); len(got) != 2 || got[1][0] != "chottag: C needs login" {
		t.Fatalf("notices = %q, want one for B and one for C", got)
	}
	ev.AccountOK("b")
	ev.NeedsLogin("B")
	if got := em.all(); len(got) != 3 {
		t.Fatalf("notices = %q, want AccountOK(\"b\") to re-arm B", got)
	}
}

// TestNeedsLoginBurstFiresOnce is Review Focus 4.
func TestNeedsLoginBurstFiresOnce(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ev.NeedsLogin("B")
		}()
	}
	close(start)
	wg.Wait()
	if got := em.all(); len(got) != 1 {
		t.Fatalf("a burst of 64 needs-login passthroughs sent %d notices, want 1", len(got))
	}
}

func TestNeedsLoginIgnoresAnEmptyName(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NeedsLogin("")
	if got := em.all(); len(got) != 0 {
		t.Fatalf("notices = %q, want none for a nameless event", got)
	}
}

// TestAccountOKWithNothingPendingIsANoOp is the lock-free fast path: no
// account has an outstanding needs-login notice, so AccountOK must not
// allocate, take the lock, or otherwise disturb state. A NeedsLogin right
// after still fires normally.
func TestAccountOKWithNothingPendingIsANoOp(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.AccountOK("B")
	if got := em.all(); len(got) != 0 {
		t.Fatalf("notices = %q, want none from AccountOK with nothing pending", got)
	}
	ev.NeedsLogin("B")
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want NeedsLogin(\"B\") to fire after a no-op AccountOK", got)
	}
}

func TestAllLimitedFiresOnceThenAvailableOncePerEpisode(t *testing.T) {
	ev, em, now, _ := newTestEvents()
	reset := time.Date(2026, 9, 25, 17, 0, 0, 0, time.UTC)
	all := LimitState{AllLimited: true, NextReset: reset, NextResetAccount: "B"}
	avail := LimitState{Available: "A"}
	ev.Limits(avail) // the baseline: nothing to say
	ev.Limits(all)
	ev.Limits(all)
	*now = now.Add(11 * time.Minute)
	ev.Limits(avail)
	ev.Limits(avail)
	*now = now.Add(11 * time.Minute)
	ev.Limits(all)
	want := [][2]string{
		{"chottag: all accounts limited", "Next reset: B at Fri 17:00. Run: chottag status"},
		{"chottag: an account is available again", "A is no longer limited. Run: chottag status"},
		{"chottag: all accounts limited", "Next reset: B at Fri 17:00. Run: chottag status"},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
}

func TestAvailableNeedsAnAllLimitedNoticeFirst(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Limits(LimitState{Available: "A"})
	ev.Limits(LimitState{Available: "A"})
	if got := em.all(); len(got) != 0 {
		t.Fatalf("notices = %q, want none: nothing was limited", got)
	}
}

// A daemon that starts while every account is limited says so once (D11: a
// restart may repeat a still-true notice).
func TestAllLimitedAtStartupFiresOnce(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Limits(LimitState{AllLimited: true})
	ev.Limits(LimitState{AllLimited: true})
	if got := em.all(); len(got) != 1 || got[0][1] != "No reset time is known yet. Run: chottag status" {
		t.Fatalf("notices = %q, want one all-limited notice with an unknown reset", got)
	}
}

func TestAllLimitedWithAResetButNoAccountOmitsTheName(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Limits(LimitState{AllLimited: true, NextReset: time.Date(2026, 9, 26, 8, 5, 0, 0, time.UTC)})
	if got := em.all(); len(got) != 1 || got[0][1] != "Next reset: Sat 08:05. Run: chottag status" {
		t.Fatalf("notices = %q", got)
	}
}

func TestAvailableWithNoNameStillPointsAtStatus(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Limits(LimitState{AllLimited: true})
	ev.Limits(LimitState{})
	if got := em.all(); len(got) != 2 || got[1] != [2]string{"chottag: an account is available again", "Run: chottag status"} {
		t.Fatalf("notices = %q", got)
	}
}

// TestLimitsFlappingIsBoundedByTheFlapWindow is Review Focus 1: a roll-up
// flipping every 5s at a reset boundary posts one pair per window.
func TestLimitsFlappingIsBoundedByTheFlapWindow(t *testing.T) {
	ev, em, now, _ := newTestEvents()
	all := LimitState{AllLimited: true}
	avail := LimitState{Available: "A"}
	for i := 0; i < 50; i++ { // 500s, inside the 10-minute window
		ev.Limits(all)
		*now = now.Add(5 * time.Second)
		ev.Limits(avail)
		*now = now.Add(5 * time.Second)
	}
	if got := em.all(); len(got) != 2 {
		t.Fatalf("50 flips sent %d notices, want 2 (one all-limited, one available): %q", len(got), got)
	}
	*now = now.Add(DefaultFlapWindow)
	ev.Limits(all)
	ev.Limits(avail)
	got := em.all()
	if len(got) != 4 || got[2][0] != "chottag: all accounts limited" || got[3][0] != "chottag: an account is available again" {
		t.Fatalf("after the window: %q, want a second pair", got)
	}
}

func TestRouteDriftFiresOncePerGeneration(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.RouteDrift(0)
	ev.RouteDrift(1)
	ev.RouteDrift(7)
	want := [][2]string{{"chottag: route drift", "The daemon resent a swapped request unchanged; the route table may not match this Claude Code version. Run: chottag trace on"}}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
	next, em2, _, _ := newTestEvents() // a new daemon generation
	next.RouteDrift(7)
	if got := em2.all(); len(got) != 1 {
		t.Fatalf("a new generation sent %d route-drift notices, want 1", len(got))
	}
}

// TestOffSuppressesAndConsumes is Review Focus 2: nothing suppressed while
// off is replayed when notifications come back on.
func TestOffSuppressesAndConsumes(t *testing.T) {
	ev, em, _, on := newTestEvents()
	*on = false
	ev.NeedsLogin("B")
	ev.RouteDrift(1)
	ev.Limits(LimitState{AllLimited: true})
	*on = true
	ev.NeedsLogin("B")
	ev.RouteDrift(2)
	ev.Limits(LimitState{AllLimited: true})
	if got := em.all(); len(got) != 0 {
		t.Fatalf("notices = %q, want none: events seen while off are consumed", got)
	}
	ev.AccountOK("B")
	ev.NeedsLogin("B")
	if got := em.all(); len(got) != 1 || got[0][0] != "chottag: B needs login" {
		t.Fatalf("notices = %q, want B's needs-login after it was seen ok", got)
	}
}

func TestEnabledIsReadEachTimeANoticeWouldFire(t *testing.T) {
	ev, em, _, on := newTestEvents()
	ev.NeedsLogin("A")
	*on = false
	ev.NeedsLogin("B")
	*on = true
	ev.NeedsLogin("C")
	got := em.all()
	if len(got) != 2 || got[0][0] != "chottag: A needs login" || got[1][0] != "chottag: C needs login" {
		t.Fatalf("notices = %q, want A and C (B fired while off)", got)
	}
}

func TestNilEnabledMeansOn(t *testing.T) {
	em := &emitted{}
	ev := NewEvents(Config{Emit: em.emit})
	ev.NeedsLogin("A")
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want one (absent switch = on, D12)", got)
	}
}

// TestFlapSuppressedAllLimitedFiresLateOnceWindowElapsesStillLimited is the
// review's fix-round-1 finding 2: a flap-suppressed "all limited" notice is
// deferred, not dropped. Once FlapWindow has elapsed since the last one
// that actually fired, and the roll-up is still all-limited, it fires —
// exactly once, however many ticks land inside the window first.
func TestFlapSuppressedAllLimitedFiresLateOnceWindowElapsesStillLimited(t *testing.T) {
	ev, em, now, _ := newTestEvents()
	all := LimitState{AllLimited: true}
	avail := LimitState{Available: "A"}
	ev.Limits(all) // episode 1: fires immediately, lastAllSent = t0
	*now = now.Add(time.Minute)
	ev.Limits(avail)            // episode 1's "available"
	*now = now.Add(time.Minute) // t0 + 2m
	ev.Limits(all)              // episode 2: suppressed (2m < 10m since t0)
	if got := em.all(); len(got) != 2 {
		t.Fatalf("notices after the suppressed flip = %q, want still 2: nothing fires yet", got)
	}
	// A few more ticks while still all-limited and still inside the window:
	// still nothing.
	for i := 0; i < 3; i++ {
		*now = now.Add(time.Minute)
		ev.Limits(all)
	}
	if got := em.all(); len(got) != 2 {
		t.Fatalf("notices while still inside the window = %q, want still 2", got)
	}
	// Push past the 10-minute window measured from t0 (now at t0+5m; two
	// more minutes clears it).
	*now = now.Add(6 * time.Minute) // t0 + 11m
	ev.Limits(all)
	want := [][2]string{
		{"chottag: all accounts limited", "No reset time is known yet. Run: chottag status"},
		{"chottag: an account is available again", "A is no longer limited. Run: chottag status"},
		{"chottag: all accounts limited", "No reset time is known yet. Run: chottag status"},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q: exactly one late all-limited notice", got, want)
	}
	// It must not fire twice: another tick at the same state changes nothing.
	ev.Limits(all)
	if got := em.all(); len(got) != 3 {
		t.Fatalf("notices after a second all-limited tick = %q, want still 3", got)
	}
}

// TestFlapSuppressedAllLimitedDroppedIfAvailableBeforeWindowElapses is the
// review's fix-round-1 finding 2, the other half: if the roll-up flips back
// to available before the flap window elapses, the suppressed notice is
// dropped — nothing fires for it later, because there is nothing left to
// say.
func TestFlapSuppressedAllLimitedDroppedIfAvailableBeforeWindowElapses(t *testing.T) {
	ev, em, now, _ := newTestEvents()
	all := LimitState{AllLimited: true}
	avail := LimitState{Available: "A"}
	ev.Limits(all) // episode 1: fires, lastAllSent = t0
	*now = now.Add(2 * time.Minute)
	ev.Limits(avail)            // episode 1's "available"
	*now = now.Add(time.Minute) // t0 + 3m
	ev.Limits(all)              // episode 2: suppressed
	*now = now.Add(time.Minute) // t0 + 4m, still inside the window
	ev.Limits(avail)            // flips back before the window elapses: dropped
	if got := em.all(); len(got) != 2 {
		t.Fatalf("notices right after the early flip back = %q, want still 2", got)
	}
	*now = now.Add(DefaultFlapWindow) // well past the window now
	if got := em.all(); len(got) != 2 {
		t.Fatalf("notices after the window elapses with no further tick = %q, want still 2: the suppressed notice must never fire on its own", got)
	}
}

// TestAllLimitedSuppressedWhileOffNeverProducesAvailableAgain is PF3: an
// "all limited" notice consumed while notifications are off must not leave
// the episode's flag set, or the later flip to available would fire
// "available again" with no "all limited" ever having been shown.
func TestAllLimitedSuppressedWhileOffNeverProducesAvailableAgain(t *testing.T) {
	ev, em, _, on := newTestEvents()
	*on = false
	ev.Limits(LimitState{AllLimited: true})
	*on = true
	ev.Limits(LimitState{Available: "A"})
	if got := em.all(); len(got) != 0 {
		t.Fatalf("notices = %q, want none: the all-limited notice was consumed while off, so the flip back must stay silent", got)
	}
}

// TestEndAllLimitedEpisodeSuppressesTheNextAvailableNotice is item 9(c)
// (review round 2 test gap): EndAllLimitedEpisode exists so a switch whose
// own notice is deferred (a wall retry, M4 spec §4a) still ends the
// episode the moment the switch happens — a roster tick landing before the
// deferred notice fires must not also post "available again" (S8, review
// round 1 item 1). Exercised directly here, not just through Switched
// (which calls it internally): after it runs, the very next Limits(available)
// call must post nothing.
func TestEndAllLimitedEpisodeSuppressesTheNextAvailableNotice(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Limits(LimitState{AllLimited: true})
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want the one all-limited notice", got)
	}
	ev.EndAllLimitedEpisode("")
	ev.Limits(LimitState{Available: "A"})
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want no \"available again\": EndAllLimitedEpisode already closed the episode", got)
	}
}

// TestEndAllLimitedEpisodeLeavesAFreshEpisodeFireable pins that ending an
// episode does not latch anything shut forever: a LATER all-limited spell
// still posts its own notice, and the available-again after IT still
// fires normally.
func TestEndAllLimitedEpisodeLeavesAFreshEpisodeFireable(t *testing.T) {
	ev, em, now, _ := newTestEvents()
	ev.Limits(LimitState{AllLimited: true})
	ev.EndAllLimitedEpisode("")
	ev.Limits(LimitState{Available: "A"}) // suppressed by the line above
	*now = now.Add(time.Hour)             // well past DefaultFlapWindow
	ev.Limits(LimitState{AllLimited: true})
	ev.Limits(LimitState{Available: "B"})
	want := [][2]string{
		{"chottag: all accounts limited", "No reset time is known yet. Run: chottag status"},
		{"chottag: all accounts limited", "No reset time is known yet. Run: chottag status"},
		{"chottag: an account is available again", "B is no longer limited. Run: chottag status"},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
}

// TestEndAllLimitedEpisodeClearsANoCandidateEpisodeToo pins the other half
// of its doc comment ("clears the ... and no-candidate episode
// bookkeeping"): it lets a fresh NoCandidate fire again at once, without a
// CandidateOK in between.
func TestEndAllLimitedEpisodeClearsANoCandidateEpisodeToo(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NoCandidate("", "", "A", false)
	ev.NoCandidate("", "", "A", false) // already suppressed: one episode
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want one no-candidate notice", got)
	}
	ev.EndAllLimitedEpisode("")
	ev.NoCandidate("", "", "A", false)
	if got := em.all(); len(got) != 2 {
		t.Fatalf("notices = %q, want a second no-candidate notice: EndAllLimitedEpisode also cleared noCandSent", got)
	}
}

// TestEndAllLimitedEpisodeIsANoOpOutsideAnAllLimitedEpisode: called with no
// episode open at all, it must not disturb a later, unrelated all-limited
// episode's own notice.
func TestEndAllLimitedEpisodeIsANoOpOutsideAnAllLimitedEpisode(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.EndAllLimitedEpisode("")
	ev.Limits(LimitState{AllLimited: true})
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want the all-limited notice to still fire normally", got)
	}
}

func TestMovedNotice(t *testing.T) {
	ev, em, _, on := newTestEvents()
	ev.Moved(Moved{From: "A", To: []string{"B", "C"}, Sessions: 3, Window: "5h"})
	ev.Moved(Moved{From: "A", To: []string{"B"}, Sessions: 1, Window: "7d", Limited: true})
	ev.Moved(Moved{From: "A", Sessions: 2, Window: "5h"})
	ev.Moved(Moved{From: "A", To: []string{"B"}, Sessions: 2})
	want := [][2]string{
		{"chottag: moved 3 sessions from A to B, C", "A reached its 5-hour switch point."},
		{"chottag: moved 1 session from A to B", "A hit its 7-day limit."},
		{"chottag: 2 sessions on A have no account to move to", "A reached its 5-hour switch point."},
		{"chottag: moved 2 sessions from A to B", "A is over its switch point."},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q, want %q", got, want)
	}
	*on = false
	ev.Moved(Moved{From: "A", To: []string{"B"}, Sessions: 1})
	if len(em.all()) != 4 {
		t.Fatal("a notice was posted with notifications off")
	}
}
