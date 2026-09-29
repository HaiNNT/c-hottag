package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
	"github.com/HaiNNT/c-hottag/internal/tracesum"
)

const defaultListen = "127.0.0.1:47821"

// traceRefusesJSON are the trace verbs that stream, run forever or print a
// table (spec §5.3). They keep refusing --json, before any work. The
// switch and bare `trace` answer with one document (M2c spec §2).
var traceRefusesJSON = map[string]bool{"run": true, "env": true, "mark": true, "summarize": true}

func runTrace(args []string, r *reporter) int {
	if len(args) > 0 && traceRefusesJSON[args[0]] && r.JSON() {
		return refuseJSON(r, "trace "+args[0])
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	if len(args) == 0 {
		return traceShow(h, r)
	}
	stdout, stderr := r.Stdout(), r.Stderr()
	switch args[0] {
	case "on":
		return traceOn(h, args[1:], r)
	case "off":
		return traceOff(h, args[1:], r)
	case "run":
		return traceRun(h, args[1:], stderr)
	case "env":
		return traceEnv(h, args[1:], stdout, stderr)
	case "mark":
		return traceMark(h, args[1:], stderr)
	case "summarize":
		return traceSummarize(h, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "chottag: unknown trace command %q\n%s", args[0], commandUsage("trace"))
		return r.FailNoText(exit.Usage, codeUsage, fmt.Sprintf("unknown trace command %q", args[0]), nil)
	}
}

// envLines are the lines that point a shell at a chottag proxy. The secret
// is read from its file when the shell evaluates them, so the lines
// themselves (printed to a terminal and to daemon.log) never carry it.
// dquoteEscaper keeps any CHOTTAG_HOME safe both inside the command
// substitution's own double quotes and inside NODE_EXTRA_CA_CERTS's.
func envLines(h, listen string) string {
	return fmt.Sprintf("export HTTPS_PROXY=\"http://%s:$(cat \"%s\")@%s\"\nexport NODE_EXTRA_CA_CERTS=\"%s\"\n",
		proxyauth.User, dquoteEscaper.Replace(proxyauth.Path(h)), listen, dquoteEscaper.Replace(filepath.Join(h, "ca", "ca.pem")))
}

func traceEnv(h string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultListen, "proxy address")
	if fs.Parse(args) != nil {
		return exit.Usage
	}
	// trace env never binds *listen (unlike trace run, which would already
	// refuse a malformed address at net.Listen) — it only embeds it, raw,
	// in the HTTPS_PROXY line below, so a value whose "port" is not
	// actually numeric must be refused here instead (fix round 1 item 4).
	if listenPortProblem(*listen) {
		fmt.Fprintf(stderr, "chottag: --listen %q: port must be 0-65535\n", *listen)
		return exit.Usage
	}
	if !isLoopbackListen(*listen) {
		fmt.Fprintf(stderr, "chottag: --listen %q must be loopback (127.0.0.0/8, ::1, or localhost)\n", *listen)
		return exit.Usage
	}
	if _, err := ca.LoadOrCreateLocked(filepath.Join(h, "ca")); err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	if _, err := proxyauth.LoadOrCreate(h); err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	fmt.Fprint(stdout, envLines(h, *listen))
	return exit.OK
}

func traceMark(h string, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace mark", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("log", filepath.Join(h, "trace.jsonl"), "trace log")
	if fs.Parse(args) != nil || fs.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: chottag trace mark TEXT...")
		return exit.Usage
	}
	w, err := tracelog.Open(*logPath)
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	defer w.Close()
	if err := w.Write(tracelog.Record{Kind: "mark", Mark: strings.Join(fs.Args(), " ")}); err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	return exit.OK
}

func traceSummarize(h string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace summarize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("log", filepath.Join(h, "trace.jsonl"), "trace log")
	all := fs.Bool("all", false, "summarize the whole log, not only since the last trace on")
	if fs.Parse(args) != nil {
		return exit.Usage
	}
	recs, err := tracelog.ReadAll(*logPath)
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	if !*all {
		recs = tracesum.SinceLastTraceOn(recs)
	}
	if err := tracesum.Summarize(recs).WriteText(stdout); err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	return exit.OK
}

