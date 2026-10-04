package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

func staleRemoteState() store.State {
	return store.State{
		Serving: "S", Remote: "A",
		Accounts: []store.Account{{Name: "S", Dir: "/slots/S"}, {Name: "A", Dir: "/slots/A"}},
	}
}

// R147: with only default, a remote request whose account has no usable
// token is refused with a message naming the account and the fix.
func TestChooserRefusesARemoteRequestWithAStaleTokenEvenWithOnlyDefault(t *testing.T) {
	st := staleRemoteState()
	stateFn := func() (store.State, error) { return st, nil }
	sel := selector.New(selector.Config{State: stateFn, Tokens: fakeTokens{st: creds.Status{State: creds.StateStale}}, Owners: fakeOwners{}})
	c := &chooser{sel: sel, state: stateFn}

	acct, tok, _, ok, refusal := c.ChooseGuarded(context.Background(), router.Decision{Class: router.Remote}, "")
	want := "chottag: account A (remote) has no usable login right now; run: chottag login A"
	if acct != "" || tok != "" || ok || refusal != want {
		t.Fatalf("got %q %q ok=%v refusal=%q, want the refusal %q", acct, tok, ok, refusal, want)
	}
	// A serving request is unchanged: the plain passthrough, no refusal.
	acct, _, _, ok, refusal2 := c.ChooseGuarded(context.Background(), router.Decision{Class: router.Serving}, "")
	if acct != "" || ok || refusal2 != "" {
		t.Fatalf("serving: got %q ok=%v refusal=%q, want today's passthrough", acct, ok, refusal2)
	}
}

func TestChooserRefusesAnOwnerRequestWithAStaleOwner(t *testing.T) {
	st := staleRemoteState()
	stateFn := func() (store.State, error) { return st, nil }
	sel := selector.New(selector.Config{State: stateFn, Tokens: fakeTokens{st: creds.Status{State: creds.StateStale}}, Owners: fakeOwners{owner: "S"}})
	c := &chooser{sel: sel, state: stateFn}
	d := router.Decision{Class: router.Remote, Object: router.KindConnector, ObjectID: "c1"}
	_, _, _, ok, refusal := c.ChooseGuarded(context.Background(), d, "")
	want := "chottag: account S (owner) has no usable login right now; run: chottag login S"
	if ok || refusal != want {
		t.Fatalf("ok=%v refusal=%q, want %q", ok, refusal, want)
	}
}

// The event lines: one per account per minute, each its own text.
func TestEventPrinterWordsAndRateLimits(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	buf := newSyncBuf()
	p := newEventPrinter(buf, func() time.Time { return now })
	stale := creds.Status{State: creds.StateStale}

	serving := selector.Event{Kind: "passthrough", Account: "A", Status: stale, Role: selector.RoleServing}
	remote := selector.Event{Kind: "passthrough", Account: "A", Status: stale, Role: selector.RoleRemote, Refused: true}
	p(serving)
	p(serving)
	p(remote)
	p(remote)
	got := buf.String()
	if strings.Count(got, "chottag: sent on Home's own login: A's token is stale (serving)\n") != 1 {
		t.Fatalf("serving line missing or repeated: %q", got)
	}
	if strings.Count(got, "chottag: refused a remote request: A's token is stale; run `chottag login A` if it persists\n") != 1 {
		t.Fatalf("remote line missing or repeated: %q", got)
	}
	// another account is not rate-limited by A's line
	p(selector.Event{Kind: "passthrough", Account: "B", Status: stale, Role: selector.RoleServing})
	if !strings.Contains(buf.String(), "B's token is stale (serving)") {
		t.Fatalf("B's line missing: %q", buf.String())
	}
	now = now.Add(time.Minute)
	p(serving)
	if strings.Count(buf.String(), "A's token is stale (serving)") != 2 {
		t.Fatalf("serving line not repeated after a minute: %q", buf.String())
	}
}

// Events that are not a stale-token passthrough keep their plain line.
func TestEventPrinterKeepsThePlainLineForOtherEvents(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	buf := newSyncBuf()
	p := newEventPrinter(buf, func() time.Time { return now })
	p(selector.Event{Kind: "owner-unregistered", Account: "GONE", Detail: "artifact"})
	if got := buf.String(); got != "chottag: owner-unregistered GONE  artifact\n" {
		t.Fatalf("line = %q", got)
	}
}

