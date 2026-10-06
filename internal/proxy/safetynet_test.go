package proxy_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// refusingChooser swaps every serving request and can refresh once.
type refusingChooser struct {
	mu        sync.Mutex
	refreshes int
}

func (c *refusingChooser) Choose(context.Context, router.Decision, string) (string, string, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return "C", "tok-stale", false, true
}

func (c *refusingChooser) Record(router.Kind, []string, string) {}

func (c *refusingChooser) Refresh(_ context.Context, account string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshes++
	return "tok-fresh", true
}

var _ proxy.Chooser = (*refusingChooser)(nil)

// ownerMappedChooser reports owner=true whenever the decision names an
// object (Object != ""), standing in for a selector.Choice{Role: "owner"}
// (F241/R96): the object's recorded creator, not the remote pin or the
// serving account. It never has anything to refresh (a real chooser only
// asks for a refresh through the safety net, and F241's whole point is
// that an owner-mapped MCPProxyHost refusal must never reach that call),
// so a test can assert refreshes stayed at 0 to catch a regression that
// falls through to the ordinary refresh-and-retry path.
type ownerMappedChooser struct {
	mu        sync.Mutex
	account   string
	refreshes int
}

func (c *ownerMappedChooser) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	if d.Class == router.Untouched {
		return "", "", false, false
	}
	return c.account, "tok-" + c.account, d.Object != "", true
}

func (c *ownerMappedChooser) Record(router.Kind, []string, string) {}

func (c *ownerMappedChooser) Refresh(context.Context, string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshes++
	return "", false
}

// refreshCount reads refreshes under the mutex Refresh writes it under —
// mirrors refusingChooser: a bare field read here would race under -race
// with a concurrent Refresh call.
func (c *ownerMappedChooser) refreshCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshes
}

var _ proxy.Chooser = (*ownerMappedChooser)(nil)

// TestSafetyNetPassesThroughAConnectorOwnersRefusal covers F241/R96: a
// connector call to MCPProxyHost whose account came from the owner map
// gets 401 or 404 back from that owner — its own genuine protocol answer
// (a stale MCP session id, an auth handshake), not a chottag routing
// error. The client must see that response exactly as the owner sent it,
// upstream must see exactly one request (the owner's own bearer, never
// retried or resent), and RouteDrift must stay at 0.
func TestSafetyNetPassesThroughAConnectorOwnersRefusal(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			// ownerLabel classifies the Authorization header into a label,
			// never the raw value: this file's own rule (authLabel, above)
			// is that a test never prints a credential, fixtures included.
			ownerLabel := func(auth string) string {
				if auth == "Bearer tok-owner-acct" {
					return "owner"
				}
				return "unexpected"
			}
			var mu sync.Mutex
			var seen []string
			up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, ownerLabel(r.Header.Get("Authorization")))
				mu.Unlock()
				w.WriteHeader(status)
			})
			ch := &ownerMappedChooser{account: "owner-acct"}
			h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

			req, _ := http.NewRequest("POST", "https://mcp-proxy.anthropic.com/v1/mcp/conn-1", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != status {
				t.Fatalf("client saw %d, want %d passed straight through unchanged", resp.StatusCode, status)
			}

			mu.Lock()
			defer mu.Unlock()
			wantLabels(t, seen, []string{"owner"})
			if got := h.Server.RouteDrift(); got != 0 {
				t.Fatalf("RouteDrift = %d, want 0", got)
			}
			if got := ch.refreshCount(); got != 0 {
				t.Fatalf("refreshes = %d, want 0 (an owner's own refusal must never trigger the safety net's refresh-and-retry)", got)
			}
			recs := h.Records(t, "req", 1)
			if !recs[0].OwnerRefused {
				t.Fatalf("record = %+v, want OwnerRefused=true", recs[0])
			}
			if recs[0].Drift {
				t.Fatalf("record = %+v, want Drift=false: no resend happened here", recs[0])
			}
		})
	}
}

// TestSafetyNetUnknownOwnerConnectorCallStillDrifts covers R96's other
// half: a connector id the owner map does NOT know (the account came from
// the remote pin, not the owner map) keeps the safety net's refresh and
// counts as drift. Since R147 it is never resent on the client's own login
// (Home's): a remote request must not go out as the wrong account.
func TestSafetyNetUnknownOwnerConnectorCallStillDrifts(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, authLabel(auth, r.Header.Get("X-Api-Key")))
		mu.Unlock()
		if auth == "Bearer sk-ant-oat01-home" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	req, _ := http.NewRequest("POST", "https://mcp-proxy.anthropic.com/v1/mcp/conn-2", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("client saw %d, want the upstream 404 (R147: never resent on Home's login)", resp.StatusCode)
	}
	mu.Lock()
	wantLabels(t, seen, []string{"stale", "fresh"})
	mu.Unlock()
	if got := h.Server.RouteDrift(); got != 1 {
		t.Fatalf("RouteDrift = %d, want 1 (an unrecorded connector id is a routing error, not the owner's own answer)", got)
	}
}

