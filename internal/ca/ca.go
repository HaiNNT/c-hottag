// Package ca manages chottag's local certificate authority and the per-host
// leaf certificates the proxy presents when it intercepts TLS.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// PermittedDomains is the CA's X.509 name constraint on every CA it mints
// from here on: the set of DNS suffixes chottag ever intercepts. It must
// equal proxy.DefaultTraceSuffixes and cover every host in router.Hosts, or
// a client rejects the leaf (internal/proxy's TestCAConstraintsCoverTheInterceptedHosts
// pins the equality without an import cycle).
var PermittedDomains = []string{"anthropic.com", "claude.ai", "claude.com"}

// ErrKeyPermissions reports a ca.key that someone other than the running
// user could read or replace (security review L1).
var ErrKeyPermissions = errors.New("ca: ca.key is readable or owned by someone else")

// errKeyNotOwnerFixable additionally wraps ErrKeyPermissions for the two
// ca.key shapes doctor --fix refuses to touch on its own — a symlink (or
// any other non-regular file) and a foreign owner — never the third
// shape, an owned regular file with merely the wrong mode bits, which
// --fix does chmod (internal/doctor's caKeySafe/chmodOwnKey re-check the
// identical three shapes). readKeyFile uses it only to choose the right
// startup-error hint (final review N2): "run: chottag doctor --fix" would
// otherwise send a symlinked or foreign-owned ca.key to a fix doctor
// itself declines, exactly as internal/doctor's caCheck (via MoveHint
// below) already knows.
var errKeyNotOwnerFixable = errors.New("ca: this ca.key shape needs to be moved, not chmod'd")

// MoveHint is the advice for a ca/ (or a ca.key within it) doctor --fix
// refuses to touch on its own: a symlinked or foreign-owned ca.key, ca/
// itself unsafe, or half a CA pair. Exported so internal/doctor's caCheck
// can use the identical text instead of a second copy that could drift
// from this one (final re-review, concern (b)): internal/doctor already
// imports this package, so there is no cycle to avoid by duplicating it.
func MoveHint(dir string) string {
	return fmt.Sprintf("move both ca.pem and ca.key out of %s, then run chottag doctor --fix (running sessions need a restart)", dir)
}

// maxLeaves bounds the leaf cache: an attacker who could make the proxy
// mint leaves for unbounded distinct hostnames would otherwise grow it
// without limit.
const maxLeaves = 64

type Authority struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	order  []string
}

// LoadOrCreateLocked is LoadOrCreate serialised across processes by an
// advisory lock on <dir>/ca.lock.
//
// Generation is read-then-generate-then-persist, so two processes racing it
// can leave one holding a cert whose key the other replaced — and
// LoadOrCreate refuses to proceed on a half-written CA, so the symptom is a
// daemon that will not start. The daemon starts at login, which is exactly
// when a user is also likely to run a command by hand, so the race is
// ordinary rather than exotic.
//
// The kernel releases a flock when the process dies, so a crash or reboot
// never leaves this stuck.
func LoadOrCreateLocked(dir string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := fsutil.Lock(filepath.Join(dir, "ca.lock"))
	if err != nil {
		return nil, fmt.Errorf("lock CA dir: %w", err)
	}
	defer unlock()
	return LoadOrCreate(dir)
}

// LoadOrCreate loads ca.pem/ca.key from dir, generating and persisting a new
// CA if either file is missing.
func LoadOrCreate(dir string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	certPEM, errC := os.ReadFile(certPath)
	keyPEM, errK := readKeyFile(keyPath)
	switch {
	case errC == nil && errK == nil:
		return parse(certPEM, keyPEM)
	case errors.Is(errC, fs.ErrNotExist) && errors.Is(errK, fs.ErrNotExist):
		// First run: generate below.
	case errC != nil && !errors.Is(errC, fs.ErrNotExist):
		return nil, fmt.Errorf("read CA cert: %w", errC)
	case errK != nil && !errors.Is(errK, fs.ErrNotExist):
		return nil, fmt.Errorf("read CA key: %w", errK)
	default:
		// Running sessions trust the existing cert; never pair it with a new key.
		return nil, fmt.Errorf("CA in %s is incomplete (ca.pem or ca.key is missing); refusing to replace it "+
			"(move both ca.pem and ca.key away to create a new CA; running sessions will need a restart)", dir)
	}
	certPEM, keyPEM, err := generate()
	if err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return parse(certPEM, keyPEM)
}

func generate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "chottag local CA", Organization: []string{"c-hottag"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         PermittedDomains,
		// RFC 5280 name constraints apply per name type: permitting DNS
		// names says nothing about IP addresses, so a leaf with an IP SAN
		// would otherwise still verify under this CA. Exclude every IPv4
		// and IPv6 address (security review L1): chottag only ever mints
		// leaves for the DNS hosts it intercepts.
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}