// Warm loop ----------------------------------------------------------

type warmCall struct {
	dir    string
	within time.Duration
}

type fakeWarm struct {
	mu     sync.Mutex
	calls  []warmCall
	status map[string]creds.Status
}

func (f *fakeWarm) warm(_ context.Context, dir string, within time.Duration) (creds.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, warmCall{dir, within})
	st, ok := f.status[dir]
	if !ok {
		st = creds.Status{State: creds.StateOK}
	}
	return st, st.State == creds.StateOK
}

func (f *fakeWarm) callsSnapshot() []warmCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]warmCall(nil), f.calls...)
}

func warmRig(t *testing.T, st store.State, fw *fakeWarm, now *time.Time) (*remoteWarmer, *syncBuf) {
	t.Helper()
	buf := newSyncBuf()
	w := &remoteWarmer{
		state: func() (store.State, error) { return st, nil },
		warm:  fw.warm,
		log:   buf,
		now:   func() time.Time { return *now },
	}
	return w, buf
}

func TestRemoteWarmerRefreshesEveryPoolsRemoteInsideTheWindow(t *testing.T) {
	st := staleRemoteState()
	st.Accounts = append(st.Accounts, store.Account{Name: "W", Dir: "/slots/W"})
	st.Pools = map[string]store.Pool{"work": {Serving: "S", Remote: "W"}, "other": {Serving: "S", Remote: "A"}}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fw := &fakeWarm{}
	w, _ := warmRig(t, st, fw, &now)
	w.pass(context.Background(), false)
	calls := fw.callsSnapshot()
	if len(calls) != 3 {
		t.Fatalf("calls = %+v, want A, S and W once each (deduplicated across pools)", calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.dir] = true
		if c.within != creds.ExpiringWithin+time.Minute {
			t.Fatalf("window = %v, want %v", c.within, creds.ExpiringWithin+time.Minute)
		}
	}
	if !seen["/slots/A"] || !seen["/slots/W"] || !seen["/slots/S"] {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestRemoteWarmerSkipsANeedsLoginAccountAndLogsOnce(t *testing.T) {
	st := staleRemoteState()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fw := &fakeWarm{status: map[string]creds.Status{"/slots/A": {State: creds.StateNeedsLogin}}}
	w, buf := warmRig(t, st, fw, &now)
	w.pass(context.Background(), false)
	w.pass(context.Background(), false)
	if n := strings.Count(buf.String(), "A needs login"); n != 1 {
		t.Fatalf("logged %d times, want once: %q", n, buf.String())
	}
	// a login later brings the account back; a new needs-login logs again
	fw.mu.Lock()
	fw.status = nil
	fw.mu.Unlock()
	w.pass(context.Background(), false)
	fw.mu.Lock()
	fw.status = map[string]creds.Status{"/slots/A": {State: creds.StateNeedsLogin}}
	fw.mu.Unlock()
	w.pass(context.Background(), false)
	if n := strings.Count(buf.String(), "A needs login"); n != 2 {
		t.Fatalf("logged %d times after recovery, want 2: %q", n, buf.String())
	}
}

// kick runs one pass at a time and at most one a minute.
func TestRemoteWarmerKickIsRateLimitedAndSingleFlight(t *testing.T) {
	st := staleRemoteState()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fw := &fakeWarm{}
	w, _ := warmRig(t, st, fw, &now)
	ctx := context.Background()
	w.kick(ctx)
	w.wait()
	w.kick(ctx)
	w.wait()
	if n := len(fw.callsSnapshot()); n != 2 { // one pass: the remote A and the serving S
		t.Fatalf("calls = %d inside a minute, want 2", n)
	}
	now = now.Add(time.Minute)
	w.kick(ctx)
	w.wait()
	if n := len(fw.callsSnapshot()); n != 4 {
		t.Fatalf("calls = %d after a minute, want 4", n)
	}
}

func TestRemoteWarmerNilIsANoOp(t *testing.T) {
	var w *remoteWarmer
	w.kick(context.Background())
	w.wait()
}

// runDaemon kicks the warm loop from its roster stamp: a remote account's
// token is warmed on the daemon's own ticks, with no request behind it.
func TestRunDaemonWarmsTheRemoteAccountOnItsRosterTick(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: "/slots/A"}); err != nil {
			return err
		}
		st.Remote = "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := store.NewCache(s)
	warmed := make(chan string, 4)
	exited := make(chan struct{})
	w := &remoteWarmer{
		state: cache.State,
		warm: func(ctx context.Context, dir string, within time.Duration) (creds.Status, bool) {
			if within != creds.ExpiringWithin+time.Minute {
				t.Errorf("window = %v", within)
			}
			warmed <- dir
			<-ctx.Done() // a pass in flight at shutdown is cancelled...
			close(exited)
			return creds.Status{State: creds.StateOK}, false
		},
		log: io.Discard,
		now: time.Now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard, Stderr: io.Discard, Listen: "127.0.0.1:0",
			Sink: sink, Tokens: tokens.New(tokens.Config{}), Cache: cache, Owners: own,
			Warm: w,
		})
	}()
	if got := <-warmed; got != "/slots/A" {
		t.Fatalf("warmed %q, want the remote account's slot", got)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("runDaemon returned %d", code)
	}
	select {
	case <-exited: // ...and joined before runDaemon returns
	default:
		t.Fatal("runDaemon returned with the warm pass still running")
	}
}

