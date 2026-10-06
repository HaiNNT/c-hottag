package creds_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
)

const blob = `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-SECRET","refreshToken":"sk-ant-ort01-SECRET","expiresAt":1900000000000,"scopes":["user:inference","user:profile"],"subscriptionType":"max"}}`

func TestParse(t *testing.T) {
	tok, err := creds.Parse([]byte(blob))
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "sk-ant-oat01-SECRET" || tok.SubscriptionType != "max" ||
		!tok.ExpiresAt.Equal(time.UnixMilli(1900000000000)) ||
		!reflect.DeepEqual(tok.Scopes, []string{"user:inference", "user:profile"}) {
		t.Fatalf("got %+v", tok)
	}
	if !tok.Valid(time.UnixMilli(1800000000000)) || tok.Valid(time.UnixMilli(1950000000000)) {
		t.Fatal("Valid wrong")
	}
}

func TestParseNoLogin(t *testing.T) {
	if _, err := creds.Parse([]byte(`{}`)); !errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("err = %v", err)
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	tok, _ := creds.Parse([]byte(blob))
	for _, s := range []string{tok.String(), fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok)} {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("token printed: %s", s)
		}
	}
}

func TestKeychainService(t *testing.T) {
	s := creds.KeychainService("/Users/x/.chottag/accounts/B")
	if !strings.HasPrefix(s, "Claude Code-credentials-") || len(s) != len("Claude Code-credentials-")+8 {
		t.Fatalf("service = %q", s)
	}
	if s != creds.KeychainService("/Users/x/.chottag/accounts/B") {
		t.Fatal("not deterministic")
	}
}

func TestReaderDarwinUsesSecurity(t *testing.T) {
	var gotArgs []string
	r := creds.Reader{GOOS: "darwin", Run: func(name string, args ...string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		return []byte(blob + "\n"), nil
	}}
	dir := t.TempDir()
	tok, err := r.Read(dir)
	if err != nil || tok.AccessToken != "sk-ant-oat01-SECRET" {
		t.Fatalf("tok %v err %v", tok, err)
	}
	want := []string{"/usr/bin/security", "find-generic-password", "-s", creds.KeychainService(dir), "-w"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args %v, want %v", gotArgs, want)
	}
}

// exitErr mimics *exec.ExitError: an error with an exit code.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

// withProbe answers the reachability probe as healthy and sends every other
// command to run.
func withProbe(run creds.Runner) creds.Runner {
	return func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-keychains" {
			return []byte("    \"/Users/alice/Library/Keychains/login.keychain-db\"\n"), nil
		}
		return run(name, args...)
	}
}

func TestReaderDarwinOverrideAndMissing(t *testing.T) {
	creds.ResetForTest()
	var svc string
	r := creds.Reader{GOOS: "darwin", ServiceOverride: "Custom", Run: withProbe(func(name string, args ...string) ([]byte, error) {
		svc = args[2]
		return nil, exitErr(44)
	})}
	_, err := r.Read(t.TempDir())
	if !errors.Is(err, creds.ErrNoLogin) || errors.Is(err, creds.ErrKeychain) || svc != "Custom" {
		t.Fatalf("err %v svc %q", err, svc)
	}
	if !strings.Contains(err.Error(), "exit status 44") {
		t.Fatalf("err = %v, want it to contain underlying cause", err)
	}
}

func TestReaderDarwinFailureClassification(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr error
	}{
		{"authFailed denial", exitErr(51), creds.ErrKeychain},
		{"userCanceled denial", exitErr(128), creds.ErrKeychain},
		{"interactionNotAllowed transient", exitErr(36), creds.ErrKeychainUnavailable},
		{"exec failure transient", errors.New("exec: security: not found"), creds.ErrKeychainUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			creds.ResetForTest()
			r := creds.Reader{GOOS: "darwin", Run: withProbe(func(string, ...string) ([]byte, error) { return nil, c.err })}
			_, err := r.Read(t.TempDir())
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("run error %v -> %v, want wrapping %v", c.err, err, c.wantErr)
			}
			other := creds.ErrKeychain
			if c.wantErr == creds.ErrKeychain {
				other = creds.ErrKeychainUnavailable
			}
			if errors.Is(err, other) {
				t.Fatalf("run error %v -> %v, must not also wrap %v", c.err, err, other)
			}
			if errors.Is(err, creds.ErrNoLogin) {
				t.Fatalf("run error %v -> %v, must not wrap ErrNoLogin", c.err, err)
			}
		})
	}
}

func TestReaderLinuxReadsFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600)
	tok, err := creds.Reader{GOOS: "linux"}.Read(dir)
	if err != nil || tok.SubscriptionType != "max" {
		t.Fatalf("tok %v err %v", tok, err)
	}
	if _, err := (creds.Reader{GOOS: "linux"}).Read(t.TempDir()); !errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("missing file err = %v", err)
	}
}

