package proxy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

const homeTok = "sk-ant-oat01-HOME-SECRET"
const otherTok = "sk-ant-oat01-OTHER-SECRET"

type servingSwap struct{}

func (servingSwap) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	if d.Class == router.Serving {
		return "B", otherTok, false, true
	}
	return "", "", false, false
}

func (servingSwap) Record(router.Kind, []string, string) {}

func (servingSwap) Refresh(context.Context, string) (string, bool) { return "", false }

// classChooser swaps every non-Untouched class onto an account named after
// the class, so a test can tell which class (and, via the trace record's
// Account, confirm it) a request was routed as.
type classChooser struct{}

func (classChooser) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	if d.Class == router.Untouched {
		return "", "", false, false
	}
	return "acct-" + string(d.Class), "tok-" + string(d.Class), false, true
}

func (classChooser) Record(router.Kind, []string, string) {}

func (classChooser) Refresh(context.Context, string) (string, bool) { return "", false }

func authEcho(seen chan<- string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
}

func do(t *testing.T, h *proxytest.Harness, method, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp
}

func TestMITMForwardsAndTraces(t *testing.T) {
	seen := make(chan string, 1)
	h := proxytest.Start(t, authEcho(seen), proxytest.Options{})
	resp := do(t, h, "GET", "https://api.anthropic.com/api/oauth/usage")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := <-seen; got != "Bearer "+homeTok {
		t.Fatalf("upstream saw %q", got)
	}
	r := h.Records(t, "req", 1)[0]
	if r.Form != "mitm" || r.Class != "serving" || r.Auth != "oauth-access" || r.Status != 200 || r.Path != "/api/oauth/usage" || r.Swapped {
		t.Fatalf("record %+v", r)
	}
	if h.LogContains(t, "HOME-SECRET") {
		t.Fatal("token leaked into log")
	}
}

// TestMITMSendsOriginalHostHeader guards against the MITM path leaking the
// dial target's port into the upstream Host header: upstream should see
// exactly what the client sent ("api.anthropic.com"), not the CONNECT
// hostport ("api.anthropic.com:443").
func TestMITMSendsOriginalHostHeader(t *testing.T) {
	hostSeen := make(chan string, 1)
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostSeen <- r.Host
		w.WriteHeader(200)
	}), proxytest.Options{})
	do(t, h, "GET", "https://api.anthropic.com/v1/models")
	if got := <-hostSeen; got != "api.anthropic.com" {
		t.Fatalf("upstream saw Host %q, want %q", got, "api.anthropic.com")
	}
}

func TestSwapServingOnly(t *testing.T) {
	seen := make(chan string, 2)
	h := proxytest.Start(t, authEcho(seen), proxytest.Options{Choose: servingSwap{}})
	do(t, h, "POST", "https://api.anthropic.com/v1/messages?beta=true")
	if got := <-seen; got != "Bearer "+otherTok {
		t.Fatalf("serving route: upstream saw %q", got)
	}
	do(t, h, "GET", "https://api.anthropic.com/v1/environments/env_01ABCDEFGH12345678/work/poll")
	if got := <-seen; got != "Bearer "+homeTok {
		t.Fatalf("untouched route: upstream saw %q", got)
	}
	recs := h.Records(t, "req", 2)
	if !recs[0].Swapped || recs[1].Swapped {
		t.Fatalf("swapped flags wrong: %+v", recs)
	}
	if h.LogContains(t, "OTHER-SECRET") || h.LogContains(t, "HOME-SECRET") {
		t.Fatal("token leaked into log")
	}
}

// TestSwapRequiresOriginalOAuthAccess guards against swapping a request whose
// original credential wasn't an oauth access token: a jwt or no-auth request
// on an otherwise-Serving route must reach upstream unchanged, even when a
// Chooser is configured.
func TestSwapRequiresOriginalOAuthAccess(t *testing.T) {
	seen := make(chan string, 2)
	h := proxytest.Start(t, authEcho(seen), proxytest.Options{Choose: servingSwap{}})

	const jwt = "Bearer eyJhbGciOiJIUzI1NiJ9.fake.jwt"
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", jwt)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := <-seen; got != jwt {
		t.Fatalf("jwt request: upstream saw %q, want unchanged %q", got, jwt)
	}

	req2, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	resp2, err := h.Client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if got := <-seen; got != "" {
		t.Fatalf("no-auth request: upstream saw %q, want none", got)
	}

	recs := h.Records(t, "req", 2)
	for _, r := range recs {
		if r.Swapped {
			t.Fatalf("record swapped although original auth wasn't oauth-access: %+v", r)
		}
	}
	if h.LogContains(t, otherTok) {
		t.Fatal("swapped token leaked into log despite not being used")
	}
}

