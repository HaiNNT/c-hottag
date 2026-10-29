package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// move files a move request for sid to the registered account name, as
// `chottag move` would, at the rig's current time.
func (r *spreadRig) move(sid, name string) {
	r.t.Helper()
	r.moveAt(sid, name, r.now)
}

func (r *spreadRig) moveAt(sid, name string, at time.Time) {
	r.t.Helper()
	r.st.SetMove(sid, store.Move{Account: name, Dir: dirOf(spreadView{st: r.st}, name), At: at})
}

func (r *spreadRig) placementOf(sid string) placement {
	r.t.Helper()
	p, ok := r.eng.snapshot()[sid]
	if !ok {
		r.t.Fatalf("no placement for %s", sid)
	}
	return p
}

func TestMoveAppliedAtNextRequest(t *testing.T) {
	r := newSpreadRig(t)
	if got := r.place("s1"); got != "B" {
		t.Fatal(got)
	}
	r.move("s1", "work")
	if got := r.place("s1"); got != "work" {
		t.Fatalf("after the move s1 = %q, want work", got)
	}
	if p := r.placementOf("s1"); !p.Chosen || p.From != "B" || p.Account != "work" {
		t.Fatalf("placement = %+v", p)
	}
	if acct, chosen, ok := r.eng.placementOf("s1"); acct != "work" || !chosen || !ok {
		t.Fatalf("placementOf = %q %v %v", acct, chosen, ok)
	}
}

func TestMoveCreatesAPlacementForAnUnplacedSession(t *testing.T) {
	r := newSpreadRig(t)
	r.move("s1", "dev1")
	if got := r.place("s1"); got != "dev1" {
		t.Fatalf("s1 = %q, want dev1", got)
	}
	if p := r.placementOf("s1"); !p.Chosen {
		t.Fatalf("placement = %+v", p)
	}
}

func TestMoveAppliedOnce(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	r.place("s1") // B
	r.move("s1", "work")
	if got := r.place("s1"); got != "work" {
		t.Fatal(got)
	}
	// Nowhere to go: the hard branch deletes the placement.
	r.limit("work")
	r.limit("B")
	if _, ok := r.eng.account("s1", r.now); ok {
		t.Fatal("a session with nowhere to go kept a placement")
	}
	if _, _, ok := r.eng.placementOf("s1"); ok {
		t.Fatal("placement not deleted")
	}
	// Both recover, work the worse one: a re-applied move would pick work.
	r.rows["work"] = freshRow("work", 80, 80, r.now)
	r.rows["B"] = freshRow("B", 10, 10, r.now)
	got := r.place("s1")
	if got != "B" {
		t.Fatalf("after recovery s1 = %q: the move re-applied", got)
	}
	if p := r.placementOf("s1"); p.Chosen {
		t.Fatalf("placement chosen by score = %+v", p)
	}
	// A new engine loaded from the same run/ files does not re-apply either.
	r.eng.close()
	again := r.newEngine()
	again.seen["s1"] = r.now
	if got, ok := again.account("s1", r.now); !ok || got != "B" {
		t.Fatalf("after restart s1 = %q, %v: the move re-applied", got, ok)
	}
}

func TestMoveAppliedOnceDeletedPlacementThenNewEngine(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	r.place("s1")
	r.move("s1", "work")
	r.place("s1")
	r.limit("work")
	r.limit("B")
	r.eng.account("s1", r.now) // placement deleted
	r.eng.close()
	r.rows["work"] = freshRow("work", 80, 80, r.now)
	r.rows["B"] = freshRow("B", 10, 10, r.now)
	again := r.newEngine()
	if got, ok := again.account("s1", r.now); !ok || got != "B" {
		t.Fatalf("a restart with a deleted placement re-applied the move: %q, %v", got, ok)
	}
}

func TestMoveNewerRequestApplies(t *testing.T) {
	r := newSpreadRig(t)
	r.place("s1")
	r.move("s1", "work")
	if got := r.place("s1"); got != "work" {
		t.Fatal(got)
	}
	r.moveAt("s1", "dev1", r.now.Add(time.Minute))
	if got := r.place("s1"); got != "dev1" {
		t.Fatalf("a newer request gave %q, want dev1", got)
	}
}

