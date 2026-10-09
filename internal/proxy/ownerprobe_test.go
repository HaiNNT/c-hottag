package proxy_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// probeChooser routes every object request to the remote account A with the
// owner unknown, and lists candidates for discovery.
type probeChooser struct {
	mu         sync.Mutex
	owner      bool
	cands      []proxy.Candidate
	candCalls  int
	refreshes  int
	recordKind router.Kind
	recordIDs  []string
	recordAcct string
	records    int
}

func (c *probeChooser) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	return "A", "tok-A", c.owner, true
}
func (c *probeChooser) Record(k router.Kind, ids []string, acct string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records++
	c.recordKind, c.recordIDs, c.recordAcct = k, ids, acct
}
func (c *probeChooser) Refresh(context.Context, string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshes++
	return "tok-A2", true
}
func (c *probeChooser) OwnerCandidates(_ context.Context, tried string) []proxy.Candidate {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.candCalls++
	return c.cands
}

var _ proxy.OwnerProber = (*probeChooser)(nil)

// noProbeChooser is a Chooser without OwnerProber.
type noProbeChooser struct{ probeChooser }

func (c *noProbeChooser) OwnerCandidates() {}

type unknownCall struct {
	account, kind, idHash string
	status                int
}

type probeEnv struct {
	h      *proxytest.Harness
	ch     *probeChooser
	mu     sync.Mutex
	seen   []string // bearer account labels in order
	unk    []unknownCall
	found  []string
	now    time.Time
	owners map[string]string // id -> token that opens it
}

