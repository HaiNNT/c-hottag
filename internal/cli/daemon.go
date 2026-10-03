package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/rotate"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// daemonChdir is `daemon run`'s chdir(2) to its own CHOTTAG_HOME (L5): a
// daemon the shim spawns otherwise inherits the cwd of whichever shell
// first ran claude, possibly an untrusted project, and would pin that
// directory for the daemon's whole life. It is a package-level func var,
// like signalFn (daemon_common.go), but its TestMain default
// (invariants_test.go) is a safe no-op rather than a panic (Ruling 24): it
// is read-only in effect on anything outside this process, about ten
// existing daemon tests reach this line, and a real chdir would move the
// whole test binary's cwd — which breaks every relative-path test
// (e.g. `../../install.sh`) — for every test that runs afterward, not just
// the one that reached it.
var daemonChdir = os.Chdir

// SetDaemonChdirForTest swaps daemonChdir for fn and returns a func that
// restores whatever was installed at the moment of the call: never
// production's os.Chdir by name. That keeps TestMain's default armed across
// every stub-and-restore cycle (F130). Same contract as SetSignalForTest.
func SetDaemonChdirForTest(fn func(dir string) error) (restore func()) {
	orig := daemonChdir
	daemonChdir = fn
	return func() { daemonChdir = orig }
}

// daemonRunSubArgsForTest, when non-nil, receives the exact subcommand argv
// runDaemonRun is about to hand runProxyWithSignal, right before daemonChdir
// runs. nil in every production build, like credsReadForTest and
// proxyDialContextForTest (proxy.go): a fix-round-1 regression test observes
// the resolved --log value through it without ever letting
// runProxyWithSignal actually open a file — the pipeline is stopped right
// after with the existing SetDaemonChdirForTest seam.
var daemonRunSubArgsForTest func(sub []string)

// daemonLogKeep is daemon.log's retention count: it feeds BOTH the
// rotate.Config the daemon opens daemon.log with below AND the
// daemonLogSibling guard, so that if the retention count ever changes the
// guard follows it rather than silently stopping short of the family it
// protects.
const daemonLogKeep = 5

// daemonLogClock stamps every daemon.log line with local time (R158). A
// variable so a test can fix it.
var daemonLogClock = time.Now

// sameFile reports whether a and b name the same file. Lexical comparison
// (after filepath.Abs) catches the common case — a literal or
// shell-expanded match, e.g. `--log ~/.chottag/daemon.log` resolving to the
// same string as h/daemon.log. os.SameFile catches the aliasing case
// (symlink or hardlink to the same inode) too, but only when both paths
// already exist; best-effort by design, since rotate.Open's whole point is
// to create the target if it is missing, so the caller's --log path may not
// exist yet at the moment this runs.
func sameFile(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	if aa == bb {
		return true, nil
	}
	ai, aerr := os.Stat(aa)
	bi, berr := os.Stat(bb)
	if aerr != nil || berr != nil {
		return false, nil
	}
	return os.SameFile(ai, bi), nil
}

// daemonLogSibling reports whether path names daemon.log itself, or one of
// its rotation siblings daemon.log.1 .. daemon.log.daemonLogKeep, under home
// h. rotate.rotateLocked (internal/rotate/rotate.go) renames onto exactly
// those names on every one of daemon.log's rolls, so a second independent
// rotate.Writer opened at any of them would race that rename and corrupt
// both files the first time the two writers' rotations interleave.
//
// Self-contained (composes only sameFile and filepath, takes h rather than
// reading home() itself) so the `daemon run --log` guard's over-reach case —
// a path that must be ACCEPTED — can be tested directly, the same reason
// daemonSubcommandArgs below is self-contained: calling runDaemonCmd itself
// past this guard reaches runProxyWithSignal (and, inside it, the real
// net.Listen), which a test asserting acceptance must never do.
func daemonLogSibling(path, h string) (bool, error) {
	daemonLogPath := filepath.Join(h, "daemon.log")
	if same, err := sameFile(path, daemonLogPath); err != nil || same {
		return same, err
	}
	for i := 1; i <= daemonLogKeep; i++ {
		same, err := sameFile(path, fmt.Sprintf("%s.%d", daemonLogPath, i))
		if err != nil || same {
			return same, err
		}
	}
	return false, nil
}

