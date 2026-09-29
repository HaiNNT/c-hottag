package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	hostport := r.Host
	// intercepted is computed once and decides both "does this CONNECT need
	// caller auth" and "mitm or blind" (fix round 1 item 3): ServeHTTP used
	// to compute it a second time just for the gate, which could drift from
	// this one.
	if intercepted := s.cfg.Intercept != nil && s.cfg.Intercept(router.HostOnly(hostport)); intercepted {
		if s.gated() && !s.authorized(r) {
			s.requireAuth(w, tracelog.Record{Kind: "tunnel", Form: "mitm", Host: hostport})
			return
		}
		s.mitm(w, hostport)
		return
	}
	// A blind tunnel chained through an upstream proxy that carries its own
	// credentials (plan Ruling 26, amends Ruling 1) lets any local caller
	// egress under those credentials without ever seeing them, so it needs
	// the same caller auth an intercepted CONNECT gets — but only while
	// caller auth is actually on: s.gated() here matches every other path,
	// so a zero secret means auth off everywhere, this one included, not a
	// permanently-refused blind tunnel (fix round 2, N1). chained is
	// likewise computed once and used for that single decision.
	if chained := s.chainedWithCredentials(); s.gated() && chained && !s.authorized(r) {
		s.requireAuth(w, tracelog.Record{Kind: "tunnel", Form: "blind", Host: hostport})
		return
	}
	s.blind(w, r, hostport)
}

// chainedWithCredentials reports whether cfg.UpstreamProxy carries its own
// userinfo: see its doc comment (plan Ruling 26, amends Ruling 1).
func (s *Server) chainedWithCredentials() bool {
	return s.cfg.UpstreamProxy != nil && s.cfg.UpstreamProxy.User != nil
}

// blind dials upstream before accepting the tunnel, so an unreachable host
// looks to the client like a failed connection, not a dead tunnel.
func (s *Server) blind(w http.ResponseWriter, r *http.Request, hostport string) {
	start := time.Now()
	// A blind tunnel's one record is written when it closes, which may be
	// hours later: the switch is read now, at its start (M2c spec §3).
	traced := s.tracing()
	up, err := s.dialUpstream(r.Context(), hostport)
	if err != nil {
		s.emit(tracelog.Record{T: start.UTC(), Kind: "tunnel", Form: "blind", Host: hostport, Status: http.StatusBadGateway, Err: err.Error()}, traced)
		http.Error(w, "chottag: upstream unreachable", http.StatusBadGateway)
		return
	}
	client, err := hijack(w)
	if err != nil {
		up.Close()
		return
	}
	pipe(client, up)
	s.emit(tracelog.Record{T: start.UTC(), Kind: "tunnel", Form: "blind", Host: hostport, Status: http.StatusOK, Millis: time.Since(start).Milliseconds()}, traced)
}

// dialThroughProxy opens a tunnel to hostport via an upstream HTTP proxy,
// by dialling the proxy and issuing our own CONNECT. Transport.Proxy does
// this for the forwarding path; a blind tunnel bypasses the transport
// entirely, so it has to do it itself.
//
// The connection is tracked (registered with s.conns) as soon as the dial
// succeeds — before the CONNECT handshake below, not after. An upstream
// proxy that accepts the TCP connection and then never answers the CONNECT
// must still be reachable by CloseUpstreams (spec §6, sleep -> wake): a
// socket that only joined the registry once the handshake finished would
// sit outside it for exactly as long as the hang lasted, which is exactly
// when a wake needs to reach it.
//
// req.Write and http.ReadResponse below have no deadline of their own, so
// without help ctx cancellation (client hangup, shutdown) would never
// interrupt a hung handshake either. A goroutine watches ctx and closes
// the connection out from under them if it fires — the same way net/http's
// own Transport cancels its own CONNECT. Deliberately not a SetDeadline
// left on the connection: a deadline that survived the handshake would
// later kill a legitimate long-lived tunnel, which the standing
// no-timeouts ruling forbids.
func (s *Server) dialThroughProxy(ctx context.Context, hostport string) (net.Conn, error) {
	u := s.cfg.UpstreamProxy
	c, err := s.dial(ctx, "tcp", proxyHostPort(u))
	if err != nil {
		return nil, err
	}
	tc := s.conns.track(c)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			tc.Close()
		case <-done:
		}
	}()

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: hostport},
		Host:   hostport,
		Header: make(http.Header),
	}
	if u.User != nil {
		pw, _ := u.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw)))
	}
	if err := req.Write(tc); err != nil {
		tc.Close()
		return nil, err
	}
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		tc.Close()
		return nil, err
	}
	// A CONNECT response has no body to drain, but anything the proxy
	// pipelined after the headers must not be lost.
	if resp.StatusCode != http.StatusOK {
		tc.Close()
		return nil, fmt.Errorf("upstream proxy refused CONNECT %s: %s", hostport, resp.Status)
	}
	if n := br.Buffered(); n > 0 {
		pre, _ := br.Peek(n)
		return &prefixConn{Conn: tc, r: io.MultiReader(bytes.NewReader(bytes.Clone(pre)), tc)}, nil
	}
	return tc, nil
}

