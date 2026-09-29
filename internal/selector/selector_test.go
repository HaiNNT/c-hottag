package selector_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
)

type fakeTokens map[string]string // slot dir -> token; missing = unusable

func (f fakeTokens) Token(_ context.Context, dir string) (string, creds.Status, bool) {
	if tok, ok := f[dir]; ok {
		return tok, creds.Status{State: creds.StateOK}, true
	}
	return "", creds.Status{State: creds.StateNeedsLogin, Reason: "no-login"}, false
}

// awaitCall records one call to fakeAwaiter, and whether its ctx carried a
// deadline (Choose wraps ctx with AwaitTimeout before calling Await).
type awaitCall struct {
	method   string // "Token" | "Await"
	deadline time.Time
	hasDL    bool
}

// fakeAwaiter is fakeTokens plus Awaiter, so a test can tell Choose apart
// calling Token from it calling Await, and inspect the ctx each call got.
type fakeAwaiter struct {
	fakeTokens
	mu    sync.Mutex
	calls []awaitCall
	// awaitOK, when false, makes Await report a stale passthrough — Choose
	// must still emit the same "passthrough" event Token's failure does.
	awaitOK bool
}

func (f *fakeAwaiter) Token(ctx context.Context, dir string) (string, creds.Status, bool) {
	f.record("Token", ctx)
	return f.fakeTokens.Token(ctx, dir)
}

func (f *fakeAwaiter) Await(ctx context.Context, dir string) (string, creds.Status, bool) {
	f.record("Await", ctx)
	if !f.awaitOK {
		return "", creds.Status{State: creds.StateStale}, false
	}
	return f.fakeTokens.Token(ctx, dir)
}

func (f *fakeAwaiter) record(method string, ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dl, hasDL := ctx.Deadline()
	f.calls = append(f.calls, awaitCall{method: method, deadline: dl, hasDL: hasDL})
}

func (f *fakeAwaiter) callsSnapshot() []awaitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]awaitCall(nil), f.calls...)
}

type fakeOwners map[string]string // "kind:id" -> account

func (f fakeOwners) Lookup(kind router.Kind, id string) (string, bool) {
	a, ok := f[string(kind)+":"+id]
	return a, ok
}

func testState() store.State {
	st := store.Default()
	st.Add(store.Account{Name: "B", Dir: "/slots/B"})
	st.Add(store.Account{Name: "C", Dir: "/slots/C"})
	st.Serving, st.Remote = "B", "C"
	return st
}

func newSelector(t fakeTokens, o fakeOwners, events *[]selector.Event) *selector.Selector {
	return selector.New(selector.Config{
		State:  func() (store.State, error) { return testState(), nil },
		Tokens: t,
		Owners: o,
		OnEvent: func(e selector.Event) {
			if events != nil {
				*events = append(*events, e)
			}
		},
	})
}

func TestChooseByClass(t *testing.T) {
	s := newSelector(fakeTokens{"/slots/B": "tok-B", "/slots/C": "tok-C"}, fakeOwners{}, nil)
	ctx := context.Background()
	if got := s.Choose(ctx, router.Decision{Class: router.Serving}, ""); got != (selector.Choice{Account: "B", Token: "tok-B", Role: "serving"}) {
		t.Errorf("serving choice = %+v", got)
	}
	if got := s.Choose(ctx, router.Decision{Class: router.Remote}, ""); got != (selector.Choice{Account: "C", Token: "tok-C", Role: "remote"}) {
		t.Errorf("remote choice = %+v", got)
	}
	if got := s.Choose(ctx, router.Decision{Class: router.Untouched}, ""); got != (selector.Choice{}) {
		t.Errorf("untouched choice = %+v, want empty", got)
	}
}

func TestChooseFollowsTheOwner(t *testing.T) {
	own := fakeOwners{"artifact:slug1": "B", "session:s1": "B"}
	s := newSelector(fakeTokens{"/slots/B": "tok-B", "/slots/C": "tok-C"}, own, nil)
	ctx := context.Background()
	// id from the path
	d := router.Decision{Class: router.Remote, Object: router.KindArtifact, ObjectID: "slug1"}
	if got := s.Choose(ctx, d, ""); got != (selector.Choice{Account: "B", Token: "tok-B", Role: "owner"}) {
		t.Errorf("owner-by-path choice = %+v", got)
	}
	// id from the request body
	d = router.Decision{Class: router.Remote, Object: router.KindArtifact, RequestField: "slug"}
	if got := s.Choose(ctx, d, "slug1"); got.Account != "B" || got.Role != "owner" {
		t.Errorf("owner-by-body choice = %+v", got)
	}
	// unknown object falls back to the class default
	d = router.Decision{Class: router.Remote, Object: router.KindArtifact, ObjectID: "unknown"}
	if got := s.Choose(ctx, d, ""); got.Account != "C" || got.Role != "remote" {
		t.Errorf("unknown-owner choice = %+v, want the remote account", got)
	}
	// a serving-class object route with a known owner still follows the owner
	d = router.Decision{Class: router.Serving, Object: router.KindSession, ObjectID: "s1"}
	if got := s.Choose(ctx, d, ""); got.Account != "B" || got.Role != "owner" {
		t.Errorf("owner over serving = %+v", got)
	}
}

