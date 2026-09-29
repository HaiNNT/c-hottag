// Package owners remembers which account created each claude.ai object (RC
// sessions and environments, artifacts, connectors) so later requests on
// that object keep using its creator after `chottag remote` changes, across
// daemon restarts. The daemon is the only writer.
package owners

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/router"
)

const DefaultMax = 10000

// ErrClosed is returned by Record, Reassign and Forget when they are called
// after Close. The call still mutates the in-memory map — Lookup reflects
// it immediately — but Close means the writer has already stopped, so
// nothing will ever persist it: without this, the caller would see a nil
// error for a write that silently never reaches disk, and Writes would not
// move either, so nothing could observe the loss. See Close's doc comment
// for the shutdown-ordering half of this contract.
var ErrClosed = errors.New("owners: closed")

type entry struct {
	Account string    `json:"account"`
	At      time.Time `json:"at"`
}

type Map struct {
	// Recovered is true if Open found a corrupt file, moved it to
	// <path>.corrupt and started empty.
	Recovered bool

	path   string
	max    int
	mu     sync.Mutex
	m      map[string]entry
	writes atomic.Uint64

	// d is what this process changed since its last successful write (see
	// merge.go). The writer applies it to whatever the file currently
	// holds, rather than overwriting the file with a snapshot of m: this
	// file has more than one writer and the daemon is not always the
	// winner (§4.7, F96).
	//
	// It replaces a `pending []byte` that queueLocked filled by marshalling
	// the whole map under mu. Marshalling moved to the writer along with
	// the merge, because the document can only be computed after the file
	// has been read under the lock.
	d dirty

	// inflight is the delta writePending has taken from m.d and is
	// currently trying to write, guarded by mu exactly like m.d. Between
	// take() and the write's outcome, m.d alone is empty and no longer
	// reflects this process's own not-yet-durable mutation (F104): a
	// lock-free Reload landing in that window would merge base+d and lose
	// it from m.m, and a subsequent Record on the same key would then
	// wrongly treat itself as first-writer. inflight closes that window by
	// staying visible to Reload's merge until the write's outcome (success
	// or fail()) clears it — see writePending, fail and Reload.
	//
	// Two narrower fixes were considered and rejected (fix-brief round 1,
	// Item 2). Patching fail() to also repair m.m — `m.m =
	// mergeInto(m.m, d)` — fails 3/3 against a live regression test: unlike
	// dirty.foldUnder, mergeInto's forced rule has no `d.has(k)` precedence
	// guard, so it would unconditionally re-stamp a key to the failed
	// write's STALE value even when a newer Reassign on that same key
	// arrived (and correctly won) during the in-flight write. And a
	// `d.minus(m.d)`-shaped fold-back only repairs the PERSISTENT window
	// (after a failure); it does nothing for the transient window a
	// SUCCEEDING write also opens, and does not stop the F19 violation from
	// reaching disk that Reload's own dropped read causes independently of
	// what fail() does (see the disk-loss trace in
	// TestReloadDuringAnInFlightWriteCanLoseTheFirstWriterOnDisk,
	// owners_test.go). inflight fixes the invariant itself — m.m always
	// reflects the file plus everything not yet durably written — rather
	// than patching either symptom.
	inflight dirty

	// lastWriteMtime and lastWriteSize are the file state this process has
	// already adopted — either its own last successful write, or the last
	// external change Reload merged in, since Reload advances them too — so
	// Reload can tell "the file changed underneath us" from "the file is
	// exactly what we already have". Zero until the first successful write
	// or Reload, which reads as "unknown" and makes Reload re-read once.
	// lastWriteFile is that same stat, kept for os.SameFile: mtime and size
	// alone miss a same-size rewrite within one mtime tick (Linux stamps
	// from a coarse clock), but every write here is temp-and-rename, so
	// that rewrite is a different file (F237). nil means "unknown" too.
	lastWriteMtime time.Time
	lastWriteSize  int64
	lastWriteFile  fs.FileInfo

	// writeGen counts every write writePending completes, bumped
	// unconditionally under mu regardless of whether the trailing os.Stat
	// below succeeded (D2, whole-branch fix round 2). Reload's guard
	// against racing its own writer keys off THIS, not
	// lastWriteMtime/lastWriteSize: those two advance only `if statErr ==
	// nil`, so "we could not stat the file we just wrote" would be
	// indistinguishable from "no write happened" if the guard used them —
	// a value that exists to answer a DIFFERENT question (is the file
	// externally changed) silently going inert for a question it was never
	// built to answer. A monotone counter has no such blind spot: it moves
	// on every completed write, stat or no stat.
	writeGen uint64

	// reloadTestHook, if non-nil, is called by Reload after it has read and
	// parsed the file but before it re-takes mu to apply the merge — see
	// export_test.go's SetReloadTestHook. nil in production.
	reloadTestHook func()

	// wake nudges the writer; done closes when the writer has stopped. Both
	// are the internal/cli/status.go pattern, which this deliberately
	// mirrors rather than reinvents.
	//
	// This replaces a second mutex taken while mu was still held. That
	// ordering bound write order to marshal order correctly, but it meant a
	// second writer parked on it WHILE HOLDING mu, so every Lookup queued
	// behind a whole fsync (F66, measured at 2.0008s). Here the caller
	// records its intent in m.d under mu and hands off; it never waits for
	// a write.
	wake    chan struct{}
	done    chan struct{}
	closed  bool
	onError func(error) // nil = drop

	// zeroIDs counts Record calls whose ids were empty — a recording route
	// that extracted nothing from the request/response, which otherwise
	// looks identical to a quiet session. Accessed without mu: Record must
	// still count this case when it returns before ever touching m.
	zeroIDs atomic.Uint64

	// write performs the actual disk write; it exists as a field, defaulting
	// to the local writeFileAtomic wrapper (installed by OpenMax), the same
	// shape as statusSink.write in internal/cli/status.go. It lets a test
	// replace the fsync with a controllable stand-in. Set once at
	// construction (or by a test before any write is queued); only the
	// writer goroutine ever reads it, never a caller's.
	write func(path string, b []byte) error
}

