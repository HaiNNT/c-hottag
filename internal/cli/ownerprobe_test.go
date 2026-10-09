package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// probeChooser is a chooser over accts (state order) whose tokens come from
// toks, keyed by slot dir; an account with no entry has no usable login.
func probeChooser(accts []store.Account, toks fakeTokens2) *chooser {
	state := func() (store.State, error) {
		st := store.Default()
		st.Accounts = accts
		return st, nil
	}
	sel := selector.New(selector.Config{State: state, Tokens: toks, Owners: fakeOwners{}})
	return newDaemonChooser(sel, nil, nil, state, nil)
}

func probeCtx(pool string) context.Context {
	return proxy.ContextWithIdentityForTest(context.Background(), proxy.Identity{
		Caller: proxyauth.Caller{SID: "s1", Pool: pool},
	})
}

func names(cs []proxy.Candidate) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Account+"="+c.Token)
	}
	return strings.Join(out, ",")
}

var _ proxy.OwnerProber = (*chooser)(nil)

func TestOwnerCandidatesSkipsTriedAndUnusableInMemberOrder(t *testing.T) {
	c := probeChooser(
		[]store.Account{{Name: "A", Dir: "/s/A"}, {Name: "B", Dir: "/s/B"}, {Name: "C", Dir: "/s/C"}, {Name: "D", Dir: "/s/D"}},
		fakeTokens2{"/s/A": "ta", "/s/B": "tb", "/s/D": "td"}, // C has no login
	)
	got := names(c.OwnerCandidates(context.Background(), "A"))
	if got != "B=tb,D=td" {
		t.Fatalf("candidates = %q, want B=tb,D=td (tried A and unusable C skipped)", got)
	}
}

func TestOwnerCandidatesNeverLeavesTheCallersPool(t *testing.T) {
	st := store.Default()
	if err := st.AddPool("work"); err != nil {
		t.Fatal(err)
	}
	st.Accounts = []store.Account{
		{Name: "A", Dir: "/s/A", PoolList: []string{"work"}},
		{Name: "B", Dir: "/s/B", PoolList: []string{"work"}},
		{Name: "X", Dir: "/s/X"}, // default pool only
	}
	state := func() (store.State, error) { return st, nil }
	toks := fakeTokens2{"/s/A": "ta", "/s/B": "tb", "/s/X": "tx"}
	sel := selector.New(selector.Config{State: state, Tokens: toks, Owners: fakeOwners{}})
	c := newDaemonChooser(sel, nil, nil, state, nil)
	if got := names(c.OwnerCandidates(probeCtx("work"), "A")); got != "B=tb" {
		t.Fatalf("work candidates = %q, want B=tb only", got)
	}
	if got := names(c.OwnerCandidates(probeCtx("default"), "Z")); got != "X=tx" {
		t.Fatalf("default candidates = %q, want X=tx only", got)
	}
}

func TestOwnerCandidatesCapsAtMaxOwnerProbes(t *testing.T) {
	var accts []store.Account
	toks := fakeTokens2{}
	for i := 0; i < proxy.MaxOwnerProbes+4; i++ {
		n := fmt.Sprintf("N%02d", i)
		accts = append(accts, store.Account{Name: n, Dir: "/s/" + n})
		toks["/s/"+n] = "t" + n
	}
	got := probeChooser(accts, toks).OwnerCandidates(context.Background(), "none")
	if len(got) != proxy.MaxOwnerProbes || got[0].Account != "N00" {
		t.Fatalf("got %d candidates (first %v), want %d from N00", len(got), got, proxy.MaxOwnerProbes)
	}
}

func TestOwnerCandidatesStateErrorYieldsNone(t *testing.T) {
	state := func() (store.State, error) { return store.State{}, fmt.Errorf("boom") }
	sel := selector.New(selector.Config{State: state, Tokens: fakeTokens2{}, Owners: fakeOwners{}})
	c := newDaemonChooser(sel, nil, nil, state, nil)
	if got := c.OwnerCandidates(context.Background(), "A"); len(got) != 0 {
		t.Fatalf("candidates = %v, want none", got)
	}
}

// R168: the unknown-owner handler logs the hashed id (never the raw one) on
// one line and posts one notice per account per hour.
func TestUnknownOwnerHookLogsAndNotifiesOnce(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A", "B"), n)
	defer dn.Close()
	var log bytes.Buffer
	hook := unknownOwnerHook(&log, dn)
	h := tracelog.HashID("frame-123")
	hook("A", "artifact", h, 404)
	hook("A", "artifact", h, 403)
	want := "chottag: A refused artifact " + h + " (404); chottag doesn't know its owner\n" +
		"chottag: A refused artifact " + h + " (403); chottag doesn't know its owner\n"
	if log.String() != want {
		t.Fatalf("log = %q\nwant %q", log.String(), want)
	}
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: an artifact's owner is unknown" ||
		got[0].body != "A could not open it, and chottag doesn't know which account made it. This is not route drift." {
		t.Fatalf("notices = %+v, want exactly one unknown-owner notice", got)
	}
}

func TestUnknownOwnerHookCleansControlCharsAndToleratesNoNotifier(t *testing.T) {
	var log bytes.Buffer
	unknownOwnerHook(&log, nil)("A", "artifact\nchottag: forged", "h", 404)
	if strings.Count(log.String(), "\n") != 1 {
		t.Fatalf("log = %q, want one line", log.String())
	}
}

func TestOwnerFoundHookLogsOneLine(t *testing.T) {
	var log bytes.Buffer
	ownerFoundHook(&log)("artifact", "abc123", "B", "A", 404)
	want := "chottag: owner of artifact abc123 found: B (A answered 404)\n"
	if log.String() != want {
		t.Fatalf("log = %q, want %q", log.String(), want)
	}
}

// cancelingTokens serves a token and cancels the lookup's context, standing
// in for the overall budget running out while a slow account is awaited.
type cancelingTokens struct {
	fakeTokens2
	cancel context.CancelFunc
}

func (c cancelingTokens) Token(ctx context.Context, dir string) (string, creds.Status, bool) {
	tok, st, ok := c.fakeTokens2.Token(ctx, dir)
	c.cancel()
	return tok, st, ok
}

func TestOwnerCandidatesStopsWhenTheBudgetIsSpent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := func() (store.State, error) {
		st := store.Default()
		st.Accounts = []store.Account{{Name: "B", Dir: "/s/B"}, {Name: "C", Dir: "/s/C"}}
		return st, nil
	}
	toks := cancelingTokens{fakeTokens2{"/s/B": "tb", "/s/C": "tc"}, cancel}
	sel := selector.New(selector.Config{State: state, Tokens: toks, Owners: fakeOwners{}})
	c := newDaemonChooser(sel, nil, nil, state, nil)
	if got := names(c.OwnerCandidates(ctx, "A")); got != "B=tb" {
		t.Fatalf("candidates = %q, want B=tb only (C dropped once the budget is spent)", got)
	}
}
