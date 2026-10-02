package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// recorder is a Chooser that always swaps — even a request classed
// Untouched, if asked — and remembers what it was told. It is deliberately
// permissive so that a test relying on it proves the proxy's own guards
// (e.g. never asking about, or swapping, an Untouched route) rather than
// the fake refusing on the proxy's behalf.
type recorder struct {
	mu      sync.Mutex
	token   string
	account string
	bodyIDs []string
	records []string       // "kind:id=account"
	asked   []router.Class // every class Choose was called with
}

func (r *recorder) Choose(_ context.Context, d router.Decision, bodyID string) (string, string, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, d.Class)
	if bodyID != "" {
		r.bodyIDs = append(r.bodyIDs, bodyID)
	}
	return r.account, r.token, false, true
}

func (r *recorder) Refresh(context.Context, string) (string, bool) { return "", false }

func (r *recorder) Record(kind router.Kind, ids []string, account string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		r.records = append(r.records, string(kind)+":"+id+"="+account)
	}
}

func (r *recorder) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodyIDs...), append([]string(nil), r.records...)
}

// waitForRecords blocks until at least n owner records have landed, or a 2s
// deadline passes — the record_test.go analogue of proxytest.Harness.Records
// (harness.go:138)'s poll-for-count pattern. A test must never assert on
// r.records immediately after its HTTP round trip returns: the client
// receiving a response happens-before nothing about the server's own
// bookkeeping for that same request (F14), so an immediate snapshot proves
// nothing about whether Record ran in time for a real follow-up request.
func (r *recorder) waitForRecords(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		out := append([]string(nil), r.records...)
		r.mu.Unlock()
		if len(out) >= n || time.Now().After(deadline) {
			if len(out) < n {
				t.Fatalf("want %d owner records, got %d: %v", n, len(out), out)
			}
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecordsOwnersFromResponseAndRequestBodies(t *testing.T) {
	rec := &recorder{token: "tok-C", account: "C"}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/code/sessions":
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_1"}})
		default:
			// /api/frame/deploy/direct deliberately does NOT echo the slug
			// back here: its RecordField and RequestField are both "slug",
			// so an echoing response would let the artifact record come
			// from either source and hide a broken request-body Record
			// call. An empty body forces "artifact:abc123=C" to come only
			// from the id already read out of the request body.
			w.Write([]byte(`{}`))
		}
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: rec})

	post := func(path, body string) {
		t.Helper()
		req, _ := http.NewRequest("POST", "https://api.anthropic.com"+path, stringBody(body))
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("/v1/code/sessions", `{}`)
	post("/api/frame/deploy/direct", `{"slug":"abc123","content":"<h1>hi</h1>"}`)

	bodyIDs, _ := rec.snapshot()
	if len(bodyIDs) != 1 || bodyIDs[0] != "abc123" {
		t.Errorf("request-body ids = %v, want [abc123]", bodyIDs)
	}
	records := rec.waitForRecords(t, 2)
	want := map[string]bool{"session:cse_1=C": false, "artifact:abc123=C": false}
	for _, r := range records {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("owner %q not recorded (got %v)", k, records)
		}
	}

	for _, r := range h.Records(t, "req", 2) {
		if r.Account != "C" {
			t.Errorf("trace record account = %q, want %q: %+v", r.Account, "C", r)
		}
	}
}

// ownerChooser is a Chooser backed by a real *owners.Map, standing in for
// production's selector: a route that names an object routes to that
// object's recorded creator when the owner map knows it; a fresh artifact
// publish (no RequestField — nothing to look up yet) always goes out as "C",
// simulating whichever account is remote-default at creation time; every
// other remote/serving route with no recorded owner falls back to "R",
// simulating a remote-default account that has since rotated away from the
// one that actually created the object. This gap between "C" (the creator)
// and "R" (today's default) is what makes a race in owner recording
// observable: a republish that outraces its publish's Record call routes as
// "R" instead of "C".
type ownerChooser struct{ own *owners.Map }

func (c *ownerChooser) Choose(_ context.Context, d router.Decision, bodyID string) (string, string, bool, bool) {
	if d.Class == router.Untouched {
		return "", "", false, false
	}
	if d.Object != "" {
		id := d.ObjectID
		if id == "" {
			id = bodyID
		}
		if id != "" {
			if owner, ok := c.own.Lookup(d.Object, id); ok {
				return owner, "tok-" + owner, true, true
			}
		}
	}
	if d.Records == router.KindArtifact && d.RequestField == "" {
		return "C", "tok-C", false, true
	}
	return "R", "tok-R", false, true
}

