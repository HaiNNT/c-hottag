// Package store owns ~/.chottag/state.json: the registered accounts, which
// one serves requests, which one owns claude.ai objects, and daemon settings.
// Readers load without locking (writes are atomic renames); every change goes
// through Update, which holds an flock across read-modify-write.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

const (
	Version     = 1
	DefaultPort = 47821
)

var (
	ErrNotFound   = errors.New("no such account")
	ErrAmbiguous  = errors.New("ambiguous account name")
	ErrExists     = errors.New("account already exists")
	ErrInUse      = errors.New("account is the serving or remote account")
	ErrNoAccounts = errors.New("no accounts registered")
	// ErrServingChanged is SwapServing's lost compare-and-swap: serving was
	// no longer the account the swap expected (M4 spec §5, S10).
	ErrServingChanged = errors.New("serving changed")
)

type Account struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	// Org is the organization name, for display only. Two accounts can share
	// an email and differ only by org (F16), so status shows both. It is
	// never an identity key and never comes from a response header.
	Org string `json:"org,omitempty"`
	// Dir is the slot's CLAUDE_CONFIG_DIR. It is fixed when the account is
	// created and never follows a rename: the macOS Keychain item holding the
	// login is named after this path.
	Dir     string    `json:"dir"`
	AddedAt time.Time `json:"addedAt"`
	// NoRotate keeps this account out of `next` and auto-switch (spec §5,
	// R29). It can still be the remote account and still accepts an
	// explicit `chottag tag`.
	NoRotate bool `json:"noRotate,omitempty"`
	// Plan is the account's plan tier for auto-switch (M4 spec §2):
	// "pro", "max5x", "max20x" or "team", set by `chottag plan`, or "max"
	// when login/adopt saw a Max plan whose size it cannot tell (F200, S2).
	// Empty until known. store does not validate it; internal/autoswitch
	// owns the tier names.
	Plan string `json:"plan,omitempty"`
	// Units overrides the tier's capacity units (`chottag plan … --units
	// N`). 0 means the tier's own.
	Units int `json:"units,omitempty"`
	// LoggedInAt is when `chottag login` or `chottag adopt` last confirmed
	// this account logged in. internal/cli's planAccounts compares it
	// against status.json's TokenAt to tell a needs-login row that predates
	// this login (cleared) from one recorded after it (still needs one) —
	// fix round 1 item 1, superseding plan ruling 3: a stale needs-login
	// must not outlive a re-login. omitzero: never logged in via chottag
	// carries no LoggedInAt.
	LoggedInAt time.Time `json:"loggedInAt,omitzero"`
}

// Rotates reports whether `next` and auto-switch may select this account.
func (a Account) Rotates() bool { return !a.NoRotate }

// Auto is the auto-switch settings (M4 spec §7). Every field is omitted
// until set, and absent means the default: on, mode balanced, the §3
// switch points (S1). store keeps the values as written and validates
// nothing; internal/autoswitch owns the names and the bounds.
type Auto struct {
	// Enabled is a pointer because absent must read as on (R68).
	Enabled      *bool          `json:"enabled,omitempty"`
	Mode         string         `json:"mode,omitempty"`
	SwitchPoints map[string]int `json:"switchPoints,omitempty"`
	Hold5h       string         `json:"hold5h,omitempty"`
	Hold7d       string         `json:"hold7d,omitempty"`
	Cooldown     string         `json:"cooldown,omitempty"`
	// Threshold is the pre-M4 field. Every state.json written before M4
	// holds {"enabled": false, "threshold": 95}, which no command ever set,
	// so Load drops a block that carries it: it records no choice, and
	// reading its enabled:false would turn auto-switch off for everyone
	// (R68). Never written.
	Threshold int `json:"threshold,omitempty"`
}

// Trace is the daemon's trace-mode switch (M2c spec §2, T3): tracing is on
// while Until is in the future. Nothing clears it when the window ends;
// the daemon never writes state.json, and `chottag trace off` removes it.
type Trace struct {
	Until time.Time `json:"until"`
}

// Updates holds the update switches (R124, R126). All are pointers so an
// absent key reads as the default: check on, auto-install off, restart on.
type Updates struct {
	Check   *bool `json:"check,omitempty"`   // nil = true
	Auto    *bool `json:"auto,omitempty"`    // nil = false
	Restart *bool `json:"restart,omitempty"` // nil = true
}

