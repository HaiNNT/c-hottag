package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// fakeTokens implements selector.Tokens with a fixed answer, so the account
// name and the token value below are deliberately distinct strings: a
// Choose that swapped them (returning ch.Token, ch.Account instead of
// ch.Account, ch.Token) would be caught by comparing against the wrong one.
type fakeTokens struct {
	tok string
	st  creds.Status
	ok  bool
}

func (f fakeTokens) Token(context.Context, string) (string, creds.Status, bool) {
	return f.tok, f.st, f.ok
}

type fakeOwners struct{ owner string }

func (f fakeOwners) Lookup(router.Kind, string) (string, bool) {
	if f.owner == "" {
		return "", false
	}
	return f.owner, true
}

// TestChooserChooseReturnsAccountThenToken pins the adapter's return order:
// (account, token, ok), not (token, account, ok). A swap here would still
// compile (both are strings) and would slip a token into requests as if it
// were an account name.
func TestChooserChooseReturnsAccountThenToken(t *testing.T) {
	sel := selector.New(selector.Config{
		State: func() (store.State, error) {
			return store.State{Serving: "X", Accounts: []store.Account{{Name: "X", Dir: "/slots/X"}}}, nil
		},
		Tokens: fakeTokens{tok: "tok-secret", st: creds.Status{State: creds.StateOK}, ok: true},
		Owners: fakeOwners{},
	})
	c := &chooser{sel: sel}

	account, token, _, ok := c.Choose(context.Background(), router.Decision{Class: router.Serving}, "")
	if !ok {
		t.Fatalf("Choose ok = false, want true")
	}
	if account != "X" {
		t.Fatalf("account = %q, want %q (got the token in the account slot?)", account, "X")
	}
	if token != "tok-secret" {
		t.Fatalf("token = %q, want %q (got the account in the token slot?)", token, "tok-secret")
	}
}

// TestChooserChooseReportsOwnerRole pins the wiring F241/R96 depends on:
// Choose's owner return value must say whether selector.Choice.Role was
// "owner" (the object's recorded creator), not "remote" or "serving" —
// forward.go uses it to decide whether the safety net may pass a
// connector's own refusal straight through.
func TestChooserChooseReportsOwnerRole(t *testing.T) {
	sel := selector.New(selector.Config{
		State: func() (store.State, error) {
			return store.State{
				Remote:   "R",
				Accounts: []store.Account{{Name: "R", Dir: "/slots/R"}, {Name: "creator", Dir: "/slots/creator"}},
			}, nil
		},
		Tokens: fakeTokens{tok: "tok-secret", st: creds.Status{State: creds.StateOK}, ok: true},
		Owners: fakeOwners{owner: "creator"},
	})
	c := &chooser{sel: sel}

	account, _, owner, ok := c.Choose(context.Background(), router.Decision{Class: router.Remote, Object: router.KindConnector, ObjectID: "conn-1"}, "")
	if !ok || account != "creator" || !owner {
		t.Fatalf("Choose = %q, owner=%v, ok=%v; want (\"creator\", true, true) for a recorded owner", account, owner, ok)
	}

	// No Object named: selector.owner never even asks the owner map, so
	// this falls to the class fallback (the remote pin, "R"), and owner
	// must report false even though fakeOwners would answer "creator" for
	// any id it were asked about.
	account, _, owner, ok = c.Choose(context.Background(), router.Decision{Class: router.Remote}, "")
	if !ok || account != "R" || owner {
		t.Fatalf("Choose = %q, owner=%v, ok=%v; want (\"R\", false, true) for the remote-pin fallback", account, owner, ok)
	}

	// The owner map does not know this connector id at all (Lookup fails):
	// falls to the remote pin, same as no Object at all.
	unmapped := &chooser{sel: selector.New(selector.Config{
		State: func() (store.State, error) {
			return store.State{
				Remote:   "R",
				Accounts: []store.Account{{Name: "R", Dir: "/slots/R"}},
			}, nil
		},
		Tokens: fakeTokens{tok: "tok-secret", st: creds.Status{State: creds.StateOK}, ok: true},
		Owners: fakeOwners{},
	})}
	account, _, owner, ok = unmapped.Choose(context.Background(), router.Decision{Class: router.Remote, Object: router.KindConnector, ObjectID: "conn-x"}, "")
	if !ok || account != "R" || owner {
		t.Fatalf("Choose = %q, owner=%v, ok=%v; want (\"R\", false, true) for an unrecorded connector id", account, owner, ok)
	}

	// The owner map names an account, but it's since been removed from
	// state.json: falls to the remote pin too (selector's "creator was
	// logged out" branch), and owner must still report false — it must
	// never say the fallback account was the recorded owner.
	deregistered := &chooser{sel: selector.New(selector.Config{
		State: func() (store.State, error) {
			return store.State{
				Remote:   "R",
				Accounts: []store.Account{{Name: "R", Dir: "/slots/R"}},
			}, nil
		},
		Tokens: fakeTokens{tok: "tok-secret", st: creds.Status{State: creds.StateOK}, ok: true},
		Owners: fakeOwners{owner: "removed-creator"},
	})}
	account, _, owner, ok = deregistered.Choose(context.Background(), router.Decision{Class: router.Remote, Object: router.KindConnector, ObjectID: "conn-1"}, "")
	if !ok || account != "R" || owner {
		t.Fatalf("Choose = %q, owner=%v, ok=%v; want (\"R\", false, true) for a recorded owner no longer registered", account, owner, ok)
	}
}

