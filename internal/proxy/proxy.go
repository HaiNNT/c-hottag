// Package proxy is chottag's local HTTPS proxy: it intercepts Anthropic hosts,
// classifies each request, optionally swaps its bearer, and traces it.
package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// Chooser picks the account for one request and is told which object ids the
// exchange revealed, so later requests on the same object can follow their
// creator. Returning ok=false leaves the request unchanged. owner reports
// whether account came from the owner map (the object's recorded creator)
// rather than the remote pin or the serving account — forward.go threads it
// to the safety net, which uses it to tell a connector's owner's own
// 401/403/404 apart from a routing error (F241/R96).
type Chooser interface {
	Choose(ctx context.Context, d router.Decision, bodyID string) (account, token string, owner, ok bool)
	Record(kind router.Kind, ids []string, account string)

	// Refresh forces a refresh of that account's token after a swapped
	// request was refused, and reports the new token.
	Refresh(ctx context.Context, account string) (token string, ok bool)
}

type Config struct {
	CA        *ca.Authority
	Intercept func(host string) bool // host without port; nil = intercept nothing
	Log       *tracelog.Writer
	// Tracing reports whether the request starting now is traced (M2c
	// spec §3). forward asks it ONCE, at the request's start, and uses that
	// answer for every shape decision and record the request makes; a
	// blind tunnel asks at its start; a MITM tunnel asks only for its one
	// (handshake-failure) record. A traced request is handled as `trace run
	// --shapes` always was: JSON bodies are read for types-only shapes.
	// nil = never traced. `trace run` passes func() bool { return shapes }.
	Tracing func() bool
	// TraceLog, when set, receives every traced record WITH shapes, and
	// Log then receives the same record WITHOUT them: proxy.jsonl's
	// content never depends on tracing (M2c T5). When nil (`trace run`),
	// Log receives the full record, exactly as before M2c.
	TraceLog *tracelog.Writer
	// OnClaudeVersion, when set, is handed the Claude Code version a
	// traced request's User-Agent names (ClaudeCLIVersion, M2c T7): the
	// version only, never the header. It runs on the request goroutine
	// before the request goes upstream, so it must not block. A panic is
	// recovered and reported through OnLogError as a fixed string.
	OnClaudeVersion func(version string)
	// LimitFingerprint enables `trace run --limit-fingerprint` (gate G6,
	// spec §6.2): a redacted usage-limit classifier fingerprint recorded on
	// every response with status >= 400, via tracelog.FingerprintLimit.
	// Off by default; `proxy run` never sets it.
	LimitFingerprint bool
	Choose           Chooser     // nil = observe only
	OnLogError       func(error) // nil = ignore trace-write failures

	// OnUsageError, when set, receives errors from the OnUsage callback —
	// today only "OnUsage callback panicked". Separate from OnLogError
	// because a broken usage consumer silently stops usage recording
	// altogether, and reporting that as "trace log write failed" sends the
	// reader to the wrong subsystem entirely.
	//
	// nil falls back to OnLogError, so a caller wiring only the older field
	// keeps being told rather than losing the report.
	OnUsageError func(error)

	// OnUsage, when set, is handed the response headers of every SWAPPED
	// response that was actually answered on that account's credential,
	// before the client sees it. Anthropic's unified rate-limit headers ride
	// on every response including successes (F23), so this is how chottag
	// learns usage without spending a request on it.
	//
	// Headers only, never a body: that is what makes it safe on a stream.
	//
	// OnUsage may be called CONCURRENTLY, once per response, each on its own
	// request goroutine, and it runs before any byte reaches the client: the
	// callback must do its own locking, and must not block or panic. Treat h
	// as read-only and do not retain it past the call.
	OnUsage func(account string, status int, h http.Header)

	// WallRetry, if set, arms the wall retry (M4 spec §4a) for a swapped
	// serving-class request that no object owner routes. It is called at
	// most once per request, synchronously on the request's goroutine,
	// when the first attempt comes back 429 on the account's own
	// credential, with a clone of that response's headers. The daemon
	// decides there whether the 429 is a usage limit and runs its planner.
	//
	// retry false passes the 429 through untouched, and done is never
	// called. retry true means the serving account may have moved: the
	// proxy asks Choose again and, if that names a different account,
	// discards the 429's body unread and resends the request there once.
	// A refusal (401/403/404) on that resend still gets the safety net's
	// own refresh-and-retry and, failing that, its original-login
	// fallback — so the resend is not necessarily the last upstream call.
	//
	// done, if not nil, is called exactly once, only after retry was true,
	// and only once the whole chain has reached its final response: the
	// account the response actually came from ("" when nothing was
	// resent, or when the chain ended in the original-login fallback,
	// because then the response is not the resent account's) and that
	// response's status (0 when it or an intermediate step failed at the
	// transport).
	//
	// Only a request whose body could be buffered is eligible: up to 32 MiB
	// (maxWallRetryBody), fully read without error. A panicking hook is
	// treated as retry=false with no done call, mirroring how OnUsage's
	// panics are absorbed: it must never turn a response the client was
	// about to receive into a dropped connection.
	WallRetry func(ctx context.Context, account string, h http.Header) (retry bool, done func(to string, status int))

	// UpstreamProxy routes chottag's own upstream traffic through another
	// proxy — the one the user already had in HTTPS_PROXY before the shim
	// replaced it (spec §4.3, §6). nil means dial directly.
	//
	// Deliberately NOT read from this process's environment: a daemon
	// started by launchd inherits a login environment nobody audited, and
	// routing a user's traffic through whatever it happens to contain is
	// not chottag's decision to make silently.
	//
	// When this carries its own userinfo, a blind CONNECT chained through
	// it needs the same caller auth as an intercepted one (plan Ruling 26,
	// amends Ruling 1): without that gate, any local caller could tunnel
	// through chottag and egress under those credentials without ever
	// seeing them.
	UpstreamProxy *url.URL

	// Version is reported by the health endpoint so the shim can identify
	// this daemon as chottag's (§4.8). Empty in tests that do not care.
	Version string

	// ProxyAuth is the per-install secret (F221): a zero Secret means no
	// caller auth and no health proof, for trace/test callers that don't
	// set it. Set, it gates an unauthenticated CONNECT to an intercepted
	// host, a blind CONNECT chained through a credentialed UpstreamProxy
	// (Ruling 26, amends Ruling 1), and every absolute-form request behind
	// 407 (Rulings 1-3), and arms the health proof for a caller that sends
	// a valid nonce.
	ProxyAuth proxyauth.Secret

	// Test hooks.
	UpstreamRootCAs *x509.CertPool                                                    // nil = system roots
	DialContext     func(ctx context.Context, network, addr string) (net.Conn, error) // nil = net.Dialer
}

