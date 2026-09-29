package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// writeStateWithPort writes state.json under home with Port set to port, via
// store.Store's own Update/Save path — never hand-written JSON. This is how
// every daemon test below reaches a chosen port: store.State.ResolvedPort
// (internal/store/store.go) is unit-tested directly in store_test.go, so
// these tests only need to pin that runDaemonCmd actually USES it.
func writeStateWithPort(t *testing.T, home string, port int) {
	t.Helper()
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		st.Port = port
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDaemonRefusesAPortThatIsTaken pins spec §4.8: a port already in use
// must be a startup FAILURE naming the port, not a search. Binding
// 127.0.0.1:0 is safe — the kernel picks an ephemeral port, never the
// product's real default.
func TestDaemonRefusesAPortThatIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() // held open: the daemon's real Listen must genuinely fail
	taken := ln.Addr().(*net.TCPAddr).Port

	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, taken)

	var stderr bytes.Buffer
	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, &stderr)); code == 0 {
		t.Fatal("runDaemonCmd succeeded on a taken port, want a failure")
	}
	if !strings.Contains(stderr.String(), strconv.Itoa(taken)) {
		t.Errorf("error %q does not name the port %d the operator must free", stderr.String(), taken)
	}
}

// TestRunDaemonCmdListensOnStateJSONsPort pins §4.8's actual product
// behaviour at the one call site Task 1 exists for: the address handed to
// net.Listen is state.json's port, not a hardcoded default. listenTCP
// (proxy.go) is stubbed to capture the address and refuse the bind, so
// nothing is ever bound — under correct code or under a regression to
// store.DefaultPort — which is what makes this a safe proof where
// TestDaemonRefusesAPortThatIsTaken and
// TestRunDaemonCmdWiresDaemonLogAsRunProxyWithSignalsStdout are not: both
// of those use a real, ephemeral port specifically so a regression can
// never fall through to a bind on the product's real default; stubbing
// listenTCP here sidesteps needing any real bind at all, so it is the one
// place a regression TO that default is actually observable rather than
// merely defended against.
//
// The stub returns an error, not a real listener on 127.0.0.1:0: a real
// listener would let runDaemonCmd proceed into runProxyWithSignal's real
// serve loop, which hangs forever waiting on a signal (its sig parameter
// is hardcoded to nil) — an error instead makes runDaemonCmd fail fast
// (exit 1) and return, which is all this test needs.
func TestRunDaemonCmdListensOnStateJSONsPort(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeStateWithPort(t, home, 51234) // never bound: listenTCP is stubbed below

	var got string
	orig := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) {
		got = addr
		return nil, errors.New("test: refusing to bind")
	}
	defer func() { listenTCP = orig }()

	var errb strings.Builder
	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, errb.String())
	}
	if want := "127.0.0.1:51234"; got != want {
		t.Fatalf("runDaemonCmd listened on %q, want %q (state.json's port)", got, want)
	}
}

// TestDaemonRunWritesAPreServeStartupFailureToRealStderr pins the fix for a
// review finding: launchd never reads daemon.log (it only surfaces whatever
// lands on the process's real stderr, via StandardErrorPath), so any startup
// failure that happens before the proxy begins serving must reach the real
// stderr too, not just logw.
func TestDaemonRunWritesAPreServeStartupFailureToRealStderr(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	// A corrupt state.json makes store.Store.Load() fail.
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	if got := runDaemonCmd([]string{"run"}, newReporter(false, &out, &errb)); got != 1 {
		t.Fatalf("exit = %d, want 1 for a store.Load failure", got)
	}
	if !strings.Contains(errb.String(), "corrupt") {
		t.Fatalf("real stderr %q does not show the store.Load startup failure; launchd only surfaces the real stderr, never daemon.log", errb.String())
	}
}

