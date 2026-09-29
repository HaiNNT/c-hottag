package shim

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// execCall records the arguments a swapped execFn was invoked with.
type execCall struct {
	bin  string
	args []string
	env  []string
	// live is a snapshot of the session registry taken AT THE MOMENT execFn
	// was called — not after Run returns. In production execFn is
	// syscall.Exec, which never returns on success, so the stub is the only
	// observation point from which "was the session registered before
	// exec?" is directly testable without a production seam around
	// session.Add itself (fix round 1, D7).
	live []session.Session
}

// swapExec replaces execFn with a stub that records into got instead of
// exec'ing anything, and returns a func that restores the original. home is
// the registry's parent (Run's own home argument) so the stub can open it;
// session.Open takes no lock (internal/session/registry.go), so reading it
// here is safe.
func swapExec(home string, got *execCall) func() {
	orig := execFn
	execFn = func(bin string, args, env []string) error {
		got.bin = bin
		got.args = append([]string(nil), args...)
		got.env = append([]string(nil), env...)
		if reg, err := session.Open(filepath.Join(home, "run")); err == nil {
			got.live, _ = reg.Live()
		}
		return nil
	}
	return func() { execFn = orig }
}

// swapSpawn replaces spawnFn with fn, and returns a func that restores the
// original.
func swapSpawn(fn func(exe, home, upstream string) error) func() {
	orig := spawnFn
	spawnFn = fn
	return func() { spawnFn = orig }
}