// daemonSubcommandArgs builds the `proxy run` argument list the daemon runs
// under. It is a separate function so the flag plumbing can be tested
// without starting a proxy.
//
// defaultLog is the --log FlagSet default, not a post-hoc prepend: letting
// `flag` itself resolve "unset -> defaultLog" versus an explicit, empty
// --log -> disabled is what makes both cases correct at once. An earlier version
// prepended the default ahead of the caller's own args and relied on
// flag's last-wins parsing to let an explicit --log override it — that
// broke silently under argument reordering (review finding, M1c4 Task 3
// round 1): appending the default AFTER the caller's args instead of
// before would have made `--log ""` lose to the default every time,
// defeating the privacy off-switch with the suite still green throughout.
//
// --log is ALWAYS passed to the subcommand, including when empty: the
// daemon's whole point is that nobody is at a terminal, so inheriting
// `proxy run`'s own default silently would reproduce F68 — an
// unbounded-by-configuration file the operator cannot redirect or switch
// off.
//
// defaultUpstream is CHOTTAG_UPSTREAM_PROXY's value (runDaemonCmd), the
// spawned daemon's env handoff for a pre-existing HTTPS_PROXY (fix round 3,
// D2). It is the --upstream-proxy FlagSet default here for the identical
// reason defaultLog is: an explicit --upstream-proxy must still win, and
// letting `flag` itself resolve that is what makes both cases correct
// without a post-hoc override.
//
// The args this returns are consumed entirely in-process by
// runProxyWithSignal below, never by os/exec (F81's own audit of exactly
// this call), so putting the resolved upstream in them does not reintroduce
// the process-table exposure the env handoff exists to avoid — that
// exposure is specific to a real child process's argv, which this is not.
func daemonSubcommandArgs(flags []string, listen, defaultLog, defaultUpstream string) ([]string, bool) {
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	claudePath := fs.String("claude", "", "path to the real claude binary")
	logPath := fs.String("log", defaultLog, "request log path; empty disables request logging")
	upstream := fs.String("upstream-proxy", defaultUpstream, "route chottag's own upstream traffic through this proxy")
	if err := fs.Parse(flags); err != nil {
		return nil, false
	}
	if len(fs.Args()) != 0 {
		// A stray positional argument (e.g. a misplaced value or typo)
		// stops flag.Parse silently rather than erroring, so anything after
		// it — including a real --log the caller meant to send — is never
		// seen, and the default above wins instead. That is a silent
		// failure of the off-switch, not just a usage error, so it is
		// rejected explicitly rather than left to flag's default behaviour.
		return nil, false
	}
	args := []string{"run", "--listen", listen, "--log", *logPath}
	if *claudePath != "" {
		args = append(args, "--claude", *claudePath)
	}
	if *upstream != "" {
		args = append(args, "--upstream-proxy", *upstream)
	}
	return args, true
}

// daemonUsage is `chottag daemon`'s usage line.
const daemonUsage = "usage: chottag daemon run [--claude PATH] [--log PATH] | start | stop [--force] | restart [--force] | logs [-n N] [-f]"

// runDaemonCmd dispatches `chottag daemon <verb>`. `run` is the daemon
// process itself. The shim starts it on demand (spec §4.3), and the other
// four verbs are conveniences that find it through its held lock (spec §5,
// R42).
func runDaemonCmd(args []string, r *reporter) int {
	if len(args) == 0 {
		return r.Usage(daemonUsage)
	}
	switch args[0] {
	case "run":
		// run streams daemon.log and never returns while healthy (spec
		// §5.3): refused under --json before anything starts.
		if r.JSON() {
			return refuseJSON(r, "daemon run")
		}
		// run alone gets the full args, "run" included: runDaemonRun
		// predates this dispatcher and already skips args[0] itself
		// (fs.Parse(args[1:]), below) — passing args[1:] here would skip
		// its real first flag instead. Every other verb below never had
		// that convention, so it gets args[1:] — its own flags only.
		return runDaemonRun(args, r)
	case "start":
		return runDaemonStart(args[1:], r)
	case "stop":
		return runDaemonStop(args[1:], r)
	case "restart":
		return runDaemonRestart(args[1:], r)
	case "logs":
		if r.JSON() {
			return refuseJSON(r, "daemon logs")
		}
		// Ctrl-C ends `logs -f`, and that is a normal exit 0 (spec §5).
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDaemonLogs(ctx, args[1:], r)
	default:
		fmt.Fprintf(r.Stderr(), "chottag: unknown daemon command %q\n%s\n", args[0], daemonUsage)
		return r.FailNoText(exit.Usage, codeUsage, fmt.Sprintf("unknown daemon command %q", args[0]), nil)
	}
}

