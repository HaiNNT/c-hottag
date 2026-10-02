package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

var loopNow = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type loopHarness struct {
	t       *testing.T
	home    string
	sink    *statusSink
	loop    *updateLoop
	st      store.State
	notices []string
	runs    []string // "<self> <tag>" per child run
	runOK   bool
	runNoop bool // the child says ok but installed nothing
	runWhy  string
	fetches int
	// installed is what installedVersion reports; newLoopHarness starts it at the running version.
	installed string
	logged    strings.Builder
}

// newLoopHarness builds a loop for running version ver, with a stubbed
// clock, child and fetch, and an installed bin/chottag link.
func newLoopHarness(t *testing.T, ver string, rel updatecheck.Release, ferr error) *loopHarness {
	t.Helper()
	oldVer, oldClock, oldRun, oldFetch, oldInst := Version, updateLoopClock, autoUpdateRun, updateFetch, installedVersion
	t.Cleanup(func() {
		Version, updateLoopClock, autoUpdateRun, updateFetch, installedVersion = oldVer, oldClock, oldRun, oldFetch, oldInst
	})
	Version = ver
	updateLoopClock = func() time.Time { return loopNow }
	h := &loopHarness{t: t, home: t.TempDir(), st: store.Default(), installed: ver}
	installedVersion = func(string) (string, error) {
		if h.installed == "" {
			return "", errors.New("no installed version")
		}
		return h.installed, nil
	}
	target := filepath.Join(h.home, "versions", "v1", "chottag")
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
	sink, err := newStatusSink(h.home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	h.sink = sink
	updateFetch = func(context.Context, *url.URL, string) (updatecheck.Release, error) {
		h.fetches++
		return rel, ferr
	}
	autoUpdateRun = func(_ context.Context, self, tag string) autoRun {
		h.runs = append(h.runs, self+" "+tag)
		return autoRun{OK: h.runOK || h.runNoop, Installed: h.runOK && !h.runNoop, Reason: h.runWhy, Stderr: "child said: boom"}
	}
	h.loop = &updateLoop{
		home: h.home, repo: "Acme/chottag", version: ver, sink: sink, log: &h.logged,
		state:  func() (store.State, error) { return h.st, nil },
		notify: func(title, body string) { h.notices = append(h.notices, body) },
	}
	return h
}

func (h *loopHarness) autoOn() { h.st.SetAutoUpdate(true) }

func (h *loopHarness) tick() { h.loop.tick(context.Background()) }

func (h *loopHarness) update() status.Update { return h.sink.updateCopy() }

func aged(tag string, age time.Duration) updatecheck.Release {
	return release(tag, loopNow.Add(-age))
}

func TestLoopNotifiesOncePerVersionAcrossTicks(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	h.tick()
	h.tick()
	want := "chottag 0.6.0 is available. Run: chottag update"
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices = %q, want one %q", h.notices, want)
	}
	if u := h.update(); !u.Available || u.Latest != "0.6.0" || u.Notified != "0.6.0" || !u.CheckedAt.Equal(loopNow) {
		t.Errorf("update = %+v", u)
	}
}

func TestLoopNotifiesOncePerVersionAcrossARestart(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	h.tick()
	h.sink.Close()
	sink2, err := newStatusSink(h.home, nil) // a new daemon generation reads status.json
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink2.Close)
	h.loop.sink = sink2
	h.tick()
	if len(h.notices) != 1 {
		t.Fatalf("notices = %q, want exactly one across the restart", h.notices)
	}
}

func TestLoopNotifiesAgainForANewerVersion(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	h.tick()
	updateFetch = func(context.Context, *url.URL, string) (updatecheck.Release, error) {
		return aged("v0.6.1", time.Hour), nil
	}
	h.tick()
	if len(h.notices) != 2 || !strings.Contains(h.notices[1], "0.6.1") {
		t.Fatalf("notices = %q", h.notices)
	}
}

func TestLoopNoNoticeWhenNotNewerDraftOrPrerelease(t *testing.T) {
	for name, rel := range map[string]updatecheck.Release{
		"same":       aged("v0.5.0", time.Hour),
		"older":      aged("v0.4.0", time.Hour),
		"draft":      {Tag: "v0.6.0", Version: "0.6.0", PublishedAt: loopNow.Add(-time.Hour), Draft: true},
		"prerelease": {Tag: "v0.6.0", Version: "0.6.0", PublishedAt: loopNow.Add(-time.Hour), Prerelease: true},
	} {
		h := newLoopHarness(t, "0.5.0", rel, nil)
		h.autoOn()
		h.tick()
		if len(h.notices) != 0 || len(h.runs) != 0 || h.update().Available {
			t.Errorf("%s: notices %q runs %q update %+v, want none", name, h.notices, h.runs, h.update())
		}
	}
}