var DefaultTraceSuffixes = []string{"anthropic.com", "claude.ai", "claude.com"}

// SuffixMatcher matches hosts equal to, or subdomains of, any suffix.
func SuffixMatcher(suffixes []string) func(string) bool {
	return func(host string) bool {
		for _, s := range suffixes {
			if host == s || strings.HasSuffix(host, "."+s) {
				return true
			}
		}
		return false
	}
}

type Server struct {
	cfg       Config
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	netDialer *net.Dialer // nil when cfg.DialContext was supplied
	transport *http.Transport
	drift     atomic.Uint64
	// inflight counts requests being served; lastStart is the Unix-nano
	// time the latest one began (construction time before any). Both feed Idle.
	inflight  atomic.Int64
	lastStart atomic.Int64
	conns     connSet

	// clientConns holds the CLIENT side of every live MITM tunnel: the
	// TLS conn mitm() serves an inner http.Server over. It is deliberately
	// SEPARATE from conns, which holds UPSTREAM connections.
	//
	// The two are closed at different times and for opposite reasons.
	// CloseUpstreams runs on WAKE, to discard upstream sockets the peer
	// forgot while the laptop slept; closing a client tunnel there would
	// tear down a session the user is actively using. CloseClientTunnels
	// runs only on SHUTDOWN.
	clientConns connSet
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.lastStart.Store(time.Now().UnixNano())
	if cfg.DialContext != nil {
		s.dial = cfg.DialContext
	} else {
		// Spec §6: an upstream that dies while the laptop sleeps leaves a
		// socket the kernel still believes in. The wake detector is the
		// primary defence; these probes are the backstop that notices a
		// genuinely dead peer in ~30s (15s idle + 3 x 5s) without waiting
		// for an application-level timeout that may never come.
		s.netDialer = &net.Dialer{
			Timeout: 30 * time.Second,
			KeepAliveConfig: net.KeepAliveConfig{
				Enable:   true,
				Idle:     15 * time.Second,
				Interval: 5 * time.Second,
				Count:    3,
			},
		}
		s.dial = s.netDialer.DialContext
	}
	s.transport = &http.Transport{
		Proxy: proxyFunc(cfg.UpstreamProxy),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := s.dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return s.conns.track(c), nil
		},
		TLSClientConfig:     &tls.Config{RootCAs: cfg.UpstreamRootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   false,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	return s
}

// dialer exposes the default dialer for tests. ok is false when the caller
// supplied its own DialContext, in which case there is nothing to inspect.
func (s *Server) dialer() (*net.Dialer, bool) { return s.netDialer, s.netDialer != nil }

// proxyFunc adapts an optional upstream proxy to Transport.Proxy, which
// treats a nil function and a function returning nil the same way: direct.
func proxyFunc(u *url.URL) func(*http.Request) (*url.URL, error) {
	if u == nil {
		return nil
	}
	return http.ProxyURL(u)
}

// dialUpstream opens a tracked connection to hostport. Every tunnel dial
// goes through here so CloseUpstreams can reach it. When cfg.UpstreamProxy
// is set, the tunnel is chained through it instead of dialled direct: this
// path bypasses s.transport entirely, so it has to do its own CONNECT.
// dialThroughProxy tracks its own connection (before its handshake, not
// after — see its doc comment), so it is not tracked again here.
func (s *Server) dialUpstream(ctx context.Context, hostport string) (net.Conn, error) {
	if s.cfg.UpstreamProxy != nil {
		return s.dialThroughProxy(ctx, hostport)
	}
	c, err := s.dial(ctx, "tcp", hostport)
	if err != nil {
		return nil, err
	}
	return s.conns.track(c), nil
}

// CloseUpstreams closes every upstream connection this proxy owns and
// drains the idle pool, returning how many live connections it closed.
// Every session then sees the reset it would have seen without chottag and
// retries (spec §6, sleep -> wake).
func (s *Server) CloseUpstreams() int {
	s.transport.CloseIdleConnections()
	return s.conns.closeAll()
}

// CloseClientTunnels closes the client side of every live MITM tunnel and
// reports how many it closed. Shutdown only — never call this on wake.
//
// http.Server.Shutdown cannot reach these: a MITM tunnel's client conn is
// hijacked out of the outer server, and the inner http.Server that serves it
// is a local in mitm(). Without this, a tunnel established before shutdown
// keeps serving requests after serve() returns (F54).
func (s *Server) CloseClientTunnels() int { return s.clientConns.closeAll() }

// Idle reports whether the proxy is quiet enough to restart (R126): no HTTP
// request in flight (a streaming response counts until its body ends), and
// the most recent one started at least quiet before now. A tunnel with no
// request in it does not count. A new server counts its construction as the
// last start.
func (s *Server) Idle(now time.Time, quiet time.Duration) bool {
	if s.inflight.Load() != 0 {
		return false
	}
	return now.Sub(time.Unix(0, s.lastStart.Load())) >= quiet
}

// requestStarted marks one request in flight and returns the func that ends
// it. The start time is stored before the counter rises, so Idle never sees
// the request counted without its time.
func (s *Server) requestStarted() func() {
	s.lastStart.Store(time.Now().UnixNano())
	s.inflight.Add(1)
	return func() { s.inflight.Add(-1) }
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		// handleConnect (connect.go) decides the CONNECT-specific gate
		// itself, from the one "is this host intercepted" boolean it also
		// uses to pick mitm vs blind (fix round 1 item 3): duplicating that
		// computation here could drift from the one handleConnect actually
		// acts on.
		s.handleConnect(w, r)
	case r.URL.IsAbs():
		var caller proxyauth.Caller
		if s.gated() {
			var ok bool
			if caller, ok = s.caller(r); !ok {
				s.requireAuth(w, tracelog.Record{Kind: "req", Form: "absolute", Method: r.Method, Host: router.HostOnly(r.URL.Host)})
				return
			}
		}
		// ReverseProxy would drop this anyway (Proxy-Authorization is
		// hop-by-hop), but deleting it here means upstream never sees it
		// even if that stops being true; TestAuthorisedClientStillSwapsAndStripsTheHeader
		// pins the outcome.
		r.Header.Del("Proxy-Authorization")
		s.forward(w, withIdentity(r, caller, router.HostOnly(r.URL.Host), false), "absolute")
	case r.URL.Path == HealthPath:
		s.serveHealth(w, r)
	default:
		http.Error(w, "chottag: not a proxy request", http.StatusBadRequest)
	}
}

