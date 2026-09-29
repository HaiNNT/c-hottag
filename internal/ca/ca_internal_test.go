package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLeafCacheIsBounded(t *testing.T) {
	a, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.Leaf("h0.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("Leaf returned a nil certificate")
	}
	const total = maxLeaves + 6
	leaves := make([]*tls.Certificate, total)
	leaves[0] = first
	for i := 1; i < total; i++ {
		c, err := a.Leaf(fmt.Sprintf("h%d.anthropic.com", i))
		if err != nil {
			t.Fatal(err)
		}
		leaves[i] = c
	}
	if n := len(a.leaves); n != maxLeaves {
		t.Fatalf("leaf cache holds %d, want exactly %d", n, maxLeaves)
	}
	if n := len(a.order); n != maxLeaves {
		t.Fatalf("order holds %d, want exactly %d", n, maxLeaves)
	}
	// Every one of the last maxLeaves hosts (the ones FIFO eviction must
	// have kept) must still return its identical, cached pointer.
	for i := total - maxLeaves; i < total; i++ {
		host := fmt.Sprintf("h%d.anthropic.com", i)
		again, err := a.Leaf(host)
		if err != nil {
			t.Fatal(err)
		}
		if again != leaves[i] {
			t.Errorf("Leaf(%s) returned a different pointer: it should still be cached", host)
		}
	}
	// Only after confirming the survivors are untouched: the oldest host
	// must have been evicted, so asking for it again mints a new leaf.
	again, err := a.Leaf("h0.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if again == first {
		t.Error("the oldest leaf must have been evicted and re-minted")
	}
}

// TestReadKeyFileNeverBlocksOnAFIFO pins that a ca.key planted as a FIFO
// cannot hang Load/LoadOrCreate forever waiting for a writer that will
// never arrive: O_NONBLOCK on the open makes it return immediately, and
// checkKeyInfo's IsRegular check then refuses it as ErrKeyPermissions, like
// any other non-regular file. The 60s bound is a hang guard, not a timing
// assertion (F185): a regression would block forever without it, not
// merely run slow.
func TestReadKeyFileNeverBlocksOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 2)
	go func() {
		_, err := LoadOrCreate(dir)
		done <- err
	}()
	go func() {
		_, err := Load(dir)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if !errors.Is(err, ErrKeyPermissions) {
				t.Errorf("got %v, want ErrKeyPermissions", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("Load/LoadOrCreate hung reading a FIFO ca.key")
		}
	}
}

// TestCheckKeyInfoOwnerMismatch pins checkKeyInfo's owner branch directly,
// with a uid that belongs to nobody in particular rather than root or
// another real account on the machine.
func TestCheckKeyInfoOwnerMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkKeyInfo(info, os.Getuid()); err != nil {
		t.Fatalf("checkKeyInfo with the real owner = %v, want nil", err)
	}
	if err := checkKeyInfo(info, os.Getuid()+1); !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("checkKeyInfo with a foreign uid = %v, want ErrKeyPermissions", err)
	}
}

// TestCheckKeyInfoNamesTheRightRemedy pins N2's own remedy split directly
// at checkKeyInfo (final re-review NEW-3, probes x5 and x6, which survived
// against readKeyFile's ELOOP-only test): errKeyNotOwnerFixable must wrap
// ErrKeyPermissions for the two shapes doctor --fix refuses to touch — a
// foreign owner and a non-regular file (a FIFO here; a symlink is
// readKeyFile's own ELOOP branch, already pinned via os.Symlink in
// hardening_test.go) — and must NOT wrap it for an owned, merely
// wrong-mode regular file, which --fix does repair in place. checkKeyInfo
// takes uid as a plain parameter, which is itself the test seam (like
// TestCheckKeyInfoOwnerMismatch above): no package-level getuid var exists
// in this package to swap.
func TestCheckKeyInfoNamesTheRightRemedy(t *testing.T) {
	dir := t.TempDir()

	wrongMode := filepath.Join(dir, "wrong-mode.key")
	if err := os.WriteFile(wrongMode, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	wrongModeInfo, err := os.Lstat(wrongMode)
	if err != nil {
		t.Fatal(err)
	}

	owned := filepath.Join(dir, "owned.key")
	if err := os.WriteFile(owned, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownedInfo, err := os.Lstat(owned)
	if err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(dir, "fifo.key")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fifoInfo, err := os.Lstat(fifo)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		info     os.FileInfo
		uid      int
		wantMove bool
	}{
		{"owned, wrong mode only: doctor --fix repairs this", wrongModeInfo, os.Getuid(), false},
		{"foreign owner: doctor --fix refuses this", ownedInfo, os.Getuid() + 1, true},
		{"non-regular file (FIFO): doctor --fix refuses this", fifoInfo, os.Getuid(), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkKeyInfo(c.info, c.uid)
			if !errors.Is(err, ErrKeyPermissions) {
				t.Fatalf("checkKeyInfo = %v, want ErrKeyPermissions", err)
			}
			if got := errors.Is(err, errKeyNotOwnerFixable); got != c.wantMove {
				t.Errorf("checkKeyInfo = %v, errors.Is(_, errKeyNotOwnerFixable) = %v, want %v", err, got, c.wantMove)
			}
		})
	}
}

// TestNewCAExcludesIPNames pins the defence in depth from security review
// L1: RFC 5280 name constraints apply per name type, so permitting DNS
// names alone says nothing about IP addresses. A leaf minted directly with
// the CA's own key material (bypassing Leaf/Permits entirely) for an IP SAN
// must still fail to verify, specifically because it names an excluded IP
// (not for some unrelated reason), because generate() also excludes every
// IPv4 and IPv6 address. DNS leaves for the hosts chottag actually
// intercepts must keep verifying.
func TestNewCAExcludesIPNames(t *testing.T) {
	a, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := randSerial()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "10.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 1),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("10.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	ipLeaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	_, verifyErr := ipLeaf.Verify(x509.VerifyOptions{Roots: a.Pool()})
	var invalid x509.CertificateInvalidError
	if !errors.As(verifyErr, &invalid) || invalid.Reason != x509.CANotAuthorizedForThisName {
		t.Fatalf("Verify = %v, want a CertificateInvalidError with Reason CANotAuthorizedForThisName", verifyErr)
	}

	for _, h := range []string{"api.anthropic.com", "mcp-proxy.anthropic.com"} {
		leaf, err := a.Leaf(h)
		if err != nil {
			t.Fatalf("Leaf(%s): %v", h, err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: h, Roots: a.Pool()}); err != nil {
			t.Errorf("DNS leaf for %s does not verify: %v", h, err)
		}
	}
}
