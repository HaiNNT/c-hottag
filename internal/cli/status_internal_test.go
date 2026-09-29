package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/status"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
)

func knownWindow() usagehdr.Window {
	return usagehdr.Window{Known: true, HasUtilization: true, Utilization: 0.1, Status: "allowed"}
}

// TestStatusSinkObserveIsRaceSafeUnderConcurrentUse pins contract 1:
// status.File has no internal mutex by design, and OnUsage is invoked
// concurrently, once per response, each on its own request goroutine. A
// reviewer confirmed a real data race on eight concurrent Observe calls
// against an unguarded File; run with -race, this fails the same way if the
// sink's mutex is ever dropped or narrowed to not cover Save.
func TestStatusSinkObserveIsRaceSafeUnderConcurrentUse(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Drain before the test returns: t.TempDir()'s cleanup races the
	// writer goroutine's atomic write (temp file + rename into
	// <home>/cache/) otherwise, since flush/Close now only QUEUE a write —
	// they no longer complete it inline the way Save used to.
	defer sink.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Two distinct account names, so goroutines both create new
			// rows (File.account appending to the slice) and mutate a row
			// another goroutine already created concurrently.
			name := fmt.Sprintf("acct-%d", n%2)
			sink.observe(name, usagehdr.Snapshot{Known: true, At: time.Now(), FiveHour: knownWindow(), SevenDay: knownWindow()}, usagehdr.Verdict{})
		}(i)
	}
	wg.Wait()
}

// TestStatusSinkObserveClearsAPriorPassthroughMark pins contract 3: OnUsage
// only fires for a response actually answered on the account's own
// credential, so observing usage for an account is positive proof it is no
// longer passing through and must clear a stale mark — otherwise a user who
// fixed a stale token keeps seeing "passthrough" after chottag has in fact
// swapped back onto them.
func TestStatusSinkObserveClearsAPriorPassthroughMark(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close() // safety net: an early Fatal below must not skip the drain (idempotent alongside the explicit Close below)
	sink.setPassthrough("D", "token stale")
	// Checked in-memory, not via disk: the write is now queued
	// asynchronously (F34), so a round trip through disk immediately after
	// would race the writer goroutine. sink.mu guards c.file the same way
	// it guards every other read of it. The final state below still goes
	// through disk, via Close, which is what proves the write itself works.
	sink.mu.Lock()
	got := sink.passthroughLocked("D")
	sink.mu.Unlock()
	if got != "token stale" {
		t.Fatalf("passthrough = %q, want D marked passthrough", got)
	}

	sink.observe("D", usagehdr.Snapshot{Known: true, At: time.Now(), FiveHour: knownWindow(), SevenDay: knownWindow()}, usagehdr.Verdict{})
	sink.Close()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if f.Accounts[0].Passthrough != "" {
		t.Fatalf("Passthrough = %q after a real swap succeeded, want cleared (F20: a fixed account must stop reading as passthrough)", f.Accounts[0].Passthrough)
	}
}

// TestStatusSinkEnsureAccountsSeedsTheFullConfiguredSetBeforeObserving pins
// contract 2: EnsureAccounts must run against the configured account set
// before anything is observed, or Limits.AllLimited is computed over only
// the accounts the cache happens to have seen — with three full-quota
// accounts never touched and one limited, that reads as "all accounts
// limited" and `chottag next` would exit 3 while quota is available.
func TestStatusSinkEnsureAccountsSeedsTheFullConfiguredSetBeforeObserving(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close() // safety net: idempotent alongside the explicit Close below
	sink.ensureAccounts([]string{"A", "B", "C", "D"})

	until := time.Now().Add(time.Hour)
	sink.observe("D", usagehdr.Snapshot{Known: true, At: time.Now(), Overall: "rejected",
		FiveHour: usagehdr.Window{Known: true},
		SevenDay: usagehdr.Window{Known: true, HasUtilization: true, Utilization: 1, Status: "rejected", ResetsAt: until},
	}, usagehdr.Verdict{Limited: true, Until: until, Window: "seven_day"})
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 4 {
		t.Fatalf("accounts = %+v, want all 4 configured accounts seeded", f.Accounts)
	}
	if f.Limits.AllLimited {
		t.Fatal("AllLimited = true with 3 of 4 configured accounts never observed (full quota); EnsureAccounts must have seeded them so the roll-up covers the whole configured set")
	}
}

