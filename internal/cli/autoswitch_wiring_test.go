package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// Item 3 (review round 2): runProxyWithSignal's two auto-switch wiring
// lines — `cfg.WallRetry = as.wallRetry` and wrapping the usage hook in
// `autoUsageHook(as, ...)` — have no test that actually drives a real
// proxied round trip through them. Dropping either line leaves the rest of
// the package green, since every existing auto-switch daemon test either
// injects usage straight into status.json and drives the roster tick
// (TestRunProxyWiresAutoSwitch) or calls the daemon's own hooks directly
// (autoswitch_daemon_test.go), never through proxy.Config's OnUsage/
// WallRetry fields as the real proxy.Server would call them.
//
// The two tests below send a genuine HTTPS request through
// runProxyWithSignal's own listener (CONNECT, then a TLS handshake with
// the daemon's real MITM leaf, verified against the same CA
// runProxyWithSignal itself just created under CHOTTAG_HOME/ca), redirected
// to a local fake upstream via the test-only proxyDialContextForTest/
// proxyUpstreamRootCAsForTest hooks (proxy.go), with a fabricated but
// genuinely valid credential via credsReadForTest so the daemon's REAL
// chooser actually swaps a bearer — without ever touching the real
// network, the real macOS Keychain, or a real credentials file.

// startFakeUpstream starts a local TLS server standing in for
// api.anthropic.com, signed by a fresh, throwaway CA, and returns it with
// that CA so the caller can trust it via proxyUpstreamRootCAsForTest.
func startFakeUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *ca.Authority) {
	t.Helper()
	upCA, err := ca.LoadOrCreate(filepath.Join(t.TempDir(), "upstream-ca"))
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewUnstartedServer(handler)
	up.TLS = upCA.ServerTLSConfig("api.anthropic.com")
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.StartTLS()
	t.Cleanup(up.Close)
	return up, upCA
}

