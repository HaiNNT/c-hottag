package proxy_test

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// dialDeadline is a hang guard only (F185): every raw dial in this file uses
// it, never a production timing bound.
const dialDeadline = 60 * time.Second

func authHarness(t *testing.T, upstream http.Handler, ch proxy.Chooser) (*proxytest.Harness, proxyauth.Secret, string) {
	t.Helper()
	home := t.TempDir()
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	return proxytest.Start(t, upstream, proxytest.Options{ProxyAuth: s, Choose: ch}), s, secretHex(t, home)
}

// secretHex reads home's raw secret file: a raw-request test builds its own
// Proxy-Authorization header by hand, outside proxyauth's own (deliberately
// non-comparable, non-printable) Secret type.
func secretHex(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(proxyauth.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// rawConnect dials addr, issues a raw CONNECT to target with an optional
// Proxy-Authorization header, and returns the response status and headers.
func rawConnect(t *testing.T, addr, target, proxyAuth string) (int, http.Header) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dialDeadline))

	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if proxyAuth != "" {
		req += "Proxy-Authorization: " + proxyAuth + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

// rawAbsolute dials addr, issues a raw absolute-form GET for target (a full
// URL, any scheme or host), with Proxy-Authorization built from
// proxyURL.User when set, and returns the response status.
func rawAbsolute(t *testing.T, addr string, proxyURL *url.URL, target string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dialDeadline))

	tu, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	req := "GET " + target + " HTTP/1.1\r\nHost: " + tu.Host + "\r\n"
	if proxyURL != nil && proxyURL.User != nil {
		pw, _ := proxyURL.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+pw)) + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getHealth(t *testing.T, u string, hdr http.Header) proxy.Health {
	t.Helper()
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h proxy.Health
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestConnectToInterceptedHostRequiresProxyAuth(t *testing.T) {
	var hits atomic.Int32
	ch := &countingChooser{} // from tunnelhost_test.go (T3)
	h, _, raw := authHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }), ch)
	b64 := func(x string) string { return base64.StdEncoding.EncodeToString([]byte(x)) }

	// A second install's secret: structurally a perfectly valid credential
	// (fix round 1 item 6), so only a value comparison — never merely
	// "looks like Basic chottag:<hex>" — can refuse it.
	otherHome := t.TempDir()
	if _, err := proxyauth.LoadOrCreate(otherHome); err != nil {
		t.Fatal(err)
	}
	otherRaw := secretHex(t, otherHome)

	cases := map[string]string{
		"absent":          "",
		"bearer":          "Bearer " + b64("chottag:"+raw),
		"wrong user":      "Basic " + b64("root:"+raw),
		"newline":         "Basic " + b64("chottag:"+raw+"\n"),
		"bad b64":         "Basic %%%",
		"another install": "Basic " + b64("chottag:"+otherRaw),
	}
	sent := make([]string, 0, len(cases))
	for name, hdr := range cases {
		sent = append(sent, hdr)
		code, respHdr := rawConnect(t, h.ProxyAddr, "api.anthropic.com:443", hdr)
		if code != http.StatusProxyAuthRequired || respHdr.Get("Proxy-Authenticate") != `Basic realm="chottag"` {
			t.Errorf("%s: status %d, Proxy-Authenticate %q; want 407 with the challenge", name, code, respHdr.Get("Proxy-Authenticate"))
		}
	}
	if code, _ := rawConnect(t, h.ProxyAddr, "api.anthropic.com:443", "basic "+b64("chottag:"+raw)); code != http.StatusOK {
		t.Errorf("right credential (lower-case scheme): status %d, want 200", code)
	}
	if hits.Load() != 0 || ch.n.Load() != 0 {
		t.Error("a refused CONNECT reached upstream or Choose")
	}
	// Sweep every header value actually sent (fix round 1 item 6), not only
	// the right one: a partial redaction that happened to catch just the
	// literal right credential would still leak, say, the OTHER install's
	// secret unnoticed.
	for _, hdr := range sent {
		if hdr != "" && h.LogContains(t, hdr) {
			t.Fatalf("a sent Proxy-Authorization value reached proxy.jsonl: %q", hdr)
		}
	}
	if h.LogContains(t, raw) || h.LogContains(t, otherRaw) {
		t.Fatal("a proxy secret reached proxy.jsonl")
	}
	// The one successful CONNECT above leaves its own "tunnel" record too
	// (mitm's client TLS handshake fails once rawConnect closes the raw
	// socket without ever sending a ClientHello) — Status 0, not 407, so it
	// is filtered out here rather than counted against the refusals.
	recs := h.Records(t, "tunnel", len(cases))
	refused := 0
	for _, r := range recs {
		if r.Status != http.StatusProxyAuthRequired {
			continue
		}
		refused++
		if r.Host != "api.anthropic.com:443" && r.Host != "api.anthropic.com" {
			t.Errorf("407 record host = %+v", r)
		}
	}
	if refused != len(cases) {
		t.Errorf("got %d refused-tunnel records, want %d: %+v", refused, len(cases), recs)
	}
}