// newWriteWatcher wires sink.write to signal a channel every time a write
// actually happens — what contract 4 is about, rather than an internal
// queuing decision (that field was removed: it duplicated state the write
// seam already lets a test observe directly). It returns two helpers: waitFor
// blocks until the next write lands, or fails after a generous timeout;
// assertNone waits a short interval and fails if a write arrives during it
// (safe as a non-flaky negative check here, since a coalesced/deferred
// write is skipped altogether, not merely delayed — nothing is ever going
// to arrive for it). Must be called before anything queues a write.
func newWriteWatcher(t *testing.T, sink *statusSink) (waitFor func(), assertNone func()) {
	t.Helper()
	writes := make(chan struct{}, 8)
	sink.write = func(path string, b []byte) error {
		err := status.WriteBytes(path, b)
		writes <- struct{}{}
		return err
	}
	waitFor = func() {
		t.Helper()
		select {
		case <-writes:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a write")
		}
	}
	assertNone = func() {
		t.Helper()
		select {
		case <-writes:
			t.Fatal("a write happened that should have been coalesced/deferred")
		case <-time.After(50 * time.Millisecond):
		}
	}
	return waitFor, assertNone
}

// TestStatusSinkCoalescesSaves pins contract 4: Observe runs once per
// response, but a write must not happen for every one — at most once per
// interval, and flush forces the last write through regardless, so a
// shutdown never loses the most recent observation to the coalescing
// window.
func TestStatusSinkCoalescesSaves(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()        // safety net: idempotent alongside the explicit Close below
	sink.interval = time.Hour // wide open, so only flush can force a second write through
	waitFor, assertNone := newWriteWatcher(t, sink)

	sink.observe("A", usagehdr.Snapshot{Known: true, At: time.Now(), FiveHour: knownWindow(), SevenDay: knownWindow()}, usagehdr.Verdict{})
	waitFor() // the first observation should have written immediately

	sink.observe("B", usagehdr.Snapshot{Known: true, At: time.Now(), FiveHour: knownWindow(), SevenDay: knownWindow()}, usagehdr.Verdict{})
	assertNone() // a second Observe inside the coalescing window must not write

	sink.flush()
	waitFor() // flush must force the coalesced write through on shutdown

	sink.Close()
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want both A and B's observations to have reached disk", f.Accounts)
	}
}

func floatPtr(f float64) *float64 { return &f }

// TestRenderStatusNeverPrintsZeroForUnknown pins contract 5 by calling
// renderStatus directly (it already takes an injectable clock) against
// rows that exercise exactly the three ways "unknown" can be mistaken for a
// concrete value:
//
//   - A: a FRESH observation whose 5h window reported no utilization at
//     all (FiveHourPct nil) — must read "unknown", never "0%": that would
//     tell a user "0% of 5h used" while they are sitting at the wall.
//   - B: a STALE observation (older than status.StaleAfter) whose numbers
//     are known but must not be trusted — must read "unknown" for BOTH
//     windows, not the real percentages and not a blank column.
//   - C: a limited account with a zero LimitedUntil (unknown clearing
//     time) — must say so in words, never format the zero time into a
//     date that reads as "already expired".
func TestRenderStatusNeverPrintsZeroForUnknown(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	f := status.File{
		Serving: "A", Remote: "A",
		Accounts: []status.Account{
			{
				Name: "A",
				Usage: &status.Usage{
					FiveHourPct: nil, // window known, but no utilization reported
					SevenDayPct: floatPtr(50),
					UpdatedAt:   now, // fresh
				},
			},
			{
				Name: "B",
				Usage: &status.Usage{
					FiveHourPct: floatPtr(90),
					SevenDayPct: floatPtr(10),
					UpdatedAt:   now.Add(-2 * status.StaleAfter), // stale
				},
			},
			{
				Name:         "C",
				Limited:      true,
				LimitedUntil: time.Time{}, // unknown, not "already expired"
			},
		},
	}

	var buf bytes.Buffer
	renderStatus(&buf, f, now)
	out := buf.String()

	for _, field := range strings.Fields(out) {
		if field == "0%" {
			t.Fatalf("output = %q; a window with no reported utilization must render unknown, never 0%%", out)
		}
	}
	if strings.Contains(out, "Jan 1 00:00") || strings.Contains(out, "0001") {
		t.Fatalf("output = %q; a zero LimitedUntil must never be formatted into a date (reads as already expired)", out)
	}
	if !strings.Contains(out, "50%") {
		t.Fatalf("output = %q; A's known 7d utilization of 50%% must still render", out)
	}
	if !strings.Contains(out, "reset time unknown") {
		t.Fatalf("output = %q; C's zero LimitedUntil must say the reset time is unknown, in words", out)
	}

	var bLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "B") {
			bLine = line
		}
	}
	if bLine == "" {
		t.Fatalf("no row for B found in output: %q", out)
	}
	if strings.Contains(bLine, "90%") || strings.Contains(bLine, "10%") {
		t.Fatalf("B row = %q; a stale observation must render unknown for BOTH windows even though the numbers are known", bLine)
	}
	if strings.Count(bLine, "unknown") != 2 {
		t.Fatalf("B row = %q, want \"unknown\" under both 5h and 7d, not one column blank", bLine)
	}
}