// TestChooserChooseClearsNotifyBlockedForTheAccount drives the real
// *chooser newDaemonChooser builds — the same constructor runProxyWithSignal
// calls, wired with `dn` — through Choose: a successful Choose must clear
// the account's blocked flag, or the usage hook's needs-login re-arm leaks
// forever (M2b T5, T1 review #5). Deleting `notify: dn` inside
// newDaemonChooser or the `c.notify.beginChoose(...)` call in Choose fails
// this test.
func TestChooserChooseClearsNotifyBlockedForTheAccount(t *testing.T) {
	state := func() (store.State, error) {
		return store.State{Serving: "X", Accounts: []store.Account{{Name: "X", Dir: "/slots/X"}}}, nil
	}
	sel := selector.New(selector.Config{
		State:  state,
		Tokens: fakeTokens{tok: "tok-secret", st: creds.Status{State: creds.StateOK}, ok: true},
		Owners: fakeOwners{},
	})
	n := newRecordingNotifier()
	dn := newDaemonNotify(func() (store.State, error) { return store.State{}, nil }, n)
	defer dn.Close()
	dn.markStale("X")
	c := newDaemonChooser(sel, nil, nil, state, dn)

	if _, _, _, ok := c.Choose(context.Background(), router.Decision{Class: router.Serving}, ""); !ok {
		t.Fatalf("Choose ok = false, want true")
	}
	if dn.isBlocked("X") {
		t.Fatal("Choose left X blocked: newDaemonChooser's notify wiring or the beginChoose call in Choose is missing")
	}
}

// TestChooserChooseDeclinesOnAnEmptyChoice pins ok=false for an untouched
// decision: dropping the ok=false branch (or inverting it) would silently
// swap requests onto an empty account/token pair.
func TestChooserChooseDeclinesOnAnEmptyChoice(t *testing.T) {
	sel := selector.New(selector.Config{
		State:  func() (store.State, error) { return store.State{}, nil },
		Tokens: fakeTokens{},
		Owners: fakeOwners{},
	})
	c := &chooser{sel: sel}

	account, token, _, ok := c.Choose(context.Background(), router.Decision{Class: router.Untouched}, "")
	if ok || account != "" || token != "" {
		t.Fatalf("Choose = %q, %q, %v; want empty and ok=false for an untouched decision", account, token, ok)
	}
}

