package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// autoRig is one daemon's auto-switcher over a temp home seeded by
// eligHome (A serving, max5x; B max5x; C pro; D max20x), with an injected
// clock (F185), a recording notifier and a captured daemon.log.
type autoRig struct {
	t     *testing.T
	home  string
	s     store.Store
	sink  *statusSink
	dn    *daemonNotify
	rec   *recordingNotifier
	log   *syncBuf
	as    *autoSwitcher
	state func() (store.State, error)
	now   time.Time
}

// newAutoRig starts the switcher with every account fresh at 10% except
// the rows given, which replace theirs.
func newAutoRig(t *testing.T, rows ...status.Account) *autoRig {
	t.Helper()
	home, s := eligHome(t)
	base := map[string]status.Account{}
	for _, n := range []string{"A", "B", "C", "D"} {
		base[n] = freshRow(n, 10, 10, eligNow)
	}
	for _, r := range rows {
		base[r.Name] = r
	}
	writeEligStatus(t, home, base["A"], base["B"], base["C"], base["D"])
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	cache := store.NewCache(s)
	rec := newRecordingNotifier()
	dn := newDaemonNotify(cache.State, rec)
	t.Cleanup(dn.Close)
	r := &autoRig{t: t, home: home, s: s, sink: sink, dn: dn, rec: rec, log: newSyncBuf(), state: cache.State, now: eligNow}
	r.as = newAutoSwitcher(s, cache, sink, dn, r.log, nil)
	r.as.now = func() time.Time { return r.now }
	return r
}

// row replaces name's cached status row (what a response would have
// recorded) without writing status.json.
func (r *autoRig) row(a status.Account) {
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	for i := range r.sink.file.Accounts {
		if strings.EqualFold(r.sink.file.Accounts[i].Name, a.Name) {
			r.sink.file.Accounts[i] = a
			return
		}
	}
	r.sink.file.Accounts = append(r.sink.file.Accounts, a)
}

func (r *autoRig) serving() string {
	r.t.Helper()
	st, err := r.s.Load()
	if err != nil {
		r.t.Fatal(err)
	}
	return st.Serving
}

func (r *autoRig) published() status.Auto {
	r.as.mu.Lock()
	defer r.as.mu.Unlock()
	return r.as.published
}

func (r *autoRig) notices() []notice { return drainNotices(r.t, r.dn, r.rec) }

// fallbackTarget reads the switcher's own fallbackTarget flag (item 3a/3b,
// review round 4): whether it clears the moment serving changes for any
// OTHER reason, and whether it is set only by a fallback switch, are both
// facts about this exact field, not observable from notices alone in every
// scenario worth pinning directly.
func (r *autoRig) fallbackTarget() bool {
	r.as.mu.Lock()
	defer r.as.mu.Unlock()
	return r.as.fallbackTarget
}

func (r *autoRig) update(edit func(*store.State)) {
	r.t.Helper()
	if _, err := r.s.Update(func(st *store.State) error { edit(st); return nil }); err != nil {
		r.t.Fatal(err)
	}
}

// busyRow is name fresh at pct in the 5-hour window, resetting in reset.
func busyRow(name string, pct float64, at time.Time, reset time.Duration) status.Account {
	row := freshRow(name, pct, 10, at)
	row.Usage.FiveHourResetsAt = at.Add(reset)
	return row
}

// onlyB makes C (pro, 95% >= 88%) and D (max20x, 99.5% >= 98%) ineligible,
// so B is the only switch target.
func onlyB(at time.Time) []status.Account {
	return []status.Account{busyRow("C", 95, at, 3*time.Hour), busyRow("D", 99.5, at, 3*time.Hour)}
}

func countLines(log, substr string) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// TestAutoSwitchAtTheSwitchPoint: serving A crosses its 5h switch point
// (max5x 93%); one tick writes B as serving, logs the switch and posts one
// notice naming the window and percentage.
func TestAutoSwitchAtTheSwitchPoint(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	r.now = eligNow.Add(time.Minute)
	r.as.tick(r.now)
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q, want B", got)
	}
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to B" || got[0].body != "A reached 95% of its 5-hour limit." {
		t.Fatalf("notices = %+v", got)
	}
	if n := countLines(r.log.String(), "chottag: auto-switch A -> B (threshold 5h 95% >= 93%, balanced, continuity "); n != 1 {
		t.Fatalf("log = %q, want one auto-switch line", r.log.String())
	}
	p := r.published()
	if p.LastSwitch == nil || p.LastSwitch.From != "A" || p.LastSwitch.To != "B" || p.LastSwitch.Trigger != "threshold" || p.LastSwitch.Pct != 95 || !p.LastSwitch.At.Equal(r.now) {
		t.Fatalf("lastSwitch = %+v", p.LastSwitch)
	}
	// B is below its point: the next tick stays.
	r.as.tick(r.now.Add(time.Minute))
	if got := r.serving(); got != "B" || len(r.notices()) != 0 {
		t.Fatalf("serving = %q after a second tick, want B and no notice", got)
	}
}

