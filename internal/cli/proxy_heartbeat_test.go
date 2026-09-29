package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

// awaitDaemonSnapshot is a fix-round-2 helper (item 1): since serve's clean
// shutdown now calls sink.clearDaemon() before sink.Close() (so `chottag
// own`/`chottag status --json` stop reporting a live daemon the instant the
// process exits, not up to DaemonStaleAfter later), a test can no longer
// read the daemon object's Heartbeat/Port/counters AFTER a full shutdown —
// that read would see the clean-shutdown clear, not the roster tick's
// stamp. This drains status.File snapshots off a channel fed by a
// sink.write override (see its callers) until one satisfies match, so the
// caller inspects the tick's own write, not a later one that superseded it.
// It cannot simply take the FIRST post-tick write either: the writer
// goroutine may already be mid-flight on an OLDER write when the tick's
// queueLocked() runs, in which case the first write to complete after that
// point is still the stale one and a second, newer write follows it.
func awaitDaemonSnapshot(t *testing.T, snapshots <-chan status.File, match func(status.Daemon) bool) status.File {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-snapshots:
			if f.Daemon != nil && match(*f.Daemon) {
				return f
			}
		case <-deadline:
			t.Fatal("no write matching the expected daemon snapshot arrived before the deadline")
			return status.File{}
		}
	}
}

// daemonSnapshotWriter returns a statusSink.write replacement that performs
// the real write, THEN parses the bytes and pushes the resulting
// status.File onto a channel awaitDaemonSnapshot can drain. The real write
// happens BEFORE the send, deliberately: a snapshot that arrives on the
// channel is therefore already on disk — when that write succeeded. A
// failed write still sends (its error goes back to the sink either way),
// because what this seam exists to observe is the document the sink
// produced, not whether the filesystem accepted it.
func daemonSnapshotWriter(t *testing.T) (func(path string, b []byte) error, <-chan status.File) {
	t.Helper()
	snapshots := make(chan status.File, 16)
	return func(path string, b []byte) error {
		err := status.WriteBytes(path, b)
		var f status.File
		if uerr := json.Unmarshal(b, &f); uerr == nil {
			select {
			case snapshots <- f:
			default:
			}
		}
		return err
	}, snapshots
}

