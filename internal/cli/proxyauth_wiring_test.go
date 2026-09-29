package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
)

// TestNewProxyConfigCarriesTheSecret pins the three call sites that build a
// proxy.Config from a loaded secret (proxy.go's newProxyConfig and
// wireProxyConfig, trace.go's traceRunConfig): each must carry the secret
// through to Config.ProxyAuth, or the daemon (or `trace run`) would run
// unauthenticated with the whole suite otherwise green (proxyauth.Secret is
// not comparable — see the ruling — so this uses Equal, never ==).
func TestNewProxyConfigCarriesTheSecret(t *testing.T) {
	s, err := proxyauth.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg := newProxyConfig(nil, s, nil, nil, nil, nil, nil, nil); !cfg.ProxyAuth.Equal(s) {
		t.Fatal("newProxyConfig dropped the proxy secret: the daemon would run unauthenticated")
	}
	var stderr bytes.Buffer
	if cfg := wireProxyConfig(&stderr, nil, s, nil, nil, nil, nil); !cfg.ProxyAuth.Equal(s) {
		t.Fatal("wireProxyConfig dropped the proxy secret")
	}
	if cfg := traceRunConfig(nil, s, nil, nil, false, false, nil); !cfg.ProxyAuth.Equal(s) {
		t.Fatal("traceRunConfig dropped the proxy secret")
	}
}

