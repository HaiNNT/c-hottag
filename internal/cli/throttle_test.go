package cli

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestLogErrorThrottle checks that trace-log write failures are reported to
// stderr at most once per 10s, so a full disk doesn't flood the terminal.
func TestLogErrorThrottle(t *testing.T) {
	var buf bytes.Buffer
	warn := logErrorThrottle(&buf)
	warn(errors.New("disk full"))
	warn(errors.New("disk full again"))
	warn(errors.New("disk full third"))

	out := buf.String()
	if n := strings.Count(out, "chottag: trace log write failed:"); n != 1 {
		t.Fatalf("want 1 printed line within the 10s window, got %d: %q", n, out)
	}
	if !strings.Contains(out, "disk full\n") {
		t.Fatalf("want first error's text, got %q", out)
	}
}

// TestOwnersSaveErrorThrottle checks that owners.json write failures are
// reported to stderr at most once per 10s, naming owners.json rather than
// the trace log (F72: own.SetOnError must not reuse logErrorThrottle,
// which names the wrong file for this failure).
func TestOwnersSaveErrorThrottle(t *testing.T) {
	var buf bytes.Buffer
	warn := ownersSaveErrorThrottle(&buf)
	warn(errors.New("disk full"))
	warn(errors.New("disk full again"))
	warn(errors.New("disk full third"))

	out := buf.String()
	if n := strings.Count(out, "chottag: owners.json write failed:"); n != 1 {
		t.Fatalf("want 1 printed line within the 10s window, got %d: %q", n, out)
	}
	if !strings.Contains(out, "disk full\n") {
		t.Fatalf("want first error's text, got %q", out)
	}
}

// TestOwnersReloadErrorThrottle checks that owners.json RELOAD failures
// (Item 6, M1c5b Task 3 fix round 1) are reported to stderr at most once per
// 10s, naming reload rather than write — a persistently corrupt file must
// not flood the terminal, and must not be blamed on the roster watch
// (runDaemon's own onError, which ownersTick deliberately bypasses for this
// error — see ownersTick's doc comment).
func TestOwnersReloadErrorThrottle(t *testing.T) {
	var buf bytes.Buffer
	warn := ownersReloadErrorThrottle(&buf)
	warn(errors.New("owners.json is corrupt"))
	warn(errors.New("owners.json is corrupt again"))
	warn(errors.New("owners.json is corrupt third"))

	out := buf.String()
	if n := strings.Count(out, "chottag: owners.json reload failed:"); n != 1 {
		t.Fatalf("want 1 printed line within the 10s window, got %d: %q", n, out)
	}
	if !strings.Contains(out, "owners.json is corrupt\n") {
		t.Fatalf("want first error's text, got %q", out)
	}
}

// TestUsageErrorThrottle checks that usage-callback failures are reported to
// stderr at most once per 10s, naming the usage callback rather than the
// trace log — a panicking OnUsage silently and permanently stops usage
// recording, and reporting that as a trace-log failure would send the
// reader to the wrong subsystem.
func TestUsageErrorThrottle(t *testing.T) {
	var buf bytes.Buffer
	warn := usageErrorThrottle(&buf)
	warn(errors.New("OnUsage callback panicked"))
	warn(errors.New("OnUsage callback panicked"))
	warn(errors.New("OnUsage callback panicked"))

	out := buf.String()
	if n := strings.Count(out, "chottag: usage callback failed:"); n != 1 {
		t.Fatalf("want 1 printed line within the 10s window, got %d: %q", n, out)
	}
	if !strings.Contains(out, "OnUsage callback panicked\n") {
		t.Fatalf("want first error's text, got %q", out)
	}
}

// TestThrottleDropsCallsInsideTheWindow guards the mechanism runProxy relies
// on to keep a selector.Event stream (one per request — a serving account
// stuck needs-login emits one every time) from flooding stderr: only the
// first call inside an interval window runs.
func TestThrottleDropsCallsInsideTheWindow(t *testing.T) {
	var n atomic.Int32
	f := throttle(time.Hour, func(int) { n.Add(1) })
	f(1)
	f(2)
	f(3)
	if got := n.Load(); got != 1 {
		t.Fatalf("calls that ran = %d, want 1 within the window", got)
	}
}