// The roster tick is the only periodic path that writes status.json (the
// wake detector runs its own ticker too, internal/proxy/wake.go, but never
// touches the cache), so the daemon object rides it rather than getting a
// goroutine or ticker of its own (F94) — but
// the stamp is its own write, not a free ride on one the tick already makes.
//
// F96 gave watchRoster its own startup stamp, fired once before its select
// loop, before this test's explicit tick is ever sent — so that startup
// stamp, not this tick, is what "always writes despite the rate-limiting"
// now (all three counters are already correct by the time it fires, since
// this test drives them before runDaemon is even launched; only the listen
// port is not resolved yet at that point, which is why the tick below is
// sent far enough in the future to force a genuine re-stamp rather than
// relying on it changing anything watchRoster already knows). This test
// reads the tick's OWN write off the sink.write seam, and its matcher
// requires Port != 0 for exactly that reason: the startup stamp fires
// before listenTCP resolves a port and so writes port 0, which is what
// tells the two apart. Reading the final persisted document after a full
// shutdown — what this test did before the daemon object gained a
// clean-shutdown clear — could not distinguish them at all.
//
// Every one of the three counters is driven to a distinct, exact, non-zero
// value before the tick — not merely made non-zero — because F94 Item 2
// found that replacing all three stamped counters with literal zeroes left
// the whole suite green: an assertion of merely != 0 cannot catch a mutation
// that stamps the wrong counter in the wrong slot, or a stale value.
func TestRosterTickStampsTheDaemonObject(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "A")})
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	// RouteDrift = 2: a real *proxy.Server, built and driven through
	// proxytest exactly the way internal/proxy's own
	// TestSafetyNetRouteDriftCountsConcurrentSwaps does, then handed to
	// runDaemon via daemonDeps.Srv (a test-only seam — see its own doc
	// comment) rather than built fresh from an empty Cfg. Each of the two
	// requests is refused by the fake upstream and fixedServingChooser's
	// Refresh always declines, so each one falls straight through to the
	// original-login resend and increments drift exactly once, without an
	// extra retry round trip.
	driftUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
	})
	ph := proxytest.Start(t, driftUpstream, proxytest.Options{Choose: fixedServingChooser{account: "A", token: "tok-A"}})
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-HOME-SECRET")
		resp, err := ph.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if got := ph.Server.RouteDrift(); got != 2 {
		t.Fatalf("test setup: RouteDrift = %d, want 2 before the roster tick even runs", got)
	}

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	// ZeroIDExtractions = 3: three Record calls with no ids, the same path
	// a recording route with an id-extraction failure takes in production.
	for i := 0; i < 3; i++ {
		if err := own.Record(router.KindArtifact, nil, "A", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got := own.ZeroIDExtractions(); got != 3 {
		t.Fatalf("test setup: ZeroIDExtractions = %d, want 3", got)
	}
	// OwnerWriteDrops = 4: close this SAME map — nothing else writes
	// through it after this point in the test, so closing it early is
	// safe — then four Record calls with DISTINCT ids each return
	// owners.ErrClosed and get counted. Distinct ids matter: Record
	// no-ops (no error, no count) for an id already present in its
	// in-memory map even when closed, so reusing one id would only ever
	// count the first drop (see TestChooserCountsOwnerWriteDropsAfterClose
	// for the single-drop case this generalizes).
	own.Close()
	ch := &chooser{own: own}
	for i, id := range []string{"drop-1", "drop-2", "drop-3", "drop-4"} {
		ch.Record(router.KindArtifact, []string{id}, "A")
		if got := ch.OwnerWriteDrops(); got != uint64(i+1) {
			t.Fatalf("test setup: OwnerWriteDrops after drop %d = %d, want %d", i+1, got, i+1)
		}
	}

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, snapshots := daemonSnapshotWriter(t)
	sink.write = writer
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()

	tick := make(chan time.Time)
	processed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Cache:           cache,
			Sink:            sink,
			Tokens:          tm,
			Owners:          own,
			Chooser:         ch,
			Srv:             ph.Server,
			RosterTick:      tick,
			RosterProcessed: processed,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	// Sent daemonHeartbeatInterval+ past now, not just time.Now(): the F96
	// startup stamp already fired (before this goroutine ever got here) with
	// the listen port still unresolved, and by now every counter it stamped
	// matches what this tick would see too, so a bare time.Now() tick would
	// hit the `!changed && !heartbeatDue` skip and never re-stamp the
	// now-resolved port at all. Forcing heartbeatDue guarantees a real
	// re-stamp regardless of that race's exact timing.
	select {
	case tick <- time.Now().Add(daemonHeartbeatInterval + time.Second):
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never consumed the tick")
	}
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never finished processing the tick")
	}

	// Read the tick's own write off the sink.write seam (see
	// awaitDaemonSnapshot's doc comment), not from disk after a full
	// shutdown: since fix round 2 item 1, a clean shutdown clears the
	// daemon object's heartbeat, so reading after shutdown would see that
	// clear instead of this tick's stamp.
	f := awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool {
		return d.Port != 0 && d.RouteDrift == 2 && d.ZeroIDExtractions == 3 && d.OwnerWriteDrops == 4
	})
	if f.Daemon.Heartbeat.IsZero() {
		t.Error("daemon object has no heartbeat")
	}

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// TestWatchRosterStampsAHeartbeatBeforeItsFirstTick pins F96's actual fix
// directly: watchRoster stamps the daemon object once, before it ever
// enters its select loop, so `chottag own`'s liveness guard
// (status.File.DaemonRunningAt, read from cache/status.json) sees a live
// daemon within microseconds of the daemon starting rather than only after
// the real 5s roster ticker's first tick — the ~5s window F96 exists to
// narrow. No tick is ever sent on this test's RosterTick channel: if the
// startup stamp were removed, this test would time out waiting for a write
// that nothing else here would ever produce.
func TestWatchRosterStampsAHeartbeatBeforeItsFirstTick(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "A")})
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, snapshots := daemonSnapshotWriter(t)
	sink.write = writer
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()

	// Never sent on: the whole point is that the daemon object exists
	// before the roster ticker ever fires.
	tick := make(chan time.Time)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:     io.Discard,
			Stderr:     stderr,
			Listen:     "127.0.0.1:0",
			Home:       dir,
			Cache:      cache,
			Sink:       sink,
			Tokens:     tm,
			Owners:     own,
			Chooser:    &chooser{own: own},
			RosterTick: tick,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	// Read the startup stamp's own write off the sink.write seam (see
	// awaitDaemonSnapshot's doc comment), not from disk after a full
	// shutdown: since fix round 2 item 1, a clean shutdown clears the
	// daemon object's heartbeat, so reading after shutdown would see that
	// clear instead of this startup stamp.
	f := awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool {
		return !d.Heartbeat.IsZero()
	})
	now := time.Now()
	f.DaemonRunningAt(now)
	if !f.Daemon.Running {
		t.Errorf("daemon reads as not running from a heartbeat only %s old", now.Sub(f.Daemon.Heartbeat))
	}

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt
// pins F94's rate-limiting itself, which fix round 1 added but never
// pinned: the reviewer measured that disabling the skip (forcing the stamp
// closure's `if !changed && !heartbeatDue { return }` to never return) left
// the whole suite green, meaning a daemon that regressed to fsyncing
// status.json on every 5s tick — the exact defect this round exists to
// remove — would pass unnoticed (F46: correct is not guarded).
//
// It drives three ticks with explicit, test-chosen timestamps (watchRoster
// passes the TICK's own time to stamp, not a fresh timeNow() — see
// watchRoster's own doc comment) rather than sleeping out a real 30s
// window, and counts exactly how many times a write reaches
// statusSink.write, the fsync-level seam, not merely how many times
// setDaemon was called.
//
// F96 gave watchRoster its own startup stamp, fired once before its select
// loop and before any of this test's three ticks — with lastStampAt still
// zero at that point, IT is the call that "always writes" now, not tick 1,
// so it is drained explicitly first rather than left for tick 1's own
// awaitWrite to absorb by accident (that would pass whether or not tick 1
// itself ever wrote anything, the same unsynchronized-sampling trap F64
// names):
//  1. the startup stamp: nothing stamped yet, so it always writes —
//     write #1, drained before any tick is sent.
//  2. tick 1, ~0s after the startup stamp, unchanged counters: well inside
//     daemonHeartbeatInterval (30s) of the startup stamp's own timestamp.
//     Must NOT produce a write.
//  3. tick 2, +1s, same (idle, unchanged) counters: still well inside
//     daemonHeartbeatInterval. Must NOT produce a write either.
//  4. tick 3, +31s from tick 1: daemonHeartbeatInterval has now elapsed
//     since the startup stamp. Must produce a second write — otherwise the
//     rate limit has silently become "never writes again", a different bug
//     from the one it's supposed to fix.
func TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "A")})
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// snapshots decodes every call that reaches the fsync-level seam —
	// status.go's own comment on statusSink.write: "so a test can
	// substitute its own to observe writes that actually happen" — via the
	// same daemonSnapshotWriter/awaitDaemonSnapshot pair
	// TestRunDaemonReadsAsRunningFromCurrentHeartbeat uses above, not a
	// raw count (see the startup drain below for why: F227).
	writer, snapshots := daemonSnapshotWriter(t)
	sink.write = writer
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()

	tick := make(chan time.Time)
	processed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Cache:           cache,
			Sink:            sink,
			Tokens:          tm,
			Owners:          own,
			Chooser:         &chooser{own: own},
			RosterTick:      tick,
			RosterProcessed: processed,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	sendTick := func(at time.Time) {
		t.Helper()
		select {
		case tick <- at:
		case <-time.After(2 * time.Second):
			t.Fatal("watchRoster never consumed the tick")
		}
		select {
		case <-processed:
		case <-time.After(2 * time.Second):
			t.Fatal("watchRoster never finished processing the tick")
		}
	}
	awaitWrite := func(tag string) {
		t.Helper()
		select {
		case <-snapshots:
		case <-time.After(2 * time.Second):
			t.Fatalf("no write reached sink.write for %s", tag)
		}
	}
	noWrite := func(msg string) {
		t.Helper()
		select {
		case <-snapshots:
			t.Fatal(msg)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// F96's startup stamp can legitimately reach sink.write as ONE OR TWO
	// physical writes, not always exactly one (F227): the startup stamp
	// (watchRoster's tickStamp) runs ensureRoster — which queues its own
	// write at once, unconditionally, the moment the roster changes (its
	// own doc comment) — immediately followed by the counter-gated stamp
	// closure below, on the SAME goroutine, with nothing forcing the async
	// writer to coalesce the two queued documents into a single fsync
	// (queueLocked's own doc comment: it only coalesces whatever happens
	// to still be pending when the writer next wakes, not a guarantee
	// about any specific pair of calls). Draining by CONTENT — the daemon
	// object's own heartbeat, which ensureRoster's write never carries,
	// since ensureRoster does not touch status.Daemon at all — rather than
	// by a raw count of writes absorbs either outcome deterministically:
	// whichever physical write carries the heartbeat is always the LAST
	// one of the pair (stamp always runs after ensureRoster within one
	// tickStamp call), so waiting for it drains any earlier, uncoalesced
	// ensureRoster-only write too. A plain `writes <- struct{}{}` counter
	// here (this test's own shape before F227) left a stray, undrained
	// count sitting in the channel whenever the two writes landed
	// separately, which the very next check below then misread as tick
	// 1's own write — measured to fail 6/8 fresh-process runs (see F227's
	// own report for the full measurement).
	awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool {
		return !d.Heartbeat.IsZero()
	})

	base := time.Now()

	sendTick(base)
	noWrite("tick 1 wrote even though nothing changed since the startup stamp and daemonHeartbeatInterval cannot yet have elapsed")

	sendTick(base.Add(time.Second))
	noWrite("a write reached sink.write for an unchanged tick well inside daemonHeartbeatInterval — the F94 rate-limit is unpinned")

	sendTick(base.Add(daemonHeartbeatInterval + time.Second))
	awaitWrite("the tick past daemonHeartbeatInterval")

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// TestRosterTickWritesAgainWhenACounterChangesWellInsideTheHeartbeatWindow
// is the mirror of the test above: that one pins the HEARTBEAT half of the
// rate limit (an unchanged tick skips, then writes once
// daemonHeartbeatInterval has passed); this one pins the CHANGE half, which
// fix round 2 left unpinned (fix round 3, R1). The reviewer measured that
// replacing the counter comparison with `changed := lastStampAt.IsZero()`
// (i.e. deleting the three `!=` clauses) left the whole suite green — a
// pure-30s-heartbeat build would delay a real route-drift, zero-id or
// owner-drop event's visibility by up to 30s instead of at most one
// 5-second tick, which defeats the milestone's actual point.
func TestRosterTickWritesAgainWhenACounterChangesWellInsideTheHeartbeatWindow(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "A")})
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// See the doc comment on the identical setup in
	// TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt
	// for why this decodes writes by content rather than counting them
	// (F227).
	writer, snapshots := daemonSnapshotWriter(t)
	sink.write = writer
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()

	tick := make(chan time.Time)
	processed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Cache:           cache,
			Sink:            sink,
			Tokens:          tm,
			Owners:          own,
			Chooser:         &chooser{own: own},
			RosterTick:      tick,
			RosterProcessed: processed,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	sendTick := func(at time.Time) {
		t.Helper()
		select {
		case tick <- at:
		case <-time.After(2 * time.Second):
			t.Fatal("watchRoster never consumed the tick")
		}
		select {
		case <-processed:
		case <-time.After(2 * time.Second):
			t.Fatal("watchRoster never finished processing the tick")
		}
	}
	awaitWrite := func(tag string) {
		t.Helper()
		select {
		case <-snapshots:
		case <-time.After(2 * time.Second):
			t.Fatalf("no write reached sink.write for %s", tag)
		}
	}
	noWrite := func(msg string) {
		t.Helper()
		select {
		case <-snapshots:
			t.Fatal(msg)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// F96's startup stamp: drain it by content (the daemon object's own
	// heartbeat), not by a raw write count, so that whichever of one or
	// two physical writes it actually produces gets fully absorbed here
	// rather than leaving a stray write for the very next check to
	// misread as tick 1's own (F227; same reasoning as
	// TestRosterTickSkipsAnUnchangedTickWithinTheHeartbeatWindowThenWritesAgainAfterIt's
	// own comment).
	awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool {
		return !d.Heartbeat.IsZero()
	})

	base := time.Now()
	sendTick(base)
	noWrite("tick 1 wrote even though nothing changed since the startup stamp and daemonHeartbeatInterval cannot yet have elapsed")

	// A real zero-id extraction after tick 1 — the same path a recording
	// route with an id-extraction failure takes in production — changes
	// ZeroIDExtractions before the second tick, 1s later: still well inside
	// daemonHeartbeatInterval (30s). If this write did not happen, the
	// event would sit invisible for up to 30s.
	if err := own.Record(router.KindArtifact, nil, "A", base); err != nil {
		t.Fatal(err)
	}
	sendTick(base.Add(time.Second))
	awaitWrite("the tick after a counter changed, still inside the heartbeat window")

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}