// TestDaemonRunPreServeFailureReachesRealStderrEvenWhenDaemonLogWriteFails
// pins fix round 1, I1, through runDaemonRun's own real logw (a
// *rotate.Writer on a real file), not a stand-in: daemon.log is pre-sized
// to rotate.Config's MaxBytes (8<<20, matching daemonLogKeep's own Open
// call), and EVERY one of its rotation siblings, daemon.log.1 ..
// daemon.log.daemonLogKeep, is made a non-empty directory — rotateLocked
// shifts .1 -> .2 -> .3 ... before its final rename, so blocking .1 alone
// is not enough: the shift itself would just move that blocker out of the
// way first. With every slot blocked, each shift step fails in turn
// (os.Rename onto an existing non-empty directory fails with "file
// exists" on both linux and darwin; verified, not assumed) and .1 is still
// occupied when the final rename (daemon.log -> daemon.log.1) runs, so the
// very next Write forces a rotation that fails deterministically. That is
// a REAL logw.Write failure, the same shape as the finding's "a rotation
// failure, or a full disk" — not a synthetic stand-in — and the pre-serve
// failure below (a corrupt state.json) is runDaemonRun's first write to
// logw, so it is what actually triggers the rotation.
func TestDaemonRunPreServeFailureReachesRealStderrEvenWhenDaemonLogWriteFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(home, "daemon.log")
	if err := os.WriteFile(logPath, make([]byte, 8<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= daemonLogKeep; i++ {
		d := fmt.Sprintf("%s.%d", logPath, i)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "inner"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var errb bytes.Buffer
	if code := runDaemonCmd([]string{"run"}, newReporter(false, io.Discard, &errb)); code != 1 {
		t.Fatalf("exit = %d, want 1 for a store.Load failure", code)
	}
	if !strings.Contains(errb.String(), "corrupt") {
		t.Fatalf("real stderr %q does not carry the startup failure even though daemon.log's own write failed (rotation blocked by same-named directories): startupWriter must reach every destination regardless of an earlier one's error", errb.String())
	}
}

func TestDaemonRejectsAMissingSubcommand(t *testing.T) {
	var out, errb strings.Builder
	if got := runDaemonCmd(nil, newReporter(false, &out, &errb)); got != 2 {
		t.Fatalf("exit = %d, want 2 for a missing subcommand", got)
	}
	if !strings.Contains(errb.String(), "daemon run") {
		t.Fatalf("stderr %q does not show the usage line", errb.String())
	}
}

// TestDaemonRunRejectsAStrayPositionalArgument pins the same off-switch
// hazard as TestDaemonSubcommandArgsRejectsAStrayPositionalArgument, but
// through runDaemonCmd's own top-level FlagSet — the one that actually
// reports to real stderr with exit code 2. It also pins that an ordinary
// stray argument stays legible: only a value that can actually carry a
// credential (see TestDaemonRunRedactsACredentialShapedStrayArgument
// below) is redacted, so an ordinary typo like "somearg" still names
// itself in the message rather than becoming an opaque "<redacted>" for
// every input (review finding, M1c4 Task 6 fix round 2 — an earlier,
// broader redaction did exactly that).
func TestDaemonRunRejectsAStrayPositionalArgument(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	var out, errb strings.Builder
	if got := runDaemonCmd([]string{"run", "somearg", "--log", ""}, newReporter(false, &out, &errb)); got != 2 {
		t.Fatalf("exit = %d, want 2 for a stray positional argument", got)
	}
	if !strings.Contains(errb.String(), "somearg") {
		t.Fatalf("stderr %q does not name the unexpected argument", errb.String())
	}
}

// TestDaemonRunRedactsACredentialShapedStrayArgument pins the fix for a
// review finding (M1c4 Task 6 fix round 1): daemon run used to reject
// --upstream-proxy outright, so nobody had reason to type a proxy URL at
// it. Now that the flag exists, "user:pass@host:port" — no scheme, exactly
// what http_proxy and `curl -x` accept — is the natural typo for someone
// who forgot the flag name, and it used to be echoed verbatim into the
// unexpected-argument message, which reaches only real stderr — a
// retained file under launchd. This project's rule against logging or
// printing credentials is absolute, so a stray value that can carry
// userinfo (this one contains "@") is redacted before it is echoed, while
// TestDaemonRunRejectsAStrayPositionalArgument above pins that ordinary
// values are not swept up by the same check.
func TestDaemonRunRedactsACredentialShapedStrayArgument(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	var out, errb strings.Builder
	if got := runDaemonCmd([]string{"run", "bob:hunter2@proxy:8080"}, newReporter(false, &out, &errb)); got != 2 {
		t.Fatalf("exit = %d, want 2 for a stray positional argument", got)
	}
	if strings.Contains(errb.String(), "hunter2") {
		t.Fatalf("stderr leaked a credential-shaped stray argument verbatim: %q", errb.String())
	}
}

// TestDaemonRunPassesLogThrough pins that a daemon user can redirect or
// disable the trace log. Before this, runDaemonCmd never passed --log at
// all, so proxy.jsonl was written to the default path with no way to
// change it (F68).
//
// The "unset" row pins a review finding (M1c4 Task 3 round 1): an earlier
// version prepended defaultLog ahead of the caller's own args and relied on
// flag's last-wins parsing to let an explicit --log override it. That broke
// silently in both directions depending on which side of the caller's args
// the prepend landed on — one ordering silently disabled logging for every
// daemon user, the other silently defeated the --log "" privacy off-switch
// — with the rest of the suite green throughout. Letting flag itself
// resolve the --log default (this signature's defaultLog parameter) makes
// "unset" and "explicit, empty --log" two different, individually pinned
// outcomes instead of a fragile ordering.
func TestDaemonRunPassesLogThrough(t *testing.T) {
	const defaultLog = "/tmp/default.jsonl"
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unset", []string{"run"}, defaultLog},
		{"explicit path", []string{"run", "--log", "/tmp/x.jsonl"}, "/tmp/x.jsonl"},
		{"disabled", []string{"run", "--log", ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := daemonSubcommandArgs(tc.args[1:], "127.0.0.1:0", defaultLog, "")
			if !ok {
				t.Fatal("daemonSubcommandArgs rejected valid flags")
			}
			if v, present := flagValue(got, "--log"); !present || v != tc.want {
				t.Fatalf("subcommand args %v carry --log=%q (present=%v), want %q", got, v, present, tc.want)
			}
		})
	}
}

// TestDaemonSubcommandArgsRejectsAStrayPositionalArgument pins that a
// misplaced argument does not silently defeat the --log off-switch. Without
// this check, flag.Parse stops at the first non-flag token rather than
// erroring, so `daemon run somearg --log ""` would leave --log unparsed and
// the default would win instead of the caller's explicit "" (review
// finding, M1c4 Task 3 round 1).
func TestDaemonSubcommandArgsRejectsAStrayPositionalArgument(t *testing.T) {
	_, ok := daemonSubcommandArgs([]string{"somearg", "--log", ""}, "127.0.0.1:0", "/tmp/default.jsonl", "")
	if ok {
		t.Fatal("daemonSubcommandArgs accepted a stray positional argument; a --log after it would be silently dropped")
	}
}

// TestDaemonRunForwardsUpstreamProxy pins that a user behind a corporate
// proxy can run the supervised daemon. proxy run has had this flag since
// M1c2; daemon run did not, so the daemon was unusable there.
func TestDaemonRunForwardsUpstreamProxy(t *testing.T) {
	got, ok := daemonSubcommandArgs([]string{"--upstream-proxy", "http://127.0.0.1:3128"}, "127.0.0.1:0", "", "")
	if !ok {
		t.Fatal("daemonSubcommandArgs rejected --upstream-proxy")
	}
	v, present := flagValue(got, "--upstream-proxy")
	if !present || v != "http://127.0.0.1:3128" {
		t.Fatalf("subcommand args %v do not carry --upstream-proxy, got %q present=%v", got, v, present)
	}
}

// TestDaemonSubcommandArgsFallsBackToDefaultUpstreamWhenUnset pins fix round
// 3's D2 at the level daemonSubcommandArgs can be tested directly: with no
// --upstream-proxy flag, the subcommand's argv carries defaultUpstream
// (runDaemonCmd's name for CHOTTAG_UPSTREAM_PROXY, spawnDaemon's env handoff
// for the shell's pre-existing HTTPS_PROXY — internal/shim/shim.go), exactly
// the way an unset --log falls back to defaultLog.
func TestDaemonSubcommandArgsFallsBackToDefaultUpstreamWhenUnset(t *testing.T) {
	got, ok := daemonSubcommandArgs(nil, "127.0.0.1:0", "", "http://env-proxy:8080")
	if !ok {
		t.Fatal("daemonSubcommandArgs rejected an empty flag set")
	}
	if v, present := flagValue(got, "--upstream-proxy"); !present || v != "http://env-proxy:8080" {
		t.Fatalf("subcommand args %v carry --upstream-proxy=%q (present=%v), want the default %q", got, v, present, "http://env-proxy:8080")
	}
}

// TestDaemonSubcommandArgsExplicitUpstreamProxyWinsOverDefault pins the other
// half: an explicit --upstream-proxy must override defaultUpstream, not be
// overridden by it — the documented flag is the one interface, the env
// handoff only fills in when the flag is absent.
func TestDaemonSubcommandArgsExplicitUpstreamProxyWinsOverDefault(t *testing.T) {
	got, ok := daemonSubcommandArgs([]string{"--upstream-proxy", "http://flag-proxy:9090"}, "127.0.0.1:0", "", "http://env-proxy:8080")
	if !ok {
		t.Fatal("daemonSubcommandArgs rejected --upstream-proxy")
	}
	if v, present := flagValue(got, "--upstream-proxy"); !present || v != "http://flag-proxy:9090" {
		t.Fatalf("subcommand args %v carry --upstream-proxy=%q (present=%v), want the explicit flag %q to win", got, v, present, "http://flag-proxy:9090")
	}
}

// listenTCPTripwire stubs listenTCP (proxy.go) to unconditionally refuse to
// bind, restoring the original on cleanup. It does not record whether or
// how it was called — callers that need that use listenTCP's own capturing
// stub pattern directly (TestRunDaemonCmdListensOnStateJSONsPort). Same
// return makes runDaemonCmd fail fast rather than fall into
// runProxyWithSignal's real serve loop (which hangs forever on a nil sig
// channel), so this is the one place a regression that lets validation
// fall through is actually observable rather than merely defended against
// with an ephemeral port — no real bind happens under ANY mutation of the
// validation logic upstream of it.
func listenTCPTripwire(t *testing.T) {
	t.Helper()
	orig := listenTCP
	listenTCP = func(network, addr string) (net.Listener, error) {
		return nil, errors.New("test: refusing to bind")
	}
	t.Cleanup(func() { listenTCP = orig })
}

// TestDaemonRunHonoursTheUpstreamProxyEnvFallback pins fix round 3's D2 at
// runDaemonCmd itself: with CHOTTAG_UPSTREAM_PROXY set and no --upstream-proxy
// flag, the env value is what parseUpstreamProxy's validation actually sees
// — proved by giving it a malformed value and observing the validation
// reject it, while never leaking the credential it carries.
//
// listenTCPTripwire is the safety net, not a defence-in-depth extra: an
// earlier version of this test relied entirely on parseUpstreamProxy
// rejecting the malformed value to stop execution before any real
// net.Listen. Proving the property's own mutation (an empty defaultUpstream,
// which passes parseUpstreamProxy("") validation at every layer that reads
// it — runDaemonCmd's own check AND, because defaultUpstream also feeds
// daemonSubcommandArgs, runProxyWithSignal's redundant one) showed why that
// is not enough: control fell all the way through to the real net.Listen
// with nothing left to stop it, binding the real default port during `go
// test`. The stub makes that path fail fast and observably instead,
// regardless of which validation layer the mutation defeats.
func TestDaemonRunHonoursTheUpstreamProxyEnvFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv(shim.UpstreamProxyEnvVar, "http://user:secret@nowhere:1/%zz")
	listenTCPTripwire(t)

	var out, errb strings.Builder
	code := runDaemonCmd([]string{"run"}, newReporter(false, &out, &errb))
	if code == 0 {
		t.Fatal("a malformed CHOTTAG_UPSTREAM_PROXY must not start the daemon")
	}
	if !strings.Contains(errb.String(), "upstream-proxy") {
		t.Fatalf("stderr %q does not show the env fallback reached validation", errb.String())
	}
	if strings.Contains(errb.String(), "secret") {
		t.Fatalf("stderr leaked the env upstream proxy's password: %q", errb.String())
	}
}