// TestAutoHoldsForACacheAndLogsOnce: A is over its point but its 5h
// window resets inside hold5h (30m in balanced): stay, and log the hold
// once however many ticks see it.
func TestAutoHoldsForACacheAndLogsOnce(t *testing.T) {
	r := newAutoRig(t, busyRow("A", 95, eligNow, 9*time.Minute))
	r.as.tick(eligNow)
	r.as.tick(eligNow.Add(time.Minute))
	if got := r.serving(); got != "A" {
		t.Fatalf("serving = %q, want A held", got)
	}
	if d := r.published().Decision; d != "holding A (5h 95%, resets in 8m)" {
		t.Fatalf("decision = %q", d)
	}
	if n := countLines(r.log.String(), "chottag: auto: holding A (5h 95%, resets in 9m)"); n != 1 || countLines(r.log.String(), "holding") != 1 {
		t.Fatalf("log = %q, want exactly one hold line", r.log.String())
	}
	if len(r.notices()) != 0 {
		t.Fatal("a hold posted a notice")
	}
}

// TestAutoCooldownHoldsASoftSwitchButNotALimit: inside the 15m cooldown a
// second threshold crossing holds, but a hard limit still switches (S7).
func TestAutoCooldownHoldsASoftSwitchButNotALimit(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	r.as.tick(eligNow)
	if r.serving() != "B" {
		t.Fatal("setup: no switch to B")
	}
	r.notices()

	at := eligNow.Add(5 * time.Minute)
	r.row(busyRow("B", 95, at, 3*time.Hour))
	r.row(busyRow("D", 10, at, 3*time.Hour))
	r.as.tick(at)
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q inside the cooldown, want B", got)
	}
	if d := r.published().Decision; d != "holding B (5h 95%, cooldown 10m left)" {
		t.Fatalf("decision = %q", d)
	}

	at = at.Add(time.Minute)
	lim := busyRow("B", 100, at, time.Hour)
	lim.Limited, lim.LimitedUntil, lim.Window = true, at.Add(time.Hour), "five_hour"
	r.row(lim)
	r.row(busyRow("D", 10, at, 3*time.Hour))
	r.row(busyRow("A", 95, at, 3*time.Hour))
	r.row(busyRow("C", 95, at, 3*time.Hour))
	r.as.tick(at)
	if got := r.serving(); got != "D" {
		t.Fatalf("serving = %q after B's limit inside the cooldown, want D", got)
	}
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to D" || got[0].body != "B hit its 5-hour limit. Resend your last message." {
		t.Fatalf("notices = %+v", got)
	}
	if countLines(r.log.String(), "chottag: auto-switch B -> D (limit 5h, balanced") != 1 {
		t.Fatalf("log = %q", r.log.String())
	}
}

// TestAutoLosesTheCompareAndSwapToATag: the planner decided off a snapshot
// where A serves, but `chottag tag C` landed first. The swap is refused
// (S10): serving stays C, nothing is announced, and the next decision
// treats C as the user's choice.
func TestAutoLosesTheCompareAndSwapToATag(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	stale, err := r.s.Load()
	if err != nil {
		t.Fatal(err)
	}
	r.update(func(st *store.State) { st.Serving = "C" })
	live := r.as.state
	r.as.state = func() (store.State, error) { return stale, nil }
	r.as.tick(eligNow)
	if got := r.serving(); got != "C" {
		t.Fatalf("serving = %q, want the user's C kept", got)
	}
	if len(r.notices()) != 0 || strings.Contains(r.log.String(), "auto-switch") {
		t.Fatalf("a lost swap was announced: log %q", r.log.String())
	}
	if d := r.published().Decision; !strings.Contains(d, "serving changed") {
		t.Fatalf("decision = %q", d)
	}
	r.as.state = live
	r.as.tick(eligNow.Add(time.Minute))
	if p := r.published(); r.serving() != "C" || !p.UserChosen || p.Decision != "holding C (5h 95%, chosen by you)" {
		t.Fatalf("serving %q, auto %+v, want C held as the user's choice", r.serving(), p)
	}
}

// TestAutoHoldsTheUsersChoiceUntilItDropsBelow (S6, ruling 8): the user
// tags C while it is over its point; auto-switch leaves it alone until C
// drops below every point or serving changes again.
func TestAutoHoldsTheUsersChoiceUntilItDropsBelow(t *testing.T) {
	r := newAutoRig(t, busyRow("C", 95, eligNow, 3*time.Hour))
	r.as.tick(eligNow) // learns A
	r.update(func(st *store.State) { st.Serving = "C" })
	r.as.tick(eligNow.Add(time.Minute))
	if p := r.published(); r.serving() != "C" || !p.UserChosen {
		t.Fatalf("serving %q, auto %+v, want C held", r.serving(), p)
	}
	at := eligNow.Add(2 * time.Minute)
	r.row(busyRow("C", 50, at, 3*time.Hour))
	r.as.tick(at)
	if r.published().UserChosen {
		t.Fatal("the guard outlived C dropping below its point")
	}
	r.row(busyRow("C", 95, at.Add(time.Minute), 3*time.Hour))
	r.as.tick(at.Add(time.Minute))
	if got := r.serving(); got == "C" {
		t.Fatal("C crossed its point again and was not switched off")
	}
}

