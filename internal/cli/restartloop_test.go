package cli

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

type restartHarness struct {
	t         *testing.T
	home      string
	sink      *statusSink
	loop      *restartLoop
	st        store.State
	installed string
	now       time.Time
	lastReq   time.Time // when the latest request started; zero = none
	busy      bool
	quiets    []time.Duration
	notices   []string
	spawns    [][]string // bin, then args
	spawnErr  error
	spawnOut  string
	noLink    bool // the installed version comes from install.json only
	logged    strings.Builder
}

func newRestartHarness(t *testing.T, running, installed string) *restartHarness {
	t.Helper()
	oldInst, oldSpawn, oldLink := installedVersion, restartSpawn, installedViaLink
	t.Cleanup(func() { installedVersion, restartSpawn, installedViaLink = oldInst, oldSpawn, oldLink })
	h := &restartHarness{t: t, home: t.TempDir(), st: store.Default(), installed: installed, now: loopNow}
	installedVersion = func(string) (string, error) {
		if h.installed == "" {
			return "", errors.New("no installed version")
		}
		return h.installed, nil
	}
	restartSpawn = func(out, bin string, args ...string) error {
		h.spawnOut = out
		h.spawns = append(h.spawns, append([]string{bin}, args...))
		return h.spawnErr
	}
	installedViaLink = func(_, v string) bool { return !h.noLink && v == h.installed }
	sink, err := newStatusSink(h.home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	h.sink = sink
	h.loop = h.newLoop(running)
	return h
}

// newLoop is a loop for a daemon generation running version running, on the
// same home as every earlier one.
func (h *restartHarness) newLoop(running string) *restartLoop {
	return &restartLoop{
		home: h.home, version: running, sink: h.sink, log: &h.logged,
		state:  func() (store.State, error) { return h.st, nil },
		notify: func(title, body string) { h.notices = append(h.notices, body) },
		now:    func() time.Time { return h.now },
		idle: func(now time.Time, quiet time.Duration) bool {
			h.quiets = append(h.quiets, quiet)
			return !h.busy && (h.lastReq.IsZero() || now.Sub(h.lastReq) >= quiet)
		},
		wake: make(chan struct{}, 1),
	}
}

func (h *restartHarness) pending() string {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	if h.sink.file.Daemon == nil {
		return ""
	}
	return h.sink.file.Daemon.RestartPending
}

func TestRestartPendingOnlyWhenInstalledIsNewerThanRunning(t *testing.T) {
	for _, c := range []struct{ name, running, installed, want string }{
		{"newer installed", "0.5.0", "0.6.0", "0.6.0"},
		{"equal", "0.6.0", "0.6.0", ""},
		{"older installed", "0.6.0", "0.5.0", ""},
		{"a dev running build", "dev", "0.6.0", ""},
		{"installed unknown", "0.5.0", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRestartHarness(t, c.running, c.installed)
			h.busy = true
			h.loop.look()
			if got := h.pending(); got != c.want {
				t.Fatalf("pending = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRestartPendingClearsWhenTheInstalledVersionCatchesUp(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.busy = true
	h.loop.look()
	h.installed = "0.5.0"
	h.loop.look()
	if got := h.pending(); got != "" {
		t.Fatalf("pending = %q after the installed version fell back, want none", got)
	}
}

func TestRestartSpawnsTheInstalledBinaryDetachedWhenIdle(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.loop.look()
	want := [][]string{{filepath.Join(h.home, "bin", "chottag"), "daemon", "restart", "--force", "--json"}}
	if !reflect.DeepEqual(h.spawns, want) {
		t.Fatalf("spawns = %q, want %q", h.spawns, want)
	}
	if len(h.quiets) != 1 || h.quiets[0] != 5*time.Minute {
		t.Fatalf("idle asked with %v, want one 5m quiet window", h.quiets)
	}
}

func TestRestartBusyProxyBlocksIt(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.busy = true
	h.loop.look()
	if len(h.spawns) != 0 {
		t.Fatalf("spawned while busy: %q", h.spawns)
	}
}

func TestRestartRequestFourMinutesAgoBlocksSixAllows(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.lastReq = h.now.Add(-4 * time.Minute)
	h.loop.look()
	if len(h.spawns) != 0 {
		t.Fatalf("spawned 4 minutes after a request: %q", h.spawns)
	}
	h.lastReq = h.now.Add(-6 * time.Minute)
	h.loop.look()
	if len(h.spawns) != 1 {
		t.Fatalf("spawns = %q, want one 6 minutes after a request", h.spawns)
	}
}

func TestRestartNothingPendingNeverSpawns(t *testing.T) {
	h := newRestartHarness(t, "0.6.0", "0.6.0")
	h.loop.look()
	if len(h.spawns) != 0 || len(h.notices) != 0 {
		t.Fatalf("spawns %q notices %q, want none", h.spawns, h.notices)
	}
}

func TestRestartOffStillShowsPendingButNeitherSpawnsNorNotifies(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.st.SetAutoRestart(false)
	h.loop.look()
	if len(h.spawns) != 0 || len(h.notices) != 0 {
		t.Fatalf("spawns %q notices %q, want none with auto-restart off", h.spawns, h.notices)
	}
	if got := h.pending(); got != "0.6.0" {
		t.Fatalf("pending = %q, want 0.6.0 shown even with auto-restart off", got)
	}
}

func TestRestartNotifiesOncePerInstalledVersion(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.busy = true
	h.loop.look()
	h.loop.look()
	want := "chottag 0.6.0 is installed. The daemon switches to it when Claude Code is idle."
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices = %q, want one %q", h.notices, want)
	}
	h.installed = "0.6.1"
	h.loop.look()
	if len(h.notices) != 2 || !strings.Contains(h.notices[1], "0.6.1 is installed") {
		t.Fatalf("notices = %q, want a second for 0.6.1", h.notices)
	}
}

func TestRestartAttemptsOncePerVersionPerHour(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.loop.look()
	h.now = h.now.Add(59 * time.Minute)
	h.loop.look()
	if len(h.spawns) != 1 {
		t.Fatalf("spawns = %d within the hour, want 1", len(h.spawns))
	}
	h.now = h.now.Add(2 * time.Minute)
	h.loop.look()
	if len(h.spawns) != 2 {
		t.Fatalf("spawns = %d after the hour, want 2", len(h.spawns))
	}
	h.installed = "0.6.1"
	h.loop.look()
	if len(h.spawns) != 3 {
		t.Fatalf("spawns = %d, want a fresh attempt for a different installed version", len(h.spawns))
	}
}

func TestRestartSpawnFailureNotifiesOncePerVersion(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.spawnErr = errors.New("fork refused")
	h.loop.look()
	h.now = h.now.Add(2 * time.Hour)
	h.loop.look()
	if len(h.spawns) != 2 {
		t.Fatalf("spawns = %d, want the failed attempt retried after the hour", len(h.spawns))
	}
	var fails []string
	for _, n := range h.notices {
		if strings.Contains(n, "could not restart") {
			fails = append(fails, n)
		}
	}
	want := "chottag could not restart onto 0.6.0: fork refused. Run: chottag daemon restart"
	if len(fails) != 1 || fails[0] != want {
		t.Fatalf("failure notices = %q, want one %q", fails, want)
	}
	if !strings.Contains(h.logged.String(), "fork refused") {
		t.Errorf("log %q lacks the reason", h.logged.String())
	}
}

func TestRestartLoopLooksAtStartOnEachTickAndOnWake(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.busy = true
	ticks := make(chan time.Time)
	processed := make(chan struct{})
	h.loop.ticks, h.loop.processed = ticks, processed
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.loop.Run(ctx); close(done) }()
	<-processed // the look at start
	if got := h.pending(); got != "0.6.0" {
		t.Fatalf("pending = %q after the first look", got)
	}
	h.busy = false
	h.lastReq = h.now.Add(-time.Hour)
	ticks <- h.now
	<-processed
	if len(h.spawns) != 1 {
		t.Fatalf("spawns = %d after a tick, want 1", len(h.spawns))
	}
	h.now = h.now.Add(2 * time.Hour)
	h.loop.Wake()
	<-processed
	if len(h.spawns) != 2 {
		t.Fatalf("spawns = %d after a wake, want 2 (a wake triggers a look)", len(h.spawns))
	}
	cancel()
	<-done
}

func TestNilRestartLoopIsInert(t *testing.T) {
	var l *restartLoop
	l.Wake()
	l.Run(context.Background())
}

// TestRunDaemonRunsTheRestartLoopWakesItAndBindsTheProxyIdleSignal drives the
// wiring: runDaemon binds its own proxy's Idle to the loop, a wake from
// sleep makes the loop look, and the loop is joined at shutdown.
func TestRunDaemonRunsTheRestartLoopWakesItAndBindsTheProxyIdleSignal(t *testing.T) {
	oldInst, oldSpawn, oldLink := installedVersion, restartSpawn, installedViaLink
	t.Cleanup(func() { installedVersion, restartSpawn, installedViaLink = oldInst, oldSpawn, oldLink })
	installedViaLink = func(string, string) bool { return true }
	var installed atomic.Value
	installed.Store("0.5.0")
	installedVersion = func(string) (string, error) { return installed.Load().(string), nil }
	spawned := make(chan []string, 4)
	restartSpawn = func(_, bin string, args ...string) error {
		spawned <- append([]string{bin}, args...)
		return nil
	}

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
	cache := store.NewCache(store.Store{Dir: t.TempDir()})
	looked := make(chan struct{})
	loop := &restartLoop{
		// Far enough ahead that the real proxy's construction-time start is
		// outside the 5-minute quiet window.
		home: home, version: "0.5.0", sink: sink, now: func() time.Time { return time.Now().Add(time.Hour) },
		state:     cache.State,
		ticks:     make(chan time.Time), // never fires: only the start and the wake look
		wake:      make(chan struct{}, 1),
		processed: looked,
	}
	wakeTicks := make(chan time.Time)
	var wall atomic.Int64
	wall.Store(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	daemonDone := make(chan int, 1)
	go func() {
		daemonDone <- runDaemon(ctx, daemonDeps{
			Stdout: io.Discard, Stderr: io.Discard, Listen: "127.0.0.1:0",
			Sink: sink, Tokens: tokens.New(tokens.Config{}), Cache: cache, Owners: own,
			Restart: loop,
			Waker: proxy.WakerConfig{
				Gap: 30 * time.Second, Ticks: wakeTicks,
				Wall: func() time.Time { return time.Unix(0, wall.Load()).UTC() },
				Mono: func() time.Duration { return 0 },
			},
		})
	}()
	<-looked // the look at start: nothing pending yet
	// The waker takes its baseline when it starts, which nothing here orders
	// against the loop's first look. An unbuffered tick is received only
	// once that baseline exists, and moves no clock, so it fires nothing.
	wakeTicks <- time.Now()
	installed.Store("0.6.0")
	wall.Add(int64(time.Hour)) // a one-hour wall jump with a frozen monotonic clock is a wake
	wakeTicks <- time.Now()
	<-looked
	select {
	case got := <-spawned:
		if len(got) != 5 || got[1] != "daemon" || got[2] != "restart" {
			t.Fatalf("spawned %q", got)
		}
	default:
		t.Fatal("a wake did not make the restart loop look, or the proxy's idle signal was not bound")
	}
	cancel()
	if code := <-daemonDone; code != 0 {
		t.Fatalf("runDaemon returned %d", code)
	}
}

func TestRestartLimitsAndNoticesHoldAcrossDaemonGenerations(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.loop.look()
	if len(h.spawns) != 1 || len(h.notices) != 1 {
		t.Fatalf("spawns %d notices %d, want 1 and 1", len(h.spawns), len(h.notices))
	}
	// A new generation, still the old version (the restart did not end the
	// mismatch), a minute later: no spawn and no second notice.
	h.now = h.now.Add(time.Minute)
	h.loop = h.newLoop("0.5.0")
	h.loop.look()
	if len(h.spawns) != 1 || len(h.notices) != 1 {
		t.Fatalf("spawns %d notices %q, want the hourly limit and the notice kept across generations", len(h.spawns), h.notices)
	}
	h.now = h.now.Add(time.Hour)
	h.loop = h.newLoop("0.5.0")
	h.loop.look()
	if len(h.spawns) != 2 {
		t.Fatalf("spawns = %d, want a retry after the hour", len(h.spawns))
	}
	info, err := os.Stat(filepath.Join(h.home, "run", "restart.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restart.json: %v %v, want mode 0600", info, err)
	}
}

func TestRestartOnlyActsOnAVersionTheLinkPointsAt(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.noLink = true
	h.loop.look()
	h.loop.look()
	if len(h.spawns) != 0 || len(h.notices) != 0 {
		t.Fatalf("spawns %q notices %q, want none: the version comes from install.json only", h.spawns, h.notices)
	}
	if got := h.pending(); got != "0.6.0" {
		t.Fatalf("pending = %q, want it shown anyway", got)
	}
	if n := strings.Count(h.logged.String(), "not restarting"); n != 1 {
		t.Fatalf("log %q, want the skip logged once", h.logged.String())
	}
}

func TestRestartSendsTheChildsOutputToRestartLog(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.loop.look()
	if want := filepath.Join(h.home, "run", "restart.log"); h.spawnOut != want {
		t.Fatalf("child output = %q, want %q", h.spawnOut, want)
	}
}

// F2: a restart that started but did not happen is reported once.
func TestRestartReportsAFailedChildOnce(t *testing.T) {
	for _, c := range []struct {
		name, log, want string
	}{
		{"ok false", "chottag: warning\n{\n  \"ok\": false,\n  \"error\": {\"code\": \"stop_failed\"}\n}\n", "stop_failed"},
		{"unparseable", "garbage", "no_result"},
		{"empty", "", "no_result"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRestartHarness(t, "0.5.0", "0.6.0")
			h.loop.look() // spawns
			if err := os.WriteFile(h.spawnOut, []byte(c.log), 0o600); err != nil {
				t.Fatal(err)
			}
			h.busy = true
			h.now = h.now.Add(restartReportWait + time.Second)
			h.loop.look()
			want := "chottag could not restart onto 0.6.0: " + c.want + ". Run: chottag daemon restart"
			count := func() (n int) {
				for _, m := range h.notices {
					if m == want {
						n++
					}
				}
				return
			}
			if count() != 1 {
				t.Fatalf("notices = %q, want one %q", h.notices, want)
			}
			h.now = h.now.Add(time.Minute)
			h.loop.look()
			h.loop = h.newLoop("0.5.0") // the next generation does not repeat it
			h.loop.look()
			if count() != 1 {
				t.Fatalf("notices = %q, want it told once", h.notices)
			}
		})
	}
}

func TestRestartDoesNotReportASuccessfulOrNotYetFinishedChild(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.loop.look()
	h.busy = true
	h.now = h.now.Add(restartReportWait - time.Second) // too early to read
	h.loop.look()
	// The restart happened: the new generation runs 0.6.0 and nothing is pending.
	h.now = h.now.Add(time.Minute)
	h.loop = h.newLoop("0.6.0")
	os.WriteFile(h.spawnOut, []byte("not json yet"), 0o600)
	h.loop.look()
	for _, n := range h.notices {
		if strings.Contains(n, "could not restart") {
			t.Fatalf("notices = %q, want no failure for a restart that happened", h.notices)
		}
	}
}

func TestSpawnDetachedRunsItsOwnSessionAndDoesNotWait(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(dir, "chottag")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\necho $$\nread x < '" + fifo + "'\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := spawnDetached(out, self, "daemon", "restart", "--force", "--json"); err != nil {
		t.Fatal(err)
	}
	// spawnDetached has returned while the child is still alive, blocked on
	// the fifo. Wait until it has written its argv and pid (hang guard: 10 s).
	var lines []string
	for i := 0; i < 2000; i++ {
		b, _ := os.ReadFile(out)
		if lines = strings.Fields(string(b)); len(lines) >= 5 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(lines) < 5 || strings.Join(lines[:4], " ") != "daemon restart --force --json" {
		t.Fatalf("child output = %q, want the argv then its pid", lines)
	}
	pid, err := strconv.Atoi(lines[4])
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	// Setsid makes the child the leader of a new session and group.
	if pgid != pid || pgid == syscall.Getpgrp() {
		t.Fatalf("child pid %d group %d (ours %d), want its own group", pid, pgid, syscall.Getpgrp())
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode: %v %v", info, err)
	}
	g, err := os.OpenFile(fifo, os.O_WRONLY, 0) // let the child finish
	if err != nil {
		t.Fatal(err)
	}
	g.WriteString("go\n")
	g.Close()
}

func TestNewRestartLoopIsNilForADevBuild(t *testing.T) {
	old := Version
	Version = "dev"
	t.Cleanup(func() { Version = old })
	if l := newRestartLoop(t.TempDir(), nil, nil, nil, io.Discard); l != nil {
		t.Fatalf("newRestartLoop = %+v for a dev build, want nil", l)
	}
}

// Finding 1: the loop never restarts the daemon while an update is running.
func TestRestartNeverSpawnsWhileAnUpdateIsInProgress(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	if err := os.MkdirAll(filepath.Join(h.home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(filepath.Join(h.home, "run", updateLockName))
	if err != nil || !ok {
		t.Fatalf("could not take the update lock: %v %v", ok, err)
	}
	h.loop.look()
	if len(h.spawns) != 0 {
		t.Fatalf("spawns = %q while the update lock is held, want none", h.spawns)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	autoInstalling.Store(true)
	t.Cleanup(func() { autoInstalling.Store(false) })
	h.loop.look()
	if len(h.spawns) != 0 {
		t.Fatalf("spawns = %q while an auto-install runs, want none", h.spawns)
	}

	autoInstalling.Store(false)
	h.loop.look()
	if len(h.spawns) != 1 {
		t.Fatalf("spawns = %d with both cleared, want 1", len(h.spawns))
	}
	// The probe released the lock at once: a person's update can take it.
	unlock, ok, err = fsutil.TryLock(filepath.Join(h.home, "run", updateLockName))
	if err != nil || !ok {
		t.Fatalf("the loop kept the update lock: %v %v", ok, err)
	}
	unlock()
}

// Findings 1, 4 and 8: the two loops together. One unattended update is one
// notice, and the restart loop does nothing until the install is over.
func TestUpdateAndRestartLoopsTogetherOneNoticeAndNoRestartMidInstall(t *testing.T) {
	h := newRestartHarness(t, "0.6.0", "0.6.0")
	oldVer, oldClock, oldRun, oldFetch := Version, updateLoopClock, autoUpdateRun, updateFetch
	t.Cleanup(func() { Version, updateLoopClock, autoUpdateRun, updateFetch = oldVer, oldClock, oldRun, oldFetch })
	Version = "0.6.0"
	updateLoopClock = func() time.Time { return loopNow }
	target := filepath.Join(h.home, "versions", "0.6.0", "chottag")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.home, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(h.home, "bin", "chottag")); err != nil {
		t.Fatal(err)
	}
	updateFetch = func(context.Context, *url.URL, string) (updatecheck.Release, error) {
		return aged("v0.6.1", 48*time.Hour), nil
	}
	h.st.SetAutoUpdate(true)
	var midSpawns, midNotices int
	autoUpdateRun = func(context.Context, string, string) autoRun {
		h.installed = "0.6.1" // setup relinks bin/chottag early
		h.loop.look()
		midSpawns, midNotices = len(h.spawns), len(h.notices)
		return autoRun{OK: true, Installed: true}
	}
	ul := &updateLoop{
		home: h.home, repo: "Acme/chottag", version: "0.6.0", sink: h.sink, log: &h.logged,
		state:           func() (store.State, error) { return h.st, nil },
		notify:          func(title, body string) { h.notices = append(h.notices, body) },
		restartNotified: h.loop.NoteNotified,
	}
	ul.tick(context.Background())
	if midSpawns != 0 || midNotices != 0 {
		t.Fatalf("during the install: %d spawns, %d notices, want none of either", midSpawns, midNotices)
	}
	h.loop.look()
	if len(h.spawns) != 1 {
		t.Fatalf("spawns = %d after the install, want 1", len(h.spawns))
	}
	want := "chottag updated to 0.6.1. The daemon switches to it when Claude Code is idle."
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices = %q, want exactly %q", h.notices, want)
	}
}

// Finding 7: a restart that reports ok but leaves the daemon older than the
// installed version is a failure, told once, and not retried.
func TestRestartThatLeavesTheDaemonOlderIsAFailureNotRetried(t *testing.T) {
	h := newRestartHarness(t, "0.5.0", "0.6.0")
	h.loop.look()
	if err := os.WriteFile(h.spawnOut, []byte("{\"ok\": true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(restartReportWait + time.Second)
	h.loop.look()
	want := "chottag could not restart onto 0.6.0: the installed binary is not 0.6.0. Run: chottag update --version v0.6.0"
	count := func() (n int) {
		for _, m := range h.notices {
			if m == want {
				n++
			}
		}
		return
	}
	if count() != 1 {
		t.Fatalf("notices = %q, want one %q", h.notices, want)
	}
	h.now = h.now.Add(3 * time.Hour)
	h.loop.look()
	h.loop = h.newLoop("0.5.0") // and not in the next generation either
	h.now = h.now.Add(3 * time.Hour)
	h.loop.look()
	if len(h.spawns) != 1 || count() != 1 {
		t.Fatalf("spawns %d, notices %q, want no retry and no second notice", len(h.spawns), h.notices)
	}
}
