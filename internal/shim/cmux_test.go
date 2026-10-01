package shim

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/session"
)

// cmuxTestEnv builds the environment for a launch inside a cmux surface
// whose CMUX_CLAUDE_WRAPPER_SHIM is shimPath, with real (the fake claude
// fixture) first on PATH so the normal path, if it runs, succeeds. extra is
// appended last, so a test can override CMUX_CLAUDE_PID, add the hand-off
// marker, or set one of the other cmux variables.
func cmuxTestEnv(shimPath, real string, extra ...string) []string {
	env := []string{
		"CMUX_SURFACE_ID=surface-1",
		"CMUX_CLAUDE_WRAPPER_SHIM=" + shimPath,
		"PATH=" + filepath.Dir(real),
	}
	return append(env, extra...)
}

// writeCmuxShim writes an executable file standing in for cmux's own
// per-surface shim script.
func writeCmuxShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cmux-claude-wrapper")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// swapGetpid replaces getpid with a fixed value and returns a func that
// restores the original.
func swapGetpid(pid int) func() {
	orig := getpid
	getpid = func() int { return pid }
	return func() { getpid = orig }
}

// health builds a minimal Health document a fake daemon can prove, for
// tests in this file that need the normal path to succeed (whether a
// hand-off was correctly skipped, or the hand-off itself needs a complete
// child environment to hand off — fix round 1, F243-R1), not to assert
// anything about the health document's own content.
func health(t *testing.T, home string) proxy.Health {
	t.Helper()
	return proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()}
}

// pinnedSID is the sid the fixture makes the shim mint.
const pinnedSID = "0123456789abcdef0123456789abcdef"

// cmuxHandoffFixture bundles what every F243 test in this file needs: a
// home with a running (fake) daemon already confirmed at state.json's own
// port — an httptest server's real, ephemeral port, never 47821 — a fake
// claude on PATH, and a stand-in for cmux's own wrapper shim. Fix round 1
// (F243-R1) moved the hand-off itself to AFTER the daemon is probed and the
// child environment is built, so every hand-off test now needs a working
// daemon behind it, not just the ones that fall through to the normal path;
// using the exact same provingHealthServer/writeStateWithPort pattern every
// other Run test in this package uses is what keeps a future regression
// from ever silently probing the real default port instead.
type cmuxHandoffFixture struct {
	home, real, cmuxShim string
	secret               proxyauth.Secret
	port                 int
}

func newCmuxHandoffFixture(t *testing.T) cmuxHandoffFixture {
	t.Helper()
	home := t.TempDir()
	secret := secretOf(t, home)
	daemon := provingHealthServer(t, secret, health(t, home))
	port := mustPort(t, daemon.URL)
	writeStateWithPort(t, home, port)
	origSID := newSID
	newSID = func() string { return pinnedSID }
	t.Cleanup(func() { newSID = origSID })
	return cmuxHandoffFixture{
		home:     home,
		real:     fakeClaude(t),
		cmuxShim: writeCmuxShim(t),
		secret:   secret,
		port:     port,
	}
}