// openCorruptTestHook, if non-nil, is called by OpenMax's corrupt-file path
// right after the first (failed) unmarshal, before it acquires owners.lock
// — see export_test.go's SetOpenCorruptTestHook. Package-level rather than
// a *Map field like reloadTestHook: at this point in OpenMax there is no
// caller-visible *Map yet for a test to have attached a hook to beforehand.
// nil in production.
var openCorruptTestHook func()

func Open(path string) (*Map, error) { return OpenMax(path, DefaultMax) }

func OpenMax(path string, max int) (*Map, error) {
	m := &Map{
		path:     path,
		max:      max,
		m:        map[string]entry{},
		d:        newDirty(),
		inflight: newDirty(),
		write:    writeFileAtomic,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		go m.writeLoop()
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if json.Unmarshal(b, &m.m) != nil {
		// Before this fix, this was the one rename in the package taken
		// without the lock — exactly the race Reload's own doc comment
		// cites as the reason IT does not quarantine. §4.7 now states a
		// uniform policy: a corrupt-file rename always happens under the
		// lock. The lock is taken only on this already-rare path, so the
		// common case (a file that parses) never pays for it.
		if openCorruptTestHook != nil {
			openCorruptTestHook()
		}
		unlock, lockErr := fsutil.Lock(lockPathFor(path))
		if lockErr != nil {
			return nil, lockErr
		}
		defer unlock()
		// Re-read and re-check under the lock before renaming: the bytes
		// this Open call unmarshalled above were read before it held the
		// lock, so another process's OWN write (writePending's
		// readBaseLocked quarantines a corrupt file through this same
		// lock) may have already repaired the file while this call was
		// waiting to acquire it. Renaming without re-checking would
		// quarantine a file someone else just fixed.
		b, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		// Into a FRESH map, never m.m again (D1, whole-branch fix round 2):
		// encoding/json partially populates its destination map on a type
		// error before returning it — it keeps decoding and reports the
		// first error only at the end — so the FIRST Unmarshal above, the
		// one that failed, already left ghost entries in m.m. Unmarshalling
		// this re-read into that same map would merge a repaired document
		// with those ghosts instead of replacing them, and Lookup would
		// then return an owner that was never in any file. Confirmed:
		// json.Unmarshal([]byte(`{"ghost":{"account":"G"},"bad":{"account":123}}`), &m)
		// fails with "cannot unmarshal number into ... .bad.account" and
		// still leaves m["ghost"] = {Account:"G"}.
		var repaired map[string]entry
		if json.Unmarshal(b, &repaired) != nil {
			if err := os.Rename(path, path+".corrupt"); err != nil {
				return nil, err
			}
			m.m = map[string]entry{}
			m.Recovered = true
		} else {
			m.m = repaired
		}
	}
	if m.m == nil {
		m.m = map[string]entry{}
	}
	go m.writeLoop()
	return m, nil
}

// sessionPrefixes are stripped from session ids before keying, so the RC id
// ("cse_…") and the sessions-API id ("session_…") of one session share a key.
var sessionPrefixes = []string{"cse_", "session_"}

// Key is the map key for an object.
func Key(kind router.Kind, id string) string {
	if kind == router.KindSession {
		for _, p := range sessionPrefixes {
			if rest, ok := strings.CutPrefix(id, p); ok {
				id = rest
				break
			}
		}
	}
	return string(kind) + ":" + id
}

func (m *Map) Lookup(kind router.Kind, id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.m[Key(kind, id)]
	return e.Account, ok
}

// Record claims ownership of ids for account, for ids that have no owner
// yet. It writes the file only if an id was new.
//
// First writer wins (F19): the account that created an object keeps it.
// Live traffic showed a session re-listing objects under a newly pinned
// remote account and silently re-attributing them — harmless for an object
// both accounts can see, fatal for the follow-through guarantee on an
// artifact or an RC session. Use Reassign to change a known owner
// deliberately.
//
// Two caveats (F27), because they are easy to assume away otherwise:
// eviction can undo first-writer-wins on its own — once the oldest entry
// beyond max is dropped, the id has no recorded owner again, and whichever
// account next Records it becomes the new first writer; only Reassign can
// correct a mis-attribution like that. And Record never refreshes At for an
// id it already knows (the skip happens before At would be touched), so an
// entry's age always reflects when it was first created, never when it was
// last seen.
func (m *Map) Record(kind router.Kind, ids []string, account string, now time.Time) error {
	if account == "" {
		return errors.New("owners: Record: account is empty")
	}
	if len(ids) == 0 {
		m.zeroIDs.Add(1)
		return nil
	}
	m.mu.Lock()
	changed := false
	for _, id := range ids {
		k := Key(kind, id)
		if _, exists := m.m[k]; exists {
			continue
		}
		e := entry{Account: account, At: now}
		m.m[k] = e
		m.d.record(k, e)
		changed = true
	}
	if !changed {
		m.mu.Unlock()
		return nil
	}
	m.evict()
	err := m.queueLocked()
	m.mu.Unlock()
	return err
}

// Reassign deliberately moves an object to a different account, overriding
// Record's first-writer-wins rule (F19). It always writes (unless the Map
// has been Close()d, in which case it still updates memory but returns
// ErrClosed instead of persisting — see Close): an explicit Reassign call
// is, by definition, an intended owner change, not a no-op to be skipped.
// Reassign on an id with no recorded owner yet creates it
// rather than erroring — that is the more useful semantic for the operator
// surface this will eventually get (a repair path correcting a
// mis-attribution should not first have to check whether the map already
// knows the id).
func (m *Map) Reassign(kind router.Kind, id, account string, now time.Time) error {
	if account == "" {
		return errors.New("owners: Reassign: account is empty")
	}
	if id == "" {
		return errors.New("owners: Reassign: id is empty")
	}
	m.mu.Lock()
	k := Key(kind, id)
	e := entry{Account: account, At: now}
	m.m[k] = e
	m.d.force(k, e)
	m.evict()
	err := m.queueLocked()
	m.mu.Unlock()
	return err
}

// ZeroIDExtractions counts Record calls whose ids were empty — a recording
// route that yielded no ids, which otherwise looks identical to a quiet
// session with nothing to record.
func (m *Map) ZeroIDExtractions() uint64 { return m.zeroIDs.Load() }

// Forget drops every object owned by account (case-insensitive), e.g. after
// logout; requests on those objects fall back to the current remote account.
func (m *Map) Forget(account string) error {
	m.mu.Lock()
	changed := false
	for k, e := range m.m {
		if strings.EqualFold(e.Account, account) {
			m.d.forget(k, e.Account)
			delete(m.m, k)
			changed = true
		}
	}
	if !changed {
		m.mu.Unlock()
		return nil
	}
	err := m.queueLocked()
	m.mu.Unlock()
	return err
}

// RenameAccount rewrites every entry whose account is from
// (case-insensitively) to to, and keeps each entry's time — mirroring
// Tx.RenameAccount (edit.go), the short-lived-process counterpart used by
// `chottag rename`'s own step 2, but through this Map's asynchronous write
// path instead of a synchronous owners.Edit transaction (F174): the daemon
// holds this Map open for its whole lifetime, so it is what watchRoster
// calls the moment a roster tick sees an account's Dir keep its slot but
// change its Name — finishing an interrupted rename's step 2 itself,
// without waiting for (or requiring) a re-run of `chottag rename`.
//
// An entry already spelled exactly to is left alone and not counted, so
// calling this again after it has already run — or after owners.Edit has
// already rewritten the file out from under this process, which the next
// Reload adopts — rewrites nothing and queues no write.
func (m *Map) RenameAccount(from, to string) (int, error) {
	m.mu.Lock()
	n := 0
	for k, e := range m.m {
		if strings.EqualFold(e.Account, from) && e.Account != to {
			e.Account = to
			m.m[k] = e
			m.d.force(k, e)
			n++
		}
	}
	if n == 0 {
		m.mu.Unlock()
		return 0, nil
	}
	err := m.queueLocked()
	m.mu.Unlock()
	return n, err
}

// evict drops the oldest entries beyond max. Caller holds mu.
func (m *Map) evict() { evictMap(m.m, m.max) }

// writeLoop performs the fsync-ing writes off every caller's goroutine. It
// exits when Close closes wake.
func (m *Map) writeLoop() {
	defer close(m.done)
	for range m.wake {
		m.writePending()
	}
	// This trailing call IS load-bearing, unlike when this comment first
	// described it (the earlier reasoning is wrong for the current code,
	// see below). writePending's fail() folds a failed write's dirty set
	// back into m.d WITHOUT queuing a new wake — deliberately: on a
	// persistent error (a full disk) an automatic re-wake would become a
	// tight fail-wake-fail loop burning CPU and the error sink's throttle
	// (queueLocked's own doc comment has the liveness argument for the
	// non-blocking send; the "don't re-wake on failure" choice is this
	// function's own). So a failed write can leave a non-empty m.d with no
	// token ever queued for it again, and if no further mutation arrives
	// before Close, this call is the ONLY thing that still flushes it.
	// Measured: with this line deleted,
	// TestCloseFlushesAFoldedBackDeltaWithNoFurtherMutation (owners_test.go)
	// fails 5/5; with it present, 5/5 pass.
	//
	// The earlier reasoning here assumed queueLocked always attempted a
	// wake send while holding mu, so a queued document always implied a
	// queued token — true under the snapshot-write design this comment was
	// written for, where every write path went through queueLocked. It no
	// longer holds now that a failed write's retry is requeued by fail()
	// directly into m.d, bypassing queueLocked and its wake send entirely.
	m.writePending()
}

func (m *Map) lockPath() string { return lockPathFor(m.path) }

func (m *Map) writePending() {
	// The dirty set and onError are captured together under mu: SetOnError
	// can be called concurrently, so reading m.onError after the unlock
	// would be a data race. take() leaves m.d empty so mutations arriving
	// during this write accumulate behind us rather than being written
	// twice.
	//
	// The empty() check and the m.inflight assignment happen in this SAME
	// critical section (F104): m.inflight publishes the taken set so a
	// concurrent Reload can still see it while m.d itself is empty, and
	// checking empty() under the lock is what keeps m.inflight from ever
	// being set on the early-return path below.
	m.mu.Lock()
	d := m.d.take()
	onError := m.onError
	empty := d.empty()
	if !empty {
		m.inflight = d
	}
	m.mu.Unlock()
	if empty {
		return
	}

	// On EVERY failure path below, the taken set goes back under whatever
	// arrived meanwhile (foldUnder) and the write is retried on the next
	// wake. This is new and it is load-bearing: when writes were whole-map
	// snapshots a dropped document cost nothing, because the next mutation
	// re-marshalled everything. A dropped DELTA is permanent data loss.
	fail := func(err error) {
		m.mu.Lock()
		m.d.foldUnder(d)
		// Clearing inflight here IS load-bearing (fix-brief round 2, Item
		// 1) — a fix-brief round 1 comment previously claimed the opposite
		// ("provably redundant"/"equivalent mutant") and was wrong: the
		// argument conflated what m.d CONTAINS with whether mergeInto will
		// APPLY it. mergeInto(mergeInto(base, m.inflight), m.d) mutates
		// base in place, so the second call sees a base the stale inflight
		// has already rewritten — and two of mergeInto's three rules are
		// conditional on THAT base, not unconditional the way `forced` is:
		// `recorded` only applies `if _, ok := base[k]; !ok`, so a stale
		// inflight entry that re-inserted k makes m.d's own newer recorded
		// entry for k get silently skipped
		// (TestReloadDoesNotLetAStaleInflightBeatANewerRecordAfterFail);
		// and `forgotten` only deletes if base[k] exists AND EqualFolds the
		// forgotten account, so a stale inflight entry re-inserting k under
		// a DIFFERENT account defeats the match and resurrects a key m.d
		// says should be gone
		// (TestReloadDoesNotResurrectAForgottenKeyFromAStaleInflightAfterFail).
		// Deleting this line is CAUGHT by both.
		m.inflight = newDirty()
		m.mu.Unlock()
		if onError != nil {
			onError(err)
		}
	}

	unlock, err := fsutil.Lock(m.lockPath())
	if err != nil {
		fail(err)
		return
	}
	defer unlock()

	base, corrupted, err := m.readBaseLocked()
	if err != nil {
		fail(err)
		return
	}
	if corrupted != nil && onError != nil {
		// Not a fail() path: readBaseLocked has already recovered a base
		// from memory and the write below still proceeds from it. This is
		// purely so the operator is told their file just moved, instead of
		// the rename being completely silent (an earlier version of this
		// code reported neither this nor Map.Recovered for it).
		onError(corrupted)
	}
	merged := mergeInto(base, d)
	evictMap(merged, m.max)
	b, err := json.Marshal(merged)
	if err != nil {
		fail(err)
		return
	}
	if err := m.write(m.path, b); err != nil {
		fail(err)
		return
	}

	// os.Stat happens before the lock below, not under it: no fsync is
	// ever taken under mu in this design, and a stat has no business there
	// either — mu IS the hot-path lock (Lookup takes it on every request)
	// and it is the one thing this design exists to protect (F66).
	fi, statErr := os.Stat(m.path)

	// Adopt the merged document as this process's view, re-applying
	// anything that arrived while the write was in flight. Without that
	// re-application those mutations would be dropped from m.m — they are
	// still in m.d and will be written next time, but Lookup would stop
	// reporting them in the meantime.
	m.mu.Lock()
	m.m = mergeInto(merged, m.d)
	m.inflight = newDirty()
	// Unconditional: this write completed (m.write above returned nil)
	// regardless of whether the stat that follows did — see writeGen's own
	// field comment for why lastWriteMtime/lastWriteSize cannot carry this
	// signal on their own.
	m.writeGen++
	if statErr == nil {
		m.lastWriteMtime, m.lastWriteSize, m.lastWriteFile = fi.ModTime(), fi.Size(), fi
	}
	m.mu.Unlock()
}

// baseFromMemoryLocked is the best-available base when the file cannot
// supply one — it is absent, or it would not parse. Caller holds
// owners.lock and NOT m.mu.
func (m *Map) baseFromMemoryLocked() map[string]entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	base := make(map[string]entry, len(m.m))
	for k, e := range m.m {
		base[k] = e
	}
	return base
}

