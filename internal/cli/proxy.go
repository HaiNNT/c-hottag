package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/rotate"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/stickyval"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

// runProxy runs the routing proxy in the foreground. M1c3 turns this into
// the launchd/systemd daemon. Usage is NOT polled on an interval — it is
// observed from responses chottag already proxies (F23, spec 6.3); a
// /api/oauth/usage poll is the fallback for an account that has sent no
// traffic. The status cache shipped in M1c.
func runProxy(args []string, r *reporter) int {
	stdout, stderr := r.Stdout(), r.Stderr()
	// sig is nil: installShutdown registers the real signal.Notify channel.
	// runProxyWithSignal exists so a test can supply its own channel
	// instead and drive both signals — including cleanly stopping a
	// daemon it started, from a t.Cleanup, without a real OS signal ever
	// touching the process (N3).
	//
	// startupErr is nil here: this is the foreground `proxy run`, whose own
	// stderr parameter already IS the real process stderr, so a second copy
	// of a pre-serve failure would double every message on it.
	return runProxyWithSignal(args, stdout, stderr, nil, nil)
}

// newStatusSinkFn is newStatusSink, overridable so a test can force this
// exact failure — own already open, sink creation failing — without a real
// disk failure. Production never overrides it. own.Close() right after this
// call is otherwise unpinned: nothing else in the package exercises it, so
// deleting it left the whole package green before this seam existed.
var newStatusSinkFn = newStatusSink

// ownersOpenFn is owners.Open, overridable so a test can capture the exact
// *owners.Map this function opens and probe it afterwards (a write
// returning owners.ErrClosed proves Close ran — the same technique
// TestRunDaemonDrainsOwnersWhenListenFails uses on runDaemon's own
// daemonDeps.Owners). runProxyWithSignal builds this Map itself rather than
// taking it as a parameter, so that injection point does not reach it;
// production never overrides this one either.
var ownersOpenFn = owners.Open

// credsReadForTest overrides the credential read every account slot uses
// (creds.Reader.Read in production, which on darwin execs the real
// /usr/bin/security). nil in every production build. A wiring test (item
// 3, review round 2) sets it to hand the daemon's real token manager a
// fabricated, already-valid token for a fake slot, so the real chooser
// genuinely swaps a bearer and drives a real proxied round trip — without
// ever touching the real macOS Keychain or a real credentials file.
var credsReadForTest func(configDir string) (creds.Token, error)

// proxyDialContextForTest and proxyUpstreamRootCAsForTest are proxy.Config's
// own "Test hooks" (its doc comment: nil means the real net.Dialer and the
// system roots), threaded through runProxyWithSignal so a wiring test can
// redirect every upstream dial to a local fake backend and trust its
// certificate. Both are nil in every production build.
var (
	proxyDialContextForTest     func(ctx context.Context, network, addr string) (net.Conn, error)
	proxyUpstreamRootCAsForTest *x509.CertPool
)

// startupWriter is where a failure before the daemon starts serving is
// written: stderr, plus startupErr when that is non-nil. It is the ONE place
// the double write happens (spec §4.6, F67, F109), and every such path
// uses it. runProxyWithSignal's fail and flag output, runDaemon's bind, and
// runDaemonRun's own checks all go through it, so no single path gets it
// right while its siblings reach daemon.log alone, which is F109 exactly.
// For `daemon run`, stderr is daemon.log and startupErr is the process's
// real stderr, which is what launchd's StandardErrorPath and the systemd
// journal read. For the foreground `proxy run`, startupErr is nil and
// stderr already IS the real one, so nothing is doubled.
//
// The two-destination case is NOT io.MultiWriter (fix round 1, I1):
// MultiWriter stops at the first destination that returns an error, and
// stderr — daemon.log for `daemon run` — is that first destination. A
// daemon.log write can itself fail (a rotation failure, or a full disk),
// exactly the moment a start is already failing, and MultiWriter would then
// silently drop the message from startupErr too: the one stream launchd's
// StandardErrorPath and the systemd journal actually read. allWriter below
// writes to every destination regardless of an earlier one's error.
func startupWriter(stderr, startupErr io.Writer) io.Writer {
	if startupErr == nil {
		return stderr
	}
	return allWriter{stderr, startupErr}
}

// allWriter writes p to every writer in the slice, unconditionally: unlike
// io.MultiWriter, an error from one writer never stops the rest from being
// tried. Write reports len(p) and the FIRST error encountered (nil if every
// writer succeeded) — the same shape io.MultiWriter uses for the case that
// matters to every caller here, which only checks the error to log or
// retry, never to decide how many bytes were "really" written.
type allWriter []io.Writer

