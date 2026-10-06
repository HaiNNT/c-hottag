package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

// recordingPoller stands in for *usagepoll.Poller in runDaemon tests.
type recordingPoller struct {
	rosters chan []usagepoll.Account
	wakes   chan struct{}
	started chan struct{}
	stopped chan struct{} // closed when Run sees its context end
	release chan struct{} // Run returns only once this is closed; nil returns at once
}

func newRecordingPoller() *recordingPoller {
	return &recordingPoller{
		rosters: make(chan []usagepoll.Account, 64),
		wakes:   make(chan struct{}, 8),
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func (r *recordingPoller) Run(ctx context.Context) {
	close(r.started)
	<-ctx.Done()
	close(r.stopped)
	if r.release != nil {
		<-r.release
	}
}

func (r *recordingPoller) SyncRoster(a []usagepoll.Account) {
	r.rosters <- append([]usagepoll.Account(nil), a...)
}

func (r *recordingPoller) Wake() { r.wakes <- struct{}{} }

func (r *recordingPoller) nextRoster(t *testing.T) []string {
	t.Helper()
	select {
	case a := <-r.rosters:
		names := make([]string, len(a))
		for i, x := range a {
			names[i] = x.Name + "=" + filepath.Base(x.Dir)
		}
		return names
	case <-time.After(5 * time.Second):
		t.Fatal("the poller never got a roster")
		return nil
	}
}

// startWiringDaemon runs runDaemon on dir with d's test seams filled in.
// done closes when runDaemon returns, after *code is set. Cleanup cancels
// it and waits for it.
func startWiringDaemon(t *testing.T, dir string, d daemonDeps) (cancel func(), done <-chan struct{}, code *int) {
	t.Helper()
	sink := d.Sink
	if sink == nil {
		var err error
		if sink, err = newStatusSink(dir, nil); err != nil {
			t.Fatal(err)
		}
	}
	own, err := owners.Open(filepath.Join(dir, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	d.Stdout, d.Listen, d.Home = io.Discard, "127.0.0.1:0", dir
	if d.Stderr == nil {
		d.Stderr = io.Discard
	}
	d.Sink, d.Owners = sink, own
	d.Tokens = tokens.New(tokens.Config{})
	d.Cache = store.NewCache(store.Store{Dir: dir})
	ctx, stop := context.WithCancel(context.Background())
	fin := make(chan struct{})
	code = new(int)
	go func() {
		*code = runDaemon(ctx, d)
		close(fin)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-fin:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})
	return stop, fin, code
}

func addSlotAccount(t *testing.T, dir, name string) {
	t.Helper()
	if _, err := (store.Store{Dir: dir}).Update(func(st *store.State) error {
		return st.Add(store.Account{Name: name, Dir: filepath.Join(dir, "accounts", name)})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunDaemonFeedsThePollerTheSlotRoster(t *testing.T) {
	dir := t.TempDir()
	addSlotAccount(t, dir, "A")
	rp := newRecordingPoller()
	tick := make(chan time.Time)
	processed := make(chan struct{})
	startWiringDaemon(t, dir, daemonDeps{Poller: rp, RosterTick: tick, RosterProcessed: processed})

	// The watcher's startup stamp is the daemon-start seeding.
	if got := rp.nextRoster(t); strings.Join(got, ",") != "A=A" {
		t.Fatalf("startup roster = %v, want [A=A]", got)
	}
	doTick := func() {
		t.Helper()
		select {
		case tick <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatal("the roster watcher never took the tick")
		}
		select {
		case <-processed:
		case <-time.After(5 * time.Second):
			t.Fatal("the roster watcher never finished the tick")
		}
	}
	addSlotAccount(t, dir, "B")
	doTick()
	if got := rp.nextRoster(t); strings.Join(got, ",") != "A=A,B=B" {
		t.Fatalf("roster after adding B = %v", got)
	}
	if _, err := (store.Store{Dir: dir}).Update(func(st *store.State) error { return st.Remove("B") }); err != nil {
		t.Fatal(err)
	}
	doTick()
	if got := rp.nextRoster(t); strings.Join(got, ",") != "A=A" {
		t.Fatalf("roster after removing B = %v", got)
	}
}

// TestRunDaemonJoinsThePollerBeforeItReturns: a poll must never land
// after the sink closes, so runDaemon waits for Run to return. The
// negative wait is bounded: without the join, runDaemon returns within
// milliseconds of the cancel while Run is still parked.
func TestRunDaemonJoinsThePollerBeforeItReturns(t *testing.T) {
	dir := t.TempDir()
	rp := newRecordingPoller()
	rp.release = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(rp.release) }) }
	cancel, done, code := startWiringDaemon(t, dir, daemonDeps{Poller: rp})
	t.Cleanup(release) // runs before the daemon cleanup (LIFO)

	select {
	case <-rp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon never started the poller")
	}
	cancel()
	select {
	case <-rp.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the poller's context was not cancelled on shutdown")
	}
	select {
	case <-done:
		t.Fatal("runDaemon returned while the poller was still running")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-done:
		if *code != 0 {
			t.Fatalf("runDaemon = %d, want 0", *code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("runDaemon did not return once the poller stopped")
	}
}

func TestRunDaemonTellsThePollerAboutAWake(t *testing.T) {
	dir := t.TempDir()
	rp := newRecordingPoller()
	ticks := make(chan time.Time)
	var wall atomic.Int64
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	wall.Store(base.UnixNano())
	errb := newSyncBuf()
	startWiringDaemon(t, dir, daemonDeps{Poller: rp, Stderr: errb, Waker: proxy.WakerConfig{
		Gap:   30 * time.Second,
		Ticks: ticks,
		Wall:  func() time.Time { return time.Unix(0, wall.Load()).UTC() },
		Mono:  func() time.Duration { return 0 },
		// OnWake unset: the production default, with the poller chained.
	}})
	send := func() {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(5 * time.Second):
			t.Fatal("the wake detector never took the tick")
		}
	}
	send() // warm-up: proves Run captured its baseline (see TestRunDaemonDefaultsOnWakeAndLogsWhenItFires)
	wall.Store(base.Add(time.Minute).UnixNano())
	send()
	select {
	case <-rp.wakes:
	case <-time.After(5 * time.Second):
		t.Fatal("a wake never reached the poller")
	}
	// The poller is chained AFTER the production default OnWake, not
	// instead of it: without this, a mutation that skips the default
	// handler entirely (e.g. `_ = onWake`) still leaves rp.wakes fed and
	// this test green.
	const wantLog = "chottag: woke from sleep"
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(errb.String(), wantLog) {
		if time.Now().After(deadline) {
			t.Fatalf("stderr = %q, want the default wake handler's line %q", errb.String(), wantLog)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type wiringTokens struct{ tok string }

func (w wiringTokens) Token(context.Context, string) (string, creds.Status, bool) {
	return w.tok, creds.Status{}, true
}
func (w wiringTokens) ForceRefresh(context.Context, string) (string, bool) { return "", false }

var _ usagepoll.TokenSource = (*tokens.Manager)(nil)

func TestDaemonPollerConfigUsesTheSinkAndTheUpstreamProxy(t *testing.T) {
	var seen atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.String() + " " + r.Header.Get("Authorization"))
		w.Write([]byte(`{"five_hour": {"utilization": 42, "resets_at": "2026-09-24T15:00:00Z"},
			"seven_day": {"utilization": 7, "resets_at": "2026-09-30T08:00:00Z"}}`))
	}))
	defer upstream.Close()
	up, _ := url.Parse(upstream.URL)
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var log bytes.Buffer
	cfg := daemonPollerConfig(&log, wiringTokens{"tok-A"}, sink, up, "http://usage.invalid/api/oauth/usage")

	out := cfg.Fetch(context.Background(), "/slots/A")
	if !strings.HasPrefix(out.Status, "200") {
		t.Fatalf("fetch status = %q, want a 200 through the upstream proxy", out.Status)
	}
	if got, _ := seen.Load().(string); got != "http://usage.invalid/api/oauth/usage Bearer tok-A" {
		t.Fatalf("upstream proxy saw %q", got)
	}
	if cfg.Log != io.Writer(&log) {
		t.Fatal("the poll log is not the writer the daemon passed (daemon.log)")
	}

	now := time.Now().Round(0)
	reset := now.Add(time.Hour).Truncate(time.Second)
	ap := cfg.Apply("A", usagepoll.Result{
		FiveHour: usagehdr.Window{Utilization: 1, HasUtilization: true, ResetsAt: reset, Known: true},
		SevenDay: usagehdr.Window{Utilization: 0.2, HasUtilization: true, ResetsAt: reset, Known: true},
		At:       now,
	}, now)
	if !ap.Written || !ap.Limited || !ap.Until.Equal(reset) {
		t.Fatalf("Apply = %+v, want the sink's poll write", ap)
	}
	if v := cfg.Cached("A", now); !v.Fresh || !v.Limited {
		t.Fatalf("Cached = %+v, want the sink's view of the write", v)
	}
}

// TestRunProxyWiresThePoller drives the production path: runProxyWithSignal
// builds the poller from its own token manager, sink, stderr and
// --upstream-proxy, hooks it into observe, and runDaemon seeds and runs it.
func TestRunProxyWiresThePoller(t *testing.T) {
	stubDaemonNotifier(t) // the warm pass may post a needs-login notice (F269)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	addSlotAccount(t, home, "A")

	type args struct {
		log      io.Writer
		tm       usagepoll.TokenSource
		sink     *statusSink
		upstream *url.URL
	}
	got := make(chan args, 1)
	fetched := make(chan string, 4)
	prev := newDaemonPoller
	t.Cleanup(func() { newDaemonPoller = prev }) // registered first, runs last
	newDaemonPoller = func(log io.Writer, tm usagepoll.TokenSource, sink *statusSink, upstream *url.URL) *usagepoll.Poller {
		got <- args{log, tm, sink, upstream}
		return usagepoll.New(usagepoll.Config{
			Fetch: func(_ context.Context, dir string) usagepoll.Outcome {
				fetched <- dir
				return usagepoll.Outcome{Status: "error"}
			},
			Cached: sink.cached,
			Apply:  sink.poll,
			Log:    log,
		})
	}

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", "", "--upstream-proxy", "http://127.0.0.1:9"}, io.Discard, errb, nil, sig)
	}()
	t.Cleanup(func() {
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down during cleanup")
		}
	})

	var a args
	select {
	case a = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("runProxyWithSignal never built the poller")
	}
	if a.upstream == nil || a.upstream.String() != "http://127.0.0.1:9" {
		t.Fatalf("poller upstream = %v, want the --upstream-proxy value", a.upstream)
	}
	if _, ok := a.tm.(*tokens.Manager); !ok || a.tm == nil {
		t.Fatalf("poller tokens = %T, want the daemon's *tokens.Manager", a.tm)
	}
	if a.log != io.Writer(errb) {
		t.Fatal("poller log is not the daemon's stderr (daemon.log)")
	}
	select {
	case dir := <-fetched:
		if dir != filepath.Join(home, "accounts", "A") {
			t.Fatalf("polled %q, want A's slot", dir)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon-start poll for A (no cached usage) never ran")
	}
	// Checked only after the first poll: newDaemonPoller signals got
	// before runProxyWithSignal's next line sets the hook, while the poll
	// runs only once runDaemon has started, after it. Checked straight
	// after got, this raced on a slow runner (F219).
	a.sink.mu.Lock()
	hooked := a.sink.onObserved != nil
	a.sink.mu.Unlock()
	if !hooked {
		t.Fatal("the sink's observe hook was not set to the poller")
	}
	const want = "chottag: usage poll A start error "
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(errb.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("stderr = %q, want a poll line %q", errb.String(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestObservePollAndRosterDoNotDeadlock hammers the three entry points
// that cross the sink and the poller locks. The poller never holds its
// lock while calling the sink, so this finishes; an inversion hangs it
// (probabilistically, which is why the lock order is also pinned
// structurally by TestSinkObserveTellsThePollerOnlyWhatItLearned and
// usagepoll's TestObservedNeverWaitsForAnInFlightPoll).
func TestObservePollAndRosterDoNotDeadlock(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	now := time.Now().Round(0)
	result := usagepoll.Result{
		FiveHour: usagehdr.Window{Utilization: 0.5, HasUtilization: true, Known: true},
		SevenDay: usagehdr.Window{Utilization: 0.5, HasUtilization: true, Known: true},
	}
	p := usagepoll.New(usagepoll.Config{
		Fetch: func(context.Context, string) usagepoll.Outcome {
			r := result
			r.At = time.Now()
			return usagepoll.Outcome{OK: true, Result: r, Status: "200"}
		},
		Cached: func(a string, now time.Time) usagepoll.CacheView {
			v := sink.cached(a, now)
			v.Fresh = false // every re-add seeds a start poll
			return v
		},
		Apply: sink.poll,
	})
	sink.setOnObserved(p.Observed)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { p.Run(ctx); close(runDone) }()
	// Defers run LIFO: cancel first (so Run can return), then join it
	// with a hang guard, then sink.Close last, so nothing is still
	// running against the sink when it closes.
	defer func() {
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return within 5s after cancel")
		}
	}()
	defer cancel()

	reset := now.Add(time.Hour).Truncate(time.Second)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			at := time.Now()
			sink.observe("A", usagehdr.Parse(sinkLimitHeaders(reset), at), usagehdr.Classify(429, sinkLimitHeaders(reset), at))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			p.SyncRoster(nil)
			p.SyncRoster([]usagepoll.Account{{Name: "A", Dir: "/slots/A"}})
			p.Wake()
		}
	}()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("observe, poll and roster sync deadlocked")
	}
}

// orderPoller records, at each SyncRoster, whether the sink already held a
// limited row under name: the roster tick must carry a renamed account's
// row before the poller seeds the new name from the cache (F171 review).
type orderPoller struct {
	*recordingPoller
	sink *statusSink
	name string
	seen chan bool
}

func (o *orderPoller) SyncRoster(a []usagepoll.Account) {
	f := o.sink.fileCopy()
	limited := false
	for _, r := range f.Accounts {
		if r.Name == o.name && r.Limited {
			limited = true
		}
	}
	o.seen <- limited
	o.recordingPoller.SyncRoster(a)
}

// F171: the roster tick carries a renamed account's status row across by
// its slot dir with no traffic: before the poller syncs, and written to
// status.json at once, so `next` and `auto` read it too.
func TestRunDaemonRosterTickCarriesARenamedAccountsStatusRow(t *testing.T) {
	dir := t.TempDir()
	addSlotAccount(t, dir, "A")
	slot := filepath.Join(dir, "accounts", "A")
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	var seed status.File
	seed.EnsureRoster([]status.Member{{Name: "A", Dir: slot}})
	seed.Observe("A", usagehdr.Snapshot{Known: true, At: time.Now()}, usagehdr.Verdict{Limited: true, Until: until, Window: "five_hour"})
	b, err := status.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(dir), b); err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	carried := make(chan struct{}, 1)
	sink.write = func(path string, b []byte) error {
		err := status.WriteBytes(path, b)
		var f status.File
		if json.Unmarshal(b, &f) == nil {
			for _, a := range f.Accounts {
				if a.Name == "work" && a.Limited && a.LimitedUntil.Equal(until) {
					select {
					case carried <- struct{}{}:
					default:
					}
				}
			}
		}
		return err
	}
	op := &orderPoller{recordingPoller: newRecordingPoller(), sink: sink, name: "work", seen: make(chan bool, 64)}
	tick := make(chan time.Time)
	processed := make(chan struct{})
	startWiringDaemon(t, dir, daemonDeps{Sink: sink, Poller: op, RosterTick: tick, RosterProcessed: processed})
	<-op.seen // the startup sync, before the rename

	if _, err := (store.Store{Dir: dir}).Update(func(st *store.State) error {
		st.Accounts[0].Name = "work"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case tick <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("the roster watcher never took the tick")
	}
	select {
	case <-processed:
	case <-time.After(5 * time.Second):
		t.Fatal("the roster watcher never finished the tick")
	}
	if !<-op.seen {
		t.Fatal("the poller synced the renamed roster before the status row was carried to work")
	}
	select {
	case <-carried:
	case <-time.After(5 * time.Second):
		t.Fatal("the carried work row was not written while the daemon ran")
	}
}
