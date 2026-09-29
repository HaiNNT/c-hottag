package creds_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
)

func TestAssess(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	tok := func(d time.Duration) creds.Token { return creds.Token{AccessToken: "x", ExpiresAt: now.Add(d)} }
	cases := []struct {
		name string
		tok  creds.Token
		err  error
		want creds.Status
	}{
		{"fresh", tok(time.Hour), nil, creds.Status{State: creds.StateOK}},
		{"expiring", tok(4 * time.Minute), nil, creds.Status{State: creds.StateExpiring}},
		{"expired", tok(-time.Second), nil, creds.Status{State: creds.StateStale, Reason: "expired"}},
		{"exactly now", tok(0), nil, creds.Status{State: creds.StateStale, Reason: "expired"}},
		{"no login", creds.Token{}, fmt.Errorf("x: %w", creds.ErrNoLogin), creds.Status{State: creds.StateNeedsLogin, Reason: "no-login"}},
		{"keychain", creds.Token{}, fmt.Errorf("x: %w", creds.ErrKeychain), creds.Status{State: creds.StateNeedsLogin, Reason: "keychain-denied"}},
		{"other read error", creds.Token{}, errors.New("permission denied"), creds.Status{State: creds.StateStale, Reason: "read-error"}},
		{"keychain unavailable", creds.Token{}, fmt.Errorf("x: %w", creds.ErrKeychainUnavailable), creds.Status{State: creds.StateStale, Reason: "keychain-unavailable"}},
	}
	for _, c := range cases {
		if got := creds.Assess(c.tok, c.err, now); got != c.want {
			t.Errorf("%s: Assess = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestLockPath(t *testing.T) {
	if got := creds.LockPath("/h/.chottag/accounts/B"); got != "/h/.chottag/accounts/B/.chottag.lock" {
		t.Fatalf("LockPath = %s", got)
	}
}