func (a allWriter) Write(p []byte) (int, error) {
	var firstErr error
	for _, w := range a {
		if _, err := w.Write(p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(p), firstErr
}

func runProxyWithSignal(args []string, stdout, stderr, startupErr io.Writer, sig chan os.Signal) int {
	// Every return before serving is a failed start, and each one goes
	// through fail, never a bare fmt.Fprint(stderr, ...). A new pre-serve
	// return that writes to stderr directly is F109 again:
	// TestEveryPreServeFailureReachesTheRealStderr needs a row for it.
	preServe := startupWriter(stderr, startupErr)
	fail := func(code int, msg string) int {
		fmt.Fprintln(preServe, msg)
		return code
	}
	if len(args) == 0 || args[0] != "run" {
		return fail(exit.Usage, strings.TrimSuffix(usage, "\n"))
	}
	fs := flag.NewFlagSet("proxy run", flag.ContinueOnError)
	// flag writes a parse error itself, so its output is the pre-serve
	// writer too.
	fs.SetOutput(preServe)
	h, err := home()
	if err != nil {
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}
	listen := fs.String("listen", "127.0.0.1:"+strconv.Itoa(store.DefaultPort), "proxy address (loopback)")
	logPath := fs.String("log", filepath.Join(h, "proxy.jsonl"), "request log")
	claudeFlag := fs.String("claude", "", "path to the real claude binary used to refresh a slot login (default: the real claude on PATH, never chottag's own shim)")
	upstream := fs.String("upstream-proxy", "", "route chottag's own upstream traffic through this proxy (e.g. http://127.0.0.1:3128)")
	if fs.Parse(args[1:]) != nil {
		return exit.Usage
	}
	if listenPortProblem(*listen) {
		return fail(exit.Usage, fmt.Sprintf("chottag: --listen %q: port must be 0-65535", *listen))
	}
	if !isLoopbackListen(*listen) {
		return fail(exit.Usage, fmt.Sprintf("chottag: --listen %q must be loopback", *listen))
	}

	upstreamURL, err := parseUpstreamProxy(*upstream)
	if err != nil {
		return fail(exit.Usage, fmt.Sprintf("chottag: --upstream-proxy %q: %v", redact.UpstreamProxy(*upstream), err))
	}

	// Explicit, not incidental: ca.LoadOrCreateLocked happens to MkdirAll
	// this directory as a parent today, so the 0700 is an ordering accident
	// that a reordering would silently undo. This also does nothing to an
	// EXISTING directory's mode — see doctor, which is where a wrong mode
	// on an existing ~/.chottag gets reported.
	if err := os.MkdirAll(h, 0o700); err != nil {
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}

	authority, err := ca.LoadOrCreateLocked(filepath.Join(h, "ca"))
	if err != nil {
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}
	secret, err := proxyauth.LoadOrCreate(h)
	if err != nil {
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}
	lw, err := openTraceLog(*logPath)
	if err != nil {
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}
	if lw != nil {
		defer lw.Close()
	}
	own, err := ownersOpenFn(filepath.Join(h, "owners.json"))
	if err != nil {
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}
	// A warning, not a failure: startup continues, so this stays on stderr
	// alone, like every other routine daemon line (spec §4.6 doubles
	// failures only).
	if own.Recovered {
		fmt.Fprintln(stderr, "chottag: owners.json was corrupt and has been moved aside")
	}
	own.SetOnError(ownersSaveErrorThrottle(stderr))

	cache := store.NewCache(store.Store{Dir: h})
	reader := creds.Reader{GOOS: runtime.GOOS, Run: creds.ExecRunner}
	read := reader.Read
	if credsReadForTest != nil {
		read = credsReadForTest
	}
	read = evidenceRead(read, stderr)
	// The reporter's sink and notifier do not exist yet; they are set below,
	// before the daemon runs and so before any refresh can report.
	rr := &refreshReporter{log: stderr, now: timeNow}
	tm := tokens.New(tokens.Config{
		Read:        read,
		Refresh:     newRefresher(*claudeFlag, h),
		LockPath:    creds.LockPath,
		TryLock:     fsutil.TryLock,
		OnRefresh:   rr.onRefresh,
		AccountName: accountNameByDir(cache.State),
	})

	sink, err := newStatusSinkFn(h, statusSaveErrorThrottle(stderr))
	if err != nil {
		// own is already open at this point (item iii, Task 5's review):
		// nothing has queued a write against it this early, so there is no
		// durability loss either way, but "nothing writes this early, so
		// it's fine" is exactly the reasoning F71 is a finding about — the
		// writer goroutine own.Open started would otherwise leak silently
		// on this exit. Pinned by
		// TestRunProxyWithSignalClosesOwnersWhenNewStatusSinkFails
		// (newStatusSinkFn is the injectable seam): deleting this call leaves
		// the whole package green otherwise.
		own.Close()
		return fail(exit.Error, fmt.Sprintf("chottag: %v", err))
	}
	// Seed a row for every configured account before observing anything:
	// Limits.AllLimited must be computed over the accounts state.json
	// registers, not just the ones traffic happens to have touched so far
	// (contract 2).
	sink.dropTokenPassthroughs()
	seedRosterAtStartup(cache.State, sink)
	// The release checklist's fake-limit hook (spec §10.1, R52): a no-op
	// unless this binary was built with -tags chottag_fakeusage. It runs
	// after the seed, so every named row exists, and before the poller is
	// built, so the poller's first roster sync already sees the simulated
	// limit and schedules a reset poll rather than a start poll.
	applyFakeLimits(os.Getenv, cache.State, sink, stderr, time.Now())

	// Desktop notifications (M2 spec §4): one state machine and one
	// dispatcher for this daemon generation (D11). Closed once runDaemon
	// has returned; nothing feeds it after that.
	dn := newDaemonNotify(cache.State, newDaemonNotifier())
	defer dn.Close()
	rr.sink, rr.dn = sink, dn

	printEvent := newEventPrinter(stderr, timeNow)
	sel := selector.New(newSelectorConfig(cache.State, tm, own, sink, printEvent, dn))
	ch := newDaemonChooser(sel, own, tm, cache.State, dn)

	// Auto-switch (M4 spec §4, §4a): the planner runs from the usage hook,
	// the roster tick and the wall retry. The fake-util knob is a no-op
	// outside the chottag_fakeusage build (fakeutil_off.go).
	fake := applyFakeUtil(os.Getenv, cache.State, sink, stderr, time.Now())
	as := newAutoSwitcher(store.Store{Dir: h}, cache, sink, dn, stderr, fake)
	as.tokenStatus, as.warmFor = tm.Status, tm.WarmFor
	as.awaitToken = func(ctx context.Context, dir string) { tm.Await(ctx, dir) }

	// The spread policy's placements (M7): loaded now, so a daemon restart
	// keeps every session on its account; saved on change and at shutdown.
	sp := newSpreadEngine(filepath.Join(h, "run", "placements.json"), cache.State, sink.fileCopy, ch.tracker.Peek, stderr, time.Now())
	ch.spread, as.spread = sp, sp
	ch.log, ch.refresh = stderr, cache.Invalidate
	stickyAccounts := wireStickyValidate(ch, h, stderr)
	sp.lastAccount = ch.tracker.Account

	cfg := wireProxyConfig(stderr, authority, secret, lw, ch, autoUsageHook(as, newUsageHook(cache.State, sink, dn)), upstreamURL)
	cfg.WallRetry = as.wallRetry
	cfg.OnServingRefusal = servingRefusalHook(stderr, dn)
	// Test hooks only (proxy.Config's own doc comment: nil = the real
	// net.Dialer and the system roots). Both are nil in every production
	// build; a wiring test sets them to redirect every upstream dial to a
	// local fake backend and trust its certificate, so a genuine proxied
	// round trip can drive the usage hook and the wall retry without ever
	// touching the real network (item 3, review round 2).
	cfg.DialContext = proxyDialContextForTest
	cfg.UpstreamRootCAs = proxyUpstreamRootCAsForTest

	// Trace mode (M2c spec §3): the switch in state.json is read per
	// request; while it is on, records with shapes go to a bounded
	// trace.jsonl, and the Claude Code version seen goes to status.json.
	// Closed once runDaemon has returned.
	dt := newDaemonTrace(h, cache.State, sink)
	defer dt.Close()
	cfg = dt.wire(cfg)

	// The usage poll (spec §6.4): stderr is daemon.log under `daemon run`,
	// and upstreamURL is the same resolved --upstream-proxy the forwarding
	// transport uses (F103: one resolution, two consumers).
	var poller pollScheduler
	if p := newDaemonPoller(stderr, tm, sink, upstreamURL); p != nil {
		sink.setOnObserved(p.Observed)
		// A poll that finds no login records it where the planner and the
		// notifier look (F269), then the poller leaves the account alone.
		p.SetOnNeedsLogin(func(account string) {
			fmt.Fprintf(stderr, "chottag: usage poll %s: no usable login; not polled until it logs in again (run: chottag login %s)\n", account, account)
			recordNeedsLogin(sink, dn, account)
		})
		p.SetOnOK(func(account string) { recordRecovered(sink, dn, account, timeNow()) })
		as.pollNow, as.pollSkip = p.PollNow, p.PollSkip
		poller = p
	}

	ul := newUpdateLoop(h, upstreamURL, cache.State, sink, dn, stderr)
	rl := newRestartLoop(h, cache.State, sink, dn, stderr)
	if ul != nil && rl != nil {
		ul.restartNotified = rl.NoteNotified // one notice per unattended update
	}

	// A single SIGINT/SIGTERM begins graceful shutdown (drains the sink,
	// closes tunnels, gives in-flight requests shutdownGrace). A SECOND
	// one arriving before that has finished skips the rest of the grace
	// period and exits immediately instead — the standard double-Ctrl-C
	// contract: a daemon that keeps absorbing repeated signals for up to
	// 5s trains people to reach for kill -9, which would skip the same
	// drain anyway, just without ever telling the user it did.
	return installShutdown(sig, func() {
		fmt.Fprintln(stderr, "chottag: second signal, exiting immediately — skipping the rest of the drain")
		os.Exit(130) // 128+SIGINT, distinct from 1 (a startup failure) so an aborted shutdown is not mistaken for one
	}, func(ctx context.Context) int {
		return runDaemon(ctx, daemonDeps{
			Stdout:     stdout,
			Stderr:     stderr,
			StartupErr: startupErr,
			Listen:     *listen,
			LogPath:    *logPath,
			Home:       h,
			Cfg:        cfg,
			Sink:       sink,
			Tokens:     tm,
			Cache:      cache,
			Owners:     own,
			Chooser:    ch,
			Poller:     poller,
			Notify:     dn,
			Auto:       as,
			Spread:     sp,
			Update:     ul,
			Restart:    rl,
			Session:    newSessionWatcher(h, stderr),
			Warm: &remoteWarmer{state: cache.State, warm: tm.Warm, probe: tm.LockedOut, sticky: stickyAccounts, log: stderr, now: timeNow,
				needsLogin: func(account string) { recordNeedsLogin(sink, dn, account) },
				wakeWarm: func(ctx context.Context, dir string, within time.Duration) (creds.Status, bool) {
					return tm.WarmFor(ctx, dir, within, tokens.TriggerWake)
				}},
			// nil in production: runDaemon's own RosterProcessed default.
			RosterProcessed: rosterProcessedForTest,
		})
	})
}

// newReloginHook is watchRoster's invalidate hook: when an account's
// LoggedInAt advances, the token cache stops trusting its old read, and the
// account's token-based marks (a needs-login or stale state, and a `token ...`
// passthrough text) are cleared, since the login that produced them has been
// replaced (F273). A fresh login that is in fact broken is marked again by the
// next poll, refresh or request. sink and dn may be nil. It runs off the
// roster goroutine, as the invalidate always has.
func newReloginHook(invalidate func(dir string), state func() (store.State, error), sink *statusSink, dn *daemonNotify, now func() time.Time) func(string) {
	return func(dir string) {
		// Marks first, then the cache: a read that started before the hook
		// can then only re-mark through a result the invalidate has already
		// discarded.
		if sink != nil && state != nil {
			if st, err := state(); err == nil {
				for _, a := range st.Accounts {
					if a.Dir == dir {
						recordRecovered(sink, dn, a.Name, now())
						break
					}
				}
			}
		}
		invalidate(dir)
	}
}

// pollScheduler is the part of *usagepoll.Poller runDaemon drives. An
// interface so a daemon test can record what runDaemon does with it.
type pollScheduler interface {
	Run(ctx context.Context)
	SyncRoster(accounts []usagepoll.Account)
	Wake()
}

// newDaemonPoller builds the daemon's usage poller against the real
// endpoint. It is a variable only so this package's TestMain can replace
// it with one that returns nil: a test that runs runProxyWithSignal with a
// registered account must never reach api.anthropic.com or the Keychain.
// Production never overrides it; nil means "no poller".
var newDaemonPoller = func(log io.Writer, tm usagepoll.TokenSource, sink *statusSink, upstream *url.URL) *usagepoll.Poller {
	return usagepoll.New(daemonPollerConfig(log, tm, sink, upstream, usagepoll.DefaultURL))
}

// daemonPollerConfig wires the poller to the status sink and the token
// manager. endpoint is a parameter so a test can aim the real wiring at a
// loopback server.
func daemonPollerConfig(log io.Writer, tm usagepoll.TokenSource, sink *statusSink, upstream *url.URL, endpoint string) usagepoll.Config {
	return usagepoll.Config{
		Fetch: usagepoll.NewFetcher(usagepoll.FetchConfig{
			URL:    endpoint,
			Client: usagepoll.NewClient(upstream),
			Tokens: tm,
		}),
		Cached: sink.cached,
		Apply:  sink.poll,
		Log:    log,
	}
}

// pollAccounts lists the accounts the poller may poll: every registered
// slot. store.State.Add requires a slot dir, so the Dir check only keeps a
// hand-edited state.json from handing the poller a slotless account.
func pollAccounts(st store.State) []usagepoll.Account {
	out := make([]usagepoll.Account, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		if a.Dir == "" {
			continue
		}
		out = append(out, usagepoll.Account{Name: a.Name, Dir: a.Dir, Rotates: a.Rotates(), LoggedInAt: a.LoggedInAt})
	}
	return out
}

// installShutdown wires signal handling around run: the first
// SIGINT/SIGTERM cancels run's ctx, beginning graceful shutdown; a second
// one, arriving before run returns, calls forceExit immediately instead of
// waiting out the rest of run's own grace period (F4).
//
// sig is nil in production, which registers a real signal.Notify channel
// and unregisters it (signal.Stop) once run has returned. A test supplies
// its own channel instead, so every path here — including signal.Notify
// itself — can be driven without a real OS signal ever reaching the
// process, and a leaked call (e.g. one that outlives its own test) can be
// stopped cleanly by sending on that channel rather than leaving a real
// handler armed inside the test binary (N3).
func installShutdown(sig chan os.Signal, forceExit func(), run func(ctx context.Context) int) int {
	real := sig == nil
	if real {
		sig = make(chan os.Signal, 2)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sig)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go awaitShutdownSignal(sig, cancel, done, forceExit)
	code := run(ctx)
	// Closed here, not deferred: the watcher above must stop treating a
	// signal as "still shutting down, force exit" at the exact moment run
	// returns, not whenever this function's other defers happen to
	// unwind.
	close(done)
	return code
}

// The trace log's bound. Same shape as daemon.log's (M1c3 Task 2), and the
// same reasoning at a much higher rate: daemon.log gets occasional startup
// and error lines, proxy.jsonl gets one line per proxied request and one per
// tunnel. Measured: a typical record is 127 bytes including the newline, so
// 8 MiB is roughly 66,052 requests per generation and five generations keep
// roughly 330,000 — enough history to investigate yesterday, bounded enough
// to survive a machine's lifetime.
const (
	traceLogMaxBytes = 8 << 20
	traceLogKeep     = 5
)

// openTraceLog opens the bounded request log, or returns (nil, nil) when
// path is empty.
//
// A nil *tracelog.Writer in proxy.Config.Log is already a supported state —
// proxy.Server.log returns early on it — so "no trace log" needs no new
// branch in the proxy. That is the off-switch for a user who wants no
// request metadata on disk at all: the records carry hosts, templated paths
// and hashed id labels (tracelog.HashID — never the claude.ai object id
// itself, never tokens or bodies), and since M1c3 they are retained across
// generations rather than scrolling past a terminal.
func openTraceLog(path string) (*tracelog.Writer, error) {
	if path == "" {
		return nil, nil
	}
	rw, err := rotate.Open(rotate.Config{
		Path:     path,
		MaxBytes: traceLogMaxBytes,
		Keep:     traceLogKeep,
	})
	if err != nil {
		return nil, err
	}
	return tracelog.NewWriter(rw), nil
}

// newProxyConfig builds the proxy.Config runDaemon hands to proxy.New. It
// exists as its own function so upstream-proxy wiring can be asserted
// directly: a `UpstreamProxy: upstreamURL` silently shrinking to `nil`
// here compiles and left the whole suite green before this existed.
func newProxyConfig(authority *ca.Authority, secret proxyauth.Secret, lw *tracelog.Writer, ch *chooser, onUsage func(account string, status int, h http.Header), onUsageError func(error), onLogError func(error), upstreamURL *url.URL) proxy.Config {
	return proxy.Config{
		CA:            authority,
		ProxyAuth:     secret,
		Intercept:     router.Intercepted,
		Log:           lw,
		Choose:        ch,
		OnUsage:       onUsage,
		OnUsageError:  onUsageError,
		OnLogError:    onLogError,
		UpstreamProxy: upstreamURL,
		Version:       Version,
	}
}

// wireProxyConfig is runProxyWithSignal's only call to newProxyConfig,
// pulled out to its own function so it can be tested directly rather than
// only reachable through a full runProxyWithSignal invocation (fix round 2,
// item 3). usageErrorThrottle and logErrorThrottle are two adjacent,
// same-typed func(error) values — the reviewer transposed them at what was
// then the inline call site and the whole tree stayed green, since nothing
// asserted a *proxy.Config's OnUsageError/OnLogError actually route to the
// throttle their own names promise (an OnUsage panic must print "usage
// callback failed", never "trace log write failed", and vice versa — the
// entire point of giving OnUsage its own error channel, Task 6). See
// TestWireProxyConfigRoutesErrorsToTheirOwnThrottle, which calls this exact
// function and would fail if the two arguments below were ever swapped
// again.
func wireProxyConfig(stderr io.Writer, authority *ca.Authority, secret proxyauth.Secret, lw *tracelog.Writer, ch *chooser, onUsage func(account string, status int, h http.Header), upstreamURL *url.URL) proxy.Config {
	return newProxyConfig(authority, secret, lw, ch, onUsage, usageErrorThrottle(stderr), logErrorThrottle(stderr), upstreamURL)
}

// parseUpstreamProxy validates --upstream-proxy's raw flag value and
// reports nil, nil for an unset flag (direct dialling). It is pure and
// side-effect free on purpose: its rejections can then be asserted
// directly, rather than only through a full runProxy invocation that — if
// this validation ever regressed — would fall through to actually binding
// and serving on the real default port (F6).
//
// http ONLY, deliberately. Transport.Proxy speaks TLS to an https://
// proxy and SOCKS5 to a socks5:// one, but dialThroughProxy speaks
// neither — it would send a plaintext CONNECT into a TLS listener.
// Accepting those schemes would give a split brain: API traffic works
// while every blind tunnel breaks, which is miserable to diagnose. Reject
// them at the door until dialThroughProxy learns TLS (F44).
func parseUpstreamProxy(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse wraps a syntax failure in a *url.Error whose Error()
		// repeats the raw URL verbatim — including any userinfo password —
		// so a caller that logs this err as-is echoes the secret a second
		// time even after redacting its own copy of raw. Unwrap to just the
		// underlying reason instead.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, uerr.Err
		}
		return nil, err
	}
	if u.Scheme != "http" {
		return nil, errors.New("only http:// is supported")
	}
	if u.Host == "" {
		return nil, errors.New("no host")
	}
	return u, nil
}