// TestAutoHookAndTickNeverDoubleSwitch: the usage hook and the roster tick
// race on one over-point serving account; exactly one switch happens.
func TestAutoHookAndTickNeverDoubleSwitch(t *testing.T) {
	r := newAutoRig(t, busyRow("A", 95, eligNow, 3*time.Hour))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				r.as.tick(eligNow)
			} else {
				r.as.onUsage("A", 200, http.Header{})
			}
		}()
	}
	wg.Wait()
	if got := r.notices(); len(got) != 1 || !strings.HasPrefix(got[0].title, "chottag: switched to ") {
		t.Fatalf("notices = %+v, want exactly one switch", got)
	}
	if n := countLines(r.log.String(), "chottag: auto-switch A -> "); n != 1 {
		t.Fatalf("log = %q, want one switch line", r.log.String())
	}
}

// TestAutoOffSwitchesNothing: `chottag auto off` takes effect on the next
// decision, without a restart.
func TestAutoOffSwitchesNothing(t *testing.T) {
	r := newAutoRig(t, busyRow("A", 99, eligNow, 3*time.Hour))
	r.update(func(st *store.State) { st.SetAutoEnabled(false) })
	r.as.tick(eligNow)
	if r.serving() != "A" || r.published().Decision != "off" || len(r.notices()) != 0 || r.log.String() != "" {
		t.Fatalf("serving %q decision %q log %q, want nothing done", r.serving(), r.published().Decision, r.log.String())
	}
}

// TestAutoSwitchesSilentlyWithNotificationsOff: notify off suppresses the
// notice, not the switch or its log line.
func TestAutoSwitchesSilentlyWithNotificationsOff(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	off := false
	r.update(func(st *store.State) { st.Notify = &off })
	r.as.tick(eligNow)
	if r.serving() != "B" || countLines(r.log.String(), "auto-switch A -> B") != 1 {
		t.Fatalf("serving %q log %q, want the switch made and logged", r.serving(), r.log.String())
	}
	if got := r.notices(); len(got) != 0 {
		t.Fatalf("notices = %+v with notify off", got)
	}
}

// TestAutoAllLimitedThenAResetSwitches (S8): every account limited posts
// "all accounts limited" and no no-candidate notice; when B's window
// resets, the tick switches to B and "switched to B" replaces "available
// again". The roster tick's order is the stamp closure's: auto first.
func TestAutoAllLimitedThenAResetSwitches(t *testing.T) {
	lim := func(name string, until time.Duration) status.Account {
		a := freshRow(name, 100, 50, eligNow)
		a.Limited, a.LimitedUntil, a.Window = true, eligNow.Add(until), "five_hour"
		return a
	}
	r := newAutoRig(t, lim("A", 3*time.Hour), lim("B", time.Hour), lim("C", 3*time.Hour), lim("D", 3*time.Hour))
	roster := func(at time.Time) {
		r.as.tick(at)
		r.dn.tick(r.state, r.sink, 0, at)
	}
	roster(eligNow)
	roster(eligNow.Add(time.Minute))
	at := eligNow.Add(90 * time.Minute)
	b := freshRow("B", 0, 50, at)
	r.row(b)
	roster(at)
	var titles []string
	for _, n := range r.notices() {
		titles = append(titles, n.title)
	}
	if strings.Join(titles, " | ") != "chottag: all accounts limited | chottag: switched to B" {
		t.Fatalf("notices = %q", titles)
	}
	if r.serving() != "B" {
		t.Fatalf("serving = %q, want B", r.serving())
	}
}

// TestAutoWallRetryDeferredSwitchEndsTheAllLimitedEpisodeAtOnce (S8, review
// round 1 item 1): a wall-retry switch made while every account is limited
// must end that episode the moment the switch itself happens, not when its
// own (deferred) notice fires — a roster tick landing in between must not
// see the episode still open and re-post "available again" a moment
// before "switched to B".
func TestAutoWallRetryDeferredSwitchEndsTheAllLimitedEpisodeAtOnce(t *testing.T) {
	lim := func(name string, until time.Duration) status.Account {
		a := freshRow(name, 100, 50, eligNow)
		a.Limited, a.LimitedUntil, a.Window = true, eligNow.Add(until), "five_hour"
		return a
	}
	r := newAutoRig(t, lim("A", 3*time.Hour), lim("B", time.Hour), lim("C", 3*time.Hour), lim("D", 3*time.Hour))
	roster := func(at time.Time) {
		r.as.tick(at)
		r.dn.tick(r.state, r.sink, 0, at)
	}
	roster(eligNow)
	roster(eligNow.Add(time.Minute))

	at := eligNow.Add(90 * time.Minute)
	r.row(freshRow("B", 0, 50, at))
	r.now = at
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(at.Add(time.Hour)))
	if !retry || done == nil {
		t.Fatal("A's limit 429 was not retried")
	}
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q, want B switched at once", got)
	}

	// A background roster tick lands before the resend's outcome is known.
	roster(at.Add(time.Second))

	done("B", 200)
	var titles []string
	for _, n := range r.notices() {
		titles = append(titles, n.title)
	}
	if strings.Join(titles, " | ") != "chottag: all accounts limited | chottag: switched to B" {
		t.Fatalf("notices = %q, want no \"available again\" between them", titles)
	}
}

