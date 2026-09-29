package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// purgeConfirmation is the one word --purge accepts on stdin. --purge
// deletes every slot login, which cannot be undone and which no other
// chottag command does. It therefore takes its own typed confirmation
// rather than riding on a global --yes (§5): a flag that means "do not ask
// me about routine things" must not also mean "destroy my logins".
const purgeConfirmation = "purge"

// uninstallResult is `uninstall --json`'s fields (spec §5.3): the paths
// actually removed (the rc file only when its fenced block was present),
// and whether --purge deleted the home.
type uninstallResult struct {
	Removed []string `json:"removed"`
	Purged  bool     `json:"purged"`
}

// runUninstall undoes what runSetup did: the fenced PATH block in the shell
// rc (removeRCBlock — Task 6, restoring the rc byte-for-byte per §6.1
// invariant 5), the bin/chottag and bin/claude symlinks, and ca/bundle.pem
// if the shim ever wrote one. It leaves ca/ca.pem, ca/ca.key and accounts/
// in place: those hold the CA identity and every slot's login, and
// uninstall alone must not be able to destroy a login.
//
// --purge additionally deletes the whole $CHOTTAG_HOME tree, but only after
// reading one line from stdin that is exactly "purge" — anything else
// aborts with a non-zero exit and changes nothing.
//
// Like runSetup and runAdopt, it reads the environment rather than taking
// home/rc paths as parameters: home() for $CHOTTAG_HOME, $HOME for the rc
// file's directory, $SHELL for which rc to edit.
//
// --purge's typed word is asked only on a terminal and never under --json
// (spec §5.3): otherwise it refuses with exit 3 (confirmation_required)
// before anything is removed. --yes does not cover it. Its prompt stays on
// stdout: it is shown only in text mode on a terminal.
func runUninstall(args []string, stdin io.Reader, r *reporter) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	purge := fs.Bool("purge", false, "also delete every account slot's login (irreversible; requires typed confirmation)")
	// parseInterspersed (spec §5.3): uninstall takes no positional at all, so
	// a bare fs.Parse(args) — which stops at the first non-flag token and
	// silently leaves everything from there, --purge included, unparsed in
	// fs.Args() — would let a stray token like an account name drop --purge
	// on the floor while still running every other removal step. Any
	// positional here is a usage error, never something to act on.
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage("usage: chottag uninstall [--purge]")
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}

	if *purge {
		if r.JSON() || !isInteractive(stdin) {
			return r.Fail(exit.UserAction, codeConfirmationRequired,
				fmt.Sprintf("uninstall --purge deletes every account's login and asks you to type %q first; run it in a terminal, without --json", purgeConfirmation), nil)
		}
		fmt.Fprintf(r.Stdout(), "This deletes every account slot's login in %s. This cannot be undone.\nType %q to confirm: ", h, purgeConfirmation)
		line, err := bufio.NewReader(stdin).ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if err != nil && err != io.EOF {
			return r.FailErr(err)
		}
		if line != purgeConfirmation {
			return r.Fail(exit.Error, codeAborted, "uninstall --purge aborted: confirmation did not match, nothing changed", nil)
		}
	}

	res := uninstallResult{Removed: []string{}}
	rcPath := rcPathFor(os.Getenv("SHELL"), os.Getenv("HOME"))
	if rcPath != "" {
		// Read first, only to report whether stripping actually changes
		// anything: removeRCBlock itself is unchanged, and this must use the
		// SAME test it uses internally (stripRCBlock(before) != before) —
		// not just "does the start fence appear somewhere" — or a start
		// fence with no matching end fence (which stripRCBlock, and so
		// removeRCBlock, leaves untouched) would be reported as removed
		// despite the file coming back byte-identical (spec §5.3 detail 9:
		// removed lists paths ACTUALLY removed).
		before, _ := os.ReadFile(rcPath)
		if err := removeRCBlock(rcPath); err != nil {
			return r.FailErr(err)
		}
		if stripRCBlock(string(before)) != string(before) {
			res.Removed = append(res.Removed, rcPath)
		}
	}

	binDir := filepath.Join(h, "bin")
	for _, name := range []string{"chottag", "claude"} {
		p := filepath.Join(binDir, name)
		if err := os.Remove(p); err == nil {
			res.Removed = append(res.Removed, p)
		} else if !os.IsNotExist(err) {
			return r.FailErr(err)
		}
	}
	bundle := filepath.Join(h, "ca", "bundle.pem")
	if err := os.Remove(bundle); err == nil {
		res.Removed = append(res.Removed, bundle)
	} else if !os.IsNotExist(err) {
		return r.FailErr(err)
	}
	r.Text("uninstalled the shim and shell PATH line\n")

	if *purge {
		if !looksLikeChottagHome(h) {
			return r.Fail(exit.Error, codeNotChottagHome, fmt.Sprintf("refusing to purge %s: it contains none of state.json, accounts/, ca/ca.pem, so it does not look like a chottag home (is $CHOTTAG_HOME set correctly?)", h), nil)
		}

		// On darwin a slot's credential is a Keychain item, not a file in
		// the slot (§4.5), so RemoveAll alone would orphan every login this
		// command promises to destroy. purgeSlotCredentials deletes the
		// credential for every slot dir it is about to remove, including an
		// orphan a failed run left behind, but never touches an account
		// whose Dir points outside the tree — RemoveAll can't reach that
		// one either, so its login stays reachable and the user is told so.
		if err := purgeSlotCredentials(h, r); err != nil {
			return r.FailNoText(exit.Error, codePurgeFailed, err.Error(), nil)
		}

		if err := os.RemoveAll(h); err != nil {
			return r.FailErr(err)
		}
		r.Text("purged %s\n", h)
		res.Purged = true
	}
	return r.OK(res)
}

