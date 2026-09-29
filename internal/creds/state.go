package creds

import (
	"errors"
	"path/filepath"
	"time"
)

type TokenState string

const (
	StateOK         TokenState = "ok"
	StateExpiring   TokenState = "expiring"
	StateStale      TokenState = "stale"       // expired or unreadable for now; a refresh may fix it
	StateNeedsLogin TokenState = "needs-login" // only `chottag login` fixes it
)

// ExpiringWithin is how close to expiry a token counts as expiring.
const ExpiringWithin = 5 * time.Minute

type Status struct {
	State  TokenState `json:"state"`
	Reason string     `json:"reason,omitempty"`
}

// Assess classifies the result of Reader.Read at time now. It does not
// refresh: the daemon's refresher turns stale into ok, or into needs-login
// on a definitive rejection.
func Assess(tok Token, err error, now time.Time) Status {
	switch {
	case errors.Is(err, ErrNoLogin):
		return Status{State: StateNeedsLogin, Reason: "no-login"}
	case errors.Is(err, ErrKeychain):
		return Status{State: StateNeedsLogin, Reason: "keychain-denied"}
	case errors.Is(err, ErrKeychainUnavailable):
		return Status{State: StateStale, Reason: "keychain-unavailable"}
	case err != nil:
		return Status{State: StateStale, Reason: "read-error"}
	case !now.Before(tok.ExpiresAt):
		return Status{State: StateStale, Reason: "expired"}
	case tok.ExpiresAt.Sub(now) <= ExpiringWithin:
		return Status{State: StateExpiring}
	}
	return Status{State: StateOK}
}

// LockPath is the per-slot lock file. The daemon holds it while refreshing
// that slot's token and `chottag login` holds it while logging in, so only
// one holder ever uses the slot's single-use refresh token.
func LockPath(slotDir string) string { return filepath.Join(slotDir, ".chottag.lock") }
