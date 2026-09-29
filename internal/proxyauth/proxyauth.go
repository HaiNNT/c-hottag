// Package proxyauth is caller authentication for chottag's local proxy
// (F221, public release design §3): a per-install secret, the
// Proxy-Authorization check, and the health challenge's proof. The secret
// never leaves this package in printable form except through ProxyURL,
// whose one caller hands it to claude's environment (internal/shim).
package proxyauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

const (
	User       = "chottag"      // the userinfo name in HTTPS_PROXY
	FileName   = "proxy.secret" // under <home>/ca/
	NonceParam = "nonce"        // the health query parameter

	// lockName is the secret's own lock, never ca.lock: ca.LoadOrCreateLocked
	// holds ca.lock, and flock on a second open of the same file blocks
	// even inside one process, so sharing it would deadlock any caller that
	// ever nested the two.
	lockName    = "proxy.secret.lock"
	healthLabel = "chottag-health-v1\n" // domain separation: a proof is only ever a health proof
	secretBytes = 32
	nonceBytes  = 16
)

var ErrMalformed = errors.New("proxyauth: the proxy secret file is malformed")

// getuid is a seam over os.Getuid so a test can force the ForeignOwner
// branch of permProblem/checkDirPerm without a second real uid (there is no
// other way to reach it in CI). Production never swaps it.
var getuid = os.Getuid

// PermError is a secret file (or its ca/ directory) that others could read
// or replace: group or other bits set, another owner, or not a regular
// file (a symlink, for the secret; anything but a directory, for ca/). dir
// and uid are unexported: they only steer Error's wording (the ca/
// directory gets its own text, never the secret file's "want 0600"), not
// the frozen exported fields other packages match on.
type PermError struct {
	Path                     string
	Mode                     fs.FileMode
	ForeignOwner, NotRegular bool

	dir bool
	uid int
}

func (e *PermError) Error() string {
	switch {
	case e.dir && e.NotRegular:
		return fmt.Sprintf("proxyauth: %s is not a directory; run: chottag doctor --fix", e.Path)
	case e.dir && e.ForeignOwner:
		return fmt.Sprintf("proxyauth: %s is owned by uid %d; run: chottag doctor --fix", e.Path, e.uid)
	case e.dir:
		return fmt.Sprintf("proxyauth: %s is group/other-writable (mode %04o), want 0700; run: chottag doctor --fix", e.Path, e.Mode.Perm())
	case e.NotRegular:
		return fmt.Sprintf("proxyauth: %s is not a regular file; run: chottag doctor --fix", e.Path)
	case e.ForeignOwner:
		return fmt.Sprintf("proxyauth: %s is owned by another user; run: chottag doctor --fix", e.Path)
	}
	return fmt.Sprintf("proxyauth: %s has mode %04o, want 0600; run: chottag doctor --fix", e.Path, e.Mode.Perm())
}

// Secret is 32 random bytes held as their 64-char lower-case hex, which is
// both the userinfo password and the HMAC key. The hex lives only inside a
// closure: a func value is opaque to every fmt verb — including the "bad
// verb" fallback (e.g. %s, %t, %c on a value fmt doesn't otherwise know how
// to render), which dereferences a pointer field and prints the pointee's
// fields even when the field holding it is unexported — and to
// reflection-based dumpers, which cannot call through a func either. This
// also makes Secret deliberately NON-COMPARABLE: == on two Secrets is a
// compile error, not a silent pointer-identity or timing-unsafe comparison;
// use Equal. A zero Secret has v == nil. It never formats its value.
type Secret struct{ v func() string }

func newSecret(hexStr string) Secret { return Secret{v: func() string { return hexStr }} }

func Path(home string) string { return filepath.Join(home, "ca", FileName) }

