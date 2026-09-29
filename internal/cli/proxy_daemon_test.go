package cli

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// rosterTicks stands in for runDaemon's real roster ticker (part 5: the
// two tests that slept 5.3 s to land between real 5 s ticks). The watcher
// ticks only when tick is called, and tick returns only once that tick is
// fully handled, so nothing a test does races a tick in flight.
type rosterTicks struct {
	t         *testing.T
	c         chan time.Time
	processed chan struct{} // the daemon's RosterProcessed
	every     chan time.Duration
}

// injectRosterTicks replaces newRosterTicker, and rosterProcessedForTest
// for a daemon runProxyWithSignal builds, until the test ends. A test that
// calls runDaemon itself passes processed as daemonDeps.RosterProcessed.
// every receives the interval runDaemon asked for.
func injectRosterTicks(t *testing.T) *rosterTicks {
	t.Helper()
	r := &rosterTicks{t: t, c: make(chan time.Time), processed: make(chan struct{}), every: make(chan time.Duration, 1)}
	oldTicker, oldProcessed := newRosterTicker, rosterProcessedForTest
	newRosterTicker = func(every time.Duration) (<-chan time.Time, func()) {
		select {
		case r.every <- every:
		default:
		}
		return r.c, func() {}
	}
	rosterProcessedForTest = r.processed
	t.Cleanup(func() { newRosterTicker, rosterProcessedForTest = oldTicker, oldProcessed })
	return r
}

// tick sends one roster tick and waits until the watcher has handled it.
// The 10 s bounds are hang guards only: a live watcher takes a tick at once.
func (r *rosterTicks) tick(what string) {
	r.t.Helper()
	select {
	case r.c <- time.Now():
	case <-time.After(10 * time.Second):
		r.t.Fatalf("the roster watcher never took the %s tick", what)
	}
	select {
	case <-r.processed:
	case <-time.After(10 * time.Second):
		r.t.Fatalf("the roster watcher never finished the %s tick", what)
	}
}

// TestNewRosterTickerDefaultIsARealTickerAtTheIntervalItIsGiven pins what
// injectRosterTicks replaces: production's newRosterTicker is a real
// ticker at the interval runDaemon passes, and that interval is 5 s.
func TestNewRosterTickerDefaultIsARealTickerAtTheIntervalItIsGiven(t *testing.T) {
	if rosterTickInterval != 5*time.Second {
		t.Fatalf("rosterTickInterval = %s, want 5s", rosterTickInterval)
	}
	c, stop := newRosterTicker(time.Millisecond)
	defer stop()
	for i := 1; i <= 2; i++ {
		select {
		case <-c:
		case <-time.After(10 * time.Second):
			t.Fatalf("newRosterTicker(1ms): tick %d never came", i)
		}
	}
}

