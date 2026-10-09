package cli

// The five §6.1 invariants, driven end to end through runSetup -> shim.Run
// -> runUninstall (or, for invariant 4, through a real proxy request) with
// shim's execFn/spawnFn stubbed so nothing is ever really exec'd or spawned.
//
// TestMain below is what makes reaching shim.Run here safe at all: it
// installs a panicking execFn/spawnFn pair via shim.SetSeamsForTest before
// any test in this package runs, so a test that reaches shim.Run without
// stubbing the seams itself fails loudly instead of really syscall.Exec'ing
// this machine's claude or forking a detached daemon (see cli.go's Run doc
// comment, and internal/shim's own identical TestMain — identical since fix
// round 3, D7/F-H gave shim's own TestMain the matching execFn default;
// before that it guarded only spawnFn, and "identical" was false as
// written).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/notify"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

// TestMain closes the hazard cli.go's Run doc comment warns about: without
// it, a test in this package that reaches shim.Run (an argv0 whose base is
// `claude`, or a direct shim.Run call as the invariant tests below make)
// would run with shim's PRODUCTION execFn/spawnFn — really replacing this
// test process's image with the machine's real claude, or forking a real
// detached daemon, during `go test`. internal/shim guards its OWN test
// binary with an identical TestMain; that lives in shim's test binary and
// does nothing for this package (F103) — hence shim.SetSeamsForTest, the
// exported, non-test seam-setter this TestMain installs a failing default
// through. A test that legitimately drives shim.Run (every invariant test
// below except invariant 1 and 5's rc/tree assertions) opts back in by
// calling shim.SetSeamsForTest itself and restoring on cleanup, exactly the
// way internal/shim's own tests opt in with swapExec/swapSpawn.
var testClaudeConfigDir string

