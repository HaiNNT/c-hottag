package notify

import (
	"reflect"
	"testing"
	"time"
)

// TestSwitchedNoticeTexts pins spec §7's texts: a threshold switch names
// the percentage; a wall switch says where the request went when the
// resend succeeded, and asks for a resend when it did not.
func TestSwitchedNoticeTexts(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Switched(Switch{From: "A", To: "B", Trigger: "threshold", Window: "5h", Pct: 96.4})
	ev.Switched(Switch{From: "A", To: "B", Trigger: "limit", Window: "5h", Retried: true})
	ev.Switched(Switch{From: "B", To: "C", Trigger: "limit", Window: "7d"})
	ev.Switched(Switch{From: "C", To: "D", Trigger: "limit"})
	want := [][2]string{
		{"chottag: switched to B", "A reached 96% of its 5-hour limit."},
		{"chottag: switched to B", "A hit its 5-hour limit; your request went to B."},
		{"chottag: switched to C", "B hit its 7-day limit. Resend your last message."},
		{"chottag: switched to D", "C hit its usage limit. Resend your last message."},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
}

// TestSwitchDuringAllLimitedReplacesAvailableAgain is Review Focus 4 in
// the state machine (S8): all limited, then a switch at the reset, then
// the roll-up flips to available. Exactly two notices: "all limited" and
// "switched to", never "available again".
func TestSwitchDuringAllLimitedReplacesAvailableAgain(t *testing.T) {
	ev, em, now, _ := newTestEvents()
	reset := time.Date(2026, 9, 25, 17, 0, 0, 0, time.UTC)
	ev.Limits(LimitState{}.withAll(reset))
	ev.Switched(Switch{From: "A", To: "B", Trigger: "limit", Window: "5h"})
	ev.Limits(LimitState{Available: "B"})
	got := em.all()
	if len(got) != 2 || got[0][0] != "chottag: all accounts limited" || got[1][0] != "chottag: switched to B" {
		t.Fatalf("notices = %q, want all limited then switched to B, and no available again", got)
	}
	// The next episode is untouched: all limited again, then a reset with
	// no switch (the serving account itself recovered) says available.
	// 11 minutes on, past the 10-minute flap window.
	*now = now.Add(11 * time.Minute)
	ev.Limits(LimitState{}.withAll(reset))
	ev.Limits(LimitState{Available: "A"})
	if got := em.all(); len(got) != 4 || got[3][0] != "chottag: an account is available again" {
		t.Fatalf("notices = %q; a later episode without a switch must still say available again", got)
	}
}

// With notifications off, the switch posts nothing, and the replacement
// still holds: turning them back on never produces a stray "available
// again" for that episode.
func TestSwitchedRespectsNotifyOff(t *testing.T) {
	ev, em, _, on := newTestEvents()
	ev.Limits(LimitState{}.withAll(time.Time{}))
	*on = false
	ev.Switched(Switch{From: "A", To: "B", Trigger: "threshold", Window: "5h", Pct: 95})
	*on = true
	ev.Limits(LimitState{Available: "B"})
	if got := em.all(); len(got) != 1 || got[0][0] != "chottag: all accounts limited" {
		t.Fatalf("notices = %q, want only the all-limited one", got)
	}
}

func TestNoCandidateOncePerEpisode(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NoCandidate("", "", "A", false)
	ev.NoCandidate("", "", "A", false)
	want := [][2]string{{"chottag: no account to switch to", "A needs to switch, but no other account can serve now. Run: chottag status"}}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
	ev.CandidateOK("")
	ev.NoCandidate("", "", "A", false)
	if got := em.all(); len(got) != 2 {
		t.Fatalf("notices = %q, want a second one after the episode ended", got)
	}
	ev.Switched(Switch{From: "A", To: "B", Trigger: "limit", Window: "5h"})
	ev.NoCandidate("", "", "B", false)
	if got := em.all(); len(got) != 4 || got[3][0] != "chottag: no account to switch to" {
		t.Fatalf("notices = %q; a switch ends the episode too", got)
	}
}

// TestNoCandidateAddsNothingWhileAllLimited (S8): the all-limited notice
// already says there is nowhere to go. If the episode outlasts the
// all-limited state, it posts then.
func TestNoCandidateAddsNothingWhileAllLimited(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NoCandidate("", "", "A", true)
	ev.NoCandidate("", "", "A", true)
	if got := em.all(); len(got) != 0 {
		t.Fatalf("notices = %q, want none while every account is limited", got)
	}
	ev.NoCandidate("", "", "A", false)
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want one once not every account is limited", got)
	}
}

