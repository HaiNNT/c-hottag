package owners_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
)

var t0 = time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)

func TestRecordLookupPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"slug1"}, "C", t0); err != nil {
		t.Fatal(err)
	}
	m.Close() // the write is now asynchronous; drain before reopening below
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if a, ok := m2.Lookup(router.KindArtifact, "slug1"); !ok || a != "C" {
		t.Fatalf("Lookup after reopen = %q, %v", a, ok)
	}
	if _, ok := m2.Lookup(router.KindConnector, "slug1"); ok {
		t.Fatal("kinds are not separate")
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v err = %v", fi.Mode().Perm(), err)
	}
}

func TestSessionIDPrefixesShareAKey(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	m.Record(router.KindSession, []string{"cse_01ABC"}, "C", t0)
	for _, id := range []string{"cse_01ABC", "session_01ABC", "01ABC"} {
		if a, ok := m.Lookup(router.KindSession, id); !ok || a != "C" {
			t.Errorf("Lookup(session, %q) = %q, %v", id, a, ok)
		}
	}
	m.Record(router.KindEnvironment, []string{"cse_X"}, "C", t0)
	if _, ok := m.Lookup(router.KindEnvironment, "X"); ok {
		t.Error("prefix stripping applied to a non-session kind")
	}
}

// TestRecordSkipsNoOpsAndNeverOverwrites replaces a test that used to assert
// the opposite: that re-Record-ing a known id under a new account moved its
// owner. That was last-writer-wins, the exact bug F19 fixes (a session
// re-listing objects under a newly pinned remote account silently
// re-attributed one of them). Record must now leave a known owner alone;
// Reassign is the deliberate way to change it.
func TestRecordSkipsNoOpsAndNeverOverwrites(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	m.Record(router.KindArtifact, []string{"a", "b"}, "B", t0)
	w := owners.Writes(m)
	m.Record(router.KindArtifact, []string{"a", "b"}, "B", t0.Add(time.Hour))
	if owners.Writes(m) != w {
		t.Fatal("unchanged Record rewrote the file")
	}
	// F19: a different account Record-ing an already-owned id must not move
	// ownership, and must not write.
	m.Record(router.KindArtifact, []string{"a"}, "C", t0.Add(time.Hour))
	if a, _ := m.Lookup(router.KindArtifact, "a"); a != "B" {
		t.Fatalf("owner = %s, want B — Record must not overwrite a known owner (F19)", a)
	}
	if owners.Writes(m) != w {
		t.Fatal("Record on an already-owned id wrote the file")
	}
	if err := m.Reassign(router.KindArtifact, "a", "C", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a, _ := m.Lookup(router.KindArtifact, "a"); a != "C" {
		t.Fatalf("owner after Reassign = %s, want C", a)
	}
	if owners.Writes(m) != w+1 {
		t.Fatal("Reassign did not write")
	}
}

func TestRecordDoesNotOverwriteAKnownOwner(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	now := time.Unix(1789870000, 0)
	m.Record(router.KindConnector, []string{"mcpsrv_1"}, "A", now)
	m.Record(router.KindConnector, []string{"mcpsrv_1"}, "D", now.Add(time.Minute))
	if got, ok := m.Lookup(router.KindConnector, "mcpsrv_1"); !ok || got != "A" {
		t.Fatalf("owner = %q,%v; want A — the creating account keeps ownership (F19)", got, ok)
	}
}

func TestRecordStillLearnsAnUnknownObject(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	now := time.Unix(1789870000, 0)
	m.Record(router.KindConnector, []string{"mcpsrv_1"}, "A", now)
	m.Record(router.KindConnector, []string{"mcpsrv_2"}, "D", now)
	if got, _ := m.Lookup(router.KindConnector, "mcpsrv_2"); got != "D" {
		t.Fatalf("owner of a new id = %q, want D", got)
	}
}

func TestReassignChangesAKnownOwner(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	now := time.Unix(1789870000, 0)
	m.Record(router.KindConnector, []string{"mcpsrv_1"}, "A", now)
	m.Reassign(router.KindConnector, "mcpsrv_1", "D", now.Add(time.Minute))
	if got, _ := m.Lookup(router.KindConnector, "mcpsrv_1"); got != "D" {
		t.Fatalf("owner after Reassign = %q, want D", got)
	}
}

// TestReassignCreatesUnknownObject pins the ruling in Reassign's doc
// comment: reassigning an id the map has never seen creates it rather than
// erroring, since a repair/override caller shouldn't have to check whether
// the map already knows the id first.
func TestReassignCreatesUnknownObject(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	if err := m.Reassign(router.KindConnector, "mcpsrv_new", "D", t0); err != nil {
		t.Fatalf("Reassign on an unknown id returned an error: %v", err)
	}
	if got, ok := m.Lookup(router.KindConnector, "mcpsrv_new"); !ok || got != "D" {
		t.Fatalf("owner after Reassign on an unknown id = %q,%v; want D,true", got, ok)
	}
}

// TestReassignPersists mirrors TestRecordLookupPersists: the existing
// no-op-skip/write-count assertions exercise the in-memory writes counter,
// not the file on disk, so a Reassign that stopped calling save() (e.g.
// `m.writes++; return nil`) would still pass every other Reassign test.
// Only reopening the map from the path it was given can catch that.
func TestReassignPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindConnector, []string{"mcpsrv_1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Reassign(router.KindConnector, "mcpsrv_1", "D", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m.Close() // the write is now asynchronous; drain before reopening below
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if a, ok := m2.Lookup(router.KindConnector, "mcpsrv_1"); !ok || a != "D" {
		t.Fatalf("Lookup after reopen = %q, %v; want D, true — Reassign must persist like Record", a, ok)
	}
}

func TestReassignRejectsEmptyAccount(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	if err := m.Record(router.KindArtifact, []string{"z"}, "C", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Reassign(router.KindArtifact, "z", "", t0.Add(time.Minute)); err == nil {
		t.Fatal("Reassign with an empty account should error")
	}
	if a, _ := m.Lookup(router.KindArtifact, "z"); a != "C" {
		t.Fatalf("owner = %s, want C — a rejected Reassign must not have moved it", a)
	}
}

func TestReassignRejectsEmptyID(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	if err := m.Reassign(router.KindArtifact, "", "C", t0); err == nil {
		t.Fatal("Reassign with an empty id should error")
	}
	if _, ok := m.Lookup(router.KindArtifact, ""); ok {
		t.Fatal("Reassign with an empty id must not have written a junk entry keyed on the empty id")
	}
}

func TestReassignEvictsOldestBeyondMax(t *testing.T) {
	m, _ := owners.OpenMax(filepath.Join(t.TempDir(), "owners.json"), 2)
	defer m.Close()
	m.Record(router.KindArtifact, []string{"old"}, "C", t0)
	m.Record(router.KindArtifact, []string{"mid"}, "C", t0.Add(time.Minute))
	if err := m.Reassign(router.KindArtifact, "new", "C", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Lookup(router.KindArtifact, "old"); ok {
		t.Error("oldest entry not evicted after Reassign pushed the map over max")
	}
	for _, id := range []string{"mid", "new"} {
		if _, ok := m.Lookup(router.KindArtifact, id); !ok {
			t.Errorf("%s evicted", id)
		}
	}
}

func TestZeroIDExtractionsIsCounted(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	before := m.ZeroIDExtractions()
	m.Record(router.KindArtifact, nil, "A", time.Unix(1789870000, 0))
	if m.ZeroIDExtractions() != before+1 {
		t.Fatal("a recording route that yielded no ids must be counted, or the failure is silent")
	}
}

func TestEvictsOldestBeyondMax(t *testing.T) {
	m, _ := owners.OpenMax(filepath.Join(t.TempDir(), "owners.json"), 2)
	defer m.Close()
	m.Record(router.KindArtifact, []string{"old"}, "C", t0)
	m.Record(router.KindArtifact, []string{"mid"}, "C", t0.Add(time.Minute))
	m.Record(router.KindArtifact, []string{"new"}, "C", t0.Add(2*time.Minute))
	if _, ok := m.Lookup(router.KindArtifact, "old"); ok {
		t.Error("oldest entry not evicted")
	}
	for _, id := range []string{"mid", "new"} {
		if _, ok := m.Lookup(router.KindArtifact, id); !ok {
			t.Errorf("%s evicted", id)
		}
	}
}

func TestForget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, _ := owners.Open(p)
	m.Record(router.KindArtifact, []string{"x"}, "B", t0)
	m.Record(router.KindArtifact, []string{"y"}, "C", t0)
	if err := m.Forget("b"); err != nil {
		t.Fatal(err)
	}
	m.Close() // the write is now asynchronous; drain before reopening below
	m2, _ := owners.Open(p)
	defer m2.Close()
	if _, ok := m2.Lookup(router.KindArtifact, "x"); ok {
		t.Error("B's entry survived Forget")
	}
	if _, ok := m2.Lookup(router.KindArtifact, "y"); !ok {
		t.Error("C's entry lost")
	}
}

func TestNullFileOpensAndRecordWorks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(p, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Recovered {
		t.Fatal("a null file is valid JSON, not corrupt")
	}
	if err := m.Record(router.KindArtifact, []string{"z"}, "C", t0); err != nil {
		t.Fatal(err)
	}
	if a, ok := m.Lookup(router.KindArtifact, "z"); !ok || a != "C" {
		t.Fatalf("Lookup after Record on a null-opened map = %q, %v", a, ok)
	}
	if _, err := os.Stat(p + ".corrupt"); err == nil {
		t.Fatal("a null file must not be moved aside as corrupt")
	}
}

func TestForgetWithNoEntriesDoesNotWrite(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	w := owners.Writes(m)
	if err := m.Forget("nobody"); err != nil {
		t.Fatal(err)
	}
	if owners.Writes(m) != w {
		t.Fatal("Forget with no matching entries wrote the file")
	}
}

func TestRecordWithEmptyIDsDoesNotWrite(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	w := owners.Writes(m)
	if err := m.Record(router.KindArtifact, nil, "C", t0); err != nil {
		t.Fatal(err)
	}
	if owners.Writes(m) != w {
		t.Fatal("Record with no ids wrote the file")
	}
}

func TestRecordRejectsEmptyAccount(t *testing.T) {
	m, _ := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	defer m.Close()
	if err := m.Record(router.KindArtifact, []string{"z"}, "", t0); err == nil {
		t.Fatal("Record with an empty account should error")
	}
	if _, ok := m.Lookup(router.KindArtifact, "z"); ok {
		t.Fatal("Record with an empty account must not have written an entry")
	}
}

// TestUnlockHappensBeforeTheDiskWrite pins the F34 shape for the owner map
// deterministically: the marshal happens under mu, the write does not.
//
// An earlier version of this test measured wall-clock time (a Lookup racing
// 50 Records, asserting it never waited >500ms). That could never fail on
// this hardware: a local fsync is a few milliseconds, so even a mutation
// that put the unlock back after the write left every wait far under the
// threshold — the test always passed, guarding nothing. This version
// replaces the race with a controllable seam: the injected write blocks on
// an unbuffered channel until the test releases it, so a concurrent Lookup
// either returns immediately (unlock-before-write, the fix) or blocks for
// the test's own bounded wait (unlock-after-write, the defect), with no
// dependency on how fast the disk happens to be.
func TestUnlockHappensBeforeTheDiskWrite(t *testing.T) {
	cases := []struct {
		name    string
		trigger func(m *owners.Map) error
	}{
		{"Record", func(m *owners.Map) error {
			return m.Record(router.KindArtifact, []string{"id"}, "A", time.Now())
		}},
		{"Reassign", func(m *owners.Map) error {
			return m.Reassign(router.KindArtifact, "id", "A", time.Now())
		}},
		{"Forget", func(m *owners.Map) error {
			return m.Forget("A")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "owners.json")
			if tc.name == "Forget" {
				// Forget only writes if it actually deletes something; seed
				// an entry for it to drop, on a throwaway Map that is fully
				// drained (Close) before m below ever opens the same file.
				// Seeding through m itself would race: its write is now
				// asynchronous, so it could still be in flight — reading
				// m.write on the writer goroutine — exactly when SetWrite
				// below writes m.write with no synchronization of its own.
				seed, err := owners.Open(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := seed.Record(router.KindArtifact, []string{"id"}, "A", t0); err != nil {
					t.Fatal(err)
				}
				seed.Close()
			}
			m, err := owners.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()

			// inWrite is buffered so the injected write's send can never
			// itself wedge: without a reader ready at the exact instant it
			// sends, an unbuffered inWrite would leave the writer goroutine
			// parked forever on that send once the test below gives up
			// waiting on it — and the deferred m.Close() above would then
			// hang too, since the writer would never reach <-release to be
			// unblocked. release's close is deferred (idempotently, via
			// releaseOnce) for the same reason: on either of this
			// subtest's two bounded-wait failures below, the writer is
			// still parked inside the injected write at that point, and
			// without releasing it, m.Close() blocks forever, turning a
			// clean, diagnosable t.Fatal into a package-wide
			// "panic: test timed out" that also masks the other subtests
			// (measured under the brief's mutation 4).
			inWrite := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
			defer closeRelease()
			owners.SetWrite(m, func(path string, b []byte) error {
				inWrite <- struct{}{}
				<-release
				return nil
			})

			done := make(chan error, 1)
			go func() { done <- tc.trigger(m) }()

			// Wait (bounded) for the trigger to reach the write seam — it
			// must have released mu by the time it gets here, or the next
			// step would prove nothing.
			select {
			case <-inWrite:
			case <-time.After(2 * time.Second):
				t.Fatal("the write seam was never reached")
			}

			// A concurrent Lookup must not be blocked by the write in
			// flight. Bounded: if mu is still held across the write, this
			// times out rather than hanging the suite.
			lookupDone := make(chan struct{})
			go func() {
				m.Lookup(router.KindArtifact, "probe")
				close(lookupDone)
			}()
			select {
			case <-lookupDone:
			case <-time.After(1 * time.Second):
				t.Fatalf("%s: a Lookup blocked while its write was still in flight — mu is held across the write", tc.name)
			}

			closeRelease()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("trigger never returned after the write seam was released")
			}
		})
	}
}

// TestConcurrentRecordsAllPersist is the original durability check for F62:
// marshalling under mu but writing outside it lets concurrent Records' full-
// file renames race each other, and the loser's rename can silently discard
// a newer snapshot that already includes the winner's id — measured before
// the original fix at 11/32 and 24/32 entries lost across two runs. The
// single-writer-goroutine design gives the same guarantee a different way:
// every Record hands its change to one writer goroutine, serialised behind
// owners.lock, that reads the file, merges every accumulated change in, and
// writes — so no rename can race another. Every one of 32 concurrently
// recorded ids must still be present after a real fsync-backed write (no
// seam substituted) once the writer has drained. See
// TestConcurrentRecordsAllPersistAcrossTheWriter for the same property
// pinned against the writer goroutine directly.
func TestConcurrentRecordsAllPersist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- m.Record(router.KindArtifact, []string{fmt.Sprintf("concurrent-%d", i)}, "A", t0)
		}(i)
	}

	wgDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(10 * time.Second):
		t.Fatal("32 concurrent Records did not finish in time")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	m.Close() // the write is now asynchronous; drain before reopening below

	// Reopen from disk: only what actually landed in the file survives a
	// restart, which is the property under test — an in-memory Lookup on m
	// itself would pass even with the pre-fix race, since m.m never lost an
	// entry in memory, only the file did.
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	missing := 0
	for i := 0; i < n; i++ {
		if _, ok := m2.Lookup(router.KindArtifact, fmt.Sprintf("concurrent-%d", i)); !ok {
			missing++
		}
	}
	if missing != 0 {
		t.Fatalf("%d/%d concurrently recorded ids were lost on disk after a real fsync-backed write race", missing, n)
	}
}