// runDaemonRun runs the supervisor-managed daemon. Unlike `proxy run` it
// resolves its port from state.json, records nothing on the process's real
// stdout, and logs to a rotating daemon.log instead (spec §4.6). It takes
// the invocation's reporter like every command, but uses only its stderr:
// nothing is ever written to the reporter's stdout, and runProxyWithSignal
// is called with logw in its place. `daemon run` refuses --json (spec §5.3).
//
// It does NOT detach: a supervisor, when there is one, owns the process,
// and a daemon that forks fights its own supervisor. It holds
// <home>/run/daemon.lock (internal/daemonlock) from before daemon.log is
// opened until it returns, which is the process's whole life. That lock
// is how `daemon stop|start|restart` find it (spec §4.6).
//
// The port itself is never probed or searched (§4.8): st.ResolvedPort()
// names the port to bind, and runDaemon's own net.Listen (proxy.go, reached
// through runProxyWithSignal below) is the single, authoritative attempt —
// no courtesy bind-and-close here first. A taken port is a startup failure
// naming the port, not a scan — see store.State.ResolvedPort's doc comment
// for why the search this used to do was removed. Because that failure
// happens deep inside runProxyWithSignal, whose own stderr slot is logw
// (daemon.log) for this caller, runProxyWithSignal's startupErr parameter
// is what gets the message to this process's real stderr too — see
// daemonDeps.StartupErr's doc comment (proxy.go) for why.
func runDaemonRun(args []string, r *reporter) int {
	stderr := r.Stderr()
	h, err := home()
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	// The daemon's own default matches proxy run's (F68) — the difference
	// is that it is bounded and overridable. Computed before the FlagSet
	// below so it can be the --log default there too: letting flag resolve
	// "unset -> this default" versus an explicit, empty --log -> disabled is
	// what makes both cases correct without a post-hoc prepend (review
	// finding, M1c4 Task 3 round 1 — a prepended default combined with
	// flag's last-wins parsing broke silently under argument reordering).
	defaultLog := filepath.Join(h, "proxy.jsonl")

	// defaultUpstream is CHOTTAG_UPSTREAM_PROXY, spawnDaemon's env handoff
	// for this shell's pre-existing HTTPS_PROXY (internal/shim's
	// daemonSpawnEnv). It moved here from argv (fix round 3, D2 —
	// internal/shim/shim.go's daemonSpawnArgs doc comment has the full
	// reasoning: argv is visible to `ps`/`/proc/<pid>/cmdline`, environ is
	// not). Same defaulting technique as defaultLog just above: passed as
	// BOTH FlagSets' own --upstream-proxy default below, so an explicit
	// --upstream-proxy always overrides it and an absent one falls back to
	// it, with flag.Parse itself resolving which — no post-hoc override
	// after the fact to get wrong.
	defaultUpstream := os.Getenv(shim.UpstreamProxyEnvVar)

	// Parsed once here, with its own FlagSet writing the standard flag
	// package usage text to the real stderr, so an unknown or malformed
	// flag, or a stray positional argument, reports exactly as it always
	// did. daemonSubcommandArgs below re-parses the same args[1:] silently
	// to build the subcommand's argv — it is deliberately self-contained so
	// it can be tested without starting a daemon.
	//
	// claude, logPath and upstream are all read here for the SAME reason:
	// each needs to reject or rewrite one specific input before
	// daemonSubcommandArgs re-parses args[1:] to build the subcommand's own
	// argv — never to decide, on its own, what the daemon actually runs
	// with, which stays daemonSubcommandArgs's defaulting alone (see its
	// doc comment). claude is read only to absolutise a value containing a
	// path separator (the collision check just below does the equivalent
	// for logPath; upstream's read only validates its shape). Reaching for
	// any of them to build the subcommand's argv directly, instead of
	// re-appending the resolved value as the LAST occurrence for
	// daemonSubcommandArgs's own last-wins re-parse to pick up, would
	// shadow daemonSubcommandArgs's defaulting the way building the argv
	// here outright would.
	fs := flag.NewFlagSet("daemon run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	claudePath := fs.String("claude", "", "path to the real claude binary")
	logPath := fs.String("log", defaultLog, "request log path; empty disables request logging")
	upstream := fs.String("upstream-proxy", defaultUpstream, "route chottag's own upstream traffic through this proxy (e.g. http://127.0.0.1:3128)")
	if err := fs.Parse(args[1:]); err != nil {
		return exit.Usage
	}
	if len(fs.Args()) != 0 {
		// flag.Parse stops silently at the first non-flag token rather than
		// erroring, so anything after it — including a real --log the
		// caller meant to send — is never parsed and the default silently
		// wins instead. That is a silent failure of the privacy off-switch,
		// not merely a usage error, so it is rejected explicitly.
		//
		// The stray value is echoed so the user can tell which argument was
		// rejected — but only when it cannot carry a secret. The hazard is
		// userinfo: user:pass@host:port, with or without a scheme (the
		// shape http_proxy and `curl -x` accept, and the natural typo for
		// someone who forgot --upstream-proxy, which is now declared just
		// above and reaches only real stderr — a retained file under
		// launchd). "@" is what userinfo always requires, so a literal "@"
		// or a URL parse that finds a non-nil User is redacted with the
		// existing redact.UpstreamProxy rather than a second redactor;
		// everything else — "somearg", "typo.txt", "--lo", "host:8080" —
		// is an ordinary typo with no way to carry a credential and stays
		// legible. A bare secret with no "@" (someone typing a password as
		// its own positional argument) is NOT caught here: nothing here
		// can distinguish that from a filename, and pretending otherwise
		// would trade a real signal for a false sense of safety. That is a
		// deliberate boundary, not an oversight.
		//
		// The `u.User != nil` half of the condition below is currently
		// dead: net/url's parseAuthority finds userinfo with a literal
		// strings.LastIndex(authority, "@"), so u.User can only be non-nil
		// when arg already contains a literal "@" — meaning the "@" check
		// has always already short-circuited the ||. It stays anyway,
		// because deleting the WRONG half is the natural mistake here: the
		// "@" check is the load-bearing one — "user@host" and
		// "bob@example.com" both parse with u.User == nil (no "//"
		// authority prefix, so they're opaque paths, not authorities), so
		// dropping "@" for the "more precise"-looking URL check would
		// silently stop catching them. If this ever gets simplified,
		// delete the u.User clause, not the "@" one.
		arg := fs.Args()[0]
		shown := arg
		if u, err := url.Parse(arg); strings.Contains(arg, "@") || (err == nil && u.User != nil) {
			shown = redact.UpstreamProxy(arg)
		}
		fmt.Fprintf(stderr, "chottag: unexpected argument %q\nusage: chottag daemon run [--claude PATH] [--log PATH]\n", shown)
		return exit.Usage
	}

	// A non-empty --log is made absolute here, against the cwd this process
	// actually started in, before anything below can move that cwd. Without
	// this, a relative --log would silently resolve against CHOTTAG_HOME
	// instead: daemonChdir(h) runs further down, and runProxyWithSignal
	// (proxy.go) does not open the trace log until well after that, so a
	// raw relative path threaded through unresolved would be opened
	// relative to h, not to wherever the operator actually ran `daemon run`
	// from (review finding, fix round 1).
	if *logPath != "" {
		abs, err := filepath.Abs(*logPath)
		if err != nil {
			fmt.Fprintf(stderr, "chottag: --log %q: %v\n", *logPath, err)
			return exit.Usage
		}
		*logPath = abs
	}

	// A --claude value containing a path separator is made absolute here
	// too, against this same original cwd, for the identical reason as
	// --log just above (review finding M1): daemonChdir(h) below moves the
	// cwd runProxyWithSignal's own exec.LookPath (via refresh, which sets
	// cmd.Dir to the slot dir) would otherwise resolve a raw relative
	// --claude against, silently pointing it at the wrong binary or at
	// nothing. An empty value (the default) is left alone: the proxy
	// resolves the real claude on every refresh (newRefresher), from
	// state.json's cache or PATH, never from the cwd.
	if *claudePath != "" && strings.ContainsRune(*claudePath, filepath.Separator) {
		abs, err := filepath.Abs(*claudePath)
		if err != nil {
			fmt.Fprintf(stderr, "chottag: --claude %q: %v\n", *claudePath, err)
			return exit.Usage
		}
		*claudePath = abs
	}

	// One daemon per home (spec §4.6, R42). The lock is taken BEFORE
	// daemon.log is opened, not merely before the bind. A second daemon
	// that opened daemon.log first would hold a second rotate.Writer on the
	// running daemon's own log, which is the corruption daemonLogSibling
	// guards against, just to report that it cannot start. So its refusal
	// goes to the real stderr alone, which is where a supervisor and an
	// operator look anyway. The lock is held until this function returns.
	// In production that is the process's whole life, and the kernel drops
	// it on any exit, including SIGKILL.
	release, err := daemonlock.Acquire(h, daemonlock.Current())
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	defer release()

	logw, err := rotate.Open(rotate.Config{
		Path:     filepath.Join(h, "daemon.log"),
		MaxBytes: 8 << 20,
		Keep:     daemonLogKeep,
		Stamp:    daemonLogClock,
	})
	if err != nil {
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Error
	}
	defer logw.Close()

	// --log pointing at daemon.log itself, or at one of its own rotation
	// siblings daemon.log.1 .. daemon.log.daemonLogKeep, would open a
	// SECOND, independent rotate.Writer on a path logw already owns (or will
	// own the moment daemon.log next rolls onto it): each rotate.Writer
	// keeps its own size counter and file descriptor (internal/rotate has
	// no cross-instance coordination — it was never designed to), so two of
	// them racing rotation on the same file would shred both logs the first
	// time their rotations interleave. Newly reachable now that `daemon
	// run` has its own --log (M1c4 Task 6a); rejected here rather than left
	// to whichever of the two writers loses that race first (F90).
	//
	// daemonLogSibling is the self-contained helper (defined above, next to
	// sameFile) so the "a mere prefix match is not a sibling" case can be
	// tested directly without going through runDaemonCmd and reaching
	// runProxyWithSignal (and, inside it, the real net.Listen).

	// From here on, a failure goes to daemon.log AND the real stderr
	// through startupWriter, the one helper every pre-serve path uses
	// (spec §4.6, F109). Here logw is its "stderr" and this function's
	// real stderr is its "startupErr".
	if *logPath != "" {
		daemonLogPath := filepath.Join(h, "daemon.log")
		sibling, err := daemonLogSibling(*logPath, h)
		if err != nil {
			fmt.Fprintln(startupWriter(logw, stderr), "chottag:", err)
			return exit.Error
		}
		if sibling {
			msg := fmt.Sprintf("chottag: --log %q must not be daemon.log itself or one of its rotation siblings (%s, %s.1..%s.%d): a second independent rotating writer on any of those paths would corrupt it, or daemon.log, the next time daemon.log rotates", *logPath, daemonLogPath, daemonLogPath, daemonLogPath, daemonLogKeep)
			fmt.Fprintln(startupWriter(logw, stderr), msg)
			return exit.Error
		}
	}

	if _, err := parseUpstreamProxy(*upstream); err != nil {
		msg := fmt.Sprintf("chottag: --upstream-proxy %q: %v", redact.UpstreamProxy(*upstream), err)
		fmt.Fprintln(startupWriter(logw, stderr), msg)
		return exit.Error
	}

	st, err := store.Store{Dir: h}.Load()
	if err != nil {
		fmt.Fprintln(startupWriter(logw, stderr), "chottag:", err)
		return exit.Error
	}

	listen := net.JoinHostPort("127.0.0.1", strconv.Itoa(st.ResolvedPort()))
	// *logPath and *claudePath by this point are each either "" (disabled,
	// or no --claude given) or the already-abs path resolved above. Both
	// are appended, not merely relied on through defaultLog, as the LAST
	// --log / --claude occurrence in the args daemonSubcommandArgs
	// re-parses: flag's last-wins parsing (see daemonSubcommandArgs's own
	// doc comment) then makes each win over whatever raw (possibly
	// relative) value the caller actually typed, which is exactly what
	// closes the hole above — daemonSubcommandArgs itself stays
	// self-contained and untouched. An empty *claudePath here re-appends
	// "--claude", "" as the last occurrence too, which daemonSubcommandArgs
	// treats identically to no --claude at all (its own `*claudePath != ""`
	// check), so the no-flag case is unaffected.
	sub, ok := daemonSubcommandArgs(append(append([]string{}, args[1:]...), "--log", *logPath, "--claude", *claudePath), listen, defaultLog, defaultUpstream)
	if !ok {
		// Unreachable in practice: fs above already parsed args[1:] (the
		// same flags, and rejected the same stray-positional-argument
		// case) successfully.
		fmt.Fprintln(stderr, "chottag: internal error building the daemon subcommand")
		return exit.Error
	}
	if daemonRunSubArgsForTest != nil {
		// nil in every production build. A fix-round-1 regression test
		// observes the exact argv runProxyWithSignal is about to receive —
		// in particular the resolved --log value — without ever letting
		// runProxyWithSignal actually open it: daemonChdir below is what a
		// test stops the pipeline with (SetDaemonChdirForTest), the same
		// technique TestDaemonRunChdirsToItsHome already uses.
		daemonRunSubArgsForTest(sub)
	}
	// sig is nil, exactly as runProxy passes: installShutdown then registers
	// the real signal.Notify channel, which is how launchd/systemd SIGTERM
	// reaches this daemon. logw fills BOTH the stdout and stderr slots —
	// runDaemon (internal/cli/proxy.go) writes the resolved listen address
	// to its stdout parameter as well as its startup line to stderr, and
	// both are routine daemon output that belongs in daemon.log, not on the
	// process's real stdout. The reporter's stdout is
	// deliberately never passed through. The real stderr IS passed as
	// startupErr, so a pre-serve failure inside runDaemon — most notably a
	// taken port, since nothing here probes it first (§4.8) — reaches this
	// process's real stderr too, not just daemon.log (see
	// daemonDeps.StartupErr's doc comment in proxy.go).
	//
	// M1: pin CHOTTAG_HOME to h — already made absolute by home() above,
	// against this process's ORIGINAL cwd — before daemonChdir moves that
	// cwd. Without this, a relative CHOTTAG_HOME (exported by the rc block
	// couldNotConfirmMsg tells a user to run `daemon run` under) would
	// re-resolve against h itself the next time something in-process calls
	// home() again: runProxyWithSignal (proxy.go) does exactly that, and
	// would otherwise serve a second, empty home nested inside the first
	// (review finding). sub above is already built from the pre-chdir h, so
	// this Setenv is for every OTHER home() call downstream, not sub.
	//
	// Restored on return (final re-review NEW-5), not left permanently
	// mutated: in production this is a no-op in substance, since the
	// process exits right after runDaemonRun returns either way, but it
	// closes a real test-pollution hazard — a future test that reaches
	// this line without its own t.Setenv("CHOTTAG_HOME", …) would otherwise
	// leak an absolute value into every test that runs afterward in the
	// same binary, silently outliving this call.
	origHome, hadHome := os.LookupEnv("CHOTTAG_HOME")
	os.Setenv("CHOTTAG_HOME", h)
	defer func() {
		if hadHome {
			os.Setenv("CHOTTAG_HOME", origHome)
		} else {
			os.Unsetenv("CHOTTAG_HOME")
		}
	}()
	// L5: a daemon the shim spawns inherits the cwd of whichever shell first
	// ran claude, possibly an untrusted project, and would pin that
	// directory for days. It runs from its own home instead.
	if err := daemonChdir(h); err != nil {
		fmt.Fprintln(startupWriter(logw, stderr), "chottag: cannot enter", h+":", err)
		return exit.Error
	}
	return runProxyWithSignal(sub, logw, logw, stderr, nil)
}