func TestMain(m *testing.M) {
	shim.SetSeamsForTest(
		func(bin string, args, env []string) error {
			panic("shim.execFn reached from internal/cli's test binary: stub it with shim.SetSeamsForTest before driving shim.Run or cli.Run with a `claude` argv0")
		},
		func(exe, home, upstream string) error {
			panic("shim.spawnFn reached from internal/cli's test binary: stub it with shim.SetSeamsForTest before driving shim.Run or cli.Run with a `claude` argv0")
		},
	)
	// claudeAuthExec (login.go) is the one exec in this tree that inherits
	// real stdio: the real runClaudeAuth would hand this test runner's
	// terminal to a `claude auth login` browser prompt and block. A test
	// that forgets to stub it via SetAuthExecForTest must fail loudly
	// instead of hanging (login.go's own doc comment, and F116's lesson
	// that an unstubbed irreversible seam is a landmine that only goes off
	// under timing).
	SetAuthExecForTest(func(bin, slotDir, sub string, _ io.Reader, _, _ io.Writer) error {
		panic("cli.claudeAuthExec reached from internal/cli's test binary: a test " +
			"must stub it with SetAuthExecForTest. The real one gives a child " +
			"process this test runner's terminal and blocks on a browser login.")
	})
	// credsDelete (logout.go) is the analogous seam for the Keychain/
	// credential-file delete: a test that forgets to stub it via
	// SetCredsDeleteForTest must fail loudly instead of deleting from the
	// user's real Keychain on darwin (F130).
	SetCredsDeleteForTest(func(slotDir string) error {
		panic("cli.credsDelete reached from internal/cli's test binary: a test " +
			"must stub it with SetCredsDeleteForTest. The real one deletes from " +
			"the user's Keychain on darwin.")
	})
	// sessions and resume read a transcript's title (R171) from Claude's
	// config dir; default it to an empty directory so no test reads the
	// real ~/.claude. A test that wants transcripts sets its own.
	if d, err := os.MkdirTemp("", "chottag-test-claude-config"); err == nil {
		os.Setenv("CLAUDE_CONFIG_DIR", d)
		testClaudeConfigDir = d
	}
	// journalAlive (journal_tick.go) probes real pids; a test that reaches
	// it must stub it.
	journalAlive = func(int) bool { panic("cli.journalAlive reached: stub it in the test") }
	// journalBootTime (boottime.go) reads the machine's real boot time. Every
	// daemon test reaches it through the journal pass, so the default is the
	// deterministic "unknown" (zero), not a panic; a test of the boot rule
	// stubs it.
	journalBootTime = func() time.Time { return time.Time{} }
	// cmuxRun and cmuxFind (resume.go) run and locate the real cmux.
	cmuxRun = func(string, []string, []string) ([]byte, error) { panic("cli.cmuxRun reached: stub it in the test") }
	cmuxFind = func() string { panic("cli.cmuxFind reached: stub it in the test") }
	// daemonChdir (daemon.go) is `daemon run`'s chdir to its home (L5). A
	// safe no-op, not a panic: many daemon tests reach it, and a real chdir
	// would move this whole test binary's cwd. TestDaemonRunChdirsToItsHome
	// stubs it to observe the call.
	SetDaemonChdirForTest(func(string) error { return nil })
	// signalFn (daemon_common.go) is `daemon stop`/`restart`'s kill(2). A
	// test that forgets SetSignalForTest must fail loudly rather than
	// signal a real pid read from a lock record (F116, F130). A test may
	// only ever let a real signal reach a helper process it started itself.
	SetSignalForTest(func(pid int, sig syscall.Signal) error {
		panic("cli.signalFn reached from internal/cli's test binary: a test must stub it with SetSignalForTest, and may only ever signal a helper process it started itself")
	})
	// daemonlock.Inspect rejects a held lock's record whose pid is not alive
	// (T1 fix round 1, I2c). This package's tests hold the lock in-process
	// with fabricated records (fakeRecord(4242)) and stub every signal, so
	// treat every pid as alive here; the liveness filter itself is tested
	// in internal/daemonlock.
	daemonlock.SetPIDAliveForTest(func(int) bool { return true })
	// isInteractive (interactive.go) decides whether logout and uninstall
	// --purge may prompt. `go test`'s own stdin is sometimes a terminal, so
	// the default is NOT interactive: a prompt test opts in with
	// interactiveStdin(t) (M1d-d).
	SetInteractiveForTest(func(io.Reader) bool { return false })
	// doctorNet (doctor.go) is doctor's health probe and free-port bind.
	// The real ones reach 127.0.0.1:<state port>, which is 47821 on a home
	// without one. A doctor test stubs both with SetDoctorNetForTest. One
	// that forgets gets an internal error naming the seam (doctor.Run
	// recovers a check's panic, M2 spec §2.3), and nothing is probed or
	// bound.
	SetDoctorNetForTest(
		func(int) bool {
			panic("doctor's health probe reached from internal/cli's test binary: stub it with SetDoctorNetForTest")
		},
		func(int) (bool, string) {
			panic("doctor's health-version probe reached from internal/cli's test binary: stub it with SetDoctorNetForTest")
		},
		func(string) (io.Closer, error) {
			panic("doctor's port probe reached from internal/cli's test binary: stub it with SetDoctorNetForTest")
		},
	)
	// doctorTempDirs (doctor.go, I3) is what newDoctorEnv wires into
	// doctor.Env.TempDirs — the bin check's refusal to link at a binary
	// living under one of them, or under a go-build temp dir. This test
	// binary IS itself a `go test` temp binary (go-build.../pkgname.test,
	// under os.TempDir()), so the real value would trip that refusal on
	// every test in this package that breaks bin/ and expects doctor --fix
	// to repair it. nil fully disables it, go-build check included. I3's
	// own behaviour is covered against the real value in internal/doctor's
	// own tests.
	SetDoctorTempDirsForTest(nil)
	// doctorClaudeVersion (doctor.go) runs `claude --version` on the
	// resolved real claude (M2c row 15). A doctor test stubs it (every
	// doctorInstall does, through installDoctorNet). One that forgets gets
	// an internal error naming the seam, and the real claude never runs.
	SetDoctorClaudeVersionForTest(func(string) (string, error) {
		panic("doctor's claude --version reached from internal/cli's test binary: stub it with SetDoctorClaudeVersionForTest")
	})
	// statusProbe (status.go) is `status`'s daemon health probe (public
	// release design §2.4). Unlike doctorNet's probe above, this default is
	// a safe "no daemon" stub rather than a panic: most existing status
	// tests build a status.File directly and run through runStatus/runHome
	// without ever touching a daemon, and status --json's own contract
	// (spec §5.1) promises those stay byte-identical. A test that needs a
	// daemon stubs this itself with SetStatusProbeForTest.
	SetStatusProbeForTest(func(int) (bool, string) { return false, "" })
	// doctorIdentity (doctor.go) and statusIdentity (status.go) are
	// doctor's and status's shared health-challenge identity (F221, Ruling
	// 24): read-only, like statusProbe above, and most existing doctor and
	// status tests never stub either. Both get the same safe "none"
	// default a real probe would report against a home with no daemon,
	// rather than a panic — a live daemon on a developer's machine must
	// never leak into a test that forgot to stub this.
	SetDoctorIdentityForTest(func(string, int) string { return "none" })
	SetStatusIdentityForTest(func(string, int) string { return "none" })
	// parentPID and statuslineProbe (statusline.go) are `statusline`'s ps(1)
	// walk and daemon probe. A test that forgets to stub one must fail loudly
	// rather than run ps or reach a real port.
	SetParentPIDForTest(func(int) (int, error) {
		panic("parentPID reached from internal/cli's test binary: stub it with SetParentPIDForTest")
	})
	SetStatuslineProbeForTest(func(int) bool {
		panic("statuslineProbe reached from internal/cli's test binary: stub it with SetStatuslineProbeForTest")
	})
	// cmuxSetStatus (statusline.go) runs cmux(1); a test that sets
	// CMUX_WORKSPACE_ID must stub it with SetCmuxSetStatusForTest.
	SetCmuxSetStatusForTest(func(context.Context, string, string, string) error {
		panic("cmuxSetStatus reached from internal/cli's test binary: stub it with SetCmuxSetStatusForTest")
	})
	// newDaemonPoller (proxy.go) builds a poller aimed at the real
	// api.anthropic.com, whose first poll reads a slot token through the
	// real tokens.Manager (the Keychain on darwin). A test that runs
	// runProxyWithSignal with a registered account would reach both. The
	// package default is no poller; a test that wants one overrides this
	// itself and restores it (M1c6b).
	newDaemonPoller = func(io.Writer, usagepoll.TokenSource, *statusSink, *url.URL) *usagepoll.Poller { return nil }
	// notify's command runner is the daemon's osascript exec (M2b). A test
	// that makes a real daemon post a notice without stubDaemonNotifier(t)
	// must fail loudly rather than put a notification on this Mac. The
	// panic lands in the dispatcher's goroutine and takes this test binary
	// down with a message naming the fix.
	notify.SetRunForTest(func(context.Context, string, []string) error {
		panic("osascript reached from internal/cli's test binary: call stubDaemonNotifier(t) in the test that runs the daemon")
	})
	// spreadWrite (spread.go) writes run/placements.json. Only a path under
	// the OS temp dir (a test's t.TempDir) may be written: a placement
	// engine pointed at a real home fails loudly instead of touching it.
	spreadWrite = func(path string, data []byte, perm os.FileMode) error {
		if !strings.HasPrefix(path, os.TempDir()) {
			panic("spreadWrite reached " + path + " from internal/cli's test binary: placements.json may only be written under t.TempDir()")
		}
		return fsutil.WriteFileAtomic(path, data, perm)
	}
	// updateGH, updateChild and updateProbe (update.go) are `chottag
	// update`'s three seams: the real ones would run gh, exec a downloaded
	// or newly-placed chottag binary, and probe a real daemon's health
	// port. A test that forgets to stub one of them must fail loudly
	// instead of doing any of that for real (F116/F130); an update test
	// swaps its own fakes in and restores these defaults through
	// t.Cleanup.
	updateGH = func(context.Context, ...string) ([]byte, error) {
		panic("updateGH reached from internal/cli's test binary: an update test must stub it directly and restore it with t.Cleanup")
	}
	updateChild = func(context.Context, string, ...string) error {
		panic("updateChild reached from internal/cli's test binary: an update test must stub it directly and restore it with t.Cleanup")
	}
	updateProbe = func(int) (bool, string) {
		panic("updateProbe reached from internal/cli's test binary: an update test must stub it directly and restore it with t.Cleanup")
	}
	// updateFetch (update.go) is the update check's one HTTP request, to
	// api.github.com: no test may reach it (R124).
	updateFetch = func(context.Context, *url.URL, string) (updatecheck.Release, error) {
		panic("updateFetch reached from internal/cli's test binary: a test must stub it with stubFetch")
	}
	// updateReleases (update.go) is the what's new summary's one HTTP request.
	updateReleases = func(context.Context, *url.URL, string) ([]updatecheck.Release, error) {
		panic("updateReleases reached from internal/cli's test binary: a test must stub it (stubUpdateSeams does)")
	}
	// The update loop's clock, jitter, timer and child (updateloop.go).
	updateLoopClock = func() time.Time {
		panic("updateLoopClock reached from internal/cli's test binary: a test must stub it")
	}
	updateJitter = func() time.Duration {
		panic("updateJitter reached from internal/cli's test binary: a test must stub it")
	}
	newUpdateTimer = func(time.Duration) (<-chan time.Time, func()) {
		panic("newUpdateTimer reached from internal/cli's test binary: a test must stub it")
	}
	autoUpdateRun = func(context.Context, string, string) autoRun {
		panic("autoUpdateRun reached from internal/cli's test binary: a test must stub it")
	}
	installedVersion = func(string) (string, error) {
		panic("installedVersion reached from internal/cli's test binary: a test must stub it")
	}
	// The restart loop's spawn and ticker (restartloop.go, R126): a test
	// must never start a real chottag.
	restartSpawn = func(string, string, ...string) error {
		panic("restartSpawn reached from internal/cli's test binary: a test must stub it")
	}
	installedViaLink = func(string, string) bool {
		panic("installedViaLink reached from internal/cli's test binary: a test must stub it")
	}
	newRestartTicker = func(time.Duration) (<-chan time.Time, func()) {
		panic("newRestartTicker reached from internal/cli's test binary: a test must stub it")
	}
	// The session watcher's ticker never fires and its probe must not run
	// (sessiongone.go, R164): a test drives both itself.
	newSessionTicker = func(time.Duration) (<-chan time.Time, func()) {
		return make(chan time.Time), func() {}
	}
	sessionProbe = func() bool {
		panic("sessionProbe reached from internal/cli's test binary: a test must stub it")
	}
	// A package-wide safe default for $HOME, installed before any test runs:
	// setup.go and uninstall.go both read os.Getenv("HOME") for the shell rc
	// path, and a mutation-testing exercise against login.go or logout.go
	// (§6.1 invariant 1's own mutation table) can make runLogin/runLogout
	// write there too. Most tests in this package override HOME themselves
	// via t.Setenv (which restores this default afterwards), but a test that
	// forgets to — or a live mutation run before its compensating edit is
	// applied — must land in a throwaway directory, never this test runner's
	// real ~/.claude.json or shell rc.
	//
	// This directory is deliberately never cleaned up if a test panics or
	// the test binary is killed on a timeout: os.Exit below is skipped in
	// both cases, so nothing after m.Run() would run anyway. It is a single
	// throwaway dir under the OS temp directory, which the OS or CI job
	// already reclaims; chasing every abnormal-exit path to remove it too is
	// not worth the complexity.
	safeHome, err := os.MkdirTemp("", "chottag-cli-test-home-")
	if err != nil {
		panic(err)
	}
	testMainHome = safeHome
	if err := os.Setenv("HOME", safeHome); err != nil {
		panic(err)
	}
	// A developer's real shell may already export CHOTTAG_HOME (e.g. a
	// personal install to try chottag against); a test that forgets its own
	// t.Setenv("CHOTTAG_HOME", ...) must never silently inherit that instead
	// of failing loudly against whatever home() falls back to.
	os.Unsetenv("CHOTTAG_HOME")
	// The same for the fake-limit hook's variables (spec §10.1): a
	// developer's shell that still exports them from a checklist run must
	// not turn every daemon test in a tagged build into a limit test.
	os.Unsetenv("CHOTTAG_FAKE_LIMIT")
	os.Unsetenv("CHOTTAG_FAKE_LIMIT_TTL")
	os.Unsetenv("CHOTTAG_FAKE_UTIL")
	// This test binary is itself very often run from inside a real cmux
	// surface (F243): cli.Run passes os.Environ() straight through to
	// shim.Run for a `claude` argv0, so without this, every one of those
	// six variables would genuinely be present and executable, and
	// shim.Run's cmux hand-off (cmuxHandoffTarget) would fire for real
	// against whatever this test stubbed execFn to do — long before a
	// test's own assertions about the daemon, the identity check or
	// version wiring ever ran. Unsetting them here, process-wide, is what
	// makes TestCliRunPassesItsOwnVersionToShim's own isolation redundant.
	os.Unsetenv("CMUX_SURFACE_ID")
	// statusline sets the cmux pill when this is present (cmuxSetStatus).
	os.Unsetenv("CMUX_WORKSPACE_ID")
	os.Unsetenv("CMUX_CLAUDE_WRAPPER_SHIM")
	os.Unsetenv("CMUX_CLAUDE_PID")
	os.Unsetenv("CMUX_CLAUDE_HOOKS_DISABLED")
	os.Unsetenv("CMUX_CUSTOM_CLAUDE_PATH")
	os.Unsetenv("CHOTTAG_CMUX_HANDOFF")
	// Run from inside a chottag session, the shell's HTTPS_PROXY is chottag's
	// own address, so `daemon restart` reports an upstream change the test
	// never set up (F250). A test that needs a proxy sets it with t.Setenv.
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		os.Unsetenv(k)
	}
	// login, logout and adopt now resolve the real claude from PATH when no
	// --claude is given (issue #2, R145). Put a do-nothing stand-in first so
	// the default resolves the same everywhere: a machine with no claude (CI)
	// and one with a real claude (a developer's) must not differ, and a test
	// that forgets --claude must never run the real one. It reports "not
	// logged in" (exit 1). A test that cares sets its own PATH.
	fakeClaudeDir, err := os.MkdirTemp("", "chottag-fakeclaude-")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(fakeClaudeDir, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", fakeClaudeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(fakeClaudeDir)
	os.RemoveAll(safeHome)
	if testClaudeConfigDir != "" {
		os.RemoveAll(testClaudeConfigDir)
	}
	os.Exit(code)
}