// TestSafetyNetOwnerMappedAPIHostObjectStillDrifts pins F241/R96's scope
// to MCPProxyHost: an owner-mapped object on api.anthropic.com (e.g. an
// artifact) keeps the safety net's refresh and drift count, even though its
// account also came from the owner map — the connector protocol reasoning
// behind R96 does not apply there. Since R147 it is never resent on Home's
// login.
func TestSafetyNetOwnerMappedAPIHostObjectStillDrifts(t *testing.T) {
	var calls int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ch := &ownerMappedChooser{account: "owner-acct"}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/read/artifact-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("client saw %d, want the upstream 404 (R147: never resent on Home's login)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream saw %d requests, want 1 (the swapped attempt; this chooser's refresh fails, and there is no Home resend)", got)
	}
	if got := h.Server.RouteDrift(); got != 1 {
		t.Fatalf("RouteDrift = %d, want 1: an owner-mapped object still drifts outside MCPProxyHost", got)
	}
}

// authLabel names which login an upstream fake saw, never the header value
// itself: this repo never logs or prints a credential, including a test
// fixture, so every assertion below compares and reports labels.
func authLabel(auth, apiKey string) string {
	switch {
	case auth == "Bearer tok-stale":
		return "stale"
	case auth == "Bearer tok-fresh":
		return "fresh"
	case auth == "Bearer sk-ant-oat01-home" && apiKey == "sk-ant-api01-original":
		return "original+apikey"
	case auth == "Bearer sk-ant-oat01-home":
		return "original"
	default:
		return "unexpected"
	}
}

// wantLabels fails the test with only labels, never header values, if got
// doesn't match want exactly, in order.
func wantLabels(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("upstream saw %d requests %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("upstream request %d was %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestSafetyNetRefreshesThenResendsTheOriginal(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	var body atomic.Int64
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		n, _ := io.Copy(io.Discard, r.Body)
		body.Add(n)
		mu.Lock()
		seen = append(seen, authLabel(auth, r.Header.Get("X-Api-Key")))
		mu.Unlock()
		if auth == "Bearer sk-ant-oat01-home" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{"m":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("client saw %d, want 200 (the swap refusal must never reach it)", resp.StatusCode)
	}
	mu.Lock()
	wantLabels(t, seen, []string{"stale", "fresh", "original"})
	mu.Unlock()
	if ch.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", ch.refreshes)
	}
	if got := body.Load(); got != int64(3*len(`{"m":"hi"}`)) {
		t.Fatalf("upstream read %d body bytes, want the body replayed each time", got)
	}
	// R158: a refused serving request is the account's login being
	// refused, not route drift: no count, but the record keeps drift (the
	// resend happened) and names the first refused status.
	if got := h.Server.RouteDrift(); got != 0 {
		t.Fatalf("RouteDrift = %d, want 0 for a serving route", got)
	}
	if r := h.Records(t, "req", 1)[0]; !r.Drift || !r.Swapped || r.Refused != 401 {
		t.Fatalf("record = %+v, want drift, swapped and refused 401", r)
	}
}

type servingRefusal struct {
	account      string
	status       int
	resent       bool
	method, path string
}

// TestSafetyNetServingRefusalIsReportedNotCountedAsDrift: a 401 or 403 on a
// route the router lists as serving reports through OnServingRefusal and is
// not drift. Everything else keeps counting drift: an object or remote route
// (including /v1/sessions/{id}), a route the table does not list (the router's
// fallback), and a 404 on a serving route.
func TestSafetyNetServingRefusalIsReportedNotCountedAsDrift(t *testing.T) {
	cases := []struct {
		name, method, url string
		status            int
		wantDrift         uint64
		wantRefusals      int
	}{
		{"serving 401", "POST", "https://api.anthropic.com/v1/messages", 401, 0, 1},
		{"serving 403", "POST", "https://api.anthropic.com/v1/messages", 403, 0, 1},
		{"other serving path 404 stays drift", "POST", "https://api.anthropic.com/v1/messages/count_tokens", 404, 1, 0},
		{"unlisted route stays drift", "GET", "https://api.anthropic.com/some/new/route", 401, 1, 0},
		{"serving object stays drift", "POST", "https://api.anthropic.com/v1/sessions/sess-1/events", 401, 1, 0},
		{"object", "GET", "https://api.anthropic.com/api/frame/read/artifact-1", 401, 1, 0},
		{"remote", "POST", "https://api.anthropic.com/api/frame/deploy/prepare", 401, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(c.status)
			})
			var mu sync.Mutex
			var got []servingRefusal
			h := proxytest.Start(t, up, proxytest.Options{
				Choose: &refusingChooser{},
				OnServingRefusal: func(account string, status int, resent bool, method, path string) {
					mu.Lock()
					defer mu.Unlock()
					got = append(got, servingRefusal{account, status, resent, method, path})
				},
			})
			req, _ := http.NewRequest(c.method, c.url, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if d := h.Server.RouteDrift(); d != c.wantDrift {
				t.Fatalf("RouteDrift = %d, want %d", d, c.wantDrift)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != c.wantRefusals {
				t.Fatalf("serving refusals = %v, want %d", got, c.wantRefusals)
			}
			if c.wantRefusals == 1 {
				want := servingRefusal{"C", c.status, true, "POST", "/v1/messages"}
				if got[0] != want {
					t.Fatalf("refusal = %+v, want %+v", got[0], want)
				}
			}
			if r := h.Records(t, "req", 1)[0]; r.Refused != c.status {
				t.Fatalf("record refused = %d, want %d", r.Refused, c.status)
			}
		})
	}
}