// TestDaemonRunExplicitUpstreamFlagOverridesTheEnvFallback pins that an
// explicit --upstream-proxy is validated instead of CHOTTAG_UPSTREAM_PROXY
// when both are present: the env value is given a shape (bad URL escape)
// that produces a different rejection reason than the flag's (a rejected
// scheme), so which one actually got validated is observable.
//
// listenTCPTripwire is the safety net for the same reason as
// TestDaemonRunHonoursTheUpstreamProxyEnvFallback just above.
func TestDaemonRunExplicitUpstreamFlagOverridesTheEnvFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv(shim.UpstreamProxyEnvVar, "http://user:secret@nowhere:1/%zz")
	listenTCPTripwire(t)

	var out, errb strings.Builder
	code := runDaemonCmd([]string{"run", "--upstream-proxy", "ftp://badscheme:1234"}, newReporter(false, &out, &errb))
	if code == 0 {
		t.Fatal("a malformed --upstream-proxy must not start the daemon")
	}
	if !strings.Contains(errb.String(), "only http") {
		t.Fatalf("stderr %q does not show the EXPLICIT flag's own rejection reason: the env fallback must not have been validated instead", errb.String())
	}
	if strings.Contains(errb.String(), "secret") {
		t.Fatalf("stderr leaked the env upstream proxy's password: %q", errb.String())
	}
}