// TestAutoLimitSwitchDoesNotStartACooldown (S7, review round 1 item 3): a
// cooldown gates only a threshold (soft) switch. A hard limit switch must
// not arm it, so a threshold crossing minutes later still switches.
func TestAutoLimitSwitchDoesNotStartACooldown(t *testing.T) {
	lim := busyRow("A", 100, eligNow, time.Hour)
	lim.Limited, lim.LimitedUntil, lim.Window = true, eligNow.Add(time.Hour), "five_hour"
	r := newAutoRig(t, append(onlyB(eligNow), lim)...)
	r.as.tick(eligNow)
	if r.serving() != "B" {
		t.Fatalf("setup: serving = %q, want B after A's limit", r.serving())
	}
	r.notices()

	at := eligNow.Add(5 * time.Minute)
	r.row(busyRow("B", 95, at, 3*time.Hour))
	r.row(busyRow("D", 10, at, 3*time.Hour))
	r.as.tick(at)
	if got := r.serving(); got != "D" {
		t.Fatalf("serving = %q, want B's threshold crossing to switch (A's limit never armed a cooldown)", got)
	}
}

// TestAutoSwitcherSeedsLastSwitchAndCooldownAcrossARestart (review round 1
// item 4): a new daemon generation reads the prior one's status.json
// auto.lastSwitch at construction, so a cooldown a soft switch started
// just before a restart still holds, and the first decision does not wipe
// it. Controller ruling 2 stands: the user-choice guard is not seeded.
func TestAutoSwitcherSeedsLastSwitchAndCooldownAcrossARestart(t *testing.T) {
	home, s := eligHome(t)
	writeEligStatus(t, home, freshRow("A", 10, 10, eligNow), freshRow("B", 10, 10, eligNow), freshRow("C", 10, 10, eligNow), freshRow("D", 10, 10, eligNow))
	priorAt := eligNow.Add(-5 * time.Minute)
	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	f.SetAuto(status.Auto{LastSwitch: &status.AutoSwitch{From: "A", To: "B", Trigger: autoswitch.TriggerThreshold, Window: "5h", Pct: 95, At: priorAt}})
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(func(st *store.State) error { st.Serving = "B"; return nil }); err != nil {
		t.Fatal(err)
	}

	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	cache := store.NewCache(s)
	dn := newDaemonNotify(cache.State, newRecordingNotifier())
	t.Cleanup(dn.Close)
	as := newAutoSwitcher(s, cache, sink, dn, newSyncBuf(), nil)
	as.now = func() time.Time { return eligNow }

	if as.lastSwitch == nil || as.lastSwitch.To != "B" || as.lastSoft.IsZero() {
		t.Fatalf("lastSwitch/lastSoft not seeded from status.json: lastSwitch=%+v lastSoft=%v", as.lastSwitch, as.lastSoft)
	}

	// B crosses its own 5h switch point now: the seeded cooldown (10m left
	// of the balanced preset's 15m) must hold it, not switch again.
	sink.mu.Lock()
	found := false
	for i := range sink.file.Accounts {
		if strings.EqualFold(sink.file.Accounts[i].Name, "B") {
			sink.file.Accounts[i] = busyRow("B", 95, eligNow, 3*time.Hour)
			found = true
		}
	}
	sink.mu.Unlock()
	if !found {
		t.Fatal("B has no cached row")
	}

	as.tick(eligNow)
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "B" {
		t.Fatalf("serving = %q, want B held by the seeded cooldown", st.Serving)
	}
	if d := as.published.Decision; !strings.Contains(d, "cooldown") {
		t.Fatalf("decision = %q, want the seeded cooldown to hold", d)
	}
}

// TestAutoSwapFailureLogsOnceAndBacksOff (review round 1 item 5): a
// SwapServing failure that is not a lost compare-and-swap (state.json
// itself unwritable, say) logs the failure once, words the decision "could
// not write state", and does not retry the write again until either the
// decision changes or a minute (the injected clock) has passed.
func TestAutoSwapFailureLogsOnceAndBacksOff(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	bad := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.as.store = store.Store{Dir: bad}

	r.as.tick(eligNow)
	if r.serving() != "A" {
		t.Fatalf("serving = %q, want A (the write failed)", r.serving())
	}
	if d := r.published().Decision; !strings.Contains(d, "could not write state") {
		t.Fatalf("decision = %q", d)
	}
	if n := countLines(r.log.String(), "chottag: auto-switch A -> B failed:"); n != 1 {
		t.Fatalf("log = %q, want exactly one failure line", r.log.String())
	}

	// Repair the store while still inside the 1-minute backoff: the very
	// next tick must skip the write rather than retry it early just
	// because the store happens to be fine again now — proving this is an
	// actual backoff on re-attempting the write, not only a log dedupe (a
	// mutant that always re-attempts but keeps deduping the log line would
	// otherwise pass this test on the log check alone).
	r.as.store = r.s
	r.as.tick(eligNow.Add(10 * time.Second))
	if got := r.serving(); got != "A" {
		t.Fatalf("serving = %q, want still A: the backoff must skip the write until it elapses, even once the store is writable again", got)
	}
	if n := countLines(r.log.String(), "chottag: auto-switch A -> B failed:"); n != 1 {
		t.Fatalf("log = %q, want the failure line logged only once inside the backoff", r.log.String())
	}

	// Once the backoff (1 minute, the injected clock) has elapsed, the
	// next tick tries again and now succeeds.
	r.as.tick(eligNow.Add(70 * time.Second))
	if r.serving() != "B" {
		t.Fatalf("serving = %q, want B once the backoff has elapsed and the store is writable again", r.serving())
	}
}