// gated reports whether caller auth is on (F221): the daemon and trace run
// set ProxyAuth, and tests that leave it zero see the old behaviour.
func (s *Server) gated() bool { return !s.cfg.ProxyAuth.IsZero() }

// caller authenticates r's Proxy-Authorization once: ok=false is a 407. With
// caller auth off the caller is the zero Caller and ok is false, so a caller
// checks gated() first, as every gate here does.
func (s *Server) caller(r *http.Request) (proxyauth.Caller, bool) {
	return s.cfg.ProxyAuth.Caller(r.Header.Get("Proxy-Authorization"))
}

// requireAuth answers 407 and records the refusal with the host only: never
// the Proxy-Authorization value, never a path or a body.
func (s *Server) requireAuth(w http.ResponseWriter, rec tracelog.Record) {
	rec.T, rec.Status, rec.Err = time.Now().UTC(), http.StatusProxyAuthRequired, "proxy authentication required"
	s.emit(rec, s.tracing())
	w.Header().Set("Proxy-Authenticate", `Basic realm="chottag"`)
	http.Error(w, "chottag: proxy authentication required; start claude through chottag (a session started before this chottag version needs restarting)", http.StatusProxyAuthRequired)
}

// HealthPath is the origin-form path the daemon answers to prove it is
// chottag. The shim probes it before setting HTTPS_PROXY (§4.8).
const HealthPath = "/__chottag/health"