func TestMoveDroppedWhenTargetCannotServe(t *testing.T) {
	cases := map[string]func(r *spreadRig) string{
		"limited":      func(r *spreadRig) string { r.limit("work"); return "work" },
		"needs login":  func(r *spreadRig) string { r.needsLogin("work", true); return "work" },
		"rotation off": func(r *spreadRig) string { return "A" },
		"unregistered": func(r *spreadRig) string {
			r.st.SetMove("s1", store.Move{Account: "ghost", At: r.now})
			return "ghost"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newSpreadRig(t)
			r.place("s1")
			target := setup(r)
			if _, ok := r.st.Moves["s1"]; !ok {
				r.move("s1", target)
			}
			if got := r.place("s1"); got != "B" {
				t.Fatalf("s1 = %q, want to stay on B", got)
			}
			if p := r.placementOf("s1"); p.Chosen {
				t.Fatalf("a dropped move chose the placement: %+v", p)
			}
			lines := strings.Split(strings.TrimSpace(r.log.String()), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], "s1") || !strings.Contains(lines[0], target) {
				t.Fatalf("log = %q, want one line naming s1 and %s", r.log.String(), target)
			}
			r.place("s1")
			r.eng.account("s1", r.now)
			if got := strings.Count(r.log.String(), "\n"); got != 1 {
				t.Fatalf("the dropped request was retried: %q", r.log.String())
			}
		})
	}
	t.Run("not in the session's pool", func(t *testing.T) {
		r := poolSpreadRig(t)
		if _, ok := r.eng.accountFor("s1", "", "work", r.now); !ok {
			t.Fatal("no placement")
		}
		r.move("s1", "C") // C is only in personal
		got, ok := r.eng.accountFor("s1", "", "work", r.now)
		if !ok || (got != "A" && got != "B") {
			t.Fatalf("s1 = %q, %v; want a work member", got, ok)
		}
		lines := strings.Split(strings.TrimSpace(r.log.String()), "\n")
		if len(lines) != 1 || !strings.Contains(lines[0], "session s1 to C dropped") {
			t.Fatalf("log = %q, want one line dropping s1's move to C", r.log.String())
		}
	})
}

func TestMoveForcedAboveSwitchApplies(t *testing.T) {
	r := newSpreadRig(t)
	r.place("s1")
	r.usage("work", 97, 10)
	r.move("s1", "work")
	if got := r.place("s1"); got != "work" {
		t.Fatalf("s1 = %q: a forced move above the switch point must apply", got)
	}
}

func TestMoveResolvesByDirAfterRename(t *testing.T) {
	r := newSpreadRig(t)
	r.place("s1")
	r.move("s1", "work")
	r.st.Accounts[2].Name = "work2"
	r.rows["work2"] = freshRow("work2", 10, 10, r.now)
	delete(r.rows, "work")
	if got := r.place("s1"); got != "work2" {
		t.Fatalf("s1 = %q, want work2", got)
	}
}

func TestChosenStaysAtSwitchPoint(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r.place(sid) // B work dev1 B
	}
	r.move("s4", "B") // already there: becomes chosen
	if got := r.place("s4"); got != "B" {
		t.Fatal(got)
	}
	r.usage("B", 95, 10)
	r.eng.mark("B", "B at its 5h switch point", r.now)
	if got := r.place("s1"); got == "B" {
		t.Fatalf("the unchosen peer stayed on B")
	}
	if got := r.place("s4"); got != "B" {
		t.Fatalf("the chosen session left a switch point for %q", got)
	}
	if p := r.placementOf("s4"); !p.Chosen {
		t.Fatalf("placement = %+v", p)
	}
}

func TestChosenStaysOnColdCache(t *testing.T) {
	r := newSpreadRig(t, "B", "work")
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r.place(sid) // B work B work
	}
	r.move("s3", "B")
	r.place("s3")
	r.usage("B", 60, 10)
	r.convs["s3"] = 1
	if got := r.place("s3"); got != "B" {
		t.Fatalf("the chosen cold session moved to %q", got)
	}
	// The same conditions move an unchosen peer (so the test bites). It is
	// checked on a twin rig, as s3's request above leaves the loads as they were.
	r2 := newSpreadRig(t, "B", "work")
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r2.place(sid)
	}
	r2.usage("B", 60, 10)
	r2.convs["s3"] = 1
	if got := r2.place("s3"); got != "work" {
		t.Fatalf("the unchosen cold session stayed on %q (the test does not bite)", got)
	}
}

