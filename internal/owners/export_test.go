package owners

import "time"

// Writes exposes the write counter to tests. It counts ATTEMPTS TO WAKE THE
// WRITER: queueLocked's m.writes.Add(1) is unconditional and runs before
// its own non-blocking send, so a call that hits the send's `default` arm
// (a wake is already pending) still increments this counter without
// actually queuing anything. It is not a count of completed fsyncs, and
// not a count of documents: the document is only computed later, by the
// writer, from whatever the file holds at that point. What coalesces a
// burst of mutations behind one write is the DIRTY SET (m.d) accumulating
// them — not the writer draining a backlog of wakes, since the wake
// channel holds at most one token and any surplus is dropped at the
// sender, inside queueLocked, before the writer ever sees it. A test must
// not read this counter as a count of fsyncs or of documents built.
func Writes(m *Map) int { return int(m.writes.Load()) }

// SetWrite overrides the write seam so a test can control (or block on) the
// disk write deterministically, instead of racing a real fsync against a
// wall-clock timeout. Must be called before the write it means to affect is
// queued — see the field doc on Map.write.
func SetWrite(m *Map, w func(path string, b []byte) error) { m.write = w }

// LastWrite exposes lastWriteMtime/lastWriteSize so a test can wait for the
// state Reload actually depends on, instead of polling the file's existence
// on disk: writePending's post-write m.mu block that sets these fields runs
// AFTER the file is visible via os.Stat, so a test polling the file alone
// can observe a write that has landed but whose bookkeeping has not (the
// window measured for TestReloadIsANoOpWhenTheFileIsOurOwnLastWrite: 1/5
// full -race runs, 5/5 with the interleaving forced).
func LastWrite(m *Map) (time.Time, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastWriteMtime, m.lastWriteSize
}

// WriteGen exposes writeGen, which writePending bumps in its post-write
// m.mu block, AFTER the file is already visible on disk. A test that must
// know a write's bookkeeping is done (not only that its bytes landed) waits
// on this rather than on the file.
func WriteGen(m *Map) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.writeGen
}

// WaitWriteGen waits until writeGen exceeds after, polling like the test
// helpers do; the 5s bound only turns a hang into a failure.
func WaitWriteGen(m *Map, after uint64) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if WriteGen(m) > after {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// WaitIdle waits until the writer has nothing left to do: no dirty set
// waiting for a write (m.d) and none being written (m.inflight). The file
// showing a key is not enough to know that: a write whose file-absent or
// corrupt base is a COPY of m.m can put on disk a key that a later Record
// added after the write's take(), and that key is still in m.d, so another
// write is still coming. Once idle, nothing is written again until the test
// mutates the Map. The 5s bound only turns a hang into a failure.
func WaitIdle(m *Map) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		idle := m.d.empty() && m.inflight.empty()
		m.mu.Unlock()
		if idle {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// SetReloadTestHook installs a function Reload calls after it has read and
// parsed the file but before it re-takes m.mu to apply the merge — the
// exact window in which a concurrent writePending can complete and move
// the baseline out from under it (whole-branch review, Item 1). A test can
// block in the hook to land a write deterministically inside that window;
// nil (the default, and the only state any production Map is ever in) costs
// Reload one extra nil check.
func SetReloadTestHook(m *Map, fn func()) { m.reloadTestHook = fn }

// SetOpenCorruptTestHook installs a function OpenMax's corrupt-file path
// calls right after its first (failed) unmarshal, before it acquires
// owners.lock — the window in which another process can repair the file
// (D1, whole-branch fix round 2). Package-level, not per-Map: OpenMax has
// not returned a *Map yet at this point in its own execution. A test using
// it must reset it (e.g. via defer) when done, since it persists across
// this package's otherwise-sequential tests.
func SetOpenCorruptTestHook(fn func()) { openCorruptTestHook = fn }
