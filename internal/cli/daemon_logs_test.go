package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/rotate"
)

// logsOut is a goroutine-safe stdout for `logs -f`. Every write wakes
// waitFor, so these tests wait on an event, not a sleep.
type logsOut struct {
	mu   sync.Mutex
	b    bytes.Buffer
	wake chan struct{}
}

func newLogsOut() *logsOut { return &logsOut{wake: make(chan struct{}, 1)} }

func (o *logsOut) Write(p []byte) (int, error) {
	o.mu.Lock()
	n, err := o.b.Write(p)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
	return n, err
}

func (o *logsOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.b.String()
}

func (o *logsOut) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !strings.Contains(o.String(), want) {
		select {
		case <-o.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %q; output so far %q", want, o.String())
		}
	}
}

// fastLogsPoll shortens -f's poll for the test and restores the value it
// replaced.
func fastLogsPoll(t *testing.T) {
	t.Helper()
	old := logsPoll
	logsPoll = 5 * time.Millisecond
	t.Cleanup(func() { logsPoll = old })
}

// writeDaemonLog creates home's daemon.log with content and returns its
// path.
func writeDaemonLog(t *testing.T, home, content string) string {
	t.Helper()
	p := filepath.Join(home, "daemon.log")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func logsNumberedLines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// logsFollowReady is the last line every follow test seeds daemon.log
// with. `logs -f -n 1` prints it once its initial read is done, and that is
// the event logsFollow waits for. Without it, a line appended before the
// initial read would count as history and never be printed as new.
const logsFollowReady = "follow-ready\n"

// logsFollow runs `logs -f -n 1` in the background and waits until it has
// printed logsFollowReady. It returns the output and a stop func that
// cancels the follow and returns its exit code.
func logsFollow(t *testing.T) (*logsOut, func() int) {
	t.Helper()
	out := newLogsOut()
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- runDaemonLogs(ctx, []string{"-f", "-n", "1"}, newReporter(false, out, io.Discard)) }()
	stop := func() int {
		cancel()
		select {
		case c := <-code:
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("logs -f did not return within 10s of its context being cancelled")
			return -1
		}
	}
	t.Cleanup(func() { cancel() })
	out.waitFor(t, logsFollowReady)
	return out, stop
}

func TestDaemonLogsPrintsTheLastNLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeDaemonLog(t, home, logsNumberedLines(1, 60))
	var out bytes.Buffer
	if code := runDaemonCmd([]string{"logs", "-n", "3"}, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if want := logsNumberedLines(58, 60); out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestDaemonLogsDefaultsToFiftyLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeDaemonLog(t, home, logsNumberedLines(1, 60))
	var out bytes.Buffer
	if code := runDaemonLogs(context.Background(), nil, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if want := logsNumberedLines(11, 60); out.String() != want {
		t.Fatalf("stdout = %q, want lines 11-60", out.String())
	}
}

// The daemon may be mid-write: a last line with no newline yet is still a
// line.
func TestDaemonLogsCountsAnUnterminatedLastLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeDaemonLog(t, home, "a\nb\nc")
	var out bytes.Buffer
	if code := runDaemonLogs(context.Background(), []string{"-n", "2"}, newReporter(false, &out, io.Discard)); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out.String() != "b\nc" {
		t.Fatalf("stdout = %q, want %q", out.String(), "b\nc")
	}
}

func TestDaemonLogsWithoutALogSaysSoAndExitsZero(t *testing.T) {
	for _, args := range [][]string{nil, {"-f"}} {
		home := t.TempDir()
		t.Setenv("CHOTTAG_HOME", home)
		var out bytes.Buffer
		if code := runDaemonLogs(context.Background(), args, newReporter(false, &out, io.Discard)); code != 0 {
			t.Fatalf("logs %q: exit = %d, want 0", args, code)
		}
		if !strings.Contains(out.String(), "no daemon log yet") {
			t.Errorf("logs %q: stdout = %q, want \"no daemon log yet\"", args, out.String())
		}
	}
}

func TestDaemonLogsRejectsANegativeCount(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	if code := runDaemonLogs(context.Background(), []string{"-n", "-1"}, newReporter(false, io.Discard, io.Discard)); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
}

// Fix round 4, item 3: `daemon logs` used a bare flags.Parse, which stops at
// the first non-flag token; a stray positional ahead of -f must not leave
// -f (or the daemon.log content) reaching stdout — the whole call is a
// usage error, and nothing is printed.
func TestDaemonLogsRejectsAStrayPositionalAheadOfAFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	writeDaemonLog(t, home, "a\nb\nc\n")
	var out bytes.Buffer
	if code := runDaemonLogs(context.Background(), []string{"x", "-f"}, newReporter(false, &out, io.Discard)); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty: a stray positional must refuse before printing anything", out.String())
	}
}

func TestDaemonLogsFollowPrintsAppendedLinesAndExitsZeroWhenCancelled(t *testing.T) {
	fastLogsPoll(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	path := writeDaemonLog(t, home, "old line\n"+logsFollowReady)
	out, stop := logsFollow(t)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "new line 1")
	out.waitFor(t, "new line 1\n")
	fmt.Fprintln(f, "new line 2")
	out.waitFor(t, "new line 2\n")

	if code := stop(); code != 0 {
		t.Errorf("exit after cancel = %d, want 0 (spec §5: Ctrl-C exits 0)", code)
	}
	if strings.Contains(out.String(), "old line") {
		t.Errorf("output = %q: -n 1 must print only the last existing line", out.String())
	}
}

// Rotation exactly as the daemon does it: a real rotate.Writer, with a
// small MaxBytes so it rolls daemon.log onto daemon.log.1 (a rename, then a
// fresh file with a new inode) several times. Each line is waited for
// before the next is written, so no two rotations can land inside one
// poll.
func TestDaemonLogsFollowSurvivesRotation(t *testing.T) {
	fastLogsPoll(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	path := writeDaemonLog(t, home, logsFollowReady)
	out, stop := logsFollow(t)

	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 64, Keep: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var want strings.Builder
	want.WriteString(logsFollowReady)
	for i := 1; i <= 12; i++ {
		line := fmt.Sprintf("rotated line %02d\n", i)
		if _, err := io.WriteString(w, line); err != nil {
			t.Fatal(err)
		}
		want.WriteString(line)
		out.waitFor(t, line)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("daemon.log never rotated (%v): this test did not exercise rotation", err)
	}
	stop()
	if out.String() != want.String() {
		t.Errorf("output = %q, want every line once, in order: %q", out.String(), want.String())
	}
}

// An operator truncating daemon.log by hand (`: > daemon.log`). The file
// shrinks below what was already read, so the follower must start from
// the top again.
func TestDaemonLogsFollowSurvivesTruncation(t *testing.T) {
	fastLogsPoll(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	path := writeDaemonLog(t, home, logsFollowReady)
	out, stop := logsFollow(t)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "before truncation, long enough to matter")
	out.waitFor(t, "before truncation, long enough to matter\n")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "after")
	out.waitFor(t, "after\n")
	stop()
}