func TestLoopFetchErrorKeepsLatestAndSetsError(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	h.tick()
	updateFetch = func(context.Context, *url.URL, string) (updatecheck.Release, error) {
		return updatecheck.Release{}, errors.New("403 rate limit")
	}
	h.autoOn()
	h.tick()
	u := h.update()
	if u.Latest != "0.6.0" || u.Error != "403 rate limit" || len(h.runs) != 0 || len(h.notices) != 1 {
		t.Fatalf("update %+v runs %q notices %q", u, h.runs, h.notices)
	}
}

func TestLoopSwitchOffSkipsTheTick(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	h.st.SetUpdateCheck(false)
	h.tick()
	if h.fetches != 0 || len(h.notices) != 0 {
		t.Fatalf("fetches %d notices %q, want a skipped tick", h.fetches, h.notices)
	}
}

func TestLoopUnreadableStateSkipsTheTick(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	h.loop.state = func() (store.State, error) { return store.State{}, errors.New("boom") }
	h.tick()
	if h.fetches != 0 {
		t.Fatal("fetched with an unreadable state.json")
	}
}

func TestNewUpdateLoopEligibility(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	home := t.TempDir()
	mk := func() *updateLoop { return newUpdateLoop(home, nil, nil, nil, nil, nil) }
	Version = "dev"
	if mk() != nil {
		t.Error("a dev build must never check")
	}
	Version = "0.5.0"
	t.Setenv(noUpdateCheckEnv, "1")
	if mk() != nil {
		t.Error("CHOTTAG_NO_UPDATE_CHECK=1 must stop the loop")
	}
	t.Setenv(noUpdateCheckEnv, "0")
	if mk() == nil {
		t.Error("a release build with the env var not 1 must check")
	}
	t.Setenv(noUpdateCheckEnv, "")
	if mk() == nil {
		t.Error("a release build must check")
	}
}

func TestLoopDevBuildNeverAutoInstalls(t *testing.T) {
	// A dev build has no loop at all, so there is nothing to tick.
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "dev"
	var l *updateLoop = newUpdateLoop(t.TempDir(), nil, nil, nil, nil, nil)
	l.Run(context.Background()) // a nil loop returns at once
}

func TestLoopAutoOffNeverRunsTheChild(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.tick()
	if len(h.runs) != 0 {
		t.Fatalf("runs = %q", h.runs)
	}
}

func TestLoopAutoRefusals(t *testing.T) {
	tried := &status.AutoAttempt{Version: "0.6.0", At: loopNow.Add(-time.Hour), OK: false, Error: "update_failed"}
	for name, c := range map[string]struct {
		ver  string
		rel  updatecheck.Release
		auto *status.AutoAttempt
	}{
		"major jump":      {"0.9.0", aged("v1.0.0", 48*time.Hour), nil},
		"major jump 1->2": {"1.4.0", aged("v2.0.0", 48*time.Hour), nil},
		"too young":       {"0.5.0", aged("v0.6.0", 23*time.Hour+59*time.Minute), nil},
		"no publish time": {"0.5.0", release("v0.6.0", time.Time{}), nil},
		"tried today":     {"0.5.0", aged("v0.6.0", 48*time.Hour), tried},
	} {
		h := newLoopHarness(t, c.ver, c.rel, nil)
		h.autoOn()
		h.runOK = true
		if c.auto != nil {
			h.sink.setUpdate(&status.Update{Auto: c.auto})
		}
		h.tick()
		if len(h.runs) != 0 {
			t.Errorf("%s: ran the child: %q", name, h.runs)
		}
	}
}

func TestLoopAutoAllowedAtTheBoundaries(t *testing.T) {
	for name, c := range map[string]struct {
		rel  updatecheck.Release
		auto *status.AutoAttempt
	}{
		"exactly 24h old":     {aged("v0.6.0", 24*time.Hour), nil},
		"tried 24h ago":       {aged("v0.6.0", 48*time.Hour), &status.AutoAttempt{Version: "0.6.0", At: loopNow.Add(-24 * time.Hour)}},
		"older version tried": {aged("v0.6.0", 48*time.Hour), &status.AutoAttempt{Version: "0.5.5", At: loopNow.Add(-time.Hour)}},
	} {
		h := newLoopHarness(t, "0.5.0", c.rel, nil)
		h.autoOn()
		h.runOK = true
		if c.auto != nil {
			h.sink.setUpdate(&status.Update{Auto: c.auto})
		}
		h.tick()
		if len(h.runs) != 1 {
			t.Errorf("%s: runs = %q, want one", name, h.runs)
		}
	}
}