// TestAutoSwapFailureLogDedupeSurvivesTheBackoffElapsing is item 9(b)
// (review round 2 test gap): once the backoff elapses, the planner retries
// the write (TestAutoSwapFailureLogsOnceAndBacksOff pins that it retries),
// but if the store is STILL broken with the same error, the log dedupe
// must still suppress a repeat — it is keyed on the error's own message,
// not on the backoff window, so it must not reset just because a new
// attempt was made.
func TestAutoSwapFailureLogDedupeSurvivesTheBackoffElapsing(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	bad := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.as.store = store.Store{Dir: bad}

	r.as.tick(eligNow)
	if r.serving() != "A" {
		t.Fatalf("serving = %q, want A (the write failed)", r.serving())
	}
	if n := countLines(r.log.String(), "chottag: auto-switch A -> B failed:"); n != 1 {
		t.Fatalf("log = %q, want exactly one failure line", r.log.String())
	}

	// The backoff has now elapsed, and the store is STILL broken with the
	// same error: the retry fails again, but the dedupe must still
	// suppress a second log line.
	r.as.tick(eligNow.Add(70 * time.Second))
	if r.serving() != "A" {
		t.Fatalf("serving = %q, want still A: the store is still broken", r.serving())
	}
	if n := countLines(r.log.String(), "chottag: auto-switch A -> B failed:"); n != 1 {
		t.Fatalf("log = %q, want still exactly one failure line: the dedupe survives the backoff elapsing while the error is unchanged", r.log.String())
	}
}

// TestAutoSwitchLineNamesAFallback is item 7 (review round 3):
// autoSwitchLine's daemon.log line says "(fallback)" when Decision.Fallback
// is set, and says nothing extra when it is not.
func TestAutoSwitchLineNamesAFallback(t *testing.T) {
	d := autoswitch.Decision{From: "A", Target: "B", Trigger: autoswitch.TriggerLimit, Window: autoswitch.Win5h, Fallback: true}
	p := autoswitch.Preset(autoswitch.ModeCacheOptimize)
	if got, want := autoSwitchLine(d, p), "chottag: auto-switch A -> B (limit 5h, cache-optimize) (fallback)"; got != want {
		t.Fatalf("autoSwitchLine = %q, want %q", got, want)
	}
	d.Fallback = false
	if got, want := autoSwitchLine(d, p), "chottag: auto-switch A -> B (limit 5h, cache-optimize)"; got != want {
		t.Fatalf("autoSwitchLine = %q, want %q (no fallback suffix)", got, want)
	}
}

// limitHeader is a 429's unified headers: the 5-hour window rejected,
// resetting at reset.
func limitHeader(reset time.Time) http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(reset.Unix(), 10))
	return h
}

// TestAutoWallRetrySwitchesAndReportsTheResend (spec §4a, ruling 5, revised
// review round 1 item 2): the hook switches, and logs the switch line, at
// once; done() writes the wall-retry line, one notice that says the
// request went through, and lastSwitch.retried.
func TestAutoWallRetrySwitchesAndReportsTheResend(t *testing.T) {
	r := newAutoRig(t, onlyB(eligNow)...)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	if r.serving() != "B" {
		t.Fatalf("serving = %q before done, want B", r.serving())
	}
	if len(r.notices()) != 0 {
		t.Fatal("the notice went out before the resend's outcome was known")
	}
	if countLines(r.log.String(), "chottag: auto-switch A -> B (limit 5h") != 1 {
		t.Fatalf("log = %q, want the switch's own line at once, before done", r.log.String())
	}
	done("B", 200)
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to B" || got[0].body != "A hit its 5-hour limit; your request went to B." {
		t.Fatalf("notices = %+v", got)
	}
	if countLines(r.log.String(), "chottag: auto-switch A -> B (limit 5h") != 1 || countLines(r.log.String(), "chottag: wall retry A -> B 200 (limit 5h)") != 1 {
		t.Fatalf("log = %q, want the switch line and the wall retry line, once each", r.log.String())
	}
	if ls := r.published().LastSwitch; ls == nil || !ls.Retried || ls.Trigger != "limit" || ls.Window != "5h" {
		t.Fatalf("lastSwitch = %+v", ls)
	}
}