// TestRecordTimeIsRequestStart guards against attributing a long-lived
// request to the wrong trace mark: T must be stamped at request start, not
// at the time the record is written (request end).
func TestRecordTimeIsRequestStart(t *testing.T) {
	const delay = 300 * time.Millisecond
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}), proxytest.Options{})
	t0 := time.Now()
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	r := h.Records(t, "req", 1)[0]
	if got := r.T.Sub(t0); got < 0 || got > delay/2 {
		t.Fatalf("record T is %v after request start (handler delayed %v); want close to 0 (start time), not close to the delay (end time)", got, delay)
	}
}

// TestTunnelRecordTimeIsConnectStart is the tunnel-record analogue of
// TestRecordTimeIsRequestStart for a blind CONNECT tunnel.
func TestTunnelRecordTimeIsConnectStart(t *testing.T) {
	const delay = 300 * time.Millisecond
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{})
	t0 := time.Now()
	c, _, status := connect(t, h, "echo.example.org:443")
	if status != 200 {
		t.Fatalf("CONNECT status %d", status)
	}
	time.Sleep(delay)
	c.Close()
	r := h.Records(t, "tunnel", 1)[0]
	if got := r.T.Sub(t0); got < 0 || got > delay/2 {
		t.Fatalf("record T is %v after connect start (tunnel held open %v); want close to 0 (start time)", got, delay)
	}
}

func TestAbsoluteForm(t *testing.T) {
	seen := make(chan string, 1)
	h := proxytest.Start(t, authEcho(seen), proxytest.Options{})
	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST https://api.anthropic.com/v1/environments/bridge HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\n\r\n{}", homeTok)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp %v err %v", resp, err)
	}
	<-seen
	r := h.Records(t, "req", 1)[0]
	if r.Form != "absolute" || r.Class != "remote" {
		t.Fatalf("record %+v", r)
	}
}

// TestAbsoluteFormDerivationFromWireShape pins forward's own translation of
// which wire path a request actually arrived on into router.Request's
// AbsoluteForm (forward.go: `AbsoluteForm: form == "absolute"`) — not just
// router.Route's behaviour once AbsoluteForm is known, which
// TestRouteUsesTheRequestForm in router_test.go already covers.
// POST /api/oauth/validate is the one route the table classifies
// differently per form: remote for the `claude remote-control` server's
// absolute-form call (proxy.go's r.URL.IsAbs() path, no CONNECT), serving
// for a REPL's own login over the MITM path (connect.go). Hardcoding
// AbsoluteForm to a constant, or inverting form == "absolute", makes both
// requests below classify identically and this test catches either.
func TestAbsoluteFormDerivationFromWireShape(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: classChooser{}})

	// Absolute form: an absolute-URL request line sent directly over the
	// proxy connection, with no CONNECT tunnel — the shape `claude
	// remote-control`'s server uses (proxy.go's r.URL.IsAbs() case).
	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST https://api.anthropic.com/api/oauth/validate HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\n\r\n{}", homeTok)
	absResp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || absResp.StatusCode != 200 {
		t.Fatalf("absolute-form resp %v err %v", absResp, err)
	}
	io.ReadAll(absResp.Body)
	absResp.Body.Close()

	// MITM form: the same call through CONNECT + TLS — the shape a REPL
	// session uses for its own login (connect.go's mitm path).
	do(t, h, "POST", "https://api.anthropic.com/api/oauth/validate")

	recs := h.Records(t, "req", 2)
	absRec, mitmRec := recs[0], recs[1]
	if absRec.Form != "absolute" || mitmRec.Form != "mitm" {
		t.Fatalf("unexpected forms: absolute rec %+v, mitm rec %+v", absRec, mitmRec)
	}
	if absRec.Class != "remote" || absRec.Account != "acct-remote" {
		t.Fatalf("absolute-form validate = %+v, want class remote / account acct-remote", absRec)
	}
	if mitmRec.Class != "serving" || mitmRec.Account != "acct-serving" {
		t.Fatalf("mitm-form validate = %+v, want class serving / account acct-serving", mitmRec)
	}
}

