package proxyauth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// raw reads the secret's text straight off disk, the one place a test may
// hold it, so it can check that nothing else ever prints it.
func raw(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(proxyauth.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

func TestLoadOrCreateCreatesA0600HexSecretOnce(t *testing.T) {
	home := t.TempDir()
	s1, err := proxyauth.LoadOrCreate(home)
	if err != nil || s1.IsZero() {
		t.Fatalf("LoadOrCreate = %v, %v", s1, err)
	}
	fi, err := os.Stat(proxyauth.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Join(home, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("ca/ mode = %o, want 0700", di.Mode().Perm())
	}
	if v := raw(t, home); len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
		t.Errorf("secret file holds %d chars, want 64 lower-case hex", len(v))
	}
	s2, err := proxyauth.LoadOrCreate(home)
	if err != nil || !s1.Equal(s2) {
		t.Fatal("a second LoadOrCreate must return the same secret")
	}
}

func TestLoadOrCreateConcurrentCallersAgree(t *testing.T) {
	home := t.TempDir()
	const n = 16
	got := make([]proxyauth.Secret, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], _ = proxyauth.LoadOrCreate(home) }()
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if got[i].IsZero() || !got[i].Equal(got[0]) {
			t.Fatalf("caller %d got a different secret: the daemon/shim race must converge on one file", i)
		}
	}
}

func TestLoadNeverCreates(t *testing.T) {
	home := t.TempDir()
	if _, err := proxyauth.Load(home); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load on an empty home = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(filepath.Join(home, "ca")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("Load created ca/")
	}
}

func TestLoadRefusesAWiderModeASymlinkAndBadContent(t *testing.T) {
	home := t.TempDir()
	if _, err := proxyauth.LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	p := proxyauth.Path(home)
	os.Chmod(p, 0o640)
	var pe *proxyauth.PermError
	if _, err := proxyauth.Load(home); !errors.As(err, &pe) || pe.Mode.Perm() != 0o640 {
		t.Fatalf("0640 secret: err = %v, want *PermError with the mode", err)
	}
	if _, err := proxyauth.LoadOrCreate(home); !errors.As(err, &pe) {
		t.Fatalf("LoadOrCreate must refuse, not repair, a 0640 secret: %v", err)
	}
	os.Chmod(p, 0o600)

	target := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(target, []byte(strings.Repeat("a", 64)+"\n"), 0o600)
	os.Remove(p)
	os.Symlink(target, p)
	if _, err := proxyauth.Load(home); !errors.As(err, &pe) || !pe.NotRegular {
		t.Fatalf("symlinked secret: err = %v, want *PermError{NotRegular}", err)
	}
	os.Remove(p)

	for _, bad := range []string{"", "abc\n", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("a", 64) + "\n\n", strings.Repeat("g", 64)} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := proxyauth.Load(home); !errors.Is(err, proxyauth.ErrMalformed) {
			t.Errorf("content %q: err = %v, want ErrMalformed", bad, err)
		}
	}
}

func TestLoadRefusesAWritableCaDir(t *testing.T) {
	home := t.TempDir()
	if _, err := proxyauth.LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "ca")
	os.Chmod(dir, 0o707)
	var pe *proxyauth.PermError
	_, err := proxyauth.Load(home)
	if !errors.As(err, &pe) || pe.NotRegular || pe.ForeignOwner {
		t.Fatalf("world-writable ca/: err = %v, want a plain *PermError", err)
	}
	// D2: the ca/ directory gets its own wording, never the secret file's.
	// The path is dropped first: a random t.TempDir name can contain "0600"
	// (F246).
	if msg := strings.ReplaceAll(err.Error(), dir, "PATH"); !strings.Contains(msg, "group/other-writable") || !strings.Contains(msg, "want 0700") || strings.Contains(msg, "0600") {
		t.Errorf("world-writable ca/ error = %q, want the dir-specific text", msg)
	}
	if _, err := proxyauth.LoadOrCreate(home); !errors.As(err, &pe) {
		t.Fatalf("LoadOrCreate must refuse, not repair, a world-writable ca/: %v", err)
	}
	os.Chmod(dir, 0o700)
}

func TestSecretNeverFormats(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	v := raw(t, home)

	// A Secret nested in an UNEXPORTED struct field has no exported method
	// fmt can reach through reflection (CanInterface is false). fmt's "bad
	// verb" fallback (e.g. %s, %t, %c on a value it has no other rendering
	// for) then dereferences a pointer field and prints the pointee's own
	// fields even though the field holding it is unexported — the leak a
	// plain *secretValue pointer had. Holding the hex behind a func closure
	// closes that: a func is opaque to every verb, at every depth, in every
	// container.
	lower := struct{ s proxyauth.Secret }{s}
	upper := struct{ S proxyauth.Secret }{s}
	slice := []any{s}
	targets := []any{s, lower, upper, slice, &s, &lower, &upper, &slice}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%c", "%e", "%U", "%p", "%T"}
	for _, tg := range targets {
		for _, f := range verbs {
			if out := fmt.Sprintf(f, tg); strings.Contains(out, v) {
				t.Errorf("%s on %T printed the secret: %q", f, tg, out)
			}
		}
	}

	for _, x := range []any{lower, upper} {
		b, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), v) {
			t.Errorf("json.Marshal(%#v) printed the secret: %s", x, b)
		}
	}
}