// TestAutoWallRetryFailedResendDoesNotMarkRetried is item 9(a) (review
// round 2 test gap): a resend that lands on the switch's own target but
// itself fails (a non-2xx status) must not mark lastSwitch.retried, and the
// notice must keep the resend hint rather than claiming the request went
// through.
func TestAutoWallRetryFailedResendDoesNotMarkRetried(t *testing.T) {
	r := newAutoRig(t, onlyB(eligNow)...)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	done("B", 500)
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to B" || got[0].body != "A hit its 5-hour limit. Resend your last message." {
		t.Fatalf("notices = %+v, want the resend hint: the resend itself failed with a 500", got)
	}
	if ls := r.published().LastSwitch; ls == nil || ls.Retried {
		t.Fatalf("lastSwitch = %+v, want Retried false: a 500 resend is not a success", ls)
	}
}

// TestAutoWallRetryRetargetedByAConcurrentTagReportsTheActualAccount
// (review round 1 item 2): a user `chottag tag C` lands after the wall
// retry's own switch (A -> B) but before the proxy's Choose resends, so the
// resend actually lands on C, not B. The notice and lastSwitch must not
// claim B: they must report what actually served the request.
func TestAutoWallRetryRetargetedByAConcurrentTagReportsTheActualAccount(t *testing.T) {
	r := newAutoRig(t, onlyB(eligNow)...)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	if r.serving() != "B" {
		t.Fatalf("serving = %q before done, want B", r.serving())
	}
	// A user `chottag tag C` lands before the resend's outcome is known.
	r.update(func(st *store.State) { st.Serving = "C" })

	done("C", 200)
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to C" || got[0].body != "A hit its 5-hour limit; your request went to C." {
		t.Fatalf("notices = %+v, want the notice to name the account that actually served it", got)
	}
	if countLines(r.log.String(), "chottag: auto-switch A -> B (limit 5h") != 1 {
		t.Fatalf("log = %q, want this switcher's own A -> B line", r.log.String())
	}
	if countLines(r.log.String(), "chottag: wall retry A -> C 200 (limit 5h)") != 1 {
		t.Fatalf("log = %q, want the wall retry line naming the account that actually served it", r.log.String())
	}
	if ls := r.published().LastSwitch; ls == nil || ls.To != "B" || ls.Retried {
		t.Fatalf("lastSwitch = %+v, want this switcher's own A -> B record left unmarked", ls)
	}
}

// fallbackRig seeds B just over its own switch point (95%, max5x's is
// 93%) but well below the wall, and C and D each genuinely at their own
// wall (100%) — not even fallback-eligible — so a LIMIT trigger on A finds
// no ordinary candidate and falls back to B, the only account that still
// has any capacity at all, deterministically (item 1, review round 3).
func fallbackRig(t *testing.T) *autoRig {
	t.Helper()
	return newAutoRig(t,
		busyRow("B", 95, eligNow, 4*time.Hour),
		busyRow("C", 100, eligNow, 4*time.Hour),
		busyRow("D", 100, eligNow, 4*time.Hour))
}

// TestAutoFallbackTargetSilencesTheThresholdReEvaluationBeforeDone is item
// 1 (review round 3): the reviewer's own repro. A hard limit on A falls
// back to B, which is itself over its own (softer) switch point. Before
// the wall retry's deferred switch notice fires, the resend's own usage
// hook lands on B — a plain THRESHOLD re-evaluation that finds no
// candidate either, since C and D are still the same accounts B's own
// fallback already tolerated. Without the fallback-target memory this
// posts a contradictory "no account to switch to" notice moments before
// "switched to B"; it must stay silent instead.
func TestAutoFallbackTargetSilencesTheThresholdReEvaluationBeforeDone(t *testing.T) {
	r := fallbackRig(t)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	if r.serving() != "B" {
		t.Fatalf("serving = %q before done, want B (the fallback target)", r.serving())
	}
	if len(r.notices()) != 0 {
		t.Fatal("the deferred switch notice must not have fired yet")
	}

	// The resend landed on B; its usage hook runs synchronously, before
	// done() — the reviewer's own interleaving (wallRetry(A), onUsage(B,
	// 95%), done(B, 200)).
	r.as.onUsage("B", 200, http.Header{})
	if got := r.notices(); len(got) != 0 {
		t.Fatalf("notices = %+v, want none: B's own threshold re-evaluation must stay silent while it is the fallback target", got)
	}

	done("B", 200)
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to B" {
		t.Fatalf("notices = %+v, want exactly the switch notice, not a contradictory no-candidate one first", got)
	}
	if countLines(r.log.String(), "chottag: auto-switch A -> B (limit 5h") != 1 || !strings.Contains(r.log.String(), "(fallback)") {
		t.Fatalf("log = %q, want the switch's own line naming the fallback (item 7)", r.log.String())
	}
}

// TestAutoFallbackTargetSilencesARosterTickBeforeDone is the tick-path
// half of the same interleaving: a 5s roster tick, not the usage hook,
// lands between the wall retry's own switch and its deferred notice.
func TestAutoFallbackTargetSilencesARosterTickBeforeDone(t *testing.T) {
	r := fallbackRig(t)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	if r.serving() != "B" {
		t.Fatalf("serving = %q before done, want B (the fallback target)", r.serving())
	}

	r.as.tick(eligNow.Add(time.Second))
	if got := r.notices(); len(got) != 0 {
		t.Fatalf("notices = %+v, want none: the tick's threshold re-evaluation must stay silent while B is the fallback target", got)
	}

	done("B", 200)
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: switched to B" {
		t.Fatalf("notices = %+v, want exactly the switch notice", got)
	}
}

