package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// credsDelete removes a slot's local credential. A package variable for the
// same reason as claudeAuthExec: on darwin the real one talks to the user's
// Keychain, and internal/cli's TestMain installs a panicking default so a
// test that forgets to stub it fails loudly instead of destroying a real
// login.
var credsDelete = defaultCredsDelete

func defaultCredsDelete(slotDir string) error {
	return creds.Deleter{GOOS: runtime.GOOS, Run: creds.ExecRunner}.Delete(slotDir)
}

// SetCredsDeleteForTest replaces the delete seam and returns a func that
// restores WHATEVER WAS INSTALLED AT THE MOMENT OF THE CALL — never the
// production default. Inside internal/cli's test binary that is TestMain's
// panicking default, and restoring the real delete instead would silently
// disarm it after the first logout test: a later test that forgot to stub
// would then run `security delete-generic-password` against the user's real
// Keychain. Same shape, and same reason, as shim.SetSeamsForTest
// (internal/shim/testseams.go) and SetAuthExecForTest (Task 6, F130).
// There is deliberately no nil-means-real path.
func SetCredsDeleteForTest(fn func(slotDir string) error) (restore func()) {
	prev := credsDelete
	credsDelete = fn
	return func() { credsDelete = prev }
}

// errLogoutHoldsRole is the sentinel the post-confirmation re-check (F3)
// below returns from its Update when the account picked up a role between
// the pre-prompt check and now: the caller maps it to the exact same "holds
// a role" message and exit.UserAction the pre-prompt check uses, and
// touches nothing.
var errLogoutHoldsRole = errors.New("logout: account now holds a role")

// logoutHoldsRoleMessage is shared by the pre-prompt check and the
// post-prompt re-check (F3): both refusals are the identical situation
// (the account holds a role and --force was not given), just caught at two
// different moments, so they say the identical thing.
func logoutHoldsRoleMessage(name string) string {
	return fmt.Sprintf("chottag: %s is the serving or remote account; `chottag logout --force %s` moves the role first\n", name, name)
}

// logoutResult is `logout --json`'s fields (spec §5.3). removed is true
// when the slot directory was deleted; false for an out-of-tree account
// that --force only deregistered.
type logoutResult struct {
	Account string `json:"account"`
	Removed bool   `json:"removed"`
	Dir     string `json:"dir"`
	// MovedServing and MovedRemote name the account --force handed each
	// role to, and are absent when that role did not move to anyone
	// (F152).
	MovedServing string `json:"movedServing,omitempty"`
	MovedRemote  string `json:"movedRemote,omitempty"`
	// MovedInPools lists the roles --force handed off in pools other than
	// default (M8); absent when there were none.
	MovedInPools []poolMoved `json:"movedInPools,omitempty"`
}

// poolMoved is one extra pool's roles that --force handed to another account.
type poolMoved struct {
	Pool    string `json:"pool"`
	Serving string `json:"serving,omitempty"`
	Remote  string `json:"remote,omitempty"`
}

// holdsAnyRole reports whether name is the serving or remote account of any
// pool.
func holdsAnyRole(st store.State, name string) bool {
	for _, pn := range st.PoolNames() {
		p := st.PoolOf(pn)
		if strings.EqualFold(p.Serving, name) || strings.EqualFold(p.Remote, name) {
			return true
		}
	}
	return false
}

// moveRolesOffEverywhere moves name's roles off in the default pool (the
// returned serving and remote, exactly as before pools) and in every other
// pool it holds one.
func moveRolesOffEverywhere(st *store.State, name string) (servingTo, remoteTo string, others []poolMoved, err error) {
	if servingTo, remoteTo, err = moveRolesOff(st, name); err != nil {
		return "", "", nil, err
	}
	for _, pn := range st.PoolNames()[1:] {
		sv, rm, err := moveRolesOffInPool(st, pn, name)
		if err != nil {
			return "", "", nil, err
		}
		if sv != "" || rm != "" {
			others = append(others, poolMoved{Pool: pn, Serving: sv, Remote: rm})
		}
	}
	return servingTo, remoteTo, others, nil
}