// TestRefusedConnectNeverHijacksTheConnection proves more than "hits and
// Choose stayed at zero" (fix round 1 item 4): that assert is vacuous for a
// raw CONNECT, because this test never sends any TLS bytes either way, so
// neither counter could ever fire regardless of whether the server hijacked
// the connection first. Instead it attempts a real TLS handshake for the
// CONNECT target on the SAME connection right after the 407: a hijacked and
// served mitm tunnel would complete that handshake (ca.ServerTLSConfig
// would mint it a leaf), so the handshake must fail.
func TestRefusedConnectNeverHijacksTheConnection(t *testing.T) {
	h, _, _ := authHarness(t, http.NotFoundHandler(), nil)
	conn, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dialDeadline))

	target := "api.anthropic.com:443"
	if _, err := io.WriteString(conn, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status %d, want 407", resp.StatusCode)
	}

	tlsConn := tls.Client(conn, &tls.Config{ServerName: "api.anthropic.com", InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err == nil {
		t.Fatal("TLS handshake succeeded on a refused CONNECT: the connection was hijacked before the 407")
	}
}

func TestAuthorisedClientStillSwapsAndStripsTheHeader(t *testing.T) {
	var sawProxyAuth atomic.Bool
	var sawSwapped atomic.Bool
	ch := &countingChooser{} // from tunnelhost_test.go (T3): swaps to "sk-ant-oat01-SWAPPED"
	h, _, _ := authHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			sawProxyAuth.Store(true)
		}
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat01-SWAPPED" {
			sawSwapped.Store(true)
		}
	}), ch)

	req, err := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-HOME") // oauth-shaped: forward() only swaps this kind
	resp, err := h.Client.Do(req)                               // h.ProxyURL carries the userinfo
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !sawSwapped.Load() || ch.n.Load() == 0 {
		t.Fatalf("authorised MITM request: status %d, swapped %v, Choose calls %d", resp.StatusCode, sawSwapped.Load(), ch.n.Load())
	}

	// Absolute-form: plaintext to the proxy, https URL; Go sends it when
	// the request URL is http, so build it by hand.
	sawProxyAuth.Store(false)
	code := rawAbsolute(t, h.ProxyAddr, h.ProxyURL, "https://api.anthropic.com/v1/models")
	if code != 200 || sawProxyAuth.Load() {
		t.Fatalf("absolute-form with auth: %d, upstream saw Proxy-Authorization=%v", code, sawProxyAuth.Load())
	}
	if code := rawAbsolute(t, h.ProxyAddr, h.PlainProxyURL, "https://api.anthropic.com/v1/models"); code != http.StatusProxyAuthRequired {
		t.Fatalf("absolute-form without auth: %d, want 407", code)
	}
}

// TestAbsoluteFormAlwaysNeedsAuthWhateverTheHostOrScheme pins Ruling 2: a
// mutation that narrowed the absolute-form gate to intercepted hosts, or to
// https only, would let one of these three through (fix round 1 item 1).
func TestAbsoluteFormAlwaysNeedsAuthWhateverTheHostOrScheme(t *testing.T) {
	h, _, raw := authHarness(t, http.NotFoundHandler(), nil)
	targets := []string{
		"http://example.com/",
		"https://echo.example.org/",
		"http://" + h.ProxyAddr + proxy.HealthPath,
	}
	for _, target := range targets {
		if code := rawAbsolute(t, h.ProxyAddr, nil, target); code != http.StatusProxyAuthRequired {
			t.Errorf("%s without auth: %d, want 407", target, code)
		}
	}
	recs := h.Records(t, "req", len(targets))
	for _, r := range recs {
		if r.Form != "absolute" || r.Status != http.StatusProxyAuthRequired {
			t.Errorf("record = %+v, want Form absolute Status 407", r)
		}
	}
	if h.LogContains(t, raw) {
		t.Fatal("the proxy secret reached proxy.jsonl")
	}
}

func TestBlindTunnelNeedsNoAuth(t *testing.T) {
	h, _, _ := authHarness(t, http.NotFoundHandler(), nil)
	if code, _ := rawConnect(t, h.ProxyAddr, "echo.example.org:443", ""); code != http.StatusOK {
		t.Fatalf("CONNECT to a non-intercepted host without auth: %d, want 200 (Ruling 1)", code)
	}
}

