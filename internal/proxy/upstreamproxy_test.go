package proxy_test

import (
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// upstreamProxy is a minimal CONNECT proxy that counts the tunnels it is
// asked for, so a test can prove traffic actually went through it.
//
// redirect, when non-nil, is consulted for the literal dial target and can
// substitute a different address: once Transport.Proxy has taken over, Go's
// own transport tunnels straight through to the real hostname without ever
// calling chottag's test DialContext hook again, so a test that wants the
// far end of the tunnel to land on a local mock (rather than a real,
// publicly-routable host whose certificate the harness's restricted CA pool
// can never validate) has to redirect it here instead.
//
// onConnect, when non-nil, is handed the incoming CONNECT request before
// this proxy dials anywhere — the only place a test can inspect what
// chottag actually sent it (e.g. Proxy-Authorization).
func upstreamProxy(t *testing.T, connects *atomic.Int64, redirect func(hostport string) string, onConnect func(*http.Request)) *url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		if onConnect != nil {
			onConnect(r)
		}
		target := r.Host
		if redirect != nil {
			if addr := redirect(r.Host); addr != "" {
				target = addr
			}
		}
		up, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			up.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			up.Close()
			return
		}
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			client.Close()
			up.Close()
			return
		}
		go func() { io.Copy(up, client); up.Close() }()
		go func() { io.Copy(client, up); client.Close() }()
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	u, err := url.Parse("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestForwardingGoesThroughTheUpstreamProxy(t *testing.T) {
	var connects atomic.Int64
	// mockAddr is filled in once the harness (and its fake Anthropic mock)
	// exists; the CONNECT proxy's handler only reads it after the request
	// below fires, but it is set via an atomic.Pointer rather than a plain
	// variable so -race has no doubt about the ordering.
	var mockAddr atomic.Pointer[string]
	up := upstreamProxy(t, &connects, func(hostport string) string {
		// A future test wiring another hostname through this proxy must fail
		// loudly here, not silently fall back to a real dial — that's the
		// exact defect this redirect hook exists to prevent (see its doc
		// comment). "127.0.0.1:1" refuses instantly and locally: it can
		// never accidentally reach a real host.
		if hostport != "api.anthropic.com:443" {
			t.Errorf("upstream proxy asked to CONNECT to unexpected host %q", hostport)
			return "127.0.0.1:1"
		}
		a := mockAddr.Load()
		if a == nil {
			t.Errorf("upstream proxy asked to CONNECT before the mock address was known")
			return "127.0.0.1:1"
		}
		return *a
	}, nil)

	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}), proxytest.Options{UpstreamProxy: up})
	mockAddr.Store(&h.UpstreamAddr)

	resp, err := h.Client.Get("https://api.anthropic.com/v1/mcp_servers")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := connects.Load(); got == 0 {
		t.Error("the upstream proxy saw no CONNECT: chottag went direct")
	}
}

func TestBlindTunnelGoesThroughTheUpstreamProxy(t *testing.T) {
	var connects atomic.Int64
	up := upstreamProxy(t, &connects, nil, nil)

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

	h := proxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		proxytest.Options{UpstreamProxy: up})

	// CONNECT to the echo server through chottag. It is not an Anthropic
	// host, so chottag tunnels it blind.
	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "CONNECT "+ln.Addr().String()+" HTTP/1.1\r\nHost: "+ln.Addr().String()+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "200") {
		t.Fatalf("CONNECT response = %q, want a 200", got)
	}
	if got := connects.Load(); got == 0 {
		t.Error("the upstream proxy saw no CONNECT: the blind tunnel went direct")
	}
}

// hungUpstreamProxyListener starts a TCP listener that accepts one
// connection and reads (but never answers) whatever CONNECT request lands
// on it, then hands the connection back over the returned channel.
//
// Signalling only after that read succeeds matters: Accept alone only
// proves the TCP handshake landed, not that dialThroughProxy has actually
// reached http.ReadResponse. The read cannot return anything until
// dialThroughProxy's req.Write has put bytes on the wire, and req.Write
// happens strictly after s.conns.track (see dialThroughProxy's doc
// comment) — so a caller that waits for the accepted conn here is
// guaranteed the connection is both tracked and genuinely parked in
// ReadResponse, not merely mid-dial. Signalling on Accept alone would race
// CloseUpstreams against the very tracking a test asserts on.
func hungUpstreamProxyListener(t *testing.T) (*url.URL, <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 4096)
		if _, err := c.Read(buf); err != nil {
			c.Close()
			return
		}
		accepted <- c
	}()
	u, err := url.Parse("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return u, accepted
}

