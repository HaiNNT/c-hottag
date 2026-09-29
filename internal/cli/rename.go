package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const renameUsage = "usage: chottag rename <old> <new>"

// renameResult is `rename --json`'s fields (M2 spec §3).
type renameResult struct {
	From         string   `json:"from"`
	To           string   `json:"to"`
	Roles        []string `json:"roles"`
	OwnerEntries int      `json:"ownerEntries"`
	Resumed      bool     `json:"resumed"`
	// Changed is false only for the no-op "nothing to do" case (old was
	// already spelled new and nothing was pending in owners.json); every
	// other outcome — a fresh rename or a resume — actually wrote
	// something, so Changed is true.
	Changed bool `json:"changed"`
}

// ownersEdit is owners.Edit. It is a seam so a test can interrupt rename
// between its two writes (M2 spec §6).
var ownersEdit = owners.Edit

// errOwnersUnchanged aborts the owners.Edit transaction when no entry
// names the old account: nothing is written, and no owners.json is created
// where none existed.
var errOwnersUnchanged = errors.New("owners.json has no entry to rewrite")

// runRename changes an account's display name (M2 spec §3, R21, D10). The
// slot dir and its Keychain item never change. Names are keys in two
// files, so the rename is two ordered writes:
//  1. one store.Update: the account's name, and serving and remote where
//     they held it;
//  2. one owners.Edit: every entry that named it.
//
// The daemon adopts both on its next roster tick.
//
// Step 1 is always resolved and written before step 2 takes owners.lock:
// Edit's lock-ordering rule forbids holding both locks.
//
// A run whose step 2 failed is finished by running the same command again.
// Then old is no longer registered, new is, and owners.json still names
// old, so only step 2 runs and the result says resumed. A case-only rename
// resumes the same way: old still matches, and its stored name already
// equals new — but that same shape (old still matches, stored name already
// equals new) also happens when nothing was ever pending, e.g. a re-run of
// an already-finished rename, or `rename` given a no-op. Step 2's own
// report (whether it rewrote anything) is what tells the two apart: an
// owners.json rewrite means it was a genuine resume; none means there was
// nothing to do, and the result says so instead (fix round 1).
//
// It takes no flags: a stray one is exit 2 (spec §5.3).
func runRename(args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, renameUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) != 2 {
		return r.Usage(renameUsage)
	}
	oldName, newName := args[0], args[1]
	if err := store.ValidName(newName); err != nil {
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

	res := renameResult{From: oldName, To: newName, Roles: []string{}}
	// An exact match only (F15): Find's prefix and email resolution would
	// rename the wrong account.
	a, found := findExact(&st, oldName)
	// alreadyNamed is the ambiguous shape step 1 can leave behind: old still
	// resolves (case-insensitively) to an account already spelled exactly
	// new. That is either a case-only rename's step 2 resuming, or nothing
	// pending at all — the owners step below is what tells them apart.
	alreadyNamed := found && a.Name == newName
	switch {
	case alreadyNamed:
		res.Resumed = true
	case found:
		if _, err := s.Update(func(st *store.State) error {
			from, roles, err := st.Rename(oldName, newName)
			res.From, res.Roles = from, roles
			return err
		}); err != nil {
			if errors.Is(err, store.ErrExists) {
				return r.Fail(exit.Usage, codeNameTaken, fmt.Sprintf("cannot rename %s to %s: %v", oldName, newName, err), map[string]any{"name": newName})
			}
			return r.FailErr(err)
		}
	default:
		newAcct, ok := findExact(&st, newName)
		if !ok {
			return r.FailErr(fmt.Errorf("%w: %q", store.ErrNotFound, oldName))
		}
		// Step 2 must rewrite owners.json to the account's actual
		// registered spelling, not necessarily what was typed here:
		// state.json's step 1 already landed under its own case, and a
		// mismatched write would leave an entry RenameAccount's own
		// exact-spelling "already done" check never recognizes as finished.
		newName = newAcct.Name
		res.To = newName
		res.Resumed = true
	}

	n, err := renameOwners(h, oldName, newName, r)
	switch {
	case errors.Is(err, errOwnersUnchanged):
		if !found {
			// Not registered, and nothing to resume: an unknown name.
			return r.FailErr(fmt.Errorf("%w: %q", store.ErrNotFound, oldName))
		}
		if alreadyNamed {
			// Nothing was pending in owners.json either: this run found
			// nothing to do at all, not a resume.
			res.Resumed = false
		}
	case err != nil:
		if found && !res.Resumed {
			return r.Fail(exit.Error, codeInternal, fmt.Sprintf("renamed %s to %s in state.json, but rewriting owners.json failed: %v; run `chottag rename %s %s` again to finish", res.From, newName, err, res.From, newName), nil)
		}
		return r.FailErr(err)
	}
	res.OwnerEntries = n
	res.Changed = !(alreadyNamed && !res.Resumed)

	switch {
	case alreadyNamed && !res.Resumed:
		r.Text("nothing to do: %s is already named %s\n", res.From, res.To)
	case res.Resumed:
		r.Text("renamed %s -> %s (resumed; owner entries: %d)\n", res.From, res.To, n)
	default:
		roles := "none"
		if len(res.Roles) > 0 {
			roles = strings.Join(res.Roles, ", ")
		}
		r.Text("renamed %s -> %s (roles: %s; owner entries: %d)\n", res.From, res.To, roles, n)
	}
	return r.OK(res)
}

// renameOwners is step 2: one owners.Edit transaction that rewrites every
// entry naming from, case-insensitively, to to. It returns
// errOwnersUnchanged, having written nothing, when no entry matched.
func renameOwners(h, from, to string, r *reporter) (int, error) {
	n := 0
	err := ownersEdit(filepath.Join(h, "owners.json"), func(tx *owners.Tx) error {
		if tx.Recovered {
			r.Warn(warnOwnersRecovered, ownRecoveredLine)
		}
		n = tx.RenameAccount(from, to)
		if n == 0 {
			return errOwnersUnchanged
		}
		return nil
	})
	return n, err
}
