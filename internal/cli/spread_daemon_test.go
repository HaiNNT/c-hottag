package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// spreadAutoRig is the auto-switch rig (A serving, B, C pro, D max20x, all
// at 10%) under the spread policy, with an engine and three sessions s1..s3
// placed on A through the pin.
func spreadAutoRig(t *testing.T) *autoRig {
	t.Helper()
	r := newAutoRig(t)
	r.update(func(st *store.State) { st.Policy, st.Pin = store.PolicySpread, "A" })
	r.as.spread = newSpreadEngine(filepath.Join(r.home, "run", "placements.json"), r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now)
	for _, sid := range []string{"s1", "s2", "s3"} {
		if got, ok := r.as.spread.account(sid, r.now); !ok || got != "A" {
			t.Fatalf("%s placed on %q, %v; want the pinned A", sid, got, ok)
		}
	}
	r.update(func(st *store.State) { st.Pin = "" })
	r.as.evaluate(r.now, false) // the first evaluation after a start only seeds the marks
	r.notices()
	return r
}

func TestSpreadEvaluateMarksInsteadOfSwitchingAndNoticesOnce(t *testing.T) {
	r := spreadAutoRig(t)
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)

	// serving follows what a new session would get (D: the most headroom),
	// not the serial planner's switch.
	if got := r.serving(); got != "D" {
		t.Fatalf("serving = %q, want D (a new session's account)", got)
	}
	if strings.Contains(r.log.String(), "auto-switch") {
		t.Fatalf("spread logged an auto-switch: %q", r.log.String())
	}
	if d := r.published().Decision; !strings.HasPrefix(d, "spread: ") || !strings.Contains(d, "D") {
		t.Fatalf("published decision = %q", d)
	}
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: moved 3 sessions from A to D, B, C" || got[0].body != "A reached its 5-hour switch point." {
		t.Fatalf("notices = %+v, want one: moved 3 sessions from A to D, B, C (A reached its 5-hour switch point.)", got)
	}
	// the same condition on the next evaluations: no second notice.
	r.as.evaluate(r.now.Add(time.Second), false)
	r.as.tick(r.now.Add(2 * time.Second))
	if n := r.notices(); len(n) != 0 {
		t.Fatalf("repeated notices: %+v", n)
	}
	// the sessions move lazily, at their next request, off A.
	for _, sid := range []string{"s1", "s2", "s3"} {
		if acct, ok := r.as.spread.account(sid, r.now); !ok || acct == "A" {
			t.Fatalf("%s is on %q after A crossed its point", sid, acct)
		}
	}
	// A recovers (its window reset): a new crossing is a new event.
	r.row(freshRow("A", 10, 10, r.now))
	r.as.evaluate(r.now, false)
	r.row(busyRow("A", 96, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if n := r.notices(); len(n) != 0 {
		t.Fatalf("a crossing with no sessions left on A still posted: %+v", n)
	}
}

func TestSpreadEvaluateSerialPathIsUntouched(t *testing.T) {
	r := newAutoRig(t, onlyB(eligNow)...)
	r.as.spread = newSpreadEngine(filepath.Join(r.home, "run", "placements.json"), r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now) // wired, but the policy is serial
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.evaluate(r.now, false)
	if got := r.serving(); got != "B" {
		t.Fatalf("serial serving = %q, want the planner's B", got)
	}
	if !strings.Contains(r.log.String(), "auto-switch A -> B") {
		t.Fatalf("no auto-switch line: %q", r.log.String())
	}
	if len(r.as.spread.snapshot()) != 0 || len(r.as.spread.marked) != 0 {
		t.Fatal("the serial path touched the engine")
	}
}

func TestSpreadWallRetryMovesOnlyTheLimitedSession(t *testing.T) {
	r := spreadAutoRig(t)
	retry, done := r.as.wallRetry(idCtx("s1"), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("the limit hit was not retried")
	}
	to, ok := r.as.spread.placed("s1")
	if !ok || to == "A" {
		t.Fatalf("s1 is on %q after its limit hit", to)
	}
	for _, sid := range []string{"s2", "s3"} {
		if acct, _ := r.as.spread.placed(sid); acct != "A" {
			t.Fatalf("%s was moved eagerly to %q", sid, acct)
		}
	}
	if got := r.serving(); got == "A" {
		t.Fatal("serving stayed on the limited account")
	}
	done(to, 200)
	if !strings.Contains(r.log.String(), "wall retry A -> "+to+" 200") {
		t.Fatalf("log = %q", r.log.String())
	}
	// the limited account is marked: the others follow at their next request.
	if acct, ok := r.as.spread.account("s2", r.now); !ok || acct == "A" {
		t.Fatalf("s2 stayed on the limited A: %q", acct)
	}
}

func TestSpreadWallRetryForAnUnidentifiedSessionKeepsToday(t *testing.T) {
	r := spreadAutoRig(t)
	r.update(func(st *store.State) { st.Serving = "A" })
	retry, _ := r.as.wallRetry(context.Background(), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry {
		t.Fatal("an unidentified session's limit hit was not retried")
	}
	if got := r.serving(); got == "A" {
		t.Fatalf("serving = %q: the unidentified retry has nowhere to go", got)
	}
	for _, sid := range []string{"s1", "s2", "s3"} {
		if acct, _ := r.as.spread.placed(sid); acct != "A" {
			t.Fatalf("%s moved to %q by an unidentified session's retry", sid, acct)
		}
	}
}

func TestSpreadOnUsageOfANonServingAccountMarksIt(t *testing.T) {
	r := newAutoRig(t)
	r.update(func(st *store.State) { st.Policy, st.Pin = store.PolicySpread, "B" })
	r.as.spread = newSpreadEngine(filepath.Join(r.home, "run", "placements.json"), r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now)
	if got, _ := r.as.spread.account("s1", r.now); got != "B" {
		t.Fatalf("s1 on %q, want the pinned B", got)
	}
	r.update(func(st *store.State) { st.Pin = "" })
	r.as.evaluate(r.now, false) // seeds; serving moves off A to a new session's account
	r.notices()
	r.update(func(st *store.State) { st.Serving = "A" })
	r.row(busyRow("B", 95, r.now, 3*time.Hour)) // what the usage hook folded in just before
	r.as.onUsage("B", 200, http.Header{})
	got := r.notices()
	if len(got) != 1 || !strings.HasPrefix(got[0].title, "chottag: moved 1 session from B to ") {
		t.Fatalf("notices = %+v: a response from B (not serving) did not mark it", got)
	}
}

func TestSpreadWallRetryWorksWithAutoSwitchOff(t *testing.T) {
	r := spreadAutoRig(t)
	r.update(func(st *store.State) { st.Auto = &store.Auto{Enabled: new(bool)} })
	if st, _ := r.state(); st.AutoOn() {
		t.Fatal("auto is still on")
	}
	retry, done := r.as.wallRetry(idCtx("s1"), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("an identified session's limit hit was not retried with auto-switch off")
	}
	if to, _ := r.as.spread.placed("s1"); to == "A" {
		t.Fatal("s1 stayed on the limited account")
	}
}

func TestSpreadWallRetryIsReportedOncePerLimitedAccount(t *testing.T) {
	r := spreadAutoRig(t)
	retry, done := r.as.wallRetry(idCtx("s1"), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry {
		t.Fatal("not retried")
	}
	to, _ := r.as.spread.placed("s1")
	done(to, 200)
	var titles []string
	for _, n := range r.notices() {
		titles = append(titles, n.title)
	}
	want := "chottag: switched to " + to
	if len(titles) != 2 || !strings.HasPrefix(titles[0], "chottag: moved 2 sessions from A to ") || titles[1] != want {
		t.Fatalf("notices = %q, want the moved notice and %q", titles, want)
	}
	// a second session's limit hit on the same account: retried, not re-announced.
	retry, done = r.as.wallRetry(idCtx("s2"), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry {
		t.Fatal("second not retried")
	}
	to2, _ := r.as.spread.placed("s2")
	done(to2, 200)
	if n := r.notices(); len(n) != 0 {
		t.Fatalf("a second limit hit on A posted again: %+v", n)
	}
}

func TestSpreadUnidentifiedWallRetryIsReported(t *testing.T) {
	r := spreadAutoRig(t)
	r.update(func(st *store.State) { st.Serving = "A" })
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("not retried")
	}
	to := r.serving()
	done(to, 200)
	var titles []string
	for _, n := range r.notices() {
		titles = append(titles, n.title)
	}
	if !strings.Contains(strings.Join(titles, "|"), "chottag: switched to "+to) {
		t.Fatalf("notices = %q, want the switched notice", titles)
	}
}

func TestSpreadFirstEvaluationOnlySeedsMarks(t *testing.T) {
	r := spreadAutoRig(t) // seeded, A at 10%
	// a daemon restart: a new switcher and engine on the same placements.
	r.row(busyRow("A", 95, r.now, 3*time.Hour))
	r.as.spread.close()
	r.as.spread = newSpreadEngine(filepath.Join(r.home, "run", "placements.json"), r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now)
	r.as.spreadSeeded = false
	for _, sid := range []string{"s1", "s2", "s3"} {
		r.as.spread.account(sid, r.now)
	}
	r.as.evaluate(r.now, false)
	if n := r.notices(); len(n) != 0 {
		t.Fatalf("the first evaluation after a start announced an old crossing: %+v", n)
	}
	// a later, different crossing is a new event.
	r.row(busyRow("B", 95, r.now, 3*time.Hour))
	r.as.spread.placements["s1"].Account, r.as.spread.placements["s1"].Dir = "B", ""
	r.as.evaluate(r.now.Add(time.Second), false)
	if n := r.notices(); len(n) != 1 {
		t.Fatalf("notices after B crossed = %+v, want one", n)
	}
}

// The daemon wiring: the roster tick saves and prunes, shutdown saves, and a
// restart keeps each session on its account.
func TestDaemonSpreadPlacementsSurviveARestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	addSlotAccount(t, home, "B")
	s := store.Store{Dir: home}
	setPin := func(pin string) {
		if _, err := s.Update(func(st *store.State) error { st.Policy, st.Pin = store.PolicySpread, pin; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "run", "placements.json")
	run := func() (ch *chooser, tick chan<- time.Time, processed <-chan struct{}, stop func(), fin <-chan struct{}) {
		cache := store.NewCache(s)
		sink, err := newStatusSink(home, nil)
		if err != nil {
			t.Fatal(err)
		}
		tr := sessions.NewTracker()
		elog := newSyncBuf()
		t.Cleanup(func() { t.Log("spread log:", elog.String()) })
		eng := newSpreadEngine(path, cache.State, sink.fileCopy, tr.Peek, elog, time.Now())
		toks := fakeTokens2{filepath.Join(home, "accounts", "A"): "tok-A", filepath.Join(home, "accounts", "B"): "tok-B"}
		sel := selector.New(selector.Config{State: cache.State, Tokens: toks, Owners: fakeOwners{}})
		ch = &chooser{sel: sel, state: cache.State, spread: eng, tracker: tr}
		tc, pc := make(chan time.Time), make(chan struct{})
		stop, fin, _ = startWiringDaemon(t, home, daemonDeps{Sink: sink, Spread: eng, Chooser: ch, RosterTick: tc, RosterProcessed: pc})
		return ch, tc, pc, stop, fin
	}

	setPin("B")
	ch, tick, processed, stop, fin := run()
	if acct, _, _, ok := ch.Choose(idCtx("s1"), servingDecision, ""); !ok || acct != "B" {
		t.Fatalf("s1 on %q, %v; want the pinned B", acct, ok)
	}
	tick <- time.Now()
	<-processed
	stop()
	<-fin
	// shutdown saved it (the tick may have, too).
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"s1"`) {
		t.Fatalf("placements.json after shutdown: %q, %v", raw, err)
	}

	setPin("") // a fresh placement would now be A, the first account
	ch, tick, processed, stop, fin = run()
	if acct, _, _, ok := ch.Choose(idCtx("s1"), servingDecision, ""); !ok || acct != "B" {
		t.Fatalf("after the restart s1 is on %q, %v; want B, where its cache is warm", acct, ok)
	}
	for i := 0; i < 2; i++ { // prune: s1 is not in the registry but was just seen
		tick <- time.Now()
		<-processed
	}
	if acct, _, _, _ := ch.Choose(idCtx("s1"), servingDecision, ""); acct != "B" {
		t.Fatalf("after a tick s1 is on %q", acct)
	}
	stop()
	<-fin
}

// Final review 1: under spread, responses never write state.json, and the
// roster tick keeps serving steady while usage creeps.
func TestSpreadResponsesNeverWriteServingAndTicksAreSteady(t *testing.T) {
	r := newAutoRig(t)
	r.update(func(st *store.State) { st.Policy = store.PolicySpread })
	eng := newSpreadEngine(filepath.Join(r.home, "run", "placements.json"), r.state, r.sink.fileCopy,
		func(string, string) int { return 0 }, r.log, r.now)
	r.as.spread = eng
	for i := 0; i < 16; i++ { // sessions spread over A, B, C, D by score
		eng.account(fmt.Sprintf("s%02d", i), r.now)
	}
	r.as.tick(r.now) // seeds the marks and sets serving once
	r.notices()
	statePath := filepath.Join(r.home, "state.json")
	before, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, _ := os.ReadFile(statePath)
	for i := 1; i <= 100; i++ { // usage creeping on every account, 100 responses
		for _, n := range []string{"A", "B", "C", "D"} {
			r.row(freshRow(n, 10+float64(i%40)*0.5+float64(len(n)), 10, r.now))
			r.as.onUsage(n, 200, http.Header{})
		}
	}
	after, _ := os.Stat(statePath)
	afterBytes, _ := os.ReadFile(statePath)
	if !after.ModTime().Equal(before.ModTime()) || string(afterBytes) != string(beforeBytes) {
		t.Fatal("responses under spread rewrote state.json")
	}
	// even when the serving account crosses its point on a response, only the
	// tick moves serving.
	serving := r.serving()
	r.row(busyRow(serving, 96, r.now, 3*time.Hour))
	r.as.onUsage(serving, 200, http.Header{})
	if got := r.serving(); got != serving {
		t.Fatalf("a response moved serving from %s to %s", serving, got)
	}
	r.as.tick(r.now)
	if got := r.serving(); got == serving {
		t.Fatalf("the tick left serving on %s, over its switch point", serving)
	}
	// the tick cadence: serving changes are bounded by hysteresis.
	changes, last := 0, r.serving()
	for i := 1; i <= 100; i++ {
		for _, n := range []string{"A", "B", "C", "D"} {
			r.row(freshRow(n, 10+float64((i+len(n))%40)*0.5, 10, r.now))
		}
		r.as.tick(r.now)
		if got := r.serving(); got != last {
			changes++
			last = got
		}
	}
	if changes > 5 {
		t.Fatalf("serving changed %d times over 100 ticks of small usage creep", changes)
	}
}

// Final review 4: the limit notice is owed until one is posted, even when the
// resend never happened (the proxy declined: to == "").
func TestSpreadWallRetryNoticeSurvivesADeclinedResend(t *testing.T) {
	r := spreadAutoRig(t)
	retry, done := r.as.wallRetry(idCtx("s1"), "A", limitHeader(r.now.Add(2*time.Hour)))
	if !retry {
		t.Fatal("not retried")
	}
	chosen, _ := r.as.spread.placed("s1")
	done("", 0) // the re-Choose gave nothing
	var titles []string
	for _, n := range r.notices() {
		titles = append(titles, n.title)
	}
	if !strings.Contains(strings.Join(titles, "|"), "chottag: switched to "+chosen) {
		t.Fatalf("a declined resend posted no notice: %q", titles)
	}
	// and now it was posted, a later one for the same account is not repeated.
	_, done2 := r.as.wallRetry(idCtx("s2"), "A", limitHeader(r.now.Add(2*time.Hour)))
	done2("D", 200)
	if n := r.notices(); len(n) != 0 {
		t.Fatalf("repeated: %+v", n)
	}
}
