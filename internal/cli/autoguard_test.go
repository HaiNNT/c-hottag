package cli

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
)

// guardRig is an autoRig whose switcher has the pre-swap guard's seams
// replaced by fakes: no Keychain, no claude, no network, no real timers.
type guardRig struct {
	*autoRig
	mu     sync.Mutex
	tokens map[string]creds.TokenState // by account name; absent is ok
	warmed chan string                 // account names warmFor was asked for
	polled chan string                 // account names pollNow was asked for
	pollFn func(account string) bool
	warmFn func(account string)
	bound  chan time.Time // what a.after returns; closed means the bound elapsed
	// awaitFn is what the manager's Await does for an account; bounds are
	// the waits a.after was asked for, in order.
	awaitFn func(account string)
	bounds  []time.Duration
}

func newGuardRig(t *testing.T, rows ...status.Account) *guardRig {
	t.Helper()
	g := &guardRig{
		autoRig: newAutoRig(t, rows...),
		tokens:  map[string]creds.TokenState{},
		warmed:  make(chan string, 16),
		polled:  make(chan string, 16),
		bound:   make(chan time.Time),
	}
	g.as.tokenStatus = func(dir string) creds.Status {
		g.mu.Lock()
		defer g.mu.Unlock()
		if st, ok := g.tokens[filepath.Base(dir)]; ok {
			return creds.Status{State: st}
		}
		return creds.Status{State: creds.StateOK}
	}
	g.as.warmFor = func(_ context.Context, dir string, _ time.Duration, _ string) (creds.Status, bool) {
		name := filepath.Base(dir)
		g.warmed <- name
		g.mu.Lock()
		fn := g.warmFn
		g.mu.Unlock()
		if fn != nil {
			fn(name)
		}
		return creds.Status{State: creds.StateOK}, true
	}
	g.as.pollNow = func(_ context.Context, account string) bool {
		g.polled <- account
		g.mu.Lock()
		fn := g.pollFn
		g.mu.Unlock()
		return fn != nil && fn(account)
	}
	g.as.after = func(d time.Duration) <-chan time.Time {
		g.mu.Lock()
		g.bounds = append(g.bounds, d)
		g.mu.Unlock()
		return g.bound
	}
	g.as.awaitToken = func(_ context.Context, dir string) {
		name := filepath.Base(dir)
		g.mu.Lock()
		fn := g.awaitFn
		g.mu.Unlock()
		if fn != nil {
			fn(name)
		}
	}
	// Background work runs inline, so "nothing was started" is a plain read.
	g.as.spawn = func(f func()) { f() }
	return g
}

func (g *guardRig) setToken(account string, st creds.TokenState) {
	g.mu.Lock()
	g.tokens[account] = st
	g.mu.Unlock()
}

func (g *guardRig) tokenState(account string) creds.TokenState {
	for _, a := range g.sink.fileCopy().Accounts {
		if a.Name == account {
			return a.Token
		}
	}
	return ""
}

func recv(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(10 * time.Second):
		t.Fatalf("never saw %s", what)
		return ""
	}
}

// F269: a target whose slot holds no login is never swapped to. It is
// recorded and announced once, and the plan is made again without it.
func TestGuardNeverSwapsToANeedsLoginTarget(t *testing.T) {
	// C (pro) is eligible too, so the switch goes to C instead of B.
	g := newGuardRig(t, busyRow("A", 95, eligNow, 3*time.Hour), busyRow("D", 99.5, eligNow, 3*time.Hour))
	g.setToken("B", creds.StateNeedsLogin)
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := g.serving(); got != "C" {
		t.Fatalf("serving = %q, want C: B has no login", got)
	}
	if st := g.tokenState("B"); st != creds.StateNeedsLogin {
		t.Fatalf("B's token state = %q, want needs-login recorded", st)
	}
	var login, switched int
	for _, n := range g.notices() {
		switch {
		case strings.Contains(n.body, "chottag login B"):
			login++
		case n.title == "chottag: switched to C":
			switched++
		}
	}
	if login != 1 || switched != 1 {
		t.Fatalf("needs-login notices %d, switch notices %d, want 1 and 1", login, switched)
	}
	if !strings.Contains(g.log.String(), "not switching to B: it needs login") {
		t.Fatalf("log = %q", g.log.String())
	}
}

func TestGuardNeedsLoginWithNoOtherCandidateStaysAndNeverSwaps(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	g.setToken("B", creds.StateNeedsLogin)
	for i := 1; i <= 3; i++ {
		g.now = eligNow.Add(time.Duration(i) * time.Minute)
		g.as.tick(g.now)
	}
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A: the only candidate has no login", got)
	}
	if n := countLines(g.log.String(), "chottag: auto-switch"); n != 0 {
		t.Fatalf("log = %q, want no switch", g.log.String())
	}
}