// wireFakeUpstream redirects every upstream dial to up and trusts upCA, and
// hands the daemon's real token manager a fabricated, already-valid token
// for whichever slot it asks about (derived from the slot dir's base name,
// e.g. "A" or "B"). All three seams are restored in t.Cleanup.
func wireFakeUpstream(t *testing.T, up *httptest.Server, upCA *ca.Authority) {
	t.Helper()
	prevDial, prevRoots, prevRead := proxyDialContextForTest, proxyUpstreamRootCAsForTest, credsReadForTest
	t.Cleanup(func() {
		proxyDialContextForTest, proxyUpstreamRootCAsForTest, credsReadForTest = prevDial, prevRoots, prevRead
	})
	proxyDialContextForTest = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, up.Listener.Addr().String())
	}
	proxyUpstreamRootCAsForTest = upCA.Pool()
	credsReadForTest = func(configDir string) (creds.Token, error) {
		return creds.Token{AccessToken: "tok-" + filepath.Base(configDir), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
}

// mitmConn establishes chottag's own CONNECT + TLS handshake to
// api.anthropic.com through addr (runProxyWithSignal's real listener),
// verifying the leaf mitm() presents against the same CA
// runProxyWithSignal itself created under home/ca. The CONNECT carries the
// real on-disk install secret (F221/T10): runProxyWithSignal's daemon
// gates every intercepted CONNECT on it now, exactly as claude's own
// HTTPS_PROXY would.
func mitmConn(t *testing.T, home, addr string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	secret, err := proxyauth.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return mitmConnAs(t, home, addr, proxyAuthHeaderForTest(t, secret))
}

// mitmConnAs is mitmConn with the caller's own Proxy-Authorization value
// (a session credential, say).
func mitmConnAs(t *testing.T, home, addr, authHeader string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	proxyCA, err := ca.LoadOrCreate(filepath.Join(home, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nProxy-Authorization: "+authHeader+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("reading the CONNECT response failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to the intercepted host returned %d, want 200", resp.StatusCode)
	}
	tlsConn := tls.Client(c, &tls.Config{RootCAs: proxyCA.Pool(), ServerName: "api.anthropic.com"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake over the MITM tunnel failed: %v", err)
	}
	return tlsConn, bufio.NewReader(tlsConn)
}

// listenAddrFromStderr extracts the resolved listen address from
// runProxyWithSignal's startup line, the same way every other full daemon
// test in this package does (proxy_run_test.go).
func listenAddrFromStderr(t *testing.T, s string) string {
	t.Helper()
	const marker = "chottag proxy listening on "
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("stderr = %q, want it to name the listen address", s)
	}
	rest := s[i+len(marker):]
	return rest[:strings.IndexByte(rest, ',')]
}

// TestRunProxyWiresTheUsageHookThroughARealProxiedResponse is item 3(a): a
// proxied response carrying usage headers over A's switch point switches
// serving to B, through runProxyWithSignal's own OnUsage wiring
// (autoUsageHook), on a genuine round trip — not a status.json injection.
func TestRunProxyWiresTheUsageHookThroughARealProxiedResponse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	addSlotAccount(t, home, "B")

	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.97")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusOK)
	})
	wireFakeUpstream(t, up, upCA)

	rec := stubDaemonNotifier(t)
	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})
	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	addr := listenAddrFromStderr(t, errb.String())

	tlsConn, br := mitmConn(t, home, addr)
	if _, err := io.WriteString(tlsConn, "GET /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-oat01-client-owned\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the proxied response failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied response status = %d, want 200", resp.StatusCode)
	}

	// OnUsage runs synchronously inside ModifyResponse (proxy/forward.go
	// ~144), before the reverse proxy writes anything back to the client —
	// so the switch is already durable the moment ReadResponse above
	// returns. Asserting serving == "B" right here, with no wait at all, is
	// what actually kills the mutation this test exists for — unwrapping
	// autoUsageHook so the usage hook itself never runs — without tying the
	// kill to the 5s roster tick (proxy.go's rosterTickC), which also runs
	// a.evaluate and would eventually notice A's 97% usage and switch on
	// its own regardless of whether the hook is wired (F185, item 4, review
	// round 3): a wait long enough to let the tick fire would let that
	// mutation hide behind it.
	st, err := (store.Store{Dir: home}).Load()
	if err != nil || st.Serving != "B" {
		t.Fatalf("serving = %q (%v), want B immediately after the response was read", st.Serving, err)
	}
	// The notice is posted from a goroutine and is not on the synchronous
	// path above, so it gets a generous bound instead — 10s, well clear of
	// the 5s tick, since this assertion only wants confirmation the notice
	// eventually arrives, not to distinguish the hook from the tick.
	select {
	case m := <-rec.got:
		if m.title != "chottag: switched to B" {
			t.Fatalf("first notice = %+v, want the switch to B", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no switch notice arrived; stderr %q", errb.String())
	}
}

// TestRunProxyWiresTheWallRetryThroughARealProxied429 is item 3(b): a
// usage-limit 429 on A is resent on B through runProxyWithSignal's own
// WallRetry wiring (cfg.WallRetry = as.wallRetry), and the client sees B's
// 2xx — a genuine round trip, with exactly two upstream calls.
func TestRunProxyWiresTheWallRetryThroughARealProxied429(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	addSlotAccount(t, home, "B")

	reset := time.Now().Add(3 * time.Hour).Unix()
	var calls atomic.Int32
	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.0")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	wireFakeUpstream(t, up, upCA)

	rec := stubDaemonNotifier(t)
	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})
	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	addr := listenAddrFromStderr(t, errb.String())

	tlsConn, br := mitmConn(t, home, addr)
	if _, err := io.WriteString(tlsConn, "GET /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-oat01-client-owned\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the proxied response failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client-visible status = %d, want 200: the wall retry must resend on B and hand the client its 2xx", resp.StatusCode)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (the original 429 on A, then the resend on B)", n)
	}

	select {
	case m := <-rec.got:
		if m.title != "chottag: switched to B" {
			t.Fatalf("first notice = %+v, want the switch to B", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the wall retry never switched; stderr %q", errb.String())
	}
	st, err := (store.Store{Dir: home}).Load()
	if err != nil || st.Serving != "B" {
		t.Fatalf("serving = %q (%v), want B", st.Serving, err)
	}
	if !strings.Contains(errb.String(), "chottag: wall retry A -> B 200 (limit 5h)") {
		t.Fatalf("stderr = %q, want the wall-retry daemon.log line", errb.String())
	}
}

// R158: runProxyWithSignal wires proxy.Config.OnServingRefusal. A request
// swapped onto A that A's login refuses twice (401), then goes out on the
// client's own login, writes the daemon.log line and posts the notice.
// Dropping `cfg.OnServingRefusal = ...` leaves both missing.
func TestRunProxyWiresTheServingRefusalHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")

	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-client-owned" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	wireFakeUpstream(t, up, upCA)
	// The safety net's forced refresh runs the slot's claude: a stand-in
	// that renews nothing, never the real binary.
	fakeClaude := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fakeClaude, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	rec := stubDaemonNotifier(t)
	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", "", "--claude", fakeClaude}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})
	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	addr := listenAddrFromStderr(t, errb.String())

	tlsConn, br := mitmConn(t, home, addr)
	if _, err := io.WriteString(tlsConn, "GET /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-oat01-client-owned\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the proxied response failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client saw %d, want 200 (the refusal must not reach it)", resp.StatusCode)
	}
	// The hook runs on the request goroutine before the resend, so the log
	// line is already written.
	const line = "chottag: A's login was refused (401) on GET /v1/messages; sent on Home's own login"
	if !strings.Contains(errb.String(), line) {
		t.Fatalf("stderr %q lacks %q", errb.String(), line)
	}
	select {
	case m := <-rec.got:
		if m.title != "chottag: A's login was refused (401)" {
			t.Fatalf("first notice = %+v, want the serving-refusal notice", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no serving-refusal notice arrived; stderr %q", errb.String())
	}
}
