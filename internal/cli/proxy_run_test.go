package cli

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// TestProxyRunPrintsExportLinesBeforeServing checks the one piece of output
// a user actually needs from `proxy run`: the two `export` lines, printed
// before it blocks serving on the loopback listener, naming the address the
// daemon actually resolved to — not the literal --listen flag value.
// --listen 127.0.0.1:0 makes those two disagree unless the daemon resolves
// its own listener address first (F3): the OS assigns an ephemeral port,
// so a build that printed the flag verbatim would tell a user to `export
// HTTPS_PROXY=http://127.0.0.1:0`, and pasting that into a shell would
// point every request at a wildcard port nothing is actually listening on.
//
// Drives runProxyWithSignal directly (not cli.Run) with an injected signal
// channel: the daemon this starts never returns on its own, and t.Cleanup
// sends it a shutdown signal on that channel so nothing real OS-signal-
// handling stays armed inside the test binary after this test ends (N3) —
// a real signal.Notify registration that outlives its test can swallow the
// FIRST real Ctrl-C sent to a later `go test` run, and (given a second,
// rapid one) reach the real os.Exit(130), with the stderr line explaining
// why going into a buffer nobody ever prints. The cleanup WAITS for the
// daemon to actually finish shutting down, not just for the signal send to
// complete: t.TempDir()'s own cleanup (removing CHOTTAG_HOME) is
// registered before this one and so runs after it (cleanups are LIFO), and
// racing it against the daemon's still-in-flight shutdown I/O intermittently
// failed the test with "directory not empty" before this waited.
func TestProxyRunPrintsExportLinesBeforeServing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	out := newSyncBuf()
	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	// runProxyWithSignal, unlike cli.Run, takes args with "proxy" already
	// stripped — cli.Run's own dispatch (internal/cli/cli.go) is what
	// strips it before calling runProxy.
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0"}, out, errb, nil, sig)
	}()

	select {
	case <-out.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to print the export lines")
	}
	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to print the listening line")
	}

	const marker = "chottag proxy listening on "
	errs := errb.String()
	i := strings.Index(errs, marker)
	if i < 0 {
		t.Fatalf("stderr = %q, want it to name the resolved listen address", errs)
	}
	rest := errs[i+len(marker):]
	addr := rest[:strings.IndexByte(rest, ',')]
	if addr == "" || addr == "127.0.0.1:0" {
		t.Fatalf("resolved addr = %q, want a real ephemeral port, not the wildcard flag value", addr)
	}

	want := "export HTTPS_PROXY=\"http://chottag:$(cat \"" + proxyauth.Path(home) + "\")@" + addr + "\"\nexport NODE_EXTRA_CA_CERTS=\"" + filepath.Join(home, "ca", "ca.pem") + "\"\n"
	if got := out.String(); got != want {
		t.Fatalf("export lines = %q, want %q", got, want)
	}
}

