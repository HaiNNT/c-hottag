package selector_test

import (
	"context"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// R147: a remote or owner request whose account has no usable token is
// refused, never left to go out on Home's login, even with one pool.
func TestRemoteWithAnUnusableTokenIsRefusedNotPassedThrough(t *testing.T) {
	var events []selector.Event
	s := newSelector(fakeTokens{"/slots/B": "tok-B"}, fakeOwners{}, &events) // C (remote) has no login
	got := s.Choose(context.Background(), router.Decision{Class: router.Remote}, "")
	if got.Account != "" || got.Refused == "" || got.RefusedAccount != "C" || got.RefusedRole != selector.RoleRemote {
		t.Fatalf("choice = %+v, want a refusal naming C as the remote account", got)
	}
	if len(events) != 1 || events[0].Kind != "passthrough" || events[0].Account != "C" || events[0].Role != selector.RoleRemote || !events[0].Refused || events[0].Status.State != creds.StateNeedsLogin {
		t.Fatalf("events = %+v", events)
	}
}

func TestOwnerWithAnUnusableTokenIsRefusedNotPassedThrough(t *testing.T) {
	s := newSelector(fakeTokens{"/slots/C": "tok-C"}, fakeOwners{"artifact:slug1": "B"}, nil) // owner B has no login
	d := router.Decision{Class: router.Remote, Object: router.KindArtifact, ObjectID: "slug1"}
	got := s.Choose(context.Background(), d, "")
	if got.Account != "" || got.Refused == "" || got.RefusedAccount != "B" || got.RefusedRole != selector.RoleOwner {
		t.Fatalf("choice = %+v, want a refusal naming owner B", got)
	}
	// An owner lookup on a serving-class route refuses the same way.
	d = router.Decision{Class: router.Serving, Object: router.KindSession, ObjectID: "s1"}
	s = newSelector(fakeTokens{"/slots/C": "tok-C"}, fakeOwners{"session:s1": "B"}, nil)
	if got := s.Choose(context.Background(), d, ""); got.RefusedRole != selector.RoleOwner || got.Account != "" {
		t.Fatalf("serving-class owner choice = %+v, want a refusal", got)
	}
}

// Serving is unchanged: with a single pool it still goes out unchanged.
func TestServingWithAnUnusableTokenStillPassesThrough(t *testing.T) {
	var events []selector.Event
	s := newSelector(fakeTokens{"/slots/C": "tok-C"}, fakeOwners{}, &events)
	if got := s.Choose(context.Background(), router.Decision{Class: router.Serving}, ""); got != (selector.Choice{}) {
		t.Fatalf("choice = %+v, want the zero Choice", got)
	}
	if len(events) != 1 || events[0].Refused || events[0].Role != selector.RoleServing {
		t.Fatalf("events = %+v", events)
	}
}

// M1: with several pools a serving request with an unusable token is refused,
// not sent on Home's login, and its event says so.
func TestGuardedServingRefusalEventIsMarkedRefused(t *testing.T) {
	st := poolState(t)
	var events []selector.Event
	sel := selector.New(selector.Config{
		State:   func() (store.State, error) { return st, nil },
		Tokens:  fakeTokens{"/slots/A": "tA"},
		Owners:  fakeOwners{},
		OnEvent: func(e selector.Event) { events = append(events, e) },
	})
	got := sel.ChooseIn(context.Background(), router.Decision{Class: router.Serving}, "", "personal", "")
	if got.Refused == "" {
		t.Fatalf("choice = %+v, want a refusal", got)
	}
	if len(events) != 1 || !events[0].Refused || events[0].Role != selector.RoleServing {
		t.Fatalf("events = %+v, want a refused serving event", events)
	}
}

// The wait for a refresh is bounded by 15 seconds unless configured.
func TestDefaultAwaitTimeoutIs15Seconds(t *testing.T) {
	if selector.DefaultAwaitTimeout != 15*time.Second {
		t.Fatalf("DefaultAwaitTimeout = %v, want 15s", selector.DefaultAwaitTimeout)
	}
	f := &fakeAwaiter{fakeTokens: fakeTokens{"/slots/C": "tok-C"}, awaitOK: true}
	s := selector.New(selector.Config{State: func() (store.State, error) { return testState(), nil }, Tokens: f, Owners: fakeOwners{}})
	before := time.Now()
	s.Choose(context.Background(), router.Decision{Class: router.Remote}, "")
	calls := f.callsSnapshot()
	if len(calls) != 1 || !calls[0].hasDL || calls[0].deadline.After(before.Add(16*time.Second)) || calls[0].deadline.Before(before.Add(14*time.Second)) {
		t.Fatalf("calls = %+v, want one Await with a ~15s deadline", calls)
	}
}