// fakeClaude writes a 0755 script standing in for the real `claude` binary.
// It is a path only: with execFn stubbed in every test, it is never run.
func fakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// secretOf loads (or creates) home's proxy secret, so a test's fake daemon
// (provingHealthServer, identity_test.go) can prove it holds the exact
// value shim.Run itself will load for that home.
func secretOf(t *testing.T, home string) proxyauth.Secret {
	t.Helper()
	s, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mustPort parses the TCP port a test server is listening on out of its URL.
func mustPort(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// writeStateWithPort persists a state.json under home naming port, so the
// shim under test finds the fake daemon at the port a test server actually
// bound (never a fixed or privileged port).
func writeStateWithPort(t *testing.T, home string, port int) {
	t.Helper()
	if _, err := (store.Store{Dir: home}).Update(func(s *store.State) error {
		s.Port = port
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMain makes reaching the real spawnDaemon OR the real syscallExec
// structurally impossible to do by accident: their defaults panic instead
// of forking a Setsid-detached copy of this test binary (fix round 1, D5 —
// three tests previously reached spawnDaemon for real on any health probe
// stall over 500ms, with no test asserting anything about it) or really
// syscall.Exec'ing this machine's claude. Tests that legitimately exercise
// either path opt in with swapSpawn/swapExec, as they already do.
//
// execFn's guard was added in fix round 3 (D7/F-H): every one of this
// package's own Run( call sites already called swapExec, so the gap was
// latent rather than live, but an unstubbed irreversible seam is a
// landmine that only goes off under timing (F116's ruling) — the next test
// added to this package must not be the one that finds it live. Without
// this, internal/cli's own TestMain doc comment calling this "identical"
// (cli.go, invariants_test.go) would keep being false as written, the
// "conclusion right, stated reason false" defect class this project
// already has a name for.
func TestMain(m *testing.M) {
	execFn = func(string, []string, []string) error {
		panic("execFn reached: a test that can reach the exec path must stub it with swapExec")
	}
	spawnFn = func(string, string, string) error {
		panic("spawnFn reached: a test that can reach the spawn path must stub it with swapSpawn")
	}
	// This test binary is very often run from inside a real cmux surface
	// (F243, fix round 1 F243-R1): every one of these six variables is
	// then genuinely present in os.Environ(), which daemonSpawnEnv (above)
	// reads directly, and which a future test could pass through some
	// other seam without noticing. Unset process-wide, the same reasoning
	// as internal/cli's own TestMain, so no test in this package can ever
	// hand off, or observe a cmux variable leaking through, for real.
	os.Unsetenv("CMUX_SURFACE_ID")
	os.Unsetenv("CMUX_CLAUDE_WRAPPER_SHIM")
	os.Unsetenv("CMUX_CLAUDE_PID")
	os.Unsetenv("CMUX_CLAUDE_HOOKS_DISABLED")
	os.Unsetenv("CMUX_CUSTOM_CLAUDE_PATH")
	os.Unsetenv("CHOTTAG_CMUX_HANDOFF")
	os.Exit(m.Run())
}

// CHOTTAG_BYPASS must work before anything else — it is the escape hatch for
// when the daemon, state.json or the CA are broken, so it cannot depend on
// any of them.
func TestBypassExecsTheRealClaudeWithoutTouchingTheDaemon(t *testing.T) {
	home := t.TempDir()
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	code := Run([]string{"--version"}, home,
		[]string{"CHOTTAG_BYPASS=1", "PATH=" + filepath.Dir(real)},
		ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != real {
		t.Errorf("exec'd %q, want the real claude %q", got.bin, real)
	}
	for _, e := range got.env {
		if strings.HasPrefix(e, "HTTPS_PROXY=") {
			t.Errorf("bypass set %q: it must not route through the daemon", e)
		}
	}
}

// The bypass check must run before state.json is even read: a corrupt
// state.json must not be able to defeat the escape hatch. This pins the
// ORDER, not just the outcome — a state.json load that happens to succeed
// on an empty temp dir would not catch a bypass check moved below it.
func TestBypassWorksEvenWhenStateJSONIsUnreadable(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	code := Run([]string{"--version"}, home,
		[]string{"CHOTTAG_BYPASS=1", "PATH=" + filepath.Dir(real)},
		ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if got.bin != real {
		t.Errorf("exec'd %q, want the real claude %q: bypass must not depend on state.json loading cleanly", got.bin, real)
	}
}

// A listener that answers with something else is NOT our daemon. This is a
// different code path from "nothing answers", and it is the token-leak case
// the health endpoint exists for: HTTPS_PROXY must never be pointed at it.
func TestFailsClosedWhenTheHealthDocumentIsWrong(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"chottag":false,"version":"impostor"}` + "\n"))
	}))
	defer foreign.Close()
	port := mustPort(t, foreign.URL)

	home := t.TempDir()
	writeStateWithPort(t, home, port)
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()
	var spawnCount int
	var spawnUpstream string
	noSpawn := swapSpawn(func(exe, h, upstream string) error {
		spawnCount++
		spawnUpstream = upstream
		return nil
	})
	defer noSpawn()

	var stderr bytes.Buffer
	code := Run([]string{"--version"}, home,
		[]string{"PATH=" + filepath.Dir(real), "HTTPS_PROXY=http://corp:8080"}, ownVersion, io.Discard, &stderr)

	if code == 0 {
		t.Fatal("Run succeeded against a foreign listener, want a failure")
	}
	if got.bin != "" {
		t.Errorf("exec'd %q: the shim must not run claude when it cannot confirm the daemon", got.bin)
	}
	if !strings.Contains(stderr.String(), "chottag daemon run") {
		t.Errorf("error %q should name `chottag daemon run` so the operator can see the real startup error", stderr.String())
	}
	// D4: the shim must actually try to start the daemon when the probe
	// does not confirm — deleting the spawnFn call left the whole package
	// green before this assertion existed.
	if spawnCount != 1 {
		t.Errorf("spawnFn called %d times, want 1: the shim must start the daemon when the probe does not confirm", spawnCount)
	}
	// D2: the spawned daemon must be told this shell's upstream, or the
	// mismatch message's own remedy ("stop it, it will restart with the
	// new one") can never come true for a corporate-proxy user.
	if spawnUpstream != "http://corp:8080" {
		t.Errorf("spawnFn upstream = %q, want the shell's HTTPS_PROXY passed through", spawnUpstream)
	}
}

// A shellProxy that already names this daemon's own loopback address must
// never be forwarded to a freshly-spawned daemon as its upstream: doing so
// would make the daemon its own upstream, dialling itself for every
// request. This is round 1's D3 (the "not a conflict" ruling) applied to
// the spawn path too — TestFailsClosedWhenTheHealthDocumentIsWrong above
// already pins that a genuine upstream is passed through unchanged (fix
// round 2, D1).
func TestSpawnedUpstreamExcludesOurOwnAddress(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"chottag":false}` + "\n"))
	}))
	defer foreign.Close()
	port := mustPort(t, foreign.URL)

	home := t.TempDir()
	writeStateWithPort(t, home, port)
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	var spawnCalled bool
	var spawnUpstream string
	noSpawn := swapSpawn(func(exe, h, upstream string) error {
		spawnCalled = true
		spawnUpstream = upstream
		return nil
	})
	defer noSpawn()

	ourProxy := "HTTPS_PROXY=http://127.0.0.1:" + strconv.Itoa(port)
	Run([]string{"--version"}, home, []string{"PATH=" + filepath.Dir(real), ourProxy}, ownVersion, io.Discard, io.Discard)

	if !spawnCalled {
		t.Fatal("spawnFn was never called")
	}
	if spawnUpstream != "" {
		t.Errorf("spawnFn upstream = %q, want \"\": a shellProxy already naming this daemon's own address must not be forwarded as an upstream", spawnUpstream)
	}
}

// A pre-existing HTTPS_PROXY that disagrees with the running daemon's
// upstream is fatal: the daemon takes an upstream only at startup, so
// proceeding would route this shell's traffic through an upstream the
// operator did not ask for.
func TestFailsClosedOnUpstreamMismatch(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid(), Upstream: "http://corp-a:8080"})
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	var stderr bytes.Buffer
	code := Run([]string{"--version"}, home,
		[]string{"PATH=" + filepath.Dir(real), "HTTPS_PROXY=http://corp-b:8080"},
		ownVersion, io.Discard, &stderr)

	if code == 0 || got.bin != "" {
		t.Fatalf("Run = %d, exec'd %q: an upstream mismatch must fail closed", code, got.bin)
	}
	for _, want := range []string{"corp-a", "corp-b"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("error %q must name both upstreams so the operator can see the conflict", stderr.String())
		}
	}
}