func connect(t *testing.T, h *proxytest.Harness, hostport string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostport, hostport)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp.StatusCode
}

func TestBlindTunnel(t *testing.T) {
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{})
	c, br, status := connect(t, h, "echo.example.org:443")
	if status != 200 {
		t.Fatalf("CONNECT status %d", status)
	}
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo got %q err %v", buf, err)
	}
	c.Close()
	r := h.Records(t, "tunnel", 1)[0]
	if r.Form != "blind" || r.Host != "echo.example.org:443" {
		t.Fatalf("record %+v", r)
	}
}

func TestBlindTunnelUnreachable(t *testing.T) {
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{DownHosts: []string{"down.example.org"}})
	c, _, status := connect(t, h, "down.example.org:443")
	defer c.Close()
	if status != http.StatusBadGateway {
		t.Fatalf("CONNECT status %d, want 502", status)
	}
}

func TestSSEFlushesPerEvent(t *testing.T) {
	release := make(chan struct{})
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: two\n\n")
	}), proxytest.Options{})
	defer close(release)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: one\n" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first event not flushed before upstream finished")
	}
}

func TestShapesCaptureIDsNotSecrets(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"environment_id":"env_01ABCDEFGH12345678","secret":"sk-ant-oat01-XYZ-SECRET"}`)
	}), proxytest.Options{Shapes: true})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/environments/bridge", strings.NewReader(`{"machine_name":"mbp"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	r := h.Records(t, "req", 1)[0]
	b, _ := json.Marshal(r.RespShape)
	if !strings.Contains(string(b), `"environment_id":"id:`+tracelog.HashID("env_01ABCDEFGH12345678")) {
		t.Fatalf("resp shape %s", b)
	}
	rb, _ := json.Marshal(r.ReqShape)
	if !strings.Contains(string(rb), `"machine_name":"string"`) {
		t.Fatalf("req shape %s", rb)
	}
	if h.LogContains(t, "XYZ-SECRET") || h.LogContains(t, "mbp") {
		t.Fatal("value leaked into log")
	}
}

// TestClientCancelIsNotLoggedAs502 checks that a client-canceled request
// (the client hangs up while the proxy is still waiting on a slow upstream)
// is recorded as Status 0 / "client canceled", not a spurious 502, since the
// upstream never actually misbehaved.
func TestClientCancelIsNotLoggedAs502(t *testing.T) {
	release := make(chan struct{})
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(200)
	}), proxytest.Options{})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := h.Client.Do(req); err == nil {
		t.Fatal("expected client error after context cancel")
	}

	r := h.Records(t, "req", 1)[0]
	if r.Status != 0 || r.Err != "client canceled" {
		t.Fatalf("record %+v, want Status=0 Err=\"client canceled\"", r)
	}
}

func TestHealthIdentifiesChottagAndEchoesNothing(t *testing.T) {
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{Version: "test-version"})

	resp, err := http.Get("http://" + h.ProxyAddr + proxy.HealthPath + "?probe=" + url.QueryEscape("<script>x</script>"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var got proxy.Health
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("health body is not JSON: %v (%q)", err, body)
	}
	if !got.Chottag {
		t.Errorf("health document does not identify chottag: %q", body)
	}
	if got.PID != os.Getpid() {
		t.Errorf("health PID = %d, want this process %d", got.PID, os.Getpid())
	}
	if got.Version != "test-version" {
		t.Errorf("health Version = %q, want the configured %q: an unasserted config field is a field nobody notices going dead", got.Version, "test-version")
	}
	if strings.Contains(string(body), "script") {
		t.Errorf("health document echoed request content: %q", body)
	}
}