func TestProxyURLCarriesUserinfo(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	u, err := url.Parse(s.ProxyURL("127.0.0.1:47999"))
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Scheme != "http" || u.Host != "127.0.0.1:47999" || u.User.Username() != proxyauth.User || pw != raw(t, home) {
		t.Fatalf("ProxyURL parsed to %s://%s@%s", u.Scheme, u.User.Username(), u.Host)
	}
	// What Go's Transport (and Bun, F225) send from that userinfo.
	hdr := "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
	if !s.Authorized(hdr) {
		t.Fatal("the header a client builds from ProxyURL's userinfo must be Authorized")
	}
	if got := (proxyauth.Secret{}).ProxyURL("127.0.0.1:47999"); got != "" {
		t.Errorf("a zero Secret's ProxyURL = %q, want \"\"", got)
	}
}

func TestAuthorizedTable(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	o, _ := proxyauth.LoadOrCreate(other)
	v, ov := raw(t, home), raw(t, other)
	b64 := func(x string) string { return base64.StdEncoding.EncodeToString([]byte(x)) }
	cases := []struct {
		name, hdr string
		want      bool
	}{
		{"right", "Basic " + b64("chottag:"+v), true},
		{"scheme case-insensitive", "basic " + b64("chottag:"+v), true},
		// RFC 7235 §2.1: credentials is "auth-scheme [ 1*SP token68 ]" — one
		// or more spaces between the scheme and the credential are valid.
		{"extra spaces after scheme", "Basic   " + b64("chottag:"+v), true},
		{"absent", "", false},
		{"bearer", "Bearer " + b64("chottag:"+v), false},
		{"bad base64", "Basic !!!", false},
		{"wrong user", "Basic " + b64("root:"+v), false},
		{"trailing newline in password", "Basic " + b64("chottag:"+v+"\n"), false},
		{"other install", "Basic " + b64("chottag:"+ov), false},
		{"no credential", "Basic", false},
		{"trailing garbage in password", "Basic " + b64("chottag:"+v+":x"), false},
		{"truncated password", "Basic " + b64("chottag:"+v[:63]), false},
		{"empty password", "Basic " + b64("chottag:"), false},
	}
	for _, c := range cases {
		if got := s.Authorized(c.hdr); got != c.want {
			t.Errorf("%s: Authorized = %v, want %v", c.name, got, c.want)
		}
	}
	if (proxyauth.Secret{}).Authorized("Basic " + b64("chottag:")) {
		t.Error("a zero Secret authorizes nothing")
	}
	_ = o
}

func TestProofBindsSecretNonceAndPort(t *testing.T) {
	const p1, p2 = 12345, 54321
	homeA := t.TempDir()
	a, _ := proxyauth.LoadOrCreate(homeA)
	b, _ := proxyauth.LoadOrCreate(t.TempDir())
	n1, n2 := proxyauth.NewNonce(), proxyauth.NewNonce()
	if !proxyauth.ValidNonce(n1) || n1 == n2 {
		t.Fatalf("NewNonce = %q, %q", n1, n2)
	}
	if !a.VerifyProof(p1, n1, a.Proof(p1, n1)) {
		t.Fatal("a proof must verify under its own secret, port and nonce")
	}
	for name, ok := range map[string]bool{
		"other nonce":  a.VerifyProof(p1, n2, a.Proof(p1, n1)),
		"other secret": a.VerifyProof(p1, n1, b.Proof(p1, n1)),
		// Ruling 32 (final review M2): a proof minted for one port must
		// not verify against another — this is the relay the ruling
		// closes, pinned directly at the HMAC layer.
		"other port":  a.VerifyProof(p2, n1, a.Proof(p1, n1)),
		"empty proof": a.VerifyProof(p1, n1, ""),
		"bad nonce":   a.VerifyProof(p1, "../x", a.Proof(p1, "../x")),
	} {
		if ok {
			t.Errorf("%s verified", name)
		}
	}
	for _, n := range []string{"", "../x", strings.Repeat("a", 31), strings.Repeat("a", 33), strings.Repeat("A", 32)} {
		if proxyauth.ValidNonce(n) {
			t.Errorf("ValidNonce(%q) = true", n)
		}
	}
	if (proxyauth.Secret{}).Proof(p1, n1) != "" {
		t.Error("a zero Secret must produce no proof")
	}
	if got := a.Proof(p1, "../x"); got != "" {
		t.Errorf(`Proof(port, "../x") = %q, want "" (an invalid nonce is never an HMAC oracle)`, got)
	}

	// Pin the format: hex(HMAC-SHA256(key = the 64-hex string as bytes,
	// msg = "chottag-health-v1\n" + decimal(port) + "\n" + nonce)).
	m := hmac.New(sha256.New, []byte(raw(t, homeA)))
	m.Write([]byte("chottag-health-v1\n" + strconv.Itoa(p1) + "\n" + n1))
	want := hex.EncodeToString(m.Sum(nil))
	if got := a.Proof(p1, n1); got != want {
		t.Errorf("Proof(%d, %q) = %q, want %q (the pinned HMAC format, Ruling 32)", p1, n1, got, want)
	}
}