// withAll is a LimitState with every account limited and the given reset.
func (s LimitState) withAll(reset time.Time) LimitState {
	s.AllLimited, s.NextReset = true, reset
	if !reset.IsZero() {
		s.NextResetAccount = "B"
	}
	return s
}

// Each pool has its own no-candidate episode (M8): one pool's cannot silence
// another's, a switch or CandidateOK ends only its own pool's, and the
// notice names the pool.
func TestNoCandidateEpisodesAreKeyedByPool(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NoCandidate("work", "work", "A", false)
	ev.NoCandidate("personal", "personal", "C", false)
	ev.NoCandidate("work", "work", "A", false) // same episode
	want := [][2]string{
		{"chottag: work: no account to switch to", "A needs to switch, but no other account can serve now. Run: chottag status"},
		{"chottag: personal: no account to switch to", "C needs to switch, but no other account can serve now. Run: chottag status"},
	}
	if got := em.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices = %q\nwant %q", got, want)
	}
	ev.CandidateOK("personal")
	ev.NoCandidate("work", "work", "A", false)
	if len(em.all()) != 2 {
		t.Fatalf("personal's CandidateOK ended work's episode: %q", em.all())
	}
	ev.NoCandidate("personal", "personal", "C", false)
	if len(em.all()) != 3 {
		t.Fatalf("personal's episode did not restart: %q", em.all())
	}
	ev.Switched(Switch{From: "A", To: "B", Trigger: "limit", Window: "5h", Pool: "work", Episode: "work"})
	ev.NoCandidate("work", "work", "B", false)
	ev.NoCandidate("personal", "personal", "C", false)
	got := em.all()
	if len(got) != 5 || got[3][0] != "chottag: work: switched to B" || got[4][0] != "chottag: work: no account to switch to" {
		t.Fatalf("notices = %q; a work switch ends work's episode only", got)
	}
}

func TestPoolNamedSwitchAndMovedNotices(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.Switched(Switch{From: "A", To: "C", Trigger: "limit", Window: "5h", Pool: "work"})
	ev.Moved(Moved{From: "B", To: []string{"C"}, Sessions: 2, Window: "5h", Pool: "work, personal"})
	ev.Moved(Moved{From: "B", Sessions: 1, Window: "5h", Pool: "work, personal"})
	got := em.all()
	want := []string{
		"chottag: work: switched to C",
		"chottag: work, personal: moved 2 sessions from B to C",
		"chottag: work, personal: 1 session on B have no account to move to",
	}
	if len(got) != len(want) {
		t.Fatalf("notices = %q", got)
	}
	for i := range want {
		if got[i][0] != want[i] {
			t.Errorf("title %d = %q, want %q", i, got[i][0], want[i])
		}
	}
}

// The episode is keyed by the pool, not by the title's tag: adding a pool
// (the tag goes from "" to the name) does not start a second episode.
func TestNoCandidateEpisodeSurvivesTheTagChanging(t *testing.T) {
	ev, em, _, _ := newTestEvents()
	ev.NoCandidate("default", "", "A", false)
	ev.NoCandidate("default", "default", "A", false)
	if got := em.all(); len(got) != 1 {
		t.Fatalf("notices = %q, want one", got)
	}
}
