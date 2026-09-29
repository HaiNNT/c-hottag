package notify

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sendFunc adapts a func to Notifier.
type sendFunc func(ctx context.Context, title, body string) error

func (f sendFunc) Send(ctx context.Context, title, body string) error { return f(ctx, title, body) }

// recorder delivers every notice onto a channel, in order.
type recorder struct{ got chan [2]string }

func newRecorder() *recorder { return &recorder{got: make(chan [2]string, 64)} }

func (r *recorder) Send(_ context.Context, title, body string) error {
	r.got <- [2]string{title, body}
	return nil
}

func nextNotice(t *testing.T, ch <-chan [2]string) [2]string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no notice was delivered")
		return [2]string{}
	}
}

func TestDispatcherDeliversInOrder(t *testing.T) {
	rec := newRecorder()
	d := NewDispatcher(rec, time.Second)
	defer d.Close()
	d.Enqueue("t1", "b1")
	d.Enqueue("t2", "b2")
	if m := nextNotice(t, rec.got); m != [2]string{"t1", "b1"} {
		t.Fatalf("first = %q, want t1/b1", m)
	}
	if m := nextNotice(t, rec.got); m != [2]string{"t2", "b2"} {
		t.Fatalf("second = %q, want t2/b2", m)
	}
	if got := d.Errors(); got != 0 {
		t.Fatalf("Errors = %d, want 0", got)
	}
}

// TestEnqueueNeverBlocksWhileTheNotifierHangs is Review Focus 3: a hung
// osascript never reaches the caller (a request goroutine), the overflow is
// counted, and Close still returns promptly.
func TestEnqueueNeverBlocksWhileTheNotifierHangs(t *testing.T) {
	entered := make(chan struct{}, 1)
	hang := sendFunc(func(ctx context.Context, _, _ string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	})
	d := NewDispatcher(hang, time.Hour)
	d.Enqueue("first", "")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sender never started the first notice")
	}
	// The sender holds "first", so the queue is empty with room for
	// QueueSize: exactly five of these overflow.
	start := time.Now()
	for i := 0; i < QueueSize+5; i++ {
		d.Enqueue("more", "")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("%d Enqueue calls took %v with the notifier hung, want them never to block", QueueSize+5, el)
	}
	if got := d.Errors(); got != 5 {
		t.Fatalf("Errors = %d, want 5: the queue holds QueueSize, the rest are dropped and counted", got)
	}
	closed := make(chan struct{})
	go func() { d.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a hung notifier")
	}
	// Every notice is delivered or counted once: 5 dropped, "first"
	// cancelled in flight, QueueSize skipped at Close.
	if got, want := d.Errors(), uint64(5+1+QueueSize); got != want {
		t.Fatalf("Errors after Close = %d, want %d", got, want)
	}
}

func TestSendTimesOutAndTheNextNoticeStillGoesOut(t *testing.T) {
	var calls atomic.Int32
	delivered := make(chan string, 4)
	n := sendFunc(func(ctx context.Context, title, _ string) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		delivered <- title
		return nil
	})
	d := NewDispatcher(n, 50*time.Millisecond)
	defer d.Close()
	d.Enqueue("hangs", "")
	d.Enqueue("after", "")
	select {
	case got := <-delivered:
		if got != "after" {
			t.Fatalf("delivered %q, want %q", got, "after")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung send held the dispatcher past its timeout")
	}
	if got := d.Errors(); got != 1 {
		t.Fatalf("Errors = %d, want 1 (the timed-out send)", got)
	}
}

func TestSendErrorIsCountedOnceAndNotRetried(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	sentinel := make(chan struct{})
	n := sendFunc(func(_ context.Context, title, _ string) error {
		mu.Lock()
		calls[title]++
		mu.Unlock()
		if title == "sentinel" {
			close(sentinel)
			return nil
		}
		return errors.New("boom")
	})
	d := NewDispatcher(n, time.Second)
	defer d.Close()
	d.Enqueue("fails", "")
	d.Enqueue("sentinel", "")
	select {
	case <-sentinel:
	case <-time.After(5 * time.Second):
		t.Fatal("the sentinel was never sent")
	}
	mu.Lock()
	got := calls["fails"]
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the failing notice was sent %d times, want 1: never retried", got)
	}
	if e := d.Errors(); e != 1 {
		t.Fatalf("Errors = %d, want 1", e)
	}
}

// TestMissingOsascriptIsCountedNotRetried is Review Focus 3 through the real
// Osascript notifier: a missing binary is one error, one attempt.
func TestMissingOsascriptIsCountedNotRetried(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	sentinel := make(chan struct{})
	withRun(t, func(_ context.Context, _ string, args []string) error {
		title := args[len(args)-2]
		mu.Lock()
		calls[title]++
		mu.Unlock()
		if title == "sentinel" {
			close(sentinel)
			return nil
		}
		return &fs.PathError{Op: "fork/exec", Path: OsascriptPath, Err: fs.ErrNotExist}
	})
	d := NewDispatcher(Osascript{}, time.Second)
	defer d.Close()
	d.Enqueue("missing", "body")
	d.Enqueue("sentinel", "")
	select {
	case <-sentinel:
	case <-time.After(5 * time.Second):
		t.Fatal("the sentinel was never sent")
	}
	mu.Lock()
	got := calls["missing"]
	mu.Unlock()
	if got != 1 {
		t.Fatalf("osascript ran %d times for one notice, want 1", got)
	}
	if e := d.Errors(); e != 1 {
		t.Fatalf("Errors = %d, want 1", e)
	}
}

// TestNewDispatcherDefaultsNonPositiveTimeoutToSendTimeout is a fix-round-1
// finding: NewDispatcher(n, 0) (or a negative timeout) must not mean "no
// timeout" — context.WithTimeout(ctx, 0) would expire before the notifier
// ever runs.
func TestNewDispatcherDefaultsNonPositiveTimeoutToSendTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		d := NewDispatcher(sendFunc(func(context.Context, string, string) error { return nil }), timeout)
		if d.timeout != SendTimeout {
			t.Errorf("NewDispatcher(n, %v).timeout = %v, want SendTimeout (%v)", timeout, d.timeout, SendTimeout)
		}
		d.Close()
	}
}

func TestEnqueueAfterCloseIsIgnoredAndCloseIsIdempotent(t *testing.T) {
	var calls atomic.Int32
	d := NewDispatcher(sendFunc(func(context.Context, string, string) error {
		calls.Add(1)
		return nil
	}), time.Second)
	d.Close()
	d.Enqueue("late", "")
	d.Close()
	if calls.Load() != 0 || d.Errors() != 0 {
		t.Fatalf("calls = %d, Errors = %d after Close, want 0 and 0", calls.Load(), d.Errors())
	}
}
