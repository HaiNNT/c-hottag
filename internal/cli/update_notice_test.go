package cli

// R139: the update notice is posted once per version, by whichever check
// (the daemon's tick or `update --check`) finds it first. R140: four checks
// a day. No test reaches osascript, the network or real time.

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

// noticeEnv is a loop harness whose home is also the CLI's CHOTTAG_HOME, with
// the CLI's notifier recorded.
type noticeEnv struct {
	*loopHarness
	cli   *recordingNotifier
	mu    sync.Mutex
	daemn []string
}

func newNoticeEnv(t *testing.T, running, tag string) *noticeEnv {
	t.Helper()
	h := newLoopHarness(t, running, aged(tag, time.Hour), nil)
	t.Setenv("CHOTTAG_HOME", h.home)
	origReleases := updateReleases
	updateReleases = func(context.Context, *url.URL, string) ([]updatecheck.Release, error) { return nil, nil }
	t.Cleanup(func() { updateReleases = origReleases })
	writeStateWithPort(t, h.home, closedPort(t))
	e := &noticeEnv{loopHarness: h, cli: stubDaemonNotifier(t)}
	h.loop.notify = func(title, body string) {
		e.mu.Lock()
		e.daemn = append(e.daemn, title+"|"+body)
		e.mu.Unlock()
	}
	return e
}

// manualRun is one `update --check`: safe from any goroutine (it never
// touches t).
func (e *noticeEnv) manualRun() (int, string) {
	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check"}, newReporter(true, &out, &errb))
	return code, errb.String()
}

func (e *noticeEnv) manual(t *testing.T) string {
	t.Helper()
	code, stderr := e.manualRun()
	if code != exit.OK {
		t.Fatalf("update --check = %d: %s", code, stderr)
	}
	return stderr
}

// seedNotified writes a status.json whose update.notified is v, the state an
// install has after the old daemon notified and before run/update-notified
// exists. Call before the daemon's sink is opened.
func seedNotified(t *testing.T, home, v string) {
	t.Helper()
	var f status.File
	f.Update = &status.Update{Notified: v}
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

// reopenSink is a new daemon generation reading status.json. A non-empty
// seed is written between the old sink's close (which flushes) and the open.
func (e *noticeEnv) reopenSink(t *testing.T, seed string) {
	t.Helper()
	e.sink.Close()
	if seed != "" {
		seedNotified(t, e.home, seed)
	}
	sink2, err := newStatusSink(e.home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink2.Close)
	e.loop.sink = sink2
}

// takeCLI is the notice `update --check` just posted: the send is synchronous,
// so it is already queued when the command returns.
func (e *noticeEnv) takeCLI(t *testing.T) notice {
	t.Helper()
	select {
	case n := <-e.cli.got:
		return n
	default:
		t.Fatal("update --check posted no notice")
		return notice{}
	}
}

// total is every notice posted by either side so far.
func (e *noticeEnv) total() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.daemn) + len(e.cli.got)
}

func TestUpdateIntervalIsFourChecksADay(t *testing.T) {
	if updateInterval != 6*time.Hour || updateJitterMax != 15*time.Minute || updateFirstCheck != 2*time.Minute || updateSoak != 24*time.Hour {
		t.Fatalf("interval %v jitter %v first %v soak %v", updateInterval, updateJitterMax, updateFirstCheck, updateSoak)
	}
}

func TestManualCheckNoticesOnceThenNothingElseDoes(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	e.manual(t)
	if n := e.takeCLI(t); n.title != "chottag: update available" || n.body != "chottag 0.6.0 is available. Run: chottag update" {
		t.Fatalf("notice = %+v", n)
	}
	e.manual(t)
	e.tick()
	e.sink.Close() // a daemon restart
	sink2, err := newStatusSink(e.home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink2.Close)
	e.loop.sink = sink2
	e.tick()
	if e.total() != 0 || len(e.cli.got) != 0 {
		t.Fatalf("a second notice for one version: daemon %q cli %d", e.daemn, len(e.cli.got))
	}
	if u := e.update(); u.Notified != "0.6.0" {
		t.Errorf("status notified = %q", u.Notified)
	}
}

func TestDaemonNoticePreventsAManualOne(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	e.tick()
	e.manual(t)
	if len(e.daemn) != 1 || len(e.cli.got) != 0 {
		t.Fatalf("daemon %q, cli %d", e.daemn, len(e.cli.got))
	}
}

func TestANewerVersionNoticesAgain(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	e.manual(t)
	e.takeCLI(t)
	stubFetch(t, aged("v0.6.1", time.Hour), nil)
	e.manual(t)
	if n := e.takeCLI(t); !strings.Contains(n.body, "0.6.1") {
		t.Fatalf("notice = %+v", n)
	}
	stubFetch(t, aged("v0.6.2", time.Hour), nil)
	e.tick()
	if len(e.daemn) != 1 || !strings.Contains(e.daemn[0], "0.6.2") {
		t.Fatalf("daemon %q", e.daemn)
	}
}