func TestOwnerNoLongerRegisteredFallsBack(t *testing.T) {
	var events []selector.Event
	s := newSelector(fakeTokens{"/slots/C": "tok-C"}, fakeOwners{"artifact:old": "GONE"}, &events)
	d := router.Decision{Class: router.Remote, Object: router.KindArtifact, ObjectID: "old"}
	if got := s.Choose(context.Background(), d, ""); got.Account != "C" || got.Role != "remote" {
		t.Fatalf("choice = %+v, want fallback to the remote account", got)
	}
	if len(events) != 1 || events[0].Kind != "owner-unregistered" || events[0].Account != "GONE" {
		t.Fatalf("events = %+v", events)
	}
}

func TestOwnerNoLongerRegisteredDoesNotMatchAPrefixNeighbour(t *testing.T) {
	// F15, second instance: st.Find resolves a unique NAME PREFIX, which is
	// right for a human typing a name and wrong as an identity check. With
	// the owner "work" removed and "work2" still registered, a Find-based
	// check would treat "work2" as proof that "work" is still registered,
	// skip the owner-unregistered event, and route the object to "work2" as
	// if it were the object's owner rather than the remote-account fallback.
	st := store.Default()
	st.Add(store.Account{Name: "work2", Dir: "/slots/work2"})
	st.Serving, st.Remote = "work2", "work2"
	var events []selector.Event
	s := selector.New(selector.Config{
		State:   func() (store.State, error) { return st, nil },
		Tokens:  fakeTokens{"/slots/work2": "tok-work2"},
		Owners:  fakeOwners{"artifact:old": "work"},
		OnEvent: func(e selector.Event) { events = append(events, e) },
	})
	d := router.Decision{Class: router.Remote, Object: router.KindArtifact, ObjectID: "old"}
	got := s.Choose(context.Background(), d, "")
	if got.Account != "work2" || got.Role != "remote" {
		t.Fatalf("choice = %+v, want fallback to the remote account (work2) with role %q, not a prefix match resolving the removed owner", got, "remote")
	}
	if len(events) != 1 || events[0].Kind != "owner-unregistered" || events[0].Account != "work" {
		t.Fatalf("events = %+v, want a single owner-unregistered event naming %q", events, "work")
	}
}

func TestUnusableTokenPassesThrough(t *testing.T) {
	var events []selector.Event
	s := newSelector(fakeTokens{"/slots/C": "tok-C"}, fakeOwners{}, &events) // B has no login
	if got := s.Choose(context.Background(), router.Decision{Class: router.Serving}, ""); got != (selector.Choice{}) {
		t.Fatalf("choice = %+v, want pass-through", got)
	}
	if len(events) != 1 || events[0].Kind != "passthrough" || events[0].Account != "B" || events[0].Status.State != creds.StateNeedsLogin {
		t.Fatalf("events = %+v", events)
	}
}

func TestNoRoleSetPassesThrough(t *testing.T) {
	s := selector.New(selector.Config{
		State:  func() (store.State, error) { return store.Default(), nil },
		Tokens: fakeTokens{},
		Owners: fakeOwners{},
	})
	if got := s.Choose(context.Background(), router.Decision{Class: router.Serving}, ""); got != (selector.Choice{}) {
		t.Fatalf("choice with no accounts = %+v, want pass-through", got)
	}
}

