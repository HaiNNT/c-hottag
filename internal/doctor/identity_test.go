package doctor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDaemonIdentityRows maps every value Env.DaemonIdentity can return
// (cli.daemonIdentity's contract) onto daemon-identity's status, detail and
// hint.
func TestDaemonIdentityRows(t *testing.T) {
	for _, tc := range []struct {
		identity   string
		status     string
		detail     string
		hintSubstr string
	}{
		{"none", StatusOK, "no daemon running", ""},
		{"verified", StatusOK, "the daemon proved it holds this install's proxy secret", ""},
		{"legacy", StatusProblem, fmt.Sprintf("the daemon on port %d predates proxy authentication", testPort), "chottag daemon restart"},
		{"mismatch", StatusProblem, fmt.Sprintf("the process on port %d did not prove it holds ca/proxy.secret", testPort), fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", testPort)},
		{"unknown", StatusInfo, "a daemon answers, but ca/proxy.secret is unreadable, so it cannot be verified (see the proxy-secret row)", ""},
	} {
		t.Run(tc.identity, func(t *testing.T) {
			ti := newTestInstall(t)
			ti.daemonIdentity = tc.identity
			r := rowByID(t, mustRun(t, ti.env, []Check{daemonIdentityCheck()}, false), "daemon-identity")
			if r.Status != tc.status || r.Detail != tc.detail {
				t.Fatalf("daemon-identity(%s) = %+v, want status %s detail %q", tc.identity, r, tc.status, tc.detail)
			}
			if tc.hintSubstr == "" && r.Hint != "" {
				t.Fatalf("daemon-identity(%s) hint = %q, want none", tc.identity, r.Hint)
			}
			if tc.hintSubstr != "" && !strings.Contains(r.Hint, tc.hintSubstr) {
				t.Fatalf("daemon-identity(%s) hint = %q, want it to contain %q", tc.identity, r.Hint, tc.hintSubstr)
			}
			if daemonIdentityCheck().Fix != nil {
				t.Fatal("daemon-identity has a Fix; a restart interrupts every live session, so it must stay the user's call")
			}
		})
	}
}

// TestCAKeyPermFixesByChmodWhenWeOwnTheKey pins the review correction: a
// 0640 ca.key we own is fixed by chmod 0600, never by moving the CA away.
func TestCAKeyPermFixesByChmodWhenWeOwnTheKey(t *testing.T) {
	ti := newTestInstall(t)
	keyPath := filepath.Join(ti.home, "ca", "ca.key")
	breakThenRepair(t, ti, InstallChecks(), "ca", func() {
		must(t, os.Chmod(keyPath, 0o640))
	}, "home:ca/ca.key")
	info, err := os.Stat(keyPath)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode = %o, want 0600", info.Mode().Perm())
	}
}

// TestCAKeyPermFixRefusesASymlinkedCAKey is review round 1 finding 2: a
// symlinked ca.key must never be chmod'd through — the link's target
// mode must stay untouched, and the row stays a problem with the move
// hint rather than reading "fixed".
func TestCAKeyPermFixRefusesASymlinkedCAKey(t *testing.T) {
	ti := newTestInstall(t)
	dir := filepath.Join(ti.home, "ca")
	keyPath := filepath.Join(dir, "ca.key")
	target := filepath.Join(t.TempDir(), "elsewhere.key")
	must(t, os.WriteFile(target, []byte("not really a key"), 0o640))
	must(t, os.Remove(keyPath))
	must(t, os.Symlink(target, keyPath))

	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "ca")
	if r.Status != StatusProblem || r.Hint != caMoveHint(dir) {
		t.Fatalf("ca with a symlinked ca.key = %+v, want a problem with the move hint %q", r, caMoveHint(dir))
	}

	if err := caCheck().Fix(ti.env); err == nil {
		t.Fatal("Fix called directly on a symlinked ca.key succeeded, want a refusal")
	}
	r2 := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "ca")
	if r2.Status != StatusProblem || r2.Hint != caMoveHint(dir) {
		t.Fatalf("--fix on a symlinked ca.key = %+v, want it to stay a problem with the same move hint", r2)
	}
	link, err := os.Readlink(keyPath)
	must(t, err)
	if link != target {
		t.Fatalf("the symlink itself changed: %q, want %q", link, target)
	}
	info, err := os.Stat(target)
	must(t, err)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the symlink target's mode changed to %o, want it untouched at 0640", info.Mode().Perm())
	}
}