// TestDaemonRunRejectsLogPointedAtDaemonLogItself pins the collision `daemon
// run --log` can now reach: daemon.log itself is already a rotate.Writer
// (logw) before this check runs, and --log pointing the trace log at the
// same path would open a SECOND, independent rotate.Writer on it — each
// with its own size counter and file descriptor, since internal/rotate has
// no cross-instance coordination. Two of those racing rotation on the same
// file corrupt both the first time their rotations interleave. Newly
// reachable only since M1c4 Task 6a gave `daemon run` its own --log; `proxy
// run`'s --log has always defaulted to proxy.jsonl, never daemon.log, so
// this collision did not exist before.
//
// Both real streams are asserted positively, and that they carry the SAME
// line, for the same reason as the sibling upstream-proxy test just below:
// an assertion that only checks code != 0 and stderr's content would stay
// green even if the message reached only one of the two sinks, or neither.
//
// state.json is written with a genuinely free ephemeral port (bound, read,
// released) rather than left unset: if the collision check under test ever
// regressed, the pipeline would otherwise fall through to store.Load and
// runProxyWithSignal (and, inside it, the real net.Listen), and an
// ephemeral port keeps that fallthrough away from the product's real
// default port (§4.8's "never bind 47821" rule) — even though the check
// working correctly never lets control reach that far.
func TestDaemonRunRejectsLogPointedAtDaemonLogItself(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	daemonLogPath := filepath.Join(home, "daemon.log")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // released: only a genuinely free port NUMBER is needed
	writeStateWithPort(t, home, port)

	var out, errb strings.Builder
	code := runDaemonCmd([]string{"run", "--log", daemonLogPath}, newReporter(false, &out, &errb))
	if code == 0 {
		t.Fatal("--log pointed at daemon.log itself must not start the daemon")
	}
	if !strings.Contains(errb.String(), "daemon.log itself") {
		t.Fatalf("stderr = %q, want it to name the collision", errb.String())
	}

	logBytes, err := os.ReadFile(daemonLogPath)
	if err != nil {
		t.Fatalf("daemon.log was not written: %v", err)
	}
	if got, want := strings.TrimRight(string(logBytes), "\n"), strings.TrimRight(errb.String(), "\n"); got != want {
		t.Fatalf("daemon.log %q does not carry the same rejection line stderr got %q", got, want)
	}
}