// TestHealthNeverSerialisesAnUpstreamPassword pins fix round 3's D1: the
// health document is unauthenticated and loopback-reachable by any local
// process, so a configured upstream proxy's userinfo must never reach the
// wire here at all — not masked, absent (Health.Upstream's doc comment).
func TestHealthNeverSerialisesAnUpstreamPassword(t *testing.T) {
	up, err := url.Parse("http://bob:hunter2@corp-proxy.example:3128")
	if err != nil {
		t.Fatal(err)
	}
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{UpstreamProxy: up})

	resp, err := http.Get("http://" + h.ProxyAddr + proxy.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(body), "hunter2") || strings.Contains(string(body), "bob:") {
		t.Fatalf("health document leaked the upstream proxy's userinfo: %q", body)
	}

	var got proxy.Health
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("health body is not JSON: %v (%q)", err, body)
	}
	if want := "http://corp-proxy.example:3128"; got.Upstream != want {
		t.Errorf("health Upstream = %q, want %q (scheme://host, userinfo removed)", got.Upstream, want)
	}
}

// The endpoint must be unreachable through a CONNECT tunnel: inside a
// tunnel the request is served by the INNER http.Server, which never
// dispatches to ServeHTTP's origin-form cases.
func TestHealthIsNotReachableThroughATunnel(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), proxytest.Options{})

	resp, err := h.Client.Get("https://api.anthropic.com" + proxy.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418 from the upstream: a tunnelled request must reach the upstream, never chottag's health handler", resp.StatusCode)
	}
}

// TestAbortedMidBodyIsTraced reproduces the "upstream drops mid-stream" half
// of the mid-body abort: the fake upstream sends one SSE event, then closes
// its connection without finishing the chunked body. The proxy's read from
// upstream therefore fails with a non-EOF error while copying the response
// to the client, ReverseProxy panics with http.ErrAbortHandler, and that
// must still produce exactly one "req" record with the abort noted.
//
// An equivalent client-side trigger (the client hangs up while the upstream
// keeps writing) exercises the identical panic/recover path in forward, but
// deterministically forcing that direction requires racing a TCP RST against
// the upstream's next write, which was flaky in practice; closing the
// upstream's connection mid-chunk is deterministic because net/http's
// chunked-body reader always turns a truncated body into io.ErrUnexpectedEOF,
// regardless of timing.
func TestAbortedMidBodyIsTraced(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}), proxytest.Options{})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Fatalf("line %q err %v", line, err)
	}

	r := h.Records(t, "req", 1)[0]
	if !strings.Contains(r.Err, "aborted") {
		t.Fatalf("record %+v", r)
	}
}

// TestSwapRequiresHTTPS guards against sending a swapped token in cleartext:
// an absolute-form http:// request must never be swapped, even on a
// route class the Chooser would otherwise swap.
func TestSwapRequiresHTTPS(t *testing.T) {
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{Choose: servingSwap{}})
	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(c, "POST http://api.anthropic.com/v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\n\r\n{}", homeTok)
	// The fake upstream is TLS-only, so this plain-HTTP request will likely
	// fail upstream; only the swap flag matters here.
	http.ReadResponse(bufio.NewReader(c), nil)

	r := h.Records(t, "req", 1)[0]
	if r.Swapped {
		t.Fatalf("record %+v", r)
	}
}

// TestOnLogErrorHookCalledOnWriteFailure checks that a trace-write failure
// is reported through Config.OnLogError instead of being silently dropped.
func TestOnLogErrorHookCalledOnWriteFailure(t *testing.T) {
	errCh := make(chan error, 1)
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{
		OnLogError: func(err error) { errCh <- err },
	})
	h.CloseLog()
	do(t, h, "GET", "https://api.anthropic.com/v1/models")

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("OnLogError called with nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnLogError was not called")
	}
}

// TestShapesPreservesRequestBody checks that shaping a JSON request body
// never changes what the upstream actually receives.
func TestShapesPreservesRequestBody(t *testing.T) {
	const body = `{"machine_name":"mbp","nested":{"n":1}}`
	got := make(chan string, 1)
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}), proxytest.Options{Shapes: true})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/environments/bridge", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if g := <-got; g != body {
		t.Fatalf("upstream got %q, want %q", g, body)
	}
}

func TestUntrustedClientHandshakeIsTraced(t *testing.T) {
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{})
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(h.ProxyURL), TLSClientConfig: &tls.Config{}}}
	if _, err := c.Get("https://api.anthropic.com/v1/models"); err == nil {
		t.Fatal("expected TLS failure with an untrusted CA")
	}
	r := h.Records(t, "tunnel", 1)[0]
	if r.Form != "mitm" || !strings.Contains(r.Err, "handshake") {
		t.Fatalf("record %+v", r)
	}
}