// TestRecordDoesNotPropagateAWriteFailure pins the settled contract: the
// write no longer happens on the caller's goroutine, so Record can only
// ever return a MARSHAL error. A disk write failure must not surface here —
// see TestWriteFailuresReachOnError for where it does surface.
func TestRecordDoesNotPropagateAWriteFailure(t *testing.T) {
	m, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	owners.SetWrite(m, func(path string, b []byte) error { return errors.New("disk full") })
	if err := m.Record(router.KindArtifact, []string{"z"}, "C", t0); err != nil {
		t.Fatalf("Record error = %v, want nil — an async write failure must not reach the caller", err)
	}
}

// TestLookupDoesNotWaitForAWriteInFlight pins F66, the reason this task
// exists: with the old two-mutex design a second writer parked on writeMu
// while still holding mu, so every Lookup queued behind a whole fsync
// (measured at 2.0008s). A caller must never wait on a write.
func TestLookupDoesNotWaitForAWriteInFlight(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	release := make(chan struct{})
	var stalled sync.WaitGroup
	stalled.Add(1)
	var once sync.Once
	owners.SetWrite(m, func(path string, b []byte) error {
		once.Do(func() { stalled.Done(); <-release })
		return os.WriteFile(path, b, 0o600)
	})

	if err := m.Record(router.KindArtifact, []string{"a"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	stalled.Wait() // the writer goroutine is now parked inside the write

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Both a reader and a second writer must get through while the
		// first write is still in flight.
		m.Lookup(router.KindArtifact, "a")
		_ = m.Record(router.KindArtifact, []string{"b"}, "B", t0)
		m.Lookup(router.KindArtifact, "b")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Lookup/Record blocked behind a write in flight: the fsync is still on the response path (F66)")
	}
	close(release)
}

// TestCloseFlushesTheNewestDocument pins the half of the pattern that is
// easy to drop: the writer must perform one final pass after its wake
// channel closes, or the last queued document never reaches disk.
func TestCloseFlushesTheNewestDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"last"}, "Z", t0); err != nil {
		t.Fatal(err)
	}
	m.Close() // must not return until that record is on disk

	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if a, ok := m2.Lookup(router.KindArtifact, "last"); !ok || a != "Z" {
		t.Fatalf("after Close, Lookup = %q, %v; want Z, true: Close did not flush the newest document", a, ok)
	}
}