// TestRunProxyWithSignalLogEmptyDisablesLoggingEndToEnd pins F68's privacy
// off-switch through the real runProxyWithSignal path, not just
// openTraceLog in isolation: --log "" must reach a nil *tracelog.Writer
// through runProxyWithSignal's own `if lw != nil { defer lw.Close() }`
// guard (internal/cli/proxy.go) without a nil-pointer panic on shutdown,
// and no proxy.jsonl must ever appear in CHOTTAG_HOME. Before this test
// existed, an unguarded `defer lw.Close()` on that exact path panicked
// with the rest of the suite green (review finding, M1c4 Task 3 round 1).
func TestRunProxyWithSignalLogEmptyDisablesLoggingEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	sig := make(chan os.Signal, 2)

	codeCh := make(chan int, 1)
	var stopped atomic.Bool
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}

	if got := errb.String(); !strings.Contains(got, "chottag proxy listening on") || !strings.Contains(got, "request logging disabled") {
		t.Fatalf("stderr = %q, want a startup line naming the address and saying request logging is disabled", got)
	}
	if _, err := os.Stat(filepath.Join(home, "proxy.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("proxy.jsonl stat = %v, want it to not exist: --log \"\" must disable the trace log entirely", err)
	}

	sig <- os.Interrupt
	select {
	case code := <-codeCh:
		stopped.Store(true)
		if code != 0 {
			t.Fatalf("code = %d, want 0 on a clean shutdown", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runProxyWithSignal did not return after a signal")
	}
}

// TestRunProxyWithSignalActuallyStopsOnASignal closes a gap
// TestProxyRunPrintsExportLinesBeforeServing cannot see: that test only
// waits for the export lines and then leaves its daemon running
// (cleaned up in t.Cleanup, never observed to actually stop), so it
// cannot tell a real ctx from context.Background() at runProxy's own call
// site — `runDaemon(context.Background(), ...)` there, distinct from
// installShutdown's own internal ctx-vs-Background (already covered by
// TestInstallShutdownCancelsRunsContextAndPropagatesItsCode), survived a
// fully green suite until this test existed. This one sends the signal
// itself and requires runProxyWithSignal to actually return.
func TestRunProxyWithSignalActuallyStopsOnASignal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	sig := make(chan os.Signal, 2)

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0"}, io.Discard, errb, nil, sig)
	}()
	// F6b: an early t.Fatal below (e.g. the daemon never starts listening)
	// must not leak this goroutine's daemon into a t.TempDir() that is
	// about to be removed — the same shape TestProxyRunPrintsExportLinesBeforeServing
	// already guards against. stopped tracks whether the main body already
	// drained codeCh: once it has, codeCh is empty for good (the goroutine
	// above sends to it exactly once), so the cleanup must not peek it
	// again to decide whether a signal is still owed — it would always
	// see it empty and send a signal nothing will ever answer, then hang
	// out its own bound waiting on a codeCh that will never receive again.
	var stopped atomic.Bool
	t.Cleanup(func() {
		if stopped.Load() {
			return
		}
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}

	sig <- os.Interrupt

	select {
	case code := <-codeCh:
		stopped.Store(true)
		if code != 0 {
			t.Fatalf("code = %d, want 0 on a clean shutdown", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runProxyWithSignal did not return after a signal — is runDaemon actually wired to installShutdown's ctx, or to context.Background()?")
	}
}

// TestRunProxyWithSignalClosesOwnersWhenNewStatusSinkFails pins the
// own.Close() call on runProxyWithSignal's newStatusSink-failure path: own
// is already open by the time newStatusSinkFn is called, and deleting the
// own.Close() on that branch left the whole package green (F71's shape —
// see that call's own comment). newStatusSinkFn and ownersOpenFn
// (proxy.go) are overridden so this drives the real function on the real
// failure branch: newStatusSinkFn is forced to fail (no real disk failure
// needed), and ownersOpenFn is wrapped only to capture the exact *owners.Map
// runProxyWithSignal opens, the same handle own.Close() is (or is not)
// called on — runProxyWithSignal builds its own owners.Map internally
// rather than taking one as a parameter, so it cannot be injected directly
// the way runDaemon's daemonDeps.Owners is.
//
// The probe itself mirrors TestRunDaemonDrainsOwnersWhenListenFails
// exactly: a write against the captured Map after runProxyWithSignal
// returns must report owners.ErrClosed, which is only possible if Close
// actually ran (owners.Map's own contract — see its Close doc comment).
func TestRunProxyWithSignalClosesOwnersWhenNewStatusSinkFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)

	origSink := newStatusSinkFn
	newStatusSinkFn = func(home string, onError func(error)) (*statusSink, error) {
		return nil, errors.New("forced newStatusSink failure")
	}
	t.Cleanup(func() { newStatusSinkFn = origSink })

	var captured *owners.Map
	origOpen := ownersOpenFn
	ownersOpenFn = func(path string) (*owners.Map, error) {
		m, err := owners.Open(path)
		if err == nil {
			captured = m
		}
		return m, err
	}
	t.Cleanup(func() { ownersOpenFn = origOpen })

	stderr := newSyncBuf()
	// sig is nil but never matters: this failure branch returns before
	// runProxyWithSignal ever calls installShutdown, so no real
	// signal.Notify is registered — confirmed by reading the function in
	// order, not assumed.
	code := runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0"}, io.Discard, stderr, nil, nil)
	if code != 1 {
		t.Fatalf("runProxyWithSignal returned %d, want 1 on a forced newStatusSink failure", code)
	}
	if captured == nil {
		t.Fatal("ownersOpenFn was never called — did runProxyWithSignal fail before reaching owners.Open?")
	}

	if err := captured.Record(router.KindArtifact, []string{"x"}, "A", time.Now()); !errors.Is(err, owners.ErrClosed) {
		t.Fatalf("own.Record after runProxyWithSignal returned = %v, want owners.ErrClosed: own.Close() must run on the newStatusSink-failure path, or the writer goroutine own.Open started leaks silently on every exit through here", err)
	}

	if got := stderr.String(); !strings.Contains(got, "forced newStatusSink failure") {
		t.Fatalf("stderr = %q, want it to report the forced failure", got)
	}
}