func TestConcurrentManualAndTickNoticeExactlyOnce(t *testing.T) {
	for i := 0; i < 20; i++ {
		e := newNoticeEnv(t, "0.5.0", "v0.6.0")
		// The harness's fetch counts calls, and stubFetch records repos: neither is goroutine-safe.
		updateFetch = func(context.Context, *url.URL, string) (updatecheck.Release, error) {
			return aged("v0.6.0", time.Hour), nil
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if code, stderr := e.manualRun(); code != exit.OK {
				t.Errorf("update --check = %d: %s", code, stderr)
			}
		}()
		go func() { defer wg.Done(); e.tick() }()
		wg.Wait()
		if n := e.total(); n != 1 {
			t.Fatalf("run %d: %d notices, want exactly 1 (daemon %q)", i, n, e.daemn)
		}
	}
}

func TestNoticeSuppressedByNotifyOffStillCountsAsGiven(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	if _, err := (store.Store{Dir: e.home}).Update(func(st *store.State) error { off := false; st.Notify = &off; return nil }); err != nil {
		t.Fatal(err)
	}
	e.manual(t)
	if len(e.cli.got) != 0 {
		t.Fatal("posted with notify off")
	}
	if _, err := (store.Store{Dir: e.home}).Update(func(st *store.State) error { on := true; st.Notify = &on; return nil }); err != nil {
		t.Fatal(err)
	}
	e.manual(t)
	e.st.Notify = nil
	e.tick()
	if e.total() != 0 {
		t.Fatalf("turning notify on replayed an old version: daemon %q cli %d", e.daemn, len(e.cli.got))
	}
}

func TestManualNoticeCarriesTheInstallLabel(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	if _, err := (store.Store{Dir: e.home}).Update(func(st *store.State) error { st.Label = "dev"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.manual(t)
	if n := e.takeCLI(t); n.title != "chottag · dev: update available" {
		t.Fatalf("title = %q", n.title)
	}
}

func TestManualCheckWithNothingNewPostsNothing(t *testing.T) {
	e := newNoticeEnv(t, "0.6.0", "v0.6.0")
	e.manual(t)
	if len(e.cli.got) != 0 {
		t.Fatal("a notice for the running version")
	}
}

// M1: an install upgraded from a daemon that kept "notified" only in
// status.json has no run/update-notified. It must not be told again.
func TestUpgradeSeedFromStatusJSONSuppressesTheDaemonNotice(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	e.reopenSink(t, "0.6.0")
	e.tick()
	if e.total() != 0 {
		t.Fatalf("an upgraded install was told about 0.6.0 again: %q", e.daemn)
	}
	stubFetch(t, aged("v0.6.1", time.Hour), nil)
	e.tick()
	e.tick()
	if len(e.daemn) != 1 || !strings.Contains(e.daemn[0], "0.6.1") {
		t.Fatalf("daemon notices = %q, want one for 0.6.1", e.daemn)
	}
}

func TestUpgradeSeedFromStatusJSONSuppressesTheManualNotice(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	seedNotified(t, e.home, "0.6.0")
	e.manual(t)
	if len(e.cli.got) != 0 {
		t.Fatal("an upgraded install was told about 0.6.0 again by update --check")
	}
	stubFetch(t, aged("v0.6.1", time.Hour), nil)
	e.manual(t)
	if n := e.takeCLI(t); !strings.Contains(n.body, "0.6.1") {
		t.Fatalf("notice = %+v", n)
	}
	e.manual(t)
	if len(e.cli.got) != 0 {
		t.Fatal("0.6.1 was told twice")
	}
}

// L1: an old daemon posts X after a new CLI recorded W; the seed (X) must
// still suppress it, once the new daemon starts.
func TestSeedStillSuppressesWhenTheFileHoldsAnOlderVersion(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	stubFetch(t, aged("v0.5.9", time.Hour), nil)
	e.manual(t)
	e.takeCLI(t) // the file now holds 0.5.9
	e.reopenSink(t, "0.6.0")
	stubFetch(t, aged("v0.6.0", time.Hour), nil)
	e.tick()
	if e.total() != 0 {
		t.Fatalf("0.6.0 was told again: %q", e.daemn)
	}
}

// L2: status.json's notified shows a notice `update --check` gave, at once,
// and the daemon's sink keeps it.
func TestManualNoticeShowsInStatusJSONAndSurvivesTheSink(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	e.manual(t)
	if u := loadUpdateCache(t, e.home); u == nil || u.Notified != "0.6.0" {
		t.Fatalf("status.json update = %+v, want notified 0.6.0", u)
	}
	e.reopenSink(t, "")
	if u := e.update(); u.Notified != "0.6.0" {
		t.Fatalf("the sink lost the CLI's notified: %+v", u)
	}
}

// L5: when the record cannot be claimed, neither side posts, and each says so.
func TestUnclaimableRecordFailsClosedAndSaysSo(t *testing.T) {
	e := newNoticeEnv(t, "0.5.0", "v0.6.0")
	if err := os.MkdirAll(filepath.Join(e.home, "run", "update-notified.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.tick()
	stderr := e.manual(t)
	if e.total() != 0 {
		t.Fatalf("a notice without a record: daemon %q cli %d", e.daemn, len(e.cli.got))
	}
	if !strings.Contains(e.logged.String(), "could not record the update notice") {
		t.Errorf("the daemon did not log it: %q", e.logged.String())
	}
	if !strings.Contains(stderr, "could not record the update notice") {
		t.Errorf("update --check did not warn: %q", stderr)
	}
}
