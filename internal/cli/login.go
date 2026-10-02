package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/refresh"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// realClaudeBin is the claude every `claude auth` exec in this package runs:
// an explicit --claude as given, otherwise the real claude on PATH, skipping
// chottag's own bin dir (home/bin) the way the shim does. A bare "claude"
// would resolve to the shim after `chottag setup` and, before the shim
// bypassed `auth`, record the serving account's identity (issue #2, R145).
func realClaudeBin(flagVal, home string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	bin, err := shim.ResolveClaude(os.Getenv("PATH"), filepath.Join(home, "bin"), "")
	if err != nil {
		// The reporter adds its own "chottag: " prefix.
		return "", errors.New(strings.TrimPrefix(err.Error(), "chottag: "))
	}
	return bin, nil
}

// newRefresher is the daemon's token refresher. Its binary is an explicit
// --claude as given; otherwise state.json's cached real claude when that is
// not under chottag's own bin dir, otherwise the real claude on PATH. Never
// the shim: a refresh through it would pick up the proxy and have Claude
// Code's profile fetch answered as the serving account, writing that
// account's email into the slot (issue #2, R145). It is resolved on every
// refresh; with no real claude the refresh fails, loudly, with the
// resolution error.
func newRefresher(flagVal, home string) refresh.Claude {
	if flagVal != "" {
		return refresh.Claude{Bin: flagVal}
	}
	// Resolved on every refresh, not once: the daemon outlives a claude that
	// moves (npm to the native installer) or is installed after it started.
	return refresh.Claude{Resolve: func() (string, error) { return resolveRefreshClaude(home) }}
}

func resolveRefreshClaude(home string) (string, error) {
	cached := ""
	if st, err := (store.Store{Dir: home}).Load(); err == nil {
		cached = st.RealClaude
	}
	if cached != "" && sameDirPath(filepath.Dir(cached), filepath.Join(home, "bin")) {
		cached = ""
	}
	return shim.ResolveClaude(os.Getenv("PATH"), filepath.Join(home, "bin"), cached)
}

// sameDirPath reports whether a and b are one directory, resolving symlinks.
func sameDirPath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = filepath.Clean(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = filepath.Clean(b)
	}
	return ra == rb
}

// claudeAuthExec runs `claude auth <sub>` against one slot. It is a package
// variable because this is the ONLY exec in the tree that inherits real
// stdio — refresh.execRun and slotEmail both nil it deliberately — and a
// test that reached the real one would hand the test runner's terminal to a
// browser login prompt and block. internal/cli's TestMain installs a
// panicking default so a test that forgets to stub it fails loudly.
var claudeAuthExec = runClaudeAuth

// SetAuthExecForTest swaps claudeAuthExec for fn and returns a func that
// restores whatever was installed at the moment of the call (production's
// own runClaudeAuth, or, inside a test binary that has already swapped it,
// whatever a previous swap or a TestMain default left behind) — mirroring
// internal/shim's SetSeamsForTest exactly, and for the identical reason: a
// nil-means-production special case would give every test an API path back
// to the real, stdio-inheriting exec once the FIRST stub's cleanup ran,
// silently disarming TestMain's panicking default for the rest of the test
// binary. There must be no such path from inside a test binary, so there is
// none here.
//
// It lives in a non-test file so other packages' tests can reach it: a
// _test.go file compiles only into its own package's test binary (F103).
func SetAuthExecForTest(fn func(bin, slotDir, sub string, stdin io.Reader, stdout, stderr io.Writer) error) (restore func()) {
	orig := claudeAuthExec
	claudeAuthExec = fn
	return func() { claudeAuthExec = orig }
}