// TestRosterTickStampsEvenWhenCacheStateFails pins fix-round-1 Item 1 rule
// 4, which fix round 2 implemented but never tested (fix round 3, R2):
// moving the stamp call back below `known = cur` — after cache.State() —
// is invisible to the suite otherwise. Liveness must not depend on the
// roster read succeeding, or a transient state.json error makes a live
// daemon read as down.
func TestRosterTickStampsEvenWhenCacheStateFails(t *testing.T) {
	dir := t.TempDir()
	// A corrupt state.json: store.Store.Load returns an error for it (it
	// cannot even json.Unmarshal), so cache.State() fails on the one and
	// only call this test's single tick makes.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(store.Store{Dir: dir})

	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()

	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, snapshots := daemonSnapshotWriter(t)
	sink.write = writer
	tm := tokens.New(tokens.Config{})
	stderr := newSyncBuf()

	tick := make(chan time.Time)
	processed := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout:          io.Discard,
			Stderr:          stderr,
			Listen:          "127.0.0.1:0",
			Home:            dir,
			Cache:           cache,
			Sink:            sink,
			Tokens:          tm,
			Owners:          own,
			Chooser:         &chooser{own: own},
			RosterTick:      tick,
			RosterProcessed: processed,
		})
	}()

	select {
	case <-stderr.done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started listening")
	}

	select {
	case tick <- time.Now():
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never consumed the tick")
	}
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("watchRoster never finished processing the tick")
	}

	// Confirm the setup actually exercised the failure path this test
	// exists to cover, via watchRoster's own onError print — processed's
	// receive above happens-after that print (same goroutine, program
	// order), so this read races nothing.
	if !strings.Contains(stderr.String(), "roster watch") {
		t.Fatalf("test setup: cache.State() did not fail on this tick; stderr = %q", stderr.String())
	}

	// Read a write off the sink.write seam (see awaitDaemonSnapshot's doc
	// comment), not from disk after a full shutdown: since fix round 2 item
	// 1, a clean shutdown clears the daemon object's heartbeat, so reading
	// after shutdown would see that clear instead of what the F96 startup
	// stamp (or this tick, if anything about it actually changed) wrote.
	// This mirrors the ORIGINAL assertion here (merely "some heartbeat"),
	// not a stronger one: this tick's own stamp() call may itself be a
	// no-op if nothing changed and the heartbeat is not yet due (F94's own
	// rate limit, unrelated to what this test pins), so the write actually
	// observed here may be the startup stamp's.
	awaitDaemonSnapshot(t, snapshots, func(d status.Daemon) bool {
		return !d.Heartbeat.IsZero()
	})

	cancel()
	select {
	case <-daemonDone:
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return")
	}
}
