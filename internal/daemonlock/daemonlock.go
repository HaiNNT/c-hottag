// Package daemonlock identifies the one chottag daemon per home (spec §4.6,
// R42). `daemon run` holds an exclusive flock on <home>/run/daemon.lock for
// its whole life and keeps a JSON Record in that same file. `daemon
// stop|start|restart` find it with Inspect, a non-blocking try-lock.
//
// The kernel drops a flock when its holder dies (cleanly, by crash, or by
// SIGKILL), so a stale file never reads as a live daemon. A pid read while
// the lock is refused names the process holding it, never a reused pid.
// That is why `daemon stop` targets this record and never the health
// document's pid (spec §4.8).
//
// The flock itself is fsutil.TryLock, reused rather than re-implemented.
// Acquire loops over it for acquireWait: fsutil.Lock would block forever,
// and a second daemon must fail fast. Inspect calls it once. TryLock does
// not expose its descriptor, so the record is written through a second
// one. flock is advisory, so that write needs no lock of its own.
package daemonlock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// FileName is the lock's name inside <home>/run, which internal/session's
// Registry shares. Live() reads only <digits>.json there, so this name is
// not an entry; keep it from ever looking like one.
const FileName = "daemon.lock"

// Path is home's daemon lock.
func Path(home string) string { return filepath.Join(home, "run", FileName) }

// Record is what the lock holder writes into the lock. It never leaves the
// machine and is never served (spec §4.6).
type Record struct {
	PID        int       `json:"pid"`
	Started    time.Time `json:"started"`
	Supervisor string    `json:"supervisor"`      // "systemd", "launchd" or ""
	Label      string    `json:"label,omitempty"` // the launchd job label
}

// Same reports whether r and o describe the same daemon process. A pid alone
// could name a later daemon that happens to reuse it.
func (r Record) Same(o Record) bool { return r.PID == o.PID && r.Started.Equal(o.Started) }

// Current is this process's record.
func Current() Record {
	sup, label := detectSupervisor(os.Getenv, os.Getpid(), os.Getppid())
	return Record{PID: os.Getpid(), Started: time.Now().UTC(), Supervisor: sup, Label: label}
}

// AlreadyRunningError is Acquire's refusal: another process holds the lock.
// PID is read from the holder's record, and is 0 when the holder has not
// written one yet.
type AlreadyRunningError struct{ PID int }

func (e *AlreadyRunningError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("daemon already running (pid %d)", e.PID)
	}
	return "daemon already running (its pid is not recorded yet)"
}

// ErrUnreadableRecord means the lock is held but its record cannot be read.
// The holder is either still writing it (Inspect waits recordWait for that
// case) or the file is damaged, stale (a crash left it behind, naming a pid
// that is no longer alive), or caught mid-update. Either way the pid is
// unknown, and nothing may be signalled on the strength of it.
var ErrUnreadableRecord = errors.New("a daemon holds the lock but its record is unreadable")

// Status is what Inspect found. Record is valid only when Running is true
// and Inspect returned no error.
type Status struct {
	Running bool
	Record  Record
}

// Timing. These are vars only so this package's tests can shorten them.
var (
	acquireWait  = time.Second            // spec §4.6: "retrying for ~1s"
	acquireRetry = 20 * time.Millisecond  // between TryLock attempts and record reads
	recordWait   = 250 * time.Millisecond // a holder writes its record right after locking
)

// heldLock is what heldLocks stores: unlock keeps the lock's *os.File
// reachable for as long as it is held (I1), and the *heldLock's own
// identity is release's proof that the entry it is about to delete is
// still its own (NB1) — never just the path, which a later Acquire on the
// same home reuses as soon as this one unlocks.
type heldLock struct{ unlock func() error }