// readBaseLocked reads the document the merge applies to. Caller holds
// owners.lock.
//
// The file cannot supply a base through two doors — it is absent, or it
// would not parse — and both build the base from a COPY OF THIS PROCESS'S
// MEMORY, not an empty map. That distinction is the whole point: with an
// empty base the merge would apply only the dirty set, and every key this
// process knew but had not touched since its last write would be silently
// dropped. On a genuinely fresh start this is no more expensive than the
// empty map it replaces, because m.m is itself empty then.
//
// corrupted is non-nil (with err nil) only when the file failed to parse
// and was renamed aside — informational, not fatal: the write below still
// proceeds, from the recovered base. The caller reports it through onError
// rather than aborting.
func (m *Map) readBaseLocked() (base map[string]entry, corrupted error, err error) {
	b, err := os.ReadFile(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m.baseFromMemoryLocked(), nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if json.Unmarshal(b, &base) != nil {
		if err := os.Rename(m.path, m.path+".corrupt"); err != nil {
			return nil, nil, err
		}
		return m.baseFromMemoryLocked(), fmt.Errorf("owners: %s was corrupt; moved aside to %s.corrupt and recovered from memory", m.path, m.path), nil
	}
	if base == nil {
		base = map[string]entry{}
	}
	return base, nil, nil
}

// queueLocked wakes the writer. The caller has already recorded its change
// in m.d; the document itself is computed by the writer, which can only
// build it after reading the file under owners.lock (§4.7).
// Caller holds m.mu.
func (m *Map) queueLocked() error {
	if m.closed {
		return ErrClosed
	}
	m.writes.Add(1)
	// The send must be non-blocking, and this is required for LIVENESS,
	// not just coalescing: queueLocked runs with mu held and writePending
	// needs mu, so a blocking send would park the caller holding mu,
	// waiting for the only goroutine that could unblock it to first take
	// that same mu — a lock-order deadlock. Measured: a blocking send
	// deadlocks TestConcurrentRecordsAllPersistAcrossTheWriter every time
	// under 32 concurrent Records.
	select {
	case m.wake <- struct{}{}:
	default: // a wake is already queued; the writer will take the newest dirty set
	}
	return nil
}

// SetOnError installs a sink for asynchronous write failures. Nothing
// marshals on the caller's goroutine any more either — that moved to the
// writer along with the merge — so the only errors Record, Reassign and
// Forget can still return directly are ErrClosed (from queueLocked) and,
// for Record and Reassign only, their own argument-validation error
// (Record: an empty account; Reassign: an empty account or id). Forget
// validates nothing and can only return ErrClosed or nil. A failed write
// arrives here instead.
//
// onError is written and read under mu, so calling SetOnError is safe at
// any time, not just before the first write (that precondition belongs to
// SetWrite, whose own doc comment states it — do not remove mu's guard
// here as redundant on the strength of this one). The only consequence of
// installing it late is that any write failure before it runs is dropped
// (nil = drop).
//
// fn runs on the writer goroutine, not the caller's. It must not call
// Close: Close waits for the writer to finish, and the writer would be
// inside fn waiting for fn to return — a permanent deadlock (measured). It
// must not block for any other reason either: Close has no timeout, so a
// wedged fn stalls every future write and stalls shutdown itself.
func (m *Map) SetOnError(fn func(error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onError = fn
}

// Close stops the writer once the newest document has been written, and is
// safe to call more than once — including two overlapping calls: whichever
// loses the race still blocks until the writer has finished, rather than
// returning the instant it observes closed already true. A Close that
// returned early would let the process exit with a write still in flight.
//
// A Record/Reassign/Forget that arrives after Close still mutates the
// in-memory map — Lookup reflects it — but is never queued for a write:
// queueLocked returns ErrClosed instead of nil, so the caller sees the loss
// rather than a false success. Ordering Close against those callers'
// shutdown (so this window is never hit in practice) is the daemon's job,
// not this package's.
//
// Close can block for as long as the writer is parked waiting on
// fsutil.Lock(owners.lock), if another process (or, from Task 4, this
// process's own owners.Edit) is holding it — there is deliberately no
// timeout, because abandoning the wait would abandon the final write,
// which is the one thing Close exists to guarantee.
func (m *Map) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		<-m.done
		return
	}
	m.closed = true
	m.mu.Unlock()
	close(m.wake)
	<-m.done
}