// testMainHome is the safe default $HOME TestMain installs above, before any
// test runs. runLifecycle rejects exactly this value (not just any path
// under os.TempDir(), which every test's own fake HOME also is): a caller
// that never overrode HOME with its own t.Setenv would otherwise silently
// pass runLifecycle's guard just because TestMain's default happens to live
// under the OS temp directory too.
var testMainHome string

// --- shared lifecycle helper ---------------------------------------------

// lifecycleCapture collects every byte chottag's setup -> shim.Run ->
// uninstall sequence emits across all three steps, plus the environment
// shim.Run's stubbed execFn was actually invoked with, so an invariant test
// can sweep the whole thing for a secret or a property regardless of which
// step would have leaked or set it.
type lifecycleCapture struct {
	setupOut, setupErr         bytes.Buffer
	loginOut, loginErr         bytes.Buffer
	shimOut, shimErr           bytes.Buffer
	logoutOut, logoutErr       bytes.Buffer
	uninstallOut, uninstallErr bytes.Buffer
	execBin                    string
	execArgs, execEnv          []string
}

// allOutput concatenates everything captured on every stdout/stderr across
// the whole lifecycle, for a test that only cares whether a secret ever
// reached any of them.
func (c *lifecycleCapture) allOutput() string {
	return c.setupOut.String() + c.setupErr.String() +
		c.loginOut.String() + c.loginErr.String() +
		c.shimOut.String() + c.shimErr.String() +
		c.logoutOut.String() + c.logoutErr.String() +
		c.uninstallOut.String() + c.uninstallErr.String()
}