type State struct {
	Version    int       `json:"version"`
	Accounts   []Account `json:"accounts"` // registration order is the `next` order
	Serving    string    `json:"serving,omitempty"`
	Remote     string    `json:"remote,omitempty"`
	Port       int       `json:"port"`
	RealClaude string    `json:"realClaude,omitempty"`
	// Auto is nil until an auto command sets something (M4 spec §7).
	Auto *Auto `json:"auto,omitempty"`
	// Notify is the desktop-notification switch (M2 spec §4, D12). It is a
	// pointer because absent must read as on: nil, which is also what an
	// explicit JSON null decodes to, is on. `chottag notify off` writes
	// false, and `notify on` writes true.
	Notify *bool `json:"notify,omitempty"`
	// Trace is the trace-mode window (M2c). nil (also what an explicit JSON
	// null decodes to) is off. A pointer so a state that never traced
	// writes no key.
	Trace *Trace `json:"trace,omitempty"`
	// Label names this install in notification titles and `chottag status`
	// (R118): `setup --label dev` marks a dev sandbox. Empty is unlabelled.
	Label string `json:"label,omitempty"`
	// Updates is the update-check settings (R124). Zero writes no key.
	Updates Updates `json:"updates,omitzero"`
}

func Default() State {
	return State{Version: Version, Port: DefaultPort}
}

// ResolvedPort returns the port the daemon must bind. It never searches.
//
// A deterministic port is what lets the shim find the daemon without a
// discovery file and without a staleness window (§4.8): the shim runs before
// every `claude` invocation, so "where is the daemon" has to be answerable
// immediately and unambiguously.
//
// This replaces a scan of port+1..port+64. That scan could leave the daemon
// on any of 65 ports with no authoritative record of which, because the
// resolved port was never written back to state.json — and the guard meant
// to prevent it (refuse to move while managed sessions are alive) could
// never fire, since nothing called session.Add until the shim existed. A
// taken port is now a startup failure naming the port, which an operator can
// act on directly.
//
// It lives in store, not cli, because internal/shim needs the identical rule
// and cannot reach an unexported cli helper (F103). One rule, two callers.
func (s State) ResolvedPort() int {
	if s.Port == 0 {
		return DefaultPort
	}
	return s.Port
}

// NotifyOn reports whether desktop notifications are on. Absent is on (D12).
func (s State) NotifyOn() bool { return s.Notify == nil || *s.Notify }

// SetNotify records the switch explicitly. It stores a fresh pointer, so a
// copy of the State taken before the call never sees the change.
func (st *State) SetNotify(on bool) { st.Notify = &on }

// UpdateCheckOn reports whether the daemon checks for a new release. Absent is on.
func (s State) UpdateCheckOn() bool { return s.Updates.Check == nil || *s.Updates.Check }

// AutoUpdateOn reports whether the daemon installs a new release by itself.
// Absent is off.
func (s State) AutoUpdateOn() bool { return s.Updates.Auto != nil && *s.Updates.Auto }

// RestartOn reports whether the daemon restarts itself onto an installed
// newer version when idle (R126). Absent is on.
func (s State) RestartOn() bool { return s.Updates.Restart == nil || *s.Updates.Restart }

// SetAutoRestart records the idle-restart switch, with a fresh pointer.
func (st *State) SetAutoRestart(on bool) { st.Updates.Restart = &on }

// SetUpdateCheck records the check switch explicitly, with a fresh pointer.
func (st *State) SetUpdateCheck(on bool) { st.Updates.Check = &on }

// SetAutoUpdate records the auto-install switch. Turning it on also turns the
// check on, since auto-install needs the check.
func (st *State) SetAutoUpdate(on bool) {
	st.Updates.Auto = &on
	if on {
		st.SetUpdateCheck(true)
	}
}

// TracingAt reports whether a trace window is open at now: strictly before
// until, so a window ends by itself at until (M2c T4).
func (s State) TracingAt(now time.Time) bool {
	return s.Trace != nil && s.Trace.Until.After(now)
}

// SetTraceUntil opens (or moves) the trace window. It stores a fresh
// pointer, so a copy of the State taken before the call never sees it.
func (st *State) SetTraceUntil(until time.Time) { st.Trace = &Trace{Until: until} }