// Health is the document HealthPath returns. The shim treats ONLY a
// response that unmarshals into this with Chottag true as "our daemon": a
// listener that merely answers is not proof, and the shim is about to hand
// claude an HTTPS_PROXY, so a foreign process holding the port would receive
// Anthropic bearer tokens.
//
// Upstream is here so the shim can refuse to run when the daemon's upstream
// disagrees with this shell's HTTPS_PROXY (§4.3) — the daemon accepts an
// upstream only at startup, so a daemon from an earlier shell may hold a
// different one. It is reduced to scheme://host by redact.WithoutUserinfo
// before it is ever assigned: this is an unauthenticated loopback endpoint
// (any local process can GET it), so any userinfo — a corporate proxy
// password — must never be serialised here at all, not merely masked (fix
// round 3, D1). Comparing on the reduced form still catches the case this
// field exists for, a genuinely different upstream; it only stops
// distinguishing two upstreams that differ solely by password, which is
// not a case this check needs to catch.
//
// There is deliberately no port field: the Server does not know its own
// listening port (the listener resolves it later, internal/cli/proxy.go),
// and the shim already knows which port it dialled, so plumbing it here
// would add wiring for no security value.
type Health struct {
	Chottag  bool   `json:"chottag"`
	Version  string `json:"version"`
	PID      int    `json:"pid"`
	Upstream string `json:"upstream,omitempty"`
	// Proof is the HMAC proof of the install secret for the probe's nonce
	// (F221, L4); empty from a daemon older than part 1.
	Proof string `json:"proof,omitempty"`
	// Sessions is true when this daemon accepts a per-session proxy
	// credential (chottag.<pool>.<sid>, M6) as well as the legacy
	// chottag:<secret>. A v0.4.0-v0.5.x daemon proves the secret but
	// accepts only the legacy credential, and omits the field, so a shim
	// that sees it absent must hand claude the legacy credential. It is
	// not covered by the proof: only a daemon whose proof verifies is
	// trusted at all, and stripping the field can only downgrade a
	// session to the legacy credential every daemon accepts.
	Sessions bool `json:"sessions,omitempty"`
}