// putFakeClaudeOnPath writes an executable stand-in for the real `claude`
// binary into a fresh directory and prepends it to $PATH (t.Setenv, so it is
// undone automatically), so shim.ResolveClaude — and therefore shim.Run —
// succeeds without ever touching a real claude installation. Returns the
// fake binary's path.
func putFakeClaudeOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

// healthServer serves h as JSON at proxy.HealthPath, standing in for the
// daemon's health endpoint that shim.Run's probe expects before it will
// proceed (same technique as internal/shim's own shim_test.go). It also
// answers a valid nonce with home's own proxy secret's proof (F221), the
// same shape internal/shim's provingHealthServer uses, so shim.Run's
// VerifyHealth resolves IdentityVerified — the lifecycle a real `claude`
// launch goes through — rather than IdentityLegacy.
func healthServer(t *testing.T, home string, h proxy.Health) *httptest.Server {
	t.Helper()
	secret, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := h
		if n := r.URL.Query().Get(proxyauth.NonceParam); proxyauth.ValidNonce(n) {
			doc.Proof = secret.Proof(identityHealthServerPort(t, r), n)
			doc.Sessions = true // like a daemon that accepts session credentials
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			t.Fatal(err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// requireUnderTempDir fails the test unless path is os.TempDir() itself or a
// descendant of it. Comparing with a plain strings.HasPrefix(path,
// os.TempDir()) would also accept an unrelated sibling that merely shares
// the string prefix (e.g. "/tmpfoo" for "/tmp"), so this checks the path
// component boundary explicitly instead.
func requireUnderTempDir(t *testing.T, label, path string) {
	t.Helper()
	td := filepath.Clean(os.TempDir())
	p := filepath.Clean(path)
	if p == td || strings.HasPrefix(p, td+string(os.PathSeparator)) {
		return
	}
	t.Fatalf("%s=%q is not under the OS temp dir %q", label, path, td)
}

// envValue returns key's value in env (a KEY=VALUE slice), or "" if key is
// absent — just enough to pull a single entry (e.g. HTTPS_PROXY) out of a
// captured execEnv for a failure message, without printing the whole slice
// (fix round 1 item 7: the slice may carry the secret).
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			return v
		}
	}
	return ""
}