// TestRecordAfterCloseReturnsErrClosedAndIsNotPersisted pins F1: a
// Record/Reassign/Forget that arrives after Close still mutates the
// in-memory map (a caller checking only Lookup would not notice), but it is
// never queued for a write. Before this, queueLocked returned nil on the
// closed branch — a caller saw success for a write that would never reach
// disk, and Writes did not move either, so nothing could observe the loss.
func TestRecordAfterCloseReturnsErrClosedAndIsNotPersisted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"before"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	m.Close()

	if err := m.Record(router.KindArtifact, []string{"after"}, "A", t0); !errors.Is(err, owners.ErrClosed) {
		t.Fatalf("Record after Close returned %v, want ErrClosed", err)
	}

	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if _, ok := m2.Lookup(router.KindArtifact, "before"); !ok {
		t.Fatal("a record made before Close did not survive — should have persisted normally")
	}
	if _, ok := m2.Lookup(router.KindArtifact, "after"); ok {
		t.Fatal("a record made after Close was persisted despite returning ErrClosed")
	}
}

// TestCloseIsIdempotentAndConcurrentCallersWait pins that a second Close is
// safe AND that the loser of the race still blocks until the drain is done
// — not merely that the data survives, which the winner alone already
// guarantees regardless of what the losers do (the original shape of this
// test: 4 goroutines racing Close then wg.Wait(), which always waits for
// whichever happens to win; a loser's early return was invisible to it by
// construction, no iteration count could have caught it).
//
// Instead, every concurrent Close call — winner or loser, and the test does
// not need to know which is which — is timed against a write seam held
// open for a known duration. An early return (the mutation this pins:
// dropping <-m.done on the already-closed branch) shows up as a duration
// many orders of magnitude below holdFor; a correct call blocks for close
// to the full holdFor regardless of whether it happened to be the winner.
// A Close that returned early would let the process exit mid-write.
func TestCloseIsIdempotentAndConcurrentCallersWait(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}

	const holdFor = 400 * time.Millisecond
	release := make(chan struct{})
	var stalled sync.WaitGroup
	stalled.Add(1)
	var once sync.Once
	owners.SetWrite(m, func(path string, b []byte) error {
		once.Do(stalled.Done)
		<-release
		return os.WriteFile(path, b, 0o600)
	})

	if err := m.Record(router.KindArtifact, []string{"x"}, "X", t0); err != nil {
		t.Fatal(err)
	}
	stalled.Wait() // the writer goroutine is now parked inside the write

	go func() {
		time.Sleep(holdFor)
		close(release)
	}()

	const n = 4
	elapsed := make([]time.Duration, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			m.Close()
			elapsed[i] = time.Since(start)
		}(i)
	}
	wg.Wait()

	// A generous floor: comfortably below holdFor (so genuine scheduling
	// jitter never trips it) but many orders of magnitude above what an
	// early return takes (measured: ~375ns for the mutant vs ~holdFor for
	// the fix).
	const floor = holdFor / 4
	for i, d := range elapsed {
		if d < floor {
			t.Fatalf("Close() call %d returned after %v, want >= %v — a concurrent Close must block until the drain finishes, whether it wins or loses the race to close(wake)", i, d, floor)
		}
	}

	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if _, ok := m2.Lookup(router.KindArtifact, "x"); !ok {
		t.Fatal("a concurrent Close returned before the drain finished")
	}
}

// TestWriteFailuresReachOnError pins that asynchronous write failures are
// reported rather than silently dropped. The write no longer happens on the
// caller's goroutine, so the error cannot be returned to it.
func TestWriteFailuresReachOnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 4)
	m.SetOnError(func(err error) {
		select {
		case got <- err:
		default:
		}
	})
	owners.SetWrite(m, func(string, []byte) error {
		return errors.New("disk on fire")
	})
	if err := m.Record(router.KindArtifact, []string{"a"}, "A", t0); err != nil {
		t.Fatalf("Record returned %v; an async write failure must reach OnError, not the caller", err)
	}
	select {
	case err := <-got:
		if !strings.Contains(err.Error(), "disk on fire") {
			t.Fatalf("OnError got %v, want the underlying failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a failed write never reached OnError; it was silently dropped")
	}
	m.Close()
}

// TestConcurrentRecordsAllPersistAcrossTheWriter re-pins the durability
// property the two-mutex design existed to guarantee (F62): every accepted
// Record must survive, and no superseded snapshot may overwrite a newer one.
func TestConcurrentRecordsAllPersistAcrossTheWriter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("id-%02d", i)
			if err := m.Record(router.KindArtifact, []string{id}, "A", t0); err != nil {
				t.Errorf("Record %s: %v", id, err)
			}
		}(i)
	}
	wg.Wait()
	m.Close()

	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	var missing []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%02d", i)
		if _, ok := m2.Lookup(router.KindArtifact, id); !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("%d/%d records lost on disk: %v", len(missing), n, missing)
	}
}

func TestCorruptFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "owners.json")
	os.WriteFile(p, []byte("{broken"), 0o600)
	m, err := owners.Open(p)
	if err != nil || !m.Recovered {
		t.Fatalf("err = %v recovered = %v", err, m != nil && m.Recovered)
	}
	defer m.Close()
	if _, err := os.Stat(p + ".corrupt"); err != nil {
		t.Fatalf("corrupt copy missing: %v", err)
	}
	if err := m.Record(router.KindArtifact, []string{"z"}, "C", t0); err != nil {
		t.Fatal(err)
	}
}

// TestSetOnErrorRacesAgainstWritesUnderRace pins writePending's own doc
// comment: onError is captured under mu, together with the dirty set,
// because SetOnError can be called concurrently with a write. Production
// never actually does this — SetOnError is called exactly once at startup,
// before any mutation — so nothing else in this suite forces the writer
// goroutine's read of m.onError and a concurrent SetOnError write to
// genuinely overlap. This test manufactures that overlap directly: one
// goroutine calls SetOnError in a tight loop while the main goroutine
// keeps queuing writes (each Record wakes the writer goroutine, which
// reads m.onError on every pass), for long enough that go test -race has
// a real chance to observe the two accesses live at the same time.
//
// Against the current code (the read under mu) this passes clean, as it
// should — the guard works. It exists to catch a REGRESSION: moving the
// onError read in writePending outside mu (mutating writePending's `onError
// := m.onError` to run after `m.mu.Unlock()`) is caught by `go test
// ./internal/owners -race` under this test (verified: DATA RACE reported
// on this machine; reverted immediately after).
//
// 300 iterations, not 3000: measured on this machine, 300 still kills the
// same mutation 10/10 (`go test ./internal/owners -race -run
// TestSetOnErrorRacesAgainstWritesUnderRace -count=1`, run individually 10
// times, each reporting WARNING: DATA RACE naming this file's onError read
// in writePending). Kept at the smallest count actually measured to hold;
// raise it again if a future measurement stops killing reliably.
func TestSetOnErrorRacesAgainstWritesUnderRace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.SetOnError(func(error) {})
		}
	}()

	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("race-%d", i)
		if err := m.Record(router.KindArtifact, []string{id}, "A", t0); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestAnUnrelatedRecordDoesNotRevertAnotherProcessesReassignment is F96's
