package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// TestProxySecretOKOnAHealthyInstall pins the ok row's exact text: doctor
// never prints the secret, so this is the one thing a passing row ever
// says about it.
func TestProxySecretOKOnAHealthyInstall(t *testing.T) {
	ti := newTestInstall(t)
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "proxy-secret")
	if r.Status != StatusOK || r.Detail != "ca/proxy.secret is 0600 and well formed" {
		t.Fatalf("proxy-secret = %+v", r)
	}
}

// TestProxySecretMissingBreakThenRepair covers the missing-file case: a
// problem with the ordinary --fix hint (no daemon holds the lock), and the
// fix leaves a fresh 0600 secret.
func TestProxySecretMissingBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = false // TestCABreakThenRepair's own reason: the fix refuses under a held lock
	breakThenRepair(t, ti, InstallChecks(), "proxy-secret", func() {
		must(t, os.Remove(proxyauth.Path(ti.home)))
	}, "home:ca/proxy.secret", "home:ca/proxy.secret.lock")
	info, err := os.Stat(proxyauth.Path(ti.home))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy.secret mode = %o, want 0600", info.Mode().Perm())
	}
}

// TestProxySecretWorldReadableBreakThenRepair pins Ruling 6: a secret that
// widened its own mode may already be leaked, so the fix regenerates a
// brand NEW secret rather than merely chmod-ing the old (possibly
// compromised) one back to 0600.
func TestProxySecretWorldReadableBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = false
	before, err := os.ReadFile(proxyauth.Path(ti.home))
	must(t, err)
	breakThenRepair(t, ti, InstallChecks(), "proxy-secret", func() {
		must(t, os.Chmod(proxyauth.Path(ti.home), 0o644))
	}, "home:ca/proxy.secret", "home:ca/proxy.secret.lock")
	after, err := os.ReadFile(proxyauth.Path(ti.home))
	must(t, err)
	if string(after) == string(before) {
		t.Fatal("the fix must regenerate a new secret, not merely chmod the old (possibly leaked) one")
	}
	info, err := os.Stat(proxyauth.Path(ti.home))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("proxy.secret mode = %o, want 0600", info.Mode().Perm())
	}
}

// TestProxySecretMalformedBreakThenRepair covers content that is not the
// 64-char lower-case hex proxyauth.Load requires.
func TestProxySecretMalformedBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = false
	breakThenRepair(t, ti, InstallChecks(), "proxy-secret", func() {
		must(t, os.WriteFile(proxyauth.Path(ti.home), []byte("not-a-valid-secret\n"), 0o600))
	}, "home:ca/proxy.secret", "home:ca/proxy.secret.lock")
}

// TestProxySecretHeldDaemonRefusesTheFix is proxy-secret's own I2: a
// daemon holding this home's lock keeps the old secret in memory, so
// regenerating it out from under a live session would 407 every request
// that session makes. The fix refuses, report-only, and writes nothing.
func TestProxySecretHeldDaemonRefusesTheFix(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = true // the default, made explicit: a daemon holds the lock
	must(t, os.Chmod(proxyauth.Path(ti.home), 0o644))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "proxy-secret")
	if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag daemon stop") {
		t.Fatalf("proxy-secret = %+v, want a report-only problem with the stop hint", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestProxySecretFixTightensAnOverOpenOwnCADir is the review correction
// (T1 review D2): the fix chmods a too-open ca/ directory it owns to 0700
// BEFORE calling proxyauth.Regenerate. Regenerate itself has no directory
// check and would happily write into it either way — but proxyauth.Load's
// checkDirPerm rejects a group/other-writable ca/ with its own PermError,
// so leaving it too-open would make the freshly-regenerated secret
// unreadable again the moment anything (including this row's own next
// Detect) tried to Load it, defeating the fix entirely.
func TestProxySecretFixTightensAnOverOpenOwnCADir(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = false
	dir := filepath.Join(ti.home, "ca")
	must(t, os.Chmod(dir, 0o770))
	must(t, os.Remove(proxyauth.Path(ti.home)))
	if err := proxySecretCheck().Fix(ti.env); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	info, err := os.Stat(dir)
	must(t, err)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("ca/ mode = %o, want 0700 after the fix tightened it", info.Mode().Perm())
	}
	if _, err := proxyauth.Load(ti.home); err != nil {
		t.Fatalf("the regenerated secret does not load: %v", err)
	}
}

// TestProxySecretRefusesASymlinkedCADir is review round 1 finding 1:
// proxyauth.Regenerate has no directory check of its own (its lock() just
// MkdirAlls and opens by path, both of which follow a symlink there), so
// this row's own Detect and Fix are the only thing standing between a
// symlinked ca/ and writing a fresh secret into wherever it points.
func TestProxySecretRefusesASymlinkedCADir(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = false // isolate the symlink refusal from the daemon-lock one
	dir := filepath.Join(ti.home, "ca")
	target := t.TempDir()
	must(t, os.RemoveAll(dir))
	must(t, os.Symlink(target, dir))

	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "proxy-secret")
	if r.Status != StatusProblem || r.Hint != caMoveHint(dir) {
		t.Fatalf("proxy-secret with a symlinked ca/ = %+v, want a problem with the move hint %q", r, caMoveHint(dir))
	}

	// --fix must be a permanent no-op: a hint from Detect means Run never
	// even calls Fix (runOne's `fixable`), and Fix itself refuses directly
	// too (belt and braces, in case it is ever called some other way).
	if err := proxySecretCheck().Fix(ti.env); err == nil {
		t.Fatal("Fix called directly on a symlinked ca/ succeeded, want a refusal")
	}
	r2 := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "proxy-secret")
	if r2.Status != StatusProblem || r2.Hint != caMoveHint(dir) {
		t.Fatalf("--fix on a symlinked ca/ = %+v, want it to stay a problem with the same move hint (never an infinite --fix loop)", r2)
	}
	link, err := os.Readlink(dir)
	must(t, err)
	if link != target {
		t.Fatalf("the symlink itself changed: %q, want %q", link, target)
	}
	if _, err := os.Stat(filepath.Join(target, "proxy.secret")); err == nil {
		t.Fatal("the fix must never write a secret into the symlink's target")
	}
}

// TestProxySecretFixRefusesDirectlyUnderTheHeldOrUnknownLock kills the
// mutation of removing the held-lock refusal from Fix itself (review round
// 1 finding 5a): Detect's own hint already keeps Run from calling Fix in
// this shape, so this calls Fix directly, bypassing Detect entirely, and
// asserts both the held and the unknown-lock-state case still refuse and
// touch nothing.
func TestProxySecretFixRefusesDirectlyUnderTheHeldOrUnknownLock(t *testing.T) {
	ti := newTestInstall(t)
	ti.running = true
	before := ti.snapshot()
	if err := proxySecretCheck().Fix(ti.env); err == nil {
		t.Fatal("Fix called directly under a held lock succeeded, want a refusal")
	}
	assertChangedOnly(t, before, ti.snapshot())

	ti2 := newTestInstall(t)
	ti2.running = false
	ti2.inspectErr = errors.New("boom")
	before2 := ti2.snapshot()
	if err := proxySecretCheck().Fix(ti2.env); err == nil {
		t.Fatal("Fix called directly with an unreadable lock state succeeded, want a refusal")
	}
	assertChangedOnly(t, before2, ti2.snapshot())
}