// mustPort parses the TCP port a test server is listening on out of its URL.
func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	u := strings.TrimPrefix(rawURL, "http://")
	_, portStr, ok := strings.Cut(u, ":")
	if !ok {
		t.Fatalf("could not parse a port out of %q", rawURL)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// lifecycleToken is the fake token runLifecycle's stubbed claudeAuthExec
// writes into the slot during login, so an invariant test has a real,
// grep-able secret to look for rather than an empty slot that could never
// prove invariant 2 caught anything (F106).
const lifecycleToken = "sk-ant-oat01-LIFECYCLE-SLOT-TOKEN"

// runLifecycle drives runSetup, then login, then shim.Run (against a
// stubbed, confirmed health server so the shim never tries to spawn a real
// daemon), then logout, then runUninstall — chottag's whole
// install/login/launch/logout/remove sequence — against home. The caller
// must already have CHOTTAG_HOME=home, HOME, SHELL and a resolvable `claude`
// on $PATH set via t.Setenv (putFakeClaudeOnPath does the last one); this
// only drives the steps and captures everything they wrote.
func runLifecycle(t *testing.T, home string) *lifecycleCapture {
	t.Helper()
	// Belt and braces alongside TestMain's package-wide safe default: this
	// drives runLogin/runLogout for real (no stub between them and whatever
	// they write to $HOME), so a caller that forgot to point HOME at a temp
	// dir of its own must fail loudly here rather than silently reusing
	// TestMain's default. That default is itself under os.TempDir(), so a
	// bare "is HOME under os.TempDir()" check could never fire — every
	// caller would trivially pass it whether or not it set its own fake
	// HOME. Rejecting testMainHome by name is what actually catches a
	// forgetful caller; requireUnderTempDir is a second, independent check
	// against whatever a caller DID set (not a naive strings.HasPrefix
	// against os.TempDir(), which would also accept an unrelated sibling
	// path like "/tmpfoo" for "/tmp").
	if h := os.Getenv("HOME"); h == testMainHome {
		t.Fatalf("HOME=%q is still TestMain's package-wide default: set your own fake HOME with t.Setenv(\"HOME\", t.TempDir()) before calling runLifecycle", h)
	}
	requireUnderTempDir(t, "HOME", os.Getenv("HOME"))
	c := &lifecycleCapture{}

	// Both irreversible seams must be stubbed before runSetup even starts,
	// not just before the steps that use them: TestMain's defaults panic,
	// and login's stub is what seeds lifecycleToken into the slot so
	// invariant 2 has something real to look for.
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, sub string, _ io.Reader, _, _ io.Writer) error {
		if sub == "login" {
			return os.WriteFile(filepath.Join(slotDir, ".credentials.json"),
				[]byte(`{"accessToken":"`+lifecycleToken+`"}`), 0o600)
		}
		return nil
	}))
	// credsDeleteCalls records every slot dir credsDelete was actually
	// invoked with, so the assertion after runLogout below (F2) can prove
	// logout really reached the delete step for slotADir, rather than only
	// checking runLogout's exit code (which a mutation returning exit.OK
	// early — right after the --yes confirmation, before anything is
	// deleted — would still satisfy).
	var credsDeleteCalls []string
	t.Cleanup(SetCredsDeleteForTest(func(dir string) error {
		credsDeleteCalls = append(credsDeleteCalls, dir)
		return nil
	}))

	if code := runSetup(nil, newReporter(false, &c.setupOut, &c.setupErr)); code != 0 {
		t.Fatalf("runSetup = %d, stderr = %q", code, c.setupErr.String())
	}

	fakeClaude := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com","orgName":"Org"}`)
	if code := runLogin([]string{"--claude", fakeClaude, "A"}, strings.NewReader(""), newReporter(false, &c.loginOut, &c.loginErr)); code != 0 {
		t.Fatalf("runLogin = %d, stderr = %q", code, c.loginErr.String())
	}
	slotADir := filepath.Join(home, "accounts", "A")
	// Prove the seeding in the stub above actually happened, so
	// TestInvariant2NeverCopiesATokenOutOfASlot's lifecycleToken assertions
	// have something real to find rather than sweeping for a token that was
	// never written (F106).
	loginCredPath := filepath.Join(slotADir, ".credentials.json")
	if b, err := os.ReadFile(loginCredPath); err != nil || !strings.Contains(string(b), lifecycleToken) {
		t.Fatalf("login did not write lifecycleToken into the slot at %s: contents = %q, err = %v", loginCredPath, b, err)
	}

	daemon := healthServer(t, home, proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()})
	port := mustPort(t, daemon.URL)
	writeStateWithPort(t, home, port)

	restore := shim.SetSeamsForTest(
		func(bin string, args, env []string) error {
			c.execBin = bin
			c.execArgs = append([]string(nil), args...)
			c.execEnv = append([]string(nil), env...)
			return nil
		},
		func(exe, home, upstream string) error {
			t.Fatal("spawnFn reached: the stubbed health server should already have confirmed the daemon")
			return nil
		},
	)
	defer restore()

	if code := shim.Run(nil, home, []string{"PATH=" + os.Getenv("PATH")}, Version, &c.shimOut, &c.shimErr); code != 0 {
		t.Fatalf("shim.Run = %d, stderr = %q", code, c.shimErr.String())
	}
	if c.execBin == "" {
		t.Fatal("shim.Run returned 0 but never reached execFn")
	}
	// F221: a verified daemon (healthServer above proves home's own secret)
	// is handed the secret in HTTPS_PROXY, never the bare loopback address.
	secret, err := proxyauth.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	// Never print c.execEnv or the URL on failure (fix round 1 item 7): both
	// carry the secret, and a test failure's own output is not exempt from
	// "never print it". M6: the URL is a per-session one, chottag.default.<sid>.
	if !isSessionProxyURL(secret, envValue(c.execEnv, "HTTPS_PROXY"), "127.0.0.1:"+strconv.Itoa(port)) {
		t.Errorf("shim.Run's execEnv missing the session URL; HTTPS_PROXY = %s", redact.UpstreamProxy(envValue(c.execEnv, "HTTPS_PROXY")))
	}

	if code := runLogout([]string{"--yes", "--force", "--claude", fakeClaude, "A"}, strings.NewReader(""), newReporter(false,
		&c.logoutOut, &c.logoutErr)); code != 0 {
		t.Fatalf("runLogout = %d, stderr = %q", code, c.logoutErr.String())
	}
	// F2: prove logout actually happened, not just that it returned 0 — a
	// mutation that returns exit.OK right after the confirmation, before
	// deleting anything, would still pass the check above.
	if _, err := os.Stat(slotADir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("logout did not remove the slot at %s: stat err = %v", slotADir, err)
	}
	if len(credsDeleteCalls) != 1 || credsDeleteCalls[0] != slotADir {
		t.Errorf("credsDelete calls = %v, want exactly one call with %s", credsDeleteCalls, slotADir)
	}

	if code := runUninstall(nil, strings.NewReader(""), newReporter(false, &c.uninstallOut, &c.uninstallErr)); code != 0 {
		t.Fatalf("runUninstall = %d, stderr = %q", code, c.uninstallErr.String())
	}
	return c
}