// End to end through the proxy with the real selector, chooser and token
// manager (only the slot read and the refresher are fake) ---------------

type e2eSlot struct {
	mu      sync.Mutex
	tok     creds.Token
	refresh func(*e2eSlot) error
	count   int
}

func (s *e2eSlot) read(string) (creds.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tok, nil
}

func (s *e2eSlot) Refresh(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return s.refresh(s)
}

func neverHomeRig(t *testing.T, slot *e2eSlot, now time.Time, upstream http.Handler) *proxytest.Harness {
	t.Helper()
	tm := tokens.New(tokens.Config{
		Read: slot.read, Refresh: slot,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		Now:      func() time.Time { return now },
	})
	st := staleRemoteState()
	stateFn := func() (store.State, error) { return st, nil }
	sel := selector.New(selector.Config{State: stateFn, Tokens: tm, Owners: fakeOwners{}})
	return proxytest.Start(t, upstream, proxytest.Options{Choose: &chooser{sel: sel, tm: tm, state: stateFn}})
}

func postRemote(t *testing.T, h *proxytest.Harness) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/code/sessions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A stale remote token whose refresh succeeds inside the wait goes out as
// the remote account, not on Home's login.
func TestRemoteRequestWithAStaleTokenRefreshesAndGoesOutAsTheRemoteAccount(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	slot := &e2eSlot{tok: creds.Token{AccessToken: "tok-old", ExpiresAt: now.Add(-time.Minute)}}
	slot.refresh = func(s *e2eSlot) error {
		s.tok = creds.Token{AccessToken: "tok-A", ExpiresAt: now.Add(time.Hour)}
		return nil
	}
	var mu sync.Mutex
	var seen []string
	h := neverHomeRig(t, slot, now, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	resp := postRemote(t, h)
	defer resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if resp.StatusCode != http.StatusOK || len(seen) != 1 || seen[0] != "Bearer tok-A" {
		t.Fatalf("status %d, upstream saw %v; want one request as the remote account", resp.StatusCode, seen)
	}
}

// A refresh that fails: the 503 names the account, nothing reaches upstream.
// A second request inside the backoff gets the same answer.
func TestRemoteRequestWithAFailingRefreshIs503AndNeverReachesUpstream(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	slot := &e2eSlot{tok: creds.Token{AccessToken: "tok-old", ExpiresAt: now.Add(-time.Minute)}}
	slot.refresh = func(*e2eSlot) error { return errors.New("network down") }
	var upstream atomic.Int32
	h := neverHomeRig(t, slot, now, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstream.Add(1) }))
	for i := 0; i < 2; i++ {
		resp := postRemote(t, h)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "account A (remote) has no usable login right now; run: chottag login A") {
			t.Fatalf("request %d: status %d body %s", i, resp.StatusCode, body)
		}
	}
	if upstream.Load() != 0 {
		t.Fatal("a refused remote request reached upstream")
	}
}