// awaitShutdownSignal blocks until sig delivers a first signal (or done
// closes first, meaning the daemon exited before any signal arrived, in
// which case there is nothing left to do), then calls cancel to begin the
// graceful shutdown path. If a SECOND signal arrives before done closes —
// the daemon has not finished shutting down yet — it calls forceExit
// instead of waiting out the rest of the grace period.
//
// Each blocking select is preceded by its own non-blocking check of done
// alone: once done has closed, this must return (or decline to force an
// exit) even if sig ALSO already has a value buffered and Go's select
// would otherwise be free to pick either ready case — a real, if narrow,
// race (not just a test artifact): without the priority check, a signal
// that happens to still be sitting in the channel's buffer at the exact
// moment the daemon finishes shutting down could non-deterministically
// win a select against the already-closed done and report a forced exit
// for a shutdown that had, in fact, already completed gracefully.
//
// The check ahead of the final select narrows that race but cannot close
// it completely: done can still close in the gap between that check and
// the final select actually blocking, at which point both cases are ready
// together and Go's select picks between them at random. So the case that
// receives the second signal calls forceExitUnlessDone instead of
// forceExit directly — one more non-blocking check of done, immediately
// before forceExit, narrowing the window a second time down to the two
// receives themselves, rather than leaving forceExit reachable from a
// select that already had done ready as an alternative (F3b).
func awaitShutdownSignal(sig <-chan os.Signal, cancel func(), done <-chan struct{}, forceExit func()) {
	select {
	case <-done:
		return
	default:
	}
	select {
	case _, ok := <-sig:
		if !ok {
			return
		}
	case <-done:
		return
	}
	cancel()
	select {
	case <-done:
		return
	default:
	}
	select {
	case _, ok := <-sig:
		if ok {
			// Load-bearing, and NOT covered by any test: reverting this to a
			// bare forceExit() leaves the whole suite green (no deterministic
			// construction can close `done` inside the window between the
			// check above and this select — see forceExitUnlessDone). Changing
			// this line needs review, not a green run.
			forceExitUnlessDone(done, forceExit)
		}
	case <-done:
	}
}

// forceExitUnlessDone is awaitShutdownSignal's final line of defence: by
// the time its last select has already committed to "a second signal
// arrived", done may ALSO have closed in the meantime — a channel close
// and a channel send racing to wake the same parked select is a genuine
// tie Go's runtime can settle either way, not just a theoretical concern
// (F3b: without a re-check here, a second, no-op signal arriving right as
// an already-graceful shutdown finishes reports a forced exit instead,
// non-deterministically).
//
// Pulled into its own function so the check itself can be asserted
// directly (done already closed → forceExit must not run; done still
// open → it must — see TestForceExitUnlessDoneSkipsWhenDoneIsAlreadyClosed
// and TestForceExitUnlessDoneRunsWhenDoneIsStillOpen), rather than only by
// constructing the goroutine race it defends against: that race is real,
// but by its nature cannot be made to land in this exact few-instruction
// window on every run without the test itself becoming flaky on entirely
// correct code (see TestAwaitShutdownSignalNeverCallsCancelWhenDoneIsAlreadyClosed's
// comment for the measurements behind that call).
func forceExitUnlessDone(done <-chan struct{}, forceExit func()) {
	select {
	case <-done:
	default:
		forceExit()
	}
}

// newWakeHandler builds the wake detector's OnWake. OnWake runs
// SYNCHRONOUSLY on the waker's own select loop (WakerConfig.OnWake's
// documented contract), so it must return promptly: closeUpstreams is
// genuinely non-blocking (it only closes already-open connections it
// already tracked — see internal/proxy/conntrack.go's closeAll, which
// closes outside its lock precisely because a Close can block, and
// CloseIdleConnections, which never touches the network) and stays
// synchronous here. invalidateAll can block on a slot's mutex, though: if
// that slot is mid-read through creds.ExecRunner (`security
// find-generic-password` on darwin) and the Keychain is locked — exactly
// the state a lid-open resume can leave things in — the mutex will not
// release until a user dismisses a prompt or the exec call itself gives
// up, and nothing bounds that today. So invalidateAll runs on its own
// goroutine, single-flighted: a second wake arriving while one
// invalidation is still in flight is dropped, not queued, because the
// in-flight call already does the work the second one would ask for.
func newWakeHandler(closeUpstreams func() int, invalidateAll func(), logWake func()) func() {
	var invalidating atomic.Bool
	return func() {
		closeUpstreams()
		if invalidating.CompareAndSwap(false, true) {
			go func() {
				// Both the guard release and the recover live in this ONE
				// deferred func, and in that order. recover() only stops a
				// panic when called directly by the function defer
				// invokes — a separate `defer recover()` would not work —
				// so it has to sit alongside the Store(false) rather than
				// in a defer of its own. And the guard release must run
				// whether or not invalidateAll panics: recover() alone
				// stops the panic from taking the whole daemon down, but
				// if the guard were released only on the normal return
				// path, a single panic here would strand `invalidating`
				// at true forever, silently dropping every future wake for
				// the rest of the process's life. This project already
				// shipped that exact bug class once — see refreshDetached
				// in internal/tokens/tokens.go, a panicking refresher
				// whose recover skipped its own backoff reset.
				defer func() {
					invalidating.Store(false)
					recover()
				}()
				invalidateAll()
			}()
		}
		logWake()
	}
}

// shutdownGrace bounds how long a shutdown waits for in-flight requests
// before the tunnels are closed under them. Short on purpose: a stuck
// upstream must not stop the daemon from exiting, and spec §6 wants a
// client to see the same reset it would see without chottag.
const shutdownGrace = 5 * time.Second

// daemonHeartbeatInterval bounds how long the daemon object's status.json
// write goes without a fresh stamp when none of its three counters have
// changed. The roster tick's own stamp call (runDaemon, below) skips the
// STAMP — never the tick's own roster work, which always runs — when
// nothing changed and the last stamp is younger than this — F94: the stamp
// is its OWN write (queueLocked → WriteBytes → f.Sync()), not a free ride
// on an existing one, so an idle daemon ticking every 5s must not fsync
// every 5s. Paired with status.DaemonStaleAfter (three missed heartbeats'
// tolerance): see that constant's own comment.
const daemonHeartbeatInterval = 30 * time.Second

// daemonDeps carries what runDaemon needs. It is a struct rather than a
// positional list because the old signature's fourteen positional
// parameters were unreadable on their own, and three of them — listen,
// logPath and h — were adjacent, same-typed strings: a transposition
// among those three compiled cleanly and was a real bug, one named
// fields eliminate.
type daemonDeps struct {
	Stdout, Stderr io.Writer
	Listen         string
	LogPath        string
	Home           string
	Cfg            proxy.Config
	Sink           *statusSink
	Tokens         *tokens.Manager
	Cache          *store.Cache
	Owners         *owners.Map
	// StartupErr, if non-nil, is the process's real stderr. A failure
	// before serving is written to it as well as to Stderr, through
	// startupWriter, which is the one helper every pre-serve path uses
	// (spec §4.6, F67, F109). For the supervised daemon, Stderr is
	// daemon.log (runDaemonCmd wires both of runProxyWithSignal's output
	// slots to the rotating logw), and a supervisor reads its own log, not
	// daemon.log. runDaemon's own use is the bind failure. nil means "no
	// separate startup stream" and must be safe: runProxy (the foreground
	// `proxy run`, whose Stderr already IS the real one) passes nil, since
	// writing the same message twice would double it there. Every literal
	// before this field existed omits it, as the Chooser field's doc
	// comment above already establishes for this struct.
	StartupErr io.Writer
	// Chooser, if non-nil, is the same *chooser wired into Cfg.Choose. It is
	// carried separately (rather than reached for via Cfg) because runDaemon
	// needs its OwnerWriteDrops counter, which proxy.Chooser's interface
	// does not expose, for the daemon object stamped on the roster tick.
	// nil (every daemonDeps literal before this field existed) reads as zero
	// drops, never a panic.
	Chooser *chooser
	// Srv, if non-nil, is used as-is instead of building a fresh
	// proxy.New(d.Cfg). Production always passes nil. The seam exists so a
	// test can hand runDaemon a *proxy.Server that already carries a real,
	// non-zero RouteDrift count — driven through proxytest.Start against a
	// refusing fake upstream, the same way internal/proxy's own
	// TestSafetyNetRouteDriftCountsConcurrentSwaps does — rather than
	// asserting the daemon-object stamp carries a RouteDrift merely != 0
	// (F94 Item 2: replacing all three stamped counters with literal
	// zeroes left the whole suite green before this seam and the test
	// using it existed).
	Srv       *proxy.Server
	Waker     proxy.WakerConfig
	WakerDone chan<- struct{}
	// RosterTick, if non-nil, replaces the real 5-second roster ticker
	// watchRoster reads from. Production always passes nil (runDaemon then
	// calls newRosterTicker(rosterTickInterval)). Observational only — unlike
	// Forget below, it cannot change what production DOES, only how a
	// test observes it — so a test can drive a tick synchronously instead
	// of waiting out a real 5s interval.
	RosterTick <-chan time.Time
	// RosterProcessed, if non-nil, is passed through to watchRoster as its
	// own processed channel: watchRoster sends on it once EVERY tick has
	// been fully handled, whether or not that tick forgot anything.
	// Production always passes nil.
	RosterProcessed chan<- struct{}
	// Forget overrides what the roster watcher calls, and is nil in every
	// production call site — runDaemon defaults it to Owners.Forget. Task 5
	// added it as a parameter so a test can park the watcher inside Forget
	// and control the shutdown interleaving deterministically, replacing a
	// sleep-calibrated race probe whose mutation kill did not survive moving
	// to another machine (F77). Keep the nil-means-default behaviour exactly
	// as it is: a field that silently changes production behaviour when a
	// caller sets it by accident is worse than no seam at all.
	Forget func(string) error
	// Rename overrides what the roster watcher calls on a Dir-keyed name
	// change, and is nil in every production call site — runDaemon defaults
	// it to Owners.RenameAccount (F174). Mirrors Forget's own seam, for the
	// same reason: a test can park the watcher inside it, or substitute a
	// fake to observe exactly which (from, to) pairs a tick produced without
	// wiring a full *owners.Map.
	Rename func(from, to string) (int, error)
	// Poller, if non-nil, is the usage poller (spec §6.4). runDaemon runs
	// it on the roster watcher's context, feeds it the roster on the
	// watcher's startup stamp and every tick, tells it about a wake, and
	// joins it before the owner map and the sink close. nil (every test
	// literal, and production when newDaemonPoller returns nil) runs no
	// poller.
	Poller pollScheduler
	// Notify, if non-nil, is this daemon generation's notifications (M2
	// spec §4). The roster tick reports route drift and the limit roll-up
	// to it, and stamps its error count as daemon.notifyErrors. nil (every
	// daemonDeps literal before M2b) posts nothing and stamps 0.
	Notify *daemonNotify
	// Auto, if non-nil, is the auto-switcher (M4 spec §4). The roster tick
	// runs it before the notification roll-up, so a switch at a reset
	// replaces "available again" (S8). nil (every daemonDeps literal
	// before M4) switches nothing.
	Auto *autoSwitcher
	// Spread, if non-nil, is the spread policy's placement engine (M7): the
	// roster tick prunes dead sessions' placements and saves a debounced
	// change. nil (every literal before M7) does nothing.
	Spread *spreadEngine
	// Update, if non-nil, is the update-check loop (R124). runDaemon runs it
	// on the roster watcher's context and joins it before the sink closes.
	// nil (a dev build, CHOTTAG_NO_UPDATE_CHECK=1, every test literal) never
	// checks.
	Update *updateLoop
	// Restart, if non-nil, is the restart-when-idle loop (R126). runDaemon
	// binds the proxy's idle signal to it (unless a test already set one),
	// runs it on the roster watcher's context, wakes it on a wake from
	// sleep, and joins it before the sink closes. nil (a dev build, every
	// test literal) never restarts the daemon.
	Restart *restartLoop
	// Session, if non-nil, ends this daemon once its login session is gone
	// (R164, F272). It stops the daemon the way a signal does.
	Session *sessionWatcher
	// Warm, if non-nil, keeps every pool's remote account's token fresh
	// (R147). The roster tick kicks it; at shutdown runDaemon cancels it
	// and joins it for a bounded time (warmStopBound) before the owner map
	// closes. nil (every test literal) warms nothing.
	Warm *remoteWarmer
}