func runClaudeAuth(bin, slotDir, sub string, stdin io.Reader, stdout, stderr io.Writer) error {
	// bin is the --claude flag: an operator-supplied, trusted binary path
	// (never request- or network-derived), the same trust boundary as
	// refresh.Claude, slotEmail and creds.ExecRunner.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.Command(bin, "auth", sub)
	cmd.Env = refresh.ChildEnv(slotDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// sameSlotDir reports whether a and b name the same account slot
// directory: by file identity (os.SameFile) when both exist — catching a
// case-insensitive filesystem's accounts/B and accounts/b, which are
// literally one inode (NEW-2) — and, when either is missing, by a cleaned,
// case-INSENSITIVE compare instead of a plain string match (re-review
// Minor): identity can only be proven when both paths can be stat'd, and a
// missing dir must not read as "safe, definitely different" — a
// case-sensitive filesystem paying for a slightly more conservative
// missing-path fallback than it strictly needs (login just picks the next
// free name) is a fine trade for never under-detecting the collision on a
// case-insensitive one.
//
// Deliberately its OWN helper, not a change to sameFile's own fallback
// (daemon.go): that helper's other callers — the log-rotation guard and
// provisionBinSymlinks (setup.go) — have nothing to do with account slot
// dirs, and folding THEIR missing-path fallback to case-insensitive too
// would change their semantics for no reason this fix needs. Mirrors
// store.sameDir's own missing-path fallback (store cannot import cli, so
// that one is its own small copy too).
func sameSlotDir(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	if aa == bb {
		return true, nil
	}
	ai, aerr := os.Stat(aa)
	bi, berr := os.Stat(bb)
	if aerr != nil || berr != nil {
		return strings.EqualFold(aa, bb), nil
	}
	return os.SameFile(ai, bi), nil
}

// dirRegistered reports whether dir already belongs to a registered
// account (C1/F173), by file identity (NEW-2) rather than a string
// compare: on a case-insensitive filesystem (macOS's default APFS,
// Windows), accounts/B and accounts/b are literally one inode, and a
// string compare alone would miss that a login for the new name "b" is
// about to take over an already-registered "B"'s (or a renamed "Bee"'s)
// own slot.
func dirRegistered(st store.State, dir string) bool {
	for _, a := range st.Accounts {
		if same, err := sameSlotDir(a.Dir, dir); err == nil && same {
			return true
		}
	}
	return false
}

// nextFreeSlotDir returns the first accounts/<name>-N (N from 2) that is
// neither a registered account's Dir nor already present on disk (C1/F173).
func nextFreeSlotDir(s store.Store, st store.State, name string) (string, error) {
	for n := 2; ; n++ {
		candidate, err := s.SlotDir(fmt.Sprintf("%s-%d", name, n))
		if err != nil {
			return "", err
		}
		if dirRegistered(st, candidate) {
			continue
		}
		if _, statErr := os.Stat(candidate); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		return candidate, nil
	}
}

// emailRegisteredLine is login's email_registered warning. With extra pools
// (or --pool) it says how to share the account instead of logging in twice
// (M8 spec §3); with only the default pool there is nothing to join, so it
// keeps its original text.
func emailRegisteredLine(st store.State, poolFlag, email string, account store.Account) string {
	// A pool the account is not in yet: --pool when it fits, else the first
	// extra pool that does.
	candidates := st.PoolNames()[1:]
	if poolFlag != "" {
		candidates = append([]string{poolFlag}, candidates...)
	}
	for _, pool := range candidates {
		if !account.InPool(pool) {
			return fmt.Sprintf("chottag: warning: %s is already account %s; to use it in another pool, run: chottag pool join %s %s", email, account.Name, account.Name, pool)
		}
	}
	return fmt.Sprintf("chottag: warning: %s is already registered as account %s", email, account.Name)
}

// loginResult is `login --json`'s fields (spec §5.3).
type loginResult struct {
	Account string `json:"account"`
	Email   string `json:"email"`
	Org     string `json:"org,omitempty"`
}

// runLogin logs a slot in and registers it (spec §5).
//
// The slot's flock is held for the whole flow (§4.7): the daemon's own
// credential reader honours it and already names `chottag login` as the
// expected other holder.
//
// In JSON mode the `claude auth login` child gets stderr as its stdout
// (reporter.ChildStdout), so nothing it prints can land in front of the one
// document (spec §5.3); its browser prompts reach the terminal either way.
func runLogin(args []string, stdin io.Reader, r *reporter) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	claudeFlag := fs.String("claude", "", "path to the real claude binary (default: the real claude on PATH, never chottag's own shim)")
	poolFlag := fs.String("pool", "", "the pool a new account joins (default: default)")
	// parseInterspersed (F2): `login NAME --claude PATH` must work exactly
	// like `login --claude PATH NAME` — a bare fs.Parse(args) stops at the
	// first non-flag token and would silently never see --claude in the
	// first form.
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 1 {
		return r.Usage("usage: chottag login <name> [--pool POOL] [--claude PATH]")
	}
	name := positional[0]
	if err := store.ValidName(name); err != nil {
		return r.Fail(exit.Usage, codeUsage, err.Error(), nil)
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	s := store.Store{Dir: h}

	st, err := s.Load()
	if err != nil {
		return r.FailErr(err)
	}
	// An unknown pool is refused before the browser opens.
	if *poolFlag != "" {
		if err := checkPool(st, *poolFlag); err != nil {
			return failPool(r, err)
		}
	}
	// Exact match, never st.Find: Find resolves a unique name PREFIX, which
	// would treat an unrelated "Alpha" as the registered "A" (F15).
	dir, existing := "", false
	for i := range st.Accounts {
		if strings.EqualFold(st.Accounts[i].Name, name) {
			dir, existing = st.Accounts[i].Dir, true
			break
		}
	}
	if !existing {
		if dir, err = s.SlotDir(name); err != nil {
			return r.FailErr(err)
		}
		// C1(c)/F173: name's natural slot dir can already belong to a
		// DIFFERENT, registered account — e.g. `chottag rename B Bee`
		// keeps Bee's Dir at accounts/B, since Dir never follows a rename.
		// Logging in a fresh "B" there would run `claude auth login`
		// inside Bee's own slot, overwriting Bee's login, and then fail to
		// register (store.State.Add now refuses a second account on the
		// same Dir). Move to the next free accounts/<name>-N instead — N
		// from 2, free meaning neither registered nor already present on
		// disk (an unregistered dir on disk, the shape `adopt` exists for,
		// is left alone: that case falls through below exactly as before).
		if dirRegistered(st, dir) {
			if dir, err = nextFreeSlotDir(s, st, name); err != nil {
				return r.FailErr(err)
			}
		}
	}

	created := false
	if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return r.FailErr(err)
		}
		created = true
	}
	// A login that fails must leave nothing behind: an empty slot dir would
	// later look adoptable. isAliasedSlotDir (uninstall.go, Task 8) is
	// included alongside IsSlotDir's path-string check so a slot reached
	// through a path alias of <h>/accounts (a case-folded spelling, or a
	// symlinked $CHOTTAG_HOME) is still recognised as one of ours and
	// cleaned up (P9) — without it, cleanup would silently do nothing for a
	// dir this very call just created.
	cleanup := func() {
		if created && (s.IsSlotDir(dir) || isAliasedSlotDir(h, dir)) {
			os.RemoveAll(dir)
		}
	}

	unlock, err := fsutil.Lock(creds.LockPath(dir))
	if err != nil {
		cleanup()
		return r.FailErr(err)
	}
	defer unlock()

	claudeBin, err := realClaudeBin(*claudeFlag, h)
	if err != nil {
		cleanup()
		return r.Fail(exit.Error, codeLoginFailed, err.Error(), nil)
	}

	if err := claudeAuthExec(claudeBin, dir, "login", stdin, r.ChildStdout(), r.Stderr()); err != nil {
		cleanup()
		return r.Fail(exit.Error, codeLoginFailed, fmt.Sprintf("login failed: %v", err), nil)
	}

	email, org, sub, loggedIn, err := slotEmail(claudeBin, dir)
	if err != nil {
		cleanup()
		return r.Fail(exit.Error, codeLoginFailed, err.Error(), nil)
	}
	if !loggedIn {
		cleanup()
		return r.Fail(exit.Error, codeLoginFailed, fmt.Sprintf("%s still reports no login after `claude auth login`", name), nil)
	}

	if existing && *poolFlag != "" {
		for _, a := range st.Accounts {
			if strings.EqualFold(a.Name, name) && !a.InPool(*poolFlag) {
				r.Warn(warnPoolNotChanged, fmt.Sprintf("chottag: %s is already registered; --pool applies only to a new account. To add it to %s, run: chottag pool join %s %s", a.Name, *poolFlag, a.Name, *poolFlag))
			}
		}
	}
	for i := range st.Accounts {
		a := st.Accounts[i]
		if email != "" && strings.EqualFold(a.Email, email) && !strings.EqualFold(a.Name, name) {
			r.Warn(warnEmailRegistered, emailRegisteredLine(st, *poolFlag, email, a))
		}
	}

	// canonicalName is the account's own stored casing, for the JSON result
	// only (fix round 4, item 4): a re-login typed as "alice" against an
	// already-registered "Alice" must report account: "Alice", not echo back
	// whatever casing was typed. The text line keeps using `name` (the typed
	// spelling) unchanged, exactly as before this fix.
	canonicalName := name
	if _, err := s.Update(func(st *store.State) error {
		now := time.Now()
		for i := range st.Accounts {
			if strings.EqualFold(st.Accounts[i].Name, name) {
				st.Accounts[i].Email, st.Accounts[i].Org = email, org
				prefillPlan(&st.Accounts[i], sub)
				st.Accounts[i].LoggedInAt = now
				canonicalName = st.Accounts[i].Name
				return nil
			}
		}
		// A new account joins the --pool pool only, else default (R132).
		var pools []string
		if *poolFlag != "" && *poolFlag != store.DefaultPool {
			pools = []string{*poolFlag}
		}
		return st.Add(store.Account{Name: name, Email: email, Org: org, Plan: planFromSubscription(sub), Dir: dir, AddedAt: now, LoggedInAt: now, PoolList: pools})
	}); err != nil {
		// F5: the browser login and the slot it landed in are both real by
		// this point — only the registration write failed — so a slot this
		// call created must still come out exactly like every earlier
		// failure path, or it survives to later look adoptable despite
		// never having been registered.
		cleanup()
		return failPool(r, err)
	}

	r.Text("logged in: %s (%s)\n", name, email)
	return r.OK(loginResult{Account: canonicalName, Email: email, Org: org})
}