// TestDaemonRunRejectsABadUpstreamProxyWithoutLeakingCredentials pins that a
// bad value is reported WITHOUT its credentials, to BOTH real stderr and
// daemon.log. redact.UpstreamProxy exists for exactly this and the daemon's
// stderr is now a retained file.
//
// The original version of this test (review, M1c4 Task 6 fix round 1) only
// asserted code != 0 and the absence of "secret" — both are satisfied by
// EMPTY stderr, so deleting either fmt.Fprintln(logw, msg) or
// fmt.Fprintln(stderr, msg) at the call site left the whole suite green.
// That is precisely the failure mode this project already has a finding
// about: exit 1 with both real streams empty while launchd restart-loops
// and the documented diagnostic file (daemon.log) is blank. Asserting
// positively on both sinks, and that they carry the SAME line, closes that.
func TestDaemonRunRejectsABadUpstreamProxyWithoutLeakingCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	var out, errb strings.Builder
	code := runDaemonCmd([]string{"run", "--upstream-proxy", "http://user:secret@nowhere:1/%zz"}, newReporter(false, &out, &errb))
	if code == 0 {
		t.Fatal("a malformed --upstream-proxy must not start the daemon")
	}
	if strings.Contains(errb.String(), "secret") {
		t.Fatalf("stderr leaked the upstream proxy password: %q", errb.String())
	}
	if !strings.Contains(errb.String(), "upstream-proxy") {
		t.Fatalf("stderr does not report the rejected flag: %q", errb.String())
	}
	logBytes, err := os.ReadFile(filepath.Join(home, "daemon.log"))
	if err != nil {
		t.Fatalf("daemon.log was not written: %v", err)
	}
	if strings.Contains(string(logBytes), "secret") {
		t.Fatalf("daemon.log leaked the upstream proxy password: %q", logBytes)
	}
	if got, want := strings.TrimRight(string(logBytes), "\n"), strings.TrimRight(errb.String(), "\n"); got != want {
		t.Fatalf("daemon.log %q does not carry the same rejection line stderr got %q", got, want)
	}
}