// Inside a cmux surface, with an executable wrapper shim, no hand-off
// marker, and CMUX_CLAUDE_PID absent: the shim hands off to cmux's wrapper
// with the FULLY-BUILT child environment (fix round 1, F243-R1) — the
// daemon has already been probed, HTTPS_PROXY and NODE_EXTRA_CA_CERTS are
// already set — and the session IS already registered by hand-off time
// (fix round 2, F243-R2): exec keeps this same pid, so if cmux's wrapper
// ends up resolving some other claude than chottag's shim, the session is
// still registered rather than lost entirely.
func TestCmuxHandsOffToTheWrapperShimWithTheFullChildEnv(t *testing.T) {
	f := newCmuxHandoffFixture(t)

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	code := Run([]string{"--resume"}, f.home, cmuxTestEnv(f.cmuxShim, f.real), ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.cmuxShim {
		t.Errorf("exec'd %q, want cmux's wrapper shim %q", got.bin, f.cmuxShim)
	}
	if want := []string{"--resume"}; !slices.Equal(got.args, want) {
		t.Errorf("args = %v, want %v: the original arguments must be passed through unchanged", got.args, want)
	}
	if !slices.Contains(got.env, "CHOTTAG_CMUX_HANDOFF=1") {
		t.Errorf("env missing CHOTTAG_CMUX_HANDOFF=1")
	}
	if !slices.Contains(got.env, "HTTPS_PROXY="+f.secret.SessionProxyURL("127.0.0.1:"+strconv.Itoa(f.port), proxyauth.DefaultPool, pinnedSID)) {
		// Never print got.env or the expected URL themselves: both may carry the
		// secret (the same rule shim_test.go's own happy-path test follows).
		t.Error("handed-off env is missing HTTPS_PROXY in the secret's own proxy-URL shape")
	}
	if !slices.ContainsFunc(got.env, func(kv string) bool { return strings.HasPrefix(kv, "NODE_EXTRA_CA_CERTS=") }) {
		t.Error("handed-off env is missing NODE_EXTRA_CA_CERTS")
	}
	if len(got.live) != 1 || got.live[0].PID != os.Getpid() || got.live[0].Port != f.port ||
		got.live[0].SID != pinnedSID || got.live[0].Pool != proxyauth.DefaultPool {
		t.Errorf("registry at hand-off time = %v, want exactly one entry for this pid (%d), port (%d), pinned sid and default pool", got.live, os.Getpid(), f.port)
	}
}

// A nested session — CMUX_CLAUDE_PID naming some OTHER process, not this
// one — must still hand off: the wrapper never ran for THIS launch, so
// point 4 of cmuxHandoffTarget's own doc comment does not apply.
func TestHandsOffForANestedSessionWithADifferentPid(t *testing.T) {
	f := newCmuxHandoffFixture(t)
	const ourPid = 9000
	restoreGetpid := swapGetpid(ourPid)
	defer restoreGetpid()

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	env := cmuxTestEnv(f.cmuxShim, f.real, "CMUX_CLAUDE_PID="+strconv.Itoa(ourPid+1))
	code := Run(nil, f.home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.cmuxShim {
		t.Errorf("exec'd %q, want cmux's wrapper shim %q: CMUX_CLAUDE_PID naming a different process must still hand off", got.bin, f.cmuxShim)
	}
}

// CMUX_CLAUDE_PID equal to this process's own pid means cmux's wrapper has
// already exec'd us directly (its hooks path): no second hand-off, the
// normal path runs instead.
func TestNoHandoffWhenWrapperAlreadyExecdUs(t *testing.T) {
	f := newCmuxHandoffFixture(t)
	restoreGetpid := swapGetpid(4242)
	defer restoreGetpid()

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	code := Run(nil, f.home, cmuxTestEnv(f.cmuxShim, f.real, "CMUX_CLAUDE_PID=4242"), ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.real {
		t.Errorf("exec'd %q, want the real claude %q: CMUX_CLAUDE_PID == our own pid must skip the hand-off", got.bin, f.real)
	}
}

// The hand-off marker already set means this IS the second pass (cmux's
// wrapper re-exec'd chottag's shim as "the real claude"): no hand-off, and
// the marker must not leak into the env the real claude receives.
func TestMarkerSetMeansNoHandoffAndMarkerIsDropped(t *testing.T) {
	f := newCmuxHandoffFixture(t)

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	code := Run(nil, f.home, cmuxTestEnv(f.cmuxShim, f.real, "CHOTTAG_CMUX_HANDOFF=1"), ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.real {
		t.Errorf("exec'd %q, want the real claude %q: an already-set marker must skip the hand-off", got.bin, f.real)
	}
	for _, kv := range got.env {
		if kv == "CHOTTAG_CMUX_HANDOFF=1" {
			t.Errorf("env passed to the real claude still carries the marker %v: a nested claude must start clean", got.env)
		}
	}
}

// No CMUX_SURFACE_ID at all: not inside cmux, no hand-off.
func TestNoHandoffOutsideCmux(t *testing.T) {
	f := newCmuxHandoffFixture(t)

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	env := []string{
		"CMUX_CLAUDE_WRAPPER_SHIM=" + f.cmuxShim,
		"PATH=" + filepath.Dir(f.real),
	}
	code := Run(nil, f.home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.real {
		t.Errorf("exec'd %q, want the real claude %q: no CMUX_SURFACE_ID must skip the hand-off", got.bin, f.real)
	}
}

// CMUX_CLAUDE_WRAPPER_SHIM missing, a directory, non-executable, or chottag
// itself must all skip the hand-off and fall through to the normal path.
func TestNoHandoffWhenWrapperShimIsNotAUsableExecutable(t *testing.T) {
	selfBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		makeVal func(t *testing.T, home string) string
	}{
		{
			name: "missing",
			makeVal: func(t *testing.T, home string) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
		},
		{
			name: "directory",
			makeVal: func(t *testing.T, home string) string {
				return t.TempDir()
			},
		},
		{
			name: "non-executable",
			makeVal: func(t *testing.T, home string) string {
				path := filepath.Join(t.TempDir(), "cmux-claude-wrapper")
				if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "is chottag itself",
			makeVal: func(t *testing.T, home string) string {
				if runtime.GOOS == "windows" {
					t.Skip("symlinks need elevation on windows")
				}
				selfDir := filepath.Join(home, "bin")
				if err := os.MkdirAll(selfDir, 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(selfDir, "claude")
				if err := os.Symlink(selfBin, link); err != nil {
					t.Fatal(err)
				}
				return link
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCmuxHandoffFixture(t)
			shimVal := tc.makeVal(t, f.home)

			var got execCall
			restore := swapExec(f.home, &got)
			defer restore()

			code := Run(nil, f.home, cmuxTestEnv(shimVal, f.real), ownVersion, io.Discard, io.Discard)

			if code != 0 {
				t.Fatalf("Run = %d, want 0", code)
			}
			if got.bin != f.real {
				t.Errorf("exec'd %q, want the real claude %q: an unusable wrapper shim must skip the hand-off", got.bin, f.real)
			}
		})
	}
}

// CMUX_CLAUDE_HOOKS_DISABLED=1 means the wrapper adds no hooks in this
// mode, so a hand-off would exec it for nothing: no hand-off.
func TestNoHandoffWhenHooksAreDisabled(t *testing.T) {
	f := newCmuxHandoffFixture(t)

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	env := cmuxTestEnv(f.cmuxShim, f.real, "CMUX_CLAUDE_HOOKS_DISABLED=1")
	code := Run(nil, f.home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.real {
		t.Errorf("exec'd %q, want the real claude %q: CMUX_CLAUDE_HOOKS_DISABLED=1 means the wrapper adds nothing", got.bin, f.real)
	}
}

// CMUX_CUSTOM_CLAUDE_PATH naming the real claude directly (cmux's own
// "Claude Binary Path" setting, pointed past chottag) means the wrapper
// never comes back to chottag no matter what: handing off would only exec
// cmux's wrapper for a launch that was always going to skip chottag. The
// value is deliberately padded with spaces: it must be trimmed before use.
func TestNoHandoffWhenCustomClaudePathNamesTheRealClaude(t *testing.T) {
	f := newCmuxHandoffFixture(t)

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	env := cmuxTestEnv(f.cmuxShim, f.real, "CMUX_CUSTOM_CLAUDE_PATH=  "+f.real+"  ")
	code := Run(nil, f.home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.real {
		t.Errorf("exec'd %q, want the real claude %q: CMUX_CUSTOM_CLAUDE_PATH naming the real claude means the wrapper would never come back", got.bin, f.real)
	}
}

// CMUX_CUSTOM_CLAUDE_PATH naming chottag ITSELF is not a bypass: the
// wrapper comes back to chottag exactly as it would with no custom path
// set, so the hand-off is still worthwhile.
func TestHandoffStillAllowedWhenCustomClaudePathNamesChottagItself(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	f := newCmuxHandoffFixture(t)
	selfBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfDir := filepath.Join(f.home, "bin")
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(selfDir, "claude")
	if err := os.Symlink(selfBin, customPath); err != nil {
		t.Fatal(err)
	}

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	env := cmuxTestEnv(f.cmuxShim, f.real, "CMUX_CUSTOM_CLAUDE_PATH="+customPath)
	code := Run(nil, f.home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.cmuxShim {
		t.Errorf("exec'd %q, want cmux's wrapper shim %q: a custom path naming chottag itself is not a bypass", got.bin, f.cmuxShim)
	}
}

// CMUX_CUSTOM_CLAUDE_PATH that is the SAME FILE as CMUX_CLAUDE_WRAPPER_SHIM
// (a different path string, but a symlink to the exact same file) is not a
// bypass either: cmux's own find_real_claude ignores a custom path that
// only points back at its own shim or wrapper, and walks PATH the normal
// way instead — the hand-off is still worthwhile (F243-R2).
func TestHandoffStillAllowedWhenCustomClaudePathIsTheSameFileAsTheWrapperShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	f := newCmuxHandoffFixture(t)
	alias := filepath.Join(t.TempDir(), "cmux-claude-wrapper-alias")
	if err := os.Symlink(f.cmuxShim, alias); err != nil {
		t.Fatal(err)
	}

	var got execCall
	restore := swapExec(f.home, &got)
	defer restore()

	env := cmuxTestEnv(f.cmuxShim, f.real, "CMUX_CUSTOM_CLAUDE_PATH="+alias)
	code := Run(nil, f.home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != f.cmuxShim {
		t.Errorf("exec'd %q, want cmux's wrapper shim %q: a custom path that is the SAME FILE as CMUX_CLAUDE_WRAPPER_SHIM is not a bypass", got.bin, f.cmuxShim)
	}
}

// The hand-off's execFn returns an error: the session must not fail just
// because cmux's own shim is broken. stderr names the fallback, and the
// normal path continues (registering the session and exec'ing the real
// claude) in this same pass, with the SAME complete child environment the
// failed hand-off attempt already had.
func TestHandoffErrorFallsBackToTheNormalPath(t *testing.T) {
	f := newCmuxHandoffFixture(t)

	var got execCall
	callCount := 0
	orig := execFn
	execFn = func(bin string, args, env []string) error {
		callCount++
		if bin == f.cmuxShim {
			return errors.New("boom")
		}
		got.bin = bin
		got.args = append([]string(nil), args...)
		got.env = append([]string(nil), env...)
		if reg, err := session.Open(filepath.Join(f.home, "run")); err == nil {
			got.live, _ = reg.Live()
		}
		return nil
	}
	defer func() { execFn = orig }()

	var stderr bytes.Buffer
	code := Run(nil, f.home, cmuxTestEnv(f.cmuxShim, f.real), ownVersion, io.Discard, &stderr)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if callCount != 2 {
		t.Fatalf("execFn called %d times, want 2 (the failed hand-off, then the normal path)", callCount)
	}
	if got.bin != f.real {
		t.Errorf("exec'd %q, want the real claude %q after the hand-off failed", got.bin, f.real)
	}
	if !slices.Contains(got.env, "HTTPS_PROXY="+f.secret.SessionProxyURL("127.0.0.1:"+strconv.Itoa(f.port), proxyauth.DefaultPool, pinnedSID)) {
		t.Error("the fallback's env is missing HTTPS_PROXY: it must reuse the same child environment the failed hand-off already built, not rebuild a bare one")
	}
	if len(got.live) != 1 || got.live[0].SID != pinnedSID || got.live[0].Pool != proxyauth.DefaultPool {
		t.Errorf("registry at fallback exec = %v, want one entry with the pinned sid and default pool", got.live)
	}
	for _, kv := range got.env {
		if kv == "CHOTTAG_CMUX_HANDOFF=1" {
			t.Error("the fallback's env still carries the hand-off marker: a nested claude must start clean")
		}
	}
	if !bytes.Contains(stderr.Bytes(), []byte("could not hand off to cmux's claude wrapper")) {
		t.Errorf("stderr %q must name the hand-off fallback", stderr.String())
	}
}

// CHOTTAG_BYPASS=1 still wins over the hand-off: the escape hatch exists
// for when everything else, including cmux's own wrapper, is suspect.
func TestBypassStillWinsOverCmuxHandoff(t *testing.T) {
	home := t.TempDir()
	real := fakeClaude(t)
	cmuxShim := writeCmuxShim(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	env := append(cmuxTestEnv(cmuxShim, real), "CHOTTAG_BYPASS=1")
	code := Run(nil, home, env, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != real {
		t.Errorf("exec'd %q, want the real claude %q: CHOTTAG_BYPASS=1 must skip the hand-off entirely", got.bin, real)
	}
}