// runLogout logs a slot out and removes it (spec §5).
//
// The order is the contract. Under --force, the account's serving/remote
// role moves off FIRST (spec §5), persisted on its own before anything
// else touches the network or disk. Then: revoke, delete the credential,
// remove the slot, deregister. If the revoke fails nothing is deleted,
// because a local delete while the server-side token stays live is the
// only outcome re-running cannot fix.
//
// An account whose Dir is genuinely outside chottag's accounts tree (F4) is
// never revoked, credential-deleted or RemoveAll'd — --force only
// deregisters it, leaving its login and directory exactly where they are.
//
// The slot's flock (F1) is taken after the confirmation prompt but BEFORE
// the F3 re-check (residual fix R2), and held through the revoke, the
// credential delete and the RemoveAll — the same lock `chottag login` holds
// for its own whole flow, in the same slot-then-state order. Locking is the
// blocking step here (`fsutil.Lock`, not `TryLock`): taking it AFTER the F3
// re-check, as an earlier version of this function did, left a window
// between that (now-stale) check and the lock actually being granted —
// which can block for as long as another holder needs it — during which a
// concurrent `chottag tag` could grant the account a role the check already
// passed; the revoke/delete/RemoveAll below would then run anyway, only for
// the final deregister to be refused. Locking first closes that window: the
// re-check below always runs on state as fresh as the moment the lock was
// actually granted. The daemon TryLocks the same path too
// (internal/tokens/tokens.go) rather than read a slot's credential while
// either login or logout is running.
//
// A missing slot dir is fine (R1): `fsutil.Lock` opens `<dir>/.chottag.lock`
// with O_CREATE, which needs the slot DIR itself to already exist, and
// fails with os.ErrNotExist if it does not — reachable by hand-removing a slot,
// or by an earlier logout that finished RemoveAll but whose own final
// deregistering Update then failed. There is nothing left in a missing slot
// to protect, so runLogout proceeds without the lock rather than exiting 1
// and leaving the account permanently stuck; it never MkdirAll's the slot
// back into existence just to lock it. The revoke and credsDelete below are
// both idempotent, and RemoveAll is a no-op on an already-missing dir.
//
// The [y/N] prompt runs only when stdin is a terminal and --json is off
// (spec §5.3); otherwise, without --yes, logout refuses with exit 3
// (confirmation_required) naming --yes, before touching anything. The
// prompt is written to stderr, so stdout stays the result (M1d-d). In JSON
// mode the `claude auth logout` child gets stderr as its stdout, as login's
// does.
func runLogout(args []string, stdin io.Reader, r *reporter) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	claudeFlag := fs.String("claude", "", "path to the real claude binary (default: the real claude on PATH, never chottag's own shim)")
	force := fs.Bool("force", false, "log out even if the account is serving or remote (moves the role first)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	// parseInterspersed (F2): `logout NAME --force --yes` must work exactly
	// like `logout --force --yes NAME`.
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 1 {
		return r.Usage("usage: chottag logout <name> [--force] [--yes] [--claude PATH]")
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
	acct, err := st.Find(positional[0])
	if err != nil {
		return r.FailErr(err)
	}
	name, dir := acct.Name, acct.Dir

	// F4: an out-of-tree dir is refused up front, before the prompt, exactly
	// like the holds-a-role refusal just below — there is nothing here a
	// confirmation could change.
	inTree := isInTreeSlot(s, h, dir)
	if !inTree && !*force {
		return r.Fail(exit.UserAction, codeOutOfTree, fmt.Sprintf("%s's directory %s is outside chottag's accounts tree; logout will not touch it; "+
			"`chottag logout --force %s` removes %s from chottag only, leaving its login and directory in place", name, dir, name, name),
			map[string]any{"account": name, "dir": dir})
	}

	holdsRole := holdsAnyRole(st, name)
	if holdsRole && !*force {
		return logoutRoleHeld(r, name)
	}

	if !*yes {
		// No prompt without a terminal (spec §5.3): a pipe, a file, /dev/null
		// or --json cannot answer it, so refuse and name --yes. A script
		// that piped `y` into logout before M1d-d passes --yes instead.
		if r.JSON() || !isInteractive(stdin) {
			return r.Fail(exit.UserAction, codeConfirmationRequired,
				fmt.Sprintf("logout %s needs confirmation, and there is no terminal to ask on (stdin is not a terminal, or --json is on); pass --yes to confirm", name),
				map[string]any{"account": name, "confirmWith": "--yes"})
		}
		fmt.Fprintf(r.Stderr(), "Log out and remove account %s (%s)? This revokes its login. [y/N]: ", name, acct.Email)
		line, rerr := bufio.NewReader(stdin).ReadString('\n')
		if rerr != nil && rerr != io.EOF {
			return r.FailErr(rerr)
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return r.Fail(exit.Error, codeAborted, "logout aborted, nothing changed", nil)
		}
	}

	// R2/F1: the slot's flock is taken here — after the prompt, but BEFORE
	// the F3 re-check just below — and only when in-tree (the out-of-tree
	// --force path only deregisters, nothing to protect). See the doc
	// comment above.
	var unlock func() error
	if inTree {
		var lockErr error
		unlock, lockErr = fsutil.Lock(creds.LockPath(dir))
		if lockErr != nil {
			// os.ErrNotExist is exactly fs.ErrNotExist (the os package
			// defines it as that value) — spelled via os here because this
			// function's own FlagSet local is already named fs, which would
			// shadow an "io/fs" import.
			if !errors.Is(lockErr, os.ErrNotExist) {
				return r.FailErr(lockErr)
			}
			// R1: the slot dir is already gone — proceed without a lock.
			unlock = nil
		}
	}
	if unlock != nil {
		defer unlock()
	}

	// F3: re-check on FRESH state after the prompt AND after the lock above
	// is granted — something else may have changed st.Serving/st.Remote
	// while the operator was answering the prompt, or while this call was
	// blocked waiting for the lock — and, under --force, move the role off
	// in the SAME Update, before anything below touches the network or disk
	// (spec §5). This replaces the old separate pre-revoke Update:
	// persisting the move here, rather than folding it into the final
	// Remove's Update, is what keeps the account demoted rather than stuck
	// serving/remote with a dead login if the revoke or the credential
	// delete fails later (fix round 1, I3).
	//
	// F4's out-of-tree-with-force case is deregistered right here too, in
	// the same Update as its role move: there is no revoke/delete/RemoveAll
	// step for it to race, so there is nothing gained by splitting that into
	// a second Update the way the in-tree path's final Remove is split out
	// below.
	deregisteredOutOfTree := false
	var movedServing, movedRemote string
	var movedElsewhere []poolMoved
	if _, err := s.Update(func(st *store.State) error {
		a, err := st.Find(name)
		if err != nil {
			return err
		}
		if holdsAnyRole(*st, a.Name) && !*force {
			return errLogoutHoldsRole
		}
		if *force {
			var err error
			if movedServing, movedRemote, movedElsewhere, err = moveRolesOffEverywhere(st, a.Name); err != nil {
				return err
			}
		}
		if !inTree {
			deregisteredOutOfTree = true
			return st.Remove(a.Name)
		}
		return nil
	}); err != nil {
		if errors.Is(err, errLogoutHoldsRole) {
			return logoutRoleHeld(r, name)
		}
		return r.FailErr(err)
	}
	reportMovedRoles(r, movedServing, movedRemote, movedElsewhere)
	if deregisteredOutOfTree {
		r.Text("%s's login and directory %s were left in place\n", name, dir)
		return r.OK(logoutResult{Account: name, Removed: false, Dir: dir, MovedServing: movedServing, MovedRemote: movedRemote, MovedInPools: movedElsewhere})
	}

	claudeBin, err := realClaudeBin(*claudeFlag, h)
	if err == nil {
		err = claudeAuthExec(claudeBin, dir, "logout", stdin, r.ChildStdout(), r.Stderr())
	}
	if err != nil {
		if !*force {
			fmt.Fprintf(r.Stderr(), "chottag: could not revoke %s's login: %v\n"+
				"chottag: nothing was deleted; fix the problem and run logout again, or use --force to remove it locally anyway\n", name, err)
			return r.FailNoText(exit.Error, codeRevokeFailed, fmt.Sprintf("could not revoke %s's login: %v", name, err), nil)
		}
		r.Warn(warnRevokeFailed, fmt.Sprintf("chottag: warning: could not revoke %s's login (%v); --force given, removing it locally anyway", name, err))
	}

	if err := credsDelete(dir); err != nil {
		// The revoke step is already done (or, under --force, already
		// attempted): re-running from here only needs to finish the local
		// half, so the message must send the user at --force rather than
		// plain `logout` again — a plain re-run would revoke a second time
		// for no reason, and if the first revoke already succeeded a
		// second one can itself fail loudly on some servers.
		fmt.Fprintf(r.Stderr(), "chottag: %s's login was already revoked, but deleting its local credential failed: %v\n"+
			"chottag: run `chottag logout --force %s` to finish the local cleanup\n", name, err, name)
		return r.FailNoText(exit.Error, codeCredentialDeleteFailed,
			fmt.Sprintf("%s's login was already revoked, but deleting its local credential failed: %v", name, err), nil)
	}

	// inTree is already proven above, including the aliased-spelling case
	// (P9): unlike the old s.IsSlotDir-only guard, this RemoveAll actually
	// runs for a slot reached through a path alias, not just a canonical
	// <h>/accounts/<name> spelling.
	if err := os.RemoveAll(dir); err != nil {
		return r.FailErr(err)
	}

	if _, err := s.Update(func(st *store.State) error {
		return st.Remove(name)
	}); err != nil {
		return r.FailErr(err)
	}

	r.Text("logged out: %s\n", name)
	return r.OK(logoutResult{Account: name, Removed: true, Dir: dir, MovedServing: movedServing, MovedRemote: movedRemote, MovedInPools: movedElsewhere})
}

// reportMovedRoles is logout --force's one line saying where the roles
// went (F152), printed as soon as the move is saved, before the revoke:
// the move stands even if a later step fails. Nothing is printed when no
// role moved to an account.
func reportMovedRoles(r *reporter, serving, remote string, others []poolMoved) {
	var parts []string
	if serving != "" {
		parts = append(parts, "serving to "+serving)
	}
	if remote != "" {
		parts = append(parts, "remote to "+remote)
	}
	for _, o := range others {
		if o.Serving != "" {
			parts = append(parts, "serving to "+o.Serving+" in "+o.Pool)
		}
		if o.Remote != "" {
			parts = append(parts, "remote to "+o.Remote+" in "+o.Pool)
		}
	}
	if len(parts) > 0 {
		r.Text("moved %s\n", strings.Join(parts, ", "))
	}
}

// logoutRoleHeld is the holds-a-role refusal, shared by the pre-prompt
// check and the post-prompt re-check (F3), with its unchanged text.
func logoutRoleHeld(r *reporter, name string) int {
	msg := logoutHoldsRoleMessage(name)
	fmt.Fprint(r.Stderr(), msg)
	return r.FailNoText(exit.UserAction, codeRoleHeld, strings.TrimSuffix(strings.TrimPrefix(msg, "chottag: "), "\n"),
		map[string]any{"account": name})
}

// moveRolesOff hands serving and remote to the next eligible account before
// name is removed. store.State.Remove refuses while either role points at
// name, so --force must move them rather than force the removal.
//
// Remote-only departure (Serving is NOT being vacated) is a special case:
// Remote goes straight to whoever is already serving, rather than through
// nextCandidate. nextCandidate always walks forward from st.Serving, and if
// name happens to sit immediately after st.Serving in rotation order,
// walking forward from st.Serving returns name itself — which the
// EqualFold guard below then (correctly) refuses, leaving Remote empty even
// though a perfectly good replacement (the account already serving) was
// sitting right there (fix round 1, I1). When Serving IS being vacated (or
// st.Serving is unexpectedly empty), the single nextCandidate result below
// is still what both roles get, for the same reason fix round 1's earlier
// correction gave: a second nextCandidate call made after st.Serving was
// already overwritten would walk forward from the NEW value instead, and
// with only two accounts that also wraps back to name.
//
// It returns the account each role moved to: "" for a role name did not
// hold, or one that nobody could take (serving, when no other account is
// in rotation).
func moveRolesOff(st *store.State, name string) (servingTo, remoteTo string, err error) {
	return moveRolesOffInPool(st, store.DefaultPool, name)
}

// moveRolesOffInPool is moveRolesOff for one pool: the walk runs over the
// pool's members, and the result is written back to the pool's roles.
func moveRolesOffInPool(whole *store.State, pool, name string) (servingTo, remoteTo string, err error) {
	view := poolView(*whole, pool)
	servingHeld := strings.EqualFold(view.Serving, name)
	remoteHeld := strings.EqualFold(view.Remote, name)
	servingTo, remoteTo = moveRolesOffView(&view, name)
	if servingHeld {
		if err := whole.SetPoolServing(pool, view.Serving); err != nil {
			return "", "", err
		}
	}
	if remoteHeld {
		if err := whole.SetPoolRemote(pool, view.Remote); err != nil {
			return "", "", err
		}
	}
	return servingTo, remoteTo, nil
}

// moveRolesOffView is the walk itself, on a one-pool view.
func moveRolesOffView(st *store.State, name string) (servingTo, remoteTo string) {
	servingHeld := strings.EqualFold(st.Serving, name)
	remoteHeld := strings.EqualFold(st.Remote, name)
	if !servingHeld && !remoteHeld {
		return "", ""
	}
	if remoteHeld && !servingHeld && st.Serving != "" {
		st.Remote = st.Serving
		return "", st.Remote
	}
	a, _, _, err := nextCandidate(st, &status.File{}, time.Now(), true)
	replacement := ""
	if err == nil && !strings.EqualFold(a.Name, name) {
		replacement = a.Name
	}
	if servingHeld {
		// Serving keeps nextCandidate's own semantics exactly: if nobody is
		// eligible to serve, Serving really does go empty (F6).
		st.Serving = replacement
	}
	if remoteHeld {
		// F6: Remote has no rotation requirement (spec §5 — an account
		// excluded from rotation still works as remote), so a replacement
		// nextCandidate refused only because everyone else is excluded from
		// ROTATION must not leave Remote empty too, as long as some other
		// account is registered at all. Falling back to the first other
		// account in registration order, ignoring rotation entirely, is
		// exactly that: a plain, deterministic "anyone but the departing
		// account".
		if replacement == "" {
			replacement = firstOtherAccount(st, name)
		}
		st.Remote = replacement
	}
	if servingHeld {
		servingTo = st.Serving
	}
	if remoteHeld {
		remoteTo = st.Remote
	}
	return servingTo, remoteTo
}

// firstOtherAccount returns the name of the first account in st.Accounts
// that is not name (case-insensitively), or "" if name is the only one
// registered.
func firstOtherAccount(st *store.State, name string) string {
	for _, a := range st.Accounts {
		if !strings.EqualFold(a.Name, name) {
			return a.Name
		}
	}
	return ""
}