// heldLocks keeps every Acquire'd lock's *heldLock reachable for as long as
// it is held, keyed by path. Without this, a caller that discards Acquire's
// release return value leaves nothing referencing the *os.File inside
// fsutil's closure; the GC could then finalize it, closing the fd and
// silently dropping the flock out from under a live daemon. The package,
// not the caller, is what keeps the lock alive.
var (
	heldLocksMu sync.Mutex
	heldLocks   = map[string]*heldLock{}
)

// betweenTruncateAndUnlock runs in release between truncating the record
// and unlocking. Production code never sets it; tests use it to pin that
// order: if release ever unlocked before truncating, a new holder could
// acquire and write its own record in the gap, and the still-pending
// truncate would then wipe THAT holder's record instead of the stale one
// it was meant to clear (I2a).
var betweenTruncateAndUnlock = func() {}

// Acquire takes home's daemon lock and writes rec into it, retrying for
// acquireWait so that an Inspect's momentary hold does not fail a real
// start. The lock is held until release is called or the process exits;
// the caller does not need to keep release (or anything else) reachable
// in between — the package itself keeps the lock alive, so the Go runtime
// can never finalize it out from under a live daemon. release empties the
// record before it unlocks, so a clean exit never leaves a stale record
// for the next holder to misread, and it never unlocks first, so a racing
// new holder's record is never the thing it wipes. release is idempotent
// (NB1): every call after the first is a no-op that returns nil, and never
// touches a later Acquire's record or registry entry, even if the caller
// holds on to a stale release from a lock it already gave up. A refusal is
// an *AlreadyRunningError.
func Acquire(home string, rec Record) (release func() error, err error) {
	path := Path(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(acquireWait)
	for {
		unlock, ok, err := fsutil.TryLock(path)
		if err != nil {
			return nil, err
		}
		if ok {
			if err := writeRecord(path, rec); err != nil {
				unlock()
				return nil, fmt.Errorf("write %s: %w", path, err)
			}
			entry := &heldLock{unlock: unlock}
			heldLocksMu.Lock()
			heldLocks[path] = entry
			heldLocksMu.Unlock()

			var once sync.Once
			return func() error {
				var releaseErr error
				once.Do(func() {
					heldLocksMu.Lock()
					if heldLocks[path] == entry {
						delete(heldLocks, path)
					}
					heldLocksMu.Unlock()
					truncErr := truncateRecord(path)
					betweenTruncateAndUnlock()
					unlockErr := unlock()
					if truncErr != nil {
						releaseErr = truncErr
						return
					}
					releaseErr = unlockErr
				})
				return releaseErr
			}, nil
		}
		if !time.Now().Before(deadline) {
			held, _ := readRecord(path)
			return nil, &AlreadyRunningError{PID: held.PID}
		}
		time.Sleep(acquireRetry)
	}
}

// Inspect reports whether a daemon holds home's lock, and its record if so.
// It never blocks on the lock, and it creates nothing when no daemon ever
// ran. When the lock is free it is taken and released at once, which is
// why Acquire retries. A record is accepted only once stableLiveRecord
// confirms it: two reads acquireRetry apart that agree, naming a pid that
// is still alive. That rules out both a record caught mid-write and a
// stale one a crash left behind, still naming a since-freed pid.
//
// Every retry re-attempts the try-lock first, not just once before the
// loop (NB2): a holder can release while Inspect is mid-retry — e.g. right
// after it truncates but before Inspect notices — and the very next
// iteration must see the lock is free and answer "not running" at once,
// rather than spinning on an empty record until recordWait and then
// wrongly refusing with ErrUnreadableRecord.
func Inspect(home string) (Status, error) {
	path := Path(home)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return Status{}, nil
	} else if err != nil {
		return Status{}, err
	}
	deadline := time.Now().Add(recordWait)
	for {
		// TryLock's O_CREATE can recreate the file if a holder released and
		// its record got pruned in the instant before this call; harmless,
		// since an empty or freshly recreated file just reads as "not
		// running" below (M3).
		unlock, ok, err := fsutil.TryLock(path)
		if err != nil {
			return Status{}, err
		}
		if ok {
			return Status{}, unlock()
		}
		if rec, ok := stableLiveRecord(path); ok {
			return Status{Running: true, Record: rec}, nil
		}
		if !time.Now().Before(deadline) {
			return Status{Running: true}, ErrUnreadableRecord
		}
		time.Sleep(acquireRetry)
	}
}