// A userinfo password in either side of the mismatch message must never
// reach stderr, while the message stays useful (both hosts still appear).
// This is the project's hardest rule ("never log, print or persist
// tokens") applied to a value this same message used to print with %q
// verbatim (fix round 1, D1).
func TestUpstreamMismatchMessageRedactsCredentials(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid(), Upstream: "http://svc:daemonSecret123@corp-a:8080"})
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	var stderr bytes.Buffer
	code := Run(nil, home,
		[]string{"PATH=" + filepath.Dir(real), "HTTPS_PROXY=http://bob:hunter2@corp-b:8080"},
		ownVersion, io.Discard, &stderr)

	if code == 0 || got.bin != "" {
		t.Fatalf("Run = %d, exec'd %q: an upstream mismatch must still fail closed", code, got.bin)
	}
	for _, leaked := range []string{"hunter2", "daemonSecret123"} {
		if strings.Contains(stderr.String(), leaked) {
			t.Errorf("stderr %q leaked a credential %q", stderr.String(), leaked)
		}
	}
	for _, want := range []string{"corp-a", "corp-b"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q must still name both hosts so the message stays useful", stderr.String())
		}
	}
}

// A shellProxy that already names THIS daemon's own loopback address —
// exactly what `chottag trace`'s export snippet sets, and what any nested
// `claude` inherits — is not a conflicting upstream at all: it is already
// configured. TestFailsClosedOnUpstreamMismatch above pins the OTHER
// direction: a genuinely different upstream still fails (fix round 1, D3).
func TestSelfAddressedHTTPSProxyIsNotAConflict(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid(), Upstream: "http://corp-a:8080"})
	defer daemon.Close()

	port := mustPort(t, daemon.URL)
	writeStateWithPort(t, home, port)
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	ourProxy := "HTTPS_PROXY=http://127.0.0.1:" + strconv.Itoa(port)
	code := Run(nil, home, []string{"PATH=" + filepath.Dir(real), ourProxy}, ownVersion, io.Discard, io.Discard)

	if code != 0 {
		t.Fatalf("Run = %d, want 0: HTTPS_PROXY already pointed at this daemon's own address must not be treated as a conflict", code)
	}
	if got.bin != real {
		t.Errorf("exec'd %q, want %q", got.bin, real)
	}
}

// TestUnreducibleHTTPSProxyIsAConflictEvenAgainstNoUpstream pins fix round
// 4's D1: a scheme-less HTTPS_PROXY like "corp-proxy:8080" (the shape
// http_proxy and `curl -x` accept, F81's documented example of what users
// actually type) is exactly the shape redact.WithoutUserinfo cannot reduce
// (net/url reads "corp-proxy" as the scheme, not a host, so it lands in
// u.Opaque) — it reduces to "", which is indistinguishable from "no
// upstream configured" unless the shim treats "cannot reduce" as its own
// case. Before this fix, that made a daemon with no upstream at all look
// like agreement, and the shim proceeded — silently routing this shell's
// traffic past the one check that exists to stop that.
func TestUnreducibleHTTPSProxyIsAConflictEvenAgainstNoUpstream(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()}) // no Upstream: reports ""
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	var stderr bytes.Buffer
	code := Run(nil, home, []string{"PATH=" + filepath.Dir(real), "HTTPS_PROXY=corp-proxy:8080"}, ownVersion, io.Discard, &stderr)

	if code == 0 || got.bin != "" {
		t.Fatalf("Run = %d, exec'd %q: an unreducible HTTPS_PROXY must fail closed even against a daemon reporting no upstream", code, got.bin)
	}
	if strings.Contains(stderr.String(), "corp-proxy:8080") {
		// Not a credential in this particular value, but the message must
		// still go through redact.UpstreamProxy rather than %q-ing the raw
		// value, so a future credentialed scheme-less value stays covered.
		t.Errorf("stderr %q printed the raw HTTPS_PROXY instead of a redacted form", stderr.String())
	}
}