// probeResult builds a Runner whose find-generic-password exits with find and
// whose list-keychains probe returns probeOut/probeErr.
func probeRunner(find error, probeOut string, probeErr error) creds.Runner {
	return func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list-keychains" {
			return []byte(probeOut), probeErr
		}
		return nil, find
	}
}

const keychainList = "    \"/Users/alice/Library/Keychains/login.keychain-db\"\n"

func TestReadDeadSessionIsNotNoLogin(t *testing.T) {
	cases := []struct {
		name     string
		find     error
		probeOut string
		probeErr error
		want     error
	}{
		{"44 probe ok", exitErr(44), keychainList, nil, creds.ErrNoLogin},
		{"44 probe errors", exitErr(44), "", exitErr(1), creds.ErrSessionGone},
		{"44 probe lists nothing", exitErr(44), "\n", nil, creds.ErrSessionGone},
		{"51 probe errors", exitErr(51), "", exitErr(1), creds.ErrSessionGone},
		{"128 probe errors", exitErr(128), "", exitErr(1), creds.ErrSessionGone},
		{"51 probe ok", exitErr(51), keychainList, nil, creds.ErrKeychain},
		{"36 unchanged", exitErr(36), "", exitErr(1), creds.ErrKeychainUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			creds.ResetForTest()
			_, err := creds.Reader{GOOS: "darwin", Run: probeRunner(c.find, c.probeOut, c.probeErr)}.Read(t.TempDir())
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if c.want == creds.ErrSessionGone {
				if !errors.Is(err, creds.ErrKeychainUnavailable) || errors.Is(err, creds.ErrNoLogin) || errors.Is(err, creds.ErrKeychain) {
					t.Fatalf("ErrSessionGone must be unavailable only: %v", err)
				}
				if !strings.Contains(err.Error(), c.find.Error()) {
					t.Fatalf("err %q lacks %q", err, c.find)
				}
				if st := creds.Assess(creds.Token{}, err, time.Now()); st.State != creds.StateStale {
					t.Fatalf("Assess = %+v, want stale", st)
				}
			}
		})
	}
}

func TestReadBackstopDoubtsAMissAfterAnEarlierOK(t *testing.T) {
	creds.ResetForTest()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	defer creds.SetNowForTest(func() time.Time { return now })()
	dir := t.TempDir()
	var find error
	r := creds.Reader{GOOS: "darwin", Run: func(name string, args ...string) ([]byte, error) {
		if args[0] == "list-keychains" {
			return []byte(keychainList), nil
		}
		if find != nil {
			return nil, find
		}
		return []byte(blob), nil
	}}
	read := func() error { _, err := r.Read(dir); return err }

	if err := read(); err != nil {
		t.Fatal(err)
	}
	find = exitErr(44)
	if err := read(); !errors.Is(err, creds.ErrKeychainUnavailable) || errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("first miss = %v, want suspect", err)
	}
	now = now.Add(30 * time.Second)
	if err := read(); !errors.Is(err, creds.ErrKeychainUnavailable) {
		t.Fatalf("miss at +30s = %v, want suspect", err)
	}
	now = now.Add(31 * time.Second)
	if err := read(); !errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("miss at +61s = %v, want ErrNoLogin", err)
	}
	// OK, miss, OK, miss: each OK clears the miss.
	find = nil
	if err := read(); err != nil {
		t.Fatal(err)
	}
	find = exitErr(44)
	if err := read(); !errors.Is(err, creds.ErrKeychainUnavailable) {
		t.Fatalf("miss after OK = %v, want suspect", err)
	}
	find = nil
	if err := read(); err != nil {
		t.Fatal(err)
	}
	find = exitErr(51)
	if err := read(); !errors.Is(err, creds.ErrKeychainUnavailable) {
		t.Fatalf("denied after OK = %v, want suspect", err)
	}
}