// TestAutoFallbackTargetClearsWhenServingDropsBelowEveryPointThenReCrossingNotifies
// is item 1 (review round 4): fallbackTarget must not outlive the condition
// it records. The reviewer's scenario — fallback A->B at 95%, B's window
// resets (B drops below every point), B crosses 95% again later while C and
// D are still exactly as over their own points as before — must post the
// no-candidate notice on that later crossing; before this fix the flag only
// ever cleared on a serving change, so it silenced every re-evaluation
// forever, and the notice never came.
func TestAutoFallbackTargetClearsWhenServingDropsBelowEveryPointThenReCrossingNotifies(t *testing.T) {
	r := fallbackRig(t)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	done("B", 200)
	r.notices() // drain the switch notice

	// B's 5h window resets: B drops below every point. C and D are still at
	// their wall (their own reset, from fallbackRig's setup, is 4h out from
	// eligNow — comfortably after everything below). Nothing forces a move
	// (a THRESHOLD trigger never fires off an account already below every
	// point), so this must not itself notify.
	at := eligNow.Add(30 * time.Minute)
	r.row(busyRow("B", 10, at, 3*time.Hour))
	r.as.tick(at)
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q, want B kept (nothing forcing a move)", got)
	}
	if got := r.notices(); len(got) != 0 {
		t.Fatalf("notices = %+v, want none: dropping below every point is not itself news", got)
	}

	// B crosses its own point again. C and D are unchanged from the
	// fallback switch above — the exact same "every other account is over
	// its own point" shape — so this is a fresh no-candidate, not a
	// continuation of the one the fallback already silenced.
	at = at.Add(time.Minute)
	r.row(busyRow("B", 95, at, 3*time.Hour))
	r.as.tick(at)
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q, want B (still nothing else to switch to)", got)
	}
	got := r.notices()
	if len(got) != 1 || got[0].title != "chottag: no account to switch to" {
		t.Fatalf("notices = %+v, want the no-candidate notice: the fallback flag must have cleared", got)
	}
}

// TestAutoFallbackTargetClearsWhenServingChangesAwayFromIt is item 3a
// (review round 4): fallbackTarget must clear the moment serving changes to
// something other than the fallback target itself, whether that is a user
// `tag` or a switcher's own attempted switch losing its compare-and-swap to
// one — both land here through the identical "serving != known" branch
// (S10, the same one userChosen is recomputed in), since a CAS loss's own
// handling in trySwitch never touches fallbackTarget itself; only the next
// decision, seeing the real current serving account, does.
func TestAutoFallbackTargetClearsWhenServingChangesAwayFromIt(t *testing.T) {
	r := fallbackRig(t)
	retry, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	if !retry || done == nil {
		t.Fatal("a limit 429 on serving A was not retried")
	}
	done("B", 200)
	r.notices()
	if !r.fallbackTarget() {
		t.Fatal("setup: B should be the fallback target")
	}

	r.update(func(st *store.State) { st.Serving = "C" }) // a user `tag C`
	r.as.tick(eligNow.Add(time.Minute))
	if got := r.serving(); got != "C" {
		t.Fatalf("serving = %q, want the user's C kept", got)
	}
	if r.fallbackTarget() {
		t.Fatal("fallbackTarget outlived serving changing away from B")
	}
}

// TestAutoFallbackTargetIsFalseAfterAnOrdinarySwitch is item 3b (review
// round 4): the flag records that THIS switch used the fallback, not that
// any switch happened at all. An ordinary threshold switch — A crosses its
// point, B is genuinely below its own — must leave fallbackTarget false.
func TestAutoFallbackTargetIsFalseAfterAnOrdinarySwitch(t *testing.T) {
	r := newAutoRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	r.as.tick(eligNow.Add(time.Minute))
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q, want B", got)
	}
	if r.fallbackTarget() {
		t.Fatal("fallbackTarget = true after an ordinary (non-fallback) switch")
	}
}

// TestAutoWallRetryNotResentSaysResend: no resend (to "") logs the switch
// line and the notice asks the user to resend.
func TestAutoWallRetryNotResentSaysResend(t *testing.T) {
	r := newAutoRig(t, onlyB(eligNow)...)
	_, done := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(2*time.Hour)))
	done("", 0)
	got := r.notices()
	if len(got) != 1 || got[0].body != "A hit its 5-hour limit. Resend your last message." {
		t.Fatalf("notices = %+v", got)
	}
	if countLines(r.log.String(), "chottag: auto-switch A -> B (limit 5h") != 1 || strings.Contains(r.log.String(), "wall retry") {
		t.Fatalf("log = %q", r.log.String())
	}
	if ls := r.published().LastSwitch; ls == nil || ls.Retried {
		t.Fatalf("lastSwitch = %+v, want not retried", ls)
	}
}