// rosterTickInterval is how often the roster watcher ticks in production.
const rosterTickInterval = 5 * time.Second

// newRosterTicker starts the roster ticker runDaemon falls back to when
// d.RosterTick is nil, and returns its channel and its stop func. It is a
// var only so a test that drives the daemon through runProxyWithSignal,
// which builds its own daemonDeps, can send the ticks itself instead of
// betting on where a real 5 s tick lands (part 5). Production never
// overrides it.
var newRosterTicker = func(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// rosterProcessedForTest, if non-nil, is the RosterProcessed channel
// runProxyWithSignal hands runDaemon: the roster watcher sends on it once
// each tick is fully handled. Production leaves it nil. Only a test that
// also injects its ticks (newRosterTicker) sets it, since the watcher
// blocks on the send until someone receives.
var rosterProcessedForTest chan<- struct{}

// listenTCP is net.Listen, overridable so a test can capture the exact
// net.Listener runDaemon ends up serving on and close it out from under the
// running http.Server — reproducing serve's <-errc branch (a listener
// failure AFTER Serve has already started, not net.Listen itself failing)
// without a second process and without touching a fixed or privileged port.
var listenTCP = net.Listen

// runDaemon builds the proxy server (from d.Cfg, unless d.Srv already
// supplies one — see its own doc comment) and wake detector, starts
// the wake detector, listens, and serves until ctx is cancelled. Every
// dependency lives on d — runProxy supplies only the process's signal
// handling — so a test can drive this whole tail, including the wake
// detector with a fake clock (d.Waker's Ticks/Wall/Mono), without any OS
// signal or listening on a real, fixed port at all.
//
// d.WakerDone, if non-nil, is closed when the wake detector's own Run
// returns — the seam that catches Run ever being started on a context
// that never gets cancelled (it would then never return, so d.WakerDone
// would never close) as well as the wake detector never being started at
// all.
//
// d.Forget, if non-nil, replaces d.Owners.Forget as the function
// watchRoster calls. Production always passes nil (own.Forget). The seam
// exists so a test can control exactly when a tick's forget call is "in
// flight" — park it there deterministically, cancel ctx, and observe
// whether closeOwners's join makes own.Close() wait for it — rather than
// racing a real 5s tick against a real shutdown and hoping the window
// between "tick received" and "forget called" is wide enough to land in
// on whatever machine the suite runs on (see
// TestCloseOwnersJoinsRosterWatcherBeforeClosingTheMap's doc comment).
func runDaemon(ctx context.Context, d daemonDeps) int {
	// stopSelf lets a watcher inside the daemon begin the same graceful
	// shutdown a signal does (the session watcher, R164).
	ctx, stopSelf := context.WithCancel(ctx)
	defer stopSelf()
	if d.Spread != nil {
		defer d.Spread.close() // the last placements reach disk, so a restart keeps every session where it was
	}
	srv := d.Srv
	if srv == nil {
		srv = proxy.New(d.Cfg)
	}
	forget := d.Forget
	if forget == nil {
		forget = d.Owners.Forget
	}
	rename := d.Rename
	if rename == nil {
		rename = d.Owners.RenameAccount
	}

	// rosterTickC is d.RosterTick when a test injects one (item ii), else
	// newRosterTicker(rosterTickInterval): a real 5s ticker in production,
	// which a test driving runProxyWithSignal replaces instead. Both seams are
	// purely observational — they can only change how fast a test SEES a tick,
	// never what production does with one — so it stays a plain nil-defaulted
	// local rather than living on d as anything more than the channel itself.
	rosterTickC := d.RosterTick
	if rosterTickC == nil {
		c, stop := newRosterTicker(rosterTickInterval)
		defer stop()
		rosterTickC = c
	}
	// watchCtx is derived from ctx but stoppable independently of it:
	// closeOwners must stop and JOIN the roster watcher before closing
	// own, and TWO paths reach closeOwners with ctx still live — serve's
	// <-errc branch (a listener error, not a graceful cancel), and
	// net.Listen failing below, the stronger and actually-tested case
	// (TestRunDaemonDrainsOwnersWhenListenFails never cancels ctx at
	// all). Waiting on ctx instead of this derived one would hang
	// shutdown forever on either.
	watchCtx, stopWatch := context.WithCancel(ctx)
	// stopWatch is also called from inside closeOwners below, on every
	// path that reaches it (both of serve's exits, and net.Listen failing
	// below) — this defer is currently unreachable-as-necessary along any
	// path this function returns through today. It stays as defence in
	// depth against a future return added before closeOwners is wired in.
	defer stopWatch()
	// listenPort holds the resolved --listen port for the daemon object
	// stamped on each roster tick, below. It starts at 0 (unknown —
	// Daemon.Port is omitempty) because the roster watcher goroutine starts
	// before listenTCP resolves the real port a few lines down; it is set,
	// with atomic ops since the watcher reads it from a different goroutine
	// than the one that resolves it, once listenTCP succeeds.
	var listenPort atomic.Int64
	// stamp is watchRoster's seam onto the daemon object (§5.1). It is its
	// OWN write (F94 — the roster tick's existing owners.Forget work does
	// NOT cover it), so it is rate-limited rather than firing on every 5s
	// tick: it writes only when a counter has changed since the last write
	// (TestRosterTickWritesAgainWhenACounterChangesWellInsideTheHeartbeatWindow),
	// or daemonHeartbeatInterval has elapsed since one
	// (TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt)
	// — an idle daemon must not fsync status.json every 5s forever.
	//
	// The very first tick always writes, even for a genuinely idle daemon
	// (every counter zero) — TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt's
	// own first tick pins exactly this. What actually delivers it: `now`
	// (a real ticker's timestamp, always many centuries past year 1) minus
	// a zero lastStampAt SATURATES at time.Duration's ~292-year maximum
	// (documented Sub behaviour), which is always >= daemonHeartbeatInterval
	// — so heartbeatDue's `lastStampAt.IsZero() ||` clause below is inert,
	// the same shape as the guard status.go's DaemonRunningAt labels for
	// the identical reason. changed's own `lastStampAt.IsZero() ||` clause
	// is inert too, for a second reason: even if it always evaluated false,
	// heartbeatDue's saturation alone already forces the first tick to
	// write. Both stay: they state the real intent plainly ("never stamped
	// yet" must always write) and would start mattering the moment
	// daemonHeartbeatInterval or either comparison's shape changes.
	//
	// stamp is called from watchRoster's single goroutine only, once per
	// tick, strictly sequentially — so the last* locals below need no
	// synchronization of their own.
	//
	// d.Chooser is nil in every daemonDeps literal that predates this
	// field, which must read as zero owner-write drops, never a panic.
	var (
		lastStampAt                            time.Time
		lastRouteDrift, lastZeroIDs, lastDrops uint64
		lastNotifyErrs                         uint64
	)
	stamp := func(now time.Time) {
		// Keeping the remote accounts warm (R147) rides this tick too; kick
		// rate-limits itself, never blocks and starts nothing once
		// shutdown began. closeOwners cancels and joins it.
		d.Warm.kick(watchCtx)
		var drops uint64
		if d.Chooser != nil {
			drops = d.Chooser.OwnerWriteDrops()
		}
		routeDrift := srv.RouteDrift()
		// Notifications ride this tick (M2 spec §4). They run before the
		// skip below: an unchanged tick must still evaluate the limit
		// roll-up at now, since a reset time passing changes nothing a
		// counter sees. A nil d.Notify does nothing.
		d.Auto.tick(now)
		// Session activity rides the same tick; it queues its own write
		// only when the snapshot changed.
		if d.Chooser != nil && d.Chooser.tracker != nil && d.Sink != nil {
			stampSessions(d.Sink, d.Chooser.tracker, d.Home, now)
		}
		if d.Spread != nil {
			d.Spread.prune(liveKeep(d.Home), now)
			d.Spread.tick(now)
		}
		d.Notify.tick(d.Cache.State, d.Sink, routeDrift, now)
		notifyErrs := d.Notify.Errors()
		zeroIDs := d.Owners.ZeroIDExtractions()
		changed := lastStampAt.IsZero() || routeDrift != lastRouteDrift || zeroIDs != lastZeroIDs || drops != lastDrops || notifyErrs != lastNotifyErrs
		heartbeatDue := lastStampAt.IsZero() || now.Sub(lastStampAt) >= daemonHeartbeatInterval
		if !changed && !heartbeatDue {
			return
		}
		// listenPort.Load() is read fresh here, not cached in a last* local
		// the way the counters above are: the port is resolved once
		// per daemon lifetime and then never moves again, so whichever
		// write happens to fire next for another reason (a changed counter,
		// or heartbeatDue) always carries whatever port is currently known
		// — including the F96 startup stamp below, which fires before
		// listenTCP resolves it and so still writes port 0 that one time.
		d.Sink.setDaemon(int(listenPort.Load()), routeDrift, zeroIDs, drops, notifyErrs, now)
		lastStampAt = now
		lastRouteDrift, lastZeroIDs, lastDrops, lastNotifyErrs = routeDrift, zeroIDs, drops, notifyErrs
	}
	// tickStamp adds the roster syncs to the watcher's per-tick stamp,
	// which also runs once before the first tick: that first call is the
	// daemon-start seeding (spec §6.4). The status rows go first, so a
	// rename carries its row across (F171) before the poller seeds the new
	// name from it, and before stamp runs the planner and the notifier on
	// the same tick. A roster read error skips the syncs for that tick
	// only; stamp still runs.
	tickStamp := stamp
	if d.Poller != nil || d.Sink != nil {
		tickStamp = func(now time.Time) {
			if st, err := d.Cache.State(); err == nil {
				if d.Sink != nil {
					d.Sink.ensureRoster(accountMembers(st))
				}
				if d.Poller != nil {
					d.Poller.SyncRoster(pollAccounts(st))
				}
			}
			stamp(now)
		}
	}
	// d.Tokens is nil in several test literals that never proxy a request,
	// so this stays nil for them (watchRoster's own doc comment: nil is a
	// no-op) rather than assuming every runDaemon caller wires a manager.
	var invalidateToken func(string)
	if d.Tokens != nil {
		invalidateToken = newReloginHook(d.Tokens.Invalidate, d.Cache.State, d.Sink, d.Notify, timeNow)
	}
	rosterDone := make(chan struct{})
	go func() {
		defer close(rosterDone)
		watchRoster(watchCtx, d.Cache, forget, rename, rosterTickC, func(err error) {
			fmt.Fprintln(d.Stderr, "chottag: roster watch:", err)
		}, d.RosterProcessed, tickStamp, ownersTick(d.Owners, ownersReloadErrorThrottle(d.Stderr)), invalidateToken)
	}()
	// The poller runs on watchCtx, so closeOwners's stopWatch stops it,
	// and every path to sink.Close goes through closeOwners first: a poll
	// is never written after the sink closes.
	pollDone := make(chan struct{})
	if d.Poller != nil {
		go func() {
			defer close(pollDone)
			d.Poller.Run(watchCtx)
		}()
	} else {
		close(pollDone)
	}
	updateDone := make(chan struct{})
	if d.Update != nil {
		go func() {
			defer close(updateDone)
			d.Update.Run(watchCtx)
		}()
	} else {
		close(updateDone)
	}
	restartDone := make(chan struct{})
	if d.Restart != nil {
		if d.Restart.idle == nil {
			d.Restart.idle = srv.Idle
		}
		go func() {
			defer close(restartDone)
			d.Restart.Run(watchCtx)
		}()
	} else {
		close(restartDone)
	}
	sessionDone := make(chan struct{})
	if d.Session != nil {
		d.Session.stop = stopSelf
		go func() {
			defer close(sessionDone)
			d.Session.Run(watchCtx)
		}()
	} else {
		close(sessionDone)
	}
	closeOwners := func() {
		// Stop and JOIN the roster watcher before closing the map.
		// watchRoster calls own.Forget every 5s; a Forget that lands after
		// Close does not reach disk, and the account it would have dropped
		// stays attributed on the next daemon start (Task 4 review, F1).
		stopWatch()
		<-rosterDone // the last kick has happened
		d.Warm.stop(warmStopBound)
		<-pollDone
		<-updateDone
		<-restartDone
		<-sessionDone
		d.Owners.Close()
	}

	wakerCfg := d.Waker
	if wakerCfg.OnWake == nil {
		wakerCfg.OnWake = newWakeHandler(srv.CloseUpstreams, d.Tokens.InvalidateAll, func() {
			// CloseUpstreams' return value is not logged as a count: it
			// runs CloseIdleConnections() first, which only ever reaches
			// pooled http.Transport connections sitting IDLE in the
			// transport's own pool — each one deregisters itself as it
			// closes, so idle pooled connections are the ones NOT counted.
			// A pooled connection that is busy serving a request when the
			// wake fires stays in s.conns, same as a tunnel (blind or
			// MITM) — closeAll then closes and counts BOTH of those
			// (measured: a blind tunnel left open makes CloseUpstreams
			// return 1; an in-flight MITM stream makes it return 1 too,
			// logged as "closed 1"). That is not the count a reader would
			// expect "closed N connections" to mean, so naming it here
			// would be actively misleading after a real wake.
			fmt.Fprintln(d.Stderr, "chottag: woke from sleep, closed upstream connections")
		})
	}
	if d.Warm != nil {
		// Chained like the poller's: wake only schedules a timer. The warm
		// pass itself runs wakeWarmDelay later, once the network and the
		// Keychain are back (R149).
		onWake := wakerCfg.OnWake
		wakerCfg.OnWake = func() {
			onWake()
			d.Warm.wake(watchCtx)
		}
	}
	if d.Poller != nil {
		// Chained after whatever OnWake is (the default above, or a
		// test's): Wake only takes the poller's lock and nudges, so the
		// waker's loop stays prompt.
		onWake := wakerCfg.OnWake
		wakerCfg.OnWake = func() {
			onWake()
			d.Poller.Wake()
		}
	}
	if d.Restart != nil {
		// Chained like the poller's: Wake never blocks.
		onWake := wakerCfg.OnWake
		wakerCfg.OnWake = func() {
			onWake()
			d.Restart.Wake()
		}
	}
	waker := proxy.NewWaker(wakerCfg)
	// wakerCtx is its OWN cancellable context, derived from ctx like
	// watchCtx but deliberately not shared with it: the wake detector must
	// be joined before runDaemon returns on every path (item iv), and an
	// earlier version ran it on watchCtx, reasoning that closeOwners's own
	// stopWatch() (called on every path that reaches a join, including
	// both of the two non-graceful exits where ctx itself is never
	// cancelled) would cover it too. Measured, that made the join
	// UNFALSIFIABLE rather than merely redundant: closeOwners cancels
	// watchCtx, and hence the shared waker, well before runDaemon ever
	// reaches <-wakerStopped, and closeOwners's own <-rosterDone gives the
	// scheduler ample opportunity to run the now-unblocked waker goroutine
	// to completion in the meantime — deleting the join outright still
	// left d.WakerDone closed by the time runDaemon returned on 50/50
	// runs of TestRunDaemonDrainsOwnersWhenListenFails, since nothing
	// distinguished "stopped because it was joined" from "stopped early
	// for an unrelated reason, and happened to finish before we checked".
	// wakerCtx is cancelled ONLY by stopWaker, called immediately before
	// each <-wakerStopped below. What removing either the call or the
	// receive actually does is NOT the same at the two sites that pair
	// them, and the claim below only holds at one of them:
	//
	//   - At the net.Listen failure below: deleting the CALL deadlocks
	//     runDaemon here too, the same shape as the post-serve site below —
	//     recoverable only by an external signal, since nothing else on
	//     this path ever cancels wakerCtx (installShutdown's own deferred
	//     cancel() only runs once run(ctx) — this call to runDaemon — has
	//     already returned, and it cannot return while parked here).
	//     Measured 3/3: commenting out stopWaker() at this site and running
	//     `go test ./internal/cli/ -run
	//     'TestRunDaemonDrainsOwnersWhenListenFails$' -timeout 25s` panics
	//     with "test timed out after 25s" every time, parked at
	//     <-wakerStopped. Deleting only the RECEIVE does NOT hang
	//     runDaemon: stopWaker() alone still cancels wakerCtx, so runDaemon
	//     returns without waiting for the now-unblocked waker goroutine to
	//     finish closing d.WakerDone — a real but probabilistic
	//     unjoined-goroutine gap, not a deterministic kill (measured on
	//     this machine: TestRunDaemonDrainsOwnersWhenListenFails's own
	//     WakerDone check catches it 0/10 isolated and 0/10 full-package
	//     under default GOMAXPROCS, but 7/10 isolated and 10/10
	//     full-package under `go test -cpu=1` — see that test's own doc
	//     comment).
	//
	//   - At the post-serve pairing further down (after `err =
	//     serve(...)` on its <-errc branch, a listener error rather than a
	//     graceful cancel): that same reasoning does NOT apply. ctx is
	//     never cancelled on that branch — installShutdown's cancel() only
	//     runs after run(ctx) returns, and run(ctx) IS runDaemon. Removing
	//     the CALL there does not merely leave the waker running longer; it
	//     removes the only thing that would ever cancel wakerCtx on this
	//     branch, so <-wakerStopped blocks and runDaemon hangs forever
	//     instead of returning 1 — worse than a crash under a supervisor,
	//     since nothing restarts a daemon that never exits (F84: the
	//     whole-branch review's finding on this exact comment). Removing
	//     only the RECEIVE is a different, narrower bug: stopWaker() alone
	//     still cancels wakerCtx, so runDaemon does not hang — it returns
	//     without joining the now-unblocked waker goroutine, a real but
	//     racy unjoined-goroutine gap. See that site's own comment for the
	//     exact measurements of both (one is a deterministic kill, the
	//     other is not).
	wakerCtx, stopWaker := context.WithCancel(ctx)
	// Currently unreachable-as-necessary along any path this function
	// returns through today, same as stopWatch's defer above: every
	// return already calls stopWaker explicitly first. Kept as defence in
	// depth against a future return added before those explicit calls.
	defer stopWaker()
	wakerStopped := make(chan struct{})
	go func() {
		defer close(wakerStopped)
		waker.Run(wakerCtx)
		if d.WakerDone != nil {
			close(d.WakerDone)
		}
	}()

	ln, err := listenTCP("tcp", d.Listen)
	if err != nil {
		// serve owns draining owners and the sink from here on, but it is
		// never reached if the listen itself fails. closeOwners first, sink
		// last — same rule as both of serve's own exit paths, kept uniform
		// here rather than "nothing writes to own this early, so it's fine"
		// (that reasoning is exactly what produced F71: an early return's
		// close hooks scoped to a justification that didn't generalize).
		// Without the sink Close, the roster seedRosterAtStartup already
		// queued would be lost to the coalescing window (contract 4).
		//
		// This order is guarded by review, not by a dedicated test:
		// unlike serve's two exit paths, closeOwners here is not a
		// parameter a test can substitute its own write-observing hook
		// for (it is the real closure runDaemon builds from own), and
		// nothing in this branch naturally queues a sink write at the
		// moment closeOwners runs — timing a probe around that window
		// would be exactly the sampling mistake round 2 of this task's
		// review already found and removed. TestRunDaemonDrainsOwnersWhenListenFails
		// pins that closeOwners runs here at all; it does not pin order.
		closeOwners()
		stopWaker()
		<-wakerStopped
		// clearDaemon here for the same reason as on both of serve's exit
		// paths, and it is NOT redundant on this one: F96's startup stamp
		// runs in the roster watcher goroutine launched above, before this
		// listenTCP, so by now a heartbeat may already have been stamped
		// for a daemon that never started serving. Without this, the
		// Sink.Close() below flushes that heartbeat and `chottag own`
		// refuses for up to DaemonStaleAfter (90s) on behalf of a daemon
		// that failed to bind (final re-review, MED 1).
		d.Sink.clearDaemon()
		d.Sink.Close()
		// Spec §4.8: the message names the port (via err, which net.Listen's
		// own error already includes) AND what to do about it — chottag
		// never searches for a free one, so "wait" is not an option.
		msg := fmt.Sprintf("chottag: %v — free the port, or configure a different one, and retry", err)
		fmt.Fprintln(startupWriter(d.Stderr, d.StartupErr), msg)
		return exit.Error
	}
	// listenPort, for the daemon object above: the resolved port, not the
	// --listen flag value, for the same reason envLines below uses ln.Addr()
	// rather than d.Listen (F3) — a wildcard port (--listen 127.0.0.1:0)
	// must stamp the port chottag actually ended up on.
	if _, portStr, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		if port, err := strconv.Atoi(portStr); err == nil {
			listenPort.Store(int64(port))
		}
	}
	// The resolved address, not the --listen flag value, so a wildcard
	// port (e.g. --listen 127.0.0.1:0) does not print an export line that
	// disagrees with where the daemon actually ended up listening (F3).
	if d.LogPath == "" {
		fmt.Fprintf(d.Stderr, "chottag proxy listening on %s, request logging disabled\n", ln.Addr())
	} else {
		fmt.Fprintf(d.Stderr, "chottag proxy listening on %s, logging to %s\n", ln.Addr(), d.LogPath)
	}
	fmt.Fprint(d.Stdout, envLines(d.Home, ln.Addr().String()))
	err = serve(ctx, ln, srv, srv.CloseUpstreams, srv.CloseClientTunnels, closeOwners, d.Sink)
	// Load-bearing on serve's <-errc branch (a listener error, not a
	// graceful ctx cancel — see serve's own doc comment): ctx is never
	// cancelled there, so this stopWaker() is the ONLY thing that ever
	// unparks the waker goroutine below, and <-wakerStopped is the only
	// thing that joins it before runDaemon returns. That is not the same
	// failure at both lines, and both are measured, not assumed:
	//   - Deleting stopWaker() deadlocks runDaemon itself: nothing else
	//     ever cancels wakerCtx on this branch (installShutdown's cancel()
	//     only runs after runDaemon returns, and runDaemon would then be
	//     the thing still parked), so <-wakerStopped blocks forever. The
	//     whole internal/cli package stays green with this deleted; only
	//     TestRunDaemonUnparksAfterAListenerFailureAfterServe's own bound
	//     catches it — reliably, every run (10/10 measured).
	//   - Deleting <-wakerStopped does NOT hang runDaemon: stopWaker()
	//     alone still cancels wakerCtx, and runDaemon returns without
	//     waiting for the now-unblocked waker goroutine to actually finish
	//     closing d.WakerDone. That is a genuine, not merely theoretical,
	//     unjoined-goroutine bug (a caller could observe d.WakerDone still
	//     open after runDaemon has returned), but it is a race, and the
	//     test that catches it — checking d.WakerDone right after runDaemon
	//     returns — catches it probabilistically, and the rate depends on
	//     context: measured on this machine, 30 individual runs of `go
	//     test ./internal/cli/ -run
	//     'TestRunDaemonUnparksAfterAListenerFailureAfterServe$'` catch it
	//     0/30 under default GOMAXPROCS and 9/30 under -cpu=1; 20 runs of
	//     the whole `go test ./internal/cli/` package catch it 0/20 under
	//     default GOMAXPROCS and 0/20 under -cpu=1 too (unlike the
	//     listen-failure site above, -cpu=1 does not reliably help here,
	//     and running as part of the full package does not either — see
	//     that test's own doc comment). Reported honestly rather than
	//     claimed as a deterministic kill it is not.
	stopWaker()
	<-wakerStopped
	if err != nil {
		fmt.Fprintln(d.Stderr, "chottag:", err)
		return exit.Error
	}
	return exit.OK
}