// TestRunProxyExitsWithCode130OnASecondSignal pins F5b: nothing else in
// this file ever executes runProxyWithSignal's production forceExit
// closure (every other test drives awaitShutdownSignal/installShutdown
// directly with its own fake forceExit), so os.Exit(130) → os.Exit(1)
// there survives a fully green suite. os.Exit cannot be exercised from
// inside this test process without killing the whole `go test` run, so
// this re-execs the test binary as a subprocess instead — the standard
// self-reexec pattern for testing an os.Exit call site (as os/exec's own
// tests do): the child actually reaches the real forceExit closure and
// the real os.Exit(130), and the parent only inspects its exit code.
func TestRunProxyExitsWithCode130OnASecondSignal(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunProxyExitsWithCode130OnASecondSignalHelper$")
	cmd.Env = append(os.Environ(), "CHOTTAG_T8_HELPER=1", "CHOTTAG_HOME="+home)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper process error = %v, want an *exec.ExitError carrying a non-zero exit code; output:\n%s", err, out)
	}
	if code := exitErr.ExitCode(); code != 130 {
		t.Fatalf("helper process exit code = %d, want 130 (128+SIGINT); output:\n%s", code, out)
	}
}

// TestRunProxyExitsWithCode130OnASecondSignalHelper is not a real test:
// it skips immediately unless CHOTTAG_T8_HELPER is set, so it does
// nothing under a normal `go test` run. TestRunProxyExitsWithCode130OnASecondSignal
// above re-execs the test binary with that variable set and -test.run
// scoped to just this function's name.
//
// It drives runProxyWithSignal with an INJECTED sig channel rather than a
// real OS signal: sending on that channel reaches exactly the same
// production forceExit closure (defined inline in runProxyWithSignal,
// independent of where sig itself came from), without also having to
// race a real signal.Notify registration inside a freshly-forked
// process — a real SIGINT was tried first and found genuinely racy for a
// different reason than the one this test targets: srv.Shutdown does not
// wait on hijacked CONNECT tunnels at all (serve's own doc comment says
// so — that is why serve calls closeConns itself, separately), so holding
// one open does not slow the first signal's shutdown down, and it was
// already finished — signal.Stop already called — well before a
// deliberately-delayed second real signal arrived, which this process's
// default SIGINT disposition then killed outright.
//
// A real, in-flight, NON-hijacked request is what srv.Shutdown DOES wait
// for (up to shutdownGrace): an absolute-form GET routed at this proxy's
// own listener, aimed at a "black hole" TCP listener that accepts the
// connection and never answers, keeps runDaemon's serve() genuinely busy
// long enough for both signals to land while run() is still going.
func TestRunProxyExitsWithCode130OnASecondSignalHelper(t *testing.T) {
	if os.Getenv("CHOTTAG_T8_HELPER") == "" {
		t.Skip("only runs as a subprocess of TestRunProxyExitsWithCode130OnASecondSignal")
	}
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := blackhole.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			_ = c // accepted and held open, never answered, never closed
		}
	}()

	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	sig := make(chan os.Signal, 2)
	go runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0"}, io.Discard, errb, nil, sig)

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	const marker = "chottag proxy listening on "
	errs := errb.String()
	i := strings.Index(errs, marker)
	if i < 0 {
		t.Fatalf("stderr = %q, want it to name the listen address", errs)
	}
	rest := errs[i+len(marker):]
	addr := rest[:strings.IndexByte(rest, ',')]

	// CHOTTAG_HOME (set on this subprocess's env above) is unset again by
	// this test binary's own TestMain before m.Run(), same as every other
	// test here: runProxyWithSignal actually resolved home() to $HOME's
	// (TestMain's throwaway) .chottag, not the parent's home var, so this
	// must resolve it the same way rather than reading CHOTTAG_HOME back.
	h, err := home()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := proxyauth.Load(h)
	if err != nil {
		t.Fatal(err)
	}
	// secret.ProxyURL carries the real on-disk install secret as userinfo
	// (F221/T10): the daemon now gates every absolute-form request on it,
	// exactly as claude's own HTTPS_PROXY does, and net/http's Transport
	// turns that userinfo into the Proxy-Authorization header itself.
	pu, err := url.Parse(secret.ProxyURL(addr))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	// Fire-and-forget: this request never gets a response, so it stays
	// in flight for as long as the daemon does — exactly what keeps
	// srv.Shutdown below from returning instantly.
	go client.Get("http://" + blackhole.Addr().String() + "/")
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the black-hole request never reached the daemon's own forwarding dial")
	}

	sig <- os.Interrupt
	sig <- os.Interrupt

	// Correct code exits the process from inside forceExit before this
	// point is ever reached; if it does not (the mutation this test
	// exists to catch), fall through and exit distinctly so the parent
	// sees an observably wrong code, never a false PASS.
	time.Sleep(shutdownGrace + 5*time.Second)
	os.Exit(99)
}