// With the pool boundary on, a refused serving request is never resent on
// Home's login, and the report says so (resent=false).
func TestSafetyNetServingRefusalWithNoOriginalIsNotResent(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	var mu sync.Mutex
	var got []servingRefusal
	h := proxytest.Start(t, up, proxytest.Options{
		Choose: &guardChooser{guarded: true},
		OnServingRefusal: func(account string, status int, resent bool, method, path string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, servingRefusal{account, status, resent, method, path})
		},
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if want := (servingRefusal{"W", 401, false, "POST", "/v1/messages"}); len(got) != 1 || got[0] != want {
		t.Fatalf("refusals = %+v, want [%+v]", got, want)
	}
	if d := h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
}

// TestSafetyNetRouteDriftCountsConcurrentSwaps drives RouteDrift's counter
// concurrently under -race: a plain uint64++ would both race and undercount.
// It uses an object route: a refused serving route is not drift (R158).
func TestSafetyNetRouteDriftCountsConcurrentSwaps(t *testing.T) {
	const n = 20
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // refused on every login
	})
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/read/artifact-1", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	if got := h.Server.RouteDrift(); got != n {
		t.Fatalf("RouteDrift = %d, want %d", got, n)
	}
}

// TestSafetyNetSkipsRetryForDeclaredOversizedBody covers a request that
// declares (correctly) a Content-Length over the replay buffer: bufferBody's
// cheap early check must reject it without ever touching the body, same as
// the unknown-length case, so it is refused straight through with no retry.
func TestSafetyNetSkipsRetryForDeclaredOversizedBody(t *testing.T) {
	const bodySize = (4 << 20) + 4096 // just over the 4 MiB replay buffer
	body := bytes.Repeat([]byte{'x'}, bodySize)

	var received atomic.Int64
	var calls atomic.Int64
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		received.Add(n)
		w.WriteHeader(http.StatusUnauthorized)
	})
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	// bytes.NewReader gives http.NewRequest a correct, known Content-Length
	// (unlike round 1's unknown-length test), exercising the declared-size
	// guard rather than the after-the-fact len(buf) check.
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != int64(bodySize) {
		t.Fatalf("req.ContentLength = %d, want %d (test setup)", req.ContentLength, bodySize)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("client saw %d, want 401 passed straight through unchanged", resp.StatusCode)
	}
	if got := received.Load(); got != int64(bodySize) {
		t.Fatalf("upstream received %d body bytes, want all %d", got, bodySize)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream saw %d requests, want exactly 1 (no retry for an oversized body)", got)
	}
	if ch.refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0", ch.refreshes)
	}
	if got := h.Server.RouteDrift(); got != 0 {
		t.Fatalf("RouteDrift = %d, want 0", got)
	}
	recs := h.Records(t, "req", 1)
	if !recs[0].Unreplayable || recs[0].UnreplayableBytes != int64(bodySize) {
		t.Fatalf("trace record = %+v, want Unreplayable=true UnreplayableBytes=%d (the declared Content-Length) — this exact scenario (oversized body + refused swap) is the one gap the safety net cannot close; deferred pending data on how often it fires", recs[0], bodySize)
	}
	if recs[0].Drift {
		t.Fatalf("trace record = %+v, want Drift=false: no resend happened here", recs[0])
	}
}

