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
	"strings"
	"sync"
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

// ErrSessionGone means this process can no longer reach the user's keychains:
// typically the GUI login session it was started in has ended (a logout or a
// WindowServer crash) while the process lived on. It is a kind of
// ErrKeychainUnavailable, so Assess reports stale, never needs-login: the
// logins are fine, only this process cannot see them.
var ErrSessionGone = fmt.Errorf("the login session is gone: %w", ErrKeychainUnavailable)

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

// missWindow is how long a service that read OK earlier in this process may
// keep missing before the miss is believed to be a real lost login.
const missWindow = 60 * time.Second

// nowFunc is the clock for the backstop; tests replace it.
var nowFunc = time.Now

type slotRecord struct {
	lastOK    time.Time
	firstMiss time.Time
}

var (
	stateMu     sync.Mutex
	slots       = map[string]*slotRecord{}
	goneSince   time.Time
	probeArgs   = []string{"list-keychains", "-d", "user"}
	securityBin = "/usr/bin/security"
)

// ResetForTest clears the process-wide read record and the dead-session
// signal. Tests call it so one test's reads never colour another's.
func ResetForTest() {
	stateMu.Lock()
	defer stateMu.Unlock()
	slots = map[string]*slotRecord{}
	goneSince = time.Time{}
}

// SessionGoneSince is the time of the first ErrSessionGone since the last
// successful read of any slot; zero if there has been none.
func SessionGoneSince() time.Time {
	stateMu.Lock()
	defer stateMu.Unlock()
	return goneSince
}

// KeychainReachable reports whether this process can still see the user's
// keychains: `security list-keychains -d user` succeeds and names at least
// one .keychain path. The output is paths only; it is never logged.
func KeychainReachable(run Runner) bool {
	out, err := run(securityBin, probeArgs...)
	return err == nil && strings.Contains(string(out), ".keychain")
}

func noteGone() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if goneSince.IsZero() {
		goneSince = nowFunc()
	}
}

func noteReadOK(svc string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	rec := slots[svc]
	if rec == nil {
		rec = &slotRecord{}
		slots[svc] = rec
	}
	rec.lastOK = nowFunc()
	rec.firstMiss = time.Time{}
	goneSince = time.Time{}
}

// correlatedLocked reports whether two or more services that read OK earlier
// have unresolved first misses within missWindow of each other (no OK since),
// and the earliest first-miss time among them. A dead login session makes
// every slot miss together; a lost login makes one slot miss alone. Caller
// holds stateMu.
func correlatedLocked() (bool, time.Time) {
	var misses []time.Time
	for _, rec := range slots {
		if !rec.lastOK.IsZero() && !rec.firstMiss.IsZero() {
			misses = append(misses, rec.firstMiss)
		}
	}
	var earliest time.Time
	found := false
	for i, a := range misses {
		for j, b := range misses {
			if i != j && a.Sub(b) < missWindow && b.Sub(a) < missWindow {
				found = true
				if earliest.IsZero() || a.Before(earliest) {
					earliest = a
				}
			}
		}
	}
	return found, earliest
}

// SessionGoneCorrelated reports whether the dead-session signal stands on
// correlation: two or more slots that read OK earlier are missing together.
// It holds without the reachability probe failing, for a session where the
// probe cannot see the problem.
func SessionGoneCorrelated() bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	ok, _ := correlatedLocked()
	return ok && !goneSince.IsZero()
}

// ClearSessionGone drops the dead-session signal. The daemon's watcher calls
// it when a fresh probe passes and nothing correlates.
func ClearSessionGone() {
	stateMu.Lock()
	defer stateMu.Unlock()
	if ok, _ := correlatedLocked(); !ok {
		goneSince = time.Time{}
	}
}

// suspectMiss reports whether a not-found or denied result for svc should
// still be doubted: the service read OK earlier in this process and either
// the misses have not yet lasted missWindow, or at least two such services
// are missing together (then the signal is set, and the doubt lasts while
// they do).
func suspectMiss(svc string) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	rec := slots[svc]
	if rec == nil || rec.lastOK.IsZero() {
		return false
	}
	now := nowFunc()
	if rec.firstMiss.IsZero() {
		rec.firstMiss = now
	}
	if ok, earliest := correlatedLocked(); ok {
		if goneSince.IsZero() {
			goneSince = earliest
		}
		return true
	}
	return now.Sub(rec.firstMiss) < missWindow
}

func isMissOrDenied(err error) bool {
	var ec exitCoder
	if !errors.As(err, &ec) {
		return false
	}
	switch ec.ExitCode() {
	case securityItemNotFound, securityAuthFailed, securityUserCanceled:
		return true
	}
	return false
}

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
			if isMissOrDenied(err) {
				if !KeychainReachable(r.Run) {
					noteGone()
					return Token{}, fmt.Errorf("keychain item %q: %v: %w", svc, err, ErrSessionGone)
				}
				if suspectMiss(svc) {
					return Token{}, fmt.Errorf("keychain item %q: %v (read OK earlier, doubted for now): %w", svc, err, ErrKeychainUnavailable)
				}
			}
			var ec exitCoder
			if errors.As(err, &ec) && ec.ExitCode() == securityItemNotFound {
				return Token{}, fmt.Errorf("keychain item %q: %v: %w", svc, err, ErrNoLogin)
			}
			return Token{}, fmt.Errorf("keychain item %q: %v: %w", svc, err, classifyKeychainFailure(err))
		}
		noteReadOK(svc)
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