// A stale token on a threshold switch: the refresh is started and the switch
// waits for the next evaluation; it does not start one per tick.
func TestGuardDefersAThresholdSwitchOntoAStaleTokenAndWarmsOnce(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	g.setToken("B", creds.StateStale)
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := recv(t, g.warmed, "the warm of B"); got != "B" {
		t.Fatalf("warmed %q, want B", got)
	}
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A while B's token refreshes", got)
	}
	if d := g.published().Decision; !strings.Contains(d, "switch to B deferred: its token is refreshing") {
		t.Fatalf("decision = %q", d)
	}
	g.as.tick(g.now.Add(time.Minute)) // within guardRetryEvery: no second warm
	if n := len(g.warmed); n != 0 {   // background ran inline: the count is exact
		t.Fatalf("%d more warms started within %v", n, guardRetryEvery)
	}
	// The refresh renewed it: the next evaluation switches.
	g.setToken("B", creds.StateOK)
	g.as.tick(g.now.Add(2 * time.Minute))
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B once its token is usable", got)
	}
}

func limitedA(at time.Time) status.Account {
	a := busyRow("A", 100, at, 3*time.Hour)
	a.Limited, a.LimitedUntil, a.Window = true, at.Add(3*time.Hour), "five_hour"
	return a
}

// A LIMIT switch waits for the stale target's refresh (bounded) and then
// goes ahead.
func TestGuardLimitSwitchWaitsForTheStaleTokenToRefresh(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow))...)
	g.setToken("B", creds.StateStale)
	g.awaitFn = func(account string) { g.setToken(account, creds.StateOK) }
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B after its token refreshed", got)
	}
	if len(g.warmed) != 0 {
		t.Fatal("a LIMIT switch started a background warm instead of waiting (Await)")
	}
}

// When the bound passes before the token refreshed, the next candidate is
// taken, never a target that cannot be used.
func TestGuardLimitSwitchTakesTheNextCandidateWhenTheRefreshIsTooSlow(t *testing.T) {
	g := newGuardRig(t, limitedA(eligNow), busyRow("D", 99.5, eligNow, 3*time.Hour))
	g.setToken("B", creds.StateStale)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	g.awaitFn = func(string) { <-release } // B's refresh never finishes in time
	close(g.bound)                         // the refresh bound has elapsed
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	if got := g.serving(); got != "C" {
		t.Fatalf("serving = %q, want C: B's token did not refresh within the bound", got)
	}
	if !strings.Contains(g.log.String(), "B's token did not renew in time; taking the next candidate") {
		t.Fatalf("log = %q", g.log.String())
	}
}

func oldRow(name string, pct float64, at time.Time) status.Account {
	return freshRow(name, pct, 10, at.Add(-2*time.Hour))
}

// F268: an old target is polled before a threshold switch; the switch waits
// for the next evaluation and does not poll on every tick.
func TestGuardDefersAThresholdSwitchOntoOldUsageAndPollsOnce(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour), oldRow("B", 10, eligNow))...)
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := recv(t, g.polled, "the poll of B"); got != "B" {
		t.Fatalf("polled %q, want B", got)
	}
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A until B's usage is current", got)
	}
	g.as.tick(g.now.Add(time.Minute))
	if n := len(g.polled); n != 0 {
		t.Fatalf("%d more polls started within %v", n, guardRetryEvery)
	}
	// The poll landed: B's reading is fresh now.
	g.row(freshRow("B", 10, 10, g.now.Add(2*time.Minute)))
	g.as.tick(g.now.Add(2 * time.Minute))
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B once its reading is current", got)
	}
}

// A LIMIT switch polls the old target (bounded), plans again on the new
// reading and swaps. A current candidate ranks first, so an old one is not
// even polled while another can serve.
func TestGuardLimitSwitchPollsAnOldTargetThenSwaps(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow), oldRow("B", 10, eligNow))...)
	g.pollFn = func(account string) bool {
		g.row(freshRow(account, 12, 10, g.now))
		return true
	}
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B after its poll", got)
	}
	if got := recv(t, g.polled, "the poll of B"); got != "B" {
		t.Fatalf("polled %q, want B", got)
	}
	if n := len(g.polled); n != 0 {
		t.Fatalf("%d polls beyond the one", n)
	}
}

