package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// TestWatchRosterOnlyForgetsAccountsMissingFromTheCurrentRoster pins that
// the watcher's diff is a real diff — "known but missing from the CURRENT
// roster" — not "everything known, every tick regardless of the current
// roster." It replaces two earlier, separate tests
// (TestWatchRosterForgetsARemovedAccount and
// TestWatchRosterDoesNotForgetOnTheFirstTick, which this one subsumes: it
// covers both the removal case and the do-nothing-when-unchanged case,
// plus the case neither of them caught).
//
// The do-nothing check in the old TestWatchRosterDoesNotForgetOnTheFirstTick
// was vacuous: it sent two ticks on a buffered channel — both sends
// returned immediately without the watcher having done anything yet — then
// read a plain counter with no synchronisation to the watcher's own
// progress at all. Confirmed by mutation: with `if !cur[name]` changed to
// `if true` (forget every known account on every tick, not just the ones
// actually missing from cur), that test passed 50 times out of 50 — it
// could not fail, because it never waited for the watcher to reach any
// particular point before checking.
//
// This test instead blocks on `processed`, which watchRoster sends on
// after EVERY tick it fully handles, whether or not that tick forgot
// anything. That gives each assertion below a real happens-before edge to
// "the watcher has definitely finished processing exactly N ticks" — so
// the same `if true` mutation is now caught deterministically: since
// `known` is non-nil starting on the SECOND tick (set from the first
// tick's cur), an unconditional forget on tick two forgets every entry in
// `known` regardless of the roster being unchanged, and draining
// `forgotten` right after that tick's `processed` signal reliably finds
// it non-empty — no reliance on map iteration order, no reliance on
// elapsed wall time.
func TestWatchRosterOnlyForgetsAccountsMissingFromTheCurrentRoster(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		st.Accounts = []store.Account{
			{Name: "A", Dir: filepath.Join(dir, "A")},
			{Name: "B", Dir: filepath.Join(dir, "B")},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	// Generously sized and drained after every tick, so a forget call
	// never blocks the watcher's goroutine even if a mutation makes it
	// forget more than the one account any single tick here should ever
	// produce.
	forgotten := make(chan string, 8)
	tick := make(chan time.Time, 4)
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchRoster(ctx, cache, func(name string) error {
		forgotten <- name
		return nil
	}, func(string, string) (int, error) { return 0, nil }, tick, func(error) {}, processed, nil, nil, nil)

	awaitProcessed := func(tag string) {
		t.Helper()
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatalf("watchRoster never finished processing %s", tag)
		}
	}
	// Safe to call only once awaitProcessed for the same tick has already
	// returned: processed is sent strictly after every forget call for
	// that tick (same goroutine, program order), and receiving on
	// processed synchronises-with that send, so every item that tick's
	// processing queued onto forgotten is already visible here — this is
	// not a "wait a bit and see nothing" check.
	drainForgotten := func() []string {
		var got []string
		for {
			select {
			case name := <-forgotten:
				got = append(got, name)
			default:
				return got
			}
		}
	}

	tick <- time.Now() // baseline: nothing to diff against yet
	awaitProcessed("the baseline tick")
	if got := drainForgotten(); len(got) != 0 {
		t.Fatalf("forgot %v on the baseline tick, want nothing", got)
	}

	tick <- time.Now() // unchanged roster: known == cur, nothing should be forgotten
	awaitProcessed("the unchanged tick")
	if got := drainForgotten(); len(got) != 0 {
		t.Fatalf("forgot %v on an unchanged tick, want nothing", got)
	}

	if _, err := s.Update(func(st *store.State) error {
		st.Accounts = []store.Account{{Name: "A", Dir: filepath.Join(dir, "A")}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tick <- time.Now() // B removed
	awaitProcessed("the tick after B was removed")
	if got := drainForgotten(); len(got) != 1 || got[0] != "B" {
		t.Fatalf("forgot %v after removing B, want exactly [B]", got)
	}
}

// TestWatchRosterInvalidatesTheTokenCacheWhenLoggedInAtAdvances is item 6
// (review round 3): a token read cached under tokens.Manager's ReadTTL does
// not itself watch state.json, so a request landing within ReadTTL of a
// re-login would otherwise still see the stale, pre-login read. The roster
// tick already reloads state.json (the same read the forget/rename diff
// above uses); this pins that it also invalidates a slot whose LoggedInAt
// advanced since the last tick, and leaves a baseline read and an unchanged
// roster alone.
//
// The real invalidate runs off this goroutine (item 2, review round 4:
// tokens.Manager.Invalidate can block on a Keychain prompt, the same as
// InvalidateAll, and this goroutine also runs the auto tick, the notify
// tick, the heartbeat and the shutdown join), so awaitProcessed's
// happens-before edge to "this tick's own work is done" does not cover it.
// awaitInvalidated below waits out that asynchrony directly instead of
// assuming any particular delay — a liveness bound (fail if it never
// arrives), not a correctness bound tied to any production interval.
func TestWatchRosterInvalidatesTheTokenCacheWhenLoggedInAtAdvances(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	loginAt := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	if _, err := s.Update(func(st *store.State) error {
		st.Accounts = []store.Account{
			{Name: "A", Dir: filepath.Join(dir, "A"), LoggedInAt: loginAt},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	invalidated := make(chan string, 8)
	tick := make(chan time.Time, 4)
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchRoster(ctx, cache, func(string) error { return nil }, func(string, string) (int, error) { return 0, nil }, tick, func(error) {}, processed, nil, nil, func(dir string) {
		invalidated <- dir
	})

	awaitProcessed := func(tag string) {
		t.Helper()
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatalf("watchRoster never finished processing %s", tag)
		}
	}
	// awaitInvalidated waits for the async invalidate call: a liveness
	// bound (fail the test if it never shows up), not a guess at how long
	// the goroutine it runs on takes.
	awaitInvalidated := func(tag string) string {
		t.Helper()
		select {
		case d := <-invalidated:
			return d
		case <-time.After(10 * time.Second):
			t.Fatalf("no invalidate call arrived %s", tag)
			return ""
		}
	}
	// assertNoInvalidateSoon only rules out a call that was ALREADY
	// scheduled by the time awaitProcessed returned — the tick's own
	// synchronous work (including the `go invalidate(...)` call, if any)
	// is done by then, so any goroutine it started is already running;
	// this is not racing a call that has not been scheduled yet.
	assertNoInvalidateSoon := func(tag string) {
		t.Helper()
		select {
		case d := <-invalidated:
			t.Fatalf("invalidated %q %s, want none", d, tag)
		case <-time.After(200 * time.Millisecond):
		}
	}

	tick <- time.Now() // baseline: nothing to diff against yet
	awaitProcessed("the baseline tick")
	assertNoInvalidateSoon("on the baseline tick")

	tick <- time.Now() // unchanged roster
	awaitProcessed("the unchanged tick")
	assertNoInvalidateSoon("on an unchanged tick")

	if _, err := s.Update(func(st *store.State) error {
		st.Accounts[0].LoggedInAt = loginAt.Add(time.Hour) // a re-login
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tick <- time.Now()
	awaitProcessed("the tick after A logged back in")
	want := filepath.Join(dir, "A")
	if got := awaitInvalidated("after A's LoggedInAt advanced"); got != want {
		t.Fatalf("invalidated %q, want %s", got, want)
	}
	assertNoInvalidateSoon("beyond the one expected call")
}

// TestWatchRosterNeverForgetsARenamedAccount is fix round 1's Critical fix:
// the roster diff keys on slot Dir, fixed when an account is created and
// never touched by `chottag rename` (R21/D1), not on Name, which is
// exactly what rename changes. Before this fix, a departed name looked
// identical to a removal, so the tick right after a rename's step 1 called
// forget(old) — deterministically for a case-only rename, since
// owners.Forget matches case-insensitively and the new spelling is what
// diffed as "new" (not "known"); and for ANY rename a tick happened to
// land on between its two separate writes, since state.json's new name is
// visible before owners.json is rewritten to match.
//
// It drives five scenarios against one daemon-owners wiring, in order: a
// case-only rename, a normal rename, a rename caught mid-flight (state
// renamed, owners.json step 2 failed) — which I1 (F174) requires the
// watcher itself to heal, by calling rename(old, new) on the daemon's own
// owners.Map, WITHOUT any re-run of `chottag rename` — a genuinely
// no-op re-run confirming that healing was idempotent, and — the control —
// a real removal, which must still be forgotten, under the name it was
// last seen with.
//
// The watcher's rename target here is a real, open *owners.Map (own),
// exactly the one runDaemon wires production's Rename seam to
// (Owners.RenameAccount) — not a hand-rolled substitute — so this pins the
// actual in-memory effect, not just that some function got called.
func TestWatchRosterNeverForgetsARenamedAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")}); err != nil {
			return err
		}
		st.Serving, st.Remote = "A", "A" // so B/b/Bee/Beeb can be removed below
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if err := own.Reassign(router.KindArtifact, "a1", "B", time.Now()); err != nil {
		t.Fatal(err)
	}

	forgotten := make(chan string, 8)
	renamed := make(chan [2]string, 8)
	tick := make(chan time.Time, 4)
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchRoster(ctx, cache, func(name string) error {
		forgotten <- name
		return nil
	}, func(from, to string) (int, error) {
		n, err := own.RenameAccount(from, to)
		renamed <- [2]string{from, to}
		return n, err
	}, tick, func(error) {}, processed, nil, nil, nil)

	awaitProcessed := func(tag string) {
		t.Helper()
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatalf("watchRoster never finished processing %s", tag)
		}
	}
	drainForgotten := func() []string {
		var got []string
		for {
			select {
			case name := <-forgotten:
				got = append(got, name)
			default:
				return got
			}
		}
	}
	drainRenamed := func() [][2]string {
		var got [][2]string
		for {
			select {
			case pair := <-renamed:
				got = append(got, pair)
			default:
				return got
			}
		}
	}

	tick <- time.Now() // baseline: nothing to diff against yet
	awaitProcessed("the baseline tick")
	drainForgotten()
	drainRenamed()

	// renameStateOnly is `chottag rename`'s step 1 alone (state.json), the
	// exact shape a tick can land on mid-rename, or after a genuinely
	// interrupted step 2.
	renameStateOnly := func(oldName, newName string) {
		t.Helper()
		if _, err := s.Update(func(st *store.State) error {
			_, _, err := st.Rename(oldName, newName)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. A case-only rename: B -> b. Dir unchanged.
	renameStateOnly("B", "b")
	tick <- time.Now()
	awaitProcessed("the case-only rename tick")
	if got := drainForgotten(); len(got) != 0 {
		t.Fatalf("forgot %v after a case-only rename, want nothing", got)
	}
	if got := drainRenamed(); len(got) != 1 || got[0] != [2]string{"B", "b"} {
		t.Fatalf("renamed %v, want exactly [[B b]]", got)
	}
	if a, _ := own.Lookup(router.KindArtifact, "a1"); a != "b" {
		t.Fatalf("owner = %q, want b", a)
	}

	// 2. A normal rename: b -> Bee. Still the same Dir.
	renameStateOnly("b", "Bee")
	tick <- time.Now()
	awaitProcessed("the normal rename tick")
	if got := drainForgotten(); len(got) != 0 {
		t.Fatalf("forgot %v after a normal rename, want nothing", got)
	}
	if got := drainRenamed(); len(got) != 1 || got[0] != [2]string{"b", "Bee"} {
		t.Fatalf("renamed %v, want exactly [[b Bee]]", got)
	}
	if a, _ := own.Lookup(router.KindArtifact, "a1"); a != "Bee" {
		t.Fatalf("owner = %q, want Bee", a)
	}

	// 3. An interrupted rename: Bee -> Beeb. Only state.json's step 1 runs
	// (owners.json's own step 2 is never even attempted here) — the exact
	// shape a tick can land on mid-flight, or forever, if step 2 keeps
	// failing. I1 (F174): the tick itself must heal this, by renaming the
	// daemon's own owner map, not merely avoid forgetting it.
	renameStateOnly("Bee", "Beeb")
	tick <- time.Now()
	awaitProcessed("the interrupted-rename tick")
	if got := drainForgotten(); len(got) != 0 {
		t.Fatalf("forgot %v while owners.json still names the pre-rename spelling, want nothing", got)
	}
	if got := drainRenamed(); len(got) != 1 || got[0] != [2]string{"Bee", "Beeb"} {
		t.Fatalf("renamed %v, want exactly [[Bee Beeb]]", got)
	}
	if a, _ := own.Lookup(router.KindArtifact, "a1"); a != "Beeb" {
		t.Fatalf("owner = %q, want Beeb — healed by the tick itself, with no `chottag rename` re-run", a)
	}

	// A further tick with nothing new to rename must be a genuine no-op:
	// RenameAccount's own idempotence (owners package) means nothing gets
	// rewritten, but the watcher still calls it every tick the dir's known
	// and current names differ — which, once healed, they no longer do, so
	// it is not called again at all.
	tick <- time.Now()
	awaitProcessed("the tick after healing")
	if got := drainForgotten(); len(got) != 0 {
		t.Fatalf("forgot %v after healing, want nothing", got)
	}
	if got := drainRenamed(); len(got) != 0 {
		t.Fatalf("renamed %v after healing, want nothing (names already match)", got)
	}

	// Control: a real removal (the Dir itself disappears) is still
	// forgotten, under the name it was last seen with.
	if _, err := s.Update(func(st *store.State) error { return st.Remove("Beeb") }); err != nil {
		t.Fatal(err)
	}
	tick <- time.Now()
	awaitProcessed("the removal tick")
	if got := drainForgotten(); len(got) != 1 || got[0] != "Beeb" {
		t.Fatalf("forgot %v after removing Beeb, want exactly [Beeb]", got)
	}
}

// TestWatchRosterKeepsARenamedAccountsEntriesWhenARemovedSlotSharedItsNewName
// is M1: `logout B` and `rename A B` landing within a single tick must not
// forget "B" — the dir that is actually gone is A's OLD name's dir... no,
// B's dir. The account now called "B" is A's slot, still very much present
// (a different dir). Diffing "B is missing from known dirs -> forget B"
// would wipe the just-renamed account's entries the moment they arrive,
// because forget matches by name, and the departed B and the arrived B
// share a spelling. The guard: skip forget(name) for a removed dir when
// some account in cur still has that name (case-insensitively).
func TestWatchRosterKeepsARenamedAccountsEntriesWhenARemovedSlotSharedItsNewName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")})
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if err := own.Reassign(router.KindArtifact, "fromB", "B", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := own.Reassign(router.KindArtifact, "fromA", "A", time.Now()); err != nil {
		t.Fatal(err)
	}

	forgotten := make(chan string, 8)
	renamed := make(chan [2]string, 8)
	tick := make(chan time.Time, 4)
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchRoster(ctx, cache, func(name string) error {
		forgotten <- name
		return nil
	}, func(from, to string) (int, error) {
		n, err := own.RenameAccount(from, to)
		renamed <- [2]string{from, to}
		return n, err
	}, tick, func(error) {}, processed, nil, nil, nil)

	awaitProcessed := func(tag string) {
		t.Helper()
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatalf("watchRoster never finished processing %s", tag)
		}
	}
	tick <- time.Now() // baseline
	awaitProcessed("the baseline tick")

	// logout B: the dir is gone (Remove refuses only serving/remote holders,
	// neither of which B is here). Then rename A -> B, now that the name is
	// free.
	if _, err := s.Update(func(st *store.State) error { return st.Remove("B") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(func(st *store.State) error {
		_, _, err := st.Rename("A", "B")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	tick <- time.Now()
	awaitProcessed("the logout-then-rename tick")

	var gotForgotten []string
	var gotRenamed [][2]string
drain:
	for {
		select {
		case name := <-forgotten:
			gotForgotten = append(gotForgotten, name)
		case pair := <-renamed:
			gotRenamed = append(gotRenamed, pair)
		default:
			break drain
		}
	}
	if len(gotForgotten) != 0 {
		t.Fatalf("forgot %v; the removed B's name is still in use by the renamed A, so it must not be forgotten", gotForgotten)
	}
	if len(gotRenamed) != 1 || gotRenamed[0] != [2]string{"A", "B"} {
		t.Fatalf("renamed %v, want exactly [[A B]]", gotRenamed)
	}
	// Both the old B's and the renamed (former A's) entries survive under B.
	if a, ok := own.Lookup(router.KindArtifact, "fromB"); !ok || a != "B" {
		t.Fatalf("fromB owner = %q, %v; want B (the removed account's own entries, kept)", a, ok)
	}
	if a, ok := own.Lookup(router.KindArtifact, "fromA"); !ok || a != "B" {
		t.Fatalf("fromA owner = %q, %v; want B (renamed from A)", a, ok)
	}
}

// renameRosterFixture is the shared setup for the two NEW-1 regression
// tests below: accounts A (accounts/A) and B (accounts/B), a1 owned by A
// and b1 owned by B, a real *owners.Map (own) wired as watchRoster's
// rename target through the SAME ownersTick production wiring runDaemon
// uses (so a real owners.Edit written by `chottag rename` gets adopted by
// Reload before the Dir diff ever runs — exactly the ordering NEW-1 is
// about), and a running watchRoster goroutine with forgotten/renamed
// capture channels.
func renameRosterFixture(t *testing.T) (home string, own *owners.Map, tick chan time.Time, forgotten chan string, renamed chan [2]string, awaitProcessed func(string)) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")})
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)

	if err := owners.Edit(filepath.Join(home, "owners.json"), func(tx *owners.Tx) error {
		tx.Reassign(router.KindArtifact, "a1", "A", time.Now())
		tx.Reassign(router.KindArtifact, "b1", "B", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(own.Close)

	forgotten = make(chan string, 8)
	renamed = make(chan [2]string, 8)
	tick = make(chan time.Time, 8)
	processed := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go watchRoster(ctx, cache, func(name string) error {
		forgotten <- name
		return nil
	}, func(from, to string) (int, error) {
		n, err := own.RenameAccount(from, to)
		renamed <- [2]string{from, to}
		return n, err
	}, tick, func(error) {}, processed, nil, ownersTick(own, func(error) {}), nil)

	awaitProcessed = func(tag string) {
		t.Helper()
		select {
		case <-processed:
		case <-time.After(10 * time.Second):
			t.Fatalf("watchRoster never finished processing %s", tag)
		}
	}
	return home, own, tick, forgotten, renamed, awaitProcessed
}

func drainRenameCaptures(forgotten chan string, renamed chan [2]string) (gotForgotten []string, gotRenamed [][2]string) {
	for {
		select {
		case name := <-forgotten:
			gotForgotten = append(gotForgotten, name)
		case pair := <-renamed:
			gotRenamed = append(gotRenamed, pair)
		default:
			return gotForgotten, gotRenamed
		}
	}
}

func runRenameNowForTest(t *testing.T, oldName, newName string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := runRename([]string{oldName, newName}, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("rename %s %s = %d, stderr %q", oldName, newName, code, errb.String())
	}
}

// TestWatchRosterSkipsTheDaemonHealForAChainedRenameInOneTick is NEW-1: two
// full, independently-completed renames (`rename A A2` then `rename B A`,
// each with its own real step 1 AND step 2) landing between one tick and
// the next. By the time the tick's ownersTick runs, Reload has already
// adopted BOTH renames' correct, final owners.json — a1 under "A2", b1
// under "A". The naive heal (rename oldName->newName by NAME, regardless
// of which dir) would then see accounts/A go A->A2 and blindly rename
// every entry named "A" (case-insensitively) to "A2" — sweeping up b1,
// which is now correctly, freshly named "A" by the SECOND rename, and is
// not accounts/A's entry at all. The fix: skip the daemon-side heal for a
// dir whose old name is still held by ANOTHER dir right now, or whose new
// name was held by a DIFFERENT dir that is still around — the rename
// command's own step 2 (already reloaded) is authoritative there.
func TestWatchRosterSkipsTheDaemonHealForAChainedRenameInOneTick(t *testing.T) {
	_, own, tick, forgotten, renamed, awaitProcessed := renameRosterFixture(t)

	tick <- time.Now() // baseline
	awaitProcessed("the baseline tick")
	drainRenameCaptures(forgotten, renamed)

	runRenameNowForTest(t, "A", "A2")
	runRenameNowForTest(t, "B", "A")

	tick <- time.Now()
	awaitProcessed("the chained-rename tick")
	gotForgotten, gotRenamed := drainRenameCaptures(forgotten, renamed)
	if len(gotForgotten) != 0 {
		t.Fatalf("forgot %v, want nothing", gotForgotten)
	}
	if len(gotRenamed) != 0 {
		t.Fatalf("the daemon healed %v; want it to defer entirely to the rename command's own (already-reloaded) step 2, since both dirs are ambiguous", gotRenamed)
	}
	if a, ok := own.Lookup(router.KindArtifact, "a1"); !ok || a != "A2" {
		t.Fatalf("a1 owner = %q, %v; want A2 (accounts/A's own entry, correctly renamed by the real rename command)", a, ok)
	}
	if a, ok := own.Lookup(router.KindArtifact, "b1"); !ok || a != "A" {
		t.Fatalf("b1 owner = %q, %v; want A (accounts/B's own entry) — a daemon heal that renames by NAME instead of by dir corrupts this to A2", a, ok)
	}
}

// TestWatchRosterSkipsTheDaemonHealForASwapInOneTick is NEW-1's other
// shape: a full swap done as three real renames (A->tmp, B->A, tmp->B)
// landing in one tick. Both accounts must keep their OWN objects — a1
// (accounts/A's) ends up under B, b1 (accounts/B's) ends up under A —
// exactly what the three real, already-reloaded renames produced, with the
// daemon's own heal deferring entirely (both dirs are ambiguous: each
// dir's old name is the other dir's current name).
func TestWatchRosterSkipsTheDaemonHealForASwapInOneTick(t *testing.T) {
	_, own, tick, forgotten, renamed, awaitProcessed := renameRosterFixture(t)

	tick <- time.Now() // baseline
	awaitProcessed("the baseline tick")
	drainRenameCaptures(forgotten, renamed)

	runRenameNowForTest(t, "A", "tmp")
	runRenameNowForTest(t, "B", "A")
	runRenameNowForTest(t, "tmp", "B")

	tick <- time.Now()
	awaitProcessed("the swap tick")
	gotForgotten, gotRenamed := drainRenameCaptures(forgotten, renamed)
	if len(gotForgotten) != 0 {
		t.Fatalf("forgot %v, want nothing", gotForgotten)
	}
	if len(gotRenamed) != 0 {
		t.Fatalf("the daemon healed %v; want it to defer entirely to the swap's own (already-reloaded) step 2s", gotRenamed)
	}
	if a, ok := own.Lookup(router.KindArtifact, "a1"); !ok || a != "B" {
		t.Fatalf("a1 owner = %q, %v; want B (accounts/A's own entry, now spelled B)", a, ok)
	}
	if a, ok := own.Lookup(router.KindArtifact, "b1"); !ok || a != "A" {
		t.Fatalf("b1 owner = %q, %v; want A (accounts/B's own entry, now spelled A)", a, ok)
	}
}