// ClearTrace ends the trace window now (`chottag trace off`).
func (st *State) ClearTrace() { st.Trace = nil }

// AutoOn reports whether auto-switch is on. Absent is on (R68, S1).
func (s State) AutoOn() bool {
	return s.Auto == nil || s.Auto.Enabled == nil || *s.Auto.Enabled
}

// AutoSettings returns a copy of the settings block; the zero Auto when
// none is stored. The copy shares nothing with s.
func (s State) AutoSettings() Auto {
	if s.Auto == nil {
		return Auto{}
	}
	return s.Auto.clone()
}

func (a Auto) clone() Auto {
	c := a
	if a.Enabled != nil {
		on := *a.Enabled
		c.Enabled = &on
	}
	if a.SwitchPoints != nil {
		c.SwitchPoints = make(map[string]int, len(a.SwitchPoints))
		for k, v := range a.SwitchPoints {
			c.SwitchPoints[k] = v
		}
	}
	return c
}

// editAuto applies fn to a fresh copy of the block and stores the copy, so
// a State copied before the call (store.Cache hands out copies) never sees
// the change. An edit that leaves every field empty removes the block.
func (st *State) editAuto(fn func(*Auto)) {
	a := st.AutoSettings()
	a.Threshold = 0
	fn(&a)
	if a.Enabled == nil && a.Mode == "" && len(a.SwitchPoints) == 0 && a.Hold5h == "" && a.Hold7d == "" && a.Cooldown == "" {
		st.Auto = nil
		return
	}
	st.Auto = &a
}

// SetAutoEnabled records `chottag auto on|off` explicitly.
func (st *State) SetAutoEnabled(on bool) {
	st.editAuto(func(a *Auto) { a.Enabled = &on })
}

// SetAutoMode records `chottag auto mode <mode>`.
func (st *State) SetAutoMode(mode string) {
	st.editAuto(func(a *Auto) { a.Mode = mode })
}

// SetAutoSetting records one `chottag auto set <key> <value>`: hold5h,
// hold7d and cooldown keep the duration text as given; any other key is a
// switch point and must be an integer. The caller validates the key and
// the bounds first (autoswitch.ValidateSetting).
func (st *State) SetAutoSetting(key, value string) error {
	switch key {
	case "hold5h", "hold7d", "cooldown":
		st.editAuto(func(a *Auto) {
			switch key {
			case "hold5h":
				a.Hold5h = value
			case "hold7d":
				a.Hold7d = value
			default:
				a.Cooldown = value
			}
		})
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("switch point %s must be an integer, not %q", key, value)
	}
	st.editAuto(func(a *Auto) {
		if a.SwitchPoints == nil {
			a.SwitchPoints = map[string]int{}
		}
		a.SwitchPoints[key] = n
	})
	return nil
}

// ResetAutoSettings clears every override (`chottag auto reset`): the
// switch points, holds and cooldown. The on/off switch and the mode stay.
func (st *State) ResetAutoSettings() {
	st.editAuto(func(a *Auto) {
		a.SwitchPoints, a.Hold5h, a.Hold7d, a.Cooldown = nil, "", "", ""
	})
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// ValidName reports whether name can be an account name. Names become slot
// directory names, so they must not contain path separators or start with a dot.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid account name %q: use 1-32 letters, digits, '.', '_' or '-', starting with a letter or digit", name)
	}
	return nil
}

func (st *State) index(name string) int {
	for i, a := range st.Accounts {
		if strings.EqualFold(a.Name, name) {
			return i
		}
	}
	return -1
}

// Find resolves q as an exact name, then an exact email, then a unique name
// prefix. All comparisons ignore case.
func (st *State) Find(q string) (*Account, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("%w: empty name", ErrNotFound)
	}
	if i := st.index(q); i >= 0 {
		return &st.Accounts[i], nil
	}
	var emailHit *Account
	for i, a := range st.Accounts {
		if a.Email != "" && strings.EqualFold(a.Email, q) {
			if emailHit != nil {
				return nil, fmt.Errorf("%w: %q matches %s and %s", ErrAmbiguous, q, emailHit.Name, a.Name)
			}
			emailHit = &st.Accounts[i]
		}
	}
	if emailHit != nil {
		return emailHit, nil
	}
	var hit *Account
	lq := strings.ToLower(q)
	for i, a := range st.Accounts {
		if strings.HasPrefix(strings.ToLower(a.Name), lq) {
			if hit != nil {
				return nil, fmt.Errorf("%w: %q matches %s and %s", ErrAmbiguous, q, hit.Name, a.Name)
			}
			hit = &st.Accounts[i]
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, q)
	}
	return hit, nil
}