// regression test, replaying the reported sequence exactly.
//
// Two owners.Map instances on ONE path reproduce the bug faithfully: the
// defect is two independent in-memory maps racing a full-snapshot write,
// not two OS processes. TestTwoProcessesSerialiseOnOwnersLock below covers
// the genuinely cross-process part.
func TestAnUnrelatedRecordDoesNotRevertAnotherProcessesReassignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	now := time.Now()

	// The daemon: knows artifact:art1 belongs to A.
	daemon, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if err := daemon.Record(router.KindArtifact, []string{"art1"}, "A", now); err != nil {
		t.Fatal(err)
	}

	// A second process corrects it to B and exits.
	other, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Reassign(router.KindArtifact, "art1", "B", now); err != nil {
		t.Fatal(err)
	}
	other.Close()

	// The daemon now does something COMPLETELY unrelated — one ordinary
	// Record from live traffic. Before §4.7 this flushed its whole stale
	// map and reverted art1 to A, with no error anywhere.
	if err := daemon.Record(router.KindArtifact, []string{"art2"}, "A", now); err != nil {
		t.Fatal(err)
	}
	daemon.Close()

	got := readOwnersFile(t, path)
	if got["artifact:art1"] != "B" {
		t.Errorf("artifact:art1 owned by %q on disk, want B: the daemon's unrelated write reverted another process's correction (F96)", got["artifact:art1"])
	}
	if got["artifact:art2"] != "A" {
		t.Errorf("artifact:art2 owned by %q, want A: the daemon's own new record must still land", got["artifact:art2"])
	}
}

// readOwnersFile reads owners.json and returns key -> account.
func readOwnersFile(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var doc map[string]struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := map[string]string{}
	for k, v := range doc {
		out[k] = v.Account
	}
	return out
}

// A failed write must not lose the change. With snapshot writes a dropped
// document cost nothing, because the next mutation re-marshalled the whole
// map; with a delta it is permanent loss, so writePending folds the taken
// set back and retries on the next wake.
func TestAFailedWriteRetainsTheChangeAndLandsOnTheNextWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	now := time.Now()

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	var failNext atomic.Bool
	failNext.Store(true)
	written := make(chan struct{}, 8)
	owners.SetWrite(m, func(p string, b []byte) error {
		if failNext.Swap(false) {
			return errors.New("disk full")
		}
		err := os.WriteFile(p, b, 0o600)
		select {
		case written <- struct{}{}:
		default:
		}
		return err
	})

	reported := make(chan error, 4)
	m.SetOnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", now); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed write was never reported through SetOnError")
	}

	// A second, unrelated mutation triggers the retry. art1 must be in the
	// document it writes, even though nothing re-records it.
	if err := m.Record(router.KindArtifact, []string{"art2"}, "A", now); err != nil {
		t.Fatal(err)
	}
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("no successful write arrived after the retry")
	}
	m.Close()

	got := readOwnersFile(t, path)
	if got["artifact:art1"] != "A" {
		t.Errorf("artifact:art1 = %q on disk, want A: the change from the FAILED write was dropped instead of retried", got["artifact:art1"])
	}
}

// TestCloseFlushesAFoldedBackDeltaWithNoFurtherMutation pins the case
// TestAFailedWriteRetainsTheChangeAndLandsOnTheNextWrite does not cover: a
// failed write's fail() folds the taken dirty set back into m.d WITHOUT
// queuing a new wake (deliberately — see writeLoop's doc comment on its
// trailing writePending call), so if NO further mutation ever arrives, the
// only thing left that can still flush that folded-back change is
// writeLoop's trailing call after Close closes m.wake. Without that call,
// the change is silently lost even though it was never actually dropped
// from m.d — it just never gets written.
func TestCloseFlushesAFoldedBackDeltaWithNoFurtherMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	now := time.Now()

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	var failNext atomic.Bool
	failNext.Store(true)
	owners.SetWrite(m, func(p string, b []byte) error {
		if failNext.Swap(false) {
			return errors.New("disk full")
		}
		return os.WriteFile(p, b, 0o600)
	})

	reported := make(chan error, 4)
	m.SetOnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", now); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed write was never reported through SetOnError")
	}

	// No further mutation — go straight to Close. The folded-back change
	// must still reach disk before Close returns.
	m.Close()

	// Checked separately from readOwnersFile's read below: under the
	// mutation this test exists to catch (writeLoop's trailing
	// writePending deleted), the file never gets written at all, and
	// readOwnersFile's own os.ReadFile failure would report a generic
	// "reading owners.json" Fatalf instead of the tailored message below.
	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		t.Fatal("owners.json does not exist after Close with no further mutation: the folded-back change from the earlier failed write was never flushed")
	}

	got := readOwnersFile(t, path)
	if got["artifact:art1"] != "A" {
		t.Errorf("artifact:art1 = %q on disk after Close with no further mutation, want A: the folded-back change from the earlier failed write was never flushed", got["artifact:art1"])
	}
}

// TestCorruptFileAtWriteTimeKeepsThisProcessesOtherKnownEntries pins the
// hole in readBaseLocked's corrupt branch: the base it builds from this
// process's memory must be a COPY of everything m.m knows, not just the key
// currently being written. Seed two keys, let them land on disk, corrupt
// the file out from under the process, then mutate only one of the two keys
// — the OTHER one must still survive the write that follows, because it
// came from this process's memory, not from the corrupt file.
func TestCorruptFileAtWriteTimeKeepsThisProcessesOtherKnownEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"art2"}, "B", t0); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art1"] == "A" && got["artifact:art2"] == "B"
	})
	// The file showing both keys does not mean the writer is done. If art2
	// was recorded after the first write's take() but before its base was
	// read, that base (a copy of m.m, the file being absent) already holds
	// art2, and art2 is still dirty: a second write follows. If that write
	// reads its base before the corruption below and renames after it, it
	// replaces the corrupt file with a good one, and nothing is renamed aside.
	if !owners.WaitIdle(m) {
		t.Fatal("the seeding writes never finished")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Mutate only art2; art1 is untouched by this process from here on and
	// must still come from m's memory, not from the (now corrupt) file.
	if err := m.Reassign(router.KindArtifact, "art2", "C", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art2"] == "C"
	})

	got := readOwnersFile(t, path)
	if got["artifact:art1"] != "A" {
		t.Errorf("artifact:art1 = %q on disk after a corrupt-file recovery, want A: an entry this process knew but did not touch was dropped", got["artifact:art1"])
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("expected the corrupt file to be renamed aside: %v", err)
	}
}

// TestDeletedFileStillKeepsMemoryKnownKeys pins Item 1's file-absent door:
// readBaseLocked's os.ErrNotExist branch must build its base from this
// process's memory (baseFromMemoryLocked), exactly like the corrupt-file
// branch does, not from an empty map. Without that, deleting owners.json
// out from under a running process and then recording one unrelated new id
// would silently shrink the file down to just that one id — the map is
// gone from disk even though this process still knows the rest of it in
// memory.
func TestDeletedFileStillKeepsMemoryKnownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art1"] == "A"
	})

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// A completely unrelated Record — art1 is untouched — must not cause
	// the deleted file to come back without it.
	if err := m.Record(router.KindArtifact, []string{"art2"}, "B", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art2"] == "B"
	})

	got := readOwnersFile(t, path)
	if got["artifact:art1"] != "A" {
		t.Errorf("artifact:art1 = %q on disk after the file was deleted and recreated, want A: a key this process knew but did not touch was dropped", got["artifact:art1"])
	}
}