// TestChooserRefreshForcesARealRefresh pins the fix for the safety net
// calling Refresh after a swapped request was refused: it must run the
// slot's Refresher even though the cached token still assesses fine, not
// hand back the same cache hit that just failed upstream.
func TestChooserRefreshForcesARealRefresh(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	refreshes := 0
	tm := tokens.New(tokens.Config{
		Read: func(string) (creds.Token, error) {
			return creds.Token{AccessToken: "cached", ExpiresAt: now.Add(time.Hour)}, nil
		},
		Refresh: refreshFunc(func(context.Context, string) error {
			refreshes++
			return nil
		}),
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		Now:      func() time.Time { return now },
	})
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "X", Dir: "/slots/X"}}}, nil
	}
	c := &chooser{tm: tm, state: state}

	tok, ok := c.Refresh(context.Background(), "X")
	if !ok || tok != "cached" {
		t.Fatalf("Refresh = %q, %v", tok, ok)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 (Refresh must force a real refresh, not reuse the cache)", refreshes)
	}
}

// TestChooserRefreshUnknownAccountDeclines: an account no longer in state
// (e.g. removed between the swap and the refusal) must decline, not panic
// or block.
func TestChooserRefreshUnknownAccountDeclines(t *testing.T) {
	tm := tokens.New(tokens.Config{
		Read:     func(string) (creds.Token, error) { return creds.Token{}, nil },
		Refresh:  refreshFunc(func(context.Context, string) error { return nil }),
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
	})
	c := &chooser{tm: tm, state: func() (store.State, error) { return store.State{}, nil }}
	if _, ok := c.Refresh(context.Background(), "nope"); ok {
		t.Fatal("Refresh reported ok for an account no longer in state")
	}
}

// TestChooserRefreshRemovedAccountDoesNotPrefixMatchASurvivor guards a
// credential-boundary crossing: st.Find's prefix fallback, used as an
// identity check, would resolve a just-removed account's name to an
// unrelated SURVIVING account that happens to share a name prefix (e.g.
// "work" removed, "work2" still registered) — refreshing that survivor's
// slot (running the real claude inside ITS config dir) and handing back
// ITS bearer for a request the client believes carries the removed
// account's identity. Refresh must decline instead, and must never touch
// the survivor's slot at all.
func TestChooserRefreshRemovedAccountDoesNotPrefixMatchASurvivor(t *testing.T) {
	var touched []string
	tm := tokens.New(tokens.Config{
		Read: func(string) (creds.Token, error) {
			return creds.Token{AccessToken: "work2-secret", ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
		Refresh: refreshFunc(func(_ context.Context, slotDir string) error {
			touched = append(touched, slotDir)
			return nil
		}),
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
	})
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "work2", Dir: "/slots/work2"}}}, nil
	}
	c := &chooser{tm: tm, state: state}

	tok, ok := c.Refresh(context.Background(), "work")
	if ok || tok != "" {
		t.Fatalf("Refresh(%q) = %q, %v; want \"\", false — work was removed, work2 must never stand in for it", "work", tok, ok)
	}
	if len(touched) != 0 {
		t.Fatalf("touched slots = %v, want none: work2's slot must never be refreshed to answer for a removed account", touched)
	}
}

// A Record that lands after the owners map is closed is dropped by design
// (F85). What must NOT happen is that it is dropped invisibly.
func TestChooserCountsOwnerWriteDropsAfterClose(t *testing.T) {
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	ch := &chooser{own: own}

	ch.Record(router.KindArtifact, []string{"before"}, "A")
	if got := ch.OwnerWriteDrops(); got != 0 {
		t.Fatalf("OwnerWriteDrops() = %d before any close, want 0", got)
	}

	own.Close()
	// Two distinct ids, not one repeated: a mutation replacing Add(1) with
	// Store(1) passes a single post-close Record (both leave the counter at
	// 1), so this drives a second one and requires 2 — Item 7, fix round 1.
	ch.Record(router.KindArtifact, []string{"after-1"}, "A")
	if got := ch.OwnerWriteDrops(); got != 1 {
		t.Fatalf("OwnerWriteDrops() = %d after one post-close Record, want 1", got)
	}
	ch.Record(router.KindArtifact, []string{"after-2"}, "A")
	if got := ch.OwnerWriteDrops(); got != 2 {
		t.Fatalf("OwnerWriteDrops() = %d after two post-close Records, want 2", got)
	}
}

type refreshFunc func(ctx context.Context, slotDir string) error

func (f refreshFunc) Refresh(ctx context.Context, slotDir string) error { return f(ctx, slotDir) }