func TestRegenerateReplaces(t *testing.T) {
	home := t.TempDir()
	s1, _ := proxyauth.LoadOrCreate(home)
	s2, err := proxyauth.Regenerate(home)
	if err != nil || s2.Equal(s1) || s2.IsZero() {
		t.Fatalf("Regenerate = %v, %v; want a new secret", s2, err)
	}
	if s3, _ := proxyauth.Load(home); !s3.Equal(s2) {
		t.Fatal("Load after Regenerate must return the new secret")
	}
}

// TestRegenerateUnlessRefusesWhenBusy pins finding 4's refusal: busy true
// leaves the secret untouched and returns ErrBusy.
func TestRegenerateUnlessRefusesWhenBusy(t *testing.T) {
	home := t.TempDir()
	s1, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxyauth.RegenerateUnless(home, func() (bool, error) { return true, nil }); !errors.Is(err, proxyauth.ErrBusy) {
		t.Fatalf("RegenerateUnless(busy=true) error = %v, want ErrBusy", err)
	}
	if s2, err := proxyauth.Load(home); err != nil || !s2.Equal(s1) {
		t.Fatal("RegenerateUnless must not replace the secret while busy")
	}
}

// TestRegenerateUnlessRefusesWhenBusyErrors pins the fail-closed half of
// finding 4: an error from busy is never folded into "not busy" — it
// comes back verbatim, and the secret is untouched.
func TestRegenerateUnlessRefusesWhenBusyErrors(t *testing.T) {
	home := t.TempDir()
	s1, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if _, err := proxyauth.RegenerateUnless(home, func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("RegenerateUnless(busy errors) error = %v, want it to wrap %v", err, boom)
	}
	if s2, err := proxyauth.Load(home); err != nil || !s2.Equal(s1) {
		t.Fatal("RegenerateUnless must not replace the secret when busy itself errors")
	}
}

// TestRegenerateUnlessReplacesWhenNotBusy is the success path: busy false
// still regenerates, exactly like Regenerate.
func TestRegenerateUnlessReplacesWhenNotBusy(t *testing.T) {
	home := t.TempDir()
	s1, err := proxyauth.LoadOrCreate(home)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	s2, err := proxyauth.RegenerateUnless(home, func() (bool, error) { calls++; return false, nil })
	if err != nil || calls != 1 || s2.Equal(s1) || s2.IsZero() {
		t.Fatalf("RegenerateUnless(busy=false) = %v, %v, calls=%d; want a new secret", s2, err, calls)
	}
	if s3, err := proxyauth.Load(home); err != nil || !s3.Equal(s2) {
		t.Fatal("Load after RegenerateUnless must return the new secret")
	}
}

// TestRegenerateUnlessCallsBusyAfterTheLock pins the ordering finding 4's
// whole fix depends on (fix round 2, new issue A): busy must run AFTER
// proxy.secret.lock is already held, never before — moving the busy call
// ahead of the lock would reopen the exact TOCTOU window RegenerateUnless
// exists to close, while every other RegenerateUnless test still passes.
// busy here tries to take the very same lock file itself with a
// non-blocking fsutil.TryLock and asserts it fails: proof the lock was
// already held by RegenerateUnless's own call by the time busy ran.
func TestRegenerateUnlessCallsBusyAfterTheLock(t *testing.T) {
	home := t.TempDir()
	// ca/ must already exist before busy runs, so a moved-too-early busy
	// call's TryLock has a real chance to succeed (ok=true) rather than
	// merely erroring on a missing directory — the strongest form of the
	// failure this test exists to catch.
	if _, err := proxyauth.LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(home, "ca", "proxy.secret.lock")
	var calls int
	_, err := proxyauth.RegenerateUnless(home, func() (bool, error) {
		calls++
		unlock, ok, terr := fsutil.TryLock(lockPath)
		if terr != nil {
			t.Fatalf("TryLock(%s): %v", lockPath, terr)
		}
		if ok {
			unlock()
			t.Fatal("busy could take proxy.secret.lock itself: RegenerateUnless must already hold it before calling busy")
		}
		return false, nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("RegenerateUnless = %v, calls=%d; want no error and exactly one busy call", err, calls)
	}
}