// Reload adopts an external change to owners.json, and reports whether
// there was one.
//
// It takes no FILE lock — no owners.lock, no flock: writes go through
// temp-and-rename, so a reader sees one complete document or the other,
// never a torn one. (It does take m.mu, twice — true of every exported
// method except ZeroIDExtractions, which reads its counter atomically
// instead.) It is called from the daemon's existing 5-second roster tick
// (internal/cli/proxy.go's watchRoster) — no new goroutine, no new ticker,
// no watcher (§4.7).
//
// "External" means the file's mtime and size differ from lastWriteMtime and
// lastWriteSize, or it is no longer the same file as lastWriteFile — the
// last document this process has adopted, whether by writing it or by
// reloading it (see the field comment). Both the in-flight
// set (m.inflight, a mutation writePending has taken and is currently
// trying to write — see its own comment) and the dirty set (m.d, this
// process's own mutations not yet even attempted) are re-applied on top of
// the file, INFLIGHT FIRST, m.d LAST — the code below is
// mergeInto(mergeInto(base, m.inflight), m.d), never the reverse. That
// order is not cosmetic: inflight holds the OLDER operation (it was taken
// from m.d before anything now in m.d could have been recorded), and
// applying the older one first is what lets recorded's first-writer-wins
// rule (F19) favour it — a later m.d entry for the same key finds it
// already present in the intermediate result and is skipped — while
// applying m.d last is what lets a newer Reassign win under forced, which
// always overwrites unconditionally. Getting the order backwards makes
// Reload teach the opposite of first-writer-wins. Either way, neither a
// queued nor an in-flight local mutation is undone by adopting the file
// (F104).
//
// A sibling of that same F104 shape lives between the os.Stat above and the
// os.ReadFile below: Reload stats the file, reads it, and only APPLIES both
// in the final m.mu section, so if writePending runs a complete write in
// that window, the writer has already advanced m.m past the base this call
// read — applying this call's now-stale base on top (against dirty sets the
// completed write already emptied) would silently drop the just-written
// keys from m.m. So writeGen (see its own field comment — NOT
// lastWriteMtime/lastWriteSize, which can go silently inert here since they
// advance only if a post-write stat succeeds) is captured in the FIRST
// m.mu section alongside unchanged, and re-checked against the live
// counter in the SECOND one before the merge is applied: if it moved, the
// writer won the race, and this call abandons its merge and returns false,
// nil rather than undoing the write — the next tick re-stats against the
// writer's new baseline and adopts cleanly, the same self-healing path this
// design already relies on elsewhere. This is deliberately NOT fixed by
// re-reading the file under m.mu instead: that would put a file read on the
// hot-path mutex, the one thing this whole design exists to avoid (F66).
func (m *Map) Reload() (bool, error) {
	fi, err := os.Stat(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m.mu.Lock()
	baselineGen := m.writeGen
	unchanged := m.lastWriteFile != nil && os.SameFile(fi, m.lastWriteFile) &&
		fi.ModTime().Equal(m.lastWriteMtime) && fi.Size() == m.lastWriteSize
	m.mu.Unlock()
	if unchanged {
		return false, nil
	}

	b, err := os.ReadFile(m.path)
	if err != nil {
		return false, err
	}
	var base map[string]entry
	if json.Unmarshal(b, &base) != nil {
		// A corrupt file is quarantined under owners.lock — by the WRITE
		// path (readBaseLocked) and by Open (OpenMax's own re-read-under-
		// the-lock before it renames) — because both can do it safely.
		// Reload holds no file lock, so it reports the condition and
		// changes nothing rather than racing a rename against either one.
		return false, fmt.Errorf("owners: %s is corrupt", m.path)
	}

	if m.reloadTestHook != nil {
		m.reloadTestHook()
	}

	m.mu.Lock()
	if m.writeGen != baselineGen {
		// writePending completed a write between our os.Stat/os.ReadFile
		// above and this lock: m.m already reflects something newer than
		// `base`. Applying our merge now would roll it backwards. Abandon
		// it — see the doc comment above.
		m.mu.Unlock()
		return false, nil
	}
	// inflight goes on FIRST: it holds an operation that happened EARLIER
	// than anything now in m.d (it was already taken from m.d by the time
	// any of m.d's current contents could have been recorded), and each of
	// mergeInto's three rules needs the earlier operand on the correct
	// side to stay correct — recorded first-writer-wins needs the earlier
	// Record applied first, forced needs the later Reassign applied last,
	// and forgotten needs the later operation (a Forget, or a Record/
	// Reassign that re-created the key) to be the one that is re-checked
	// against base last (F104).
	// Deliberately skips evictMap (whole-branch review, Item 7), consistent
	// with Open, which skips it for the same reason: this Map's OWN writer
	// (writePending, above) always calls evictMap on the merged document
	// before marshaling, so a file THIS Map wrote is already at or under
	// max, and m.inflight/m.d only ever add this process's own small set of
	// pending mutations on top, not a bulk import.
	//
	// This is NOT true of every write this package performs, though (whole-
	// branch fix round 2, D3 — a previous version of this comment claimed
	// it was): Edit (edit.go:92) marshals tx.m directly and never calls
	// evictMap, and the file Reload adopts is often one Edit just wrote,
	// not this Map's own writer. That is still safe today only because
	// Edit has no `max` to evict against — it takes no *Map, by design
	// (its own doc comment: a short-lived process has no use for Map's
	// writer goroutine or dirty set) — and its one production caller
	// (internal/cli/own.go) first checks tx.Lookup(kind, id) succeeds and
	// only then calls tx.Reassign, which overwrites an EXISTING key rather
	// than adding one, so Edit cannot grow the map past whatever bound it
	// started under. A file that exceeds max some OTHER way (hand-edited,
	// or written by a future tool that does not evict, OR a future caller
	// of Edit that creates new keys instead of only reassigning existing
	// ones — at which point Edit would need its own max and evictMap call)
	// simply stays over max until this process's own next write trims it —
	// no different from what Open already accepts for the file it starts
	// from.
	m.m = mergeInto(mergeInto(base, m.inflight), m.d)
	m.lastWriteMtime, m.lastWriteSize, m.lastWriteFile = fi.ModTime(), fi.Size(), fi
	m.mu.Unlock()
	return true, nil
}

// NudgePending wakes the writer if m.d is non-empty — which covers both a
// mutation still waiting for its first attempt AND one a failed write
// folded back: writePending's failure path folds the delta back into m.d
// WITHOUT re-waking — see writeLoop's trailing-call comment for why
// re-waking on failure is wrong — so on an otherwise idle process either
// kind sits in memory until the next mutation or until Close. The roster
// tick calls this every 5s, which bounds the wait (§4.7).
//
// It is a no-op when there is nothing pending, so a tick on a healthy
// process costs one mutex acquisition and dirty.empty()'s three length
// checks. queueLocked's own m.writes.Add(1) still counts as an attempt to
// wake the writer when this DOES queue one, consistent with Writes' own doc
// comment (export_test.go) — this is not a separate counter.
func (m *Map) NudgePending() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.d.empty() {
		return
	}
	_ = m.queueLocked() // ErrClosed only, already excluded above
}

// writeFileAtomic is the real write seam, installed by OpenMax.
func writeFileAtomic(path string, b []byte) error {
	return fsutil.WriteFileAtomic(path, b, 0o600)
}