func TestGuardRanksACurrentCandidateAheadOfAnOldOneWithoutPolling(t *testing.T) {
	g := newGuardRig(t, limitedA(eligNow), busyRow("D", 99.5, eligNow, 3*time.Hour), oldRow("B", 10, eligNow))
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := g.serving(); got != "C" {
		t.Fatalf("serving = %q, want the current candidate C ahead of the old B", got)
	}
	if n := len(g.polled); n != 0 {
		t.Fatalf("%d polls although a current candidate could serve", n)
	}
}

// A poll that does not finish in time never strands a LIMIT switch: it goes
// ahead on the old reading, as before.
func TestGuardLimitSwitchGoesAheadWhenThePollFails(t *testing.T) {
	g := newGuardRig(t, limitedA(eligNow), busyRow("D", 99.5, eligNow, 3*time.Hour), busyRow("C", 95, eligNow, 3*time.Hour), oldRow("B", 10, eligNow))
	g.pollFn = func(string) bool { return false }
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B on its old reading", got)
	}
	if !strings.Contains(g.log.String(), "usage poll of B failed; switching on its old reading") {
		t.Fatalf("log = %q", g.log.String())
	}
}

// A recent reading is trusted: no poll.
func TestGuardDoesNotPollARecentTarget(t *testing.T) {
	recent := freshRow("B", 10, 10, eligNow.Add(-30*time.Minute)) // not fresh, still recent
	g := newGuardRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour), recent)...)
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B", got)
	}
	if n := len(g.polled); n != 0 {
		t.Fatalf("%d polls although its reading is recent", n)
	}
}

// H1: a LIMIT guard that is mid-wait holds no lock: a response for the
// serving account is delivered (onUsage) without waiting for it, and that
// delivery starts the refresh in the background instead of waiting itself.
func TestGuardMidWaitDoesNotBlockResponseDelivery(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow))...)
	g.setToken("B", creds.StateStale)
	entered, release := make(chan struct{}), make(chan struct{})
	g.awaitFn = func(account string) {
		close(entered)
		<-release
		g.setToken(account, creds.StateOK)
	}
	g.now = eligNow.Add(time.Minute)
	switched := make(chan struct{})
	go func() {
		g.as.evaluateFor(context.Background(), g.now, true, "default", true)
		close(switched)
	}()
	<-entered // the guard is waiting for B's token
	delivered := make(chan struct{})
	go func() {
		g.as.onUsage("A", http.StatusOK, http.Header{})
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("onUsage blocked behind a guard that is waiting")
	}
	if got := recv(t, g.warmed, "the response path's own background warm"); got != "B" {
		t.Fatalf("warmed %q, want B", got)
	}
	close(release)
	<-switched
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B once the wait ended", got)
	}
}

// H1: the LIMIT waits share one budget, whatever the number of candidates:
// 15 s for the first stale token, what is left (10 s) for the second.
func TestGuardLimitWaitsShareOneBudgetAcrossCandidates(t *testing.T) {
	g := newGuardRig(t, limitedA(eligNow), busyRow("D", 99.5, eligNow, 3*time.Hour))
	g.setToken("B", creds.StateStale)
	g.setToken("C", creds.StateStale)
	g.setToken("D", creds.StateNeedsLogin) // not even the fallback can take it
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	g.awaitFn = func(string) { <-release }
	close(g.bound)
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	g.mu.Lock()
	bounds := append([]time.Duration(nil), g.bounds...)
	g.mu.Unlock()
	if len(bounds) != 2 || bounds[0] != refreshWaitBound || bounds[1] != preSwitchPollBound {
		t.Fatalf("waited %v, want [%v %v]: 25 s in all", bounds, refreshWaitBound, preSwitchPollBound)
	}
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A: no candidate's token renewed", got)
	}
}

// The swap checks the target again: a login that vanished after the guard's
// check is not swapped onto.
func TestSwapRechecksTheTargetsLogin(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour))...)
	var mu sync.Mutex
	calls := 0
	g.as.tokenStatus = func(string) creds.Status {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return creds.Status{State: creds.StateOK} // the guard's read
		}
		return creds.Status{State: creds.StateNeedsLogin} // the swap's read
	}
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A: B lost its login before the swap", got)
	}
	if st := g.tokenState("B"); st != creds.StateNeedsLogin {
		t.Fatalf("B's token state = %q, want needs-login recorded", st)
	}
}