// TestBlindTunnelNeedsAuthWhenChainedWithCredentials pins plan Ruling 26
// (amends Ruling 1): chottag's own UpstreamProxy carries its own credentials
// here (the corporate proxy's, not chottag's), so a blind CONNECT must not let
// an unauthenticated local caller ride them.
func TestBlindTunnelNeedsAuthWhenChainedWithCredentials(t *testing.T) {
	var connects atomic.Int64
	up := upstreamProxy(t, &connects, nil, nil) // from upstreamproxy_test.go (part 1 upstream-proxy work)
	up.User = url.User("bob")

	home := t.TempDir()
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	raw := secretHex(t, home)

	// A plain TCP echo server standing in for a non-intercepted host.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{UpstreamProxy: up, ProxyAuth: s})

	if code, _ := rawConnect(t, h.ProxyAddr, ln.Addr().String(), ""); code != http.StatusProxyAuthRequired {
		t.Fatalf("blind CONNECT with no caller auth through a credentialed upstream proxy: %d, want 407", code)
	}
	if connects.Load() != 0 {
		t.Error("a refused blind CONNECT still reached the credentialed upstream proxy")
	}

	b64Auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("chottag:"+raw))
	if code, _ := rawConnect(t, h.ProxyAddr, ln.Addr().String(), b64Auth); code != http.StatusOK {
		t.Fatalf("blind CONNECT with the right caller credential: %d, want 200", code)
	}
	if connects.Load() == 0 {
		t.Error("an authorised blind CONNECT never reached the upstream proxy")
	}
}

// A daemon with caller auth on accepts session credentials and says so; one
// with auth off has no credential to hand out (F255).
func TestHealthAdvertisesSessionsOnlyWithCallerAuth(t *testing.T) {
	h, _, _ := authHarness(t, http.NotFoundHandler(), nil)
	if !getHealth(t, "http://"+h.ProxyAddr+proxy.HealthPath, nil).Sessions {
		t.Fatal("a daemon with caller auth on must report sessions: true")
	}
	off := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{})
	if getHealth(t, "http://"+off.ProxyAddr+proxy.HealthPath, nil).Sessions {
		t.Fatal("a daemon with caller auth off must not report sessions")
	}
}

func TestHealthProofOnlyForAValidNonce(t *testing.T) {
	h, s, _ := authHarness(t, http.NotFoundHandler(), nil)
	n := proxyauth.NewNonce()
	got := getHealth(t, "http://"+h.ProxyAddr+proxy.HealthPath+"?nonce="+n, nil)
	if !s.VerifyProof(mustAddrPort(t, h.ProxyAddr), n, got.Proof) {
		t.Fatal("health did not prove the secret for a valid nonce and the port it was probed on (Ruling 32)")
	}
	for _, bad := range []string{"", "../x", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32)} {
		if p := getHealth(t, "http://"+h.ProxyAddr+proxy.HealthPath+"?nonce="+url.QueryEscape(bad), nil).Proof; p != "" {
			t.Errorf("nonce %q got proof %q: health must not be an HMAC oracle", bad, p)
		}
	}
}

// TestHealthProofDoesNotVerifyAgainstAnotherPort is Ruling 32's own relay
// test (final review M2, the parked "bind the proof to the listen address"
// item): p and q are two real, independently-listening proxy servers that
// share one secret — exactly a stopped daemon's old port and a same-secret
// `trace run` or leftover daemon on another. q's own proof for a nonce must
// not verify against p's port: without the port binding, a local squatter
// on p could relay the shim's nonce to q and hand back q's proof as if it
// were its own.
func TestHealthProofDoesNotVerifyAgainstAnotherPort(t *testing.T) {
	home := t.TempDir()
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	p := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{ProxyAuth: s})
	q := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{ProxyAuth: s})

	n := proxyauth.NewNonce()
	gotQ := getHealth(t, "http://"+q.ProxyAddr+proxy.HealthPath+"?nonce="+n, nil)
	if !s.VerifyProof(mustAddrPort(t, q.ProxyAddr), n, gotQ.Proof) {
		t.Fatal("q's own proof did not verify against q's own port")
	}
	if s.VerifyProof(mustAddrPort(t, p.ProxyAddr), n, gotQ.Proof) {
		t.Fatal("q's proof verified against p's port: the health proof must bind the listening port (Ruling 32)")
	}
}

