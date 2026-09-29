// Package creds reads an account slot's Claude Code login and classifies its
// token. It never refreshes or copies a token; the M1b daemon does refresh.
package creds

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var ErrNoLogin = errors.New("no Claude login in this directory")

// ErrKeychain means the macOS Keychain denied access to the item: the user
// declined an access prompt or Keychain Access policy refused it outright.
// This is a definitive rejection, not a transient failure. The message is
// operation-neutral (Read and Delete both wrap it) — it must not claim
// "cannot read" when the failing operation was a delete.
var ErrKeychain = errors.New("cannot access the macOS Keychain")

// ErrKeychainUnavailable means /usr/bin/security failed for a reason other
// than a missing item or a denial: a locked keychain, no UI to prompt in
// (e.g. a launchd context or right after wake), security(1) missing, or any
// other unrecognized failure. The problem is expected to clear on retry.
var ErrKeychainUnavailable = errors.New("macOS Keychain temporarily unavailable")

// security(1) exits with the low byte of the underlying OSStatus. Codes
// below are Security framework constants; 51 and 128 (denial) are to be
// confirmed against a live Keychain during gate G1.
const (
	securityItemNotFound          = 44  // errSecItemNotFound: no login → ErrNoLogin.
	securityAuthFailed            = 51  // errSecAuthFailed: user/policy denied access → ErrKeychain.
	securityUserCanceled          = 128 // errSecUserCanceled: user dismissed the prompt → ErrKeychain.
	securityInteractionNotAllowed = 36  // errSecInteractionNotAllowed: locked keychain / no UI → transient, ErrKeychainUnavailable.
)

type exitCoder interface{ ExitCode() int }

// classifyKeychainFailure maps a security(1) failure — anything other than
// "item not found" (exit 44), which Read and Delete each treat differently
// (ErrNoLogin vs success) and so classify themselves before calling this —
// to the sentinel it represents: a definitive denial (ErrKeychain) or a
// failure expected to clear on retry (ErrKeychainUnavailable). Read and
// Delete both call this so the same underlying Security framework failure
// is reported the same way from either operation.
func classifyKeychainFailure(err error) error {
	var ec exitCoder
	if errors.As(err, &ec) {
		switch ec.ExitCode() {
		case securityAuthFailed, securityUserCanceled:
			return ErrKeychain
		}
	}
	return ErrKeychainUnavailable
}

type Token struct {
	AccessToken      string
	ExpiresAt        time.Time
	Scopes           []string
	SubscriptionType string
}

func (t Token) String() string {
	return fmt.Sprintf("Token{expires=%s scopes=%v subscription=%s}", t.ExpiresAt.Format(time.RFC3339), t.Scopes, t.SubscriptionType)
}

func (t Token) GoString() string { return t.String() }

// Format makes every fmt verb (%v, %+v, %#v, %s) use String.
func (t Token) Format(f fmt.State, _ rune) { fmt.Fprint(f, t.String()) }

func (t Token) Valid(now time.Time) bool { return t.AccessToken != "" && now.Before(t.ExpiresAt) }

// Parse decodes Claude Code's credential JSON: {"claudeAiOauth":{...}}.
func Parse(b []byte) (Token, error) {
	var doc struct {
		OAuth *struct {
			AccessToken      string   `json:"accessToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return Token{}, errors.New("credential store is not valid JSON")
	}
	if doc.OAuth == nil || doc.OAuth.AccessToken == "" {
		return Token{}, ErrNoLogin
	}
	return Token{
		AccessToken:      doc.OAuth.AccessToken,
		ExpiresAt:        time.UnixMilli(doc.OAuth.ExpiresAt),
		Scopes:           doc.OAuth.Scopes,
		SubscriptionType: doc.OAuth.SubscriptionType,
	}, nil
}

// KeychainService is the macOS Keychain service name Claude Code uses for a
// non-default CLAUDE_CONFIG_DIR. ASSUMPTION verified by the M0 runbook:
// "Claude Code-credentials-" + first 8 hex chars of sha256(absolute dir).
func KeychainService(absConfigDir string) string {
	sum := sha256.Sum256([]byte(absConfigDir))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

type Runner func(name string, args ...string) ([]byte, error)

// ExecRunner runs name as a subprocess. Callers must pass a trusted, literal
// command path (never attacker- or user-controlled input).
func ExecRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

type Reader struct {
	GOOS            string
	Run             Runner
	ServiceOverride string
}

// Read returns the access token stored for configDir. Errors never include
// store contents.
func (r Reader) Read(configDir string) (Token, error) {
	abs, err := filepath.Abs(configDir)
	if err != nil {
		return Token{}, err
	}
	if r.GOOS == "darwin" {
		svc := r.ServiceOverride
		if svc == "" {
			svc = KeychainService(abs)
		}
		out, err := r.Run("/usr/bin/security", "find-generic-password", "-s", svc, "-w")
		if err != nil {
			// err is safe to include: exec.ExitError.Error() is just "exit
			// status N", Output() keeps stderr out of the message, and
			// stdout (store contents) is never included here.
			var ec exitCoder
			if errors.As(err, &ec) && ec.ExitCode() == securityItemNotFound {
				return Token{}, fmt.Errorf("keychain item %q: %v: %w", svc, err, ErrNoLogin)
			}
			return Token{}, fmt.Errorf("keychain item %q: %v: %w", svc, err, classifyKeychainFailure(err))
		}
		return Parse(bytes.TrimSpace(out))
	}
	b, err := os.ReadFile(filepath.Join(abs, ".credentials.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Token{}, ErrNoLogin
	}
	if err != nil {
		return Token{}, err
	}
	return Parse(b)
}