func Load(home string) (Secret, error) {
	dir := filepath.Join(home, "ca")
	if err := checkDirPerm(dir); err != nil {
		return Secret{}, err
	}
	p := Path(home)
	// O_NOFOLLOW refuses a symlink at the open itself, and O_NONBLOCK keeps
	// a FIFO planted at p from hanging the open: both close the gap a
	// separate Lstat-then-Open would leave for something to be swapped in
	// between the check and the read.
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return Secret{}, &PermError{Path: p, NotRegular: true}
		}
		return Secret{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Secret{}, err
	}
	if pe := permProblem(p, info); pe != nil {
		return Secret{}, pe
	}
	// The permission check (f.Stat) and the read below both run on the
	// same already-open fd, never a second path lookup.
	b, err := io.ReadAll(io.LimitReader(f, 2*secretBytes+2))
	if err != nil {
		return Secret{}, err
	}
	v := strings.TrimSuffix(string(b), "\n")
	if !isLowerHex(v, 2*secretBytes) {
		return Secret{}, fmt.Errorf("%w: %s", ErrMalformed, p)
	}
	return newSecret(v), nil
}

// checkDirPerm refuses a ca/ directory that others could write into
// (planting a symlink or FIFO at the secret's path moments before Load
// opens it) or that belongs to another user.
func checkDirPerm(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &PermError{Path: dir, Mode: info.Mode(), NotRegular: true, dir: true}
	}
	if info.Mode().Perm()&0o022 != 0 { // group- or other-writable
		return &PermError{Path: dir, Mode: info.Mode(), dir: true}
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		return &PermError{Path: dir, Mode: info.Mode(), ForeignOwner: true, dir: true, uid: int(st.Uid)}
	}
	return nil
}

func permProblem(p string, info fs.FileInfo) *PermError {
	pe := &PermError{Path: p, Mode: info.Mode()}
	switch {
	case !info.Mode().IsRegular():
		pe.NotRegular = true
	case info.Mode().Perm()&0o077 != 0:
	default:
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) == getuid() {
			return nil
		}
		pe.ForeignOwner = true
	}
	return pe
}

func LoadOrCreate(home string) (Secret, error) {
	unlock, err := lock(home)
	if err != nil {
		return Secret{}, err
	}
	defer unlock()
	s, err := Load(home)
	if !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	return create(home)
}

func Regenerate(home string) (Secret, error) {
	unlock, err := lock(home)
	if err != nil {
		return Secret{}, err
	}
	defer unlock()
	return create(home)
}

// ErrBusy is RegenerateUnless's refusal when busy itself reported true. A
// busy that ERRORS is never folded into this: RegenerateUnless returns
// that error verbatim (the same fail-closed rule doctor's own
// daemonHoldsLock callers already follow), since the caller's state is
// genuinely unknown, never guessed as "not busy".
var ErrBusy = errors.New("proxyauth: refused to regenerate the secret; busy reported it unsafe to")

// RegenerateUnless is Regenerate, but busy — called AFTER proxy.secret.lock
// is held, never before — decides whether it is safe to proceed: true, or
// an error from busy itself, refuses rather than writing a new secret
// (T11 fix round 1, finding 4).
//
// This closes a TOCTOU window a caller's own PRE-lock probe would
// otherwise leave open: doctor's daemonHoldsLock, checked before this
// call, can see "no daemon" a moment before a daemon actually starts
// concurrently, taking daemon.lock and loading the still-current secret —
// Regenerate would then write a new one out from under it. The daemon
// always takes daemon.lock before it ever loads or creates the secret, so
// a busy probe re-run under proxy.secret.lock can never miss that: either
// the daemon's own daemon.lock Acquire already happened by the time busy
// runs here, or this call holds proxy.secret.lock first and the daemon's
// own LoadOrCreate blocks on it until this call is done.
func RegenerateUnless(home string, busy func() (bool, error)) (Secret, error) {
	unlock, err := lock(home)
	if err != nil {
		return Secret{}, err
	}
	defer unlock()
	b, err := busy()
	if err != nil {
		return Secret{}, err
	}
	if b {
		return Secret{}, ErrBusy
	}
	return create(home)
}

func lock(home string) (func() error, error) {
	dir := filepath.Join(home, "ca")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	unlock, err := fsutil.Lock(filepath.Join(dir, lockName))
	if err != nil {
		return nil, fmt.Errorf("lock the proxy secret: %w", err)
	}
	return unlock, nil
}