// serve runs the proxy until ctx is cancelled, then stops accepting,
// gives in-flight requests shutdownGrace to finish, closes every tunnel
// http.Server cannot see (every CONNECT is hijacked, so Shutdown does not
// track it), drains the owner map, and drains the status sink last. A
// write queued against the sink after that Close is not prevented —
// queueLocked silently drops it instead (see status.go) — so "last" here
// means "last to be written", not a guarantee that nothing after it is
// even attempted.
//
// closeConns (proxy.Server.CloseUpstreams) closes only blind
// (CONNECT-passthrough) tunnels, which it tracks in s.conns. A MITM
// tunnel established before shutdown is invisible to that set: mitm()
// (internal/proxy/connect.go) runs its own inner http.Server over the
// hijacked client conn, that inner server is never Shutdown, and its conn
// is never added to s.conns — so without closeTunnels
// (proxy.Server.CloseClientTunnels) it would keep serving new requests on
// the same connection after serve returns (F54). Since MITM carries all
// of chottag's product traffic (every intercepted API call), both hooks
// are required here, not just closeConns.
//
// The <-errc branch below returns early, without calling closeConns or
// closeTunnels: it fires when srv.Serve itself returns (a listener error,
// not a graceful shutdown via ctx), and leaves any tunnels already tracked
// at that point to close on their own. closeOwners, unlike those two,
// still runs on this path: it is a flush-and-join whose cost here is nil,
// and skipping it would lose the newest owner document precisely when the
// listener has already failed — the inverse of the durability property
// Close exists to protect. Both paths keep sink.Close() last, and both
// call sink.clearDaemon() immediately before it (fix round 2, item 1): the
// <-errc path is a listener failure, and the process is exiting there too,
// so leaving a stale heartbeat behind on that path would preserve the same
// up-to-90s "chottag own refuses; chottag status --json shows running" bug
// for a user whose daemon just failed, not just one who stopped it cleanly.
func serve(ctx context.Context, ln net.Listener, h http.Handler, closeConns, closeTunnels func() int, closeOwners func(), sink *statusSink) error {
	// No read or write timeouts, deliberately: spec §6 requires long
	// streams and long-lived Remote Control connections to survive.
	srv := &http.Server{Handler: h}
	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case err := <-errc:
		// Both exit paths drain owners, unlike the connection-close hooks
		// above: losing the newest owner document because the LISTENER
		// failed would lose it precisely when something already went
		// wrong. sink stays last on this path too.
		closeOwners()
		sink.clearDaemon()
		sink.Close()
		return err
	case <-ctx.Done():
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shutCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		err = nil // the grace period expiring is a normal shutdown, not a failure
	}
	closeConns()
	// Hijacked MITM tunnels are invisible to both Shutdown (they were
	// hijacked out of this server) and closeConns (that set holds
	// UPSTREAM conns and is also used on wake, where killing a live client
	// tunnel would be a regression). Without this, a tunnel established
	// before shutdown keeps serving requests after serve returns (F54).
	closeTunnels()
	// Owners drains AFTER tunnels stop: a request still in flight can call
	// Record, and Close is what guarantees the newest document reaches
	// disk. It goes BEFORE sink.Close() only to keep sink last, which the
	// M1c2 review pinned.
	closeOwners()
	sink.clearDaemon()
	sink.Close()
	return err
}