// M3: with state.json unreadable and one pool, a remote or owner request is
// refused (never Home's login); a serving request keeps today's passthrough.
func TestChooserRefusesRemoteAndOwnerWhenStateIsUnreadableButNotServing(t *testing.T) {
	stateFn := func() (store.State, error) { return store.State{}, errors.New("corrupt") }
	sel := selector.New(selector.Config{State: stateFn, Tokens: fakeTokens{}, Owners: fakeOwners{}})
	c := &chooser{sel: sel, state: stateFn}
	ctx := context.Background()
	for _, d := range []router.Decision{
		{Class: router.Remote},
		{Class: router.Serving, Object: router.KindSession, ObjectID: "s1"},
	} {
		if _, _, _, ok, refusal := c.ChooseGuarded(ctx, d, ""); ok || refusal == "" {
			t.Fatalf("%+v: ok=%v refusal=%q, want a refusal", d, ok, refusal)
		}
	}
	if _, _, _, ok, refusal := c.ChooseGuarded(ctx, router.Decision{Class: router.Serving}, ""); ok || refusal != "" {
		t.Fatalf("serving: ok=%v refusal=%q, want today's passthrough", ok, refusal)
	}
}

// A refused serving request (pools) is not logged as sent on Home's login.
func TestEventPrinterGuardedServingRefusalDoesNotClaimHome(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	buf := newSyncBuf()
	p := newEventPrinter(buf, func() time.Time { return now })
	p(selector.Event{Kind: "passthrough", Account: "A", Status: creds.Status{State: creds.StateStale}, Role: selector.RoleServing, Refused: true})
	got := buf.String()
	if strings.Contains(got, "chottag: sent on Home's own login") || !strings.Contains(got, "A's token is stale (serving)") {
		t.Fatalf("line = %q", got)
	}
}