// TestRunDaemonCmdReachesTheComputedDefaultLogPath closes the last silent-F68
// hole Task 3's review left open (measured twice): mutating the
// daemonSubcommandArgs call inside runDaemonCmd to a wrong or empty default
// log path stayed green under the whole suite, because nothing exercised
// runDaemonCmd's own happy path — every other test in this file stops
// earlier, at a FlagSet or guard rejection.
//
// With the port probe removed, this is what used to be reachable only via
// the deleted
// runDaemonCmdWithProbe. It runs runDaemonCmd's real 3-argument entry point
// all the way to opening the trace log — proxy.go's openTraceLog does that
// before owners.Open — then owners.Open is forced to fail deterministically
// (owners.json's path is blocked by a directory) so the pipeline stops
// there, before ever reaching listenTCP. It must stop there: runDaemonCmd
// hardcodes runProxyWithSignal's sig parameter to nil, so if this ever
// reached the real serve loop it would block on a real OS signal forever
// instead of returning.
//
// What actually pins the pipeline reaching this far is owners.Open failing
// (not the ephemeral port): openTraceLog runs unconditionally, before
// owners.Open, regardless of what port is configured. The ephemeral port
// written to state.json is defence-in-depth only, the same as the sibling
// comment at TestDaemonRunRejectsLogPointedAtDaemonLogItself's own case —
// measured by removing the writeStateWithPort call and stubbing listenTCP
// with a t.Fatal tripwire: the test still passes and the tripwire never
// fires. What this test DOES cover, and is the only test that does, is the
// daemonSubcommandArgs call's defaultLog argument. It does NOT cover that
// runDaemonCmd's listen call site actually uses st.ResolvedPort() — that is
// TestRunDaemonCmdListensOnStateJSONsPort's job, above.
func TestRunDaemonCmdReachesTheComputedDefaultLogPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // released: only a genuinely free port NUMBER is needed
	writeStateWithPort(t, home, port)

	if err := os.MkdirAll(filepath.Join(home, "owners.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	got := runDaemonCmd([]string{"run"}, newReporter(false, &out, &errb))
	if got != 1 {
		t.Fatalf("exit = %d, want 1 (owners.Open must fail on the directory blocking owners.json); stderr=%q", got, errb.String())
	}
	wantLog := filepath.Join(home, "proxy.jsonl")
	if _, err := os.Stat(wantLog); err != nil {
		t.Fatalf("trace log was never opened at the computed default %s: %v — this is exactly what a wrong or empty default at the daemonSubcommandArgs call site would also produce", wantLog, err)
	}
}

// TestRunDaemonCmdWiresDaemonLogAsRunProxyWithSignalsStdout pins the
// wiring runDaemonCmd's own doc comment spends a paragraph on: "logw fills
// BOTH the stdout and stderr slots" — runProxyWithSignal(sub, logw, logw,
// stderr, nil), not runDaemonCmd's own stdout parameter (which is deliberately
// never passed through, since the packaging README leaves launchd's
// StandardOutPath unset). Changing that call to the process's real stdout
// instead of logw left the whole package green before this test existed:
// nothing else in the suite lets runDaemonCmd's happy path reach far
// enough to actually print anything on that slot — every other test in
// this file stops the pipeline early (owners.json blocked by a directory)
// specifically so it never reaches the real signal-driven serve loop.
//
// This one lets it run all the way to listening for real, then reads
// daemon.log for the export lines runDaemon writes to d.Stdout
// (internal/cli/proxy.go's `fmt.Fprint(d.Stdout, envLines(...))`) — they
// can only land in daemon.log if logw is what got passed as stdout. A real
// SIGINT stops it cleanly afterwards, the same technique
// TestInstallShutdownRegistersARealSignalHandler uses, and for the same
// reason: runDaemonCmd hardcodes runProxyWithSignal's sig parameter to
// nil, so nothing else can stop this daemon.
//
// The listen port is a genuinely free ephemeral one (bind, read, release,
// same technique as TestRunDaemonCmdReachesTheComputedDefaultLogPath),
// never the fixed default: reaching the real listener at all is exactly
// what this test needs and every sibling test in this file avoids.
func TestRunDaemonCmdWiresDaemonLogAsRunProxyWithSignalsStdout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // released: the point is a real, currently-free port number
	writeStateWithPort(t, home, port)

	// F4b-shape guard: if the mutation this test exists to catch (or any
	// other) somehow let a real SIGINT fall through to its default
	// disposition, the whole test binary would die instead of this test
	// failing cleanly. Registering our own channel alongside
	// installShutdown's real signal.Notify does not change the good path —
	// os/signal fans a signal out to every registered channel.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGINT)
	t.Cleanup(func() { signal.Stop(guard) })

	logPath := filepath.Join(home, "daemon.log")
	// out/errb are written by runDaemonCmd's goroutine below and read from
	// the test goroutine (the WaitGroup-free polling loop and the final
	// assertions); a strings.Builder is not safe for that, so this uses
	// newSyncBuf (proxy_helpers_test.go), the same synchronized writer
	// every sibling test in this package already uses for a
	// goroutine-crossing buffer.
	out := newSyncBuf()
	errb := newSyncBuf()
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runDaemonCmd([]string{"run"}, newReporter(false, out, errb))
	}()

	// Stop the real daemon on any exit from this test, including a Fatalf
	// above it: without this, a failing assertion below would leave a live
	// daemon goroutine listening on a real port, with a real signal.Notify
	// still armed, running for the rest of this test binary's life (N3's
	// shape — see TestProxyRunPrintsExportLinesBeforeServing's own comment).
	var stopped bool
	t.Cleanup(func() {
		if stopped {
			return
		}
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	// The "listening" line (d.Stderr) and the export lines (d.Stdout) are
	// two separate Fprint calls onto the same logw, in that order (spec
	// §5.1, runDaemon): reading daemon.log the instant the first shows up
	// can still catch it between the two (review round 1 item 6, a flaky
	// pre-existing race in the test, not the production ordering). Wait
	// for every marker together, not just the first one.
	// The secret is read from its file at eval time, never printed, so the
	// export line is only pinned by its prefix (up to the $(cat "...")
	// substitution) and its suffix (the resolved listen address) — never
	// the secret itself.
	wantHTTPSPrefix := "export HTTPS_PROXY=\"http://chottag:$(cat \""
	wantHTTPSSuffix := "@127.0.0.1:" + strconv.Itoa(port) + "\""
	wantCA := "export NODE_EXTRA_CA_CERTS=\"" + filepath.Join(home, "ca", "ca.pem") + "\""
	const marker = "chottag proxy listening on"
	deadline := time.Now().Add(10 * time.Second)
	var logContent string
	for {
		b, _ := os.ReadFile(logPath)
		logContent = string(b)
		if strings.Contains(logContent, marker) && strings.Contains(logContent, wantHTTPSPrefix) && strings.Contains(logContent, wantHTTPSSuffix) && strings.Contains(logContent, wantCA) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for daemon.log to report listening and the export lines; daemon.log so far = %q, stderr = %q", logContent, errb.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The process's real stdout parameter runDaemonCmd was called with must
	// stay untouched: the whole point of wiring logw into both slots is
	// that nothing routine ever reaches the process's real stdout under
	// launchd (StandardOutPath is deliberately unset — see the packaging
	// README).
	if out.String() != "" {
		t.Fatalf("runDaemonCmd's own stdout parameter = %q, want empty — it must never be passed through to runProxyWithSignal", out.String())
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-codeCh:
		stopped = true
		if code != 0 {
			t.Fatalf("runDaemonCmd returned %d after a clean SIGINT shutdown, want 0; stderr = %q", code, errb.String())
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemonCmd did not return after a real SIGINT")
	}
}

// TestDaemonRunRejectsALogOnARotationSibling pins F90: rotate.rotateLocked
// (internal/rotate/rotate.go) renames onto daemon.log.1 .. daemon.log.5 on
// every one of daemon.log's rolls, so a --log parked on any of those paths
// would be silently destroyed by the next rotation. Driven through
// runDaemonCmd itself (not the daemonLogSibling helper directly): this is
// the rejection path, so control never reaches store.Load or
// runProxyWithSignal (and, inside it, the real net.Listen) — the same reason
// TestDaemonRunRejectsLogPointedAtDaemonLogItself above calls runDaemonCmd
// directly for the exact-path case.
func TestDaemonRunRejectsALogOnARotationSibling(t *testing.T) {
	for i := 1; i <= daemonLogKeep; i++ {
		name := fmt.Sprintf("daemon.log.%d", i)
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			// home() is $CHOTTAG_HOME or ~/.chottag (cli.go:82-84); without
			// this the guard compares against the developer's REAL home and
			// the rejection never fires.
			t.Setenv("CHOTTAG_HOME", home)
			var out, errb strings.Builder
			code := runDaemonCmd([]string{"run", "--log", filepath.Join(home, name)}, newReporter(false, &out, &errb))
			// 1, not 2: this joins the existing daemon.log guard, which
			// returns 1. 2 is runDaemonCmd's usage/bad-argument code.
			if code != 1 {
				t.Fatalf("exit = %d for --log %s, want 1: rotateLocked renames over this path on every roll", code, name)
			}
		})
	}
}

// TestDaemonLogSiblingAcceptsANonSibling pins that the guard does not
// over-reach: a path that merely shares the "daemon.log" prefix, or that is
// past daemonLogKeep, is not a rotation sibling and must be accepted.
//
// Driven through the extracted daemonLogSibling helper, NOT through
// runDaemonCmd: past the guard, runDaemonCmd continues to store.Load and
// runProxyWithSignal (and, inside it, the real net.Listen — see
// runDaemonCmd's own doc comment), so "this path is accepted" cannot be
// asserted by calling the command — it would start a real daemon. Same
// reason daemonSubcommandArgs in this file
// is self-contained.
func TestDaemonLogSiblingAcceptsANonSibling(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{
		"daemon.log.keep", // non-numeric suffix
		fmt.Sprintf("daemon.log.%d", daemonLogKeep+1), // one past the retained range
		"mydaemon.log.1", // shares only a trailing substring, not the prefix
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(home, name)
			sibling, err := daemonLogSibling(path, home)
			if err != nil {
				t.Fatal(err)
			}
			if sibling {
				t.Fatalf("daemonLogSibling(%q) = true, want false: it is not one of daemon.log's rotation siblings", name)
			}
		})
	}
}

// flagValue returns the value following name in args.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
