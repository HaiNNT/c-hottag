package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tokens"
)

type sessionRig struct {
	w       *sessionWatcher
	log     *bytes.Buffer
	now     time.Time
	since   time.Time
	probes  int
	probed  bool // what the probe answers
	corr    bool // creds.SessionGoneCorrelated
	cleared int
	sup     string
	supErr  error
	stops   int
}

func newSessionRig() *sessionRig {
	g := &sessionRig{log: &bytes.Buffer{}, now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	g.w = &sessionWatcher{
		log:        g.log,
		since:      func() time.Time { return g.since },
		now:        func() time.Time { return g.now },
		probe:      func() bool { g.probes++; return g.probed },
		supervisor: func() (string, error) { return g.sup, g.supErr },
		stop:       func() { g.stops++ },
		correlated: func() bool { return g.corr },
		clear:      func() { g.cleared++ },
	}
	return g
}

const sessionEndedLine = "chottag: the login session this daemon started in has ended (the keychain is unreachable); exiting so the next claude starts a fresh daemon"

func TestSessionWatcherFiresOnlyOnALongStandingSignalAndAFailingProbe(t *testing.T) {
	g := newSessionRig()
	if g.w.look() || g.probes != 0 {
		t.Fatal("no signal must do nothing, not even probe")
	}
	g.since = g.now.Add(-59 * time.Second)
	if g.w.look() || g.probes != 0 || g.stops != 0 {
		t.Fatal("a signal younger than 60s must wait")
	}
	g.since = g.now.Add(-time.Minute)
	g.probed = true // a single failed probe elsewhere, now healthy: transient
	if g.w.look() || g.stops != 0 {
		t.Fatal("a passing fresh probe must not end the daemon")
	}
	if g.cleared != 1 {
		t.Fatalf("a passing probe with no correlation must clear the signal (cleared=%d)", g.cleared)
	}
	g.probed = false
	if !g.w.look() || g.stops != 1 {
		t.Fatalf("an old signal and a failing probe must stop the daemon (stops=%d)", g.stops)
	}
	if got := strings.TrimSpace(g.log.String()); got != sessionEndedLine {
		t.Fatalf("log = %q", got)
	}
}

func TestSessionWatcherSkipsUnderASupervisorAndLogsOnce(t *testing.T) {
	g := newSessionRig()
	g.since, g.sup = g.now.Add(-time.Hour), "launchd"
	for i := 0; i < 3; i++ {
		if g.w.look() {
			t.Fatal("must not fire under a supervisor")
		}
	}
	if g.stops != 0 || strings.Count(g.log.String(), "launchd supervises it") != 1 {
		t.Fatalf("stops=%d log=%q, want one line and no stop", g.stops, g.log.String())
	}
	// An unreadable lock record is not permission to exit.
	g2 := newSessionRig()
	g2.since, g2.supErr = g2.now.Add(-time.Hour), errors.New("record unreadable")
	if g2.w.look() || g2.stops != 0 {
		t.Fatal("must not fire when the supervisor cannot be read")
	}
}

func TestSessionWatcherRunIsDrivenByTicksAndEndsAfterFiring(t *testing.T) {
	g := newSessionRig()
	g.since = g.now.Add(-time.Hour)
	ticks := make(chan time.Time)
	processed := make(chan struct{})
	g.w.ticks, g.w.processed = ticks, processed
	done := make(chan struct{})
	go func() { g.w.Run(context.Background()); close(done) }()
	ticks <- time.Time{}
	<-processed
	<-done // fired: Run returned without another tick
	if g.stops != 1 {
		t.Fatalf("stops = %d", g.stops)
	}
}

// The wiring: the daemon ends gracefully (exit 0) when its watcher fires.
func TestRunDaemonEndsWhenTheSessionWatcherFires(t *testing.T) {
	sink, err := newStatusSink(t.TempDir(), func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	own, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	cache := store.NewCache(store.Store{Dir: t.TempDir()})
	g := newSessionRig()
	g.since = g.now.Add(-time.Hour)
	ticks := make(chan time.Time)
	g.w.ticks = ticks
	done := make(chan int, 1)
	go func() {
		done <- runDaemon(context.Background(), daemonDeps{
			Stdout: io.Discard, Stderr: io.Discard, Listen: "127.0.0.1:0",
			Sink: sink, Tokens: tokens.New(tokens.Config{}), Cache: cache, Owners: own,
			Session: g.w,
		})
	}()
	// stop is set by runDaemon; the watcher's own stop() must cancel it.
	ticks <- time.Time{}
	if code := <-done; code != 0 {
		t.Fatalf("runDaemon returned %d, want 0", code)
	}
}

func TestEvidenceReadLogsTheExitStatusOncePerSlot(t *testing.T) {
	var log bytes.Buffer
	read := evidenceRead(func(dir string) (creds.Token, error) {
		return creds.Token{}, fmt.Errorf("keychain item %q: exit status 44: %w", "svc", creds.ErrNoLogin)
	}, &log)
	read("/Users/alice/.chottag/accounts/B")
	read("/Users/alice/.chottag/accounts/B")
	read("/Users/alice/.chottag/accounts/C")
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "slot B") || !strings.Contains(lines[0], "exit status 44") || !strings.Contains(lines[1], "slot C") {
		t.Fatalf("log = %q, want one line per slot naming exit status 44", log.String())
	}
	log.Reset()
	ok := evidenceRead(func(string) (creds.Token, error) { return creds.Token{}, errors.New("exit status 36") }, &log)
	ok("/x/accounts/D")
	if log.Len() != 0 {
		t.Fatalf("a transient failure logged %q", log.String())
	}
}

// A slot reading ErrSessionGone is stale, never needs-login: no mark, no
// notice (R164).
func TestASessionGoneReadRecordsNoNeedsLoginAndNoNotice(t *testing.T) {
	g := newRefreshRig(t)
	tm := tokens.New(tokens.Config{
		Read: func(string) (creds.Token, error) {
			return creds.Token{}, fmt.Errorf("keychain item %q: exit status 44: %w", "svc", creds.ErrSessionGone)
		},
		Refresh:     failingRefresher{},
		LockPath:    func(dir string) string { return dir + "/.lock" },
		TryLock:     func(string) (func() error, bool, error) { return func() error { return nil }, true, nil },
		Now:         func() time.Time { return g.now },
		OnRefresh:   g.rr.onRefresh,
		AccountName: func(string) string { return "C" },
	})
	if st := tm.Status("/slots/C"); st.State != creds.StateStale {
		t.Fatalf("status = %+v, want stale", st)
	}
	tm.Warm(context.Background(), "/slots/C", 15*time.Minute)
	if g.token("C") == creds.StateNeedsLogin {
		t.Fatal("a dead session recorded needs-login")
	}
	if n := drainNotices(t, g.dn, g.rec); len(n) != 0 {
		t.Fatalf("notices = %+v, want none", n)
	}
}

type failingRefresher struct{}

func (failingRefresher) Refresh(context.Context, string) error { return errors.New("exit status 1") }

// Correlated slots fire the watcher even when the probe cannot see the
// problem, and without probing.
func TestSessionWatcherFiresOnCorrelationWithAPassingProbe(t *testing.T) {
	g := newSessionRig()
	g.since, g.probed, g.corr = g.now.Add(-time.Minute), true, true
	if !g.w.look() || g.stops != 1 || g.cleared != 0 {
		t.Fatalf("stops=%d cleared=%d, want fired and not cleared", g.stops, g.cleared)
	}
}