// TestRenderStatusTreatsAnElapsedLimitAsUnconfirmedNotADateGoneBy pins the
// renderStatus half of fix 2: M1c has no poll, so nothing but fresh traffic
// on the account itself can ever clear a limit. Printing a LimitedUntil that
// has already passed as a plain date tells the user the account is "limited
// until <yesterday>", which reads as confirmed-still-limited forever — the
// user has no reason to ever send it traffic again, so the trap never
// breaks. An elapsed LimitedUntil must render distinctly from both a live
// future date and the separate "reset time unknown" (zero LimitedUntil)
// case.
func TestRenderStatusTreatsAnElapsedLimitAsUnconfirmedNotADateGoneBy(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	f := status.File{
		Accounts: []status.Account{
			{Name: "E", Limited: true, LimitedUntil: now.Add(-time.Hour)}, // elapsed
			{Name: "F", Limited: true, LimitedUntil: now.Add(time.Hour)},  // still live
		},
	}
	var buf bytes.Buffer
	renderStatus(&buf, f, now)
	out := buf.String()

	var eLine, fLine string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "E"):
			eLine = line
		case strings.HasPrefix(trimmed, "F"):
			fLine = line
		}
	}
	if eLine == "" || fLine == "" {
		t.Fatalf("missing row(s) for E and/or F: %q", out)
	}
	if strings.Contains(eLine, now.Add(-time.Hour).Local().Format("Jan 2 15:04")) {
		t.Fatalf("E row = %q; an elapsed LimitedUntil must not be formatted into a date", eLine)
	}
	if strings.Contains(eLine, "reset time unknown") {
		t.Fatalf("E row = %q; an elapsed (known but stale) LimitedUntil must read distinctly from the zero (truly unknown) case", eLine)
	}
	want := now.Add(time.Hour).Local().Format("Jan 2 15:04")
	if !strings.Contains(fLine, want) {
		t.Fatalf("F row = %q, want it to contain %q; a LimitedUntil still in the future must still render as a date", fLine, want)
	}
}

