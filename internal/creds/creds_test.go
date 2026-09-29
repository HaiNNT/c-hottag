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

func TestReaderDarwinOverrideAndMissing(t *testing.T) {
	var svc string
	r := creds.Reader{GOOS: "darwin", ServiceOverride: "Custom", Run: func(name string, args ...string) ([]byte, error) {
		svc = args[2]
		return nil, exitErr(44)
	}}
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
			r := creds.Reader{GOOS: "darwin", Run: func(string, ...string) ([]byte, error) { return nil, c.err }}
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