const (
	// defaultTraceWindow and maxTraceWindow are M2c ruling T4: a trace
	// window always ends.
	defaultTraceWindow = time.Hour
	maxTraceWindow     = 24 * time.Hour

	traceOnUsage  = "usage: chottag trace on [--for DUR]"
	traceOffUsage = "usage: chottag trace off"
)

// traceResult is `trace`, `trace on` and `trace off`'s one document (M2c
// spec §2). Until and LastTraced are null when unknown, never absent.
type traceResult struct {
	Tracing    bool                  `json:"tracing"`
	Until      *time.Time            `json:"until"`
	LastTraced *status.TracedVersion `json:"lastTraced"`
}

// traceStatus is the switch as of now, plus what the daemon recorded.
// Until is set only while the window is open.
func traceStatus(h string, st store.State, now time.Time) (traceResult, error) {
	res := traceResult{Tracing: st.TracingAt(now)}
	if res.Tracing {
		u := st.Trace.Until
		res.Until = &u
	}
	f, err := status.Load(status.Path(h))
	if err != nil {
		return traceResult{}, err
	}
	if lt, ok := f.LastTraced(); ok {
		res.LastTraced = &lt
	}
	return res, nil
}

// formatTraceUntil is HH:MM when until is today (local), and "Jan 2 15:04"
// otherwise: a window can run up to 24h.
func formatTraceUntil(until, now time.Time) string {
	u, n := until.Local(), now.Local()
	if u.Year() == n.Year() && u.YearDay() == n.YearDay() {
		return u.Format("15:04")
	}
	return u.Format("Jan 2 15:04")
}

// traceReport prints the two status lines and reports the document.
func traceReport(h string, st store.State, now time.Time, r *reporter) int {
	res, err := traceStatus(h, st, now)
	if err != nil {
		return r.FailErr(err)
	}
	if res.Tracing {
		r.Text("trace on (until %s)\n", formatTraceUntil(*res.Until, now))
	} else {
		r.Text("trace off\n")
	}
	if res.LastTraced == nil {
		r.Text("last traced: never\n")
	} else {
		r.Text("last traced: Claude Code %s at %s\n", res.LastTraced.ClaudeVersion, res.LastTraced.At.Local().Format("Jan 2 15:04"))
	}
	return r.OK(res)
}

// traceShow is bare `trace`: it reads and writes nothing.
func traceShow(h string, r *reporter) int {
	st, err := store.Store{Dir: h}.Load()
	if err != nil {
		return r.FailErr(err)
	}
	return traceReport(h, st, time.Now(), r)
}

// traceOn opens or moves the trace window (M2c spec §2). The duration is
// validated before anything is read or written. The mark goes first, and
// only when no window is open: the daemon cannot trace a request before its
// window's mark exists, and moving an open window must not restart
// summarize's window (plan ruling 7).
func traceOn(h string, args []string, r *reporter) int {
	fs := flag.NewFlagSet("trace on", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	d := fs.Duration("for", defaultTraceWindow, "how long to trace (a Go duration, at most 24h)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage(traceOnUsage)
	}
	// At least 1s: until is cut to the second, so a shorter window could
	// open already closed (M2c final re-review).
	if *d < time.Second || *d > maxTraceWindow {
		return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("--for %s: want a duration of at least 1s and at most 24h", *d), nil)
	}
	s := store.Store{Dir: h}
	cur, err := s.Load()
	if err != nil {
		return r.FailErr(err)
	}
	now := time.Now()
	if !cur.TracingAt(now) {
		if err := appendTraceMark(h, tracesum.TraceOnMark); err != nil {
			return r.FailErr(err)
		}
	}
	until := now.Add(*d).Truncate(time.Second)
	st, err := s.Update(func(st *store.State) error {
		st.SetTraceUntil(until)
		return nil
	})
	if err != nil {
		return r.FailErr(err)
	}
	return traceReport(h, st, now, r)
}

// traceOff ends the window now. On a home with no state.json it creates
// one, as `notify` does (M2b ruling 4).
func traceOff(h string, args []string, r *reporter) int {
	rest, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, traceOffUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(rest) != 0 {
		return r.Usage(traceOffUsage)
	}
	st, err := store.Store{Dir: h}.Update(func(st *store.State) error {
		st.ClearTrace()
		return nil
	})
	if err != nil {
		return r.FailErr(err)
	}
	return traceReport(h, st, time.Now(), r)
}