// watchRoster drops a removed account's objects from the owner map.
//
// It runs on its own goroutine rather than off a state reload: reloads happen
// inside store.Cache.State() on a REQUEST goroutine, and forget writes a
// file, so hanging it there would put a disk write on the response path —
// the F48 hazard, where a blocking OnWake stalled the waker's whole select
// loop including its ctx arm. Cache.State() only stats the file when nothing
// changed, so ticking is nearly free.
//
// processed, if non-nil, is sent on after EVERY tick has been fully handled
// — whether or not that tick forgot anything, and whether or not
// cache.State() even succeeded. Production passes nil; a test passes a
// channel so it can block until a given tick's processing has genuinely
// finished before it inspects (or asserts the absence of) any forget calls
// that tick may have made, rather than reading a counter at an arbitrary
// moment with no happens-before edge to the watcher's own progress (see
// TestWatchRosterOnlyForgetsAccountsMissingFromTheCurrentRoster's doc
// comment for the mutation that motivated this).
//
// stamp, if non-nil, is called once per tick, BEFORE cache.State() and
// regardless of whether it succeeds: the daemon object (§5.1) rides this
// tick because it is the only periodic path that writes status.json (the
// wake detector also runs its own ticker, internal/proxy/wake.go, but never
// touches the cache) — not because the tick's own work happens to produce a
// free write (F94 — it does not; stamp is its own write, and rate-limits
// itself). Liveness must not depend on the
// roster read succeeding, so a transient cache.State() error must not skip
// the stamp and make a live daemon read as down. Production always passes
// runDaemon's stamp closure; a test may pass nil.
//
// stamp is called with the TICK's own time.Time value, not a fresh
// timeNow() — deliberately, so whatever drives tick also controls the
// clock stamp's rate-limiting sees. A test asserting the 30s
// daemonHeartbeatInterval skip/resume boundary needs that: real 5s ticks
// waiting out a real 30s window is not acceptable in this suite (see
// TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt),
// and production's real ticker's own timestamps are just as valid a "now"
// as re-querying the wall clock a moment later would be.
//
// renameHealIsAmbiguous is NEW-1's guard on the Dir-keyed diff's daemon-side
// rename heal (I1): dir kept its slot but its name changed from oldName to
// newName since the last tick. Renaming every entry named oldName
// (case-insensitively) to newName — what rename(oldName, newName) below
// does — is only correct when that name change is unambiguous. It is NOT
// when:
//
//   - oldName is STILL the current name of some OTHER dir right now (in
//     cur): a chained rename's second step can leave exactly this shape —
//     e.g. `rename A A2` then `rename B A` in one tick: dir A's own diff
//     entry is (old "A", new "A2"), and dir B is ALREADY, correctly, "A"
//     in cur. Blindly renaming every "A"-named entry to "A2" here would
//     sweep up B's own, freshly-reloaded entries along with A's.
//   - newName was the known (last-tick) name of some OTHER dir that is
//     STILL PRESENT in cur (under whatever it is called now): the same
//     chained example's OTHER half — dir B's own diff entry is (old "B",
//     new "A"), and dir A — a different, still-present dir — was known as
//     "A" last tick. A swap (A->tmp, B->A, tmp->B) hits both conditions on
//     both dirs simultaneously.
//
// In every one of those shapes, `chottag rename`'s own two ordered,
// synchronous writes (state.json then owners.json) already resolved the
// ambiguity correctly and in the right order — this tick's ownersTick call
// already Reload()ed that correct, final owners.json before this diff ever
// runs (watchRoster's own doc comment on ownersTick's ordering). A snapshot
// diff has no ordering information and cannot replay multiple renames
// correctly from a single before/after pair; deferring entirely to what
// Reload already adopted is what stays correct, not a best-effort replay of it.
//
// The condition that is NOT ambiguous, and must still heal (I1's whole
// point): oldName's dir is gone from cur, or newName's prior dir is gone
// from cur too (e.g. a genuinely interrupted single rename, where the
// other name was never anyone else's — see the heal-an-interrupted-rename
// test — or M1's own "logout B then rename A B" shape, where the dir that
// used to be "B" has been REMOVED, not merely renamed elsewhere, so there
// is nothing else currently claiming either name).
func renameHealIsAmbiguous(dir, oldName, newName string, known, cur map[string]string) bool {
	for d, n := range cur {
		if d != dir && strings.EqualFold(n, oldName) {
			return true
		}
	}
	for d, prevName := range known {
		if d == dir || !strings.EqualFold(prevName, newName) {
			continue
		}
		if _, stillPresent := cur[d]; stillPresent {
			return true
		}
	}
	return false
}