// Next returns the account after cur in registration order, wrapping around.
// An empty or unknown cur yields the first account.
func (st *State) Next(cur string) (Account, error) {
	if len(st.Accounts) == 0 {
		return Account{}, ErrNoAccounts
	}
	if i := st.index(cur); i >= 0 {
		return st.Accounts[(i+1)%len(st.Accounts)], nil
	}
	return st.Accounts[0], nil
}

// sameDir reports whether a and b name the same directory by file identity
// when both exist — os.SameFile catches a case-insensitive filesystem's
// accounts/B and accounts/b, which are literally one inode (NEW-2) — and
// falls back to a cleaned, case-INSENSITIVE compare when either is missing
// (re-review Minor): identity can only be proven when both paths can be
// stat'd, but a missing dir (removed by hand, or simply not created yet)
// must not read as "safe, definitely not the same" — that is exactly
// backwards from the conservative direction this whole check exists for. A
// case-sensitive filesystem paying for a slightly more conservative
// missing-path fallback than it strictly needs (nextFreeSlotDir just picks
// the next name) is a fine trade for never under-detecting the collision
// on a case-insensitive one. This mirrors internal/cli's own dedicated
// slot-dir helper (sameSlotDir, login.go), which this package cannot
// import (cli imports store, not the reverse — F103); it deliberately does
// NOT touch internal/cli's shared sameFile (daemon.go), whose other
// callers (the log-rotation guard, provisionBinSymlinks) have nothing to
// do with account slot dirs.
func sameDir(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	ai, aerr := os.Stat(ca)
	bi, berr := os.Stat(cb)
	if aerr != nil || berr != nil {
		return strings.EqualFold(ca, cb)
	}
	return os.SameFile(ai, bi)
}

// Add registers a. The first account becomes both serving and remote.
func (st *State) Add(a Account) error {
	if err := ValidName(a.Name); err != nil {
		return err
	}
	if a.Dir == "" {
		return errors.New("account slot dir is required")
	}
	if !filepath.IsAbs(a.Dir) {
		return fmt.Errorf("account slot dir must be absolute: %q", a.Dir)
	}
	if st.index(a.Name) >= 0 {
		return fmt.Errorf("%w: %s", ErrExists, a.Name)
	}
	// Two accounts never share a Dir (F173): the slot dir and its Keychain
	// item are fixed at creation and never follow a rename, so a second
	// account on the same Dir would silently overwrite the first one's
	// login and confuse the roster diff that keys off Dir (F172). Compared
	// by file identity (NEW-2), not just a cleaned string: on a
	// case-insensitive filesystem (macOS's default APFS, Windows),
	// accounts/B and accounts/b are literally one inode.
	for _, existing := range st.Accounts {
		if sameDir(existing.Dir, a.Dir) {
			return fmt.Errorf("%w: %s already uses %s", ErrExists, existing.Name, a.Dir)
		}
	}
	st.Accounts = append(st.Accounts, a)
	if st.Serving == "" {
		st.Serving = a.Name
	}
	if st.Remote == "" {
		st.Remote = a.Name
	}
	return nil
}

// Remove unregisters name. It refuses while name is serving or remote; the
// caller moves those roles first.
func (st *State) Remove(name string) error {
	i := st.index(name)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if strings.EqualFold(st.Serving, name) || strings.EqualFold(st.Remote, name) {
		return fmt.Errorf("%w: %s", ErrInUse, st.Accounts[i].Name)
	}
	st.Accounts = append(st.Accounts[:i], st.Accounts[i+1:]...)
	return nil
}