func create(home string) (Secret, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return Secret{}, err
	}
	s := newSecret(hex.EncodeToString(b))
	if err := fsutil.WriteFileAtomic(Path(home), []byte(s.v()+"\n"), 0o600); err != nil {
		return Secret{}, err
	}
	return s, nil
}

func NewNonce() string {
	b := make([]byte, nonceBytes)
	rand.Read(b) // never fails (crypto/rand, Go 1.24+); NewNonce's signature is frozen for its callers, so it has nowhere to return an error to.
	return hex.EncodeToString(b)
}

func ValidNonce(n string) bool { return isLowerHex(n, 2*nonceBytes) }

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s Secret) IsZero() bool               { return s.v == nil }
func (s Secret) String() string             { return "proxyauth.Secret(redacted)" }
func (s Secret) GoString() string           { return s.String() }
func (s Secret) Format(f fmt.State, _ rune) { fmt.Fprint(f, s.String()) }

// Equal reports whether s and o are the same secret, compared in constant
// time. Two zero Secrets are equal; a zero and a non-zero Secret are not.
func (s Secret) Equal(o Secret) bool {
	if s.IsZero() || o.IsZero() {
		return s.IsZero() && o.IsZero()
	}
	return subtle.ConstantTimeCompare([]byte(s.v()), []byte(o.v())) == 1
}

// ProxyURL is the HTTPS_PROXY value for a proxy at hostport, the secret in
// its userinfo. url.UserPassword percent-encodes as needed (hex needs
// none). A zero Secret has no credential to hand a client, so this is "".
func (s Secret) ProxyURL(hostport string) string {
	if s.IsZero() {
		return ""
	}
	return (&url.URL{Scheme: "http", User: url.UserPassword(User, s.v()), Host: hostport}).String()
}

// Authorized reports whether a Proxy-Authorization value is Basic
// chottag:<this secret>, compared in constant time. A zero Secret
// authorizes nothing. RFC 7235 §2.1's credentials grammar allows one or
// more spaces between the scheme and the token: strings.Cut below consumes
// only the first, and TrimSpace absorbs any that remain.
func (s Secret) Authorized(h string) bool {
	if s.IsZero() {
		return false
	}
	scheme, cred, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cred))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, []byte(User+":"+s.v())) == 1
}

// Proof is hex(HMAC-SHA256(secret, "chottag-health-v1\n" + decimal(port) +
// "\n" + nonce)); "" for a zero secret or an invalid nonce, so the health
// endpoint is never an HMAC oracle over arbitrary caller-supplied input.
//
// port binds the proof to the daemon's own local listening port (Ruling
// 32, final review M2): without it, a local squatter on the port the shim
// actually probed could relay the shim's nonce to a DIFFERENT, same-secret
// listener (a stopped daemon's old port, a foreground `proxy run`, a
// leftover `trace run`) and hand back THAT listener's valid proof. The
// encoding is unambiguous: healthLabel already ends in "\n", the decimal
// port is followed by its own "\n", and nonce is always exactly
// 2*nonceBytes lower-case hex characters (ValidNonce) — so no port/nonce
// byte string ever produces two different (port, nonce) pairs.
func (s Secret) Proof(port int, nonce string) string {
	if s.IsZero() || !ValidNonce(nonce) {
		return ""
	}
	m := hmac.New(sha256.New, []byte(s.v()))
	m.Write([]byte(healthLabel + strconv.Itoa(port) + "\n" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyProof checks proof against port — the port THIS caller actually
// probed, never a port named anywhere in the request or response (F221,
// Ruling 32): that is what stops a relayed proof from a different,
// same-secret listener from verifying here.
func (s Secret) VerifyProof(port int, nonce, proof string) bool {
	if s.IsZero() || !ValidNonce(nonce) || proof == "" {
		return false
	}
	return hmac.Equal([]byte(s.Proof(port, nonce)), []byte(proof))
}