// purgeSlotCredentials deletes the credential for every slot dir --purge is
// about to remove: the union, de-duplicated by absolute path, of the slot
// dirs state.json's accounts point at (including one reached through a path
// ALIAS of <h>/accounts, see isAliasedSlotDir) and every directory entry
// under <h>/accounts/ whose name is a valid account name (an orphan a
// failed run left behind). An account whose Dir sits outside <h>/accounts
// is never credential-deleted — RemoveAll(h) doesn't touch its dir either,
// so its login stays reachable — but its name is printed so the operator
// knows.
//
// It returns before RemoveAll runs, so any failure here leaves the whole
// tree in place; purgeFailed's two-line message is what every failure below
// reports, never "nothing was removed" — the rc block and shim were already
// removed above by the time this runs. The error it returns carries the
// same detail, for purge_failed's message. A "left in place" line is a
// warning in JSON mode (TextWarn keeps it on stdout in text).
func purgeSlotCredentials(h string, r *reporter) error {
	s := store.Store{Dir: h}
	st, err := s.Load()
	if err != nil {
		purgeFailed(r.Stderr(), err.Error())
		return err
	}

	targets := map[string]bool{}
	for _, a := range st.Accounts {
		switch {
		case isSymlinkDir(a.Dir) && (s.IsSlotDir(a.Dir) || isAliasedSlotDir(h, a.Dir)):
			// F4: a symlinked slot is left in place exactly like a
			// genuinely out-of-tree one. RemoveAll(h) only unlinks the
			// symlink entry itself, never whatever it points at, so there
			// is no real slot here for credsDelete to destroy — deleting
			// under this path could instead be deleting an unrelated
			// login the symlink happens to point at.
			r.TextWarn(warnLeftInPlace, fmt.Sprintf("%s's login at %s is a symlink and was left in place", a.Name, a.Dir))
		case s.IsSlotDir(a.Dir):
			targets[filepath.Clean(a.Dir)] = true
		case isAliasedSlotDir(h, a.Dir):
			// RemoveAll(h) will delete this dir too (it is the same file as
			// a canonical slot dir, just reached through a different path
			// spelling), and the Keychain service name is keyed by the
			// PATH STRING (F133), so the credential must be deleted under
			// a.Dir's own spelling, not the canonical one.
			targets[filepath.Clean(a.Dir)] = true
		default:
			r.TextWarn(warnLeftInPlace, fmt.Sprintf("%s's login is outside %s and was left in place", a.Name, h))
		}
	}

	entries, err := os.ReadDir(filepath.Join(h, "accounts"))
	if err != nil && !os.IsNotExist(err) {
		purgeFailed(r.Stderr(), err.Error())
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || store.ValidName(e.Name()) != nil {
			continue
		}
		targets[filepath.Join(h, "accounts", e.Name())] = true
	}

	paths := make([]string, 0, len(targets))
	for p := range targets {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := credsDelete(p); err != nil {
			detail := fmt.Sprintf("could not delete the login at %s: %v", p, err)
			purgeFailed(r.Stderr(), detail)
			return errors.New(detail)
		}
	}
	return nil
}