// N1: only the wall retry's own pool may wait; any other pool in the same
// evaluation defers and refreshes in the background.
func TestGuardOnlyTheWallRetrysPoolMayWait(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow))...)
	g.setToken("D", creds.StateNeedsLogin) // not even the fallback can serve
	g.setToken("C", creds.StateNeedsLogin)
	g.setToken("B", creds.StateStale)
	var awaited int
	g.awaitFn = func(string) { awaited++ }
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "some-other-pool", true)
	if awaited != 0 {
		t.Fatal("a pool other than the wall retry's waited")
	}
	if got := recv(t, g.warmed, "the background warm"); got != "B" {
		t.Fatalf("warmed %q, want B", got)
	}
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A while B refreshes", got)
	}
}

// N2: a LIMIT that only the tick sees takes the next candidate whose token
// and data are fine now, and starts the first one's refresh meanwhile.
func TestGuardTickLimitTakesTheNextGoodCandidate(t *testing.T) {
	g := newGuardRig(t, limitedA(eligNow), busyRow("D", 99.5, eligNow, 3*time.Hour), busyRow("C", 80, eligNow, 3*time.Hour))
	g.setToken("B", creds.StateStale)
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := recv(t, g.warmed, "B's background warm"); got != "B" {
		t.Fatalf("warmed %q, want B (the planner's first pick)", got)
	}
	if got := g.serving(); got != "C" {
		t.Fatalf("serving = %q, want C: B was deferred, C is fine now", got)
	}
}

func TestGuardTickLimitKeepsDeferringWhenNoCandidateIsGoodNow(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow))...)
	g.setToken("D", creds.StateNeedsLogin) // not even the fallback can serve
	g.setToken("C", creds.StateNeedsLogin)
	g.setToken("B", creds.StateStale)
	g.now = eligNow.Add(time.Minute)
	g.as.tick(g.now)
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A", got)
	}
	if d := g.published().Decision; !strings.Contains(d, "switch to B deferred") {
		t.Fatalf("decision = %q, want B's deferral kept; log %q", d, g.log.String())
	}
}

// N3: a poll that is not made because of a backoff is reported as such.
func TestGuardSaysWhyAPollIsSkipped(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow), oldRow("B", 10, eligNow))...)
	g.as.pollSkip = func(string) string { return "in a 429 backoff" }
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	if got := g.serving(); got != "B" {
		t.Fatalf("serving = %q, want B on its old reading", got)
	}
	log := g.log.String()
	if !strings.Contains(log, "not polled now (in a 429 backoff)") || strings.Contains(log, "failed") || len(g.polled) != 0 {
		t.Fatalf("log = %q, polls = %d; want the reason and no poll", log, len(g.polled))
	}
	// A threshold deferral names it too.
	g2 := newGuardRig(t, append(onlyB(eligNow), busyRow("A", 95, eligNow, 3*time.Hour), oldRow("B", 10, eligNow))...)
	g2.as.pollSkip = func(string) string { return "in a 429 backoff" }
	g2.now = eligNow.Add(time.Minute)
	g2.as.tick(g2.now)
	if d := g2.published().Decision; !strings.Contains(d, "not polled now (in a 429 backoff)") || len(g2.polled) != 0 {
		t.Fatalf("decision = %q, polls = %d", d, len(g2.polled))
	}
}

// N4: a request that hangs up ends the waits.
func TestGuardWaitEndsWhenTheRequestIsGone(t *testing.T) {
	g := newGuardRig(t, append(onlyB(eligNow), limitedA(eligNow))...)
	g.setToken("D", creds.StateNeedsLogin) // not even the fallback can serve
	g.setToken("C", creds.StateNeedsLogin)
	g.setToken("B", creds.StateStale)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	g.awaitFn = func(string) { <-release }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(ctx, g.now, true, "default", true) // returns: the bound channel never fires
	if got := g.serving(); got != "A" {
		t.Fatalf("serving = %q, want A", got)
	}
}

// N4: several candidates that each need a wait still end in a decision.
func TestGuardSeveralCandidatesNeedingWaitsStillDecide(t *testing.T) {
	g := newGuardRig(t, limitedA(eligNow), oldRow("D", 10, eligNow))
	g.setToken("B", creds.StateStale) // its refresh returns at once without renewing
	g.setToken("C", creds.StateStale)
	var awaited []string
	g.awaitFn = func(a string) { awaited = append(awaited, a) }
	g.pollFn = func(account string) bool {
		g.row(freshRow(account, 10, 10, g.now))
		return true
	}
	g.now = eligNow.Add(time.Minute)
	g.as.evaluateFor(context.Background(), g.now, true, "default", true)
	if got := g.serving(); got != "D" {
		t.Fatalf("serving = %q, want D after B and C failed to renew and D's poll landed (waits %v)", got, awaited)
	}
	if len(awaited) != 2 {
		t.Fatalf("token waits %v, want B and C once each", awaited)
	}
}
