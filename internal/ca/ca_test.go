package ca_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
)

func TestLoadOrCreatePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	a1, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a1.CertPEM(), a2.CertPEM()) {
		t.Fatal("CA was regenerated on reload")
	}
	st, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key perm = %v, err = %v", st.Mode().Perm(), err)
	}
}

func TestLeafVerifiesAgainstCA(t *testing.T) {
	a, _ := ca.LoadOrCreate(t.TempDir())
	c, err := a.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Leaf.Verify(x509.VerifyOptions{
		DNSName:   "api.anthropic.com",
		Roots:     a.Pool(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeafIsCached(t *testing.T) {
	a, _ := ca.LoadOrCreate(t.TempDir())
	c1, _ := a.Leaf("claude.ai")
	c2, _ := a.Leaf("claude.ai")
	if c1 != c2 {
		t.Fatal("leaf not cached")
	}
}

func TestTLSHandshakeUsesSNI(t *testing.T) {
	a, _ := ca.LoadOrCreate(t.TempDir())
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	errc := make(chan error, 1)
	go func() {
		errc <- tls.Server(sConn, a.ServerTLSConfig("claude.ai")).Handshake()
	}()
	cli := tls.Client(cConn, &tls.Config{RootCAs: a.Pool(), ServerName: "claude.ai", NextProtos: []string{"h2", "http/1.1"}})
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if p := cli.ConnectionState().NegotiatedProtocol; p != "http/1.1" {
		t.Fatalf("ALPN = %q, want http/1.1", p)
	}
}

// TestTLSHandshakeRejectsAMismatchedSNI pins that ServerTLSConfig, bound to
// the tunnel's CONNECT target, refuses a ClientHello naming a different
// host — no leaf is ever minted for a name the tunnel was not opened to.
// The client's SNI (api.anthropic.com) is itself within the CA's name
// constraints, deliberately: an out-of-constraint SNI like evil.example
// would also be refused by Permits, so it wouldn't tell us the SNI-bind
// check in ServerTLSConfig (rather than the constraint check inside Leaf)
// is what is doing the refusing here.
func TestTLSHandshakeRejectsAMismatchedSNI(t *testing.T) {
	a, _ := ca.LoadOrCreate(t.TempDir())
	cConn, sConn := net.Pipe()
	defer cConn.Close()
	defer sConn.Close()
	errc := make(chan error, 1)
	go func() {
		errc <- tls.Server(sConn, a.ServerTLSConfig("claude.ai")).Handshake()
	}()
	cli := tls.Client(cConn, &tls.Config{RootCAs: a.Pool(), ServerName: "api.anthropic.com"})
	if err := cli.Handshake(); err == nil {
		t.Fatal("handshake with a mismatched SNI succeeded")
	}
	if err := <-errc; err == nil {
		t.Fatal("server-side handshake reported no error for a mismatched SNI")
	}
}

func TestLoadOrCreateNeverReplacesUnreadableCA(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	a, err := ca.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.Chmod(keyPath, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(keyPath, 0o600)
	if _, err := ca.LoadOrCreate(dir); err == nil {
		t.Fatal("unreadable key accepted")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if !bytes.Equal(got, a.CertPEM()) {
		t.Fatal("CA cert was replaced after a read error")
	}
}

func TestLoadOrCreateRefusesHalfPresentCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "ca.key"))
	_, err := ca.LoadOrCreate(dir)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete-CA error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); !os.IsNotExist(err) {
		t.Fatal("a new key was written next to the old cert")
	}
}

func TestLoadOrCreateRejectsMismatchedKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ca.LoadOrCreate(dir)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v, want a key/cert mismatch error", err)
	}
}

func TestLoadOrCreateIncompleteErrorSuggestsRemedy(t *testing.T) {
	dir := t.TempDir()
	if _, err := ca.LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "ca.key"))
	_, err := ca.LoadOrCreate(dir)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete-CA error", err)
	}
	for _, want := range []string{"ca.pem", "ca.key", "restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to mention %q", err, want)
		}
	}
}

func TestLeafHostIsCaseInsensitive(t *testing.T) {
	a, err := ca.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l1, err := a.Leaf("API.Anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	l2, _ := a.Leaf("api.anthropic.com")
	if l1 != l2 {
		t.Fatal("mixed-case host got a separate leaf")
	}
	if got := l1.Leaf.DNSNames; len(got) != 1 || got[0] != "api.anthropic.com" {
		t.Fatalf("DNSNames = %v", got)
	}
}

// TestLoadOrCreateLockedSerialisesConcurrentCreation pins that two processes
// racing to create the CA cannot interleave. Same process, but fsutil.Lock is
// per open file, so two calls exclude each other exactly as two processes do.
func TestLoadOrCreateLockedSerialisesConcurrentCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")

	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := ca.LoadOrCreateLocked(dir)
			if err != nil {
				errs[i] = err
				return
			}
			got[i] = string(a.CertPEM())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if got[i] != got[0] {
			t.Fatalf("goroutine %d got a different CA cert than goroutine 0; generation was not serialised", i)
		}
	}
}

// TestLoadOrCreateLockedReleasesTheLock pins that the lock does not leak: a
// second sequential call must not block.
func TestLoadOrCreateLockedReleasesTheLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	if _, err := ca.LoadOrCreateLocked(dir); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ca.LoadOrCreateLocked(dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second LoadOrCreateLocked blocked: the first call never released ca.lock")
	}
}