func parse(certPEM, keyPEM []byte) (*Authority, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("ca: malformed ca.pem or ca.key")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("ca: CA key does not match CA cert")
	}
	return &Authority{cert: cert, key: key, certPEM: certPEM, leaves: map[string]*tls.Certificate{}, order: []string{}}, nil
}

// checkKeyInfo is readKeyFile's permission/ownership decision, split out so
// the owner-mismatch branch is unit-testable without root or another real
// user: a test can pass any uid, not just one belonging to somebody else on
// the machine. One message per failure mode (security review L1), all
// wrapping ErrKeyPermissions — and, for the two shapes doctor --fix
// refuses to touch (not a regular file, or a foreign owner), also
// errKeyNotOwnerFixable, so readKeyFile can pick the right hint (N2).
func checkKeyInfo(info fs.FileInfo, uid int) error {
	switch {
	case info.Mode()&fs.ModeSymlink != 0 || !info.Mode().IsRegular():
		return fmt.Errorf("%w: %w: is a symlink or not a regular file", ErrKeyPermissions, errKeyNotOwnerFixable)
	case info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%w: has mode %04o (want 0600)", ErrKeyPermissions, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return fmt.Errorf("%w: %w: is owned by uid %d, not you", ErrKeyPermissions, errKeyNotOwnerFixable, st.Uid)
	}
	return nil
}

// readKeyFile refuses a ca.key others could read or replace (security
// review L1) and never repairs: doctor --fix does, visibly (Task 11) — but
// only for a mode-only problem on a file this uid owns; a symlink or a
// foreign owner needs the move hint instead (final review N2), never
// "run: chottag doctor --fix" on its own, which doctor declines for
// either shape (internal/doctor's caCheck, which uses this same MoveHint).
//
// It opens with O_NOFOLLOW rather than checking Lstat and then reading by
// path, so the permission/ownership check and the read are against the
// very same descriptor (no check-then-read race), and a symlink — dangling
// or not — is refused as ErrKeyPermissions instead of surfacing as a
// missing file. O_NONBLOCK keeps a ca.key planted as a FIFO from blocking
// this open forever waiting for a writer that will never come: with it,
// the open returns immediately and checkKeyInfo's IsRegular check refuses
// the FIFO instead.
func readKeyFile(path string) ([]byte, error) {
	dir := filepath.Dir(path)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: %s is a symlink; %s", ErrKeyPermissions, path, MoveHint(dir))
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkKeyInfo(info, os.Getuid()); err != nil {
		hint := "run: chottag doctor --fix"
		if errors.Is(err, errKeyNotOwnerFixable) {
			hint = MoveHint(dir)
		}
		return nil, fmt.Errorf("%w: %s; %s", err, path, hint)
	}
	return io.ReadAll(f)
}

// Constrained reports whether this CA carries name constraints: false for
// a CA generated before this hardening, which permits any host.
func (a *Authority) Constrained() bool {
	return len(a.cert.PermittedDNSDomains) > 0
}

// Permits reports whether this CA is allowed to mint a leaf for host. An
// unconstrained CA (one generated before this hardening) permits any host.
func (a *Authority) Permits(host string) bool {
	if !a.Constrained() {
		return true
	}
	host = strings.ToLower(host)
	for _, d := range a.cert.PermittedDNSDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

func randSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// Leaf returns a cached leaf certificate for host, issuing one if needed.
func (a *Authority) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(host)
	if !a.Permits(host) {
		return nil, fmt.Errorf("ca: %q is outside this CA's name constraints", host)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.leaves[host]; ok && time.Now().Before(c.Leaf.NotAfter.Add(-24*time.Hour)) {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 397),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key, Leaf: leaf}
	if _, existed := a.leaves[host]; !existed {
		if len(a.leaves) >= maxLeaves {
			oldest := a.order[0]
			a.order = a.order[1:]
			delete(a.leaves, oldest)
		}
		a.order = append(a.order, host)
	}
	a.leaves[host] = c
	return c, nil
}

// ServerTLSConfig presents a leaf for host only: the tunnel's CONNECT
// target. A ClientHello naming any other host is refused, so no leaf is
// ever minted for a name the tunnel was not opened to (security review
// L1, M3). An empty SNI gets host's leaf.
func (a *Authority) ServerTLSConfig(host string) *tls.Config {
	host = strings.ToLower(host)
	return &tls.Config{
		// TLS 1.2 minimum: the proxy must stay compatible with any client it
		// fronts, not just modern Go/browser clients that support 1.3.
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if sni := strings.ToLower(h.ServerName); sni != "" && sni != host {
				return nil, fmt.Errorf("ca: refusing a certificate for %q in a tunnel to %q", sni, host)
			}
			return a.Leaf(host)
		},
	}
}

func (a *Authority) CertPEM() []byte { return a.certPEM }

func (a *Authority) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(a.cert)
	return p
}