// TestStatusSinkFlushesImmediatelyWhenAVerdictFlipsToLimited pins the part
// of contract 4 that isn't "coalesce": a limit learned in the second before
// the process is killed must survive the restart (spec §6.3), or chottag
// routes straight back to the account that just refused it. observe must
// bypass the coalescing window the instant an account's Limited flips from
// false to true.
func TestStatusSinkFlushesImmediatelyWhenAVerdictFlipsToLimited(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()        // safety net: idempotent alongside the explicit Close below
	sink.interval = time.Hour // wide open: only an immediate flush can write inside this test
	waitFor, _ := newWriteWatcher(t, sink)

	// A first, unremarkable observation: its own maybeSave saves immediately
	// only because lastSave starts zero. This sets lastSave to "now", so the
	// real test below (a second observation, well inside the interval) can
	// only write if the limit-flip bypasses the coalescing window — not
	// because it happens to be the sink's very first write.
	sink.observe("A", usagehdr.Snapshot{Known: true, At: time.Now(), FiveHour: knownWindow(), SevenDay: knownWindow()}, usagehdr.Verdict{})
	waitFor()

	until := time.Now().Add(time.Hour)
	sink.observe("D", usagehdr.Snapshot{Known: true, At: time.Now(), Overall: "rejected",
		FiveHour: usagehdr.Window{Known: true, HasUtilization: true, Utilization: 1, Status: "rejected", ResetsAt: until},
	}, usagehdr.Verdict{Limited: true, Until: until, Window: "five_hour"})
	// The write this asserts must come from observe's own immediate-flush
	// branch, not from Close (which is called only afterward, below, and
	// would queue+write the current state regardless of whether observe
	// ever bypassed the coalescing window).
	waitFor()
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 2 {
		t.Fatalf("accounts = %+v; a limit that just started must reach disk immediately, not wait for the coalescing window", f.Accounts)
	}
	var d *status.Account
	for i := range f.Accounts {
		if f.Accounts[i].Name == "D" {
			d = &f.Accounts[i]
		}
	}
	if d == nil || !d.Limited {
		t.Fatalf("accounts = %+v, want D limited on disk immediately", f.Accounts)
	}
}

// TestStatusSinkFlushesImmediatelyWhenThePassthroughMarkChanges is
// setPassthrough's half of the same contract-4 guarantee: F20 must survive
// a crash between the mark changing and the next coalesced write, or the
// user believes chottag switched when the cache on disk still shows
// whatever it last observed.
func TestStatusSinkFlushesImmediatelyWhenThePassthroughMarkChanges(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()        // safety net: idempotent alongside the explicit Close below
	sink.interval = time.Hour // wide open: only an immediate flush can write inside this test
	waitFor, _ := newWriteWatcher(t, sink)

	sink.observe("A", usagehdr.Snapshot{Known: true, At: time.Now(), FiveHour: knownWindow(), SevenDay: knownWindow()}, usagehdr.Verdict{})
	waitFor()
	sink.setPassthrough("D", "token stale")
	// This write must come from setPassthrough's own immediate-flush
	// branch, not from Close below (which would write the current state
	// regardless of whether setPassthrough ever bypassed the coalescing
	// window).
	waitFor()
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	var d *status.Account
	for i := range f.Accounts {
		if f.Accounts[i].Name == "D" {
			d = &f.Accounts[i]
		}
	}
	if d == nil || d.Passthrough != "token stale" {
		t.Fatalf("accounts = %+v; a new passthrough mark must reach disk immediately, not wait for the coalescing window", f.Accounts)
	}
}

// TestSinkWritesOffTheCallingGoroutine pins F34: observe() must return
// without having done the fsync, and the document must still reach disk.
func TestSinkWritesOffTheCallingGoroutine(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close() // safety net: idempotent alongside the explicit Close below
	sink.ensureAccounts([]string{"A"})
	sink.setPassthrough("A", "token stale")

	// The write is queued, not done inline; Close drains it.
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range f.Accounts {
		if a.Name == "A" && a.Passthrough == "token stale" {
			found = true
		}
	}
	if !found {
		t.Errorf("the queued document never reached disk: %+v", f.Accounts)
	}
}

// TestSinkCoalescesABurstToTheNewestDocument pins that a burst produces a
// write of the NEWEST document — never an older one landing last.
func TestSinkCoalescesABurstToTheNewestDocument(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close() // safety net: idempotent alongside the explicit Close below
	names := []string{"A", "B", "C"}
	sink.ensureAccounts(names)
	for _, name := range names {
		sink.setPassthrough(name, "token stale")
	}
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		var ok bool
		for _, a := range f.Accounts {
			if a.Name == name && a.Passthrough == "token stale" {
				ok = true
			}
		}
		if !ok {
			t.Errorf("account %s lost its passthrough mark — an older document won the race: %+v", name, f.Accounts)
		}
	}
}

// TestCloseIsIdempotent: the shutdown path may reach Close through both a
// signal handler and a defer.
func TestCloseIsIdempotent(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.Close()
	sink.Close()
}