func TestReadNeverReadSlotIsNoLoginImmediately(t *testing.T) {
	creds.ResetForTest()
	_, err := creds.Reader{GOOS: "darwin", Run: probeRunner(exitErr(44), keychainList, nil)}.Read(t.TempDir())
	if !errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionGoneSince(t *testing.T) {
	creds.ResetForTest()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	defer creds.SetNowForTest(func() time.Time { return now })()
	if !creds.SessionGoneSince().IsZero() {
		t.Fatal("want zero at start")
	}
	gone := creds.Reader{GOOS: "darwin", Run: probeRunner(exitErr(44), "", exitErr(1))}
	if _, err := gone.Read(t.TempDir()); !errors.Is(err, creds.ErrSessionGone) {
		t.Fatal(err)
	}
	first := creds.SessionGoneSince()
	if !first.Equal(now) {
		t.Fatalf("since = %v, want %v", first, now)
	}
	now = now.Add(time.Minute)
	gone.Read(t.TempDir())
	if !creds.SessionGoneSince().Equal(first) {
		t.Fatal("a later ErrSessionGone moved the first-seen time")
	}
	ok := creds.Reader{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) { return []byte(blob), nil }}
	if _, err := ok.Read(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !creds.SessionGoneSince().IsZero() {
		t.Fatal("a successful read must reset the signal")
	}
}

func TestKeychainReachable(t *testing.T) {
	if !creds.KeychainReachable(probeRunner(nil, keychainList, nil)) {
		t.Fatal("healthy probe reported unreachable")
	}
	if creds.KeychainReachable(probeRunner(nil, "", exitErr(1))) || creds.KeychainReachable(probeRunner(nil, "none\n", nil)) {
		t.Fatal("failed probe reported reachable")
	}
}

// twoSlots reads two slots through one runner whose find result is switchable
// per slot; the probe always passes (a session where it cannot see the
// problem).
type twoSlots struct {
	dirs [2]string
	miss [2]bool
	r    creds.Reader
}

func newTwoSlots(t *testing.T) *twoSlots {
	g := &twoSlots{dirs: [2]string{t.TempDir(), t.TempDir()}}
	g.r = creds.Reader{GOOS: "darwin", Run: func(name string, args ...string) ([]byte, error) {
		if args[0] == "list-keychains" {
			return []byte(keychainList), nil
		}
		for i, d := range g.dirs {
			abs, _ := filepath.Abs(d)
			if args[2] == creds.KeychainService(abs) && g.miss[i] {
				return nil, exitErr(44)
			}
		}
		return []byte(blob), nil
	}}
	return g
}

func (g *twoSlots) read(i int) error { _, err := g.r.Read(g.dirs[i]); return err }

func TestCorrelatedMissesStayDoubtedAndSetTheSignal(t *testing.T) {
	creds.ResetForTest()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	defer creds.SetNowForTest(func() time.Time { return now })()
	g := newTwoSlots(t)
	g.read(0)
	g.read(1)
	g.miss = [2]bool{true, true}
	t0 := now
	if err := g.read(0); !errors.Is(err, creds.ErrKeychainUnavailable) {
		t.Fatalf("first miss = %v", err)
	}
	now = now.Add(10 * time.Second)
	if err := g.read(1); !errors.Is(err, creds.ErrKeychainUnavailable) {
		t.Fatalf("second miss = %v", err)
	}
	if got := creds.SessionGoneSince(); !got.Equal(t0) || !creds.SessionGoneCorrelated() {
		t.Fatalf("since = %v correlated = %v, want %v and true", got, creds.SessionGoneCorrelated(), t0)
	}
	// Well past the 60 s window, both stay doubted.
	now = now.Add(5 * time.Minute)
	for i := 0; i < 2; i++ {
		if err := g.read(i); !errors.Is(err, creds.ErrKeychainUnavailable) || errors.Is(err, creds.ErrNoLogin) {
			t.Fatalf("slot %d at +5m = %v, want still suspect", i, err)
		}
	}
	// One reads OK again: correlation ends and the other resolves by the window.
	g.miss[1] = false
	if err := g.read(1); err != nil {
		t.Fatal(err)
	}
	if creds.SessionGoneCorrelated() || !creds.SessionGoneSince().IsZero() {
		t.Fatal("correlation must end when a slot reads OK")
	}
	// Slot 0's first miss is minutes old and it is alone now: the real
	// classification applies.
	if err := g.read(0); !errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("slot 0 alone = %v, want ErrNoLogin", err)
	}
}

func TestAloneMissStillBecomesNoLoginAfterAMinute(t *testing.T) {
	creds.ResetForTest()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	defer creds.SetNowForTest(func() time.Time { return now })()
	g := newTwoSlots(t)
	g.read(0)
	g.read(1)
	g.miss[0] = true
	g.read(0)
	now = now.Add(61 * time.Second)
	if err := g.read(0); !errors.Is(err, creds.ErrNoLogin) {
		t.Fatalf("err = %v, want ErrNoLogin", err)
	}
	if creds.SessionGoneCorrelated() || !creds.SessionGoneSince().IsZero() {
		t.Fatal("a lone miss must not signal")
	}
}

func TestMissesFarApartDoNotCorrelate(t *testing.T) {
	creds.ResetForTest()
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	defer creds.SetNowForTest(func() time.Time { return now })()
	g := newTwoSlots(t)
	g.read(0)
	g.read(1)
	g.miss[0] = true
	g.read(0)
	now = now.Add(time.Hour)
	g.miss[1] = true
	if err := g.read(1); !errors.Is(err, creds.ErrKeychainUnavailable) || creds.SessionGoneCorrelated() {
		t.Fatalf("err = %v correlated = %v, want a lone first miss", err, creds.SessionGoneCorrelated())
	}
}