func (e *probeEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func newProbeEnv(t *testing.T, ch proxy.Chooser, pc *probeChooser, owners map[string]string) *probeEnv {
	t.Helper()
	e := &probeEnv{ch: pc, now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), owners: owners}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		e.mu.Lock()
		e.seen = append(e.seen, tok)
		e.mu.Unlock()
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if want, ok := e.owners[id]; ok && want == tok {
			w.Write([]byte(`{"ok":"` + tok + `"}`))
			return
		}
		if tok == "tok-A" || tok == "tok-A2" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"first":"refusal"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"candidate":"refusal"}`))
	})
	e.h = proxytest.Start(t, up, proxytest.Options{
		Choose: ch,
		Now:    e.clock,
		OnUnknownOwner: func(a, k, h string, s int) {
			e.mu.Lock()
			e.unk = append(e.unk, unknownCall{a, k, h, s})
			e.mu.Unlock()
		},
		OnOwnerFound: func(kind, idHash, found, tried string, status int) {
			e.mu.Lock()
			e.found = append(e.found, kind+" "+idHash+" "+found+" "+tried)
			e.mu.Unlock()
		},
	})
	return e
}

func (e *probeEnv) get(t *testing.T, method, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "https://api.anthropic.com"+path, strings.NewReader(map[bool]string{true: `{"slug":"art-1"}`}[method == "POST"]))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func (e *probeEnv) seenNow() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

func sameSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func cands(names ...string) []proxy.Candidate {
	var out []proxy.Candidate
	for _, n := range names {
		out = append(out, proxy.Candidate{Account: n, Token: "tok-" + n})
	}
	return out
}

func TestOwnerDiscoveryFindsAndRecordsTheOwner(t *testing.T) {
	ch := &probeChooser{cands: cands("C", "B")}
	e := newProbeEnv(t, ch, ch, map[string]string{"art-1": "tok-B"})
	status, body := e.get(t, "GET", "/api/frame/art-1")
	if status != 200 || !strings.Contains(body, "tok-B") {
		t.Fatalf("client saw %d %q, want B's 200", status, body)
	}
	if got := e.seenNow(); !sameSeq(got, []string{"tok-A", "tok-C", "tok-B"}) {
		t.Fatalf("upstream saw %v", got)
	}
	if ch.refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0 (a 404 is not a stale token)", ch.refreshes)
	}
	if ch.records != 1 || ch.recordKind != router.KindArtifact || ch.recordAcct != "B" || len(ch.recordIDs) != 1 || ch.recordIDs[0] != "art-1" {
		t.Fatalf("Record = %d %v %v %v", ch.records, ch.recordKind, ch.recordIDs, ch.recordAcct)
	}
	if d := e.h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
	r := e.h.Records(t, "req", 1)[0]
	if !r.Discovered || r.Account != "B" || r.UnknownOwner || r.Drift {
		t.Fatalf("record = %+v", r)
	}
	want := "artifact " + tracelog.HashID("art-1") + " B A"
	if len(e.found) != 1 || e.found[0] != want {
		t.Fatalf("OnOwnerFound = %v, want [%s]", e.found, want)
	}
	if len(e.unk) != 0 {
		t.Fatalf("OnUnknownOwner = %v, want none", e.unk)
	}
}

func TestOwnerDiscoveryNobodyOwnsItIsCachedForAnHour(t *testing.T) {
	ch := &probeChooser{cands: cands("B", "C")}
	e := newProbeEnv(t, ch, ch, map[string]string{})
	status, body := e.get(t, "GET", "/api/frame/art-9")
	if status != 404 || !strings.Contains(body, "first") {
		t.Fatalf("client saw %d %q, want A's first refusal", status, body)
	}
	if got := e.seenNow(); !sameSeq(got, []string{"tok-A", "tok-B", "tok-C"}) {
		t.Fatalf("upstream saw %v", got)
	}
	if len(e.unk) != 1 || e.unk[0] != (unknownCall{"A", "artifact", tracelog.HashID("art-9"), 404}) {
		t.Fatalf("OnUnknownOwner = %v", e.unk)
	}
	if d := e.h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
	if ch.refreshes != 0 {
		t.Fatalf("refreshes = %d, want 0 on the nobody-owns path", ch.refreshes)
	}
	// A second request: no probes.
	e.get(t, "GET", "/api/frame/art-9")
	if ch.candCalls != 1 {
		t.Fatalf("candidate lookups = %d, want 1 (cache hit)", ch.candCalls)
	}
	if got := e.seenNow(); len(got) != 4 {
		t.Fatalf("upstream saw %v, want one more remote call only", got)
	}
	if len(e.unk) != 2 {
		t.Fatalf("OnUnknownOwner calls = %d, want 2", len(e.unk))
	}
	// After the TTL it probes again.
	e.mu.Lock()
	e.now = e.now.Add(proxy.UnknownOwnerTTL + time.Second)
	e.mu.Unlock()
	e.get(t, "GET", "/api/frame/art-9")
	if ch.candCalls != 2 {
		t.Fatalf("candidate lookups = %d, want 2 after the TTL", ch.candCalls)
	}
	if d := e.h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
	r := e.h.Records(t, "req", 3)[0]
	if !r.UnknownOwner || r.Refused != 404 || r.Drift || r.Discovered {
		t.Fatalf("record = %+v", r)
	}
}

func TestOwnerDiscoveryCacheClearsOnASuccess(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	owners := map[string]string{}
	e := newProbeEnv(t, ch, ch, owners)
	e.get(t, "GET", "/api/frame/art-3") // nobody: cached
	e.mu.Lock()
	owners["art-3"] = "tok-A" // now the remote can open it
	e.mu.Unlock()
	if s, _ := e.get(t, "GET", "/api/frame/art-3"); s != 200 {
		t.Fatalf("status = %d, want 200", s)
	}
	e.mu.Lock()
	delete(owners, "art-3")
	e.mu.Unlock()
	e.get(t, "GET", "/api/frame/art-3")
	if ch.candCalls != 2 {
		t.Fatalf("candidate lookups = %d, want 2 (entry cleared by the success)", ch.candCalls)
	}
}

func TestOwnerDiscoverySkipsPostButStillCountsUnknownOwner(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	e := newProbeEnv(t, ch, ch, map[string]string{"art-1": "tok-B"})
	e.get(t, "POST", "/api/frame/track")
	if len(e.unk) != 1 || e.unk[0] != (unknownCall{"A", "artifact", tracelog.HashID("art-1"), 404}) {
		t.Fatalf("OnUnknownOwner = %v", e.unk)
	}
	if ch.candCalls != 0 {
		t.Fatalf("a POST made %d candidate lookups, want 0", ch.candCalls)
	}
	if d := e.h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
}

func TestOwnerDiscoveryKeepsTheRefreshPathOn401(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-A" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	var unk []unknownCall
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch, OnUnknownOwner: func(a, k, h string, s int) { unk = append(unk, unknownCall{a, k, h, s}) }})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/art-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || ch.refreshes != 1 || ch.candCalls != 0 {
		t.Fatalf("status %d refreshes %d candLookups %d, want 200/1/0", resp.StatusCode, ch.refreshes, ch.candCalls)
	}
}

func TestOwnerDiscoveryStill401CountsUnknownOwnerNotDrift(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	var unk []unknownCall
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch, OnUnknownOwner: func(a, k, h string, s int) { unk = append(unk, unknownCall{a, k, h, s}) }})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/art-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ch.refreshes != 1 || ch.candCalls != 0 {
		t.Fatalf("refreshes %d candLookups %d, want 1/0", ch.refreshes, ch.candCalls)
	}
	if len(unk) != 1 || unk[0] != (unknownCall{"A", "artifact", tracelog.HashID("art-1"), 401}) {
		t.Fatalf("OnUnknownOwner = %v", unk)
	}
	if d := h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
}

func TestOwnerDiscoveryLeavesOwnerMappedRequestsAlone(t *testing.T) {
	ch := &probeChooser{owner: true, cands: cands("B")}
	e := newProbeEnv(t, ch, ch, map[string]string{})
	e.get(t, "GET", "/api/frame/art-1")
	if ch.candCalls != 0 || len(e.unk) != 0 {
		t.Fatalf("candLookups %d unknown %v, want none", ch.candCalls, e.unk)
	}
	if d := e.h.Server.RouteDrift(); d != 1 {
		t.Fatalf("RouteDrift = %d, want 1 (an owner-mapped refusal still drifts)", d)
	}
}

func TestOwnerDiscoveryTransportErrorEndsIt(t *testing.T) {
	ch := &probeChooser{cands: cands("B", "C")}
	// B's token makes the upstream hijack and cut the connection.
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer tok-B":
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
		case "Bearer tok-C":
			w.Write([]byte("C answered"))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("first"))
		}
	})
	var unk []unknownCall
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch, OnUnknownOwner: func(a, k, h string, s int) { unk = append(unk, unknownCall{a, k, h, s}) }})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/art-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want A's 404", resp.StatusCode)
	}
	if ch.records != 0 {
		t.Fatal("recorded an owner after a transport error")
	}
	if len(unk) != 1 || h.Server.RouteDrift() != 0 {
		t.Fatalf("unknown %v drift %d", unk, h.Server.RouteDrift())
	}
}

func TestOwnerDiscoveryWithoutAProberJustDoesNotDrift(t *testing.T) {
	ch := &noProbeChooser{}
	e := newProbeEnv(t, ch, &ch.probeChooser, map[string]string{})
	e.get(t, "GET", "/api/frame/art-1")
	if d := e.h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d, want 0", d)
	}
	if len(e.unk) != 1 {
		t.Fatalf("OnUnknownOwner = %v", e.unk)
	}
}

func TestOwnerDiscoveryConcurrentIDs(t *testing.T) {
	ch := &probeChooser{cands: cands("B", "C")}
	e := newProbeEnv(t, ch, ch, map[string]string{"art-b": "tok-B", "art-c": "tok-C"})
	ids := []string{"art-b", "art-c", "art-x", "art-y"}
	want := map[string]int{"art-b": 200, "art-c": 200, "art-x": 404, "art-y": 404}
	var wg sync.WaitGroup
	var rmu sync.Mutex
	got := map[string]int{}
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _ := e.get(t, "GET", "/api/frame/"+id)
			rmu.Lock()
			got[id] = s
			rmu.Unlock()
		}()
	}
	wg.Wait()
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s: status %d, want %d", id, got[id], w)
		}
	}
	if d := e.h.Server.RouteDrift(); d != 0 {
		t.Fatalf("RouteDrift = %d", d)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.unk) != 2 || len(e.found) != 2 {
		t.Fatalf("unknown %v found %v", e.unk, e.found)
	}
}

func TestOwnerDiscoveryProbesAHead(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	e := newProbeEnv(t, ch, ch, map[string]string{"art-1": "tok-B"})
	if s, _ := e.get(t, "HEAD", "/api/frame/art-1"); s != 200 {
		t.Fatalf("status = %d, want 200", s)
	}
	if ch.records != 1 || ch.recordAcct != "B" || ch.refreshes != 0 {
		t.Fatalf("records %d acct %s refreshes %d", ch.records, ch.recordAcct, ch.refreshes)
	}
}

func TestOwnerDiscoveryRecordsOnlyASuccess(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-B" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	var found int
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch, OnOwnerFound: func(string, string, string, string, int) { found++ }})
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/art-1", nil)
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
		resp, err := h.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 502 {
			t.Fatalf("status = %d, want B's 502", resp.StatusCode)
		}
	}
	if ch.records != 0 || found != 0 {
		t.Fatalf("records %d found hooks %d, want 0", ch.records, found)
	}
	if ch.candCalls != 2 {
		t.Fatalf("candidate lookups = %d, want 2 (nothing cached)", ch.candCalls)
	}
}

func TestOwnerDiscoverySkipsTheTriedAccountAndEmptyTokens(t *testing.T) {
	ch := &probeChooser{cands: []proxy.Candidate{{Account: "a", Token: "tok-A"}, {Account: "X", Token: ""}, {Account: "B", Token: "tok-B"}}}
	e := newProbeEnv(t, ch, ch, map[string]string{})
	e.get(t, "GET", "/api/frame/art-1")
	if got := e.seenNow(); !sameSeq(got, []string{"tok-A", "tok-B"}) {
		t.Fatalf("upstream saw %v", got)
	}
}

// A serving-class object (an unknown cloud-session id) is not the remote
// fallback: it keeps the pre-M12 path (refresh, Home resend, drift) and is
// never probed.
func TestOwnerDiscoveryLeavesServingObjectsAlone(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	var unknown, found int
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-home" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	h := proxytest.Start(t, up, proxytest.Options{
		Choose:         ch,
		OnUnknownOwner: func(string, string, string, int) { unknown++ },
		OnOwnerFound:   func(string, string, string, string, int) { found++ },
	})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/sessions/sess-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want the Home resend's 200", resp.StatusCode)
	}
	if ch.candCalls != 0 || ch.refreshes != 1 {
		t.Fatalf("candidate lookups %d refreshes %d, want 0 and 1", ch.candCalls, ch.refreshes)
	}
	if d := h.Server.RouteDrift(); d != 1 {
		t.Fatalf("RouteDrift = %d, want 1", d)
	}
	if unknown != 0 || found != 0 {
		t.Fatalf("hooks unknown %d found %d, want none", unknown, found)
	}
	if r := h.Records(t, "req", 1)[0]; r.UnknownOwner || !r.Drift {
		t.Fatalf("record = %+v, want drift and no unknown-owner", r)
	}
}

// A connector id is never probed, even when its owner is unknown.
func TestOwnerDiscoverySkipsConnectors(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})
	req, _ := http.NewRequest("GET", "https://mcp-proxy.anthropic.com/v1/mcp/srv_1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ch.candCalls != 0 {
		t.Fatalf("candidate lookups = %d, want 0 for a connector", ch.candCalls)
	}
	if r := h.Records(t, "req", 1)[0]; r.UnknownOwner || r.Discovered {
		t.Fatalf("record = %+v", r)
	}
}

// The safety net bounds the whole candidate lookup with one deadline.
func TestOwnerDiscoveryCandidatesGetOneBudget(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	dc := &deadlineChooser{probeChooser: ch}
	e := newProbeEnv(t, dc, ch, map[string]string{})
	e.get(t, "GET", "/api/frame/art-1")
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if !dc.hasDeadline {
		t.Fatal("OwnerCandidates ctx has no deadline")
	}
	if dc.remaining <= 0 || dc.remaining > proxy.OwnerCandidatesBudget {
		t.Fatalf("remaining budget %v, want within (0, %v]", dc.remaining, proxy.OwnerCandidatesBudget)
	}
}

type deadlineChooser struct {
	*probeChooser
	hasDeadline bool
	remaining   time.Duration
}

func (c *deadlineChooser) OwnerCandidates(ctx context.Context, tried string) []proxy.Candidate {
	c.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		c.hasDeadline, c.remaining = true, time.Until(dl)
	}
	c.mu.Unlock()
	return c.probeChooser.OwnerCandidates(ctx, tried)
}

// A candidate's 429 or 5xx handed to the client retargets the record and
// usage to that candidate, without recording an owner.
func TestOwnerDiscoveryRetargetsOnACandidateErrorAnswer(t *testing.T) {
	ch := &probeChooser{cands: cands("B")}
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-B" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: ch})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/frame/art-1", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want B's 429", resp.StatusCode)
	}
	r := h.Records(t, "req", 1)[0]
	if r.Account != "B" || r.Discovered || ch.records != 0 {
		t.Fatalf("record = %+v records %d, want account B, not discovered, none recorded", r, ch.records)
	}
}