// betweenStableReads runs inside stableLiveRecord between its two reads.
// Production code never sets it; tests use it to land a write exactly in
// that window, so a mid-update record is provably rejected on that attempt.
var betweenStableReads = func() {}

// stableLiveRecord reads path twice, acquireRetry apart, and reports the
// record only when both reads succeed, read identical, and name a pid that
// is still alive. Two matching reads rule out catching a record mid-write;
// the liveness check rules out a stale record a crash left behind, still
// naming a pid nothing holds any more.
func stableLiveRecord(path string) (Record, bool) {
	rec1, err := readRecord(path)
	if err != nil {
		return Record{}, false
	}
	betweenStableReads()
	time.Sleep(acquireRetry)
	rec2, err := readRecord(path)
	if err != nil || rec1 != rec2 {
		return Record{}, false
	}
	if !pidAlive(rec2.PID) {
		return Record{}, false
	}
	return rec2, true
}

// pidAliveFn holds the liveness check as an atomic.Pointer (NB3): Inspect
// (via stableLiveRecord) reads it on every check, and SetPIDAliveForTest
// can swap it from another goroutine (e.g. a caller's own concurrent
// Inspect calls) at any time. A plain package var read and written without
// synchronization would race under -race the moment those two happen
// concurrently.
var pidAliveFn atomic.Pointer[func(int) bool]

func init() {
	prod := productionPIDAlive
	pidAliveFn.Store(&prod)
}

// productionPIDAlive reports whether pid names a process on this machine.
// Signal 0 needs no permission to send; ESRCH is the only answer that
// means "no such process". EPERM (someone else's process) still counts as
// alive: what matters is whether the lock's holder exists, not whether
// this process may signal it.
func productionPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// pidAlive is the liveness check stableLiveRecord applies to a held lock's
// record; it calls through pidAliveFn so SetPIDAliveForTest can swap it
// race-free.
func pidAlive(pid int) bool {
	fn := pidAliveFn.Load()
	return (*fn)(pid)
}

// SetPIDAliveForTest replaces the liveness check Inspect applies to a held
// lock's record, and returns a func that restores whatever was installed at
// the moment of the call — mirroring internal/cli's SetAuthExecForTest
// exactly, and for the identical reason: nested swaps must restore to the
// PREVIOUS value, not fall back to production's syscall.Kill once the first
// stub's cleanup runs (F130).
//
// It is for tests in OTHER packages that hold this package's lock
// in-process with a fabricated pid (spec: internal/cli's daemon lifecycle
// tests); pidAlive itself is proven here, in this package.
//
// It lives in a non-test file so other packages' tests can reach it: a
// _test.go file compiles only into its own package's test binary (F103).
func SetPIDAliveForTest(fn func(pid int) bool) (restore func()) {
	orig := pidAliveFn.Load()
	pidAliveFn.Store(&fn)
	return func() { pidAliveFn.Store(orig) }
}

// writeRecord truncates and rewrites path in place. It must never become
// fsutil.WriteFileAtomic. A rename puts a new inode at path while this
// process's flock stays on the old, unlinked one, so the next Inspect would
// lock the new file and report "not running" while this daemon runs.
func writeRecord(path string, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// truncateRecord empties path's content while leaving the file, and this
// holder's flock on it, in place. release calls it before unlocking so a
// clean exit never leaves a stale record for whoever locks next to misread.
func truncateRecord(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func readRecord(path string) (Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, err
	}
	if rec.PID <= 0 {
		return Record{}, errors.New("record names no pid")
	}
	return rec, nil
}
