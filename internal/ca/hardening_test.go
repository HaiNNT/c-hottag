package ca_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
)

func TestNewCAIsNameConstrained(t *testing.T) {
	dir := t.TempDir()
	a, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert := parsePEMCert(t, filepath.Join(dir, "ca.pem"))
	if !cert.PermittedDNSDomainsCritical || !slices.Equal(cert.PermittedDNSDomains, ca.PermittedDomains) {
		t.Fatalf("PermittedDNSDomains = %v (critical %v), want %v critical", cert.PermittedDNSDomains, cert.PermittedDNSDomainsCritical, ca.PermittedDomains)
	}
	if !a.Constrained() {
		t.Error("Constrained() = false for a new CA")
	}
	for _, h := range []string{"api.anthropic.com", "mcp-proxy.anthropic.com", "claude.ai", "x.claude.com"} {
		leaf, err := a.Leaf(h)
		if err != nil {
			t.Fatalf("Leaf(%s): %v", h, err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: h, Roots: a.Pool()}); err != nil {
			t.Errorf("leaf for %s does not verify under the constrained CA: %v", h, err)
		}
	}
	for _, h := range []string{"example.com", "anthropic.com.evil.example", "notclaude.ai", "127.0.0.1"} {
		if a.Permits(h) {
			t.Errorf("Permits(%q) = true", h)
		}
		if _, err := a.Leaf(h); err == nil {
			t.Errorf("Leaf(%q) minted a certificate outside the name constraints", h)
		}
	}
}

func TestAnExistingUnconstrainedCAKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	writeUnconstrainedCA(t, dir) // test helper: the pre-part-1 template, written 0600/0644
	a, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Constrained() || !a.Permits("example.com") {
		t.Fatal("an old CA must load unconstrained and permit any host")
	}
	if _, err := a.Leaf("api.anthropic.com"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRefusesAGroupReadableKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(dir, "ca.key"), 0o640)
	if _, err := ca.LoadOrCreate(dir); !errors.Is(err, ca.ErrKeyPermissions) {
		t.Errorf("LoadOrCreate = %v, want ErrKeyPermissions", err)
	}
	if _, err := ca.Load(dir); !errors.Is(err, ca.ErrKeyPermissions) {
		t.Errorf("Load = %v, want ErrKeyPermissions", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal("the key must be left in place, never replaced")
	}
}

// TestLoadRefusesASymlinkKey pins that a ca.key planted as a symlink (to a
// file that is itself perfectly permissioned) is refused by both entry
// points, not silently followed and trusted.
func TestLoadRefusesASymlinkKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "ca.key")
	moved := filepath.Join(dir, "elsewhere.key")
	if err := os.Rename(real, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, real); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir); !errors.Is(err, ca.ErrKeyPermissions) {
		t.Errorf("LoadOrCreate = %v, want ErrKeyPermissions", err)
	}
	if _, err := ca.Load(dir); !errors.Is(err, ca.ErrKeyPermissions) {
		t.Errorf("Load = %v, want ErrKeyPermissions", err)
	}
}

// TestLoadRefusesADanglingSymlinkKey pins that a symlink to nowhere is
// classified as ErrKeyPermissions (a symlink is never accepted, no matter
// where it points), not as an "incomplete CA" — the distinction the
// O_NOFOLLOW open exists to make.
func TestLoadRefusesADanglingSymlinkKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "ca.key")
	if err := os.Remove(real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere.key"), real); err != nil {
		t.Fatal(err)
	}
	if _, err := ca.LoadOrCreate(dir); !errors.Is(err, ca.ErrKeyPermissions) {
		t.Errorf("LoadOrCreate = %v, want ErrKeyPermissions", err)
	}
	if _, err := ca.Load(dir); !errors.Is(err, ca.ErrKeyPermissions) {
		t.Errorf("Load = %v, want ErrKeyPermissions", err)
	}
}

// TestReadKeyErrorForASymlinkNamesTheMoveNotABareFix pins N2 (final
// review): doctor --fix refuses a symlinked ca.key (internal/doctor's
// caCheck/caMoveHint), so the daemon's own startup error must not send the
// user to "run: chottag doctor --fix" alone — it must name the move
// caMoveHint actually recommends.
func TestReadKeyErrorForASymlinkNamesTheMoveNotABareFix(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "ca.key")
	moved := filepath.Join(dir, "elsewhere.key")
	if err := os.Rename(real, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, real); err != nil {
		t.Fatal(err)
	}
	_, err := ca.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "move both ca.pem and ca.key out of") {
		t.Fatalf("a symlinked ca.key's error = %v, want the move hint", err)
	}
	if strings.Contains(err.Error(), "run: chottag doctor --fix") {
		t.Errorf("a symlinked ca.key's error = %v, must not also tell the user to run a bare doctor --fix: doctor declines this shape", err)
	}
}

// TestReadKeyErrorForAnOwnedModeProblemKeepsThePlainFixHint is the other
// half of N2: an owned ca.key with merely the wrong mode bits IS one
// doctor --fix repairs in place (chmodOwnKey), so its error keeps the
// plain hint, never the move one.
func TestReadKeyErrorForAnOwnedModeProblemKeepsThePlainFixHint(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(dir, "ca.key"), 0o640)
	_, err := ca.Load(dir)
	if err == nil || !strings.Contains(err.Error(), "run: chottag doctor --fix") {
		t.Fatalf("an owned, wrong-mode ca.key's error = %v, want the plain doctor --fix hint", err)
	}
	if strings.Contains(err.Error(), "move both ca.pem") {
		t.Errorf("an owned, wrong-mode ca.key's error = %v, must not tell the user to move it: doctor --fix repairs this shape in place", err)
	}
}

func TestServerTLSConfigPresentsOnlyTheTunnelHost(t *testing.T) {
	a, _ := ca.LoadOrCreate(t.TempDir())
	cfg := a.ServerTLSConfig("api.anthropic.com")
	// claude.ai is itself within the CA's constraints, so its rejection
	// here can only be the SNI-bind check in ServerTLSConfig, not Permits.
	// evil.example is outside the constraints too, so it is redundant with
	// the SNI-bind check above and additionally exercises Permits/Leaf.
	for sni, ok := range map[string]bool{"api.anthropic.com": true, "API.Anthropic.COM": true, "": true, "claude.ai": false, "evil.example": false} {
		c, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
		if ok != (err == nil) {
			t.Errorf("SNI %q: err = %v, want ok=%v", sni, err, ok)
			continue
		}
		if ok && !slices.Equal(c.Leaf.DNSNames, []string{"api.anthropic.com"}) {
			t.Errorf("SNI %q got a leaf for %v", sni, c.Leaf.DNSNames)
		}
	}
}

// parsePEMCert reads and parses a PEM-encoded certificate file.
func parsePEMCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("no PEM block in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// writeUnconstrainedCA writes a CA in dir built from the pre-part-1
// template (IsCA, MaxPathLenZero, no name constraints), so tests can pin
// that a CA generated before this hardening keeps loading and working.
func writeUnconstrainedCA(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
}
