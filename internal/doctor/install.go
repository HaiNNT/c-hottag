package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/daemonlock"
	// proxyauth: doctor reads the secret file's mode and format ONLY (Load,
	// PermError, ErrMalformed) and never prints its value — the value is
	// not a Claude credential, so this import is exempt from
	// imports_test.go's forbidden list the same way internal/ca's is.
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// InstallChecks are spec §2.2 rows, in order: setup, tree, ca, proxy-secret
// (part 1 T11), bin, rc-block, path — seven rows since proxy-secret was
// added (final review N6; it was six before).
func InstallChecks() []Check {
	return []Check{setupCheck(), treeCheck(), caCheck(), proxySecretCheck(), binCheck(), rcBlockCheck(), pathCheck()}
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func isExecutable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// samePath compares two directories after resolving symlinks (macOS's /var
// is /private/var), falling back to a lexical compare.
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// setupCheck is row 1. Doctor never performs a first install (D3), so a
// home without chottag gates every later check. A fresh setup with no
// logged-in slot has bin/ but no state.json yet (F169), and that counts as
// set up.
func setupCheck() Check {
	return Check{ID: "setup", Gate: true, Detect: func(e *Env) Finding {
		hasState := fileExists(filepath.Join(e.Home, "state.json"))
		if !hasState && !isDir(e.BinDir()) {
			return Finding{Status: StatusProblem, Detail: "chottag is not set up in " + e.Home, Hint: "chottag setup"}
		}
		if hasState {
			if _, err := e.State(); err != nil {
				return Internal(err)
			}
		}
		return Finding{Status: StatusOK, Detail: e.Home}
	}}
}

// treeCheck is row 2.
func treeCheck() Check {
	return Check{
		ID: "tree",
		Detect: func(e *Env) Finding {
			var missing []string
			for _, d := range e.SetupDirs {
				if !isDir(filepath.Join(e.Home, d)) {
					missing = append(missing, d)
				}
			}
			if len(missing) == 0 {
				return Finding{Status: StatusOK, Detail: "every directory of the tree exists"}
			}
			return Finding{Status: StatusProblem, Detail: "missing under " + e.Home + ": " + strings.Join(missing, ", ")}
		},
		Fix: func(e *Env) error {
			for _, d := range e.SetupDirs {
				if err := os.MkdirAll(filepath.Join(e.Home, d), 0o700); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// caMoveHint is internal/ca's own MoveHint (final re-review, concern (b)):
// a one-line wrapper, kept so every existing call site below stays
// unchanged, rather than a second copy of the text that could drift from
// ca.go's. internal/ca has no reason to import internal/doctor, so this
// direction is the only one that avoids a cycle.
func caMoveHint(dir string) string {
	return ca.MoveHint(dir)
}

// daemonHoldsLock reports whether a chottag daemon currently holds
// daemon.lock — running and healthy, or merely holding it with a record
// this process cannot read (ErrUnreadableRecord: still a live, unsettled
// holder, not "no daemon").
//
// Any OTHER Inspect error fails CLOSED (controller ruling on the I2 final
// review): the lock's state is genuinely unknown, not "no daemon", and a
// fix that guessed "no daemon" and proceeded on that guess could strand a
// running install exactly the way I2 exists to prevent. held is true and
// err is returned so the caller can both report it and refuse to fix.
// daemonHoldsLock sees the daemon only: `chottag proxy run` and `chottag
// trace run` are dev-only paths that load the secret without ever taking
// daemon.lock (final review N5), so this can't tell one of them is using
// it, and proxySecretCheck.Fix can regenerate the secret out from under a
// running one. Documented in SECURITY.md rather than widened here: turning
// busy into "or a verified health answer on the state port" would refuse
// --fix for reasons an operator running the real daemon never sees, for a
// case that only matters to a dev-only, foreground, already-visible-in-
// its-own-terminal process.
func daemonHoldsLock(e *Env) (held bool, err error) {
	st, ierr := e.Inspect()
	switch {
	case ierr == nil:
		return st.Running, nil
	case errors.Is(ierr, daemonlock.ErrUnreadableRecord):
		return true, nil
	default:
		return true, ierr
	}
}

const caRestartHint = "chottag daemon restart"

// caCheck is row 3. It creates a CA only when both halves are missing: a
// new CA breaks trust for running sessions, so doctor never replaces half a
// pair (D4). It also never creates one while a daemon is running (I2,
// F173's sibling for the CA): the daemon keeps signing with its own
// in-memory CA regardless, so a fresh pair on disk would only make every
// NEW session trust a CA the running daemon never signs with — doctor
// never causes a TLS failure it could instead leave for `chottag daemon
// restart` to fix cleanly (the daemon's own LoadOrCreateLocked creates the
// pair on start).
func caCheck() Check {
	return Check{
		ID: "ca",
		Detect: func(e *Env) Finding {
			dir := filepath.Join(e.Home, "ca")
			cert, key := fileExists(filepath.Join(dir, "ca.pem")), fileExists(filepath.Join(dir, "ca.key"))
			switch {
			case !cert && !key:
				held, ierr := daemonHoldsLock(e)
				if !held {
					return Finding{Status: StatusProblem, Detail: "no CA in " + dir}
				}
				detail := "no CA in " + dir + ", and a chottag daemon is running; doctor never creates a CA under a live daemon, since it would keep signing with its own old one"
				if ierr != nil {
					// Fail closed (controller ruling): the lock's state
					// could not be determined, so this is treated exactly
					// like "a daemon is running", not like "no daemon".
					detail = fmt.Sprintf("no CA in %s, and whether a chottag daemon is running could not be determined: %v; doctor never creates a CA when that is unknown", dir, ierr)
				}
				return Finding{Status: StatusProblem, Detail: detail, Hint: caRestartHint}
			case !cert || !key:
				missing := "ca.key"
				if !cert {
					missing = "ca.pem"
				}
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("%s is missing from %s; doctor never replaces half a CA, since a new CA breaks trust for running sessions", missing, dir), Hint: caMoveHint(dir)}
			}
			a, err := ca.Load(dir)
			switch {
			case errors.Is(err, ca.ErrKeyPermissions):
				// A symlinked ca.key, one owned by someone else, or ca/
				// ITSELF being a symlink/non-directory/foreign-owned (T11
				// fix round 1 finding 2: chmod on a path under a symlinked
				// ca/ follows it straight through to wherever it points,
				// same as ca.Load's own read did to get here) is never
				// chmod'd — Fix below refuses every one of those shapes too
				// (chmodOwnKey's own re-check), so the hint stays
				// caMoveHint for them, empty (fixable) only when it is ours
				// to chmod.
				f := Finding{Status: StatusProblem, Detail: "ca.key is readable by others or not owned by you"}
				if !caKeySafe(dir, filepath.Join(dir, "ca.key")) {
					f.Hint = caMoveHint(dir)
				}
				return f
			case err != nil:
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("the CA in %s does not load: %v", dir, err), Hint: caMoveHint(dir)}
			case !a.Constrained():
				// A CA generated before name constraints existed: it still
				// loads and works (Ruling 13), but a leaked ca.key could
				// sign for any host, not just the ones chottag intercepts.
				// Report-only: moving ca/ away breaks trust for running
				// sessions, so that is the user's call, never --fix's.
				return Finding{Status: StatusInfo, Detail: "ca.pem and ca.key load; this CA predates name constraints (a leaked ca.key could sign for any host). For a constrained one: move ca/ away, then chottag daemon restart and restart sessions."}
			}
			return Finding{Status: StatusOK, Detail: "ca.pem and ca.key load"}
		},
		Fix: func(e *Env) error {
			dir := filepath.Join(e.Home, "ca")
			keyPath := filepath.Join(dir, "ca.key")
			if fileExists(filepath.Join(dir, "ca.pem")) && fileExists(keyPath) {
				// Both halves already exist: the only fixable problem
				// Detect ever leaves unhinted for that shape is a
				// key-permission one this uid owns (review correction:
				// the fix is chmod 0600, never caMoveHint — moving the CA
				// away breaks trust for running sessions). Unlike the
				// "both missing" branch below, this never touches the
				// daemon-lock check: chmod changes no key material, so a
				// running daemon signing with the unchanged key is never
				// put out of step with what new sessions trust.
				// chmodOwnKey is the actual TOCTOU-safe gate (finding 2):
				// it refuses ca/ itself and re-verifies ca.key through an
				// already-open, O_NOFOLLOW'd fd, never a chmod by path.
				return chmodOwnKey(dir, keyPath)
			}
			// Both halves missing: belt and braces alongside Detect's own
			// daemonHoldsLock check above (which, by setting a Hint,
			// already keeps Run from calling Fix at all in that case —
			// see runOne's `fixable`): this still refuses directly, so
			// Fix is never the one thing standing between "a daemon is
			// running" (or "unknown") and a CA created out from under it,
			// no matter how it gets called.
			held, ierr := daemonHoldsLock(e)
			if ierr != nil {
				// Fail closed (controller ruling): return the Inspect
				// error itself, never guess "no daemon" and proceed.
				return ierr
			}
			if held {
				return fmt.Errorf("a chottag daemon is running; doctor never creates a CA under a live daemon (run `%s`, which creates its own new pair on start)", caRestartHint)
			}
			_, err := ca.LoadOrCreateLocked(dir)
			return err
		},
	}
}

// getuid is a seam over os.Getuid (proxyauth's own pattern, internal_test.go
// there) so a test can force caDirUnsafe's and chmodOwnKey's foreign-owner
// branches without a second real uid — there is no other way to reach them
// in CI. Production never swaps it.
var getuid = os.Getuid

// caDirUnsafe reports the reason dir (normally <home>/ca) is never safe
// for a doctor fix to write into or chmod through: proxyauth.Regenerate
// has no directory check of its own (its lock() just MkdirAlls and opens
// by path, both of which FOLLOW a symlink there), and chmod-by-path
// through a symlinked or foreign-owned dir acts on whatever it actually
// points at — so doctor's own fixes are the only thing standing between a
// symlinked or foreign-owned ca/ and writing into, or chmod-ing through,
// wherever it really is (T11 fix round 1, findings 1 and 2). "" means dir
// is safe: it is either a real directory this uid owns, or it does not
// exist yet at all (Regenerate's own MkdirAll(0o700) creates and owns it,
// and there is nothing else there yet to write through).
func caDirUnsafe(dir string) string {
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ""
	case err != nil:
		return err.Error()
	case !info.Mode().IsDir():
		return "is not a real directory (perhaps a symlink)"
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		return fmt.Sprintf("is owned by uid %d, not you", st.Uid)
	}
	return ""
}

// caKeySafe reports whether keyPath (ca.key under dir) is a shape doctor's
// Fix may repair with chmod (review correction: never caMoveHint for a
// key-permission problem when it is ours to fix): dir itself is safe
// (caDirUnsafe — a symlinked or foreign-owned ca/ is never safe to write
// through, even to reach a ca.key that would otherwise qualify), and
// keyPath is a regular file (Lstat, so a symlinked ca.key is never "ours"
// either) this uid owns. Read-only, for Detect's hint decision;
// chmodOwnKey below is Fix's own, TOCTOU-safer re-check through an
// already-open fd, never trusting this Lstat alone to gate an actual
// write.
func caKeySafe(dir, keyPath string) bool {
	if caDirUnsafe(dir) != "" {
		return false
	}
	info, err := os.Lstat(keyPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == getuid()
}

// chmodOwnKey is caCheck's Fix for an owned key-permission problem (T11 fix
// round 1, finding 2). dir must itself be safe (caDirUnsafe): opening
// "<dir>/ca.key" by path would otherwise follow a symlinked ca/ straight
// through to wherever it points, before ca.key is even looked at. keyPath
// is then opened with O_NOFOLLOW — a symlinked ca.key itself is refused,
// ELOOP, never chmod'd through — and O_NONBLOCK, so a FIFO planted there
// cannot hang the open (ca.Load's own readKeyFile and proxyauth.Load use
// the identical pattern). Fstat and Chmod both act on that one, already-
// open fd, never a second path lookup something could swap out from under.
func chmodOwnKey(dir, keyPath string) error {
	if reason := caDirUnsafe(dir); reason != "" {
		return fmt.Errorf("ca/ %s; %s", reason, caMoveHint(dir))
	}
	f, err := os.OpenFile(keyPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return fmt.Errorf("ca.key is a symlink; %s", caMoveHint(dir))
		}
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ca.key is not a regular file; %s", caMoveHint(dir))
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		return fmt.Errorf("ca.key is owned by someone else; %s", caMoveHint(dir))
	}
	return f.Chmod(0o600)
}

// proxySecretRestartHint is proxySecretCheck's problem hint whenever a
// chottag daemon holds this home's lock: regenerating the secret out from
// under it would leave every live session's HTTPS_PROXY carrying a
// password the daemon no longer accepts, so the fix refuses until the
// daemon is stopped (mirroring caCheck's own daemon-lock refusal).
const proxySecretRestartHint = "chottag daemon stop, then chottag doctor --fix (running sessions then need a restart)"

// proxySecretCheck is doctor's row for ca/proxy.secret (F221, part 1 T11):
// the per-install password every daemon start and shim launch reads
// through proxyauth.Load. Doctor never repairs a secret that could already
// be compromised (proxyauth's Ruling 6): --fix only ever regenerates a
// missing or unusable one, and only while no daemon runs.
func proxySecretCheck() Check {
	return Check{
		ID: "proxy-secret",
		Detect: func(e *Env) Finding {
			dir := filepath.Join(e.Home, "ca")
			_, err := proxyauth.Load(e.Home)
			if err == nil {
				return Finding{Status: StatusOK, Detail: "ca/proxy.secret is 0600 and well formed"}
			}
			detail := "ca/proxy.secret is missing"
			if !errors.Is(err, fs.ErrNotExist) {
				// A *proxyauth.PermError, proxyauth.ErrMalformed, or some
				// other unreadable-file error: each already names itself
				// plainly (PermError's Error() even names the doctor
				// --fix hint), so the raw error text is the detail, the
				// same pattern caCheck's own "does not load" branch uses.
				detail = err.Error()
			}
			if reason := caDirUnsafe(dir); reason != "" {
				// ca/ itself is a symlink, not a directory, or foreign-
				// owned: proxyauth.Regenerate has no directory check of
				// its own (T11 fix round 1, finding 1), so this can never
				// be fixable — report-only, and NEVER the ordinary
				// daemon-lock hint below, or a stray --fix would write a
				// fresh secret into whatever ca/ actually points at, on
				// every run, forever.
				return Finding{Status: StatusProblem, Detail: detail, Hint: caMoveHint(dir)}
			}
			hint := ""
			if held, ierr := daemonHoldsLock(e); held {
				hint = proxySecretRestartHint
				if ierr != nil {
					// Mirror caCheck's own "could not be determined"
					// wording (T11 fix round 1, finding 6): held is also
					// true here when Inspect itself failed, which is not
					// the same as "a daemon is known to be running".
					detail = fmt.Sprintf("%s, and whether a chottag daemon is running could not be determined: %v; doctor never regenerates the secret when that is unknown", detail, ierr)
				}
			}
			return Finding{Status: StatusProblem, Detail: detail, Hint: hint}
		},
		Fix: func(e *Env) error {
			dir := filepath.Join(e.Home, "ca")
			if reason := caDirUnsafe(dir); reason != "" {
				return fmt.Errorf("ca/ %s; %s", reason, caMoveHint(dir))
			}
			// proxyauth.Regenerate has no directory check of its own (its
			// lock() just MkdirAlls and opens by path): caDirUnsafe above
			// is what already refused a symlinked, non-directory or
			// foreign-owned ca/, before Regenerate is ever reached.
			// tightenOwnCADir here only handles the one shape left: a
			// group/other-writable ca/ WE OWN, which proxyauth.Load would
			// otherwise reject with its own PermError (checkDirPerm) —
			// doctor tightens that first (review D2) so a stray `chmod
			// g+w ca/` does not leave this row stuck reporting "the fix
			// failed" forever.
			if err := tightenOwnCADir(dir); err != nil {
				return err
			}
			// RegenerateUnless re-checks daemonHoldsLock AFTER taking
			// proxy.secret.lock (T11 fix round 1, finding 4): a bare
			// pre-lock probe here could see "no daemon" a moment before
			// one actually starts, taking daemon.lock and loading the
			// still-current secret just before this call overwrote it.
			_, err := proxyauth.RegenerateUnless(e.Home, func() (bool, error) { return daemonHoldsLock(e) })
			if errors.Is(err, proxyauth.ErrBusy) {
				return fmt.Errorf("a chottag daemon is running; doctor never regenerates the proxy secret under a live daemon (%s)", proxySecretRestartHint)
			}
			return err
		},
	}
}

// tightenOwnCADir chmods dir to 0700 when it is ours and wider than that
// (group- or other-writable) — the one shape proxyauth.Load's own
// checkDirPerm would otherwise reject with a PermError, and the one shape
// doctor may repair silently. It is a no-op — never an error — when dir
// does not exist, is already 0700 or tighter, or is a symlink, a
// non-directory or foreign-owned (caDirUnsafe): those are never doctor's
// to touch, and are left for the caller (proxySecretCheck.Fix already
// refuses them before this is even called) or proxyauth's own PermError
// to explain.
func tightenOwnCADir(dir string) error {
	if caDirUnsafe(dir) != "" {
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil || info.Mode().Perm()&0o022 == 0 {
		return nil
	}
	return os.Chmod(dir, 0o700)
}

// binSetupHint is I3's report-only hint: doctor must never repoint a link
// away from another working chottag, nor point one at a binary that is
// about to disappear (a `go run` temp binary), so the fix it offers is the
// one the user actually wants — running setup from the binary that should
// own bin/.
const binSetupHint = "run chottag setup from the binary you want installed"

// isUnderTempDir reports whether e.Executable lives under any of
// e.TempDirs — symlink-resolved, the same way samePath compares bin/
// links, since os.TempDir() itself is often a symlink (macOS's /var is
// /private/var). A nil/empty e.TempDirs disables the whole refusal, the
// same way "" used to on the old single-string seam (internal/cli's own
// test binary lives under a go-build temp dir too, so its TestMain must be
// able to turn this fully off). Once enabled, it also treats an
// Executable under a "go-build..." path element as temporary regardless
// of whether that path matches a configured TempDirs entry (F176): that
// is where `go run` and `go test` put their binaries, and on some
// platforms it does not sit under any of the configured temp roots.
func isUnderTempDir(e *Env) bool {
	if len(e.TempDirs) == 0 {
		return false
	}
	if executableUnderGoBuild(e.Executable) {
		return true
	}
	exe := e.Executable
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	exe = filepath.Clean(exe)
	for _, td := range e.TempDirs {
		if td == "" {
			continue
		}
		if r, err := filepath.EvalSymlinks(td); err == nil {
			td = r
		}
		td = filepath.Clean(td)
		if exe == td || strings.HasPrefix(exe, td+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// executableUnderGoBuild reports whether exe has a path element starting
// with "go-build" — the directory `go run`/`go test` puts their temp
// binaries under (e.g. ".../T/go-build1234567890/b001/exe/chottag").
func executableUnderGoBuild(exe string) bool {
	for _, part := range strings.Split(filepath.ToSlash(exe), "/") {
		if strings.HasPrefix(part, "go-build") {
			return true
		}
	}
	return false
}

// binLinkPointsAtAnotherWorkingBinary reports whether link exists, is not
// dangling, resolves to something other than e.Executable, and that target
// is itself a usable executable — the one shape doctor must never silently
// repoint (I3): some other installed chottag is what runs there today.
// Missing, dangling, or resolving to something unusable are all left
// fixable, exactly as before.
func binLinkPointsAtAnotherWorkingBinary(e *Env, link string) bool {
	if same, err := e.SameFile(e.Executable, link); err == nil && same {
		return false
	}
	if _, err := os.Lstat(link); errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return isExecutable(link)
}

// shimNotInstalledDetail and shimNotInstalledHint are what the bin,
// rc-block and path rows share when shimNotInstalled is true (F176): `chottag
// uninstall` (no --purge) or a `setup` that never ran, not a broken
// install to repair.
const (
	shimNotInstalledDetail = "the shim is not installed (chottag uninstall, or setup never ran)"
	shimNotInstalledHint   = "chottag setup"
)

// shimNotInstalled is the shared judgement bin and rc-block read (F176),
// and path (F177): the shim counts as not installed when NEITHER
// bin/chottag nor bin/claude exists (Lstat, so a dangling link still counts
// as "exists") AND the rc file for $SHELL — or an unrecognised $SHELL —
// carries no chottag block. Read-only: no write, no side effect. Any
// partial shape (one link present, links present but no block, or a block
// but no links) is left for the caller to report and repair as before.
//
// A read that fails for a reason other than "the rc file does not exist"
// is never treated as "no block" — the check that owns that read (row 5)
// reports the real problem instead of this one folding it into "not
// installed".
func shimNotInstalled(e *Env) bool {
	for _, name := range []string{"chottag", "claude"} {
		if fileExists(filepath.Join(e.BinDir(), name)) {
			return false
		}
	}
	rc := e.RCPathFor(e.Shell, e.UserHome)
	if rc == "" {
		return true
	}
	data, err := os.ReadFile(rc)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	return !strings.Contains(string(data), e.RCBlock(e.BinDir()))
}

// binCheck is row 4. Fix only ever creates a MISSING or DANGLING link
// (I3): Detect already turns every other bad shape — a link pointing at
// another working binary, or doctor itself running from a temporary one —
// report-only (its Hint is set, so Run's own `fixable` gate never calls
// Fix for it at all; see runOne). Fix repeats both checks anyway, as
// belt-and-braces matching caCheck's own.
func binCheck() Check {
	return Check{
		ID: "bin",
		Detect: func(e *Env) Finding {
			var bad []string
			elsewhere := false
			for _, name := range []string{"chottag", "claude"} {
				link := filepath.Join(e.BinDir(), name)
				if same, err := e.SameFile(e.Executable, link); err == nil && same {
					continue
				}
				bad = append(bad, "bin/"+name)
				if binLinkPointsAtAnotherWorkingBinary(e, link) {
					elsewhere = true
				}
			}
			if len(bad) == 0 {
				return Finding{Status: StatusOK, Detail: "bin/chottag and bin/claude point at " + e.Executable}
			}
			if shimNotInstalled(e) {
				return Finding{Status: StatusInfo, Detail: shimNotInstalledDetail, Hint: shimNotInstalledHint}
			}
			var detail string
			switch len(bad) {
			case 1:
				detail = bad[0] + " does not point at " + e.Executable
			default:
				detail = strings.Join(bad, " and ") + " do not point at " + e.Executable
			}
			f := Finding{Status: StatusProblem, Detail: detail}
			if elsewhere || isUnderTempDir(e) {
				f.Hint = binSetupHint
			}
			return f
		},
		Fix: func(e *Env) error {
			if isUnderTempDir(e) {
				return fmt.Errorf("chottag is running from a temporary binary (%s); %s", e.Executable, binSetupHint)
			}
			for _, name := range []string{"chottag", "claude"} {
				if binLinkPointsAtAnotherWorkingBinary(e, filepath.Join(e.BinDir(), name)) {
					return fmt.Errorf("bin/%s already points at another working binary; %s", name, binSetupHint)
				}
			}
			return e.ProvisionBin(e.Executable, e.BinDir())
		},
	}
}

// exportLine is every export line of the block (CHOTTAG_HOME's, when
// CHOTTAG_HOME was set, ahead of PATH's — cli.rcBlock's shape), joined
// with "; " so an unrecognised-shell user typing the hint by hand gets the
// whole block's effect. Returning only the first line used to silently
// drop the PATH line whenever a CHOTTAG_HOME line came before it.
func exportLine(block string) string {
	var lines []string
	for _, l := range strings.Split(block, "\n") {
		if strings.HasPrefix(l, "export ") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return strings.TrimSpace(block)
	}
	return strings.Join(lines, "; ")
}

// rcBlockCheck is row 5. os.ReadFile follows a symlinked rc (a dotfiles
// repo), and the real writeRCBlock writes the link's target.
func rcBlockCheck() Check {
	return Check{
		ID: "rc-block",
		Detect: func(e *Env) Finding {
			notInstalled := shimNotInstalled(e)
			block := e.RCBlock(e.BinDir())
			rc := e.RCPathFor(e.Shell, e.UserHome)
			if rc == "" {
				if notInstalled {
					return Finding{Status: StatusInfo, Detail: shimNotInstalledDetail, Hint: shimNotInstalledHint}
				}
				return Finding{Status: StatusInfo, Detail: fmt.Sprintf("unrecognised $SHELL %q; add this to your shell's rc yourself: %s", e.Shell, exportLine(block))}
			}
			data, err := os.ReadFile(rc)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if notInstalled {
					return Finding{Status: StatusInfo, Detail: shimNotInstalledDetail, Hint: shimNotInstalledHint}
				}
				return Finding{Status: StatusProblem, Detail: rc + " does not exist, so nothing puts " + e.BinDir() + " first on PATH; after the fix, open a new shell"}
			case err != nil:
				return Finding{Status: StatusProblem, Detail: fmt.Sprintf("cannot read %s: %v", rc, err), Hint: "check the permissions of " + rc}
			case strings.Contains(string(data), block):
				return Finding{Status: StatusOK, Detail: rc + " puts " + e.BinDir() + " first on PATH"}
			}
			if notInstalled {
				return Finding{Status: StatusInfo, Detail: shimNotInstalledDetail, Hint: shimNotInstalledHint}
			}
			return Finding{Status: StatusProblem, Detail: rc + " has no chottag block for " + e.BinDir() + "; after the fix, open a new shell"}
		},
		Fix: func(e *Env) error { return e.WriteRCBlock(e.RCPathFor(e.Shell, e.UserHome), e.BinDir()) },
	}
}

// pathCheck is row 6. It sees only this process's PATH, so it is never a
// problem (D5).
func pathCheck() Check {
	return Check{ID: "path", Detect: func(e *Env) Finding {
		if shimNotInstalled(e) {
			return Finding{Status: StatusInfo, Detail: shimNotInstalledDetail, Hint: shimNotInstalledHint}
		}
		first := ""
		for _, dir := range filepath.SplitList(e.PATH) {
			if dir != "" && isExecutable(filepath.Join(dir, "claude")) {
				first = dir
				break
			}
		}
		switch {
		case first == "":
			return Finding{Status: StatusInfo, Detail: "no claude on this process's PATH; open a new shell"}
		case samePath(first, e.BinDir()):
			return Finding{Status: StatusOK, Detail: e.BinDir() + " comes first on PATH"}
		}
		return Finding{Status: StatusInfo, Detail: fmt.Sprintf("this process's PATH finds claude in %s before %s; a new shell picks up the rc block", first, e.BinDir())}
	}}
}
