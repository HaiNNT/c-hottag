package selector_test

import (
	"context"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// poolState is A, B, C, D with pools work (A, B; serving B, remote A) and
// personal (B, C, D; serving C, remote D). Every account also stays in
// default, whose roles are the top-level serving and remote.
func poolState(t *testing.T) store.State {
	t.Helper()
	st := store.Default()
	for _, n := range []string{"A", "B", "C", "D"} {
		if err := st.Add(store.Account{Name: n, Dir: "/slots/" + n}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"work", "personal"} {
		if err := st.AddPool(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range [][2]string{{"A", "work"}, {"B", "work"}, {"B", "personal"}, {"C", "personal"}, {"D", "personal"}} {
		if err := st.JoinPool(j[0], j[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range [][3]string{{"work", "B", "A"}, {"personal", "C", "D"}} {
		if err := st.SetPoolServing(s[0], s[1]); err != nil {
			t.Fatal(err)
		}
		if err := st.SetPoolRemote(s[0], s[2]); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func poolSelector(st store.State, owners fakeOwners) *selector.Selector {
	return selector.New(selector.Config{
		State:  func() (store.State, error) { return st, nil },
		Tokens: fakeTokens{"/slots/A": "tA", "/slots/B": "tB", "/slots/C": "tC", "/slots/D": "tD"},
		Owners: owners,
	})
}

func TestChooseInServesFromThePoolsServingAndRemote(t *testing.T) {
	sel := poolSelector(poolState(t), fakeOwners{})
	serving := router.Decision{Class: router.Serving}
	remote := router.Decision{Class: router.Remote}
	for _, c := range []struct {
		pool string
		d    router.Decision
		want string
	}{
		{"work", serving, "B"}, {"personal", serving, "C"},
		{"work", remote, "A"}, {"personal", remote, "D"},
	} {
		if got := sel.ChooseIn(context.Background(), c.d, "", c.pool, "").Account; got != c.want {
			t.Errorf("pool %s class %s: %q, want %q", c.pool, c.d.Class, got, c.want)
		}
	}
}

func TestChooseInACreateGoesOutAsThePoolsRemote(t *testing.T) {
	sel := poolSelector(poolState(t), fakeOwners{})
	create := router.Decision{Class: router.Remote, Object: router.Kind("artifact")}
	ch := sel.ChooseIn(context.Background(), create, "new-id", "work", "")
	if ch.Account != "A" || ch.Role != selector.RoleRemote {
		t.Fatalf("a work create went out as %+v, want the remote A", ch)
	}
}

func TestChooseInAnOwnerLookupCrossesPools(t *testing.T) {
	owners := fakeOwners{"artifact:obj1": "C"} // C is not in work
	sel := poolSelector(poolState(t), owners)
	d := router.Decision{Class: router.Remote, Object: router.Kind("artifact")}
	ch := sel.ChooseIn(context.Background(), d, "obj1", "work", "")
	if ch.Account != "C" || ch.Role != selector.RoleOwner {
		t.Fatalf("an owned object went out as %+v, want its owner C", ch)
	}
}

func TestChooseInServingOverrideMustBeAMember(t *testing.T) {
	sel := poolSelector(poolState(t), fakeOwners{})
	serving := router.Decision{Class: router.Serving}
	if got := sel.ChooseIn(context.Background(), serving, "", "work", "A").Account; got != "A" {
		t.Fatalf("a member override gave %q, want A", got)
	}
	// C is in personal only: a work session is never served by it, and with
	// more than one pool the request is refused, not sent on Home's login.
	got := sel.ChooseIn(context.Background(), serving, "", "work", "C")
	if got.Account != "" || got.Token != "" || got.Refused == "" {
		t.Fatalf("a non-member override gave %+v, want a refusal", got)
	}
}

func TestChooseInRefusesWhenThePoolCannotServe(t *testing.T) {
	serving := router.Decision{Class: router.Serving}
	remote := router.Decision{Class: router.Remote}
	st := poolState(t)
	// no serving set in work, and personal's serving has no token
	mustSet := func(f func(*store.State)) store.State { c := st; f(&c); return c }
	noServing := mustSet(func(s *store.State) { s.Pools["work"] = store.Pool{Remote: "A"} })
	if got := poolSelector(noServing, fakeOwners{}).ChooseIn(context.Background(), serving, "", "work", ""); got.Refused == "" {
		t.Fatalf("no serving account: %+v, want a refusal", got)
	}
	sel := selector.New(selector.Config{
		State:  func() (store.State, error) { return st, nil },
		Tokens: fakeTokens{"/slots/A": "tA"}, Owners: fakeOwners{},
	})
	if got := sel.ChooseIn(context.Background(), serving, "", "personal", ""); got.Refused == "" {
		t.Fatalf("no token: %+v, want a refusal", got)
	}
	if got := sel.ChooseIn(context.Background(), remote, "", "personal", ""); got.Refused == "" {
		t.Fatalf("remote with no token: %+v, want a refusal", got)
	}
	// default too, once there is more than one pool (Home may be another pool's).
	st.Serving = "B" // no token; st is captured by sel
	if got := sel.ChooseIn(context.Background(), serving, "", "default", ""); got.Refused == "" {
		t.Fatalf("default with no token and several pools: %+v, want a refusal", got)
	}
	// an owner-routed object whose creator has no token: with several pools,
	// Home's login may be another pool's, so it is refused too
	sel2 := selector.New(selector.Config{
		State:  func() (store.State, error) { return st, nil },
		Tokens: fakeTokens{"/slots/A": "tA"}, Owners: fakeOwners{"artifact:o1": "C"},
	})
	d := router.Decision{Class: router.Remote, Object: router.Kind("artifact")}
	if got := sel2.ChooseIn(context.Background(), d, "o1", "work", ""); got.Refused == "" {
		t.Fatalf("an owner with no token went out on Home's login: %+v", got)
	}
}

// R90 in a non-default pool: a serving account with rotation off is not a
// candidate (spec §4).
func TestChooseInANonDefaultPoolsRotationOffServingIsRefused(t *testing.T) {
	st := poolState(t)
	for i := range st.Accounts {
		if st.Accounts[i].Name == "B" {
			st.Accounts[i].NoRotate = true
		}
	}
	if err := st.SetPoolServing("work", "B"); err != nil {
		t.Fatal(err)
	}
	sel := poolSelector(st, fakeOwners{})
	got := sel.ChooseIn(context.Background(), router.Decision{Class: router.Serving}, "", "work", "")
	if got.Account != "" || got.Refused == "" {
		t.Fatalf("a rotation-off serving gave %+v, want a refusal", got)
	}
	// its remote role may be rotation-off (R90's remote-only account)
	st.Accounts[0].NoRotate = true // A, work's remote
	if got := poolSelector(st, fakeOwners{}).ChooseIn(context.Background(), router.Decision{Class: router.Remote}, "", "work", ""); got.Account != "A" {
		t.Fatalf("a rotation-off remote gave %+v, want A", got)
	}
}

// With a single pool, today's passthrough stays.
func TestChooseWithOnlyDefaultStillPassesThrough(t *testing.T) {
	st := testState()
	sel := selector.New(selector.Config{
		State: func() (store.State, error) { return st, nil }, Tokens: fakeTokens{}, Owners: fakeOwners{},
	})
	got := sel.ChooseIn(context.Background(), router.Decision{Class: router.Serving}, "", "default", "")
	if got.Account != "" || got.Refused != "" {
		t.Fatalf("default-only with no token gave %+v, want the plain passthrough", got)
	}
}

func TestChooseInAnUnknownPoolIsDefault(t *testing.T) {
	st := poolState(t)
	st.Serving = "A"
	sel := poolSelector(st, fakeOwners{})
	got := sel.ChooseIn(context.Background(), router.Decision{Class: router.Serving}, "", "gone", "").Account
	want := sel.Choose(context.Background(), router.Decision{Class: router.Serving}, "").Account
	if got != want {
		t.Fatalf("an unknown pool gave %q, Choose gave %q", got, want)
	}
}