// kick runs one pass at a time even when the minute has passed, and not
// after shutdown began.
func TestRemoteWarmerKickSkipsWhileAPassRunsAndAfterCancel(t *testing.T) {
	st := staleRemoteState()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	w := &remoteWarmer{
		state: func() (store.State, error) { return st, nil },
		warm: func(context.Context, string, time.Duration) (creds.Status, bool) {
			calls.Add(1)
			entered <- struct{}{}
			<-release
			return creds.Status{State: creds.StateOK}, false
		},
		log: io.Discard, now: func() time.Time { return now },
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.kick(ctx)
	<-entered
	now = now.Add(5 * time.Minute)
	w.kick(ctx) // a pass is running: skipped though the minute has passed
	if w.stop(10 * time.Millisecond) {
		t.Fatal("stop reported done while a pass was blocked")
	}
	close(release)
	if !w.stop(time.Minute) {
		t.Fatal("stop did not see the pass finish")
	}
	if calls.Load() != 2 { // the one pass: A, then S
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	now = now.Add(5 * time.Minute)
	cancel()
	w.kick(ctx)
	w.wait()
	if calls.Load() != 2 {
		t.Fatalf("a pass started after cancel: calls = %d", calls.Load())
	}
}

// R149: a serving account is warmed too, in the same window.
func TestWarmerWarmsTheServingAccountToo(t *testing.T) {
	st := staleRemoteState()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fw := &fakeWarm{}
	w, _ := warmRig(t, st, fw, &now)
	w.pass(context.Background(), false)
	dirs := map[string]time.Duration{}
	for _, c := range fw.callsSnapshot() {
		dirs[c.dir] = c.within
	}
	if dirs["/slots/S"] != warmWindow || dirs["/slots/A"] != warmWindow || len(dirs) != 2 {
		t.Fatalf("warmed %v, want S and A in the %v window", dirs, warmWindow)
	}
}

// R149: a wake schedules a warm pass wakeWarmDelay later, with the wake
// trigger, and it runs past the one-a-minute limit.
func TestWarmerWakePassRunsAfterTheDelayWithTheWakeTrigger(t *testing.T) {
	st := staleRemoteState()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fw := &fakeWarm{}
	w, _ := warmRig(t, st, fw, &now)
	var wakeCalls atomic.Int32
	w.wakeWarm = func(ctx context.Context, dir string, within time.Duration) (creds.Status, bool) {
		wakeCalls.Add(1)
		return fw.warm(ctx, dir, within)
	}
	var fire func()
	var delay time.Duration
	w.after = func(d time.Duration, f func()) *time.Timer {
		delay, fire = d, f
		return time.NewTimer(time.Hour)
	}
	ctx := context.Background()
	w.kick(ctx) // a pass just ran, inside the minute
	w.wait()
	w.wake(ctx)
	if delay != wakeWarmDelay || fire == nil {
		t.Fatalf("scheduled after %v (fire set: %v), want %v", delay, fire != nil, wakeWarmDelay)
	}
	if wakeWarmDelay != 10*time.Second {
		t.Fatalf("wakeWarmDelay = %v, want 10s", wakeWarmDelay)
	}
	fire()
	w.wait()
	if n := wakeCalls.Load(); n != 2 {
		t.Fatalf("wake calls = %d, want 2 (A and S)", n)
	}
	if n := len(fw.callsSnapshot()); n != 4 {
		t.Fatalf("calls = %d, want 4 (one kick pass, one wake pass)", n)
	}
}

// A wake pass asked for while a pass runs follows it; it is never two
// passes at once.
func TestWarmerWakePassFollowsARunningPass(t *testing.T) {
	st := staleRemoteState()
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var running, maxRunning, wakeCalls atomic.Int32
	enter := func() {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		running.Add(-1)
	}
	w := &remoteWarmer{
		state: func() (store.State, error) { return st, nil },
		warm: func(context.Context, string, time.Duration) (creds.Status, bool) {
			enter()
			return creds.Status{State: creds.StateOK}, false
		},
		wakeWarm: func(context.Context, string, time.Duration) (creds.Status, bool) {
			wakeCalls.Add(1)
			return creds.Status{State: creds.StateOK}, false
		},
		log: io.Discard, now: func() time.Time { return now },
	}
	ctx := context.Background()
	w.kick(ctx)
	<-entered
	w.start(ctx, true) // wake while the pass is blocked
	close(release)
	w.wait()
	if wakeCalls.Load() != 2 {
		t.Fatalf("wake calls = %d, want 2 after the running pass", wakeCalls.Load())
	}
	if maxRunning.Load() != 1 {
		t.Fatalf("%d passes at once", maxRunning.Load())
	}
}

func TestWarmerWakeAfterStopSchedulesNothing(t *testing.T) {
	st := staleRemoteState()
	now := time.Now()
	w, _ := warmRig(t, st, &fakeWarm{}, &now)
	scheduled := false
	w.after = func(time.Duration, func()) *time.Timer { scheduled = true; return time.NewTimer(time.Hour) }
	w.stop(time.Second)
	w.wake(context.Background())
	if scheduled {
		t.Fatal("a wake after stop scheduled a pass")
	}
	var nilW *remoteWarmer
	nilW.wake(context.Background())
}

// R149: the warm pass also probes a slot the token manager locked out, even
// when its account is neither serving nor remote, and a renewal unlocks it.
func TestWarmerProbesALockedOutRotatingMember(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	var renewed atomic.Bool
	var refreshes atomic.Int32
	slot := &e2eSlot{tok: creds.Token{AccessToken: "t", ExpiresAt: now.Add(-time.Minute)}}
	slot.refresh = func(s *e2eSlot) error {
		refreshes.Add(1)
		if renewed.Load() {
			s.tok = creds.Token{AccessToken: "t2", ExpiresAt: clock().Add(8 * time.Hour)}
		}
		return nil
	}
	tm := tokens.New(tokens.Config{
		Read: slot.read, Refresh: slot, Now: clock,
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
	})
	st := store.State{
		Serving: "S", Remote: "A",
		Accounts: []store.Account{{Name: "S", Dir: "/slots/S"}, {Name: "A", Dir: "/slots/A"}, {Name: "M", Dir: "/slots/M"}},
	}
	buf := newSyncBuf()
	w := &remoteWarmer{
		state: func() (store.State, error) { return st, nil },
		warm: func(ctx context.Context, dir string, within time.Duration) (creds.Status, bool) {
			if dir != "/slots/M" {
				return creds.Status{State: creds.StateOK}, false
			}
			return tm.Warm(ctx, dir, within)
		},
		probe: tm.LockedOut,
		log:   buf, now: clock,
	}
	// Lock M out directly through the manager (as its own refreshes would).
	for i := 0; i < 3; i++ {
		advance(6 * time.Minute)
		tm.Warm(context.Background(), "/slots/M", warmWindow)
	}
	if got := tm.LockedOut(); len(got) != 1 || got[0] != "/slots/M" {
		t.Fatalf("LockedOut = %v", got)
	}
	before := refreshes.Load()
	advance(16 * time.Minute)
	w.pass(context.Background(), false) // M is neither serving nor remote
	if refreshes.Load() != before+1 {
		t.Fatalf("refreshes = %d, want one probe of M", refreshes.Load()-before)
	}
	if !strings.Contains(buf.String(), "account M needs login") || !strings.Contains(buf.String(), "probed every 15m") {
		t.Fatalf("log = %q, want the probe wording", buf.String())
	}
	renewed.Store(true)
	advance(16 * time.Minute)
	w.pass(context.Background(), false)
	if got := tm.LockedOut(); len(got) != 0 {
		t.Fatalf("still locked out after a renewing probe: %v", got)
	}
}

// An account removed from the roster while locked out is not probed any more,
// and nothing more is logged for it.
func TestWarmerStopsProbingALockedOutAccountTheRosterDropped(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	var refreshes atomic.Int32
	slot := &e2eSlot{tok: creds.Token{AccessToken: "t", ExpiresAt: now.Add(-time.Minute)}}
	slot.refresh = func(*e2eSlot) error { refreshes.Add(1); return nil }
	tm := tokens.New(tokens.Config{
		Read: slot.read, Refresh: slot, Now: func() time.Time { return now },
		LockPath: func(dir string) string { return dir + "/.lock" },
		TryLock:  func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
	})
	st := store.State{
		Serving: "S", Remote: "A",
		Accounts: []store.Account{{Name: "S", Dir: "/slots/S"}, {Name: "A", Dir: "/slots/A"}, {Name: "M", Dir: "/slots/M"}},
	}
	buf := newSyncBuf()
	w := &remoteWarmer{
		state: func() (store.State, error) { return st, nil },
		warm: func(ctx context.Context, dir string, within time.Duration) (creds.Status, bool) {
			if dir != "/slots/M" {
				return creds.Status{State: creds.StateOK}, false
			}
			return tm.Warm(ctx, dir, within)
		},
		probe: tm.LockedOut, log: buf, now: func() time.Time { return now },
	}
	for i := 0; i < 3; i++ {
		now = now.Add(6 * time.Minute)
		tm.Warm(context.Background(), "/slots/M", warmWindow)
	}
	if len(tm.LockedOut()) != 1 {
		t.Fatal("setup: M is not locked out")
	}
	st.Accounts = st.Accounts[:2] // M is removed
	before, logged := refreshes.Load(), buf.String()
	now = now.Add(16 * time.Minute)
	w.pass(context.Background(), false)
	if refreshes.Load() != before || buf.String() != logged {
		t.Fatalf("a removed account was probed or logged about: %d refreshes, log %q", refreshes.Load()-before, buf.String())
	}
}

// R160: an account with a validate session used in the last day is kept fresh
// too, though no pool names it any more.
func TestRemoteWarmerAlsoWarmsAccountsWithRecentValidateSessions(t *testing.T) {
	st := staleRemoteState()
	st.Accounts = append(st.Accounts, store.Account{Name: "C", Dir: "/slots/C"})
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	fw := &fakeWarm{}
	w, _ := warmRig(t, st, fw, &now)
	w.sticky = func() []string { return []string{"C", "S", "Gone"} }
	w.pass(context.Background(), false)
	seen := map[string]int{}
	for _, c := range fw.callsSnapshot() {
		seen[c.dir]++
	}
	if len(seen) != 3 || seen["/slots/C"] != 1 || seen["/slots/S"] != 1 || seen["/slots/A"] != 1 {
		t.Fatalf("warmed %v, want A, S and C once each (a removed account skipped)", seen)
	}
}