// TestAutoWallRetryDeclines: a 429 that is not a limit, or auto off, is
// never retried.
func TestAutoWallRetryDeclines(t *testing.T) {
	r := newAutoRig(t)
	if retry, _ := r.as.wallRetry(context.Background(), "A", http.Header{}); retry {
		t.Fatal("a 429 with no unified headers was retried")
	}
	r.update(func(st *store.State) { st.SetAutoEnabled(false) })
	if retry, _ := r.as.wallRetry(context.Background(), "A", limitHeader(eligNow.Add(time.Hour))); retry {
		t.Fatal("retried with auto-switch off")
	}
	if r.serving() != "A" || len(r.notices()) != 0 {
		t.Fatal("a declined wall retry switched or posted")
	}
}

// TestAutoBurnRateFromTheServingAccount: the usage hook feeds the serving
// account's 5h rise into the burn average; 2 points a minute on a max5x
// account is 5 x 120 = 600 units an hour.
func TestAutoBurnRateFromTheServingAccount(t *testing.T) {
	r := newAutoRig(t)
	for i := 0; i <= 20; i++ {
		r.now = eligNow.Add(time.Duration(i) * time.Minute)
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", strconv.FormatFloat(float64(10+2*i)/100, 'f', 2, 64))
		r.as.onUsage("A", 200, h)
		r.as.onUsage("B", 200, h) // not serving: ignored
	}
	if got := r.published().BurnRate; got < 599 || got > 601 {
		t.Fatalf("burn = %v, want 600", got)
	}
}

// TestAutoUsageHookClearsNeedsLoginOnSuccess (ruling 7): a 2xx on the
// account's own credential clears a recorded needs-login; a 401 does not.
func TestAutoUsageHookClearsNeedsLoginOnSuccess(t *testing.T) {
	r := newAutoRig(t)
	b := freshRow("B", 10, 10, eligNow)
	b.Token = creds.StateNeedsLogin
	r.row(b)
	token := func() creds.TokenState {
		f := r.sink.fileCopy()
		for _, a := range f.Accounts {
			if a.Name == "B" {
				return a.Token
			}
		}
		return ""
	}
	r.as.onUsage("B", 401, http.Header{})
	if token() != creds.StateNeedsLogin {
		t.Fatal("a 401 cleared needs-login")
	}
	r.as.onUsage("B", 200, http.Header{})
	if token() == creds.StateNeedsLogin {
		t.Fatal("a 2xx left needs-login")
	}
}

// TestRunProxyWiresAutoSwitch: the real daemon's startup tick switches off
// an over-point serving account (the Auto field and the stamp closure's
// d.Auto.tick are wired).
func TestRunProxyWiresAutoSwitch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")
	addSlotAccount(t, home, "B")
	now := time.Now()
	writeNotifyStatus(t, home, busyRow("A", 97, now, 3*time.Hour), freshRow("B", 10, 10, now))
	rec := stubDaemonNotifier(t)
	// The pre-swap guard reads the target's token state (F269): B has a
	// usable login.
	prevRead := credsReadForTest
	t.Cleanup(func() { credsReadForTest = prevRead })
	credsReadForTest = func(configDir string) (creds.Token, error) {
		return creds.Token{AccessToken: "tok-" + filepath.Base(configDir), ExpiresAt: time.Now().Add(time.Hour)}, nil
	}

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})
	select {
	case m := <-rec.got:
		if m.title != "chottag: switched to B" {
			t.Fatalf("first notice = %+v, want the switch to B", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the daemon's startup tick never switched; stderr %q", errb.String())
	}
	st, err := (store.Store{Dir: home}).Load()
	if err != nil || st.Serving != "B" {
		t.Fatalf("serving = %q (%v), want B", st.Serving, err)
	}
}

// A rename of the serving account is not the user's choice (F171 review):
// the slot, and so the account, is the same. With serving over its point
// and nowhere to go, the rename must not set the guard that would stop the
// switch once somewhere to go appears.
func TestAutoTreatsARenameOfTheServingAccountAsTheSameAccount(t *testing.T) {
	r := newAutoRig(t, busyRow("A", 95, eligNow, 3*time.Hour), busyRow("B", 95, eligNow, 3*time.Hour),
		busyRow("C", 95, eligNow, 3*time.Hour), busyRow("D", 99.5, eligNow, 3*time.Hour))
	r.as.tick(eligNow) // learns A; no candidate, so it stays
	if got := r.serving(); got != "A" {
		t.Fatalf("serving = %q, want A (no candidate)", got)
	}
	r.update(func(st *store.State) {
		st.Accounts[0].Name = "work"
		st.Serving = "work"
	})
	r.row(busyRow("work", 95, eligNow.Add(time.Minute), 3*time.Hour))
	r.as.tick(eligNow.Add(time.Minute))
	if p := r.published(); p.UserChosen {
		t.Fatalf("auto %+v: a rename of the serving account read as the user's choice", p)
	}
	at := eligNow.Add(2 * time.Minute)
	r.row(busyRow("work", 95, at, 3*time.Hour))
	r.row(freshRow("B", 10, 10, at))
	r.as.tick(at)
	if got := r.serving(); got != "B" {
		t.Fatalf("serving = %q, want the switch off work to B once B is free", got)
	}
}