// TestCAKeyPermFixRefusesASymlinkedCADir is review round 1 finding 2's
// other shape: ca/ itself a symlink, with a genuinely too-open ca.key at
// the target. keyOwnedByUs's old Lstat(keyPath) would follow the ca/
// symlink to find a regular file this uid owns and wrongly call it
// fixable; the fix must refuse before ca.key is even looked at.
func TestCAKeyPermFixRefusesASymlinkedCADir(t *testing.T) {
	ti := newTestInstall(t)
	dir := filepath.Join(ti.home, "ca")
	target := t.TempDir()
	for _, name := range []string{"ca.pem", "ca.key"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(target, name), b, 0o600))
	}
	must(t, os.Chmod(filepath.Join(target, "ca.key"), 0o640)) // the key-perm problem this scenario is about
	must(t, os.RemoveAll(dir))
	must(t, os.Symlink(target, dir))

	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "ca")
	if r.Status != StatusProblem || r.Hint != caMoveHint(dir) {
		t.Fatalf("ca with a symlinked ca/ = %+v, want a problem with the move hint %q", r, caMoveHint(dir))
	}

	if err := caCheck().Fix(ti.env); err == nil {
		t.Fatal("Fix called directly on a symlinked ca/ succeeded, want a refusal")
	}
	mustRun(t, ti.env, InstallChecks(), true)
	info, err := os.Stat(filepath.Join(target, "ca.key")) // follows the (untouched) symlink itself
	must(t, err)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the target ca.key's mode changed to %o, want it untouched at 0640", info.Mode().Perm())
	}
	link, err := os.Readlink(dir)
	must(t, err)
	if link != target {
		t.Fatalf("the ca/ symlink itself changed: %q, want %q", link, target)
	}
}

// TestCADirUnsafeForeignOwner and TestChmodOwnKeyRefusesAForeignOwnedKey
// exercise the foreign-owner branches review round 1 finding 5 called out
// as otherwise untestable in CI without a second real uid: the getuid seam
// (proxyauth's own pattern) lets a test force it.
func TestCADirUnsafeForeignOwner(t *testing.T) {
	dir := t.TempDir()
	orig := getuid
	getuid = func() int { return orig() + 1 }
	defer func() { getuid = orig }()
	if reason := caDirUnsafe(dir); reason == "" {
		t.Fatal("caDirUnsafe with a foreign getuid = \"\", want a refusal reason")
	}
}

func TestChmodOwnKeyRefusesAForeignOwnedKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ca.key")
	must(t, os.WriteFile(keyPath, []byte("key"), 0o600))
	orig := getuid
	getuid = func() int { return orig() + 1 }
	defer func() { getuid = orig }()
	if err := chmodOwnKey(dir, keyPath); err == nil {
		t.Fatal("chmodOwnKey with a foreign getuid succeeded, want a refusal")
	}
	info, err := os.Stat(keyPath)
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode changed to %o, want it untouched at 0600", info.Mode().Perm())
	}
}

// TestCAUnconstrainedIsInfo pins Ruling 13: a CA generated before name
// constraints existed still loads and works, but doctor flags it as info,
// never a problem, with no hint (there is nothing to --fix: moving ca/
// away breaks trust for running sessions, so that stays the user's call).
func TestCAUnconstrainedIsInfo(t *testing.T) {
	ti := newTestInstall(t)
	dir := filepath.Join(ti.home, "ca")
	must(t, os.RemoveAll(dir))
	must(t, os.MkdirAll(dir, 0o700))
	writeUnconstrainedCA(t, dir)
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "ca")
	if r.Status != StatusInfo || !strings.Contains(r.Detail, "predates name constraints") || r.Hint != "" {
		t.Fatalf("ca = %+v, want info about predating name constraints with no hint", r)
	}
}

// TestCANewIsOK is the baseline a fresh, constrained CA (LoadOrCreate's
// default since Ruling 13) reads as ok, not info.
func TestCANewIsOK(t *testing.T) {
	ti := newTestInstall(t)
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "ca")
	if r.Status != StatusOK {
		t.Fatalf("ca = %+v, want ok for a fresh, constrained CA", r)
	}
}

// writeUnconstrainedCA writes a CA in dir built from the pre-part-1
// template (IsCA, MaxPathLenZero, no name constraints) — the same shape
// internal/ca's own hardening_test.go pins loads and keeps working. It is
// reimplemented here (rather than imported) because that helper lives in
// package ca_test, unexported and unreachable from here.
func writeUnconstrainedCA(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "chottag local CA", Organization: []string{"c-hottag"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	kder, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	must(t, os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o644))
}