// TestCorruptFileThenFailedRetryStillKeepsMemoryKnownKeys pins the
// os.ErrNotExist door specifically as REACHED THROUGH THE CHAIN: corrupt
// file → renamed aside → the write that would recreate it FAILS → fail()
// folds the delta back → the next mutation retries → the file is now
// ABSENT (the corrupt-rename already happened; the retry that would
// recreate it is what just failed) → readBaseLocked's os.ErrNotExist
// branch. No other test reaches ErrNotExist by this route.
//
// It does NOT also pin the corrupt branch itself: this test's first write
// fails deliberately, so whatever (possibly wrong) base the corrupt branch
// computed is discarded before it can ever be persisted, and the retry
// that actually gets written goes entirely through the (here, correct)
// ErrNotExist door — verified by mutation: making the corrupt branch
// return an empty map leaves this test green. The corrupt door is pinned
// on its own by TestCorruptFileAtWriteTimeKeepsThisProcessesOtherKnownEntries,
// whose recovery write succeeds and is what actually gets persisted. Two
// doors, two tests; neither substitutes for the other.
func TestCorruptFileThenFailedRetryStillKeepsMemoryKnownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	// The seam is installed before the first Record below, honouring
	// SetWrite's own documented precondition ("must be called before the
	// write it means to affect is queued"): failNext starts false, so the
	// seeding writes succeed for real, and is only set true right before
	// the write meant to fail.
	var failNext atomic.Bool
	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	reported := make(chan error, 4)
	m.SetOnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	owners.SetWrite(m, func(p string, b []byte) error {
		if failNext.Swap(false) {
			return errors.New("disk full")
		}
		return os.WriteFile(p, b, 0o600)
	})

	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"art2"}, "B", t0); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art1"] == "A" && got["artifact:art2"] == "B"
	})
	// The file showing both keys does not mean the writer is done. If art2
	// was recorded after the first write's take() but before its base was
	// read, that base (a copy of m.m, the file being absent) already holds
	// art2, and art2 is still dirty: a second write follows. If that write
	// reads its base before the corruption below and renames after it, it
	// replaces the corrupt file with a good one, and nothing is renamed aside.
	if !owners.WaitIdle(m) {
		t.Fatal("the seeding writes never finished")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	failNext.Store(true)

	// This Reassign's write recovers the corrupt file (renaming it aside —
	// which readBaseLocked also reports through onError, see Item 7 of
	// fix round 1 — before failing outright): the file is now ABSENT, and
	// the retry has not happened yet. Wait specifically for the WRITE
	// failure rather than for the first thing SetOnError reports: the
	// corrupt-rename notice fires first, on the same call, and a bare
	// "something arrived" wait would proceed on that instead, without
	// ever actually confirming the write failed.
	if err := m.Reassign(router.KindArtifact, "art2", "C", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
waitForWriteFailure:
	for {
		select {
		case err := <-reported:
			if strings.Contains(err.Error(), "disk full") {
				break waitForWriteFailure
			}
			// Some other report (e.g. the corrupt-rename notice) — not
			// the signal this wait is for; keep waiting for the real one.
		case <-deadline:
			t.Fatal("the corrupt-recovery write's failure was never reported through SetOnError")
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("owners.json stat = %v, want ErrNotExist: the corrupt file should have been renamed aside and the recovery write should have failed, leaving no file at all", err)
	}

	// The retry: a second, unrelated mutation. It must not build its base
	// from an empty map just because the file happens to be absent right
	// now — art1 (never touched since before the corruption) must survive.
	if err := m.Record(router.KindArtifact, []string{"art3"}, "D", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art3"] == "D"
	})

	got := readOwnersFile(t, path)
	if got["artifact:art1"] != "A" {
		t.Errorf("artifact:art1 = %q on disk after corrupt-then-failed-write-then-retry, want A: a key this process knew but did not touch was dropped", got["artifact:art1"])
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("expected the corrupt file to be renamed aside: %v", err)
	}
}

// waitForWrite polls the file at path until want reports true or the bound
// expires. It exists because writePending runs on the writer goroutine
// asynchronously to the test; there is no signal available here other than
// the file itself changing.
func waitForWrite(t *testing.T, path string, want func(map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			var doc map[string]struct {
				Account string `json:"account"`
			}
			if json.Unmarshal(b, &doc) == nil {
				got := map[string]string{}
				for k, v := range doc {
					got[k] = v.Account
				}
				if want(got) {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("write never landed within the bound")
}

// TestTwoProcessesSerialiseOnOwnersLock proves the lock is a real
// cross-process flock, which the same-process tests above cannot: they
// would pass against a plain sync.Mutex.
//
// It asserts with TryLock rather than by timing a blocked write. A timing
// test ("the child had not finished after N ms") asserts that nothing
// happened by sampling state it never synchronised to — F64's class, and
// flaky by construction. TryLock returning ok=false while the child holds
// the lock is a positive, deterministic observation of exclusion.
func TestTwoProcessesSerialiseOnOwnersLock(t *testing.T) {
	dir := os.Getenv("OWNERS_LOCK_HELPER_DIR")
	if dir != "" {
		// Child: take owners.lock, announce it, wait for a line on stdin,
		// release by exiting.
		unlock, err := fsutil.Lock(filepath.Join(dir, "owners.lock"))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("held")
		bufio.NewScanner(os.Stdin).Scan()
		if err := unlock(); err != nil {
			t.Fatal(err)
		}
		return
	}

	dir = t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTwoProcessesSerialiseOnOwnersLock$")
	cmd.Env = append(os.Environ(), "OWNERS_LOCK_HELPER_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()

	sc := bufio.NewScanner(stdout)
	if !sc.Scan() || sc.Text() != "held" {
		t.Fatalf("child never reported holding the lock (got %q)", sc.Text())
	}

	// The child holds it: our TryLock must fail.
	if unlock, ok, err := fsutil.TryLock(filepath.Join(dir, "owners.lock")); err != nil {
		t.Fatal(err)
	} else if ok {
		unlock()
		t.Fatal("TryLock succeeded while another PROCESS held owners.lock; the lock is not excluding across processes")
	}

	// Let the child exit, then it must succeed.
	fmt.Fprintln(stdin, "release")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exited with %v", err)
	}
	unlock, ok, err := fsutil.TryLock(filepath.Join(dir, "owners.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("TryLock still fails after the holder exited; the lock was not released")
	}
	unlock()
}

// TestLookupReflectsTheMergeOutcomeAfterAWrite pins the post-write
// re-adoption in writePending: this process's own in-memory Record is
// necessarily OPTIMISTIC about first-writer-wins, because it can only judge
// "does this id already have an owner" against its own m.m, not against
// the file (only the merge, run by the writer under owners.lock, can do
// that). If the file already names a different owner for the id — written
// by another process before this Record's write reaches disk — the merge
// correctly keeps that owner, and m.m must be corrected to match once the
// write lands, or Lookup would keep reporting this process's wrong guess
// forever with no further write ever triggered to fix it (Record only
// records ids IT believes are unowned).
func TestLookupReflectsTheMergeOutcomeAfterAWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Simulate another process's write landing on disk between this
	// process's Open and its own write: m has no idea art1 is already
	// owned by OTHER.
	seed := map[string]struct {
		Account string    `json:"account"`
		At      time.Time `json:"at"`
	}{"artifact:art1": {Account: "OTHER", At: t0}}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// m optimistically believes it is the first writer for art1 — its own
	// m.m has no entry for it.
	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art1"] == "OTHER"
	})

	// The disk write and the in-memory re-adoption are two separate steps
	// on the writer goroutine (see writePending): the disk write landing
	// (observed above) does not itself guarantee the re-adoption has run
	// yet, so Lookup is polled rather than checked once immediately after.
	deadline := time.Now().Add(5 * time.Second)
	var a string
	var ok bool
	for time.Now().Before(deadline) {
		a, ok = m.Lookup(router.KindArtifact, "art1")
		if ok && a == "OTHER" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("Lookup(art1) = %q,%v, want OTHER,true: m.m was not corrected to match what the merge actually wrote to disk", a, ok)
}

// BenchmarkRecordUnderMu measures the hot path this task changed:
// queueLocked no longer marshals the whole map under mu. Whether that is
// faster is a question for this benchmark, not for a comment.
func BenchmarkRecordUnderMu(b *testing.B) {
	dir := b.TempDir()
	m, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()
	// Writes go nowhere: this measures the CALLER's time under mu, not the
	// writer goroutine's fsync.
	owners.SetWrite(m, func(string, []byte) error { return nil })

	now := time.Now()
	for i := 0; i < 5000; i++ {
		if err := m.Record(router.KindArtifact, []string{fmt.Sprintf("seed%d", i)}, "A", now); err != nil {
			b.Fatal(err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := m.Record(router.KindArtifact, []string{fmt.Sprintf("id%d", i)}, "A", now); err != nil {
			b.Fatal(err)
		}
	}
}

// A second process's correction must reach a LIVE Map's Lookup, not only
// the file. Without this the daemon keeps routing the object to the old
// account until it restarts, which defeats `chottag own` just as
// completely as the lost write did (§4.7).
func TestReloadAdoptsAnotherProcessesReassignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	now := time.Now()

	// Waits for the daemon's own write for "A" before `other` opens the
	// file. Without that, the write can still be in flight when Reload
	// runs, and Reload then abandons its merge by design (writeGen moved)
	// and reports no change. That is right for the product, whose next
	// tick adopts the file, but it is not what this test is about.
	daemon, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if err := daemon.Record(router.KindArtifact, []string{"art1"}, "A", now); err != nil {
		t.Fatal(err)
	}
	if !owners.WaitIdle(daemon) {
		t.Fatal("the daemon's write for A never finished")
	}

	other, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Reassign(router.KindArtifact, "art1", "B", now); err != nil {
		t.Fatal(err)
	}
	other.Close()

	if got, _ := daemon.Lookup(router.KindArtifact, "art1"); got != "A" {
		t.Fatalf("precondition: daemon's Lookup = %q before Reload, want the stale A", got)
	}

	changed, err := daemon.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Reload reported no change after another process rewrote the file")
	}
	if got, ok := daemon.Lookup(router.KindArtifact, "art1"); !ok || got != "B" {
		t.Errorf("daemon's Lookup = %q (found=%v) after Reload, want B", got, ok)
	}
}

// Linux stamps mtimes from a coarse clock tick, so another process's
// rewrite that lands in the same tick as this process's last write, at the
// same size (A and B are both one byte), leaves mtime and size exactly as
// Reload last saw them. Every write is temp-and-rename, so the file's
// identity changed even when those two did not, and Reload must see it
// (F237: CI caught the tick on ubuntu). Chtimes pins the equal mtime here
// instead of waiting for a clock to cooperate.
func TestReloadAdoptsASameSizeRewriteWithinOneMtimeTick(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	now := time.Now()

	daemon, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	if err := daemon.Record(router.KindArtifact, []string{"art1"}, "A", now); err != nil {
		t.Fatal(err)
	}
	if !owners.WaitIdle(daemon) {
		t.Fatal("the daemon's write for A never finished")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	other, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Reassign(router.KindArtifact, "art1", "B", now); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("precondition: size %d->%d, mtime %v->%v, want both unchanged", before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}

	changed, err := daemon.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Reload reported no change after a same-size rewrite in the same mtime tick")
	}
	if got, ok := daemon.Lookup(router.KindArtifact, "art1"); !ok || got != "B" {
		t.Errorf("daemon's Lookup = %q (found=%v) after Reload, want B", got, ok)
	}
}

// Reload must not undo a mutation this process has made but not yet
// written: those live in the dirty set and must survive being merged with
// the file.
func TestReloadKeepsThisProcessesUnwrittenChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	now := time.Now()

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Block writes so the change stays in the dirty set only. A write that
	// SUCCEEDS would not do this: writePending clears m.d on success (there
	// is nothing to fold back), leaving the mutation only in m.m — exactly
	// what this test must NOT exercise. An always-failing write keeps it in
	// m.d via writePending's fold-back path (fail(), owners.go).
	//
	// The test waits for onError before touching the file: writePending's
	// take() empties m.d BEFORE it attempts the write, so between take() and
	// fail()'s fold-back there is a real window where m.d is transiently
	// empty. Reload takes no lock and is not serialised against the writer
	// (by design — see Reload's doc comment), so calling it inside that
	// window is exactly as racy against the real writer as it would be
	// against a second process, and was observed to fail deterministically
	// without this wait.
	reported := make(chan error, 1)
	m.SetOnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	owners.SetWrite(m, func(string, []byte) error { return errors.New("blocked") })
	if err := m.Record(router.KindArtifact, []string{"mine"}, "A", now); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked write was never reported")
	}

	// Another process writes a file that knows nothing about "mine".
	if err := os.WriteFile(path, []byte(`{"artifact:theirs":{"account":"B","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Lookup(router.KindArtifact, "mine"); !ok || got != "A" {
		t.Errorf("Lookup(mine) = %q (found=%v) after Reload, want A: an unwritten local change was lost", got, ok)
	}
	if got, ok := m.Lookup(router.KindArtifact, "theirs"); !ok || got != "B" {
		t.Errorf("Lookup(theirs) = %q (found=%v) after Reload, want B", got, ok)
	}
}

// Reload must be a cheap no-op when the file is exactly what we last
// wrote: it runs on every 5s roster tick for the daemon's whole life.
func TestReloadIsANoOpWhenTheFileIsOurOwnLastWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Wait for lastWriteMtime itself, not the file's existence: writePending
	// sets it in its post-write m.mu block, which runs AFTER the file is
	// already visible on disk via os.Stat — polling the file alone can win
	// that race and call Reload before the bookkeeping it depends on has
	// happened (measured: 1/5 full -race runs, 5/5 with the interleaving
	// forced).
	var mtime time.Time
	for i := 0; i < 500; i++ {
		if mtime, _ = owners.LastWrite(m); !mtime.IsZero() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if mtime.IsZero() {
		t.Fatal("lastWriteMtime never became non-zero: the write never landed")
	}

	changed, err := m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("Reload reported a change for a file this process itself just wrote")
	}
}

// A transient write failure leaves the change in the dirty set with no wake
// queued (Task 2's deliberate choice). NudgePending is what bounds the wait
// on an idle process. Mutation: delete the queueLocked call from
// NudgePending, or the NudgePending call from ownersTick, must fail one of
// these two tests.
func TestNudgePendingFlushesAFoldedBackDeltaWithNoFurtherMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	var failNext atomic.Bool
	reported := make(chan error, 4)
	m.SetOnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})
	owners.SetWrite(m, func(p string, b []byte) error {
		if failNext.Swap(false) {
			return errors.New("disk full")
		}
		return os.WriteFile(p, b, 0o600)
	})

	failNext.Store(true)
	if err := m.Record(router.KindArtifact, []string{"art1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reported:
	case <-time.After(5 * time.Second):
		t.Fatal("the failed write was never reported")
	}

	// No further mutation, and no Close: the tick alone must flush it.
	m.NudgePending()
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:art1"] == "A"
	})
}

func TestNudgePendingIsANoOpWithNothingPending(t *testing.T) {
	dir := t.TempDir()
	m, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	before := owners.Writes(m)
	m.NudgePending()
	if got := owners.Writes(m); got != before {
		t.Errorf("Writes = %d after NudgePending on a clean map, want %d unchanged: a healthy tick must not wake the writer", got, before)
	}
}

// F104: Reload can transiently empty the dirty set out from under a
// mutation that is genuinely in flight, and if that write then fails, the
// silently-dropped mutation lets an unrelated LATER write win with a value
// that should have lost to it under first-writer-wins. This is not a
// stale-read bug: the wrong account reaches DISK, not just memory.
//
// Traced exactly (fix-brief round 1, Item 2): Record(A) sets m.m[k]=A and
// queues the write; the writer's take() empties m.d before it even opens
// the file; a tick's Reload lands in that window and merges base+EMPTY-d,
// dropping k from m.m; live traffic then calls Record(k, B), and because
// m.m no longer has k, first-writer-wins (which judges against m.m, not the
// file) wrongly records B; the original write then fails, and fail()'s
// foldUnder skips restoring A because m.d already holds B for that key
// (d.has(k) is true). The next write to actually land applies B against a
// base that still lacks k, so B — not A — reaches disk. Two individually
// correct mechanisms (take()-before-write, and a lock-free Reload) combine
// into an F19 violation neither one commits alone.
func TestReloadDuringAnInFlightWriteCanLoseTheFirstWriterOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	// Seed a file that EXISTS but has no "d1" entry: Reload's fs.ErrNotExist
	// early return would otherwise skip the merge this test is about.
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// The write seam blocks exactly once, on the FIRST call, so the test can
	// land Reload and the second Record precisely inside the take()-before-
	// write window; every later call (the retry) writes for real. `first`
	// is touched only from the single writer goroutine, never the test's,
	// so it needs no synchronization of its own.
	entered := make(chan struct{}, 1)
	release := make(chan error, 1)
	first := true
	owners.SetWrite(m, func(p string, b []byte) error {
		if first {
			first = false
			entered <- struct{}{}
			return <-release
		}
		return os.WriteFile(p, b, 0o600)
	})

	if err := m.Record(router.KindArtifact, []string{"d1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the blocked write")
	}

	// The write is parked inside m.write with m.d already emptied by
	// take(). A tick's Reload lands here.
	if _, err := m.Reload(); err != nil {
		t.Fatal(err)
	}

	// Live traffic: if Reload dropped "d1" from m.m, first-writer-wins
	// (judged against m.m) wrongly accepts B as though it were first.
	if err := m.Record(router.KindArtifact, []string{"d1"}, "B", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Now fail the original (A) write.
	release <- errors.New("simulated transient failure")

	// Close forces the final flush regardless of whether the buggy path's
	// erroneous Record(B) happened to queue an automatic retry: under the
	// fix, Record(B) is correctly rejected and queues nothing, so Close's
	// own trailing writePending is what lands the folded-back change.
	m.Close()

	got := readOwnersFile(t, path)
	if got["artifact:d1"] != "A" {
		t.Errorf("disk artifact:d1 = %q, want A: B reached disk ahead of the genuinely-first writer (F19) via the Reload/in-flight-write window", got["artifact:d1"])
	}
}

// The transient window's other half: Lookup during an in-flight write that
// eventually SUCCEEDS must still see this process's own not-yet-persisted
// mutation, not only after the write lands. Same take()-before-write window
// as the disk-loss test above, exercised through Reload+Lookup directly
// rather than through a second Record.
func TestReloadDuringAnInFlightSuccessfulWriteStillSeesTheChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	first := true
	owners.SetWrite(m, func(p string, b []byte) error {
		if first {
			first = false
			entered <- struct{}{}
			<-release
		}
		return os.WriteFile(p, b, 0o600)
	})

	if err := m.Record(router.KindArtifact, []string{"d1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the blocked write")
	}

	changed, err := m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Reload reported no change while the file lacks d1 and lastWriteMtime is still zero: it must not silently no-op")
	}
	if got, ok := m.Lookup(router.KindArtifact, "d1"); !ok || got != "A" {
		t.Errorf("Lookup(d1) = %q (found=%v) while the write is still in flight, want A: an in-flight mutation must stay visible", got, ok)
	}

	close(release)
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:d1"] == "A"
	})
}

// A stale m.inflight that a successful write forgot to clear must not
// resurrect an already-written value on a LATER Reload. Mutation: deleting
// the `m.inflight = newDirty()` line from writePending's success block
// (owners.go) compiles clean and every other test in this suite stays
// green — this is the only one that catches it, because it needs a SECOND
// external change to the same key, on a write that used the FORCED
// (Reassign) rule specifically: mergeInto's recorded rule only adds a key
// that is ABSENT from base, so a stale recorded entry is silently a no-op
// once base already carries the real value, but forced always overwrites
// unconditionally, which is exactly what makes a stale one dangerous.
func TestReloadDoesNotResurrectAWrittenReassignFromAStaleInflightSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Reassign(router.KindArtifact, "k1", "X", t0); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:k1"] == "X"
	})

	// A later, independent change to the SAME key.
	if err := os.WriteFile(path, []byte(`{"artifact:k1":{"account":"Y","at":"2026-09-22T01:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Lookup(router.KindArtifact, "k1"); !ok || got != "Y" {
		t.Errorf("Lookup(k1) = %q (found=%v) after Reload, want Y: a stale already-written in-flight entry resurrected the old value X instead of adopting the file", got, ok)
	}
}

// fix-brief round 2, Item 1: a stale m.inflight that fail() forgot to clear
// defeats mergeInto's forgotten rule, which deletes a key only if base
// STILL shows the forgotten account. mergeInto mutates its base argument in
// place, so in Reload's mergeInto(mergeInto(base, m.inflight), m.d) the
// second call sees whatever the FIRST call already wrote there — if the
// stale inflight re-inserted the key under a different account, the
// forgotten rule's EqualFold check fails and the delete m.d asked for never
// happens. Mutation: deleting fail()'s `m.inflight = newDirty()`
// (owners.go) must fail this.
func TestReloadDoesNotResurrectAForgottenKeyFromAStaleInflightAfterFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	entered := make(chan struct{}, 1)
	release := make(chan error, 1)
	first := true
	owners.SetWrite(m, func(p string, b []byte) error {
		if first {
			first = false
			entered <- struct{}{}
			return <-release
		}
		return os.WriteFile(p, b, 0o600)
	})
	// fail() calls onError AFTER releasing m.mu but BEFORE returning, so a
	// sink that blocks on <-proceed holds the writer out of writeLoop's
	// NEXT writePending (which would take() a fresh, empty m.d and
	// overwrite m.inflight on its own, closing the window this test needs
	// held open). The window exists without this synchronization; the sink
	// only makes landing inside it deterministic.
	reported := make(chan struct{})
	proceed := make(chan struct{})
	m.SetOnError(func(error) { close(reported); <-proceed })

	// The writer takes k1=B into m.inflight and parks on the blocked write.
	if err := m.Record(router.KindArtifact, []string{"k1"}, "B", t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the blocked write")
	}

	// An external writer gives k1 to A. mergeInto's recorded rule skips the
	// in-flight B because base already has the key, so m.m[k1] becomes A.
	if err := os.WriteFile(path, []byte(`{"artifact:k1":{"account":"A","at":"2026-09-22T01:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Lookup(router.KindArtifact, "k1"); !ok || got != "A" {
		t.Fatalf("setup: Lookup(k1) = %q (found=%v), want A", got, ok)
	}

	// Forget A: m.d now holds forgotten[k1]="A". m.inflight still holds the
	// ORIGINAL recorded[k1]="B" — a DIFFERENT account for the same key.
	if err := m.Forget("A"); err != nil {
		t.Fatal(err)
	}

	// The in-flight write fails. foldUnder skips k1 (m.d already has it).
	release <- errors.New("simulated transient failure")
	<-reported

	// The external writer drops k1 entirely. A Reload here must leave k1
	// gone: base lacks it, and the only local operation on it is a Forget.
	if err := os.WriteFile(path, []byte(`{"artifact:zzz":{"account":"Z","at":"2026-09-22T02:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Lookup(router.KindArtifact, "k1"); ok {
		t.Errorf("Lookup(k1) = %q (found), want absent: a stale in-flight recorded entry re-inserted the forgotten key under a different account, so m.d's forgotten rule no longer matched and the delete was lost", got)
	}

	close(proceed)
}

// The recorded-rule half of the same defect: a stale inflight puts the key
// back into base BEFORE m.d's own recorded entry is considered, so
// mergeInto's `if _, ok := base[k]; !ok` guard sees the key already present
// and skips m.d's newer value entirely. Mutation: deleting fail()'s
// `m.inflight = newDirty()` (owners.go) must fail this.
func TestReloadDoesNotLetAStaleInflightBeatANewerRecordAfterFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	entered := make(chan struct{}, 1)
	release := make(chan error, 1)
	first := true
	owners.SetWrite(m, func(p string, b []byte) error {
		if first {
			first = false
			entered <- struct{}{}
			return <-release
		}
		return os.WriteFile(p, b, 0o600)
	})
	reported := make(chan struct{})
	proceed := make(chan struct{})
	m.SetOnError(func(error) { close(reported); <-proceed })

	if err := m.Record(router.KindArtifact, []string{"k1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the blocked write")
	}

	// Forget A (removes k1 from m.m), then Record it to B: m.d ends up with
	// recorded[k1]=B while m.inflight still holds the ORIGINAL recorded[k1]=A.
	if err := m.Forget("A"); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"k1"}, "B", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	release <- errors.New("simulated transient failure")
	<-reported

	if err := os.WriteFile(path, []byte(`{"artifact:zzz":{"account":"Z","at":"2026-09-22T02:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.Lookup(router.KindArtifact, "k1"); !ok || got != "B" {
		t.Errorf("Lookup(k1) = %q (found=%v), want B: a stale in-flight recorded entry put the key back first, so mergeInto's recorded rule skipped m.d's newer value", got, ok)
	}

	close(proceed)
}

// Whole-branch review, Item 1: a sibling of F104 in Reload itself. Reload
// stats the file, reads it, and only APPLIES both under m.mu at the very
// end — so if writePending completes a whole write in the window between
// the read and that final lock, applying Reload's now-stale base on top
// would silently drop the just-written key from m.m and roll lastWrite*
// backwards. Reload must detect the move and abandon its merge instead.
//
// Uses owners.SetReloadTestHook to park Reload deterministically after it
// has read the file but before it re-locks — the exact window the bug
// lives in — then lands a completely independent write in that window
// before releasing it.
func TestReloadAbandonsItsMergeWhenAWriteLandsWhileItWasReading(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Establish a real lastWriteMtime/lastWriteSize baseline first: Open
	// alone never sets it (round 1's Item 1), and this test needs a real
	// baseline to roll backwards, not the zero value.
	if err := m.Record(router.KindArtifact, []string{"seed"}, "S", t0); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:seed"] == "S"
	})
	// The file lands before writePending's bookkeeping (its post-write stat
	// and writeGen++). Wait for that too, or the external rewrite below can
	// be adopted as "our own last write" and Reload returns before its hook.
	if !owners.WaitWriteGen(m, 0) {
		t.Fatal("the seed write's bookkeeping never finished")
	}

	// An external rewrite so Reload's "unchanged" check is false and it
	// proceeds all the way to the read below.
	if err := os.WriteFile(path, []byte(`{"artifact:seed":{"account":"S","at":"2026-09-22T00:00:00Z"},"artifact:ext":{"account":"E","at":"2026-09-22T00:30:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	proceed := make(chan struct{})
	owners.SetReloadTestHook(m, func() {
		close(entered)
		<-proceed
	})

	reloadDone := make(chan struct{})
	var changed bool
	var reloadErr error
	go func() {
		defer close(reloadDone)
		changed, reloadErr = m.Reload()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Reload never reached its test hook")
	}

	// A completely independent write lands and fully completes while
	// Reload is parked.
	genBeforeD1 := owners.WriteGen(m)
	if err := m.Record(router.KindArtifact, []string{"d1"}, "D", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	waitForWrite(t, path, func(got map[string]string) bool {
		return got["artifact:d1"] == "D"
	})
	// Likewise: release Reload only once the d1 write has bumped writeGen,
	// which is the signal Reload's abandon check reads.
	if !owners.WaitWriteGen(m, genBeforeD1) {
		t.Fatal("the d1 write's bookkeeping never finished")
	}

	close(proceed)
	<-reloadDone

	if reloadErr != nil {
		t.Fatal(reloadErr)
	}
	if changed {
		t.Error("Reload reported a change after abandoning its merge, want false: the write it raced against, not Reload, is what changed things")
	}
	if got, ok := m.Lookup(router.KindArtifact, "d1"); !ok || got != "D" {
		t.Errorf("Lookup(d1) = %q (found=%v) after the abandoned Reload, want D: the completed write must survive, not be rolled back by a stale merge", got, ok)
	}
}

// D2, whole-branch fix round 2: the guard the previous test pins keys off
// writeGen, not lastWriteMtime/lastWriteSize, precisely because those two
// only advance `if statErr == nil` (writePending) — so a write that
// completes but whose trailing os.Stat fails would otherwise leave the
// guard's baseline unmoved, indistinguishable from "no write happened",
// and reproduce the exact bug the previous test exists to catch. This test
// forces that stat to fail: the write seam below writes the real file and
// then removes it, so by the time writePending's own os.Stat(m.path) runs,
// the file is gone. m.m still gets updated (that assignment does not
// depend on the stat), and — the property under test — writeGen still
// bumps, so Reload's guard still fires even though lastWriteMtime/
// lastWriteSize never moved.
//
// The write's completion is awaited via m.Close(), not writeGen or
// waitForWrite: the former is the very counter this test exists to check
// (using it to detect completion would make the test's own wait fail
// silently under the D2 mutation instead of its intended assertions), and
// the latter polls the file's content on disk, which this test's write
// seam deletes.
func TestReloadCatchesItsOwnWriterEvenWhenTheWritesTrailingStatFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	owners.SetWrite(m, func(p string, b []byte) error {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			return err
		}
		// The write itself succeeds (this return is nil); removing the file
		// immediately afterward is what makes writePending's own trailing
		// os.Stat(m.path) fail, simulating a stat that cannot see a write
		// that genuinely landed.
		return os.Remove(p)
	})

	entered := make(chan struct{})
	proceed := make(chan struct{})
	owners.SetReloadTestHook(m, func() {
		close(entered)
		<-proceed
	})

	reloadDone := make(chan struct{})
	var changed bool
	var reloadErr error
	go func() {
		defer close(reloadDone)
		changed, reloadErr = m.Reload()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Reload never reached its test hook")
	}

	// A completely independent write lands while Reload is parked, and its
	// trailing stat fails by construction (the write seam above). Close
	// waits for the writer to fully finish processing it — including the
	// writeGen bump, which does not depend on the stat's outcome — without
	// relying on the file existing afterward or on writeGen itself as the
	// completion signal.
	if err := m.Record(router.KindArtifact, []string{"d1"}, "D", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	m.Close()

	close(proceed)
	<-reloadDone

	if reloadErr != nil {
		t.Fatal(reloadErr)
	}
	if changed {
		t.Error("Reload reported a change after abandoning its merge, want false: the write it raced against, not Reload, is what changed things")
	}
	if got, ok := m.Lookup(router.KindArtifact, "d1"); !ok || got != "D" {
		t.Errorf("Lookup(d1) = %q (found=%v) after the abandoned Reload, want D: writeGen must catch the race even when the write's trailing stat failed, or Reload silently rolls the write back", got, ok)
	}
}

// Whole-branch review, Item 3: Reload's merge order (inflight first, m.d
// last — see Reload's own doc comment) is the branch's longest concurrency
// argument, and until now nothing exercised it: the round-2 "...AfterFail"
// tests both run AFTER fail() has resolved the write, where m.inflight is
// already empty and cannot make the order observable. This one catches a
// Forget arriving while an earlier Record is genuinely still in flight —
// neither resolved by success nor by fail() — which the round-2 tests
// cannot reach.
//
// Applying inflight (the Record, OLDER) before m.d (the Forget, NEWER) lets
// the Forget correctly win: inflight's recorded rule adds k1, then m.d's
// forgotten rule sees it and removes it. Reversing the order (m.d before
// inflight) makes the Forget's forgotten rule run first against a base that
// still lacks k1 — a no-op, since there is nothing there yet to delete —
// and then inflight's recorded rule adds k1 back with nothing left to
// remove it, resurrecting an account that was explicitly forgotten.
func TestReloadOrdersInflightBeforeDWhenAForgetArrivesWhileARecordIsStillInFlight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:other":{"account":"Z","at":"2026-09-22T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	entered := make(chan struct{}, 1)
	release := make(chan error, 1)
	first := true
	owners.SetWrite(m, func(p string, b []byte) error {
		if first {
			first = false
			entered <- struct{}{}
			return <-release
		}
		return os.WriteFile(p, b, 0o600)
	})

	// The writer takes k1=X into m.inflight and parks on the blocked write.
	if err := m.Record(router.KindArtifact, []string{"k1"}, "X", t0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never reached the blocked write")
	}

	// While that write is STILL in flight (neither succeeded nor failed),
	// forget the account that owns k1. m.d now holds forgotten[k1]="X";
	// m.inflight still holds the LIVE recorded[k1]="X".
	if err := m.Forget("X"); err != nil {
		t.Fatal(err)
	}

	changed, err := m.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("Reload reported no change while the file lacks k1 and lastWriteMtime is still zero: it must not silently no-op")
	}
	if got, ok := m.Lookup(router.KindArtifact, "k1"); ok {
		t.Errorf("Lookup(k1) = %q (found), want absent: applying inflight's Record before m.d's later Forget is what lets the Forget win — reversing the merge order would resurrect it instead", got)
	}

	release <- errors.New("simulated transient failure")
}

// D1 (whole-branch fix round 2): Open's corrupt-file path must adopt a
// repair another process made while it waited for owners.lock, WITHOUT
// inheriting ghost entries the first (failed) parse left behind.
// encoding/json partially populates its destination map on a type error —
// it keeps decoding and reports only the first error at the end — so
// unmarshalling `{"artifact:ghost":{"account":"G",...},"artifact:bad":
// {"account":123}}` into m.m leaves m.m["artifact:ghost"] = {Account:"G"}
// even though the call returns an error. Unmarshalling the re-read
// (repaired) document into that SAME map would merge with those ghosts
// instead of replacing them, and Lookup would then return an owner that
// was never in any file.
//
// Uses a REAL fsutil.Lock, not a hook that fakes the race: the test holds
// owners.lock itself, so Open's own fsutil.Lock call blocks deterministically
// once it reaches it. owners.SetOpenCorruptTestHook confirms Open has
// already done its FIRST (failed) read — the one that must see the corrupt
// content, not the repair — before the test writes the repair and releases
// the lock.
func TestOpenAdoptsARepairWithoutInheritingGhostsFromTheFailedParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	lockPath := filepath.Join(dir, "owners.lock")
	if err := os.WriteFile(path, []byte(`{"artifact:ghost":{"account":"G","at":"2026-09-22T00:00:00Z"},"artifact:bad":{"account":123}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	owners.SetOpenCorruptTestHook(func() { close(entered) })
	defer owners.SetOpenCorruptTestHook(nil)

	unlock, err := fsutil.Lock(lockPath)
	if err != nil {
		t.Fatal(err)
	}

	openDone := make(chan struct{})
	var m *owners.Map
	var openErr error
	go func() {
		defer close(openDone)
		m, openErr = owners.Open(path)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Open never reached its corrupt-path test hook")
	}

	// Open is now confirmed past its first (failed) parse of the corrupt
	// content, and about to block on owners.lock, which this test still
	// holds. Repair the file — simulating another process's own write —
	// before releasing it.
	if err := os.WriteFile(path, []byte(`{"artifact:real":{"account":"R","at":"2026-09-22T02:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	<-openDone
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer m.Close()

	if m.Recovered {
		t.Error("m.Recovered = true; the repaired file must not be treated as still corrupt")
	}
	if _, err := os.Stat(path + ".corrupt"); !os.IsNotExist(err) {
		t.Errorf("%s.corrupt exists (err=%v); a file another process repaired must not be quarantined", path, err)
	}
	if got, ok := m.Lookup(router.KindArtifact, "ghost"); ok {
		t.Errorf("Lookup(ghost) = %q (found), want absent: a ghost entry from the FAILED first parse survived into the adopted repair", got)
	}
	if got, ok := m.Lookup(router.KindArtifact, "bad"); ok {
		t.Errorf("Lookup(bad) = %q (found), want absent", got)
	}
	if got, ok := m.Lookup(router.KindArtifact, "real"); !ok || got != "R" {
		t.Errorf("Lookup(real) = %q (found=%v), want R: the repaired document must be adopted", got, ok)
	}
}