// appendTraceMark appends one mark to the daemon's trace log,
// <home>/trace.jsonl: one O_APPEND write, beside the daemon's own writer
// (M2c spec §3).
func appendTraceMark(h, text string) error {
	w, err := tracelog.Open(filepath.Join(h, "trace.jsonl"))
	if err != nil {
		return err
	}
	if err := w.Write(tracelog.Record{Kind: "mark", Mark: text}); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

// isLoopbackListen reports whether addr's host is loopback (127.0.0.0/8,
// ::1, or "localhost") AND, when addr carries one, its port is a valid
// 0-65535 TCP port number. chottag's proxy forwards a bearer (and, with
// --swap, a swapped one), so it must never be reachable from another
// host — and, separately, an empty or non-numeric port is refused outright
// (a controller ruling, fix round 2 item C: neither "no port at all" nor a
// named service like "http" is accepted here, even though net.Dial would
// happily resolve the latter).
//
// net.SplitHostPort does not itself validate that a port it successfully
// split off is actually numeric — it only requires exactly one unescaped
// colon (outside brackets) — so a caller that trusted a "loopback" verdict
// enough to embed addr's raw port substring somewhere else (trace env's
// export line, fix round 1 item 4) would not see one for an addr whose
// "port" carries shell metacharacters. validListenPort (via
// listenPortProblem) closes that.
func isLoopbackListen(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	} else if !validListenPort(port) {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listenPortProblem reports whether addr carries a port substring that
// fails validListenPort — distinct from isLoopbackListen's combined
// host+port verdict, so a caller can give a bad port its own message (fix
// round 2 item C: "--listen %q: port must be 0-65535", not the generic
// "must be loopback" text, which says nothing about what was actually
// wrong when the host looked fine). false for an addr with no port at all
// (net.SplitHostPort itself errors on that shape; isLoopbackListen's own
// "must be loopback" message already covers it, since a bare host is
// never usable as a listen address either way).
func listenPortProblem(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && !validListenPort(port)
}

// validListenPort reports whether port is exactly the decimal digits of a
// valid TCP port number (0-65535), rejecting anything else outright —
// never trying to interpret it, only to refuse it.
func validListenPort(port string) bool {
	if port == "" {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n <= 65535
}

// parseIntercept normalizes --intercept's comma-separated host suffixes:
// trimmed and lower-cased (host names are case-insensitive, and a stray
// space or mixed case would silently never match), with empty entries
// (e.g. a trailing comma) dropped.
func parseIntercept(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// swapPreflight checks every configured --swap slot has a currently-valid
// token before the proxy starts, printing a "swap CLASS -> DIR" line only
// for slots that are actually usable. It reports (and never prints as
// ready) an expired token instead of quietly starting a slot that will pass
// every request through unswapped from the first use.
func swapPreflight(swaps swapFlags, read func(string) (creds.Token, error), stderr io.Writer, now time.Time) bool {
	for c, dir := range swaps {
		tok, err := read(dir)
		if err != nil {
			fmt.Fprintf(stderr, "chottag: --swap %s=%s: %v\n", c, dir, err)
			return false
		}
		if !tok.Valid(now) {
			fmt.Fprintf(stderr, "chottag: --swap %s=%s: token expired at %s; run: CLAUDE_CONFIG_DIR=%s claude auth status\n",
				c, dir, tok.ExpiresAt.Format(time.RFC3339), dir)
			return false
		}
		fmt.Fprintf(stderr, "chottag: swap %s -> %s: %s\n", c, dir, tok)
	}
	return true
}

type swapFlags map[router.Class]string

func (s swapFlags) String() string { return fmt.Sprint(map[router.Class]string(s)) }

func (s swapFlags) Set(v string) error {
	class, dir, ok := strings.Cut(v, "=")
	c := router.Class(class)
	if !ok || dir == "" || (c != router.Serving && c != router.Remote) {
		return errors.New("want CLASS=DIR with CLASS serving|remote")
	}
	s[c] = dir
	return nil
}

// throttle wraps fn so that only the first call inside any interval window
// actually runs fn; later calls within the same window are silently
// dropped. Used to keep a noisy per-request signal (a trace-log write
// failure, a selector passthrough/owner-unregistered event) from flooding
// stderr — a serving account stuck needs-login can otherwise print one line
// per request, hundreds per session, and unbounded once M1c turns this into
// a daemon.
func throttle[T any](interval time.Duration, fn func(T)) func(T) {
	var mu sync.Mutex
	var last time.Time
	return func(v T) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < interval {
			return
		}
		last = now
		fn(v)
	}
}

// logErrorThrottle prints trace-log write failures to stderr, at most once
// every 10s, so a full disk doesn't flood the terminal.
func logErrorThrottle(stderr io.Writer) func(error) {
	return throttle(10*time.Second, func(err error) {
		fmt.Fprintf(stderr, "chottag: trace log write failed: %v\n", err)
	})
}

// traceRunConfig builds `trace run`'s proxy.Config, pulled out to its own
// function so it can be tested directly rather than only reachable through
// a full traceRun invocation (the wireProxyConfig pattern, proxy.go).
// TraceLog stays nil: M2c split Config.Shapes into Config.Tracing and
// Config.TraceLog, but `trace run` still has only the one log, so shapesOn
// alone decides whether every request's shapes land in it, exactly as
// Config.Shapes did before the split.
func traceRunConfig(authority *ca.Authority, secret proxyauth.Secret, lw *tracelog.Writer, intercept []string, shapesOn, limitFingerprint bool, onLogError func(error)) proxy.Config {
	return proxy.Config{
		CA: authority, ProxyAuth: secret, Intercept: proxy.SuffixMatcher(intercept), Log: lw,
		Tracing: func() bool { return shapesOn }, LimitFingerprint: limitFingerprint, OnLogError: onLogError,
	}
}

// traceRunUsageError marks an error traceRunSetup returns as a usage error
// (exit 2), not a startup failure (exit 1): a malformed --intercept suffix
// or one outside the CA's name constraints (Ruling 17, fix round 1 items
// 1/6). traceRun tells the two apart with errors.As, the same distinction
// daemon_stop.go's signalFailedError draws for its own pair of outcomes.
type traceRunUsageError struct{ msg string }

func (e *traceRunUsageError) Error() string { return e.msg }

// traceRunSetup loads the CA and the install secret, validates and checks
// --intercept's suffixes against the CA's name constraints, opens the
// trace log, and builds `trace run`'s proxy.Config — including its
// ProxyAuth (Ruling 16). Pulled into its own function, mirroring
// wireProxyConfig (proxy.go): TestTraceRunSetupCarriesTheSecret calls this
// directly and fails if the secret is ever dropped at this call site, and
// TestTraceRunSetupRefusesAnInterceptOutsideTheCA/...RefusesAWildcard
// exercise the two refusals without ever reaching net.Listen — so neither
// test can hang even if the check itself were removed (fix round 1, items
// 1 and 2).
func traceRunSetup(h, logPath, interceptRaw string, shapesOn, limitFingerprint bool, onLogError func(error)) (proxy.Config, error) {
	authority, err := ca.LoadOrCreateLocked(filepath.Join(h, "ca"))
	if err != nil {
		return proxy.Config{}, err
	}
	secret, err := proxyauth.LoadOrCreate(h)
	if err != nil {
		return proxy.Config{}, err
	}
	suffixes := parseIntercept(interceptRaw)
	for _, s := range suffixes {
		// A leading "." or a "*" is never a bare domain suffix
		// SuffixMatcher/Permits can match against (fix round 1, item 6):
		// refused before it ever reaches the CA constraint check below,
		// whose own message would otherwise be a confusing way to say the
		// same thing.
		if strings.HasPrefix(s, ".") || strings.Contains(s, "*") {
			return proxy.Config{}, &traceRunUsageError{fmt.Sprintf("--intercept %q: give a bare domain suffix like anthropic.com", s)}
		}
		if !authority.Permits(s) {
			return proxy.Config{}, &traceRunUsageError{fmt.Sprintf("--intercept %q is outside this CA's name constraints (%s)", s, strings.Join(ca.PermittedDomains, ", "))}
		}
	}
	lw, err := tracelog.Open(logPath)
	if err != nil {
		return proxy.Config{}, err
	}
	return traceRunConfig(authority, secret, lw, suffixes, shapesOn, limitFingerprint, onLogError), nil
}

func traceRun(h string, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("trace run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", defaultListen, "proxy address (loopback)")
	logPath := fs.String("log", filepath.Join(h, "trace.jsonl"), "trace log")
	shapes := fs.Bool("shapes", false, "record JSON shapes (types only) of bodies")
	limitFingerprint := fs.Bool("limit-fingerprint", false, "record a redacted usage-limit classifier fingerprint (header names, an allowlist of rate-limit header values, error.type, and an extracted reset timestamp) for responses with status >= 400 — never the body")
	intercept := fs.String("intercept", strings.Join(proxy.DefaultTraceSuffixes, ","), "comma-separated host suffixes to intercept")
	kcService := fs.String("keychain-service", "", "override the macOS Keychain service name for --swap slots")
	swaps := swapFlags{}
	fs.Var(swaps, "swap", "CLASS=DIR: send CLASS (serving|remote) requests with the login in account dir DIR (repeatable, experimental)")
	if fs.Parse(args) != nil {
		return exit.Usage
	}
	if listenPortProblem(*listen) {
		fmt.Fprintf(stderr, "chottag: --listen %q: port must be 0-65535\n", *listen)
		return exit.Usage
	}
	if !isLoopbackListen(*listen) {
		fmt.Fprintf(stderr, "chottag: --listen %q must be loopback (127.0.0.0/8, ::1, or localhost)\n", *listen)
		return exit.Usage
	}

	cfg, err := traceRunSetup(h, *logPath, *intercept, *shapes, *limitFingerprint, logErrorThrottle(stderr))
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		var ue *traceRunUsageError
		if errors.As(err, &ue) {
			return exit.Usage
		}
		return exit.Error
	}
	defer cfg.Log.Close()

	if len(swaps) > 0 {
		reader := creds.Reader{GOOS: runtime.GOOS, Run: creds.ExecRunner, ServiceOverride: *kcService}
		if !swapPreflight(swaps, reader.Read, stderr, time.Now()) {
			return exit.Error
		}
		cfg.Choose = &slotSwapper{dirs: swaps, read: reader.Read, warn: stderr, cache: map[router.Class]cachedToken{}}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	srv := &http.Server{Handler: proxy.New(cfg)}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); srv.Close() }()

	fmt.Fprintf(stderr, "chottag trace listening on %s, logging to %s\n%s", *listen, *logPath, envLines(h, *listen))
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	return exit.OK
}

type cachedToken struct {
	tok creds.Token
	err error
	at  time.Time
}

// slotSwapper re-reads a slot's token at most every 30 s and never refreshes it.
type slotSwapper struct {
	dirs  map[router.Class]string
	read  func(string) (creds.Token, error)
	warn  io.Writer
	mu    sync.Mutex
	cache map[router.Class]cachedToken
}

func (s *slotSwapper) token(c router.Class) (string, bool) {
	dir, ok := s.dirs[c]
	if !ok {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Cache both outcomes for 30s so a slot that's unusable (locked
	// Keychain, expired token) doesn't re-run the read — a macOS Keychain
	// prompt/query — under this mutex for every single swapped request,
	// and so the warning below doesn't repeat once per request.
	if ct, ok := s.cache[c]; ok && now.Sub(ct.at) < 30*time.Second {
		if ct.err != nil {
			return "", false
		}
		if ct.tok.Valid(now) {
			return ct.tok.AccessToken, true
		}
	}
	tok, err := s.read(dir)
	if err == nil && !tok.Valid(now) {
		err = errors.New("token expired (run: CLAUDE_CONFIG_DIR=" + dir + " claude auth status)")
	}
	s.cache[c] = cachedToken{tok: tok, err: err, at: now}
	if err != nil {
		fmt.Fprintf(s.warn, "chottag: %s slot unusable, passing through: %v\n", c, err)
		return "", false
	}
	return tok.AccessToken, true
}

// Choose implements proxy.Chooser for `trace run --swap`: a slot per class,
// no owner map, no refresh.
func (s *slotSwapper) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	tok, ok := s.token(d.Class)
	if !ok {
		return "", "", false, false
	}
	return string(d.Class), tok, false, true
}

// Record does nothing: the trace tool observes, it does not own objects.
func (s *slotSwapper) Record(router.Kind, []string, string) {}

// Refresh does nothing: `trace run --swap` never refreshes a slot token.
func (s *slotSwapper) Refresh(context.Context, string) (string, bool) { return "", false }