// TestHealthProofPortComesFromTheConnectionNotTheHostHeader pins NEW-1
// (final re-review, probe r1): serveHealth must take the port it binds
// into the proof from the accepted connection's own local address
// (http.LocalAddrContextKey), never from the client-controlled Host
// header. The request actually arrives on q's real listening port, but
// its Host header claims to be p's — loopbackHost accepts any port for a
// loopback name, so that alone doesn't get refused. If serveHealth ever
// read the port out of r.Host instead, this would let exactly the relay
// M2/Ruling 32 closes back in: a squatter on p could spoof Host: p's-port
// while actually talking to q and walk away with a proof valid for p.
func TestHealthProofPortComesFromTheConnectionNotTheHostHeader(t *testing.T) {
	home := t.TempDir()
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	p := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{ProxyAuth: s})
	q := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{ProxyAuth: s})

	n := proxyauth.NewNonce()
	req, err := http.NewRequest("GET", "http://"+q.ProxyAddr+proxy.HealthPath+"?nonce="+n, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = p.ProxyAddr // spoofed: the TCP connection still goes to q
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET with a spoofed loopback Host = %d, want 200 (loopbackHost accepts any port)", resp.StatusCode)
	}
	var h proxy.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}

	if !s.VerifyProof(mustAddrPort(t, q.ProxyAddr), n, h.Proof) {
		t.Fatal("the proof did not verify against q's real listening port, even though the request actually arrived there")
	}
	if s.VerifyProof(mustAddrPort(t, p.ProxyAddr), n, h.Proof) {
		t.Fatal("the proof verified against p's port, taken from the spoofed Host header: it must come from the connection (LocalAddrContextKey) instead")
	}
}

// mustAddrPort splits the port out of a "host:port" listen address, the
// shape proxytest.Harness.ProxyAddr and internal/shim's own VerifyHealth
// both use.
func mustAddrPort(t *testing.T, hostport string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHealthRefusesNonLoopbackHostAndBrowsers(t *testing.T) {
	h, _, _ := authHarness(t, http.NotFoundHandler(), nil)
	for _, c := range []struct {
		host, origin string
		want         int
	}{
		{"", "", 200}, // 127.0.0.1:port as dialled
		{"localhost:47821", "", 200},
		{"[::1]:47821", "", 200},
		{"127.0.0.2", "", 200},
		{"evil.example:47821", "", 403}, // DNS rebinding
		{"10.0.0.1:1", "", 403},         // a routable private IP, not loopback
		{"0.0.0.0:1", "", 403},          // unspecified, not loopback
		{"", "https://evil.example", 403},
		{"", "null", 403}, // the Origin a sandboxed/file:// page sends
	} {
		req, _ := http.NewRequest("GET", "http://"+h.ProxyAddr+proxy.HealthPath, nil)
		if c.host != "" {
			req.Host = c.host
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("Host %q Origin %q: %d, want %d", c.host, c.origin, resp.StatusCode, c.want)
		}
	}
}

// TestHealthRefusesHTTP10WithNoHostHeader covers a shape http.NewRequest
// cannot produce at all (fix round 1 item 5): HTTP/1.0 never requires a
// Host header, so a raw client can omit it entirely. loopbackHost("") must
// fail closed, not treat a missing Host as "trust it".
func TestHealthRefusesHTTP10WithNoHostHeader(t *testing.T) {
	h, _, _ := authHarness(t, http.NotFoundHandler(), nil)
	conn, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(dialDeadline))
	if _, err := io.WriteString(conn, "GET "+proxy.HealthPath+" HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HTTP/1.0 with no Host header: %d, want 403", resp.StatusCode)
	}
}

func TestNoProxyAuthMeansNoGate(t *testing.T) {
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{})
	if code, _ := rawConnect(t, h.ProxyAddr, "api.anthropic.com:443", ""); code != 200 {
		t.Fatalf("zero ProxyAuth must not gate: %d", code)
	}

	// A zero secret means auth is off everywhere (fix round 2, N1), the
	// chained-blind-tunnel check (plan Ruling 26) included: chaining
	// through a credentialed upstream proxy must not turn into a
	// permanently-refused blind tunnel just because ProxyAuth was never set.
	var connects atomic.Int64
	up := upstreamProxy(t, &connects, nil, nil) // from upstreamproxy_test.go
	up.User = url.User("bob")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	h2 := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{UpstreamProxy: up})
	if code, _ := rawConnect(t, h2.ProxyAddr, ln.Addr().String(), ""); code != 200 {
		t.Fatalf("zero ProxyAuth must not gate a blind tunnel chained through a credentialed upstream proxy either: %d", code)
	}
}