// isAliasedSlotDir reports whether dir, while not IsSlotDir(dir) by path
// STRING, still names a file RemoveAll(h) will delete: it is absolute, its
// parent is the SAME DIRECTORY as <h>/accounts by file identity rather than
// spelling, and its base name is a valid account name.
//
// File identity, not string comparison, is required because a slot can be
// reached through more than one spelling of the same directory: an APFS
// volume that case-folds (<h>/ACCOUNTS/A vs <h>/accounts/A), or
// $CHOTTAG_HOME itself resolving differently depending on a symlink
// (/tmp vs /private/tmp on macOS). The Keychain item is keyed by the path
// STRING actually used to create it (F133), so an alias is a distinct
// Keychain key from the canonical path and both must be deleted.
func isAliasedSlotDir(h, dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	clean := filepath.Clean(dir)
	if store.ValidName(filepath.Base(clean)) != nil {
		return false
	}
	parent, err := os.Stat(filepath.Dir(clean))
	if err != nil {
		return false
	}
	accounts, err := os.Stat(filepath.Join(h, "accounts"))
	if err != nil {
		return false
	}
	return os.SameFile(parent, accounts)
}

// isSymlinkDir reports whether dir itself — not something inside it — is a
// symlink, without following it. A missing dir is not a symlink (Lstat just
// errors), which is what every caller here wants: "missing" and "safe to
// treat as a real slot" are the same case for them, and only an actual
// symlink must be singled out (F4).
func isSymlinkDir(dir string) bool {
	fi, err := os.Lstat(dir)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// isInTreeSlot reports whether dir is a slot `logout` may revoke,
// credential-delete and RemoveAll (F4): it resolves into <h>/accounts,
// either by IsSlotDir's path-string check or, for a case-folded or
// symlinked-$CHOTTAG_HOME spelling, isAliasedSlotDir's file-identity check —
// and the slot itself is not a symlink. An account whose Dir fails either
// test is left untouched by logout, whatever state.json claims: the operator
// is told and can still deregister it with --force.
func isInTreeSlot(s store.Store, h, dir string) bool {
	if !s.IsSlotDir(dir) && !isAliasedSlotDir(h, dir) {
		return false
	}
	return !isSymlinkDir(dir)
}

// purgeFailed reports a failure inside purgeSlotCredentials. Every such
// failure uses this same two-line shape: what went wrong, then that the
// account tree was NOT removed and --purge can simply be re-run. By the
// time any of these can fire, removeRCBlock and the bin/ symlinks are
// already gone, so none of them may say "nothing was removed".
func purgeFailed(stderr io.Writer, detail string) {
	fmt.Fprintf(stderr, "chottag: %s\n"+
		"chottag: the account tree was NOT removed; run `chottag uninstall --purge` again once the problem is fixed\n", detail)
}

// looksLikeChottagHome reports whether h contains at least one path a real
// chottag home always creates (setup.go's setupDirs, plus state.json). It
// exists so `uninstall --purge` — an unconditional os.RemoveAll(h) — has
// some check beyond the typed confirmation before deleting an entire
// directory tree: the confirmation guards against an accidental purge, not
// against $CHOTTAG_HOME naming the wrong directory at all (a stale `export
// CHOTTAG_HOME=$HOME` in a shell rc, or a leaked test harness variable,
// would otherwise turn --purge into `rm -rf` on whatever that names —
// whole-branch review D7/F-I).
func looksLikeChottagHome(h string) bool {
	for _, marker := range []string{"state.json", "accounts", filepath.Join("ca", "ca.pem")} {
		if _, err := os.Stat(filepath.Join(h, marker)); err == nil {
			return true
		}
	}
	return false
}