// TestConcurrentCloseBlocksUntilTheDrainFinishes covers the case
// TestCloseIsIdempotent cannot see: two Close calls racing each other, not
// running one after the other. runProxy reaches Close through both a
// signal handler and a defer, concurrently on shutdown — a Close that
// merely sees closed already true and returns immediately, without
// waiting for the writer that IS closing to finish, lets the process exit
// while the last document is still in flight, losing exactly the
// observation contract 4 says must survive.
func TestConcurrentCloseBlocksUntilTheDrainFinishes(t *testing.T) {
	home := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	sink.write = func(path string, b []byte) error {
		once.Do(func() {
			close(started)
			<-release
		})
		return status.WriteBytes(path, b)
	}

	sink.ensureAccounts([]string{"A"})
	sink.setPassthrough("A", "token stale") // queues a write; write() blocks on release
	<-started                               // the writer has grabbed the pending document and is mid-write

	returned := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sink.Close()
			returned <- struct{}{}
		}()
	}

	select {
	case <-returned:
		t.Fatal("a Close call returned before the in-flight write finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	wg.Wait()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough != "token stale" {
		t.Fatalf("accounts = %+v, want the in-flight document to have reached disk", f.Accounts)
	}
}

// TestWritePendingReportsAWriteErrorViaOnError pins writePending's
// err != nil branch: it now runs on the writer goroutine rather than the
// caller's, and had no test coverage anywhere before the write seam made
// it injectable.
func TestWritePendingReportsAWriteErrorViaOnError(t *testing.T) {
	errs := make(chan error, 1)
	sink, err := newStatusSink(t.TempDir(), func(e error) { errs <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close() // safety net: idempotent alongside the explicit Close below
	wantErr := errors.New("disk full")
	sink.write = func(string, []byte) error { return wantErr }

	sink.ensureAccounts([]string{"A"})
	sink.flush()

	select {
	case got := <-errs:
		if got != wantErr {
			t.Fatalf("onError got %v, want %v", got, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onError to be called")
	}
	sink.Close()
}

// TestObserveDoesNotBlockOnTheWrite is F34's measurement inverted: with the
// write behind a goroutine, disk latency must not be visible to the caller.
func TestObserveDoesNotBlockOnTheWrite(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.ensureAccounts([]string{"A"})

	start := time.Now()
	for i := 0; i < 200; i++ {
		sink.setPassthrough("A", "token stale")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Errorf("200 sink updates took %v on the calling goroutine; the write is still inline", d)
	}
}

// status --json must never trust a stored Running: it recomputes from the
// heartbeat, in both directions — a cache written by a daemon that has since
// been killed reports running: false regardless of what was stored, and a
// cache with a genuinely fresh heartbeat reports running: true even if what
// was stored says otherwise (Item 3, fix round 1: a build that replaced the
// overlay call with an unconditional `f.Daemon.Running = false` left the
// original, stale-only version of this test green, because it only ever
// stored Running: true and asserted false).
func TestStatusJSONRecomputesDaemonRunningFromHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name        string
		storedFresh bool // heartbeat age, not the stored Running bit
		wantRunning bool
	}{
		// Built directly, not via SetDaemon: SetDaemon never sets Running
		// (by design, see its doc comment), so a cache it produced would
		// already read Running=false regardless of whether runStatus
		// recomputes anything — these cases instead pin a cache that
		// STORES the opposite of what the heartbeat implies, as an old or
		// hand-edited document might, to prove the overlay actually
		// overrides the stored bit rather than merely agreeing with it.
		{"stale heartbeat overrides a stored running:true", false, false},
		{"fresh heartbeat overrides a stored running:false", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			heartbeat := time.Now().Add(-10 * time.Minute)
			if tc.storedFresh {
				heartbeat = time.Now()
			}
			f := status.File{Daemon: &status.Daemon{
				Running:    !tc.wantRunning,
				Heartbeat:  heartbeat,
				RouteDrift: 4,
			}}
			b, err := status.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := status.WriteBytes(status.Path(home), b); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			// --json is the pre-pass's now (M1d-d): JSON mode is the reporter's.
			if code := runStatus(home, nil, newReporter(true, &out, io.Discard)); code != 0 {
				t.Fatalf("runStatus = %d, want 0", code)
			}
			var got struct {
				Daemon *struct {
					Running    bool   `json:"running"`
					RouteDrift uint64 `json:"routeDrift"`
				} `json:"daemon"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Daemon == nil {
				t.Fatal("--json dropped the daemon object")
			}
			if got.Daemon.Running != tc.wantRunning {
				t.Errorf("running = %v, want %v", got.Daemon.Running, tc.wantRunning)
			}
			if got.Daemon.RouteDrift != 4 {
				t.Errorf("routeDrift = %d, want 4 — the stored counter must survive the overlay", got.Daemon.RouteDrift)
			}
		})
	}
}

// TestSinkRecordsTokenState pins F92: status.Account.Token had no writer
// anywhere until this task. setTokenState is the writer, fed from the token
// state the product flow's own credential lookup already computed — never a
// new probe.
func TestSinkRecordsTokenState(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink.setTokenState("A", creds.StateExpiring)
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Token != creds.StateExpiring {
		t.Fatalf("accounts = %+v, want one A with Token = %q", f.Accounts, creds.StateExpiring)
	}
}

// TestNewPassthroughHookWritesTheTokenStateFromARealSelectorEvent pins the
// wiring: a passthrough event carrying a Status (the selector's own Token()
// call, not a new probe) must reach the cache's Token field, and an event
// with no Status (e.g. "no serving account set") must not overwrite it with
// the zero TokenState.
func TestNewPassthroughHookWritesTheTokenStateFromARealSelectorEvent(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	hook := newPassthroughHook(sink, func(selector.Event) {})
	hook(selector.Event{Kind: "passthrough", Account: "A", Status: creds.Status{State: creds.StateNeedsLogin}})
	// A second passthrough event with no Status at all (e.g. "no serving
	// account set" — selector.Choose's own state-error/no-account paths
	// never populate Status) must not clobber the Token this hook already
	// wrote with the zero TokenState.
	hook(selector.Event{Kind: "passthrough", Account: "A", Detail: "no serving account set"})
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || f.Accounts[0].Token != creds.StateNeedsLogin {
		t.Fatalf("accounts = %+v, want one A with Token = %q unchanged by the zero-Status event", f.Accounts, creds.StateNeedsLogin)
	}
}

// TestRenderStatusSuppressesTheVersionLineForAMismatchToo pins N3 (final
// review): a "mismatch" identity (an unverified listener that answered but
// did not prove it holds this install's secret) must suppress the
// version-mismatch line exactly like "legacy" already does — its own
// identity line already says everything the version line would have
// added, and "run: chottag daemon restart" is the wrong advice for a
// squatter: there is no legitimate daemon on that port to restart.
func TestRenderStatusSuppressesTheVersionLineForAMismatchToo(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	f := status.File{
		Serving: "A", Remote: "A",
		Daemon: &status.Daemon{
			Running: true, Port: 47821,
			Version: "0.9.9-evil", VersionMismatch: true,
			Identity: string(shim.IdentityMismatch),
		},
	}

	var buf bytes.Buffer
	renderStatus(&buf, f, now)
	out := buf.String()

	if strings.Contains(out, "chottag daemon restart") {
		t.Fatalf("output = %q; a mismatch must never advise `daemon restart` — there is no legitimate daemon on that port", out)
	}
	if strings.Contains(out, "0.9.9-evil") {
		t.Fatalf("output = %q; the unverified listener's self-reported version must not be printed for a mismatch", out)
	}
	if !strings.Contains(out, "did not prove it is this install's daemon") {
		t.Fatalf("output = %q; the mismatch identity line itself must still print", out)
	}
}

func TestPassthroughReasonPrefersTokenStateAndDetail(t *testing.T) {
	cases := []struct {
		name string
		e    selector.Event
		want string
	}{
		{"state only", selector.Event{Status: creds.Status{State: creds.StateStale}}, "token stale"},
		{"state and detail", selector.Event{Status: creds.Status{State: creds.StateNeedsLogin}, Detail: "no such account"}, "token needs-login: no such account"},
		{"detail only", selector.Event{Detail: "no serving account set"}, "no serving account set"},
		{"neither", selector.Event{}, "passthrough"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := passthroughReason(c.e); got != c.want {
				t.Errorf("passthroughReason(%+v) = %q, want %q", c.e, got, c.want)
			}
		})
	}
}
