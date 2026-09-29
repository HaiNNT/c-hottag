package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const ownUsage = "usage: chottag own <kind> <id> [<account>]"

// ownKinds maps the CLI's kind argument to router.Kind. It is written out
// rather than derived, so that adding a router.Kind does not silently add a
// CLI surface, and so an unknown kind can name the valid set.
//
// Note there is no "routine": spec §4.1's prose names one, router.Kind has
// never had it, and the fourth kind is "connector".
var ownKinds = map[string]router.Kind{
	"session":     router.KindSession,
	"environment": router.KindEnvironment,
	"artifact":    router.KindArtifact,
	"connector":   router.KindConnector,
}

// ownResult is `own --json`'s fields (spec §5.3). The query form reports
// the current owner with reassigned false.
type ownResult struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Account    string `json:"account"`
	Reassigned bool   `json:"reassigned"`
}

// ownRecoveredLine is the warning both forms print when owners.json had to
// be moved aside.
const ownRecoveredLine = "chottag: owners.json was corrupt and has been moved aside"

// runOwn re-attributes one already-created claude.ai object (`chottag own
// <kind> <id> <account>`), or prints its current owner (`chottag own <kind>
// <id>`). This is owners.Reassign's first production caller (F27): first-
// writer-wins otherwise makes a wrong attribution permanent.
//
// An id the owner map does not already know is rejected here, in the
// command, rather than passed through to Reassign: Reassign creates on a
// missing id deliberately (its doc comment has said so since M1a, for a
// future automatic repair path that reassigns blind), but at the CLI an
// unrecognised id is far more likely a typo, and silently creating an owner
// row for a typo is worse than an error.
//
// The reassign path runs the existence check and the write inside one
// owners.Edit transaction (§4.7), so the two can never be separated by a
// concurrent writer — see the comment at that call. The read-only query
// form still uses owners.Open, since it only reads.
//
// It takes no flags: a stray one is exit 2, never a kind, an id or an
// account (spec §5.3).
func runOwn(home string, args []string, r *reporter) int {
	args, err := positionals(args)
	if err != nil {
		fmt.Fprintf(r.Stderr(), "chottag: %v\n%s\n", err, ownUsage)
		return r.FailNoText(exit.Usage, codeUsage, err.Error(), nil)
	}
	if len(args) < 2 || len(args) > 3 {
		return r.Usage(ownUsage)
	}
	kind, ok := ownKinds[args[0]]
	if !ok {
		valid := make([]string, 0, len(ownKinds))
		for k := range ownKinds {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return r.Fail(exit.Usage, codeUsage, fmt.Sprintf("unknown kind %q; valid kinds are %s", args[0], strings.Join(valid, ", ")), nil)
	}
	id := args[1]
	notFound := map[string]any{"kind": args[0], "id": id}

	if len(args) == 2 {
		own, err := owners.Open(filepath.Join(home, "owners.json"))
		if err != nil {
			return r.FailErr(err)
		}
		// Close flushes the newest write through owners.Map's writer
		// goroutine before this function returns; without it a Reassign
		// would sit in memory only and never reach disk (M1c4) — moot for
		// this read-only form today, but Close is still required to release
		// the writer goroutine Open started.
		defer own.Close()
		if own.Recovered {
			r.Warn(warnOwnersRecovered, ownRecoveredLine)
		}
		acct, ok := own.Lookup(kind, id)
		if !ok {
			return r.Fail(exit.Error, codeNotFound, fmt.Sprintf("%s %s has no recorded owner", args[0], id), notFound)
		}
		r.Text("%s %s: %s\n", args[0], id, acct)
		return r.OK(ownResult{Kind: args[0], ID: id, Account: acct})
	}

	s := store.Store{Dir: home}
	st, err := s.Load()
	if err != nil {
		return r.FailErr(err)
	}
	// args[2] is what the user typed at the prompt: Find's unique
	// name/email/prefix resolution is the intended convenience here (spec
	// §5, F16), not an identity check — do not replace with an exact-only
	// match. Resolved here, BEFORE owners.Edit below, so state.lock and
	// owners.lock are never held at once (Edit's own doc comment states
	// this ordering rule).
	a, err := st.Find(args[2])
	if err != nil {
		return r.FailErr(err)
	}

	// The lookup and the reassignment are ONE transaction (§4.7): across
	// processes, "check the id exists" followed by "write the new owner"
	// are two separate steps with a window between them.
	//
	// It works while the daemon is running, which is the normal case: the
	// lock makes this correction survive the daemon's next write, and the
	// daemon adopts it on its next roster tick. M1c5 shipped this command
	// refusing with exit 3 whenever a daemon looked live, because F96's
	// race would have reverted the correction silently; that guard is gone
	// because the defect it guarded is fixed.
	errNoOwner := errors.New("no recorded owner")
	err = owners.Edit(filepath.Join(home, "owners.json"), func(tx *owners.Tx) error {
		if tx.Recovered {
			r.Warn(warnOwnersRecovered, ownRecoveredLine)
		}
		if _, ok := tx.Lookup(kind, id); !ok {
			return errNoOwner
		}
		tx.Reassign(kind, id, a.Name, time.Now())
		return nil
	})
	if errors.Is(err, errNoOwner) {
		return r.Fail(exit.Error, codeNotFound, fmt.Sprintf("%s %s has no recorded owner; refusing to create one for what may be a typo", args[0], id), notFound)
	}
	if err != nil {
		return r.FailErr(err)
	}
	r.Text("%s %s: reassigned to %s\n", args[0], id, a.Name)
	return r.OK(ownResult{Kind: args[0], ID: id, Account: a.Name, Reassigned: true})
}