func (c *ownerChooser) Refresh(context.Context, string) (string, bool) { return "", false }

func (c *ownerChooser) Record(kind router.Kind, ids []string, account string) {
	c.own.Record(kind, ids, account, time.Now())
}

// TestZeroIDExtractionOnRecordingRouteIsCounted guards the fix at
// forward.go:187 (F19/F27): unlike the request-body Record site, the
// response-derived site no longer guards its call with len(ids) > 0, so a
// recording route (session create) whose 2xx JSON response carries no id at
// RecordField — the shape a broken or changed upstream response would have
// — must count as a zero-id extraction through to owners.Map, rather than
// looking identical to a quiet session with nothing to record. Record runs
// synchronously inside ModifyResponse, before ReverseProxy copies a byte
// back to the client (F14), so the count is already final by the time
// h.Client.Do returns — no poll needed, unlike recorder.waitForRecords.
func TestZeroIDExtractionOnRecordingRouteIsCounted(t *testing.T) {
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	ch := &ownerChooser{own: own}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON, 2xx, but no "session.id" field: exactly a shape
		// mismatch, not an empty/failed request.
		w.Write([]byte(`{}`))
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})
	before := own.ZeroIDExtractions()

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/code/sessions", stringBody(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := own.ZeroIDExtractions(); got != before+1 {
		t.Fatalf("ZeroIDExtractions() = %d, want %d — a recording route with no extractable id must be counted", got, before+1)
	}
}

// TestRepublishRoutesByOwnerEvenWhenClientActsImmediately is F14's proof: it
// reproduces the exact shape of the measured product defect (a client that
// parses a create response and immediately issues a follow-up naming that
// object) end to end through a real *owners.Map, rather than only inspecting
// a fake's internal state. Before F14 parts (a)/(b), owner recording ran in
// forward's deferred block — after ReverseProxy had already copied the
// create response back to the client — so an immediate republish could win
// the race and route on "R" (today's class default) instead of "C" (the
// actual creator). Run with -count and -race to catch the ~1-in-150 tail;
// a single green run proves nothing about an ordering bug this rare.
func TestRepublishRoutesByOwnerEvenWhenClientActsImmediately(t *testing.T) {
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	ch := &ownerChooser{own: own}

	republishAuth := make(chan string, 1)
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/frame/deploy/prepare":
			json.NewEncoder(w).Encode(map[string]any{"slug": "art1"})
		case "/api/frame/deploy/direct":
			republishAuth <- r.Header.Get("Authorization")
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{}`))
		}
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})

	publish, _ := http.NewRequest("POST", "https://api.anthropic.com/api/frame/deploy/prepare", stringBody(`{}`))
	publish.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	publish.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(publish)
	if err != nil {
		t.Fatal(err)
	}
	// A real client must read the slug out of the response before it can
	// even construct the republish request — exactly the "acts immediately
	// on the response" shape F14 describes, with no artificial delay added.
	// A cheap byte scan instead of a full JSON decode keeps the gap between
	// "client has the response" and "client fires the follow-up" as tight as
	// the race actually is (measured in the tens of microseconds); the
	// reflection-heavy encoding/json decoder alone is enough to mask the
	// race by giving forward's deferred block time to finish first.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	const marker = `"slug":"`
	i := bytes.Index(body, []byte(marker))
	if i < 0 {
		t.Fatalf("publish response %q carried no slug", body)
	}
	rest := body[i+len(marker):]
	slug := string(rest[:bytes.IndexByte(rest, '"')])
	if slug == "" {
		t.Fatalf("publish response %q carried no slug", body)
	}

	republish, _ := http.NewRequest("POST", "https://api.anthropic.com/api/frame/deploy/direct",
		stringBody(`{"slug":"`+slug+`"}`))
	republish.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	republish.Header.Set("Content-Type", "application/json")
	resp2, err := h.Client.Do(republish)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if got := <-republishAuth; got != "Bearer tok-C" {
		t.Fatalf("republish went out as %q, want the creator's token (Bearer tok-C) — owner recording lost the race", got)
	}
}