// proxyHostPort is the proxy's host:port, defaulting the port by scheme.
func proxyHostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

func (s *Server) mitm(w http.ResponseWriter, hostport string) {
	start := time.Now()
	client, err := hijack(w)
	if err != nil {
		return
	}
	tlsConn := tls.Server(client, s.cfg.CA.ServerTLSConfig(router.HostOnly(hostport)))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = tlsConn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		s.emit(tracelog.Record{T: start.UTC(), Kind: "tunnel", Form: "mitm", Host: hostport, Err: "client TLS handshake: " + err.Error()}, s.tracing())
		tlsConn.Close()
		return
	}
	// Tracked so shutdown can reach it: this conn is hijacked out of the
	// outer server and the inner srv below is a local, so neither
	// http.Server.Shutdown nor CloseUpstreams can end this tunnel (F54).
	//
	// Trap: wrapping tlsConn here makes req.TLS nil on every request the
	// inner srv serves. net/http populates req.TLS by type-asserting its
	// rwc to *tls.Conn; once wrapped in *trackedConn that assertion fails.
	// Harmless today — nothing reads r.TLS, the swap gate is the handler's
	// own r.URL.Scheme == "https", Rewrite never calls SetXForwarded, and
	// ca.ServerTLSConfig pins NextProtos to http/1.1 so h2 was never
	// reachable here anyway — but anything added later that needs TLS state
	// (SetXForwarded, ALPN, ConnectionState) must get it from tlsConn
	// directly, not from r.TLS.
	tracked := s.clientConns.track(tlsConn)
	l := newOneConnListener(tracked)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, in *http.Request) {
			// F222 / security review M1: a request inside this tunnel must
			// name the host the tunnel was opened to, or a swapped token
			// could ride a Host the edge routes elsewhere.
			if !sameTunnelHost(in.Host, hostport) {
				s.emit(tracelog.Record{T: time.Now().UTC(), Kind: "req", Form: "mitm", Method: in.Method,
					Host: router.HostOnly(hostport), Status: http.StatusMisdirectedRequest,
					Err: "Host differs from the tunnel's CONNECT target"}, s.tracing())
				http.Error(w, "chottag: Host does not match this tunnel", http.StatusMisdirectedRequest)
				return
			}
			in.Host = hostport
			in.URL.Scheme = "https"
			in.URL.Host = hostport
			s.forward(w, in, "mitm")
		}),
		ErrorLog: log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				l.Close()
			}
		},
		// IdleTimeout deliberately unset. Its documented contract is "the
		// maximum amount of time to wait for the NEXT request when
		// keep-alives are enabled" — so it never bounds an in-flight
		// request, and could not truncate a streaming SSE response either
		// way. (That is the documented guarantee, deliberately cited
		// instead of how net/http currently implements it: the
		// implementation is not covered by Go's compatibility promise and
		// a comment resting on it would expire without warning.)
		//
		// What it WOULD do is close a tunnel whose client goes quiet
		// between requests (e.g. an interactive session idling between
		// turns), forcing the next request to pay for a fresh CONNECT and
		// TLS handshake. That cost buys nothing here: the client conn this
		// server serves is tracked in s.clientConns, and
		// Server.CloseClientTunnels closes every one of them on shutdown.
		// So a silent client leaks one goroutine and one fd for the rest
		// of the daemon's process lifetime, never longer — a bounded cost
		// with an existing reclaim path, not the kind of leak an idle
		// timeout exists to bound.
	}
	_ = srv.Serve(l)
}

// sameTunnelHost reports whether a tunnelled request's Host names the
// CONNECT target: host compared case-insensitively, a missing port read
// as 443 (the tunnel is TLS). Anything else (an empty Host, a trailing
// dot, another port, a malformed CONNECT target) is a mismatch: fail
// closed.
func sameTunnelHost(reqHost, connectHostport string) bool {
	ch, cp, err := net.SplitHostPort(connectHostport)
	if err != nil || reqHost == "" {
		return false
	}
	rh, rp, err := net.SplitHostPort(reqHost)
	if err != nil {
		rh, rp = strings.TrimSuffix(strings.TrimPrefix(reqHost, "["), "]"), "443"
	}
	return rp != "" && strings.EqualFold(rh, ch) && rp == cp
}

// hijack takes over the client connection and confirms the tunnel.
func hijack(w http.ResponseWriter) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "chottag: hijack unsupported", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		return nil, err
	}
	if n := brw.Reader.Buffered(); n > 0 {
		pre, _ := brw.Reader.Peek(n)
		return &prefixConn{Conn: conn, r: io.MultiReader(bytes.NewReader(bytes.Clone(pre)), conn)}, nil
	}
	return conn, nil
}

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// pipe copies both ways and closes both sides as soon as either ends, so a
// dead upstream never leaves the client waiting on a live loopback socket.
func pipe(a, b net.Conn) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { a.Close(); b.Close() }) }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(a, b); closeBoth() }()
	go func() { defer wg.Done(); io.Copy(b, a); closeBoth() }()
	wg.Wait()
}

// oneConnListener hands one connection to http.Server, then blocks until closed.
type oneConnListener struct {
	conn      net.Conn
	once      sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c, closed: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