// TestRemoteRoleAwaitsWithADeadlineNearAwaitTimeout pins F166: a remote
// request waits out an in-flight refresh via Awaiter instead of Token's
// usual stale passthrough, bounded by its own AwaitTimeout — not the
// caller's ctx, which here is context.Background() and has no deadline of
// its own to inherit.
func TestRemoteRoleAwaitsWithADeadlineNearAwaitTimeout(t *testing.T) {
	f := &fakeAwaiter{fakeTokens: fakeTokens{"/slots/C": "tok-C"}, awaitOK: true}
	s := selector.New(selector.Config{
		State:        func() (store.State, error) { return testState(), nil },
		Tokens:       f,
		Owners:       fakeOwners{},
		AwaitTimeout: time.Second,
	})
	before := time.Now()
	got := s.Choose(context.Background(), router.Decision{Class: router.Remote}, "")
	if got != (selector.Choice{Account: "C", Token: "tok-C", Role: "remote"}) {
		t.Fatalf("remote choice = %+v", got)
	}
	calls := f.callsSnapshot()
	if len(calls) != 1 || calls[0].method != "Await" {
		t.Fatalf("calls = %+v, want exactly one Await call", calls)
	}
	if !calls[0].hasDL {
		t.Fatal("Await's ctx had no deadline")
	}
	min, max := before.Add(500*time.Millisecond), before.Add(time.Second).Add(500*time.Millisecond)
	if calls[0].deadline.Before(min) || calls[0].deadline.After(max) {
		t.Fatalf("Await's ctx deadline = %v, want between %v and %v (~AwaitTimeout away)", calls[0].deadline, min, max)
	}
}

// TestOwnerRoleAwaits: an owner-routed object waits the same way a remote
// one does — F166's 403-and-dropped-watch failure was specifically an
// owner-routed artifact request.
func TestOwnerRoleAwaits(t *testing.T) {
	f := &fakeAwaiter{fakeTokens: fakeTokens{"/slots/B": "tok-B", "/slots/C": "tok-C"}, awaitOK: true}
	own := fakeOwners{"artifact:slug1": "B"}
	s := selector.New(selector.Config{
		State:  func() (store.State, error) { return testState(), nil },
		Tokens: f,
		Owners: own,
	})
	d := router.Decision{Class: router.Remote, Object: router.KindArtifact, ObjectID: "slug1"}
	got := s.Choose(context.Background(), d, "")
	if got != (selector.Choice{Account: "B", Token: "tok-B", Role: "owner"}) {
		t.Fatalf("owner choice = %+v", got)
	}
	calls := f.callsSnapshot()
	if len(calls) != 1 || calls[0].method != "Await" {
		t.Fatalf("calls = %+v, want exactly one Await call", calls)
	}
}

// TestServingRoleUsesTokenNotAwait: a serving (inference) request keeps
// Token's instant stale passthrough — F37 is exactly why waiting there was
// removed, and F166 only asked for remote/owner to wait, not serving.
func TestServingRoleUsesTokenNotAwait(t *testing.T) {
	f := &fakeAwaiter{fakeTokens: fakeTokens{"/slots/B": "tok-B"}, awaitOK: true}
	s := selector.New(selector.Config{
		State:  func() (store.State, error) { return testState(), nil },
		Tokens: f,
		Owners: fakeOwners{},
	})
	got := s.Choose(context.Background(), router.Decision{Class: router.Serving}, "")
	if got != (selector.Choice{Account: "B", Token: "tok-B", Role: "serving"}) {
		t.Fatalf("serving choice = %+v", got)
	}
	calls := f.callsSnapshot()
	if len(calls) != 1 || calls[0].method != "Token" {
		t.Fatalf("calls = %+v, want exactly one Token call, never Await", calls)
	}
}

// TestRemoteRoleWithoutAnAwaiterStillUsesToken: a Tokens implementation
// that only has Token (most callers, including every other selector test
// in this file) must keep working unchanged for a remote/owner role —
// Awaiter is optional.
func TestRemoteRoleWithoutAnAwaiterStillUsesToken(t *testing.T) {
	s := newSelector(fakeTokens{"/slots/C": "tok-C"}, fakeOwners{}, nil)
	got := s.Choose(context.Background(), router.Decision{Class: router.Remote}, "")
	if got != (selector.Choice{Account: "C", Token: "tok-C", Role: "remote"}) {
		t.Fatalf("remote choice = %+v", got)
	}
}

// TestAwaitFailureEmitsPassthroughLikeTokenDoes: Await giving up (its
// timeout elapsed, or the refresh failed) must be reported the same way a
// Token failure already is — a "passthrough" event and an empty Choice.
func TestAwaitFailureEmitsPassthroughLikeTokenDoes(t *testing.T) {
	var events []selector.Event
	f := &fakeAwaiter{fakeTokens: fakeTokens{"/slots/C": "tok-C"}, awaitOK: false}
	s := selector.New(selector.Config{
		State:   func() (store.State, error) { return testState(), nil },
		Tokens:  f,
		Owners:  fakeOwners{},
		OnEvent: func(e selector.Event) { events = append(events, e) },
	})
	got := s.Choose(context.Background(), router.Decision{Class: router.Remote}, "")
	if got != (selector.Choice{}) {
		t.Fatalf("choice = %+v, want pass-through", got)
	}
	if len(events) != 1 || events[0].Kind != "passthrough" || events[0].Account != "C" {
		t.Fatalf("events = %+v", events)
	}
}