func TestLoopAutoSuccess(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.st.SetAutoRestart(false)
	h.runOK = true
	h.tick()
	h.tick() // the same version again: not retried within 24 h
	self, _ := filepath.EvalSymlinks(filepath.Join(h.home, "bin", "chottag"))
	if len(h.runs) != 1 || h.runs[0] != self+" v0.6.0" {
		t.Fatalf("runs = %q, want the installed binary with v0.6.0", h.runs)
	}
	want := []string{
		"chottag updated to 0.6.0. The daemon uses it after: chottag daemon restart",
	}
	if strings.Join(h.notices, "|") != strings.Join(want, "|") {
		t.Fatalf("notices = %q, want %q", h.notices, want)
	}
	a := h.update().Auto
	if a == nil || a.Version != "0.6.0" || !a.OK || a.Error != "" || !a.At.Equal(loopNow) {
		t.Errorf("auto = %+v", a)
	}
}

// With auto-restart on, the daemon switches by itself: the notice says so.
func TestLoopAutoSuccessWithAutoRestartOnSaysTheDaemonSwitchesWhenIdle(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runOK = true
	h.tick()
	want := "chottag updated to 0.6.0. The daemon switches to it when Claude Code is idle."
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices = %q, want only %q", h.notices, want)
	}
}

func TestLoopAutoFailureNotifiesOnceAndRecords(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runWhy = "update_failed"
	h.tick()
	h.tick()
	if len(h.runs) != 1 {
		t.Fatalf("runs = %q, want one (no retry within 24 h)", h.runs)
	}
	want := "chottag could not update to 0.6.0: update_failed. Run: chottag update"
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices = %q, want only %q (no available notice before an install)", h.notices, want)
	}
	if a := h.update().Auto; a == nil || a.OK || a.Error != "update_failed" || a.Version != "0.6.0" {
		t.Errorf("auto = %+v", a)
	}
}

// Finding 5: another update running is a precondition, not a failure.
func TestLoopAutoUpdateInProgressIsSkippedQuietlyAndRetriedNextTick(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runWhy = "update_in_progress"
	h.tick()
	h.tick()
	if len(h.runs) != 2 {
		t.Fatalf("runs = %q, want a retry on the next tick", h.runs)
	}
	for _, n := range h.notices {
		if strings.Contains(n, "could not update") {
			t.Fatalf("notices = %q, want no failure notice", h.notices)
		}
	}
	if a := h.update().Auto; a != nil {
		t.Errorf("auto = %+v, want no record", a)
	}
}

// Finding 3: a child that outlives its limit is a recorded timeout.
func TestLoopAutoChildTimeoutIsAFailure(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	old := autoUpdateTimeout
	autoUpdateTimeout = time.Millisecond
	t.Cleanup(func() { autoUpdateTimeout = old })
	autoUpdateRun = func(c context.Context, _, _ string) autoRun {
		<-c.Done() // a hang: only the limit ends it
		return autoRun{Reason: "update_failed"}
	}
	h.tick()
	if a := h.update().Auto; a == nil || a.OK || a.Error != "timeout" {
		t.Fatalf("auto = %+v, want a recorded failure with reason timeout", a)
	}
	want := "chottag could not update to 0.6.0: timeout. Run: chottag update"
	if len(h.notices) != 1 || h.notices[0] != want {
		t.Fatalf("notices = %q, want only %q", h.notices, want)
	}
}

func TestLoopAutoInstallingFlagIsOnOnlyWhileTheChildRuns(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	var during bool
	autoUpdateRun = func(context.Context, string, string) autoRun {
		during = autoInstalling.Load()
		return autoRun{OK: true, Installed: true}
	}
	h.tick()
	if !during || autoInstalling.Load() {
		t.Fatalf("flag during = %v after = %v, want true then false", during, autoInstalling.Load())
	}
}

func TestLoopAutoRetriesAfter24h(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runWhy = "update_failed"
	h.tick()
	updateLoopClock = func() time.Time { return loopNow.Add(24 * time.Hour) }
	h.tick()
	if len(h.runs) != 2 {
		t.Fatalf("runs = %q, want a retry after 24 h", h.runs)
	}
}

func TestLoopMissingInstalledBinaryFailsWithoutRunning(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	if err := os.Remove(filepath.Join(h.home, "bin", "chottag")); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if len(h.runs) != 0 {
		t.Fatalf("ran %q without an installed binary", h.runs)
	}
	if a := h.update().Auto; a == nil || a.OK || a.Error == "" {
		t.Errorf("auto = %+v, want a recorded failure", a)
	}
}

func TestLoopBadTagIsNeverPassedToTheChild(t *testing.T) {
	rel := aged("v0.6.0", 48*time.Hour)
	rel.Tag = "v0.6.0; rm -rf /"
	h := newLoopHarness(t, "0.5.0", rel, nil)
	h.autoOn()
	h.tick()
	if len(h.runs) != 0 {
		t.Fatalf("ran the child with %q", h.runs)
	}
}