// TestRunDaemonDefaultsForgetToOwnForget pins the ONLY path production
// actually takes: every real invocation passes forget == nil, which the
// fix-round-2 seam then defaults to own.Forget. That default line is
// unguarded by every other test in this file, since each of them supplies
// its own forget (nil is never exercised elsewhere) — deleting the
// default, or replacing it with a func(string) error{ return nil } stub,
// both leave the rest of the suite green. The former is a nil-func panic
// on the roster-watch goroutine roughly 5s after any account disappears
// (unrecoverable, kills the daemon under every in-flight request and
// tunnel); the latter silently reintroduces the exact F1 hazard this task
// exists to close, since removed accounts would never actually be
// forgotten.
//
// Driven at the runDaemon level (not through runProxy) so this pins
// exactly the seam's default, independent of anything runProxy itself
// wires around own.
//
// It also leaves d.RosterTick unset (nil), which is production's only
// path for that seam too (item ii): runDaemon must then ask
// newRosterTicker for a rosterTickInterval ticker, which
// injectRosterTicks records and stands in for. Deleting that fallback,
// or defaulting it to a channel that never fires, never asks, and this
// test fails. TestNewRosterTickerDefaultIsARealTickerAtTheIntervalItIsGiven
// pins that newRosterTicker's default is a real ticker.
func TestRunDaemonDefaultsForgetToOwnForget(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "A")}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(dir, "B")}); err != nil {
			return err
		}
		st.Serving, st.Remote = "B", "B" // so Remove("A") below is allowed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if err := own.Record(router.KindArtifact, []string{"artifact-1"}, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, ok := own.Lookup(router.KindArtifact, "artifact-1"); !ok || got != "A" {
		t.Fatalf("seed Lookup = (%q, %v), want (\"A\", true)", got, ok)
	}

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()
	ticks := injectRosterTicks(t)

	ctx, cancel := context.WithCancel(context.Background())
	daemonDone := make(chan int, 1)
	go func() {
		// Forget is left unset (nil) — the point of this test: exactly as
		// runProxy's one production call site always leaves it, and this
		// also leaves RosterTick unset, so runDaemon must fall back to
		// newRosterTicker (item ii's mutation target); injectRosterTicks
		// stands in for the real ticker there and records the interval.
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Sink:            sink,
			Tokens:          tm,
			Cache:           cache,
			Owners:          own,
			RosterProcessed: ticks.processed,
		})
	}()
	// A plain `defer cancel()` would leave the daemon goroutine mid-shutdown
	// (up to shutdownGrace) if a t.Fatal below fires before the explicit
	// cancel+wait at the bottom — racing t.TempDir()'s own RemoveAll
	// cleanup against files the still-running daemon has open (observed:
	// "directory not empty" while verifying this test's own mutations).
	// stopped lets both the happy path and any early t.Fatal converge on
	// the same wait, exactly once.
	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		cancel()
		select {
		case <-daemonDone:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	select {
	case every := <-ticks.every:
		if every != rosterTickInterval {
			t.Fatalf("runDaemon asked newRosterTicker for a %s ticker, want rosterTickInterval (%s)", every, rosterTickInterval)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runDaemon never asked newRosterTicker for a ticker: is its RosterTick==nil default wired to it?")
	}
	ticks.tick("baseline") // known={A,B}; never diffs
	if _, err := s.Update(func(st *store.State) error { return st.Remove("A") }); err != nil {
		t.Fatal(err)
	}
	ticks.tick("diff")
	if _, ok := own.Lookup(router.KindArtifact, "artifact-1"); ok {
		t.Fatal("own still resolves an owner for the removed account after the diff tick — is runDaemon's forget==nil default wired to own.Forget? this test is its sole pin")
	}

	cancel()
	select {
	case <-daemonDone:
		stopped.Store(true)
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// TestRunDaemonWiresTheRealProxyConfigAndClosesRealUpstreamConnections is
// a runDaemon-level integration test that kills three mutations at once,
// per the reviewer's explicit instruction to take this route rather than
// three separate assertions:
//
//   - F2b's first: `proxy.New(cfg)` → `proxy.New(proxy.Config{})` inside
//     runDaemon. A zero-value Config has no CA, no Intercept, no Choose
//     and no OnUsage — every one of those is individually droppable
//     without any other test noticing, because no other test builds a
//     REAL cfg (with all four set) and drives it through runDaemon's own
//     net.Listen + serve, only through proxy.New directly (proxytest) or
//     through newProxyConfig in isolation (TestNewProxyConfigWiresUpstreamProxy).
//   - F2b's second: the `serve(ctx, ln, srv, srv.CloseUpstreams,
//     srv.CloseClientTunnels, closeOwners, sink)` call inside runDaemon,
//     with closeConns replaced by a no-op. serve's
//     OWN use of its closeConns parameter is already covered by
//     TestShutdownClosesTunnels, but that test calls serve directly with
//     a hand-rolled closeConns — it cannot see runDaemon's wiring of the
//     real srv.CloseUpstreams into that parameter.
//   - M16, verbatim: "make srv.CloseUpstreams a no-op in the wake
//     handler." Real cost: a resume never closes stale upstream
//     connections.
//
// Phase 1 (wake path) makes a real, MITM'd, swapped POST to
// api.anthropic.com through runDaemon's own listener — which only
// succeeds end to end if CA, Intercept, Choose and OnUsage are ALL the
// real ones from cfg, not a zero-value Config's nils (Intercept nil
// routes to blind() instead of mitm(), so DialContext's redirect to a
// TLS-only fake upstream would fail the handshake; Choose nil never
// swaps the bearer, so the fake upstream's assertion on the swapped
// token fails; OnUsage nil never fires, caught by the usageCalls
// assertion) — then drives a real wake through the fake clock and
// requires a SECOND request to force a new dial, proving the pooled
// upstream connection was actually closed (M16, and — since Intercept/CA
// must still be wired for this second request to succeed at all — F2b's
// first mutation stays caught here too, not just on the first request).
//
// Phase 2 (shutdown path) opens a real blind CONNECT tunnel (a host
// Intercept does not match) through the same daemon, cancels ctx, and
// requires the client's own end of the tunnel to observe the close —
// reusing TestShutdownClosesTunnels' technique, but through runDaemon's
// own serve(...) call site instead of a direct call, which is the only
// way to catch F2b's second mutation.
func TestRunDaemonWiresTheRealProxyConfigAndClosesRealUpstreamConnections(t *testing.T) {
	dir := t.TempDir()

	upCA, err := ca.LoadOrCreate(filepath.Join(dir, "upstream-ca"))
	if err != nil {
		t.Fatal(err)
	}
	var usageCalls atomic.Int32
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer swapped-token" {
			t.Errorf("upstream saw Authorization = %q, want the swapped bearer — was cfg.Choose wired into runDaemon's proxy.New?", got)
		}
		w.WriteHeader(200)
	}))
	up.TLS = upCA.ServerTLSConfig("api.anthropic.com")
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.StartTLS()
	t.Cleanup(up.Close)

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	proxyCA, err := ca.LoadOrCreate(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	lw, err := tracelog.Open(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lw.Close() })

	var dialCount atomic.Int32
	cfg := proxy.Config{
		CA:        proxyCA,
		Intercept: proxy.SuffixMatcher(proxy.DefaultTraceSuffixes),
		Log:       lw,
		Choose:    fixedServingChooser{account: "A", token: "swapped-token"},
		OnUsage: func(account string, status int, h http.Header) {
			usageCalls.Add(1)
		},
		UpstreamRootCAs: upCA.Pool(),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host := router.HostOnly(addr)
			if host == "echo.example.org" {
				return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
			}
			dialCount.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, up.Listener.Addr().String())
		},
	}

	ticks := make(chan time.Time)
	var wallNanos atomic.Int64
	base := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	wallNanos.Store(base.UnixNano())
	wakerCfg := proxy.WakerConfig{
		Gap:   30 * time.Second,
		Ticks: ticks,
		Wall:  func() time.Time { return time.Unix(0, wallNanos.Load()).UTC() },
		Mono:  func() time.Duration { return 0 },
		// OnWake deliberately left unset: runDaemon must supply the
		// default, which wires srv.CloseUpstreams.
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard,
			Stderr: errb,
			Listen: "127.0.0.1:0",
			Cfg:    cfg,
			Sink:   sink,
			Tokens: tm,
			Cache:  store.NewCache(store.Store{Dir: t.TempDir()}),
			Owners: own,
			Waker:  wakerCfg,
		})
	}()

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for runDaemon to start listening")
	}
	const marker = "chottag proxy listening on "
	errs := errb.String()
	i := strings.Index(errs, marker)
	if i < 0 {
		t.Fatalf("stderr = %q, want it to name the listen address", errs)
	}
	rest := errs[i+len(marker):]
	addr := rest[:strings.IndexByte(rest, ',')]

	pu, err := url.Parse("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: proxyCA.Pool()}},
	}

	// --- Phase 1: wake ---
	doRequest := func() {
		t.Helper()
		req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-ORIGINAL")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request through the daemon failed: %v — is cfg (CA/Intercept) actually wired into runDaemon's proxy.New?", err)
		}
		resp.Body.Close()
	}

	doRequest()
	if usageCalls.Load() != 1 {
		t.Fatalf("OnUsage calls = %d, want 1 — is cfg.OnUsage wired into runDaemon's proxy.New?", usageCalls.Load())
	}
	if dialCount.Load() != 1 {
		t.Fatalf("upstream dial count = %d, want 1 after the first request", dialCount.Load())
	}

	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("Run never consumed the warm-up tick")
	}
	wallNanos.Store(base.Add(60 * time.Second).UnixNano())
	select {
	case ticks <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("Run never consumed the wake-triggering tick")
	}

	// A no-op CloseUpstreams (M16) leaves the pooled connection alive, so
	// a second request would reuse it and dialCount would stay at 1
	// forever; poll (the wake's default handler closes it on its own
	// goroutine) rather than sleep once.
	deadline := time.Now().Add(2 * time.Second)
	for {
		doRequest()
		if dialCount.Load() >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream dial count = %d after a wake, want >= 2 — the wake's default OnWake did not close the real srv.CloseUpstreams' tracked connection", dialCount.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- Phase 2: shutdown ---
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT echo.example.org:1 HTTP/1.1\r\nHost: echo.example.org:1\r\n\r\n")
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("CONNECT to the non-intercepted echo host failed: %v %q — is cfg.Intercept actually wired?", err, buf[:n])
	}
	io.WriteString(c, "ping")
	n, err = c.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("echo round-trip failed: %v %q", err, buf[:n])
	}

	cancel()
	// The deadline below is a test bound, not a proof of closure: a
	// no-op closeConns (F2b's second mutation) leaves the tunnel open
	// forever, and Read would then return a *net.OpError wrapping
	// os.ErrDeadlineExceeded once that bound trips — a non-nil error,
	// but NOT evidence the tunnel was actually closed. Only a non-timeout
	// error (EOF, connection reset, or similar, once the far side is
	// really torn down) counts.
	c.SetReadDeadline(time.Now().Add(shutdownGrace + 5*time.Second))
	_, err = c.Read(buf)
	var netErr net.Error
	if err == nil || (errors.As(err, &netErr) && netErr.Timeout()) {
		t.Fatalf("tunnel read = %v, want a non-timeout error — runDaemon's serve(..., srv.CloseUpstreams, srv.CloseClientTunnels, ...) wiring did not reach the real srv, or the tunnel is still open after shutdown", err)
	}

	select {
	case code := <-daemonDone:
		if code != 0 {
			t.Fatalf("runDaemon returned %d, want 0", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return after its context was cancelled")
	}
}

// TestRunDaemonClosesLiveMITMTunnelOnShutdown pins F54 itself, end to end,
// through the real runDaemon → serve → srv.CloseClientTunnels path — not a
// hook that merely proves serve CALLED a closure
// (TestServeClosesClientTunnelsOnShutdown, below, injects counters and
// cannot see whether a real hijacked conn stops serving) and not a blind
// tunnel (the shutdown phase of
// TestRunDaemonWiresTheRealProxyConfigAndClosesRealUpstreamConnections
// dials echo.example.org, a host cfg.Intercept does NOT match, so it
// exercises closeConns/srv.CloseUpstreams, never closeTunnels at all). A
// no-op mis-wiring of srv.CloseClientTunnels into serve would be silent to
// every other test in this file; only a real MITM'd host (in
// proxy.DefaultTraceSuffixes, so mitm() runs its own inner http.Server
// over the hijacked conn instead of blind()'s passthrough) and a second
// request issued on that exact TCP connection after shutdown can catch it.
//
// Deliberately not an http.Client: its own connection pool can silently
// open a brand new connection for a request that "should" reuse one,
// which would make the post-shutdown request's outcome say nothing about
// whether the FIRST connection's tunnel was actually closed. This test
// keeps the raw net.Conn from the CONNECT handshake, wraps it once in
// tls.Client, and writes both requests directly onto that same conn (only
// http.ReadResponse is reused from net/http, as a response PARSER with no
// pooling or dialing of its own).
//
// No real network host is contacted: cfg.DialContext here is unconditional
// — every dial, regardless of the requested host or port, is redirected to
// the local httptest.Server below — so "api.anthropic.com" is used only as
// a CONNECT target string this process's own proxy intercepts locally; it
// is never resolved or dialled (contrast the DialContext in
// TestRunDaemonWiresTheRealProxyConfigAndClosesRealUpstreamConnections
// above, which special-cases one hostname and falls through to `up` for
// everything else — here there is no fallthrough to a real dial at all).
func TestRunDaemonClosesLiveMITMTunnelOnShutdown(t *testing.T) {
	dir := t.TempDir()

	upCA, err := ca.LoadOrCreate(filepath.Join(dir, "upstream-ca"))
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	up.TLS = upCA.ServerTLSConfig("api.anthropic.com")
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.StartTLS()
	t.Cleanup(up.Close)

	proxyCA, err := ca.LoadOrCreate(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	lw, err := tracelog.Open(filepath.Join(dir, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lw.Close() })

	cfg := proxy.Config{
		CA:              proxyCA,
		Intercept:       proxy.SuffixMatcher(proxy.DefaultTraceSuffixes),
		Log:             lw,
		Choose:          fixedServingChooser{account: "A", token: "swapped-token"},
		UpstreamRootCAs: upCA.Pool(),
		// Unconditional: no branch here ever reaches a real net.Dial with
		// the CONNECT target's actual host, so nothing this test does can
		// resolve or dial out to the real internet.
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, up.Listener.Addr().String())
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard,
			Stderr: errb,
			Listen: "127.0.0.1:0",
			Cfg:    cfg,
			Sink:   sink,
			Tokens: tm,
			Cache:  store.NewCache(store.Store{Dir: t.TempDir()}),
			Owners: own,
		})
	}()

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for runDaemon to start listening")
	}
	const marker = "chottag proxy listening on "
	errs := errb.String()
	i := strings.Index(errs, marker)
	if i < 0 {
		t.Fatalf("stderr = %q, want it to name the listen address", errs)
	}
	rest := errs[i+len(marker):]
	addr := rest[:strings.IndexByte(rest, ',')]

	// Establish the tunnel by hand: CONNECT, then a TLS handshake with the
	// proxy's own CA (mitm() terminates TLS here and presents a leaf it
	// generated on the fly for api.anthropic.com, signed by proxyCA — NOT
	// upCA, which only signs the fake upstream's own cert one hop further
	// in).
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	connectResp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("reading the CONNECT response failed: %v", err)
	}
	connectResp.Body.Close()
	if connectResp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to the intercepted host returned %d, want 200 — is cfg.Intercept actually wired?", connectResp.StatusCode)
	}

	tlsConn := tls.Client(c, &tls.Config{RootCAs: proxyCA.Pool(), ServerName: "api.anthropic.com"})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake over the MITM tunnel failed: %v — is the leaf mitm() presents actually signed by proxyCA?", err)
	}
	br := bufio.NewReader(tlsConn)

	doRequest := func() (int, error) {
		if _, err := io.WriteString(tlsConn, "GET /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-oat01-ORIGINAL\r\n\r\n"); err != nil {
			return 0, err
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}

	// A request over the live tunnel, before shutdown, must succeed — a
	// precondition check, not the assertion this test exists for.
	if status, err := doRequest(); err != nil || status != http.StatusOK {
		t.Fatalf("pre-shutdown request over the MITM tunnel: status=%d err=%v, want 200/nil — test precondition broken, not what this test means to check", status, err)
	}

	cancel()
	select {
	case code := <-daemonDone:
		if code != 0 {
			t.Fatalf("runDaemon returned %d, want 0", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return after its context was cancelled")
	}

	// The tunnel itself is invisible to runDaemon's return: runDaemon can
	// only return once serve does, and by the time it has, closeTunnels()
	// (if wired) has already run — so no extra wait is needed here, and a
	// bounded deadline is still set as a backstop against a hang, not as
	// the thing being proved.
	tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
	status, err := doRequest()
	if err == nil {
		t.Fatalf("post-shutdown request on the SAME live MITM tunnel returned status %d with no error: the tunnel is still being served after runDaemon's shutdown completed — this is F54 itself, reproduced verbatim (a live MITM tunnel established before shutdown must stop serving; closeTunnels/srv.CloseClientTunnels is either not being called from serve, or not reaching the real srv)", status)
	}
}

// TestRunDaemonDrainsOwnersWhenListenFails pins runDaemon's THIRD exit
// path, distinct from either of serve's: net.Listen failing means serve is
// never even called, so closeOwners is only reachable there if runDaemon
// calls it directly. "Nothing writes to own this early, so it's fine" is
// exactly the reasoning that produced F71 (an early return's close hooks
// scoped to a justification that didn't generalize) — this test makes the
// rule uniform instead of trusting that reasoning to still hold after a
// future change.
//
// It is also the sole pin for item (iv), the wake-detector join: before
// the WakerDone assertion below existed, nothing in this file observed
// whether the waker was actually joined, only whether it eventually
// stopped (given enough time and an already-cancelled ctx, which this
// test's ctx deliberately is not). WakerDone is checked with a
// non-blocking select because, WITH the join, the ordering is
// deterministic in the correct direction regardless of scheduling: the
// waker goroutine closes d.WakerDone BEFORE its own deferred
// close(wakerStopped), so if runDaemon actually calls <-wakerStopped
// before returning, WakerDone is guaranteed already closed by the time it
// has — no race, no sleep needed.
//
// Deleting BOTH stopWaker() and its <-wakerStopped receive together from
// runDaemon's listen-failure branch is what this assertion is a reliable
// kill for (confirmed 30/30 runs under default GOMAXPROCS): with nothing
// else on this path ever cancelling wakerCtx, the waker keeps running
// and WakerDone is provably still open at this exact point, every time.
// Measured, deleting ONLY the <-wakerStopped receive while leaving
// stopWaker() itself in place is a WEAKER mutation this same assertion
// does not reliably catch under default scheduling: on this machine, 10
// individual runs of `go test ./internal/cli/ -run
// 'TestRunDaemonDrainsOwnersWhenListenFails$'` catch it 0/10 (wakerCtx is
// still cancelled synchronously on that path regardless, and the
// now-unblocked waker goroutine reliably finishes closing WakerDone
// before this check runs anyway, on ordinary multi-core scheduling), and
// 10 runs of the whole `go test ./internal/cli/` package catch it 0/10
// too. `go test -cpu=1`, this project's own established technique for
// reasoning about scheduling, does noticeably better but is NOT a
// reliable kill either: 7/10 run in isolation, 10/10 run as part of the
// whole package (denying the waker goroutine a spare core to race ahead
// on matters more when it is contending with the rest of the package's
// goroutines for the one core there is). Context changes the number;
// state it every time rather than quoting a bare fraction. Recorded here
// rather than silently claimed as fully covered: this test's guarantee is
// "the join site exists and does something", not "every single statement
// inside it is individually load-bearing under every scheduler".
//
// Forcing the failure without a fixed or privileged port: bind
// 127.0.0.1:0 here and hold it open, then hand runDaemon that address —
// its own net.Listen on the same, now-occupied address fails.
func TestRunDaemonDrainsOwnersWhenListenFails(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	stderr := newSyncBuf()
	wakerDone := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := runDaemon(ctx, daemonDeps{
		Stdout:    io.Discard,
		Stderr:    stderr,
		Listen:    held.Addr().String(),
		Sink:      sink,
		Tokens:    tm,
		Cache:     store.NewCache(store.Store{Dir: t.TempDir()}),
		Owners:    own,
		WakerDone: wakerDone,
	})
	if code != 1 {
		t.Fatalf("runDaemon returned %d, want 1 on a listen failure", code)
	}

	// own.Close() (inside closeOwners) makes every subsequent write return
	// ErrClosed instead of reaching disk (owners.go's own contract) — the
	// only externally observable proof that closeOwners actually ran here,
	// since runDaemon does not expose it directly.
	if err := own.Record(router.KindArtifact, []string{"x"}, "A", time.Now()); !errors.Is(err, owners.ErrClosed) {
		t.Fatalf("own.Record after runDaemon returned = %v, want owners.ErrClosed: closeOwners must run even when net.Listen fails, or the writer goroutine leaks and the next write after a restart races an unflushed newest document", err)
	}

	// Non-blocking: see the doc comment above for why this can never be a
	// race in the failing direction. ctx is still live at this point (the
	// deferred cancel() has not run yet), so this is a listen failure the
	// waker goroutine had no OTHER reason to have stopped for — only
	// runDaemon's own join, before returning, explains WakerDone being
	// closed here.
	select {
	case <-wakerDone:
	default:
		t.Fatal("WakerDone was not closed by the time runDaemon returned: the wake detector was not joined on the listen-failure path (item iv)")
	}

	// Pin WHICH path produced code == 1, not just that some path did: a
	// refactor that changed how runDaemon reaches its listen failure could
	// otherwise leave this test green while exercising neither of the two
	// mutations it exists to catch (runDaemon's third drain, and stopWatch()
	// → <-ctx.Done() hanging closeOwners with ctx still live). "chottag
	// proxy listening on" is only ever printed AFTER a successful
	// net.Listen, so its absence proves this never got past the failing
	// Listen call; the address-in-use text proves the failure came from
	// binding held's own address, not some other startup error that also
	// happens to return 1.
	got := stderr.String()
	if strings.Contains(got, "chottag proxy listening on") {
		t.Fatalf("stderr = %q, want no \"listening on\" line: net.Listen must have failed before that point", got)
	}
	if !strings.Contains(got, "address already in use") {
		t.Fatalf("stderr = %q, want it to name a listen failure on the already-held address", got)
	}
}

// TestRunDaemonUnparksAfterAListenerFailureAfterServe pins the post-serve
// stopWaker()/<-wakerStopped pairing (F84, whole-branch review): unlike the
// net.Listen failure above, this one drives serve's <-errc branch — the
// listener already succeeded and Serve had already started — which never
// cancels ctx (serve's own doc comment: that branch fires on "a listener
// error, not a graceful shutdown via ctx"; installShutdown's cancel() only
// runs once run(ctx), i.e. runDaemon, has already returned). So the
// stopWaker() immediately after runDaemon's `err = serve(...)` call is the
// ONLY thing that ever unparks the waker goroutine on this path, and
// <-wakerStopped is the only thing that joins it before runDaemon returns.
//
// Forced without a fixed or privileged port, and without net.Listen itself
// ever failing: listenTCP (proxy.go) is overridden to capture the exact
// net.Listener runDaemon ends up calling Serve on, and this test closes
// that listener out from under the running http.Server once startup has
// completed — srv.Serve's Accept loop then returns a non-ErrServerClosed
// error, which is exactly serve's <-errc branch, with ctx still live.
//
// The two mutations at that site are not equally caught, and that is
// reported rather than smoothed over. Deleting stopWaker() there hangs
// runDaemon itself (nothing else ever cancels wakerCtx on this branch), so
// the bounded daemonDone select below is a deterministic kill: 10/10
// measured. Deleting only <-wakerStopped does NOT hang runDaemon —
// stopWaker() alone still cancels wakerCtx, so runDaemon returns without
// waiting for the now-unblocked waker goroutine to finish closing
// d.WakerDone — and the immediately-following non-blocking WakerDone check
// below catches that race probabilistically, at a rate that is low and
// depends on context. Measured on this machine: 30 individual runs of `go
// test ./internal/cli/ -run
// 'TestRunDaemonUnparksAfterAListenerFailureAfterServe$'` catch it 0/30
// under default GOMAXPROCS and 9/30 under `go test -cpu=1`; 20 runs of the
// whole `go test ./internal/cli/` package catch it 0/20 under default
// GOMAXPROCS and 0/20 under -cpu=1 too — the opposite of the
// listen-failure site above (TestRunDaemonDrainsOwnersWhenListenFails's
// own doc comment), where -cpu=1 and full-package context both raise the
// kill rate. Recorded as measured, not assumed from either that sibling
// case or a prior run of this same test on a different machine: a bare
// fraction with no stated context is not a fact worth keeping.
func TestRunDaemonUnparksAfterAListenerFailureAfterServe(t *testing.T) {
	var captured net.Listener
	orig := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if err == nil {
			captured = ln
		}
		return ln, err
	}
	defer func() { listenTCP = orig }()

	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	stderr := newSyncBuf()
	wakerDone := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:    io.Discard,
			Stderr:    stderr,
			Listen:    "127.0.0.1:0",
			Sink:      sink,
			Tokens:    tm,
			Cache:     store.NewCache(store.Store{Dir: t.TempDir()}),
			Owners:    own,
			WakerDone: wakerDone,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for runDaemon to start listening")
	}
	if captured == nil {
		t.Fatal("listenTCP was never called before the startup line was printed — did runDaemon stop calling the overridable seam?")
	}
	// Same connection captured's Accept loop is blocked on; closing it
	// forces srv.Serve to return a non-ErrServerClosed error, driving
	// serve's <-errc branch with ctx still live — never ctx.Done().
	captured.Close()

	select {
	case code := <-daemonDone:
		if code != 1 {
			t.Fatalf("runDaemon returned %d, want 1 on a post-startup listener failure", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return after its listener failed post-startup: the post-serve stopWaker()/<-wakerStopped pairing (F84) did not unpark the waker — runDaemon is parked on <-wakerStopped forever, exactly the hang F84 describes")
	}

	select {
	case <-wakerDone:
	default:
		t.Fatal("WakerDone was not closed by the time runDaemon returned: the wake detector was not joined on the post-serve listener-failure path")
	}

	// ctx is still live at this point (this test's own deferred cancel()
	// has not run yet, and nothing else in runDaemon cancels the caller's
	// ctx on this branch) — the only thing that explains runDaemon having
	// already returned, and WakerDone already being closed, is its own
	// stopWaker()/<-wakerStopped pairing, not ctx cancellation.
	select {
	case <-ctx.Done():
		t.Fatal("ctx was already cancelled: that would make this test unable to tell the post-serve join from the listen-failure site's ctx-eventually-cancelled behaviour")
	default:
	}
}

// TestCloseOwnersJoinsRosterWatcherBeforeClosingTheMap pins the F1 hazard
// Task 4's review raised and this task's closeOwners exists to close:
// watchRoster calls forget every 5s, and if closeOwners closes the map
// without first stopping AND joining that goroutine, a Forget already
// in-flight can land after Close and return owners.ErrClosed — dropped,
// per owners.Close's own doc comment ("the caller sees the loss rather
// than a false success", but only if the caller is listening; here the
// caller is watchRoster's onError, which prints to stderr).
//
// An earlier version of this test raced a real 5s tick against a real
// shutdown, landing cancel() inside a ~150-200ms window a 500k-account
// roster reload was made to force — a window whose actual size is a
// property of the machine decoding that JSON, not of the code. It never
// failed on the machine that wrote it and passed 3/3, then 6/6, on
// another (confirmed: the mutated closeOwners deleted 3/3 passed there —
// the same mistake this project already has a finding about, where a
// sampler's silence was mistaken for a guarantee). Deleted.
//
// This version controls the interleaving instead of racing it, using the
// forget parameter this task's fix round 2 restored specifically for
// this: forget parks on a channel the moment watchRoster calls it, so the
// test can cancel ctx (letting closeOwners run) and only THEN release it
// — no timing guess decides whether Forget is "in flight" when Close
// runs, the test does. It also drives d.RosterTick itself (item ii) rather
// than waiting out two real 5s ticks: the first tick sets the baseline
// (synchronized via d.RosterProcessed, since that tick never calls
// forget), the second diffs "A" as missing and calls forget, which parks
// exactly as before. This is what took this test from ~10.3s to
// milliseconds — RosterTick is purely observational, so driving it by hand
// exercises the identical production code path a real 5s ticker would.
func TestCloseOwnersJoinsRosterWatcherBeforeClosingTheMap(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "A")}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(dir, "B")}); err != nil {
			return err
		}
		st.Serving, st.Remote = "B", "B" // so Remove("A") below is allowed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	// Forget is a no-op (never reaches queueLocked, so it can never see
	// ErrClosed) unless "A" actually owns something — seed a record so the
	// tick that forgets "A" really does attempt a write.
	if err := own.Record(router.KindArtifact, []string{"artifact-1"}, "A", time.Now()); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1) // own.Forget("A")'s own return value, not a stderr scrape
	var enteredOnce sync.Once
	forget := func(name string) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		err := own.Forget(name)
		result <- err
		return err
	}

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()

	tick := make(chan time.Time)
	processed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	// Every t.Fatal above the explicit cancel() below fires before forget
	// ever parks on release, so this alone unwinds the daemon, the
	// watcher, and the waker instead of leaking them into the rest of the
	// test binary — competing with later runs under -shuffle=on -count=N.
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Cache:           cache,
			Sink:            sink,
			Tokens:          tm,
			Owners:          own,
			RosterTick:      tick,
			RosterProcessed: processed,
			Forget:          forget,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	// First tick: the baseline. known starts nil, so this never diffs and
	// never calls forget — it only sets known={A,B}. Synchronized on
	// RosterProcessed rather than timed, since this tick's own send to
	// processed is the only thing it does after the (empty) diff.
	select {
	case tick <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never consumed the baseline tick")
	}
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never finished processing the baseline tick")
	}

	if _, err := s.Update(func(st *store.State) error { return st.Remove("A") }); err != nil {
		t.Fatal(err)
	}

	// Second tick: diffs "A" as missing against the baseline and calls
	// forget("A"), which parks on release — entered (not processed) is
	// what synchronizes on this one, since RosterProcessed is not sent to
	// until forget itself returns.
	select {
	case tick <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never consumed the diff tick")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal(`watchRoster never called forget("A") — is the roster diff, or the injected forget seam, wired?`)
	}

	cancel() // closeOwners runs now; with the join, it blocks on <-rosterDone
	// Give a MUTATED closeOwners (own.Close() with no join) time to run:
	// unlike the deleted test, this is a one-sided floor on a near-instant,
	// non-blocking call (own.Close with nothing else pending), not a
	// ceiling shared with a variable-speed decode — any machine clears it
	// by a wide margin. With the join present, closeOwners is already
	// blocked on <-rosterDone at this point regardless of how long we wait
	// here, so this sleep cannot produce a false pass on the correct code.
	time.Sleep(200 * time.Millisecond)
	close(release)

	// own.Forget("A")'s own return value, not a stderr scrape: a stronger
	// assertion than strings.Contains(out, "owners: closed"), which would
	// also pass if Forget failed for some unrelated reason, and removes
	// the need to sleep before reading stderr at all.
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("own.Forget(\"A\") = %v, want nil: a Forget call landed after Close and was dropped (F1 hazard)", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal(`forget("A") never returned after release`)
	}

	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}
