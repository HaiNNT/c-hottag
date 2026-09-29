package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// wiring is the real selector/owners/tokens stack behind proxy.Chooser. It
// also counts Choose calls, so a test can assert not just what bearer went
// out but whether the selector was consulted at all — the only way to tell
// "the router correctly short-circuited before Choose" apart from "the
// router misclassified the route, but Choose happened to decline anyway".
type wiring struct {
	sel *selector.Selector
	own *owners.Map

	mu    sync.Mutex
	calls int
}

func (w *wiring) Choose(ctx context.Context, d router.Decision, bodyID string) (string, string, bool, bool) {
	w.mu.Lock()
	w.calls++
	w.mu.Unlock()
	c := w.sel.Choose(ctx, d, bodyID)
	if c.Account == "" {
		return "", "", false, false
	}
	return c.Account, c.Token, c.Role == selector.RoleOwner, true
}

// callCount returns how many times Choose has been invoked so far.
func (w *wiring) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func (w *wiring) Record(kind router.Kind, ids []string, account string) {
	w.own.Record(kind, ids, account, time.Now())
}

func (w *wiring) Refresh(context.Context, string) (string, bool) { return "", false }

func setupWiring(t *testing.T) (*wiring, store.Store) {
	t.Helper()
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	slot := func(n string) string {
		d, err := s.SlotDir(n)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "B", Dir: slot("B")}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "C", Dir: slot("C")}); err != nil {
			return err
		}
		st.Serving, st.Remote = "B", "C"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(own.Close)
	exp := time.Now().Add(time.Hour)
	read := func(slotDir string) (creds.Token, error) {
		switch filepath.Base(slotDir) {
		case "B":
			return creds.Token{AccessToken: "tok-B", ExpiresAt: exp}, nil
		case "C":
			return creds.Token{AccessToken: "tok-C", ExpiresAt: exp}, nil
		}
		return creds.Token{}, creds.ErrNoLogin
	}
	tm := tokens.New(tokens.Config{
		Read:     read,
		Refresh:  noRefresh{},
		LockPath: func(d string) string { return filepath.Join(d, ".lock") },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		ReadTTL:  time.Millisecond, // the test changes state between requests
	})
	cache := store.NewCache(s)
	sel := selector.New(selector.Config{State: cache.State, Tokens: tm, Owners: own})
	return &wiring{sel: sel, own: own}, s
}

type noRefresh struct{}

func (noRefresh) Refresh(context.Context, string) error { return nil }

// upstreamEcho answers every request with JSON and reports the bearer it saw.
func upstreamEcho(seen chan<- string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/code/sessions":
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_9"}})
		case "/api/frame/deploy/prepare":
			json.NewEncoder(w).Encode(map[string]any{"slug": "slug9"})
		default:
			w.Write([]byte(`{}`))
		}
	})
}

// TestEndToEndRouting wires the real store/owners/tokens/selector stack in
// front of proxytest's fake upstream and proves the pieces route requests
// together, not merely in isolation: a serving/remote request goes out as
// the right account, an object follows its creator even after `remote`
// moves elsewhere, a republish whose id lives only in the request body still
// finds its creator, a live `chottag tag` (a bare store.Update) changes the
// very next request with no restart, a slot with no login passes the
// client's own bearer through unchanged, and an untouched route is never
// even asked about.
func TestEndToEndRouting(t *testing.T) {
	w, s := setupWiring(t)
	seen := make(chan string, 1)
	h := proxytest.Start(t, upstreamEcho(seen), proxytest.Options{Choose: w})

	call := func(method, url, body string) string {
		t.Helper()
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return <-seen
	}
	const api = "https://api.anthropic.com"

	// 1. class routing
	if got := call("POST", api+"/v1/messages", `{}`); got != "Bearer tok-B" {
		t.Errorf("serving route went out as %q, want tok-B", got)
	}
	if got := call("POST", api+"/v1/code/sessions", `{}`); got != "Bearer tok-C" {
		t.Errorf("remote route went out as %q, want tok-C", got)
	}
	// 2. the session created above follows its creator after remote changes
	if _, err := s.Update(func(st *store.State) error { st.Remote = "B"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", api+"/v1/code/sessions/cse_9/archive", `{}`); got != "Bearer tok-C" {
		t.Errorf("archive of C's session went out as %q, want tok-C", got)
	}
	if got := call("POST", api+"/v1/code/sessions", `{}`); got != "Bearer tok-B" {
		t.Errorf("new session after `remote B` went out as %q, want tok-B", got)
	}
	// 3. artifact republish carries its slug in the body
	if _, err := s.Update(func(st *store.State) error { st.Remote = "C"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", api+"/api/frame/deploy/prepare", `{}`); got != "Bearer tok-C" {
		t.Errorf("artifact create went out as %q, want tok-C", got)
	}
	if _, err := s.Update(func(st *store.State) error { st.Remote = "B"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", api+"/api/frame/deploy/direct", `{"slug":"slug9","content":"x"}`); got != "Bearer tok-C" {
		t.Errorf("republish of C's artifact went out as %q, want tok-C", got)
	}
	// 4. a live `tag` moves the next serving request
	if _, err := s.Update(func(st *store.State) error { st.Serving = "C"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", api+"/v1/messages", `{}`); got != "Bearer tok-C" {
		t.Errorf("after `tag C` the serving route went out as %q, want tok-C", got)
	}
	// 5. untouched routes are never swapped, proven two ways: Serving still
	// names "C", which has a valid, distinct token, so a regression that let
	// this route fall through to the Serving class (instead of being
	// short-circuited as Untouched) would swap in "Bearer tok-C" here — a
	// visibly different bearer, not a coincidental pass-through — and the
	// Choose call count would move. Ordered before scenario 6 deliberately:
	// once "A" (no login) becomes Serving, a misrouted Untouched request
	// would pass through unswapped for the wrong reason and this check would
	// no longer be able to tell the two failure modes apart.
	before := w.callCount()
	if got := call("POST", api+"/v1/code/sessions/cse_9/worker/events", `{}`); got != "Bearer sk-ant-oat01-home" {
		t.Errorf("untouched route went out as %q", got)
	}
	if after := w.callCount(); after != before {
		t.Errorf("untouched route consulted the selector: Choose call count %d -> %d, want unchanged", before, after)
	}
	// 6. an account with no login passes through. The Choose call count
	// moving proves the selector was actually consulted and declined (found
	// no usable token) rather than this request being skipped by some
	// unrelated misclassification that happens to produce the same
	// passed-through bearer.
	if _, err := s.Update(func(st *store.State) error {
		st.Accounts = append(st.Accounts, store.Account{Name: "A", Dir: filepath.Join(s.Dir, "accounts", "A")})
		st.Serving = "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before = w.callCount()
	if got := call("POST", api+"/v1/messages", `{}`); got != "Bearer sk-ant-oat01-home" {
		t.Errorf("account with no login: request went out as %q, want the client's own bearer", got)
	}
	if after := w.callCount(); after == before {
		t.Errorf("no-login route never consulted the selector: Choose call count stayed at %d", before)
	}
}