func TestLoopCancelledAttemptRecordsNothing(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	ctx, cancel := context.WithCancel(context.Background())
	autoUpdateRun = func(context.Context, string, string) autoRun {
		cancel()
		return autoRun{Reason: "context canceled"}
	}
	h.loop.tick(ctx)
	if a := h.update().Auto; a != nil {
		t.Errorf("auto = %+v, want none for a cancelled attempt", a)
	}
	if len(h.notices) != 0 {
		t.Errorf("notices = %q, want none for a cancelled attempt", h.notices)
	}
}

func TestLoopRunScheduleAndShutdown(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", time.Hour), nil)
	oldTimer, oldJitter := newUpdateTimer, updateJitter
	t.Cleanup(func() { newUpdateTimer, updateJitter = oldTimer, oldJitter })
	fire := make(chan time.Time)
	waits := make(chan time.Duration, 4)
	newUpdateTimer = func(d time.Duration) (<-chan time.Time, func()) {
		waits <- d
		return fire, func() {}
	}
	updateJitter = func() time.Duration { return 17 * time.Minute }
	processed := make(chan struct{})
	h.loop.processed = processed
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.loop.Run(ctx); close(done) }()

	if d := <-waits; d != 2*time.Minute {
		t.Fatalf("first wait = %v, want 2m", d)
	}
	fire <- time.Time{}
	<-processed
	if d := <-waits; d != 6*time.Hour+17*time.Minute {
		t.Fatalf("second wait = %v, want 6h plus the jitter", d)
	}
	if h.fetches != 1 {
		t.Fatalf("fetches = %d after the first fire", h.fetches)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // hang guard only
		t.Fatal("the loop did not stop on shutdown")
	}
	if h.fetches != 1 {
		t.Errorf("fetches = %d, want no tick after shutdown", h.fetches)
	}
}

func TestLoopCancelsARunningChildOnShutdown(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	got := make(chan error, 1)
	autoUpdateRun = func(c context.Context, _, _ string) autoRun {
		close(started)
		<-c.Done()
		got <- c.Err()
		return autoRun{Reason: "canceled"}
	}
	done := make(chan struct{})
	go func() { h.loop.tick(ctx); close(done) }()
	<-started
	cancel()
	if err := <-got; err == nil {
		t.Fatal("the child's context was not cancelled")
	}
	<-done
}

