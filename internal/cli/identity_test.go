package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/status"
)

// identityHealthServerPort is production's localListenPort
// (internal/proxy/proxy.go), duplicated here rather than exported across a
// package boundary just for tests: it reads http.LocalAddrContextKey,
// which net/http populates on every request's context from the accepted
// connection's own local address, so identityHealthServer below binds its
// proof to its OWN listening port exactly the way the real daemon does
// (Ruling 32).
func identityHealthServerPort(t *testing.T, r *http.Request) int {
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

// identityHealthServer answers health like a part-1 daemon: the proof of
// secret for the request's nonce and this server's own listening port
// (Ruling 32), or none when secret is zero (legacy). Mirrors internal/shim's
// own provingHealthServer, which lives in that package's test binary and is
// unreachable from here.
func identityHealthServer(t *testing.T, secret proxyauth.Secret, h proxy.Health) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proxy.HealthPath {
			http.NotFound(w, r)
			return
		}
		doc := h
		if n := r.URL.Query().Get(proxyauth.NonceParam); !secret.IsZero() && proxyauth.ValidNonce(n) {
			doc.Proof = secret.Proof(identityHealthServerPort(t, r), n)
		}
		json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDaemonIdentity drives cli.daemonIdentity itself against real
// httptest servers, the way shim.VerifyHealth's own suite does one layer
// down — this is the layer that adds proxyauth.Load and the "unknown"
// case (a daemon answers, but the secret file cannot be read).
func TestDaemonIdentity(t *testing.T) {
	home := t.TempDir()
	mine, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := proxyauth.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	verified := identityHealthServer(t, mine, proxy.Health{Chottag: true, Version: "t"})
	if got := daemonIdentity(home, mustPort(t, verified.URL)); got != string(shim.IdentityVerified) {
		t.Errorf("proving server: identity = %s, want verified", got)
	}

	// A proof-less, different-version listener is legacy ONLY while this
	// home's own daemon.lock is held by the exact pid the health document
	// names (Ruling 30, shim.TrustLegacy): otherwise it is a squatter, and
	// calling it legacy would tell the operator to `chottag daemon
	// restart` when the real problem is exactly what L4 targets (T11 fix
	// round 1 finding 3). Real lock, per inspectLock's own doc comment in
	// internal/shim: a fabricated Status would prove nothing about what an
	// actual flock can and cannot be forged.
	legacyPID := os.Getpid()
	release, err := daemonlock.Acquire(home, daemonlock.Record{PID: legacyPID, Started: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	legacy := identityHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "not-" + Version, PID: legacyPID})
	if got := daemonIdentity(home, mustPort(t, legacy.URL)); got != string(shim.IdentityLegacy) {
		t.Errorf("proof-less, different-version server, lock held by the same pid: identity = %s, want legacy", got)
	}

	// A proof-less listener reporting the SAME version as this build can
	// never legitimately be legacy at all (VerifyHealth's own rule), even
	// with a trusted lock held by the exact pid that answered: this only
	// holds if daemonIdentity hands VerifyHealth the real running Version,
	// never "" (a mutation of that argument would otherwise read this as
	// legacy — and, with the lock held by the right pid, TrustLegacy would
	// then say "trusted", so the held lock here is what keeps finding 3's
	// own fix from accidentally masking this mutation).
	sameVersion := identityHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: Version, PID: legacyPID})
	if got := daemonIdentity(home, mustPort(t, sameVersion.URL)); got != string(shim.IdentityMismatch) {
		t.Errorf("proof-less, same-version server (lock held by the same pid): identity = %s, want mismatch", got)
	}
	release()

	squatter := identityHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "not-" + Version, PID: legacyPID})
	if got := daemonIdentity(home, mustPort(t, squatter.URL)); got != string(shim.IdentityMismatch) {
		t.Errorf("proof-less, different-version server, no lock held: identity = %s, want mismatch (Ruling 30), not legacy", got)
	}

	mismatch := identityHealthServer(t, theirs, proxy.Health{Chottag: true, Version: "t"})
	if got := daemonIdentity(home, mustPort(t, mismatch.URL)); got != string(shim.IdentityMismatch) {
		t.Errorf("another install's proof: identity = %s, want mismatch", got)
	}

	if got := daemonIdentity(home, closedPort(t)); got != string(shim.IdentityNone) {
		t.Errorf("nothing listening: identity = %s, want none", got)
	}

	unreadableHome := t.TempDir()
	if _, err := proxyauth.LoadOrCreate(unreadableHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(proxyauth.Path(unreadableHome), 0o644); err != nil {
		t.Fatal(err)
	}
	up := identityHealthServer(t, proxyauth.Secret{}, proxy.Health{Chottag: true, Version: "t"})
	if got := daemonIdentity(unreadableHome, mustPort(t, up.URL)); got != "unknown" {
		t.Errorf("unreadable secret, a server up: identity = %s, want unknown", got)
	}
}