// TestSafetyNetNeverTruncatesAnUnreplayableBody covers a swapped request
// whose body chottag cannot buffer for replay: unknown length (as a client
// sending Transfer-Encoding: chunked would arrive server-side) and bigger
// than the safety net's replay buffer. The first, non-retried, attempt
// upstream must still carry every byte the client sent — truncating it
// would fail a request that would otherwise have succeeded, which is worse
// than the documented "not retried or resent" behaviour.
func TestSafetyNetNeverTruncatesAnUnreplayableBody(t *testing.T) {
	const bodySize = (4 << 20) + 4096 // just over the 4 MiB replay buffer
	body := bytes.Repeat([]byte{'x'}, bodySize)

	var received atomic.Int64
	var calls atomic.Int64
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		received.Add(n)
		// Refused: an unreplayable body must never be retried or resent —
		// this refusal must reach the client exactly as the upstream sent it.
		w.WriteHeader(http.StatusUnauthorized)
	})
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // unknown length: forces chunked, like a real streamed upload
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("client saw %d, want 401 passed straight through unchanged", resp.StatusCode)
	}
	if got := received.Load(); got != int64(bodySize) {
		t.Fatalf("upstream received %d body bytes, want all %d (the body must never be truncated)", got, bodySize)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream saw %d requests, want exactly 1 (no retry/resend for an unreplayable body)", got)
	}
	if ch.refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0", ch.refreshes)
	}
	if got := h.Server.RouteDrift(); got != 0 {
		t.Fatalf("RouteDrift = %d, want 0", got)
	}
	recs := h.Records(t, "req", 1)
	if !recs[0].Unreplayable || recs[0].UnreplayableBytes != -1 {
		t.Fatalf("trace record = %+v, want Unreplayable=true UnreplayableBytes=-1 (declared length unknown, chunked)", recs[0])
	}
}

func TestSafetyNetLeavesSuccessfulSwapsAlone(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ch.refreshes != 0 || h.Server.RouteDrift() != 0 {
		t.Fatalf("refreshes=%d drift=%d, want 0/0", ch.refreshes, h.Server.RouteDrift())
	}
}

// TestSafetyNetPassesServingMessages404 is F270: a 404 on a serving
// POST /v1/messages (a Message Threads continue for a thread the account
// does not hold) reaches Claude Code unchanged, with no refresh, no resend
// on Home's login, no drift and no refusal report, and the trace record says
// passed404.
func TestSafetyNetPassesServingMessages404(t *testing.T) {
	const upBody = `{"type":"error","error":{"type":"not_found_error","message":"thread"}}`
	var mu sync.Mutex
	var seen []string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, upBody)
	})
	ch := &refusingChooser{}
	var refusals atomic.Int32
	h := proxytest.Start(t, up, proxytest.Options{
		Choose:           ch,
		OnServingRefusal: func(string, int, bool, string, string) { refusals.Add(1) },
	})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || string(got) != upBody {
		t.Fatalf("client saw %d %q, want the 404 body unchanged", resp.StatusCode, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "Bearer tok-stale" {
		t.Fatalf("upstream saw %v, want exactly one request on the swapped account", seen)
	}
	if n := ch.refreshes; n != 0 {
		t.Fatalf("refreshes = %d, want 0", n)
	}
	if d := h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
	if refusals.Load() != 0 {
		t.Fatalf("serving refusals reported = %d, want 0", refusals.Load())
	}
	r := h.Records(t, "req", 1)[0]
	if !r.Passed404 || r.Drift || r.Refused != 0 {
		t.Fatalf("record = %+v, want passed404 only", r)
	}
}

// A 404 on an object route still refreshes, resends and counts as drift.
func TestSafetyNetObject404StillDrifts(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	ch := &refusingChooser{}
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/read/artifact-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := h.Server.RouteDrift(); d != 1 {
		t.Fatalf("RouteDrift = %d, want 1", d)
	}
	if ch.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", ch.refreshes)
	}
	if r := h.Records(t, "req", 1)[0]; r.Passed404 || !r.Drift {
		t.Fatalf("record = %+v, want drift and not passed404", r)
	}
}
