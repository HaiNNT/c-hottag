package shim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/redact"
)

// portFromRequest is production's localListenPort (internal/proxy/proxy.go),
// duplicated here rather than exported across a package boundary just for
// tests: it reads http.LocalAddrContextKey, which net/http populates on
// every request's context from the accepted connection's own local address,
// so these fakes bind their proof to their OWN listening port exactly the
// way the real daemon does (Ruling 32).
func portFromRequest(t *testing.T, r *http.Request) int {
	t.Helper()
	addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		t.Fatal("request carries no LocalAddrContextKey")
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// provingHealthServer answers health like a part-1 daemon: the proof of
// secret for the request's nonce and the port this fake server is itself
// listening on (Ruling 32), or none when secret is zero (legacy).
func provingHealthServer(t *testing.T, secret proxyauth.Secret, h proxy.Health) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxy.HealthPath {
			http.NotFound(w, r)
			return
		}
		doc := h
		if n := r.URL.Query().Get(proxyauth.NonceParam); !secret.IsZero() && proxyauth.ValidNonce(n) {
			doc.Proof = secret.Proof(portFromRequest(t, r), n)
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordingHealthServer is provingHealthServer with every nonce it was
// asked for recorded into *seen — even a request that carries none at all
// (an empty string) — so a caller can prove VerifyHealth actually sends a
// fresh one each time it is called, rather than merely that ITS OWN
// verification passes (fix round 1 item 1: a hard-coded nonce would still
// pass a suite that only ever calls VerifyHealth once per server).
func recordingHealthServer(t *testing.T, secret proxyauth.Secret, h proxy.Health, seen *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxy.HealthPath {
			http.NotFound(w, r)
			return
		}
		n := r.URL.Query().Get(proxyauth.NonceParam)
		*seen = append(*seen, n)
		doc := h
		if !secret.IsZero() && proxyauth.ValidNonce(n) {
			doc.Proof = secret.Proof(portFromRequest(t, r), n)
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// staleNonceHealthServer always answers with secret's proof for
// fixedNonce, ignoring whatever nonce the request actually carried: it
// pins that VerifyHealth checks the proof against the NONCE IT ITSELF
// SENT, not merely that some validly-shaped proof came back (fix round 1
// item 1).
func staleNonceHealthServer(t *testing.T, secret proxyauth.Secret, fixedNonce string, h proxy.Health) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxy.HealthPath {
			http.NotFound(w, r)
			return
		}
		doc := h
		doc.Proof = secret.Proof(portFromRequest(t, r), fixedNonce)
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// relayingHealthServer is the local squatter M2 (final review) describes:
// for every health request it receives, it relays the SAME request (path
// and query string, including the shim's nonce) to target's health
// endpoint and returns target's answer verbatim — including target's own
// proof, which was computed for target's port, not this server's. Ruling
// 32 is what makes that proof fail to verify against this server's port
// (TestVerifyHealthRefusesARelayedProofFromAnotherPort below).
func relayingHealthServer(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(target + r.URL.RequestURI())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// freePort binds 127.0.0.1:0, reads the port the kernel picked, and closes
// it at once: a port nothing is listening on, never a fixed one.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// rawSecret reads home's secret straight off disk, the one place a test may
// hold it, so it can check that nothing else ever prints it.
func rawSecret(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(proxyauth.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// ownVersion is deliberately distinct from every health.Version this file's
// tests report, so a table entry expecting IdentityLegacy stays legacy
// (TestVerifyHealthSameVersionProofLessIsMismatchNotLegacy below pins the
// case where the versions DO agree).
const ownVersion = "own-test-version"

func TestVerifyHealthIdentities(t *testing.T) {
	mine, _ := proxyauth.LoadOrCreate(t.TempDir())
	theirs, _ := proxyauth.LoadOrCreate(t.TempDir())
	anotherNonce := proxyauth.NewNonce()
	for _, c := range []struct {
		name string
		srv  func(t *testing.T) *httptest.Server
		want Identity
	}{
		{"ours", func(t *testing.T) *httptest.Server {
			return provingHealthServer(t, mine, proxy.Health{Chottag: true, Version: "t"})
		}, IdentityVerified},
		{"old daemon", func(t *testing.T) *httptest.Server {
			return provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "t"})
		}, IdentityLegacy},
		{"another install", func(t *testing.T) *httptest.Server {
			return provingHealthServer(t, theirs, proxy.Health{Chottag: true, Version: "t"})
		}, IdentityMismatch},
		{"stale proof for a different nonce", func(t *testing.T) *httptest.Server {
			// The server always answers mine.Proof(anotherNonce), a valid
			// proof, but never for the nonce VerifyHealth actually sent: a
			// constant/stale nonce implementation would still pass this if
			// it only checked "is this a valid-looking proof" (fix round 1
			// item 1).
			return staleNonceHealthServer(t, mine, anotherNonce, proxy.Health{Chottag: true, Version: "t"})
		}, IdentityMismatch},
	} {
		if id, _ := VerifyHealth(mustPort(t, c.srv(t).URL), mine, ownVersion); id != c.want {
			t.Errorf("%s: identity %s, want %s", c.name, id, c.want)
		}
	}
	if id, _ := VerifyHealth(freePort(t), mine, ownVersion); id != IdentityNone {
		t.Errorf("nothing listening: %s, want none", id)
	}
}

// TestVerifyHealthRefusesARelayedProofFromAnotherPort pins Ruling 32 (final
// review M2, the parked "bind the proof to the listen address" item) with
// two real listeners sharing one secret: q is the genuine, same-secret
// daemon on its own port; p is a local squatter that, for every health
// request it gets, relays it — nonce and all — to q and hands back q's
// answer, including q's proof, verbatim. Without the port binding, that
// relayed proof looks perfectly valid for p's own port too, and the shim
// would hand `claude` a working proxy URL pointed at the squatter. With it,
// VerifyHealth(p's port, ...) must refuse it: the proof was computed for
// q's port, not p's.
func TestVerifyHealthRefusesARelayedProofFromAnotherPort(t *testing.T) {
	mine, _ := proxyauth.LoadOrCreate(t.TempDir())
	q := provingHealthServer(t, mine, proxy.Health{Chottag: true, Version: "t"})
	p := relayingHealthServer(t, q.URL)

	if id, _ := VerifyHealth(mustPort(t, p.URL), mine, ownVersion); id != IdentityMismatch {
		t.Fatalf("relayed proof from another port: identity = %s, want mismatch (Ruling 32)", id)
	}
	// Sanity: the very same secret DOES verify directly against q, its own
	// port — so the refusal above is specifically about the port mismatch,
	// not some other difference between p and q.
	if id, _ := VerifyHealth(mustPort(t, q.URL), mine, ownVersion); id != IdentityVerified {
		t.Fatalf("direct probe of q: identity = %s, want verified", id)
	}
}

// TestVerifyHealthSendsAFreshNonceEachTime pins nonce freshness directly: a
// hard-coded or otherwise reused nonce would still pass every other test
// here, since each of them only ever calls VerifyHealth once per server
// (fix round 1 item 1).
func TestVerifyHealthSendsAFreshNonceEachTime(t *testing.T) {
	mine, _ := proxyauth.LoadOrCreate(t.TempDir())
	var seen []string
	srv := recordingHealthServer(t, mine, proxy.Health{Chottag: true, Version: "t"}, &seen)
	port := mustPort(t, srv.URL)

	if id, _ := VerifyHealth(port, mine, ownVersion); id != IdentityVerified {
		t.Fatalf("first call: identity = %s, want verified", id)
	}
	if id, _ := VerifyHealth(port, mine, ownVersion); id != IdentityVerified {
		t.Fatalf("second call: identity = %s, want verified", id)
	}

	if len(seen) != 2 {
		t.Fatalf("server saw %d nonce(s), want 2", len(seen))
	}
	for i, n := range seen {
		if !proxyauth.ValidNonce(n) {
			t.Errorf("nonce %d = %q is not a valid nonce", i, n)
		}
	}
	if seen[0] == seen[1] {
		t.Error("VerifyHealth sent the same nonce twice: a constant nonce would still pass every other test in this file")
	}
}

// TestVerifyHealthSameVersionProofLessIsMismatchNotLegacy is the controller
// ruling from fix round 1 item 5: the SAME build always turns proxy auth
// on, so a proof-less daemon reporting this exact version means auth failed
// to enable, not that it predates part 1. Only a daemon of a genuinely
// different version may be trusted as legacy.
func TestVerifyHealthSameVersionProofLessIsMismatchNotLegacy(t *testing.T) {
	mine, _ := proxyauth.LoadOrCreate(t.TempDir())

	same := provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: ownVersion})
	if id, _ := VerifyHealth(mustPort(t, same.URL), mine, ownVersion); id != IdentityMismatch {
		t.Errorf("same-version proof-less daemon: identity = %s, want mismatch", id)
	}

	older := provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "0.3.0"})
	if id, _ := VerifyHealth(mustPort(t, older.URL), mine, ownVersion); id != IdentityLegacy {
		t.Errorf("different-version proof-less daemon: identity = %s, want legacy", id)
	}
}

// TestRunSameVersionProofLessDaemonIsRefusedEvenWithTheLock drives the same
// controller ruling through shim.Run itself (fix round 2, N2): a
// proof-less daemon reporting Run's OWN version, PID-matched to a lock this
// process genuinely holds, must still be refused as a mismatch, never
// trusted as legacy. T10 guarantees a part-1+ daemon never serves at all
// without its secret loading (its start fails first, fix round 2, N4), so
// the same build answering health with no proof cannot legitimately be
// that daemon merely "not asked yet" — reverting the version parameter (or
// wiring the wrong one through) makes this pass for the wrong reason: it
// would fail only because IdentityLegacy's own lock/PID check trips, not
// because Run actually classified it as a mismatch, so the test also
// checks the specific fatal message.
func TestRunSameVersionProofLessDaemonIsRefusedEvenWithTheLock(t *testing.T) {
	const runVersion = "X"
	home := t.TempDir()
	if _, err := proxyauth.LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	srv := provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: runVersion, PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	release, err := daemonlock.Acquire(home, daemonlock.Record{PID: os.Getpid(), Started: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, runVersion, io.Discard, &errb)
	if code == 0 {
		t.Fatal("Run succeeded against a same-version proof-less daemon, want a failure")
	}
	if got.bin != "" {
		t.Errorf("exec'd %q: a same-version proof-less daemon must never run claude, lock or no lock", got.bin)
	}
	if !strings.Contains(errb.String(), "did not prove it holds this install's proxy secret") {
		t.Errorf("stderr %q must be the mismatch message, not a legacy warning or a lock-related refusal", errb.String())
	}
	if strings.Contains(errb.String(), "predates proxy authentication") {
		t.Errorf("stderr %q must not treat a same-version proof-less daemon as legacy", errb.String())
	}
}

func TestSpawnUpstreamTreatsOurURLWithUserinfoAsOurOwn(t *testing.T) {
	const port = 51234
	s, _ := proxyauth.LoadOrCreate(t.TempDir())
	for raw, want := range map[string]string{
		s.ProxyURL("127.0.0.1:51234"): "",
		"http://127.0.0.1:51234/":     "",
		"http://x:y@127.0.0.1:51234":  "",
		"http://localhost:51234":      "",
		"http://x:y@localhost:51234":  "",
		"http://[::1]:51234":          "",
		"http://x:y@[::1]:51234":      "",
		"http://127.0.0.1:51235":      "http://127.0.0.1:51235",
		"https://127.0.0.1:51234":     "https://127.0.0.1:51234",
		"http://127.0.0.1:51234/path": "http://127.0.0.1:51234/path",
		"http://127.0.0.1:51234?x":    "http://127.0.0.1:51234?x",
		"http://127.0.0.1:51234#frag": "http://127.0.0.1:51234#frag",
		"http://corp:8080":            "http://corp:8080",
	} {
		if got := SpawnUpstream([]string{"HTTPS_PROXY=" + raw}, port); got != want {
			t.Errorf("SpawnUpstream(%s) = %q, want %q", redact.UpstreamProxy(raw), redact.UpstreamProxy(got), redact.UpstreamProxy(want))
		}
	}
}

func TestRunHandsClaudeTheSecretURL(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	srv := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: "t", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	if code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, ownVersion, io.Discard, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if v := envGet(got.env, "HTTPS_PROXY"); v != s.ProxyURL("127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("HTTPS_PROXY = %s, want the secret URL", redact.UpstreamProxy(v))
	}
	if strings.Contains(errb.String(), rawSecret(t, home)) {
		t.Fatal("the secret reached stderr")
	}
}

// The shell already holds our own URL WITH userinfo (a nested claude): not
// an upstream conflict, and HTTPS_PROXY is rewritten to the same.
// TestFailsClosedOnUpstreamMismatch (shim_test.go) pins the other
// direction: a genuinely different upstream still fails.
func TestRunNestedClaudeWithOurSecretURLIsNotAConflict(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	srv := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: "t", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	hostport := "127.0.0.1:" + strconv.Itoa(port)

	var got execCall
	defer swapExec(home, &got)()

	code := Run(nil, home,
		[]string{"PATH=" + filepath.Dir(fakeClaude(t)), "HTTPS_PROXY=" + s.ProxyURL(hostport)},
		ownVersion, io.Discard, io.Discard)
	if code != 0 {
		t.Fatalf("Run = %d, want 0: HTTPS_PROXY already naming our own secret URL must not read as a conflict", code)
	}
	if v := envGet(got.env, "HTTPS_PROXY"); v != s.ProxyURL(hostport) {
		t.Fatalf("HTTPS_PROXY = %s, want it rewritten to the same secret URL", redact.UpstreamProxy(v))
	}
}

func TestRunLegacyDaemonHoldingTheLockStartsWithoutTheSecret(t *testing.T) {
	home := t.TempDir()
	srv := provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "0.3.0", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	release, err := daemonlock.Acquire(home, daemonlock.Record{PID: os.Getpid(), Started: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	if code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, ownVersion, io.Discard, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if v := envGet(got.env, "HTTPS_PROXY"); v != "http://127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("legacy daemon: HTTPS_PROXY = %s, want our plain URL", redact.UpstreamProxy(v))
	}
	if !strings.Contains(errb.String(), "chottag daemon restart") {
		t.Errorf("want the restart warning, got %q", errb.String())
	}
}

// As TestRunLegacyDaemonHoldingTheLockStartsWithoutTheSecret above, but
// without daemonlock.Acquire: a proof-less listener holding no lock at all
// is a squatter, not a legacy daemon, and claude must not start.
func TestRunProofLessListenerWithoutTheLockIsRefused(t *testing.T) {
	home := t.TempDir()
	srv := provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "0.3.0", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, ownVersion, io.Discard, &errb)
	if code == 0 {
		t.Fatal("Run succeeded against a proof-less listener holding no lock, want a failure")
	}
	if got.bin != "" {
		t.Errorf("exec'd %q: a proof-less listener without this home's lock must never run claude", got.bin)
	}
	for _, want := range []string{"holds no lock", "lsof"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr %q must contain %q", errb.String(), want)
		}
	}
}

// Legacy trust must tie the lock holder to the LISTENER: a lock held by
// this process is not enough if the health document's PID names some other
// process entirely (fix round 1 item 4) — otherwise a squatter could sit
// behind a legitimate daemon's home merely because that home's daemon.lock
// happens to be held (by the REAL daemon, on some other port), while the
// squatter itself answers on the port state.json actually names.
func TestRunLegacyDaemonPIDMismatchWithTheLockIsRefused(t *testing.T) {
	home := t.TempDir()
	srv := provingHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "0.3.0", PID: os.Getpid() + 1})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	release, err := daemonlock.Acquire(home, daemonlock.Record{PID: os.Getpid(), Started: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, ownVersion, io.Discard, &errb)
	if code == 0 {
		t.Fatal("Run succeeded against a legacy health PID that does not match the lock holder, want a failure")
	}
	if got.bin != "" {
		t.Errorf("exec'd %q: a legacy daemon whose health PID does not match the lock holder must never run claude", got.bin)
	}
	// fix round 2, N3: the PID mismatch gets its own message — a lock IS
	// held here, so "holds no lock" would be misleading. It names both
	// pids so an operator can tell the lock holder from the listener.
	wantLockPID, wantHealthPID := fmt.Sprintf("pid %d", os.Getpid()), fmt.Sprintf("pid %d", os.Getpid()+1)
	if !strings.Contains(errb.String(), wantLockPID) || !strings.Contains(errb.String(), wantHealthPID) {
		t.Errorf("stderr %q must name both the lock holder's %s and the listener's %s", errb.String(), wantLockPID, wantHealthPID)
	}
	if strings.Contains(errb.String(), "holds no lock") {
		t.Errorf("stderr %q should not say \"holds no lock\": a lock IS held here, just by a different pid", errb.String())
	}
}