// serveHealth answers the shim's "is that you?" probe.
//
// It answers loopback, Origin-less callers only (loopbackHost below, plus a
// bare check that Origin is unset): a DNS-rebound page or a browser fetch
// is refused before anything else runs (Ruling 12).
//
// It is reachable ONLY for origin-form requests: a proxied request arrives
// absolute-form and is handled by ServeHTTP's IsAbs case above, and a
// tunnelled request is served by the inner http.Server in mitm() and never
// reaches this switch at all. So there is no path here from inside a CONNECT
// tunnel.
//
// The only request-derived output is Health.Proof, a keyed HMAC — but only
// over a nonce that already passed proxyauth.ValidNonce (exactly 32
// lower-case hex characters); every other nonce, well-formed or not, gets
// no proof at all (Ruling 8). That fixed format is what keeps this from
// being an HMAC oracle over caller-chosen input, not a lack of request
// influence on the output. Beyond that, it reads no credential store and
// touches no account state, so nothing else here can be used to reflect
// attacker-chosen content or probe which accounts exist.
//
// The proof itself is bound to the port this request actually arrived on
// (Ruling 32, final review M2): net/http populates
// http.LocalAddrContextKey on every request's context from the accepted
// connection's own local address, so localListenPort below needs no config
// and cannot be spoofed by anything in the request. Binding it is what
// stops a local squatter on the port the shim probed from relaying the
// shim's nonce to a DIFFERENT, same-secret listener and handing back THAT
// listener's otherwise-valid proof. A request whose context carries no
// usable local address (only possible outside a real net.Listener-backed
// http.Server, which production always uses) gets no proof at all, never a
// proof for the wrong port.
func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "chottag: method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// L3: a DNS-rebound page reaches this with its own Host, and a browser
	// always sends Origin on a cross-origin fetch; the shim sends neither.
	if !loopbackHost(r.Host) || r.Header.Get("Origin") != "" {
		http.Error(w, "chottag: health answers loopback, non-browser callers only", http.StatusForbidden)
		return
	}
	h := Health{Chottag: true, Version: s.cfg.Version, PID: os.Getpid()}
	if s.cfg.UpstreamProxy != nil {
		// WithoutUserinfo, not Redacted(): a mask still publishes that a
		// password exists on this unauthenticated loopback endpoint (fix
		// round 3, D1 — see Health.Upstream's doc comment above).
		h.Upstream = redact.WithoutUserinfo(s.cfg.UpstreamProxy.String())
	}
	if n := r.URL.Query().Get(proxyauth.NonceParam); s.gated() && proxyauth.ValidNonce(n) {
		if port, ok := localListenPort(r); ok {
			h.Proof = s.cfg.ProxyAuth.Proof(port, n)
		}
	}
	// Caller auth on means Caller (session and legacy credentials) gates
	// requests, so this daemon understands a session credential.
	h.Sessions = s.gated()
	b, err := json.Marshal(h)
	if err != nil {
		http.Error(w, "chottag: health encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(b, '\n'))
}

// RouteDrift counts swapped requests that had to be resent unchanged: a sign
// the route table no longer matches this Claude Code version.
func (s *Server) RouteDrift() uint64 { return s.drift.Load() }

// tracing is Config.Tracing's answer, false when it is unset.
func (s *Server) tracing() bool { return s.cfg.Tracing != nil && s.cfg.Tracing() }

// logging reports whether a request's records go anywhere: proxy.jsonl,
// or trace.jsonl while traced. A stream's head record (and its random id)
// is skipped when they go nowhere.
func (s *Server) logging(traced bool) bool {
	return s.cfg.Log != nil || (traced && s.cfg.TraceLog != nil)
}

// emit writes one record. A traced record goes to TraceLog whole, first;
// then, when a TraceLog exists, Log gets the record with its shape fields
// cleared, so proxy.jsonl never carries shapes (M2c T5). Without a
// TraceLog (`trace run`) Log gets the record as it is. r is a copy, so
// clearing it never touches the caller's record.
func (s *Server) emit(r tracelog.Record, traced bool) {
	if traced && s.cfg.TraceLog != nil {
		s.write(s.cfg.TraceLog, r)
	}
	if s.cfg.TraceLog != nil {
		r.ReqShape, r.RespShape = nil, nil
	}
	s.write(s.cfg.Log, r)
}

// loopbackHost reports whether hostport (a Host header's value, with or
// without a port) names localhost or a loopback IP (Ruling 12): the server
// doesn't know its own listening port, so any port is accepted.
func loopbackHost(hostport string) bool {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// localListenPort is the port THIS request's connection was accepted on,
// read from http.LocalAddrContextKey — the accepted net.Conn's own local
// address, which net/http populates on every request's context regardless
// of ConnContext, so nothing here needs to be told the listen port
// separately (Ruling 32). false only when the context carries no usable
// local address, which production's real net.Listener-backed http.Server
// never produces.
func localListenPort(r *http.Request) (int, bool) {
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return 0, false
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, false
	}
	return port, true
}

func (s *Server) write(w *tracelog.Writer, r tracelog.Record) {
	if w == nil {
		return
	}
	if err := w.Write(r); err != nil && s.cfg.OnLogError != nil {
		s.cfg.OnLogError(err)
	}
}