// Rename changes an account's display name, and every role that named it,
// in one state (M2 spec §3, D10).
//
//   - old is an exact, case-insensitive name, never a prefix or an email
//     (F15). It need not be a ValidName, so a hand-edited name can be
//     repaired.
//   - newName must be a ValidName, and must not name another account,
//     case-insensitively. A case-only change of the same account is
//     allowed.
//   - Dir never changes: the slot and its Keychain item are fixed when the
//     account is created (R21).
//
// It returns the name the account had and the roles it moved, "serving"
// then "remote", never nil.
func (st *State) Rename(old, newName string) (from string, roles []string, err error) {
	if err := ValidName(newName); err != nil {
		return "", nil, err
	}
	i := st.index(old)
	if i < 0 || old == "" {
		return "", nil, fmt.Errorf("%w: %q", ErrNotFound, old)
	}
	for j, a := range st.Accounts {
		if j != i && strings.EqualFold(a.Name, newName) {
			return "", nil, fmt.Errorf("%w: %s", ErrExists, a.Name)
		}
	}
	from = st.Accounts[i].Name
	st.Accounts[i].Name = newName
	roles = []string{}
	if strings.EqualFold(st.Serving, from) {
		st.Serving = newName
		roles = append(roles, "serving")
	}
	if strings.EqualFold(st.Remote, from) {
		st.Remote = newName
		roles = append(roles, "remote")
	}
	return from, roles, nil
}

// Store reads and writes state.json inside Dir (normally ~/.chottag).
type Store struct{ Dir string }

func (s Store) path() string        { return filepath.Join(s.Dir, "state.json") }
func (s Store) lockPath() string    { return filepath.Join(s.Dir, "state.lock") }
func (s Store) accountsDir() string { return filepath.Join(s.Dir, "accounts") }

// SlotDir is where a new account named name gets its CLAUDE_CONFIG_DIR. It
// errors if name is not a ValidName, so the result always stays inside
// <Dir>/accounts.
func (s Store) SlotDir(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.accountsDir(), name), nil
}

// IsSlotDir reports whether dir is exactly a direct child of <Dir>/accounts
// named after a ValidName, i.e. a path SlotDir could have returned. M1c's
// `logout` checks this before any os.RemoveAll.
func (s Store) IsSlotDir(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	clean := filepath.Clean(dir)
	if filepath.Dir(clean) != s.accountsDir() {
		return false
	}
	return ValidName(filepath.Base(clean)) == nil
}

// Load returns the saved state, or Default() if none has been saved yet.
func (s Store) Load() (State, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return State{}, err
	}
	st := Default()
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s is corrupt: %w", s.path(), err)
	}
	if st.Version > Version {
		return State{}, fmt.Errorf("%s has version %d, newer than this chottag understands (%d); upgrade chottag", s.path(), st.Version, Version)
	}
	if st.Auto != nil && st.Auto.Threshold != 0 {
		st.Auto = nil // the pre-M4 block: no choice recorded (see Auto.Threshold)
	}
	return st, nil
}

// SwapServing moves the serving role from `from` to `to`, only if serving
// is still `from` (compared case-insensitively): the daemon's auto-switch
// write (M4 spec §5, S10). A concurrent `chottag tag` that got there first
// wins, and SwapServing returns ErrServingChanged without writing. to must
// be a registered account, by exact name (ErrNotFound otherwise); serving
// takes its registered spelling.
func (s Store) SwapServing(from, to string) (State, error) {
	return s.Update(func(st *State) error {
		if !strings.EqualFold(st.Serving, from) {
			return fmt.Errorf("%w: serving is %q, not %q", ErrServingChanged, st.Serving, from)
		}
		i := st.index(to)
		if i < 0 {
			return fmt.Errorf("%w: %q", ErrNotFound, to)
		}
		st.Serving = st.Accounts[i].Name
		return nil
	})
}

// Update applies fn to the current state under the state lock and saves the
// result atomically. If fn returns an error nothing is written.
func (s Store) Update(fn func(*State) error) (State, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return State{}, err
	}
	unlock, err := fsutil.Lock(s.lockPath())
	if err != nil {
		return State{}, err
	}
	defer unlock()
	st, err := s.Load()
	if err != nil {
		return State{}, err
	}
	if err := fn(&st); err != nil {
		return State{}, err
	}
	st.Version = Version
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return State{}, err
	}
	if err := fsutil.WriteFileAtomic(s.path(), append(b, '\n'), 0o600); err != nil {
		return State{}, err
	}
	return st, nil
}