// TestHungUpstreamProxyStaysReachableByCloseUpstreams proves that an
// upstream proxy which accepts the TCP connection and then never answers
// the CONNECT does not leave that socket outside s.conns. Without tracking
// the connection before the handshake (not after), dialThroughProxy would
// block forever on http.ReadResponse and CloseUpstreams would have nothing
// to reach.
func TestHungUpstreamProxyStaysReachableByCloseUpstreams(t *testing.T) {
	up, accepted := hungUpstreamProxyListener(t)

	h := proxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		proxytest.Options{UpstreamProxy: up})

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "CONNECT "+target.Addr().String()+" HTTP/1.1\r\nHost: "+target.Addr().String()+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case hungConn := <-accepted:
		defer hungConn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the hung upstream proxy never even saw the CONNECT")
	}

	// The dial's handler goroutine is now genuinely parked inside
	// http.ReadResponse, waiting on a proxy that will never answer.
	// CloseUpstreams must still reach that socket.
	if n := h.Server.CloseUpstreams(); n == 0 {
		t.Error("CloseUpstreams closed nothing: the hung dial's socket was never tracked")
	}

	// Closing it should unblock the handler: blind() surfaces the failed
	// dial as a real 502 (or the client just sees the connection go away),
	// but either way the read must not simply time out.
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	if _, err := c.Read(buf); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the blind-tunnel handler is still hung: CloseUpstreams did not unblock it")
	}
}

// TestClientHangupCancelsHungUpstreamProxyDial isolates the ctx-cancellation
// half of dialThroughProxy from the tracking half above: CloseUpstreams is
// deliberately never called anywhere in this test, so nothing except ctx —
// cancelled when the client that owns this tunnel attempt hangs up — can
// free a handshake stuck in http.ReadResponse. Deleting the ctx-watcher
// goroutine in dialThroughProxy should make this test hang until its own
// deadline, every time.
func TestClientHangupCancelsHungUpstreamProxyDial(t *testing.T) {
	up, accepted := hungUpstreamProxyListener(t)

	h := proxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		proxytest.Options{UpstreamProxy: up})

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "CONNECT "+target.Addr().String()+" HTTP/1.1\r\nHost: "+target.Addr().String()+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	var upstreamConn net.Conn
	select {
	case upstreamConn = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the hung upstream proxy never even saw the CONNECT")
	}
	defer upstreamConn.Close()

	// The client hangs up mid-handshake. chottag's own request context is
	// cancelled as a result.
	c.Close()

	// If the ctx watcher fired, chottag closed its side of the tunnel to
	// the (still silent) upstream proxy, and this read observes that as
	// EOF/reset. Without the watcher, nothing ever closes that socket —
	// CloseUpstreams is never called in this test — so this read simply
	// times out.
	upstreamConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := upstreamConn.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("chottag never closed its side of the tunnel after the client hung up: the ctx watcher did not fire")
	}
	if err == nil {
		t.Fatalf("the hung upstream proxy unexpectedly received %d bytes", n)
	}
}

// TestBlindTunnelSendsProxyAuthorizationHeader proves the blind path sends
// Proxy-Authorization for a username-only proxy URL (http://bob@proxy),
// matching what Transport.Proxy already sends on the forwarding path. The
// header's value is never logged or printed, only compared.
func TestBlindTunnelSendsProxyAuthorizationHeader(t *testing.T) {
	var connects atomic.Int64
	var gotAuth atomic.Pointer[string]
	up := upstreamProxy(t, &connects, nil, func(r *http.Request) {
		v := r.Header.Get("Proxy-Authorization")
		gotAuth.Store(&v)
	})
	up.User = url.User("bob")

	// up now carries its own userinfo, so this blind tunnel needs the
	// CALLER's auth to chottag too (plan Ruling 26, amends Ruling 1) —
	// distinct from "bob", which is what chottag sends ON to up, never what
	// a caller sends to chottag.
	home := t.TempDir()
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	callerAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("chottag:"+secretHex(t, home)))

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

	h := proxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		proxytest.Options{UpstreamProxy: up, ProxyAuth: s})

	c, err := net.Dial("tcp", h.ProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "CONNECT "+ln.Addr().String()+" HTTP/1.1\r\nHost: "+ln.Addr().String()+"\r\nProxy-Authorization: "+callerAuth+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "200") {
		t.Fatal("CONNECT did not succeed")
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:"))
	got := gotAuth.Load()
	if got == nil {
		t.Fatal("upstream proxy never saw a CONNECT")
	}
	if *got == "" {
		t.Error("Proxy-Authorization header was not sent for a proxy URL carrying a username")
	} else if *got != want {
		// Deliberately not printing either value: both are Basic-auth
		// material, even for a throwaway test credential.
		t.Error("Proxy-Authorization header did not match the value net/http would send for this proxy URL")
	}
}
