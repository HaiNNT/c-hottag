package owners

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// Tx is a locked, in-memory view of owners.json for the duration of one
// Edit. It is deliberately NOT a Map: a short-lived process has no use for
// Map's writer goroutine, its dirty set, or its ErrClosed semantics, and
// exposing those to a synchronous caller would only invite misuse.
type Tx struct {
	// Recovered is true if the file was corrupt and has been moved to
	// <path>.corrupt, exactly as Map.Recovered reports for Open.
	Recovered bool

	m map[string]entry
}

func (t *Tx) Lookup(kind router.Kind, id string) (string, bool) {
	e, ok := t.m[Key(kind, id)]
	return e.Account, ok
}

// Reassign sets an owner unconditionally, mirroring Map.Reassign: an
// explicit owner change is intent, not observation.
func (t *Tx) Reassign(kind router.Kind, id, account string, now time.Time) {
	t.m[Key(kind, id)] = entry{Account: account, At: now, Pinned: kind == router.KindConnector}
}

// RenameAccount rewrites every entry whose account is from
// (case-insensitively) to to, and keeps each entry's time. It returns how
// many entries it rewrote. An entry already spelled exactly to is left
// alone and not counted, so re-running a finished rename rewrites nothing
// (M2 spec §3: rename is resumable).
func (t *Tx) RenameAccount(from, to string) int {
	n := 0
	for k, e := range t.m {
		if strings.EqualFold(e.Account, from) && e.Account != to {
			e.Account = to
			if isListerKey(k) {
				delete(t.m, k)
				k = rekeyLister(k, to)
			}
			t.m[k] = e
			n++
		}
	}
	return n
}

// lockPathFor is the single definition of owners.json's lock path. Both the
// writer (Map.lockPath) and the synchronous transaction (Edit) must derive it
// from here: two independent copies of the same expression could drift, and
// a drifted lock path disables cross-process exclusion silently, because
// each side would still lock a file successfully — just not the same one.
// The single derivation is what stops that: TestLockPathIsBesideOwnersJSON
// (merge_test.go) pins the derivation itself against a literal expected
// path, and TestEditWaitsForTheLock (edit_internal_test.go) pins that Edit
// actually uses it for cross-process exclusion — drifting Edit's own call
// site back to a second, independent expression fails that one.
func lockPathFor(path string) string {
	return filepath.Join(filepath.Dir(path), "owners.lock")
}

// Edit applies fn to owners.json under owners.lock and writes the result,
// synchronously. It is the short-lived-process counterpart to Map (§4.7),
// shaped like store.Update for the same reasons.
//
// fn's error aborts the transaction: nothing is written and the error is
// returned unwrapped, so a caller can errors.Is it.
//
// LOCK ORDERING: fn must not acquire state.lock — do any store.Store work
// (e.g. resolving an account name) BEFORE calling Edit. Nothing today takes
// both locks, and keeping it that way is cheaper than reasoning about the
// order in which two files' locks must be taken.
func Edit(path string, fn func(*Tx) error) error {
	unlock, err := fsutil.Lock(lockPathFor(path))
	if err != nil {
		return err
	}
	defer unlock()

	tx := &Tx{m: map[string]entry{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if json.Unmarshal(b, &tx.m) != nil {
			if err := os.Rename(path, path+".corrupt"); err != nil {
				return err
			}
			tx.m = map[string]entry{}
			tx.Recovered = true
		}
		if tx.m == nil {
			tx.m = map[string]entry{}
		}
	}

	if err := fn(tx); err != nil {
		return err
	}
	out, err := json.Marshal(tx.m)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out)
}