func TestParseUpdateChildJSON(t *testing.T) {
	for _, c := range []struct {
		name   string
		out    string
		runErr error
		want   autoRun
	}{
		{"installed", `{"version":1,"ok":true,"installed":true,"warnings":[]}`, nil, autoRun{OK: true, Installed: true}},
		{"already up to date", `{"ok":true,"installed":false}`, nil, autoRun{OK: true}},
		{"ok but exit failed", `{"ok":true,"installed":true}`, errors.New("exit 1"), autoRun{Reason: "update_failed"}},
		{"code", `{"ok":false,"error":{"code":"update_in_progress","message":"another chottag update is running"}}`, errors.New("exit 1"), autoRun{Reason: "update_in_progress"}},
		{"generic code carries the message", `{"ok":false,"error":{"code":"update_failed","message":"gh is not installed; nothing was installed"}}`, errors.New("exit 1"), autoRun{Reason: "gh is not installed; nothing was installed"}},
		{"no code", `{"ok":false}`, errors.New("exit 1"), autoRun{Reason: "update_failed"}},
		{"not json", `panic: boom`, errors.New("exit 2"), autoRun{Reason: "update_failed"}},
		{"empty", ``, nil, autoRun{Reason: "update_failed"}},
	} {
		if got := parseUpdateChildJSON([]byte(c.out), c.runErr); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	long := `{"ok":false,"error":{"code":"update_failed","message":"` + strings.Repeat("x", 500) + `"}}`
	if got := parseUpdateChildJSON([]byte(long), errors.New("e")); len(got.Reason) > maxGHStderrLen {
		t.Errorf("reason is %d bytes, want at most %d", len(got.Reason), maxGHStderrLen)
	}
}

func TestAutoUpdateArgs(t *testing.T) {
	got := strings.Join(autoUpdateArgs("v0.6.0"), " ")
	if got != "update --version v0.6.0 --no-restart --json" {
		t.Fatalf("argv = %q", got)
	}
}

// shChild writes an executable /bin/sh script (never chottag) that records
// its argv and then runs body.
func shChild(t *testing.T, body string) (self, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	self = filepath.Join(dir, "chottag")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" + body + "\n"
	if err := os.WriteFile(self, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return self, argsFile
}

func TestRunAutoUpdateChildAgainstAShellFake(t *testing.T) {
	self, argsFile := shChild(t, `echo '{"ok":true,"installed":true}'`)
	got := runAutoUpdateChild(context.Background(), self, "v0.6.0")
	if !got.OK || !got.Installed {
		t.Fatalf("result = %+v", got)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "update\n--version\nv0.6.0\n--no-restart\n--json\n"; string(b) != want {
		t.Fatalf("argv = %q, want %q", b, want)
	}
}

func TestRunAutoUpdateChildFailureKeepsACappedSanitizedStderrLine(t *testing.T) {
	self, _ := shChild(t, `echo 'noise' >&2; echo 'gh: fetch https://u:pw@example.com/a?token=SECRET failed' >&2; exit 1`)
	got := runAutoUpdateChild(context.Background(), self, "v0.6.0")
	if got.OK || got.Reason != "update_failed" {
		t.Fatalf("result = %+v", got)
	}
	if !strings.Contains(got.Stderr, "gh: fetch") || strings.Contains(got.Stderr, "SECRET") || strings.Contains(got.Stderr, "pw") || strings.Contains(got.Stderr, "noise") {
		t.Fatalf("stderr line = %q, want the last line with its URL redacted", got.Stderr)
	}
}

func TestRunAutoUpdateChildStopsOnCancel(t *testing.T) {
	self, _ := shChild(t, `exec sleep 60`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan autoRun, 1)
	go func() { done <- runAutoUpdateChild(ctx, self, "v0.6.0") }()
	time.Sleep(200 * time.Millisecond) // let the child start; the assertion below does not depend on it
	cancel()
	select {
	case got := <-done:
		if got.OK {
			t.Fatalf("result = %+v", got)
		}
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatal("the cancelled child never returned")
	}
}

func TestLimitedWriterKeepsTheFirstBytesAndNeverFails(t *testing.T) {
	var buf bytes.Buffer
	w := &limitedWriter{w: &buf, left: 4}
	if n, err := w.Write([]byte("abcdefgh")); n != 8 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if n, err := w.Write([]byte("zz")); n != 2 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if buf.String() != "abcd" {
		t.Errorf("kept %q", buf.String())
	}
}

func TestInstalledChottagResolvesTheLink(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", updatecheck.Release{}, nil)
	got, err := installedChottag(h.home)
	want, _ := filepath.EvalSymlinks(filepath.Join(h.home, "bin", "chottag"))
	if err != nil || got != want {
		t.Fatalf("installedChottag = %q, %v, want %q", got, err, want)
	}
	if _, err := installedChottag(t.TempDir()); err == nil {
		t.Fatal("no error for a home without bin/chottag")
	}
}

// writtenSink is a status sink whose every completed write is announced on
// the returned channel, so a test waits on the event rather than on time.
func writtenSink(t *testing.T, home string) (*statusSink, <-chan struct{}) {
	t.Helper()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrote := make(chan struct{}, 16)
	sink.write = func(path string, b []byte) error {
		err := status.WriteBytes(path, b)
		wrote <- struct{}{}
		return err
	}
	t.Cleanup(sink.Close)
	return sink, wrote
}

func awaitWrite(t *testing.T, wrote <-chan struct{}) {
	t.Helper()
	select {
	case <-wrote:
	case <-time.After(10 * time.Second): // hang guard only
		t.Fatal("the sink never wrote status.json")
	}
}

// --- the sink keeps a newer `update --check` write ----------------------

func TestSinkKeepsANewerUpdateWrittenByTheCLI(t *testing.T) {
	home := t.TempDir()
	sink, wrote := writtenSink(t, home)
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sink.setUpdate(&status.Update{Latest: "0.6.0", CheckedAt: old, Available: true, Notified: "0.6.0"})
	awaitWrite(t, wrote)

	// `update --check` writes a newer check straight to disk.
	f, _ := status.Load(status.Path(home))
	f.Update.CheckedAt, f.Update.Latest = old.Add(time.Hour), "0.7.0"
	b, _ := status.Marshal(f)
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	sink.flush() // the sink's next flush must not overwrite it
	awaitWrite(t, wrote)
	got, _ := status.Load(status.Path(home))
	if got.Update == nil || got.Update.Latest != "0.7.0" {
		t.Fatalf("update on disk = %+v, want the CLI's newer 0.7.0 kept", got.Update)
	}
}

func TestSinkOverwritesAnOlderUpdateOnDisk(t *testing.T) {
	home := t.TempDir()
	sink, wrote := writtenSink(t, home)
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	sink.setUpdate(&status.Update{Latest: "0.7.0", CheckedAt: at})
	awaitWrite(t, wrote)
	f, _ := status.Load(status.Path(home))
	f.Update = &status.Update{Latest: "0.6.0", CheckedAt: at.Add(-time.Hour)}
	b, _ := status.Marshal(f)
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	sink.flush()
	awaitWrite(t, wrote)
	got, _ := status.Load(status.Path(home))
	if got.Update == nil || got.Update.Latest != "0.7.0" {
		t.Fatalf("update on disk = %+v, want the sink's 0.7.0", got.Update)
	}
}

// --- the daemon's notice -------------------------------------------------

func TestDaemonNotifyPostHonoursSwitchAndLabel(t *testing.T) {
	n := newRecordingNotifier()
	st := store.Default()
	st.Label = "dev"
	dn := newDaemonNotify(func() (store.State, error) { return st, nil }, n)
	defer dn.Close()
	dn.post("chottag: update available", "chottag 0.6.0 is available. Run: chottag update")
	st.SetNotify(false)
	dn.post("chottag: update available", "dropped")
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag · dev: update available" || got[0].body != "chottag 0.6.0 is available. Run: chottag update" {
		t.Fatalf("notices = %+v", got)
	}
	var nilDN *daemonNotify
	nilDN.post("t", "b") // a nil notifier does nothing
}

// --- review round 1 -------------------------------------------------------

func TestLoopNoRerunAndNoSecondNoticeAfterASuccessfulInstall(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runOK = true
	h.tick()
	h.installed = "0.6.0" // the daemon still runs 0.5.0; 0.6.0 is on disk
	updateLoopClock = func() time.Time { return loopNow.Add(25 * time.Hour) }
	h.tick()
	if len(h.runs) != 1 {
		t.Fatalf("runs = %q, want one", h.runs)
	}
	if len(h.notices) != 1 {
		t.Fatalf("notices = %q, want the updated notice only", h.notices)
	}
}

func TestLoopSuccessRecordGuardsEvenWhenTheInstalledVersionIsUnknown(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runOK = true
	h.installed = ""
	h.tick()
	updateLoopClock = func() time.Time { return loopNow.Add(30 * time.Hour) }
	h.tick()
	if len(h.runs) != 1 || len(h.notices) != 1 {
		t.Fatalf("runs %q notices %q", h.runs, h.notices)
	}
}

func TestLoopAHandInstalledVersionIsNeitherAvailableNorUpdated(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.installed = "0.6.0" // a person ran chottag update first
	h.tick()
	if len(h.notices) != 0 || len(h.runs) != 0 || h.update().Available {
		t.Fatalf("notices %q runs %q update %+v, want nothing", h.notices, h.runs, h.update())
	}
}

func TestLoopAnInstalledNothingIsNotAnUpdatedNotice(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runNoop = true
	h.tick()
	if len(h.notices) != 0 {
		t.Fatalf("notices = %q, want none: nothing was installed and the install was announced by nobody", h.notices)
	}
	updateLoopClock = func() time.Time { return loopNow.Add(30 * time.Hour) }
	h.tick()
	if len(h.runs) != 1 {
		t.Fatalf("runs = %q, want no daily re-run after an ok", h.runs)
	}
}

func TestLoopRefusesASemverPrereleaseWithoutTheFlag(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0-rc.1", 48*time.Hour), nil)
	h.autoOn()
	h.runOK = true
	h.tick()
	if len(h.runs) != 0 || len(h.notices) != 0 || h.update().Available {
		t.Fatalf("runs %q notices %q update %+v", h.runs, h.notices, h.update())
	}
	rel := aged("v0.6.0-rc.1", 48*time.Hour)
	if h.loop.autoAllowed(status.Update{}, rel, loopNow) {
		t.Fatal("autoAllowed accepted a pre-release tag")
	}
}

func TestLoopAPersistentFailureNotifiesOnceAndRetriesQuietly(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runWhy = "gh is not installed"
	h.tick()
	updateLoopClock = func() time.Time { return loopNow.Add(24 * time.Hour) }
	h.tick()
	if len(h.runs) != 2 {
		t.Fatalf("runs = %q, want a quiet retry", h.runs)
	}
	var failed int
	for _, n := range h.notices {
		if strings.Contains(n, "could not update") {
			failed++
			if !strings.Contains(n, "gh is not installed") {
				t.Errorf("notice %q lost the reason", n)
			}
		}
	}
	if failed != 1 {
		t.Fatalf("notices = %q, want one failure notice", h.notices)
	}
	h.runWhy = "update_failed" // a different reason is not news (N3): told once per version
	updateLoopClock = func() time.Time { return loopNow.Add(48 * time.Hour) }
	h.tick()
	failed = 0
	for _, n := range h.notices {
		if strings.Contains(n, "could not update") {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("notices = %q, want still one failure notice", h.notices)
	}
}

func TestLoopPassesTheTagItValidated(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runOK = true
	h.tick()
	if len(h.runs) != 1 || !strings.HasSuffix(h.runs[0], " v0.6.0") {
		t.Fatalf("runs = %q", h.runs)
	}
	rel := aged("v0.6.0", 48*time.Hour)
	rel.Tag = "0.6.0" // no leading v: refused, never rewritten
	h2 := newLoopHarness(t, "0.5.0", rel, nil)
	h2.autoOn()
	h2.runOK = true
	h2.tick()
	if len(h2.runs) != 0 {
		t.Fatalf("ran with %q", h2.runs)
	}
	if a := h2.update().Auto; a == nil || a.OK || a.Error == "" {
		t.Errorf("auto = %+v, want a recorded refusal", a)
	}
}

func TestLoopLogsTheChildsStderrOnFailure(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runWhy = "update_failed"
	h.tick()
	if !strings.Contains(h.logged.String(), "child said: boom") {
		t.Fatalf("log = %q", h.logged.String())
	}
}

func TestSinkMergesOnlyTheCLIOwnedFields(t *testing.T) {
	home := t.TempDir()
	sink, wrote := writtenSink(t, home)
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	auto := &status.AutoAttempt{Version: "0.6.0", At: old, OK: false, Error: "boom"}
	sink.setUpdate(&status.Update{Latest: "0.6.0", CheckedAt: old, Notified: "0.6.0", Auto: auto})
	awaitWrite(t, wrote)
	// A CLI check, newer, that read the file before the daemon's notified/auto landed.
	f, _ := status.Load(status.Path(home))
	f.Update = &status.Update{Latest: "0.7.0", CheckedAt: old.Add(time.Hour), Available: true}
	b, _ := status.Marshal(f)
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	sink.flush()
	awaitWrite(t, wrote)
	got, _ := status.Load(status.Path(home))
	u := got.Update
	if u == nil || u.Latest != "0.7.0" || !u.Available || u.Notified != "0.6.0" || u.Auto == nil || u.Auto.Error != "boom" {
		t.Fatalf("update on disk = %+v (auto %+v), want the CLI's check with the daemon's notified and auto", u, u.Auto)
	}
}

func TestSinkRequestPathNeverReadsTheDisk(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Close)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sink.loadUpdate = func(string) *status.Update {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}
	t.Cleanup(func() { close(release) })
	sink.flush() // wakes the writer, which parks inside the disk read
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never read the disk")
	}
	done := make(chan struct{})
	go func() {
		sink.flush()
		sink.maybeSave()
		sink.setTokenState("A", "ok")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second): // hang guard only
		t.Fatal("a request-path call waited on the disk read")
	}
}