// TestRunProxyWiresOwnersSaveErrorThrottleIntoOwnSetOnError pins F72's
// wiring: `own.SetOnError(ownersSaveErrorThrottle(stderr))` is the ONE
// production call site (`grep -rn SetOnError internal/cli/*.go` finds
// exactly it, in runProxyWithSignal) — deleting that line, or reusing
// logErrorThrottle in its place (the trace log's message, which was the
// brief's original, wrong instruction), both left the whole suite green
// before this test existed.
//
// It drives real runProxyWithSignal end to end — the wiring lives there,
// not in runDaemon, so nothing that constructs its own *owners.Map
// (as every other test in this file does) can see it. owners.json is
// pre-seeded on disk with a real record for "A" (own.SetOnError only
// catches a write FAILURE, and Forget is a no-op unless the account
// actually owns something), the roster is set up so the test's own diff
// tick (injectRosterTicks) forgets "A", and — after the baseline tick
// (never a diff) — the home directory is made unwritable so the write the
// diff tick queues genuinely fails, taking the real onError path rather
// than asserting on a hand-built one.
func TestRunProxyWiresOwnersSaveErrorThrottleIntoOwnSetOnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)

	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "A")}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "B")}); err != nil {
			return err
		}
		st.Serving, st.Remote = "B", "B" // so Remove("A") below is allowed
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Seed owners.json with a real record for "A" before the daemon ever
	// opens it, since nothing at startup calls Record and this test does
	// not drive real proxied traffic.
	seed, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Record(router.KindArtifact, []string{"artifact-1"}, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	seed.Close() // flushes to disk; runProxy opens a fresh *owners.Map below

	ticks := injectRosterTicks(t)
	errb := newSyncBuf() // "chottag proxy listening on <addr>, ..." is its first write
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	var stopped atomic.Bool
	t.Cleanup(func() {
		os.Chmod(home, 0o700) // restore before t.TempDir()'s own cleanup removes it
		if stopped.Load() {
			return
		}
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}

	// The first tick is the baseline (known={A,B}); it never diffs.
	ticks.tick("baseline")
	if _, err := s.Update(func(st *store.State) error { return st.Remove("A") }); err != nil {
		t.Fatal(err)
	}
	// Home must stay writable for the state.json write just above; only
	// now does it need to start failing the owners.json write the next
	// tick queues.
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}

	ticks.tick("diff") // forgets A; the owners.json write it queues fails

	const want = "chottag: owners.json write failed:"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if strings.Contains(errb.String(), want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stderr = %q, want it to contain %q within 10s of the second roster tick", errb.String(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}

	sig <- os.Interrupt
	select {
	case code := <-codeCh:
		stopped.Store(true)
		_ = code // shutdown itself is not this test's business
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runProxyWithSignal did not return after a signal")
	}
}
