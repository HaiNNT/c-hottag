package usagelog

import (
	"os"
	"path/filepath"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// Lock takes an exclusive flock on usage/lock (import and compaction).
func (l Ledger) Lock() (unlock func(), err error) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, err
	}
	u, err := fsutil.Lock(filepath.Join(l.Dir, "lock"))
	if err != nil {
		return nil, err
	}
	return func() { _ = u() }, nil
}

// TryLock is Lock without waiting: ok is false when another process holds it.
func (l Ledger) TryLock() (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return nil, false, err
	}
	u, ok, err := fsutil.TryLock(filepath.Join(l.Dir, "lock"))
	if err != nil || !ok {
		return nil, false, err
	}
	return func() { _ = u() }, true, nil
}