// N1/N2: installedVersionOf on a real temp home.
func TestInstalledVersionOf(t *testing.T) {
	type tc struct {
		name    string
		link    string // where bin/chottag points, relative to home; "" for no link
		made    bool   // create the link's target
		record  string // install.json version; "" for no record
		want    string
		wantErr bool
	}
	for _, c := range []tc{
		{"a link into versions wins over the record", "versions/0.6.0/chottag", true, "v0.5.0", "0.6.0", false},
		{"a link into versions, no record", "versions/0.6.0/chottag", true, "", "0.6.0", false},
		{"a link outside versions falls back to the record", "src/1.0.0/chottag", true, "v0.5.0", "0.5.0", false},
		{"a link into versions with a non-version name falls back", "versions/scratch/chottag", true, "v0.5.0", "0.5.0", false},
		{"a broken link falls back to the record", "versions/0.6.0/chottag", false, "v0.5.0", "0.5.0", false},
		{"neither is an error", "", false, "", "", true},
		{"a link outside versions and no record is an error", "src/1.0.0/chottag", true, "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := t.TempDir()
			if c.link != "" {
				target := filepath.Join(h, c.link)
				if c.made {
					if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, []byte("x"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.MkdirAll(filepath.Join(h, "versions"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(h, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(h, "bin", "chottag")); err != nil {
					t.Fatal(err)
				}
			}
			if c.record != "" {
				if err := writeInstallRecord(h, installRecord{Version: c.record, Repo: "Acme/chottag", Source: "release"}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := installedVersionOf(h)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("installedVersionOf = %q, %v; want %q (error %v)", got, err, c.want, c.wantErr)
			}
		})
	}
}

// N3: a failure is told once per version while it keeps failing, whatever
// its reason; the record keeps the latest reason.
func TestLoopAFailureWithAChangingReasonIsStillToldOnce(t *testing.T) {
	h := newLoopHarness(t, "0.5.0", aged("v0.6.0", 48*time.Hour), nil)
	h.autoOn()
	h.runWhy = "could not checksum /tmp/a1"
	h.tick()
	h.runWhy = "could not checksum /tmp/b2"
	updateLoopClock = func() time.Time { return loopNow.Add(24 * time.Hour) }
	h.tick()
	var failed int
	for _, n := range h.notices {
		if strings.Contains(n, "could not update") {
			failed++
		}
	}
	if len(h.runs) != 2 || failed != 1 {
		t.Fatalf("runs = %d failure notices = %d (%q), want 2 runs and 1 notice", len(h.runs), failed, h.notices)
	}
	if u := h.update(); u.Auto == nil || u.Auto.Error != "could not checksum /tmp/b2" {
		t.Fatalf("auto = %+v, want the latest full reason recorded", u.Auto)
	}
}

// N4: a stderr tail cut mid-line starts at the next full line, or says nothing.
func TestTailWriterDropsAPartialFirstLine(t *testing.T) {
	tw := &tailWriter{max: 16}
	tw.Write([]byte("0123456789abcdefghij\nlast line\n"))
	if got := tw.text(); got != "last line\n" {
		t.Fatalf("text = %q, want only the full last line", got)
	}
	tw = &tailWriter{max: 8}
	tw.Write([]byte("pw@host/a?token=SECRET-and-a-very-long-line"))
	if got := tw.text(); got != "" {
		t.Fatalf("text = %q, want nothing when no newline survives", got)
	}
	tw = &tailWriter{max: 64}
	tw.Write([]byte("whole\n"))
	if got := tw.text(); got != "whole\n" {
		t.Fatalf("text = %q, want an undropped tail kept whole", got)
	}
}

func TestRunAutoUpdateChildLogsNothingFromAnOverlongLastLine(t *testing.T) {
	self, _ := shChild(t, `{ printf 'https://u:pw@example.com/a?token=SECRET'; i=0; while [ $i -lt 5000 ]; do printf x; i=$((i+1)); done; echo; } >&2; exit 1`)
	got := runAutoUpdateChild(context.Background(), self, "v0.6.0")
	if got.Stderr != "" || strings.Contains(got.Stderr, "SECRET") {
		t.Fatalf("stderr line = %q, want none", got.Stderr)
	}
}

// Finding 8: runProxyWithSignal builds the update loop with the resolved
// upstream, starts it, and joins it at shutdown.
func TestRunProxyStartsTheUpdateLoopWithTheUpstreamAndStopsItOnShutdown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv(noUpdateCheckEnv, "")
	oldVer, oldTimer, oldFetch, oldInst, oldTick := Version, newUpdateTimer, updateFetch, installedVersion, newRestartTicker
	t.Cleanup(func() {
		Version, newUpdateTimer, updateFetch, installedVersion, newRestartTicker = oldVer, oldTimer, oldFetch, oldInst, oldTick
	})
	oldClock, oldJitter := updateLoopClock, updateJitter
	t.Cleanup(func() { updateLoopClock, updateJitter = oldClock, oldJitter })
	updateJitter = func() time.Duration { return 0 }
	updateLoopClock = func() time.Time { return loopNow }
	Version = "0.5.0"
	installedVersion = func(string) (string, error) { return "0.5.0", nil }
	newRestartTicker = func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	waits := make(chan time.Duration, 4)
	fire := make(chan time.Time)
	newUpdateTimer = func(d time.Duration) (<-chan time.Time, func()) {
		waits <- d
		return fire, func() {}
	}
	type fetched struct {
		upstream string
		repo     string
	}
	started := make(chan fetched, 1)
	stopped := make(chan struct{})
	updateFetch = func(ctx context.Context, up *url.URL, repo string) (updatecheck.Release, error) {
		f := fetched{repo: repo}
		if up != nil {
			f.upstream = up.String()
		}
		started <- f
		<-ctx.Done() // returns only when the daemon stops the loop
		close(stopped)
		return updatecheck.Release{}, ctx.Err()
	}
	stubDaemonNotifier(t)

	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", "", "--upstream-proxy", "http://127.0.0.1:3128"}, io.Discard, newSyncBuf(), nil, sig)
	}()
	if d := <-waits; d != updateFirstCheck {
		t.Fatalf("first wait = %v, want %v: the update loop was not started", d, updateFirstCheck)
	}
	fire <- time.Time{}
	got := <-started
	if got.upstream != "http://127.0.0.1:3128" {
		t.Errorf("the check used upstream %q, want the daemon's resolved --upstream-proxy", got.upstream)
	}
	sig <- os.Interrupt
	select {
	case <-stopped:
	case <-time.After(shutdownGrace + 5*time.Second): // hang guard only
		t.Fatal("the update loop was not stopped on shutdown")
	}
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Errorf("runProxyWithSignal = %d", code)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("the daemon did not return")
	}
}
