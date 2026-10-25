package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

// R183: a refusal opens an episode, a success closes it. One log line opens
// and one closes it; the notice waits until a refusal arrives in an episode
// that is HomeLoginSettle old, comes once per episode and at most once an hour.
func TestHomeLoginWatchEpisodes(t *testing.T) {
	n := newRecordingNotifier()
	dn := newDaemonNotify(notifyTestState("A"), n)
	defer dn.Close()
	var log bytes.Buffer
	now := time.Date(2026, 10, 25, 15, 45, 0, 0, time.UTC)
	w := newHomeLoginWatch(&log, dn, func() time.Time { return now })

	if !w.refusedSince().IsZero() {
		t.Fatal("an episode is open before any refusal")
	}
	w.report(true) // a success with no episode is silent
	if log.Len() != 0 {
		t.Fatalf("log = %q, want nothing", log.String())
	}

	w.report(false)
	start := now
	if got := w.refusedSince(); !got.Equal(start) {
		t.Fatalf("since = %v, want %v", got, start)
	}
	wantOpen := "chottag: Claude Code's own login (Home) was refused (401); sessions will say \"Login expired\"; run /login in a Claude Code session\n"
	if log.String() != wantOpen {
		t.Fatalf("log = %q\nwant %q", log.String(), wantOpen)
	}
	now = now.Add(status.HomeLoginSettle - time.Second)
	w.report(false) // inside the settle time: same episode, no notice, no line
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("notice inside the settle time: %+v", got)
	}
	if !w.refusedSince().Equal(start) || log.String() != wantOpen {
		t.Fatalf("a repeat refusal restarted the episode or logged: since %v, log %q", w.refusedSince(), log.String())
	}

	now = now.Add(time.Second)
	w.report(false)
	w.report(false) // once per episode
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag: Claude Code's own login was refused" {
		t.Fatalf("notices = %+v, want exactly one", got)
	}

	now = now.Add(time.Minute)
	w.report(true)
	if !w.refusedSince().IsZero() {
		t.Fatal("a success left the episode open")
	}
	if !strings.HasSuffix(log.String(), "chottag: Claude Code's own login (Home) works again (refused since 15:45:00)\n") {
		t.Fatalf("log = %q, want the closing line last", log.String())
	}
	closed := log.Len()
	w.report(true)
	if log.Len() != closed {
		t.Fatalf("a second success logged: %q", log.String()[closed:])
	}

	// A second episode inside the hour: logged, but its notice waits for the hour.
	w.report(false)
	now = now.Add(time.Minute)
	w.report(false)
	if got := drainNotices(t, dn, n); len(got) != 0 {
		t.Fatalf("a second episode inside the hour sent %+v", got)
	}
	// The same episode, past the hour since the last notice, sends it.
	now = now.Add(time.Hour)
	w.report(false)
	if got := drainNotices(t, dn, n); len(got) != 1 {
		t.Fatalf("notices = %+v, want one once the hour passed", got)
	}
}

// The watch is nil-safe on its notifier, and its lines carry no secret.
func TestHomeLoginWatchWithoutNotifications(t *testing.T) {
	var log bytes.Buffer
	now := time.Date(2026, 10, 25, 15, 45, 0, 0, time.UTC)
	w := newHomeLoginWatch(&log, nil, func() time.Time { return now })
	w.report(false)
	now = now.Add(time.Minute)
	w.report(false)
	w.report(true)
	for _, bad := range []string{"Bearer", "sk-ant"} {
		if strings.Contains(log.String(), bad) {
			t.Fatalf("log carries %q: %q", bad, log.String())
		}
	}
}