// TestNewDoctorEnvDaemonIdentityCallsDoctorIdentity is doctor's wiring: the
// Env newDoctorEnv builds calls doctorIdentity, the swappable seam, never
// daemonIdentity directly — so a test can steer it without a real probe.
func TestNewDoctorEnvDaemonIdentityCallsDoctorIdentity(t *testing.T) {
	var calls int
	var gotHome string
	var gotPort int
	restore := SetDoctorIdentityForTest(func(h string, port int) string {
		calls++
		gotHome, gotPort = h, port
		return "legacy"
	})
	defer restore()

	home := t.TempDir()
	env, err := newDoctorEnv(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := env.DaemonIdentity(1234); got != "legacy" {
		t.Fatalf("DaemonIdentity = %q, want legacy", got)
	}
	if calls != 1 || gotHome != home || gotPort != 1234 {
		t.Fatalf("doctorIdentity called %d time(s) with (%q, %d), want 1 with (%q, 1234)", calls, gotHome, gotPort, home)
	}
}

// statusFileWithRunningDaemon writes a status cache whose daemon heartbeat
// is fresh enough for status.File.DaemonRunningAt to read it as running
// (spec §5.1), so runStatus's identity overlay actually probes. Mirrors
// cli_test's own runningDaemon helper (status_version_test.go), which
// lives in the external test package and is unreachable from here.
func statusFileWithRunningDaemon(t *testing.T, home string, port int) {
	t.Helper()
	var f status.File
	f.SetDaemon(port, 0, 0, 0, time.Now())
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

// TestStatusShowsALegacyDaemon and TestStatusShowsAMismatchedDaemon drive
// runStatus's identity overlay end to end through SetStatusIdentityForTest
// and SetStatusProbeForTest: statusProbe must say something chottag-shaped
// answered (ok == true) or the overlay is never computed at all — a
// version is no longer required (final review N4;
// TestStatusIdentityRunsForAVersionlessButChottagShapedAnswer below pins
// that half directly).
func TestStatusShowsALegacyDaemon(t *testing.T) {
	home := t.TempDir()
	statusFileWithRunningDaemon(t, home, 47850)
	defer SetStatusProbeForTest(func(int) (bool, string) { return true, "0.3.0" })()
	defer SetStatusIdentityForTest(func(string, int) string { return string(shim.IdentityLegacy) })()

	var out bytes.Buffer
	if code := runStatus(home, nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, out.String())
	}
	want := "daemon: running without proxy authentication (run: chottag daemon restart)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want it to contain %q", out.String(), want)
	}
	// Review round 1 finding 7: a legacy daemon's health version always
	// differs from this build's (Ruling 30), so the ordinary
	// version-mismatch line would otherwise always print too, saying the
	// identical "run: chottag daemon restart" a second time.
	if strings.Contains(out.String(), "daemon: running 0.3.0, installed") {
		t.Fatalf("output = %q, must not also print the version-mismatch line for a legacy daemon", out.String())
	}

	var jsonOut bytes.Buffer
	if code := runStatus(home, nil, newReporter(true, &jsonOut, io.Discard)); code != 0 {
		t.Fatalf("runStatus --json = %d: %s", code, jsonOut.String())
	}
	var got struct {
		Daemon struct {
			Identity string `json:"identity"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut.String())
	}
	if got.Daemon.Identity != string(shim.IdentityLegacy) {
		t.Fatalf("daemon.identity = %q, want %q", got.Daemon.Identity, shim.IdentityLegacy)
	}
}

func TestStatusShowsAMismatchedDaemon(t *testing.T) {
	home := t.TempDir()
	statusFileWithRunningDaemon(t, home, 47850)
	defer SetStatusProbeForTest(func(int) (bool, string) { return true, "t" })()
	defer SetStatusIdentityForTest(func(string, int) string { return string(shim.IdentityMismatch) })()

	var out bytes.Buffer
	if code := runStatus(home, nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, out.String())
	}
	want := "daemon: port 47850 answered but did not prove it is this install's daemon (run: chottag doctor)"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want it to contain %q", out.String(), want)
	}

	var jsonOut bytes.Buffer
	if code := runStatus(home, nil, newReporter(true, &jsonOut, io.Discard)); code != 0 {
		t.Fatalf("runStatus --json = %d: %s", code, jsonOut.String())
	}
	var got struct {
		Daemon struct {
			Identity string `json:"identity"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, jsonOut.String())
	}
	if got.Daemon.Identity != string(shim.IdentityMismatch) {
		t.Fatalf("daemon.identity = %q, want %q", got.Daemon.Identity, shim.IdentityMismatch)
	}
}

// TestStatusIdentityNeverProbedWithoutARunningDaemon pins the "statusProbe
// must say running, or no overlay is computed" contract: with no status
// cache at all (f.Daemon stays nil), statusIdentity is never even called.
func TestStatusIdentityNeverProbedWithoutARunningDaemon(t *testing.T) {
	home := t.TempDir()
	var calls int
	defer SetStatusIdentityForTest(func(string, int) string { calls++; return string(shim.IdentityLegacy) })()

	var out bytes.Buffer
	if code := runStatus(home, nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, out.String())
	}
	if calls != 0 {
		t.Fatalf("statusIdentity called %d time(s), want 0: no running daemon to overlay", calls)
	}
}

// TestStatusIdentityRunsForAVersionlessButChottagShapedAnswer pins N4
// (final review): a squatter that answers `{"chottag":true}` with no
// version field at all (statusProbe's ok, "" shape) must still get an
// identity verdict — doctor's own daemon-identity row already catches this
// shape, and status must not silently give it a pass instead.
func TestStatusIdentityRunsForAVersionlessButChottagShapedAnswer(t *testing.T) {
	home := t.TempDir()
	statusFileWithRunningDaemon(t, home, 47850)
	defer SetStatusProbeForTest(func(int) (bool, string) { return true, "" })()
	var calls int
	defer SetStatusIdentityForTest(func(string, int) string { calls++; return string(shim.IdentityMismatch) })()

	var out bytes.Buffer
	if code := runStatus(home, nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, out.String())
	}
	if calls != 1 {
		t.Fatalf("statusIdentity called %d time(s), want 1: a version-less chottag-shaped answer must still be verified (N4)", calls)
	}
}