// TestEmptyHTTPSProxyStillAgreesWithNoUpstream is the other direction
// TestUnreducibleHTTPSProxyIsAConflictEvenAgainstNoUpstream's fix must not
// break: a genuinely empty HTTPS_PROXY reduces to "" too, but that really
// is agreement with a daemon reporting no upstream, not an unreducible
// value — Run must still proceed.
func TestEmptyHTTPSProxyStillAgreesWithNoUpstream(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()}) // no Upstream: reports ""
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	code := Run(nil, home, []string{"PATH=" + filepath.Dir(real)}, ownVersion, io.Discard, io.Discard) // no HTTPS_PROXY at all

	if code != 0 {
		t.Fatalf("Run = %d, want 0: a genuinely empty HTTPS_PROXY must still agree with a daemon reporting no upstream", code)
	}
	if got.bin != real {
		t.Errorf("exec'd %q, want %q", got.bin, real)
	}
}

// An already-confirmed daemon must not be (re)started — the counterpart to
// D4's "spawnFn is called when the probe does not confirm" assertion in
// TestFailsClosedWhenTheHealthDocumentIsWrong.
func TestConfirmedDaemonDoesNotSpawn(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()})
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	var spawned int
	noSpawn := swapSpawn(func(string, string, string) error { spawned++; return nil })
	defer noSpawn()

	if code := Run(nil, home, []string{"PATH=" + filepath.Dir(real)}, ownVersion, io.Discard, io.Discard); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}
	if spawned != 0 {
		t.Errorf("spawnFn called %d times, want 0: an already-confirmed daemon must not be (re)started", spawned)
	}
}

// The happy path: env set, session registered, real claude exec'd.
func TestSetsEnvRegistersTheSessionAndExecs(t *testing.T) {
	home := t.TempDir()
	s := secretOf(t, home)
	daemon := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()})
	defer daemon.Close()

	port := mustPort(t, daemon.URL)
	writeStateWithPort(t, home, port)
	real := fakeClaude(t)

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	if code := Run([]string{"--resume"}, home,
		[]string{"PATH=" + filepath.Dir(real)}, ownVersion, io.Discard, io.Discard); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}

	if got.bin != real {
		t.Errorf("exec'd %q, want %q", got.bin, real)
	}
	if want := []string{"--resume"}; !slices.Equal(got.args, want) {
		t.Errorf("args = %v, want %v: the original arguments must be passed through", got.args, want)
	}
	wantProxy := "HTTPS_PROXY=" + s.ProxyURL("127.0.0.1:"+strconv.Itoa(port))
	// The failure message below never prints got.env or wantProxy
	// themselves (fix round 1 item 7): both may carry the secret, and a
	// test failure's own output is not exempt from "never print it".
	if !slices.Contains(got.env, wantProxy) {
		t.Errorf("HTTPS_PROXY = %s, want the secret URL", redact.UpstreamProxy(envGet(got.env, "HTTPS_PROXY")))
	}
	// No pre-existing bundle, so NODE_EXTRA_CA_CERTS points straight at
	// ca.pem and no bundle file is written (§4.3 step 5).
	wantCA := "NODE_EXTRA_CA_CERTS=" + filepath.Join(home, "ca", "ca.pem")
	if !slices.Contains(got.env, wantCA) {
		t.Errorf("env missing %q; HTTPS_PROXY = %s", wantCA, redact.UpstreamProxy(envGet(got.env, "HTTPS_PROXY")))
	}
	if _, err := os.Stat(filepath.Join(home, "ca", "bundle.pem")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("bundle.pem was written with no pre-existing NODE_EXTRA_CA_CERTS: the common path must add no file")
	}

	// got.live is a snapshot of the registry taken INSIDE the execFn stub —
	// the moment of exec itself — not a re-open after Run returns: in
	// production execFn (syscall.Exec) never returns on success, so this is
	// the only point from which "was it registered before exec?" is
	// directly observable (fix round 1, D7).
	if len(got.live) != 1 || got.live[0].PID != os.Getpid() {
		t.Errorf("registry at exec time = %v, want this pid registered before exec (the exec'd process INHERITS this pid and becomes claude)", got.live)
	}
}