// hashPath returns a sha256 digest of path's content: the file's bytes if
// path is a regular file, or the sorted concatenation of every regular
// file's relative path and bytes if path is a directory, or "absent" if
// path does not exist. Two calls around a chunk of code that is supposed to
// leave path untouched can compare these values directly.
func hashPath(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if !info.IsDir() {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(b)
		return hex.EncodeToString(h.Sum(nil))
	}
	var files []string
	if err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, f := range files {
		rel, err := filepath.Rel(path, f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		h.Write([]byte(rel))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- Invariant 1: never writes Claude's config ----------------------------

// TestInvariant1NeverWritesClaudesConfig hashes <fakeHome>/.claude and
// <fakeHome>/.claude.json — Claude Code's own config, never chottag's —
// specifically, not $HOME as a whole: runSetup deliberately writes the
// shell rc, which also lives under $HOME, so a whole-$HOME hash would
// either fail here or get relaxed until it checks nothing. The rc is
// invariant 5's job (TestInvariant5AfterUninstallPlainClaudeIsUnchanged).
func TestInvariant1NeverWritesClaudesConfig(t *testing.T) {
	home, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")
	putFakeClaudeOnPath(t)

	claudeDir := filepath.Join(fakeHome, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	claudeJSON := filepath.Join(fakeHome, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{"oauthAccount":{"emailAddress":"user@example.com"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	beforeDir, beforeJSON := hashPath(t, claudeDir), hashPath(t, claudeJSON)

	runLifecycle(t, home)

	if got := hashPath(t, claudeDir); got != beforeDir {
		t.Errorf("%s changed across setup -> login -> shim.Run -> logout -> uninstall: chottag must never write Claude's own config (§6.1 invariant 1)", claudeDir)
	}
	if got := hashPath(t, claudeJSON); got != beforeJSON {
		t.Errorf("%s changed across setup -> login -> shim.Run -> logout -> uninstall: chottag must never write Claude's own config (§6.1 invariant 1)", claudeJSON)
	}
}

// --- Invariant 2: never copies a token out of a slot ----------------------

// TestInvariant2NeverCopiesATokenOutOfASlot seeds a slot with a recognisable
// fake token (a literal, grep-able string — never a placeholder) and asserts
// it appears in no file under home afterwards, and in no captured
// stdout/stderr from any of the three steps.
func TestInvariant2NeverCopiesATokenOutOfASlot(t *testing.T) {
	const secret = "sk-ant-FAKE-DO-NOT-USE-oauth-token"

	home, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")
	putFakeClaudeOnPath(t)

	slotDir := filepath.Join(home, "accounts", "demo")
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":9999999999999,"scopes":["user:inference"],"subscriptionType":"pro"}}`, secret)
	credsPath := filepath.Join(slotDir, ".credentials.json")
	if err := os.WriteFile(credsPath, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}

	c := runLifecycle(t, home)

	// F221: the proxy secret itself must be swept alongside the tokens —
	// it is a bearer credential too (RFC 7617 Basic in the shim's
	// HTTPS_PROXY), and Global Constraints requires it be found in no
	// output and no file under home except its own, ca/proxy.secret.
	proxySecretPath := proxyauth.Path(home)
	rawProxySecret, err := os.ReadFile(proxySecretPath)
	if err != nil {
		t.Fatal(err)
	}
	proxySecretText := strings.TrimSpace(string(rawProxySecret))

	// lifecycleToken is runLifecycle's own login -> logout token (its own
	// slot, "A", distinct from the "demo" slot seeded above): runLifecycle
	// already asserts it was really written into the slot after login, so
	// this has something real to find, not a token that was never written
	// (F106).
	markers := []string{secret, lifecycleToken, proxySecretText}

	for _, marker := range markers {
		if strings.Contains(c.allOutput(), marker) {
			t.Errorf("captured stdout/stderr across setup -> login -> shim.Run -> logout -> uninstall contained a slot's token: chottag must never copy a token out of a slot (§6.1 invariant 2); marker = %q, output = %q", marker, c.allOutput())
		}
	}

	// F1: sweep both CHOTTAG_HOME (where the slots themselves live) and the
	// test's fake $HOME — a leak that copies a slot's credential to
	// somewhere under $HOME (e.g. runLogin writing $HOME/leaked-token.json)
	// would previously have gone uncaught, since only `home` was ever
	// walked. proxySecretPath is skipped too: it legitimately holds
	// proxySecretText.
	assertNoMarkers(t, home, markers, func(p string) bool { return p == credsPath || p == proxySecretPath })
	assertNoMarkers(t, fakeHome, markers, nil)
}

// assertNoMarkers walks root and fails the test if any regular file under it
// contains one of markers, skipping any path skip reports true for (a
// seeded original, not a copy of it) — skip may be nil to skip nothing.
func assertNoMarkers(t *testing.T, root string, markers []string, skip func(path string) bool) {
	t.Helper()
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (skip != nil && skip(p)) {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, marker := range markers {
			if strings.Contains(string(b), marker) {
				t.Errorf("%s contains a slot's token: chottag must never copy a token out of a slot (§6.1 invariant 2); marker = %q", p, marker)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- Invariant 3: only the daemon refreshes a slot token ------------------

// TestInvariant3OnlyTheDaemonRefreshesASlotToken asserts the env shim.Run
// builds for the real `claude` it execs never carries CLAUDE_CONFIG_DIR
// (which is what would point Claude Code's own token machinery at a slot),
// and that the shim can never invoke the refresher at all: internal/shim's
// own (non-test) source never imports internal/refresh, so there is no call
// it could make even by accident.
func TestInvariant3OnlyTheDaemonRefreshesASlotToken(t *testing.T) {
	home, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")
	putFakeClaudeOnPath(t)

	c := runLifecycle(t, home)

	for _, e := range c.execEnv {
		if strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") {
			t.Errorf("shim.Run's env for the real claude carried %q: only the daemon may point CLAUDE_CONFIG_DIR at a slot to refresh its token (§6.1 invariant 3)", e)
		}
	}

	if shimDir := filepath.Join("..", "shim"); packageImports(t, shimDir, "github.com/HaiNNT/c-hottag/internal/refresh") {
		t.Error("internal/shim imports internal/refresh: the shim must never be able to invoke the refresher, only the daemon may (§6.1 invariant 3)")
	}
}

// packageImports reports whether any non-test .go file directly under dir
// imports pkgPath. It parses only the import declarations (go/parser with
// parser.ImportsOnly), not a raw grep over the source text — a doc comment
// that happens to mention pkgPath as a string, e.g. this very file's own
// comments about internal/refresh, can never produce a false positive.
func packageImports(t *testing.T, dir, pkgPath string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == pkgPath {
				return true
			}
		}
	}
	return false
}

// --- Invariant 4: logs never contain bodies or tokens ----------------------

// TestInvariant4LogsNeverContainBodiesOrTokens runs a real request, carrying
// a recognisable marker in both its request and response bodies, through a
// real chottag proxy (proxytest, the same harness internal/proxy's own
// tests use) against a fake Anthropic upstream, then sweeps every file the
// harness wrote — including its request log, the mechanism that writes
// production's proxy.jsonl — for the marker.
//
// proxytest does not reproduce chottag's on-disk layout (it has no
// daemon.log at all: that file only ever receives the export/startup/error
// lines internal/cli/daemon.go writes to it, never request content, so
// there is structurally nothing to prove about it here), so this proves the
// invariant at the one place a request body could actually reach a log:
// tracelog, which production wires to proxy.jsonl exactly as this harness
// wires it to its own request log.
func TestInvariant4LogsNeverContainBodiesOrTokens(t *testing.T) {
	const secret = "sk-ant-FAKE-DO-NOT-USE-request-body-marker"
	// bearerSecret and apiKeySecret are the "or tokens" half: without a
	// credential header on the request, this test cannot fail for a bug
	// that logs one (fix round 3, D3 — the reviewer proved the old version
	// stayed green under a mutation that wrote a raw Authorization header
	// straight into the record). Both are swept alongside secret below, and
	// neither is ever sent to Choose (the Options below leave Choose nil),
	// so forward.go never swaps them — they reach the log path exactly as
	// the caller sent them, which is what must never happen.
	const bearerSecret = "sk-ant-FAKE-DO-NOT-USE-bearer-credential"
	const apiKeySecret = "sk-ant-FAKE-DO-NOT-USE-api-key-credential"
	markers := []string{secret, bearerSecret, apiKeySecret}

	sawSecret := make(chan bool, 1)
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sawSecret <- strings.Contains(string(body), secret)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":%q}`, secret)
	}), proxytest.Options{
		// Shapes and LimitFingerprint turn on the two code paths that
		// actually read body bytes (tracelog.Shape, tracelog.FingerprintLimit);
		// a bare proxytest.Options{} (this test's shape before fix round 3's
		// D3) proves the invariant only in the proxy's least
		// body-touching configuration.
		Shapes:           true,
		LimitFingerprint: true,
	})

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(fmt.Sprintf(`{"marker":%q}`, secret)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerSecret)
	req.Header.Set("X-Api-Key", apiKeySecret)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}

	if !<-sawSecret {
		t.Fatal("fake upstream never saw the marker in the request body: test is not exercising what it claims")
	}
	if !strings.Contains(string(respBody), secret) {
		t.Fatal("response never carried the marker either: test is not exercising what it claims")
	}

	// Wait for the request to actually land in the log before checking its
	// absence, so a slow write can never make this pass vacuously.
	h.Records(t, "req", 1)

	for _, marker := range markers {
		if h.LogContains(t, marker) {
			t.Errorf("chottag's request log %s contains %q: logs must never contain bodies or tokens (§6.1 invariant 4)", h.LogPath, marker)
		}
	}

	// Sweep every other file the harness wrote too (its CA material, the
	// upstream's own CA, ...), not just the request log by name, so a leak
	// into any file under the harness's root would still be caught.
	root := filepath.Dir(h.LogPath)
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, marker := range markers {
			if strings.Contains(string(b), marker) {
				t.Errorf("%s contains %q: logs must never contain bodies or tokens (§6.1 invariant 4)", p, marker)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- Invariant 5: after uninstall, plain claude is unchanged --------------

// TestInvariant5AfterUninstallPlainClaudeIsUnchanged asserts the rc comes
// back byte-identical to its pre-setup original, bin/claude is gone, and
// shim.ResolveClaude resolves to the exact same real claude it did before
// setup ever ran — uninstall must leave a plain `claude` invocation
// completely unaffected.
func TestInvariant5AfterUninstallPlainClaudeIsUnchanged(t *testing.T) {
	home, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")
	real := putFakeClaudeOnPath(t)

	rc := filepath.Join(fakeHome, ".zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	before, err := shim.ResolveClaude(os.Getenv("PATH"), filepath.Join(home, "bin"), "")
	if err != nil {
		t.Fatalf("ResolveClaude before setup: %v", err)
	}
	if before != real {
		t.Fatalf("ResolveClaude before setup = %q, want the fake claude %q: test setup is broken", before, real)
	}

	runLifecycle(t, home)

	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("rc = %q after uninstall, want the pre-setup original %q (§6.1 invariant 5)", got, original)
	}
	if _, err := os.Lstat(filepath.Join(home, "bin", "claude")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("bin/claude still exists after uninstall: %v", err)
	}
	after, err := shim.ResolveClaude(os.Getenv("PATH"), filepath.Join(home, "bin"), "")
	if err != nil {
		t.Fatalf("ResolveClaude after uninstall: %v", err)
	}
	if after != before {
		t.Errorf("ResolveClaude = %q after uninstall, want the pre-setup %q unchanged (§6.1 invariant 5)", after, before)
	}
}

// --- cli.Run threads its OWN Version into shim.Run ------------------------

// TestCliRunPassesItsOwnVersionToShim is fix round 2's N2/N5 test: cli.Run
// must pass Version itself, not some hard-coded or default string, into
// shim.Run. It sets Version to a sentinel unlikely to collide with
// anything else, then drives a `claude` invocation against a health server
// that answers Chottag: true with NO proof and reports that exact
// sentinel as its own Version, while a daemon.lock for home is held by the
// very pid the health document names.
//
// T10 guarantees a part-1-or-later daemon never serves at all unless its
// secret loaded (its start fails first), so the same running build
// answering with no proof cannot legitimately be a legacy (pre-part-1)
// daemon that merely hasn't been asked for a proof yet — it must read as a
// mismatch (controller ruling, fix round 1 item 5 / fix round 2 N4). If
// cli.Run passed the wrong version (a stale default, an empty string, or
// simply forgot to thread Version through at all), this proof-less,
// PID-matched, lock-held daemon would satisfy shim.Run's legacy branch
// instead and the session would start — this test fails exactly then.
func TestCliRunPassesItsOwnVersionToShim(t *testing.T) {
	const sentinelVersion = "cli-version-wiring-sentinel"
	origVersion := Version
	Version = sentinelVersion
	t.Cleanup(func() { Version = origVersion })

	home, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")
	// This is the one test in this package that reaches shim.Run with the
	// real os.Environ() (cli.Run's own production behaviour for argv0 =
	// .../claude). It no longer needs its own CMUX_SURFACE_ID isolation
	// (fix round 1, F243-R1): TestMain above now unsets every cmux
	// variable process-wide, for exactly this reason.
	putFakeClaudeOnPath(t)

	if _, err := proxyauth.LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}

	// A proof-less server (no secret at all — unlike healthServer above,
	// which always proves home's own secret) reporting Version:
	// sentinelVersion, this test's own os.Getpid() as its PID.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(proxy.Health{Chottag: true, Version: sentinelVersion, PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
	}))
	t.Cleanup(srv.Close)
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)

	release, err := daemonlock.Acquire(home, daemonlock.Record{PID: os.Getpid(), Started: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release() })

	var execCalled bool
	t.Cleanup(shim.SetSeamsForTest(
		func(string, []string, []string) error { execCalled = true; return nil },
		func(string, string, string) error {
			t.Fatal("spawnFn reached: the stubbed health server should already have answered")
			return nil
		},
	))

	var out, errb bytes.Buffer
	code := Run(filepath.Join(t.TempDir(), "claude"), nil, &out, &errb)
	if code == 0 {
		t.Fatal("cli.Run succeeded against a same-version proof-less daemon, want a failure")
	}
	if execCalled {
		t.Error("claude was exec'd against a same-version proof-less daemon")
	}
	if !strings.Contains(errb.String(), "did not prove it holds this install's proxy secret") {
		t.Errorf("stderr %q must be the mismatch message, not a legacy warning", errb.String())
	}
	if strings.Contains(errb.String(), "predates proxy authentication") {
		t.Errorf("stderr %q must not treat a same-version proof-less daemon as legacy", errb.String())
	}
}

// isSessionProxyURL reports whether raw is a session proxy URL for hostport
// that secret accepts as an identified caller.
func isSessionProxyURL(secret proxyauth.Secret, raw, hostport string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.Host != hostport {
		return false
	}
	pass, _ := u.User.Password()
	c, ok := secret.Caller("Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pass)))
	return ok && c.Identified()
}