func TestChosenLeavesOnHardTriggerAndClears(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	r.place("s1") // B
	r.move("s1", "work")
	r.place("s1")
	r.limit("work")
	got := r.place("s1")
	if got == "work" {
		t.Fatalf("a chosen session stayed on a limited account")
	}
	if p := r.placementOf("s1"); p.Chosen || p.Account != got {
		t.Fatalf("placement = %+v", p)
	}

	r2 := newSpreadRig(t, "B", "work", "dev1")
	r2.place("s1")
	r2.move("s1", "work")
	r2.place("s1")
	to, ok := r2.eng.replace("s1", r2.now)
	if !ok || to == "work" {
		t.Fatalf("replace = %q, %v", to, ok)
	}
	if p := r2.placementOf("s1"); p.Chosen {
		t.Fatalf("replaceIn kept Chosen: %+v", p)
	}
}

func TestMarkNoticeCountsChosen(t *testing.T) {
	r := newSpreadRig(t, "B", "work", "dev1")
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		r.place(sid) // B work dev1 B
	}
	r.move("s4", "B")
	r.place("s4")
	r.usage("B", 95, 10)
	if res := r.eng.mark("B", "B at its 5h switch point", r.now); res.Sessions != 1 {
		t.Fatalf("mark counted %d sessions, want 1 (the chosen one stays)", res.Sessions)
	}
	// A limit is a hard trigger: the chosen session leaves too.
	r.limit("B")
	if res := r.eng.mark("B", "B is limited", r.now); res.Sessions != 2 {
		t.Fatalf("mark on a limited account counted %d sessions, want 2", res.Sessions)
	}
}

func TestStampSessionsOverlaysPlacement(t *testing.T) {
	home := t.TempDir()
	writeLiveEntry(t, home, sidOne)
	tr := sessions.NewTracker()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tr.Seen(sidOne, "default", "A", true, "", now)
	tr.Seen(sidTwo, "default", "B", true, "", now)
	sink := newTestSink(t, home)
	place := func(sid, _ string) (string, bool, bool) {
		if sid == sidOne {
			return "B", true, true
		}
		return "", false, false
	}
	got := stampSessions(sink, tr, home, now, place)
	by := map[string]sessions.Activity{}
	for _, a := range got {
		by[a.SID] = a
	}
	if a := by[sidOne]; a.Placed != "B" || !a.Chosen {
		t.Fatalf("sidOne = %+v", a)
	}
	if a := by[sidTwo]; a.Placed != "" || a.Chosen {
		t.Fatalf("sidTwo = %+v", a)
	}
	for _, a := range stampSessions(sink, tr, home, now, nil) {
		if a.Placed != "" || a.Chosen {
			t.Fatalf("nil place left %+v", a)
		}
	}
}

func TestPlacementsPublishedOnlyForSpreadPools(t *testing.T) {
	st := store.State{}
	for _, n := range []string{"A", "B"} {
		if err := st.Add(store.Account{Name: n, Dir: "/Users/alice/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddPool("work"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPoolPolicy("work", store.PolicySpread); err != nil {
		t.Fatal(err)
	}
	place := func(string) (string, bool, bool) { return "A", true, true }
	view := spreadOnly(st, place)
	if acct, chosen, ok := view("s1", "work"); !ok || acct != "A" || !chosen {
		t.Fatalf("spread pool: %q %v %v", acct, chosen, ok)
	}
	if _, _, ok := view("s2", "default"); ok {
		t.Fatal("a serial pool's stale placement was published")
	}
	if _, _, ok := view("s3", ""); ok {
		t.Fatal("a session with no pool (default, serial) was published")
	}
	if _, _, ok := view("s4", "gone"); ok {
		t.Fatal("a removed pool routes as default, which is serial")
	}
}

func TestMoveAppliesAnEarlierRequestAfterALaterOne(t *testing.T) {
	r := newSpreadRig(t)
	r.place("s1")
	r.moveAt("s1", "work", r.now.Add(time.Hour))
	if got := r.place("s1"); got != "work" {
		t.Fatal(got)
	}
	// The clock stepped back: the rewritten request is older but is new.
	r.moveAt("s1", "dev1", r.now.Add(-time.Hour))
	if got := r.place("s1"); got != "dev1" {
		t.Fatalf("an earlier-stamped new request gave %q, want dev1", got)
	}
}