// A daemon that proves ANOTHER install's secret is refused outright, with a
// hint to restart (its secret file may have been replaced) and never a
// hang.
func TestRunWrongProofIsRefusedWithARestartHint(t *testing.T) {
	home := t.TempDir()
	if _, err := proxyauth.LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	theirs, _ := proxyauth.LoadOrCreate(t.TempDir())
	srv := provingHealthServer(t, theirs, proxy.Health{Chottag: true, Version: "t", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPort(t, home, port)
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, ownVersion, io.Discard, &errb)
	if code == 0 {
		t.Fatal("Run succeeded against a daemon proving another install's secret, want a failure")
	}
	if got.bin != "" {
		t.Errorf("exec'd %q: a wrong proof must never run claude", got.bin)
	}
	if !strings.Contains(errb.String(), "chottag daemon restart") {
		t.Errorf("stderr %q must contain the restart hint", errb.String())
	}
	if !strings.Contains(errb.String(), proxyauth.Path(home)) {
		t.Errorf("stderr %q must name the secret file's path", errb.String())
	}
}

// TestRunRefusesAnUnusableProxySecret pins fix round 1 item 2: a
// pre-existing proxy.secret that LoadOrCreate refuses to trust (malformed
// content, too-wide a mode, or a symlink — Ruling 6, "never repairs") must
// stop Run before it ever probes the port, name the secret file's path in
// stderr, and never echo the file's own content.
func TestRunRefusesAnUnusableProxySecret(t *testing.T) {
	writeSecretFile := func(t *testing.T, home string, mode os.FileMode, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(home, "ca"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(proxyauth.Path(home), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	const malformedContent = "not-hex-and-not-64-chars"
	const worldReadableContent = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const symlinkTargetContent = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	cases := []struct {
		name  string
		setup func(t *testing.T, home string)
	}{
		{"malformed content", func(t *testing.T, home string) {
			writeSecretFile(t, home, 0o600, malformedContent+"\n")
		}},
		{"world-readable mode", func(t *testing.T, home string) {
			writeSecretFile(t, home, 0o644, worldReadableContent+"\n")
		}},
		{"symlink", func(t *testing.T, home string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.WriteFile(target, []byte(symlinkTargetContent+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(home, "ca"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, proxyauth.Path(home)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			c.setup(t, home)
			// Nothing listens here: Run must refuse on the secret alone,
			// before it ever dials the port.
			writeStateWithPort(t, home, freePort(t))

			var got execCall
			defer swapExec(home, &got)()
			var errb bytes.Buffer
			code := Run(nil, home, []string{"PATH=" + filepath.Dir(fakeClaude(t))}, ownVersion, io.Discard, &errb)
			if code == 0 {
				t.Fatalf("Run succeeded against an unusable proxy secret (%s), want a failure", c.name)
			}
			if got.bin != "" {
				t.Errorf("exec'd %q: an unusable proxy secret must never run claude", got.bin)
			}
			if !strings.Contains(errb.String(), proxyauth.Path(home)) {
				t.Errorf("stderr %q must name the secret file's path", errb.String())
			}
			for _, leaked := range []string{malformedContent, worldReadableContent, symlinkTargetContent} {
				if strings.Contains(errb.String(), leaked) {
					t.Errorf("stderr %q leaked the secret file's content", errb.String())
				}
			}
			// ErrMalformed's own Error() names only the path, unlike
			// *PermError, which already names the doctor hint itself (fix
			// round 1 item 3): Run must append it for this case.
			if c.name == "malformed content" && !strings.Contains(errb.String(), "chottag doctor --fix") {
				t.Errorf("stderr %q must hint at `chottag doctor --fix` for a malformed secret", errb.String())
			}
		})
	}
}