// TestUntouchedRouteIsNotRecordedOrSwapped guards forward's
// `d.Class != router.Untouched` swap guard directly: rec is deliberately
// willing to swap and record anything it is asked about (see recorder's
// doc comment), so the only thing that can keep an Untouched route from
// being swapped, or from ever reaching Choose at all, is that guard.
func TestUntouchedRouteIsNotRecordedOrSwapped(t *testing.T) {
	rec := &recorder{token: "tok-C", account: "C"}
	seen := make(chan string, 1)
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: rec})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/code/sessions/cse_1/worker/events", stringBody(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-seen; got != "Bearer sk-ant-oat01-home" {
		t.Fatalf("untouched route went out as %q", got)
	}
	if _, records := rec.snapshot(); len(records) != 0 {
		t.Fatalf("records = %v, want none", records)
	}
	rec.mu.Lock()
	asked := append([]router.Class(nil), rec.asked...)
	rec.mu.Unlock()
	if len(asked) != 0 {
		t.Fatalf("Choose was asked about the Untouched route: %v", asked)
	}
}

// TestNonSuccessResponseDoesNotRecordOwners guards forward's 2xx gate on
// owner recording directly: a failed create must not teach the owner map
// that the response body's ids belong to the swapped account, since an
// error response is not a reliable statement of what was actually created.
func TestNonSuccessResponseDoesNotRecordOwners(t *testing.T) {
	rec := &recorder{token: "tok-C", account: "C"}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_1"}})
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: rec})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/code/sessions", stringBody(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, records := rec.snapshot(); len(records) != 0 {
		t.Fatalf("records = %v, want none for a non-2xx response", records)
	}
}

// TestDriftedResponseDoesNotRecordOwners guards the BLOCKER this wave fixes:
// the safety net may abandon the swapped account entirely and resend the
// ORIGINAL request on the client's own login, returning a genuine 200 from
// an account that never actually made the swapped call. httputil.ReverseProxy
// runs ModifyResponse on whatever RoundTrip returns, so a drifted 200 is
// byte-for-byte indistinguishable from a real one there — recording must
// instead consult rec.Drift, which safetyNet sets before the final RoundTrip
// returns. Without that gate this test fails for both recording sites: the
// request-body id (the artifact publish) and the response-derived id (the
// session create), matching forward.go:125-126 and :146-148.
//
// recorder never succeeds a Refresh (see recorder.Refresh), so a refusal on
// the swapped token falls straight through to the original-login resend —
// exactly the path that must record nothing. Since R147 a remote or owner
// request is not resent on Home's login at all: the refusal goes back.
func TestDriftedResponseDoesNotRecordOwners(t *testing.T) {
	rec := &recorder{token: "tok-C", account: "C"}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer tok-C" {
			// The swapped account is refused: chottag's account C never
			// actually made this call.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// The resend on the client's own (Home) login succeeds.
		switch r.URL.Path {
		case "/v1/code/sessions":
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"id": "cse_1"}})
		default:
			w.Write([]byte(`{}`))
		}
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: rec})

	post := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", "https://api.anthropic.com"+path, stringBody(body))
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp1 := post("/v1/code/sessions", `{}`)
	if resp1.StatusCode != http.StatusForbidden {
		t.Fatalf("resp1 status = %d, want 403 (R147: a remote request is never resent on Home's login)", resp1.StatusCode)
	}
	resp1.Body.Close()

	resp2 := post("/api/frame/deploy/direct", `{"slug":"abc123","content":"<h1>hi</h1>"}`)
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("resp2 status = %d, want 403 (R147: a remote request is never resent on Home's login)", resp2.StatusCode)
	}
	resp2.Body.Close()

	// rec.bodyIDs (populated by Choose, before the request is even sent) is
	// not the thing under test here — the drift happens after Choose, once
	// the swapped attempt is refused. What must be empty is rec.records: the
	// list of ids actually Record-ed, from EITHER site (the request-body id
	// for the artifact publish, the response-derived id for the session
	// create).
	_, records := rec.snapshot()
	if len(records) != 0 {
		t.Errorf("owner records = %v, want none: account %q was refused on both calls and never actually made either — a drifted response resent on the client's own login must never teach the owner map that %q created this object", records, rec.account, rec.account)
	}
}

func stringBody(s string) *strings.Reader { return strings.NewReader(s) }

var _ proxy.Chooser = (*recorder)(nil)