// TestEnvLinesEvalToTheSecretURLWithoutPrintingIt: the export lines read the
// secret from its file at eval time, so daemon.log and a terminal never
// hold it, and eval'ing them yields the exact URL (and CA path) the shim
// builds. The fake home carries every character dquoteEscaper (or a shell
// itself) treats specially — a double quote, a dollar sign, a backtick, a
// backslash, a literal $(...) substitution attempt and a newline (fix
// round 1 item 3) — none of which unix forbids in a single path component.
func TestEnvLinesEvalToTheSecretURLWithoutPrintingIt(t *testing.T) {
	home := filepath.Join(t.TempDir(), "we\"ird $home`back`\\slash$(true)\nline")
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	lines := envLines(home, "127.0.0.1:50000")
	raw, _ := os.ReadFile(proxyauth.Path(home))
	if strings.Contains(lines, strings.TrimSpace(string(raw))) {
		t.Fatal("envLines printed the secret")
	}
	// \000 (a NUL byte, never valid in a path) separates the two eval'd
	// values so either one containing a literal newline can't be mistaken
	// for a field separator. printf's \NNN octal escape is POSIX; \x00 is
	// not — dash (Ubuntu CI's /bin/sh) prints \x00 as the four literal
	// characters `\x00` instead of a NUL byte, which broke this on Linux
	// (fix round 2 item A).
	out, err := exec.Command("/bin/sh", "-c", lines+`printf '%s\000%s' "$HTTPS_PROXY" "$NODE_EXTRA_CA_CERTS"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	gotProxy, gotCA, ok := strings.Cut(string(out), "\x00")
	if !ok {
		t.Fatalf("eval output = %q, want two NUL-separated fields", out)
	}
	// Fatalf's own %q below must never print gotProxy or the want value
	// unredacted (fix round 2 item A): both carry the real secret as
	// ProxyURL userinfo, and a test failure's output is not exempt from
	// "never print the secret" (Global Constraints).
	if want := s.ProxyURL("127.0.0.1:50000"); gotProxy != want {
		t.Fatalf("eval'd HTTPS_PROXY = %s, want %s", redact.UpstreamProxy(gotProxy), redact.UpstreamProxy(want))
	}
	if want := filepath.Join(home, "ca", "ca.pem"); gotCA != want {
		t.Fatalf("eval'd NODE_EXTRA_CA_CERTS = %q, want %q", gotCA, want)
	}
}

// TestTraceRunSetupCarriesTheSecret pins traceRunSetup's OWN call to
// traceRunConfig (fix round 1 item 1): TestNewProxyConfigCarriesTheSecret
// above only pins traceRunConfig itself, so passing proxyauth.Secret{}
// instead of the secret traceRunSetup just loaded — at this call site —
// left the whole suite green before this test existed.
func TestTraceRunSetupCarriesTheSecret(t *testing.T) {
	home := t.TempDir()
	cfg, err := traceRunSetup(home, filepath.Join(home, "trace.jsonl"), "anthropic.com", false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Log.Close()
	want, err := proxyauth.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProxyAuth.Equal(want) {
		t.Fatal("traceRunSetup dropped the proxy secret: trace run would run unauthenticated")
	}
}

// TestTraceRunSetupRefusesAnInterceptOutsideTheCA pins Ruling 17 directly
// against traceRunSetup, which never calls net.Listen: unlike driving this
// through traceRun itself (whose next step after the check is exactly
// that), a mutation that removed the check could not make this test hang
// (fix round 1 item 2 — the preferred fix over a goroutine/timeout guard).
func TestTraceRunSetupRefusesAnInterceptOutsideTheCA(t *testing.T) {
	home := t.TempDir()
	_, err := traceRunSetup(home, filepath.Join(home, "trace.jsonl"), "anthropic.com,example.com", false, false, nil)
	var ue *traceRunUsageError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "example.com") {
		t.Fatalf("err = %v, want a *traceRunUsageError naming example.com", err)
	}
}

// TestTraceRunSetupRefusesAWildcardOrLeadingDotIntercept pins fix round 1
// item 6: neither proxy.SuffixMatcher nor ca.Permits gives "*" or a
// leading "." any special meaning, so silently accepting either would
// intercept something other than what the flag looks like it asks for,
// instead of refusing outright.
func TestTraceRunSetupRefusesAWildcardOrLeadingDotIntercept(t *testing.T) {
	for _, bad := range []string{".anthropic.com", "*.anthropic.com", "*"} {
		home := t.TempDir()
		_, err := traceRunSetup(home, filepath.Join(home, "trace.jsonl"), bad, false, false, nil)
		var ue *traceRunUsageError
		if !errors.As(err, &ue) || !strings.Contains(err.Error(), "bare domain suffix") {
			t.Fatalf("--intercept %q: err = %v, want a *traceRunUsageError naming a bare domain suffix", bad, err)
		}
	}
}

// TestTraceRunRefusesAnInterceptOutsideTheCA is a traceRun-level regression
// test for Ruling 17 (fix round 2 item B), complementing
// TestTraceRunSetupRefusesAnInterceptOutsideTheCA above (which pins the
// refusal in traceRunSetup itself, never reaching traceRun's own plumbing):
// this drives the actual CLI entry point, so a break in how traceRun maps
// traceRunSetup's error to exit.Usage — or a regression that stops calling
// traceRunSetup before net.Listen at all — can't hide behind the
// lower-level test alone.
//
// Run in a goroutine with a 60s guard (F185: no tight timing bound), not a
// direct synchronous call: if the refusal itself were the thing missing,
// traceRun would fall through to a real net.Listen + srv.Serve that never
// returns, and this test must fail loudly instead of hanging the suite.
func TestTraceRunRefusesAnInterceptOutsideTheCA(t *testing.T) {
	home := t.TempDir()
	var errb bytes.Buffer
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- traceRun(home, []string{"--listen", "127.0.0.1:0", "--intercept", "anthropic.com,example.com"}, &errb)
	}()
	select {
	case code := <-codeCh:
		if code != exit.Usage || !strings.Contains(errb.String(), "example.com") {
			t.Fatalf("code %d stderr %q; want a usage error naming example.com", code, errb.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("traceRun did not return within 60s: did the --intercept refusal stop firing before net.Listen?")
	}
}

// proxyAuthHeaderForTest builds the Proxy-Authorization header value a real
// client would send from secret's ProxyURL userinfo — the wire form of the
// credential net/http's own Transport builds internally from an
// HTTPS_PROXY carrying userinfo. Used by tests across this package that
// speak CONNECT (or an absolute-form request) directly on the socket,
// rather than through net/http's Transport.
func proxyAuthHeaderForTest(t *testing.T, secret proxyauth.Secret) string {
	t.Helper()
	u, err := url.Parse(secret.ProxyURL("x"))
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
}

// rawConnect sends one raw CONNECT request to addr for host, optionally
// with a Proxy-Authorization header, and returns the response's status
// code. It never completes a TLS handshake over the tunnel: reading the
// CONNECT response line is enough to exercise the caller-auth gate this
// test cares about, and closing right after keeps the daemon's own MITM
// handshake attempt (which fails fast on the closed socket) off the
// critical path.
func rawConnect(t *testing.T, addr, host, authHeader string) int {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := "CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n"
	if authHeader != "" {
		req += "Proxy-Authorization: " + authHeader + "\r\n"
	}
	req += "\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestDaemonRequiresTheSecretEndToEnd drives runProxyWithSignal's real
// listener (Ruling 16/F221): an unauthenticated CONNECT to an intercepted
// host is refused, the same CONNECT authenticated with the file secret
// succeeds, the health endpoint's proof verifies against that same secret,
// and the secret itself never lands in daemon.log, proxy.jsonl or stderr.
func TestDaemonRequiresTheSecretEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)

	// proxyDialContextForTest redirects every upstream dial to a local
	// listener, so nothing reaches the network even if the MITM handshake
	// below ever got far enough to need one (it doesn't: the client never
	// sends a ClientHello).
	sink, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	go func() {
		for {
			c, err := sink.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	prevDial := proxyDialContextForTest
	proxyDialContextForTest = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, sink.Addr().String())
	}
	t.Cleanup(func() { proxyDialContextForTest = prevDial })

	out, errb := newSyncBuf(), newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0"}, out, errb, nil, sig)
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
	case <-out.done:
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for the export lines")
	}
	select {
	case <-errb.done:
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for the listening line")
	}
	addr := listenAddrFromStderr(t, errb.String())

	secret, err := proxyauth.Load(home)
	if err != nil {
		t.Fatalf("proxyauth.Load(home) = %v, want the daemon to have created the secret", err)
	}

	// (a) unauthenticated CONNECT to an intercepted host: 407.
	if code := rawConnect(t, addr, "api.anthropic.com:443", ""); code != http.StatusProxyAuthRequired {
		t.Fatalf("unauthenticated CONNECT = %d, want %d", code, http.StatusProxyAuthRequired)
	}

	// (b) the same with Basic chottag:<file secret>: 200.
	if code := rawConnect(t, addr, "api.anthropic.com:443", proxyAuthHeaderForTest(t, secret)); code != http.StatusOK {
		t.Fatalf("authenticated CONNECT = %d, want %d", code, http.StatusOK)
	}

	// (c) the health endpoint's proof verifies against the same secret.
	nonce := proxyauth.NewNonce()
	resp, err := http.Get(fmt.Sprintf("http://%s%s?%s=%s", addr, proxy.HealthPath, proxyauth.NonceParam, nonce))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h proxy.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	if !secret.VerifyProof(port, nonce, h.Proof) {
		t.Fatalf("health proof %q did not verify against the on-disk secret and %d, the port actually probed (Ruling 32)", h.Proof, port)
	}

	// (d) the secret never lands in daemon.log (out/errb here), proxy.jsonl,
	// or stderr — in ANY of its wire forms: the raw hex, the base64 Basic
	// credential a client actually sends, or the full ProxyURL the shim
	// builds (fix round 1 item 5; TestSecretNeverFormats, in
	// internal/proxyauth, already covers every fmt/reflection form of the
	// Secret value itself, so this only needs the forms specific to this
	// package's own wiring).
	raw, err := os.ReadFile(proxyauth.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	hexSecret := strings.TrimSpace(string(raw))
	forms := map[string]string{
		"raw hex secret":            hexSecret,
		"Basic auth (base64)":       base64.StdEncoding.EncodeToString([]byte(proxyauth.User + ":" + hexSecret)),
		"secret.ProxyURL(the addr)": secret.ProxyURL(addr),
	}
	jsonl, _ := os.ReadFile(filepath.Join(home, "proxy.jsonl"))
	for streamName, content := range map[string]string{
		"stdout (daemon.log)": out.String(),
		"stderr (daemon.log)": errb.String(),
		"proxy.jsonl":         string(jsonl),
	} {
		for formName, form := range forms {
			if strings.Contains(content, form) {
				t.Fatalf("%s contained the secret (%s form)", streamName, formName)
			}
		}
	}
}