// ownersTick, if non-nil, is called once per tick, before cache.State(): it
// is the owners-map half of this tick's work (adopting another process's
// correction to owners.json, and flushing anything a failed write left
// pending — see the ownersTick function below), not something the roster
// diff itself needs. It reports its own errors (see its own doc comment for
// why that is not routed through this function's onError), and never stops
// the tick either way; production always passes
// ownersTick(d.Owners, ownersReloadErrorThrottle(d.Stderr)), a test may pass
// nil.
//
// invalidate, if non-nil, is called with a slot dir whenever this tick's
// roster shows that dir's LoggedInAt advanced since the last tick (item 6,
// review round 3): a token read cached under tokens.Manager's ReadTTL is not
// itself watching state.json, so a request landing within ReadTTL of a
// re-login would otherwise still be served the pre-login read — a stale
// needs-login for a slot that just logged back in. Production always passes
// d.Tokens.Invalidate; a test may pass nil. Keyed by Dir, the same as the
// forget/rename diff above and for the same reason (D1): Name is what
// rename changes, Dir is not.
func watchRoster(ctx context.Context, cache *store.Cache, forget func(string) error, rename func(from, to string) (int, error), tick <-chan time.Time, onError func(error), processed chan<- struct{}, stamp func(time.Time), ownersTick func() error, invalidate func(dir string)) {
	// Stamp once before entering the select loop below, so the daemon
	// object exists in status.json within microseconds of this goroutine
	// being scheduled, rather than only after the first 5s roster tick.
	//
	// What reads it TODAY: `chottag status --json` (status.go's runStatus)
	// marshals the whole status.File, Daemon field included, after calling
	// f.DaemonRunningAt(now) to derive Running from the heartbeat this
	// stamps — renderStatus, the human table, does not read Daemon at all.
	// This early stamp is what lets a `status --json` run moments after
	// start show a daemon instead of a blank one.
	//
	// This is NOT still an F96 mitigation: the heartbeat used to be the
	// only signal `chottag own` (own.go) had for refusing a reassignment a
	// live daemon might silently revert, and a slow first stamp widened
	// that race. own.go no longer imports internal/status at all — §4.7's
	// owners.lock (Task 4's Edit) closes the race directly, by making the
	// daemon's writer and `own` take the same lock, so there is no refusal
	// left for a slow heartbeat to widen.
	if stamp != nil {
		stamp(time.Now())
	}
	var known map[string]string         // slot dir -> last-seen name; nil until the first successful read
	var knownLogin map[string]time.Time // slot dir -> last-seen LoggedInAt (item 6, review round 3); nil until the first successful read
	for {
		select {
		case <-ctx.Done():
			return
		case tickAt, ok := <-tick:
			if !ok {
				return
			}
			if stamp != nil {
				stamp(tickAt)
			}
			// Do this tick's owners-map work before the roster's own: `chottag
			// own` corrects an attribution in a different process, and until we
			// re-read it we keep routing that object to the old account (§4.7).
			// ownersTick reports its own errors (a throttled, correctly-
			// labelled sink — see its own doc comment), so nothing here
			// forwards its return value to onError. Execution falls through
			// to the roster work regardless of the outcome — unlike
			// cache.State()'s own failure path just below, which `continue`s
			// and skips the rest of the tick: an owners.json problem is
			// unrelated to the roster and to the heartbeat stamp, so it must
			// not suppress either.
			if ownersTick != nil {
				ownersTick()
			}
			st, err := cache.State()
			if err != nil {
				onError(err)
				// select on ctx.Done(), not a bare send: this is the same
				// F48 shape the doc comment above warns about — a bare
				// send here would leave this goroutine stranded forever
				// once nothing is left to receive on processed (e.g. a
				// test that has already failed and returned), since ctx
				// cancellation would then have no way to free it.
				if processed != nil {
					select {
					case processed <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
				continue
			}
			// Keyed by slot dir, not by name: Dir is fixed when an account
			// is created and never follows a rename (R21/D1), while Name
			// is exactly what `chottag rename` changes. Diffing by name
			// would read a rename's departed old name as a removal and
			// forget everything that name owned — deterministically for a
			// case-only rename (owners.Forget itself matches
			// case-insensitively, so "B" gone/"b" arrived looks identical
			// to a real removal), and for any rename caught mid-flight
			// between its two steps, since state.json's new name lands
			// before owners.json is rewritten to match (M2a T2 fix round
			// 1). A real removal still has no Dir in cur, so it is still
			// forgotten — under the name it was last seen with, since a
			// name is what forget (and owners.json) actually key on.
			cur := make(map[string]string, len(st.Accounts))
			for _, a := range st.Accounts {
				cur[a.Dir] = a.Name
			}
			// A re-login advances LoggedInAt (chottag login/adopt); a
			// slot's token cache does not itself watch state.json, so this
			// tick — which already reloaded it — is what tells the token
			// cache to stop trusting a read taken before the login (item
			// 6, review round 3). Same first-read guard as the name diff
			// below: knownLogin starts nil, so the baseline read never
			// fires invalidate for every account.
			curLogin := make(map[string]time.Time, len(st.Accounts))
			for _, a := range st.Accounts {
				curLogin[a.Dir] = a.LoggedInAt
			}
			if invalidate != nil && knownLogin != nil {
				for _, a := range st.Accounts {
					if prev, ok := knownLogin[a.Dir]; ok && a.LoggedInAt.After(prev) {
						// Off this goroutine, not inline (item 2, review
						// round 4): invalidate takes the slot's own mu,
						// which Token/Status hold across creds.ExecRunner's
						// exec — the same blocking risk InvalidateAll's own
						// doc comment warns about — and this goroutine also
						// runs the auto tick, the notify tick, the
						// heartbeat stamp and the shutdown join.
						go invalidate(a.Dir)
					}
				}
			}
			knownLogin = curLogin
			// The first read is a baseline, never a diff: a fresh daemon
			// must not read "I have not looked yet" as "every account was
			// just removed".
			//
			// This guard is currently behaviourally inert, and no test in
			// this file can tell if it is ever deleted or broken: `known`
			// starts nil, and ranging a nil map in Go is a documented,
			// legal, zero-iteration no-op — not a panic — so "skip the
			// diff because known is nil" and "range over known == nil"
			// already produce the identical result (confirmed: deleting
			// this `if` still passes the whole suite). Replacing the nil
			// initialisation with an empty, non-nil map is equally
			// invisible for the same reason. It stays because it states
			// the real intent plainly — never mistake "have not looked
			// yet" for "everything was just removed" — and would start
			// mattering again the moment the diff direction or the
			// initial value of known ever changes. The suite will not
			// stop you if a future edit breaks that intent while leaving
			// this line looking untouched.
			if known != nil {
				// curNames is M1's guard: a removed slot is forgotten under
				// its last name UNLESS some account in cur now has that
				// exact name (case-insensitively) — e.g. `logout B` and
				// `rename A B` land within the same tick, and B's dir is
				// gone while a DIFFERENT dir now answers to "B". Forgetting
				// "B" there would wipe the renamed account's just-inherited
				// entries, not the departed one's.
				curNames := make(map[string]bool, len(cur))
				for _, name := range cur {
					curNames[strings.ToLower(name)] = true
				}
				for dir, oldName := range known {
					newName, stillPresent := cur[dir]
					switch {
					case stillPresent && newName != oldName && !renameHealIsAmbiguous(dir, oldName, newName, known, cur):
						// I1 (F174): the same slot kept its dir but changed
						// name — a rename landed since the last tick, maybe
						// only step 1 of it (state.json), if step 2
						// (owners.json) failed or hasn't run yet. Renaming
						// the daemon's own owner map here finishes that work
						// itself, so an interrupted rename heals on the next
						// tick without requiring `chottag rename` to be run
						// again.
						if _, err := rename(oldName, newName); err != nil {
							onError(err)
						}
					case !stillPresent && !curNames[strings.ToLower(oldName)]:
						if err := forget(oldName); err != nil {
							onError(err)
						}
					}
				}
			}
			known = cur
			// select on ctx.Done(), not a bare send: same F48 shape as
			// above — without it, cancelling ctx could not free this
			// goroutine if nothing is left to receive on processed.
			if processed != nil {
				select {
				case processed <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// ownersTick is the owners-map work the roster tick performs: adopt an
// external change to the file, then flush anything a failed write left
// pending. Extracted rather than written inline at the watchRoster call so a
// test can drive the real production wiring (the same reason wireProxyConfig
// exists in this package).
//
// onReloadError, if non-nil, is called with Reload's own error — a read
// failure or a corrupt file — reported HERE rather than through
// watchRoster's generic onError, because that sink is unthrottled and
// labelled for the roster watch, not for an owners.json problem: production
// passes ownersReloadErrorThrottle(d.Stderr) (status.go), the throttled,
// correctly-labelled sibling of ownersSaveErrorThrottle, which is wired
// separately (own.SetOnError, proxy.go:127) to the Map's own WRITE
// failures. That includes ones the NudgePending call below triggers from
// right here: those still go to own.SetOnError's sink, not to
// onReloadError, because NudgePending itself takes no error-reporting
// parameter — a write failure it wakes the writer into is exactly as
// asynchronous, and reported exactly the same way, as one Record or
// Reassign would have triggered directly. The error this closure returns
// is Reload's alone, and is still returned too, for a test to assert on
// directly.
func ownersTick(o *owners.Map, onReloadError func(error)) func() error {
	return func() error {
		_, err := o.Reload()
		if err != nil && onReloadError != nil {
			onReloadError(err)
		}
		o.NudgePending()
		return err
	}
}

// chooser adapts the selector, owner map and token manager to proxy.Chooser.
type chooser struct {
	sel   *selector.Selector
	own   *owners.Map
	tm    *tokens.Manager
	state func() (store.State, error)
	// ownerWriteDrops counts owner writes that returned an error and were
	// discarded — in practice owners.ErrClosed, from a Record racing
	// shutdown. Monotonic for this daemon generation.
	ownerWriteDrops atomic.Uint64
	// notify, if non-nil, is told about every successful Choose (M2b T5,
	// T1 review #5): that clears the account's blocked flag, so the
	// daemon's usage hook can tell a genuinely fresh 2xx apart from one
	// belonging to a request that predates the account's last needs-login
	// notice. nil (every chooser literal before this field existed) is a
	// no-op.
	notify *daemonNotify
	// tracker, if non-nil, is told about every successful Choose made for an
	// identified caller (M6). nil is a no-op, as for notify.
	tracker *sessions.Tracker
	// sticky, if non-nil, keeps a session's validate answered by one account
	// for the session's life (R160). nil leaves validate on the serving rule.
	sticky *stickyval.Map
	// spread, if non-nil, places identified sessions while state.json's
	// policy is spread (M7). Under serial the chooser never calls it; nil
	// (a chooser built without a daemon) is serial.
	spread spreadPlacer
	// now, if non-nil, replaces timeNow for the placement clock (tests).
	now func() time.Time
	// log, if non-nil, gets the one line per pool name a session names that
	// state.json no longer has. missingPools remembers which were logged.
	log          io.Writer
	missingPools sync.Map
	// multi: more than one pool existed at the last state read. refresh, if
	// non-nil, drops the state cache (a refused request is tried again on
	// fresh state). refusedAt is when each pool's refusal was last logged.
	multi     atomic.Bool
	refresh   func()
	refuseMu  sync.Mutex
	refusedAt map[string]time.Time
}

// newDaemonChooser builds the chooser runProxyWithSignal wires into
// daemonDeps.Chooser, with notify set so a successful Choose clears the
// account's blocked flag (M2b T5, T1 review #5). Pulled out of the
// runProxyWithSignal literal so a test can construct the same wiring
// directly and pin that notify is never dropped.
func newDaemonChooser(sel *selector.Selector, own *owners.Map, tm *tokens.Manager, state func() (store.State, error), dn *daemonNotify) *chooser {
	return &chooser{sel: sel, own: own, tm: tm, state: state, notify: dn, tracker: sessions.NewTracker()}
}

func (c *chooser) Choose(ctx context.Context, d router.Decision, bodyID string) (string, string, bool, bool) {
	account, token, owner, ok, _ := c.ChooseGuarded(ctx, d, bodyID)
	return account, token, owner, ok
}

// ChooseGuarded is Choose with the pool boundary (proxy.PoolGuard): with
// more than one pool, a request no account of the session's pool can serve
// comes back with a refusal message instead of being left to go out on the
// client's own login.
func (c *chooser) ChooseGuarded(ctx context.Context, d router.Decision, bodyID string) (string, string, bool, bool, string) {
	ch, pool := c.choose(ctx, d, bodyID)
	if ch.Account == "" {
		if ch.StateErr && (c.multi.Load() || d.Class == router.Remote || d.Object != "") {
			// R147: a remote or owner request is refused whatever the pool
			// count; the class is known even when the state is not.
			ch.Refused = "state.json is unreadable"
			if !c.multi.Load() {
				return "", "", false, false, "chottag: cannot read state.json, so this remote request is refused (run: chottag doctor)"
			}
		}
		if ch.Refused == "" {
			return "", "", false, false, ""
		}
		if ch.RefusedAccount != "" {
			// R147: a remote or owner request never goes out on Home's
			// login; the selector event already logged why.
			return "", "", false, false, remoteRefusal(ch.RefusedAccount, ch.RefusedRole)
		}
		return "", "", false, false, c.refuse(pool, ch.Refused)
	}
	c.notify.beginChoose(ch.Account)
	// requests counts account choices, not client requests: a 429 retry
	// chooses again, so one request can count twice.
	if id, ok := proxy.IdentityFrom(ctx); ok && c.tracker != nil {
		c.tracker.Seen(id.Caller.SID, id.Caller.Pool, ch.Account, id.Inference, id.NativeID, timeNow())
	}
	return ch.Account, ch.Token, ch.Role == selector.RoleOwner, true, ""
}

// Guarded reports that more than one pool exists (as of the last request
// chosen): the safety net then never resends on the client's own login.
func (c *chooser) Guarded() bool { return c.multi.Load() }

// refuseEvery is how often a pool's refusal is logged.
const refuseEvery = time.Minute

// refuse is the message a refused request is answered with, logged once per
// pool per refuseEvery with the reason.
func (c *chooser) refuse(pool, why string) string {
	now := c.clock()
	c.refuseMu.Lock()
	last, seen := c.refusedAt[pool]
	log := !seen || now.Sub(last) >= refuseEvery
	if log {
		if c.refusedAt == nil {
			c.refusedAt = map[string]time.Time{}
		}
		c.refusedAt[pool] = now
	}
	c.refuseMu.Unlock()
	if log && c.log != nil {
		fmt.Fprintf(c.log, "chottag: refused a request: no account in pool %q can serve it (%s)\n", pool, why)
	}
	return fmt.Sprintf("chottag: no account in pool %q can serve this request (run: chottag pool)", pool)
}

// choose is the selector's choice for the session's pool, with the spread
// policy's placement in place of the pool's serving account for an
// identified session's serving-class request (owner and remote routing,
// which the selector applies first, are unchanged). With no placement to
// give, it is the pool's serving account (or, under spread, its fallback; a
// pool other than default whose serving account is unusable or has rotation
// off falls back to its least-bad rotating member too). A refusal because an
// account left the pool between two reads (a racing `pool leave`) is tried
// once more on fresh state before it stands. It also returns the pool.
func (c *chooser) choose(ctx context.Context, d router.Decision, bodyID string) (selector.Choice, string) {
	ch, pool := c.attempt(ctx, d, bodyID, true)
	if ch.Refused == refusedNotInPool {
		if c.refresh != nil {
			c.refresh()
		}
		// The retry skips the sticky path: its bounded refresh wait must not
		// run twice for one request.
		ch, pool = c.attempt(ctx, d, bodyID, false)
	}
	return ch, pool
}

const refusedNotInPool = "not in pool"

// wireStickyValidate opens each session's validate account map (R160) under
// home, so a daemon restart keeps a running session's answer the same, and
// attaches it to ch. A damaged file is reported and replaced by an empty map.
// It returns the lister of the accounts the warm loop keeps fresh for them.
func wireStickyValidate(ch *chooser, home string, log io.Writer) func() []string {
	vm, err := stickyval.Open(filepath.Join(home, "run", "validate-sessions.json"))
	if err != nil {
		fmt.Fprintf(log, "chottag: %v; validate answers start afresh\n", err)
	}
	ch.sticky = vm
	return func() []string { return vm.Accounts(timeNow(), stickyval.WarmWithin) }
}

// attempt is route, except that a session's validate call goes out as the
// account that answered it before, when that account can still serve it (R160,
// F267): Claude Code's Remote Control owner-pin stops when a re-validation
// names another account than the one it pinned. A recorded account whose token
// is stale is refreshed within the selector's bounded wait, never given up on.
func (c *chooser) attempt(ctx context.Context, d router.Decision, bodyID string, sticky bool) (selector.Choice, string) {
	id, identified := proxy.IdentityFrom(ctx)
	if !sticky || !d.StickySession || c.sticky == nil || c.state == nil || !identified || id.Caller.SID == "" {
		return c.route(ctx, d, bodyID)
	}
	st, err := c.state()
	if err != nil {
		return c.route(ctx, d, bodyID)
	}
	c.multi.Store(len(st.PoolNames()) > 1)
	sid := id.Caller.SID
	pool := c.poolFor(st, id.Caller.Pool)
	now := c.clock()
	prev, state := c.sticky.Lookup(sid, now)
	reason := ""
	if state == stickyval.Live {
		ch, why := c.stickyChoice(ctx, st, pool, prev)
		if ch.Account != "" {
			c.stickySet(sid, ch.Account, st, now)
			return ch, pool
		}
		reason = prev.Account + " " + whyText(why, pool)
	} else if state == stickyval.Dropped {
		reason = prev.Account + "'s record was dropped (unused for 7 days, or older than the 500 newest sessions)"
	}
	ch, pool := c.route(ctx, d, bodyID)
	if ch.Account == "" {
		return ch, pool
	}
	if state != stickyval.Absent && !strings.EqualFold(prev.Account, ch.Account) && c.log != nil {
		fmt.Fprintf(c.log, "chottag: validate for session %s moved from %s to %s: %s\n", shortSID(sid), prev.Account, ch.Account, reason)
	}
	c.stickySet(sid, ch.Account, st, now)
	return ch, pool
}

func shortSID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

func (c *chooser) stickySet(sid, account string, st store.State, now time.Time) {
	email := ""
	if a, ok := findExact(&st, account); ok {
		email = a.Email
	}
	if err := c.sticky.Set(sid, account, email, now); err != nil && c.log != nil {
		fmt.Fprintf(c.log, "chottag: could not save the validate sessions: %v\n", err)
	}
}

func whyText(w selector.Why, pool string) string {
	if w == selector.WhyNotInPool {
		return "is not in pool " + pool
	}
	return w.String()
}

// stickyChoice is the choice for the recorded account when it can still serve
// a session of pool, else the zero Choice and why it cannot. The selector
// re-checks registration, pool and rotation on its own read of the state. A
// rotation-off account is refused outside default (R90) but kept in default,
// where `tag`'s explicit override already lets it serve, and a validate is not
// an inference request.
func (c *chooser) stickyChoice(ctx context.Context, st store.State, pool string, e stickyval.Entry) (selector.Choice, selector.Why) {
	a, ok := findExact(&st, e.Account)
	if !ok {
		return selector.Choice{}, selector.WhyNotRegistered
	}
	// The name now belongs to another login (removed and added again, or
	// logged in again as another email): its answer would name another
	// account than the session pinned.
	if e.Email != "" && a.Email != "" && !strings.EqualFold(e.Email, a.Email) {
		return selector.Choice{}, selector.WhyIdentityChanged
	}
	return c.sel.ChooseNamed(ctx, pool, a.Name)
}

func (c *chooser) route(ctx context.Context, d router.Decision, bodyID string) (selector.Choice, string) {
	id, identified := proxy.IdentityFrom(ctx)
	pool := store.DefaultPool
	var st store.State
	if c.state != nil {
		s, err := c.state()
		if err != nil {
			// With pools known to exist, or an identity naming one other
			// than default, never ask the selector: its read could succeed
			// with another snapshot and route the session as default. The
			// caller's own pool is named. Otherwise this is the pre-M8
			// path: the selector reads for itself and may serve.
			if identified && id.Caller.Pool != "" {
				pool = id.Caller.Pool
			}
			if c.multi.Load() || pool != store.DefaultPool {
				return selector.Choice{StateErr: true, Refused: "state.json is unreadable"}, pool
			}
			return c.sel.ChooseIn(ctx, d, bodyID, store.DefaultPool, ""), pool
		}
		st = s
		c.multi.Store(len(st.PoolNames()) > 1)
	}
	haveState := c.state != nil
	if identified && haveState {
		pool = c.poolFor(st, id.Caller.Pool)
	}
	serving := haveState && c.spread != nil && d.Class == router.Serving && d.Object == ""
	name := ""
	if serving && identified && id.Caller.SID != "" && poolSpread(st, pool) {
		if n, ok := c.spread.accountFor(id.Caller.SID, id.NativeID, pool, c.clock()); ok {
			name = n
		} else if n, ok := c.spread.fallbackExcluding(pool, nil, c.clock()); ok {
			name = n
		}
	}
	ch := c.sel.ChooseIn(ctx, d, bodyID, pool, name)
	if ch.Refused != "" && ch.Refused != refusedNotInPool && serving {
		// The account cannot serve (none set, rotation off, no usable
		// token): the pool's least-bad rotating member other than it, if
		// there is one.
		// Each member is tried once: one whose login is stale in status.json
		// but unusable is excluded and the next pick tried, before failing.
		tried := name
		if tried == "" {
			tried = st.PoolOf(pool).Serving
		}
		exclude := map[string]bool{strings.ToLower(tried): true}
		for range st.Members(pool) {
			alt, ok := c.spread.fallbackExcluding(pool, exclude, c.clock())
			if !ok {
				break
			}
			alt2 := c.sel.ChooseIn(ctx, d, bodyID, pool, alt)
			if alt2.Refused == "" {
				return alt2, pool
			}
			ch = alt2
			exclude[strings.ToLower(alt)] = true
		}
	}
	return ch, pool
}

// poolFor is the pool a session of caller pool name routes through: name
// when state.json has it, else default (the pool was removed while the
// session ran), logged once per name.
func (c *chooser) poolFor(st store.State, name string) string {
	if st.HasPool(name) {
		return name
	}
	if _, seen := c.missingPools.LoadOrStore(name, true); !seen && c.log != nil {
		fmt.Fprintf(c.log, "chottag: pool %q no longer exists; its sessions use default\n", name)
	}
	return store.DefaultPool
}

func (c *chooser) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return timeNow()
}

func (c *chooser) Record(kind router.Kind, ids []string, account string) {
	if err := c.own.Record(kind, ids, account, timeNow()); err != nil {
		// Losing an owner entry costs correctness after a `remote` change,
		// but never the request in flight. Counted rather than waited on:
		// M1c4 moved this write off the response path for a measured ~460x
		// latency win, and a blocking wait here would trade it straight back
		// (F85). The count surfaces as status --json's
		// daemon.ownerWriteDrops.
		c.ownerWriteDrops.Add(1)
	}
}

// OwnerWriteDrops reports how many owner writes this chooser has discarded.
func (c *chooser) OwnerWriteDrops() uint64 { return c.ownerWriteDrops.Load() }

// Refresh forces a refresh of that account's token after a swapped request
// was refused, and reports the new token. It must not hand back the same
// cache hit that just failed upstream, so it calls ForceRefresh, not
// Token: a bearer that still assesses ok/expiring locally can already be
// rejected upstream (revoked, org changed, clock skew).
func (c *chooser) Refresh(ctx context.Context, account string) (string, bool) {
	st, err := c.state()
	if err != nil {
		return "", false
	}
	// account is already the exact name selector.Choose returned for this
	// in-flight request — this is an identity check ("does this account
	// still exist"), not resolving a name a human just typed, so st.Find's
	// unique-name-PREFIX fallback must never be used here (F15, third
	// instance on this branch). If the account was removed (e.g. a
	// concurrent `chottag remove`) between Choose and this call, a prefix
	// match could silently resolve to an unrelated SURVIVING account —
	// refreshing that account's slot and handing back ITS bearer for a
	// request the client believes carries the removed account's identity:
	// one account's credential used for another account's request. Decline
	// instead; the safety net already handles that correctly by falling
	// through to a resend on the client's own login.
	a, ok := findExact(&st, account)
	if !ok {
		return "", false
	}
	return c.tm.ForceRefresh(ctx, a.Dir)
}

// findExact resolves name to a registered account by exact name only,
// matching case-insensitively — the same scan store.State.index and Add's
// duplicate check already use, so it can never disagree with them. Unlike
// st.Find, it never falls back to a unique name prefix.
func findExact(st *store.State, name string) (*store.Account, bool) {
	for i, a := range st.Accounts {
		if strings.EqualFold(a.Name, name) {
			return &st.Accounts[i], true
		}
	}
	return nil, false
}

func timeNow() time.Time { return time.Now() }
