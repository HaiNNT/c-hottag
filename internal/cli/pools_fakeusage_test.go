//go:build chottag_fakeusage

package cli

import (
	"fmt"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// The M8 exit simulation (spec §7): two pools with different remotes,
//   - work: A and B, remote A;
//   - personal: B, C and D, remote C;
//
// B is in both. The auto-switcher runs on an injected clock with injected
// usage and limits; the chooser answers each request. Nothing waits on real
// time.
func TestPoolsExitSimulation(t *testing.T) {
	r := newAutoRig(t)
	r.update(func(st *store.State) {
		mustPools(t, st, func(st *store.State) error {
			for _, p := range []string{"work", "personal"} {
				if err := st.AddPool(p); err != nil {
					return err
				}
			}
			for _, j := range [][2]string{{"A", "work"}, {"B", "work"}, {"B", "personal"}, {"C", "personal"}, {"D", "personal"}} {
				if err := st.JoinPool(j[0], j[1]); err != nil {
					return err
				}
			}
			for _, n := range []string{"A", "B", "C", "D"} {
				if err := st.LeavePool(n, store.DefaultPool); err != nil {
					return err
				}
			}
			for _, p := range []string{"work", "personal"} {
				if err := st.SetPoolServing(p, "B"); err != nil {
					return err
				}
			}
			if err := st.SetPoolRemote("work", "A"); err != nil {
				return err
			}
			return st.SetPoolRemote("personal", "C")
		})
	})

	loadState := func() (store.State, error) { return r.s.Load() }
	toks := fakeTokens2{}
	st0, err := loadState()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range st0.Accounts {
		toks[a.Dir] = "tok-" + a.Name
	}
	mkChooser := func(owner string) *chooser {
		sel := selector.New(selector.Config{State: loadState, Tokens: toks, Owners: fakeOwners{owner: owner}})
		return &chooser{sel: sel, state: loadState, now: func() time.Time { return r.now }, log: newSyncBuf()}
	}
	ch := mkChooser("")
	choose := func(c *chooser, pool, sid string, d router.Decision) string {
		t.Helper()
		acct, tok, _, ok := c.Choose(poolCtx(pool, sid), d, "")
		if !ok {
			t.Fatalf("%s session %s: nothing chose an account", pool, sid)
		}
		if tok != "tok-"+acct {
			t.Fatalf("token %q is not %s's", tok, acct)
		}
		return acct
	}
	create := router.Decision{Class: router.Remote, Object: router.KindSession}

	const sessions = 20
	var bPastPoint bool
	for minute := 1; minute <= 90; minute++ {
		r.now = r.now.Add(time.Minute)
		// B's use climbs 2.5% a minute, from 10% to its wall; D is limited
		// at minute 40 and C at minute 60.
		pctB := 10 + 2.5*float64(minute)
		if pctB > 100 {
			pctB = 100
		}
		r.row(busyRow("B", pctB, r.now, 3*time.Hour))
		for _, n := range []string{"A", "C", "D"} {
			r.row(freshRow(n, 10, 10, r.now))
		}
		if minute >= 40 {
			r.row(limitedRow("D", r.now))
		}
		if minute >= 60 {
			r.row(limitedRow("C", r.now))
		}
		r.as.tick(r.now)

		for i := 0; i < sessions; i++ {
			sid := fmt.Sprintf("p%02d", i)
			if got := choose(ch, "personal", sid, servingDecision); !inSet(got, personalMembers...) {
				t.Fatalf("minute %d: personal session %s was served by %s", minute, sid, got)
			}
		}
		if got := choose(ch, "work", "w1", servingDecision); !inSet(got, workMembers...) {
			t.Fatalf("minute %d: a work session was served by %s", minute, got)
		}
		if got := choose(ch, "work", "w1", create); got != "A" {
			t.Fatalf("minute %d: a work create went out as %s, want A", minute, got)
		}
		if got := choose(ch, "personal", "p00", create); got != "C" {
			t.Fatalf("minute %d: a personal create went out as %s, want C", minute, got)
		}
		if pctB >= 96 && minute < 40 {
			bPastPoint = true
			// B's usage drove both pools' decisions: neither still serves
			// from B, and each moved within its own members.
			if w := r.poolServing("work"); w != "A" {
				t.Fatalf("minute %d: work serves %s with B at %.0f%%, want A", minute, w, pctB)
			}
			if p := r.poolServing("personal"); p == "B" || !inSet(p, personalMembers...) {
				t.Fatalf("minute %d: personal serves %s with B at %.0f%%", minute, p, pctB)
			}
		}
		if minute > 40 && minute < 60 {
			// D's limit moved personal to C, within its own members.
			if p := r.poolServing("personal"); p != "C" {
				t.Fatalf("minute %d: personal serves %s with D limited, want C", minute, p)
			}
		}
	}
	if !bPastPoint {
		t.Fatal("B never reached its switch point: the simulation proves nothing")
	}
	// Each switch's notice names its pool.
	titles := map[string]bool{}
	for _, n := range r.notices() {
		titles[n.title] = true
	}
	for _, want := range []string{"chottag: work: switched to A", "chottag: personal: switched to D", "chottag: personal: switched to C"} {
		if !titles[want] {
			t.Errorf("no %q notice among %v", want, titles)
		}
	}
	// An object a work session created is owned by A, and a personal session
	// that looks it up is sent to A: an owner lookup is the one request that
	// crosses a pool, by design.
	if got := choose(mkChooser("A"), "personal", "p01", router.Decision{Class: router.Serving, Object: router.KindSession, ObjectID: "x"}); got != "A" {
		t.Fatalf("an owner lookup of a work object went out as %s, want its owner A", got)
	}
}