// A user's own NODE_EXTRA_CA_CERTS is MERGED, never replaced.
func TestMergesAPreExistingCABundle(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()})
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	// The shim points at the CA, it does not create one — setup and the
	// daemon do. So the test must make it exist before asserting a merge.
	if _, err := ca.LoadOrCreate(filepath.Join(home, "ca")); err != nil {
		t.Fatal(err)
	}

	theirs := filepath.Join(t.TempDir(), "corp.pem")
	if err := os.WriteFile(theirs, []byte("-----BEGIN CERTIFICATE-----\ncorp\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got execCall
	restore := swapExec(home, &got)
	defer restore()

	if code := Run(nil, home,
		[]string{"PATH=" + filepath.Dir(real), "NODE_EXTRA_CA_CERTS=" + theirs},
		ownVersion, io.Discard, io.Discard); code != 0 {
		t.Fatalf("Run = %d, want 0", code)
	}

	bundle := filepath.Join(home, "ca", "bundle.pem")
	if !slices.Contains(got.env, "NODE_EXTRA_CA_CERTS="+bundle) {
		t.Fatalf("env should point at the merged bundle %q; HTTPS_PROXY = %s", bundle, redact.UpstreamProxy(envGet(got.env, "HTTPS_PROXY")))
	}
	b, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "corp") {
		t.Error("merged bundle dropped the user's own CA: it must be merged, never replaced")
	}
	ours, err := os.ReadFile(filepath.Join(home, "ca", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), strings.TrimSpace(string(ours))) {
		t.Error("merged bundle is missing chottag's own CA")
	}
}

// TestReusesAnAlreadyMergedCABundleWithoutMergingAgain pins F243-R2: a
// SECOND Run call whose own NODE_EXTRA_CA_CERTS already names a bundle a
// FIRST Run call wrote must reuse that file as-is, not merge it again. This
// is exactly what a cmux hand-off's second pass inherits: cmux's wrapper
// passes the environment it was exec'd with straight through, so pass 1's
// own already-merged NODE_EXTRA_CA_CERTS comes back unchanged as pass 2's —
// merging it again would concatenate chottag's own CA into the bundle a
// second time, on every single cmux launch rather than a one-off.
func TestReusesAnAlreadyMergedCABundleWithoutMergingAgain(t *testing.T) {
	home := t.TempDir()
	daemon := provingHealthServer(t, secretOf(t, home), proxy.Health{Chottag: true, Version: "test", PID: os.Getpid()})
	defer daemon.Close()

	writeStateWithPort(t, home, mustPort(t, daemon.URL))
	real := fakeClaude(t)

	if _, err := ca.LoadOrCreate(filepath.Join(home, "ca")); err != nil {
		t.Fatal(err)
	}

	theirs := filepath.Join(t.TempDir(), "corp.pem")
	if err := os.WriteFile(theirs, []byte("-----BEGIN CERTIFICATE-----\ncorp\n-----END CERTIFICATE-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pass 1: builds the merged bundle, exactly like
	// TestMergesAPreExistingCABundle above.
	var pass1 execCall
	restore := swapExec(home, &pass1)
	if code := Run(nil, home,
		[]string{"PATH=" + filepath.Dir(real), "NODE_EXTRA_CA_CERTS=" + theirs},
		ownVersion, io.Discard, io.Discard); code != 0 {
		restore()
		t.Fatalf("pass 1: Run = %d, want 0", code)
	}
	restore()

	bundle := filepath.Join(home, "ca", "bundle.pem")
	before, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}

	// Pass 2: exactly what cmux's wrapper would hand back untouched — the
	// SAME env pass 1 ended up with, whose own NODE_EXTRA_CA_CERTS is now
	// the bundle pass 1 just wrote.
	var pass2 execCall
	restore = swapExec(home, &pass2)
	defer restore()
	if code := Run(nil, home,
		[]string{"PATH=" + filepath.Dir(real), "NODE_EXTRA_CA_CERTS=" + bundle},
		ownVersion, io.Discard, io.Discard); code != 0 {
		t.Fatalf("pass 2: Run = %d, want 0", code)
	}

	if !slices.Contains(pass2.env, "NODE_EXTRA_CA_CERTS="+bundle) {
		t.Fatalf("pass 2 env should still point at the SAME bundle %q, not build a new one; got %q", bundle, envGet(pass2.env, "NODE_EXTRA_CA_CERTS"))
	}
	after, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("pass 2 rewrote the bundle (before %q, after %q): it must be reused as-is, never merged again", before, after)
	}
	ours, err := os.ReadFile(filepath.Join(home, "ca", "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(after), strings.TrimSpace(string(ours))); n != 1 {
		t.Errorf("chottag's own CA appears %d times in the bundle after pass 2, want exactly 1", n)
	}
}
