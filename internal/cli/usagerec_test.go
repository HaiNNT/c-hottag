package cli

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/msgusage"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

var recT0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func recNow() time.Time { return recT0 }

func fp(v float64) *float64 { return &v }

func TestRecorderWritesLiveEvent(t *testing.T) {
	l := usagelog.Open(t.TempDir())
	r := newUsageRecorder(l, recNow)
	r.Message("A", msgusage.Usage{ID: "msg_1", Model: "claude-opus-5-5", Speed: "fast", In: 2, Out: 84, CR: 5, CW5m: 1, CW1h: 3, Web: 4}, recT0)
	r.Close()
	evs, err := l.Events(recT0.Add(-time.Hour), recT0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := usagelog.Event{T: recT0, MID: "msg_1", Src: "live", Acct: "A", Model: "claude-opus-5-5", Speed: "fast", In: 2, Out: 84, CR: 5, CW5m: 1, CW1h: 3, Web: 4}
	if len(evs) != 1 || !evs[0].T.Equal(want.T) {
		t.Fatalf("got %+v", evs)
	}
	evs[0].T = want.T
	if evs[0] != want {
		t.Fatalf("got %+v want %+v", evs[0], want)
	}
}

func TestRecorderDropsWhenFull(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r := newUsageRecorderWith(func([]usagelog.Event) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}, func(usagelog.Snapshot) error { return nil }, recNow)
	u := msgusage.Usage{ID: "m", Model: "x"}
	r.Message("A", u, recT0)
	<-entered // the writer holds one item and is blocked
	for i := 0; i < usageQueueSize; i++ {
		r.Message("A", u, recT0)
	}
	if r.Dropped() != 0 {
		t.Fatalf("dropped %d before the queue was full", r.Dropped())
	}
	r.Message("A", u, recT0) // returns at once; the writer is blocked
	if r.Dropped() != 1 {
		t.Fatalf("dropped %d, want 1", r.Dropped())
	}
	close(release)
	r.Close()
}

func TestSnapshotThrottle(t *testing.T) {
	l := usagelog.Open(t.TempDir())
	r := newUsageRecorder(l, recNow)
	r.Snapshot("A", "max5x", 5, fp(10), fp(20), 7200, recT0)
	r.Snapshot("A", "max5x", 5, fp(11), fp(21), 7200, recT0.Add(5*time.Minute)) // throttled
	r.Snapshot("A", "max5x", 5, fp(12), fp(1), 14400, recT0.Add(6*time.Minute)) // r7 changed
	r.Snapshot("B", "pro", 1, nil, nil, 0, recT0)                               // nothing to record
	r.Close()
	got, err := l.Snapshots(recT0.Add(-time.Hour), recT0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].R7 != 7200 || got[1].R7 != 14400 {
		t.Fatalf("got %+v", got)
	}
}

func TestUsageHookFeedsSnapshot(t *testing.T) {
	l := usagelog.Open(t.TempDir())
	r := newUsageRecorder(l, recNow)
	state := func() (store.State, error) {
		return store.State{Accounts: []store.Account{{Name: "A", Plan: "max5x", Units: 7}}}, nil
	}
	sink, err := newStatusSink(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	sink.snap = snapshotFeed(r, state)
	hook := newUsageHook(state, sink, nil)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.25")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.5")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1900000000")
	hook("A", 200, h, proxy.UsageRequest{})
	r.Close()
	got, err := l.Snapshots(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	s := got[0]
	if s.Acct != "A" || s.Tier != "max5x" || s.Units != 7 || s.R7 != 1900000800 ||
		s.P7 == nil || *s.P7 != 50 || s.P5 == nil || *s.P5 != 25 {
		t.Fatalf("got %+v p5=%v p7=%v", s, s.P5, s.P7)
	}
}

func TestSnapshotRoundsTheResetToTheHour(t *testing.T) {
	l := usagelog.Open(t.TempDir())
	r := newUsageRecorder(l, recNow)
	// header and poll name the same reset 1s apart, then 30 min apart: one snapshot
	r.Snapshot("A", "max5x", 5, fp(10), fp(20), 7200+1, recT0)
	r.Snapshot("A", "max5x", 5, fp(10), fp(21), 7200-1, recT0.Add(2*time.Minute))
	r.Snapshot("A", "max5x", 5, fp(10), fp(22), 7200+600, recT0.Add(4*time.Minute))
	r.Close()
	got, err := l.Snapshots(recT0.Add(-time.Hour), recT0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].R7 != 7200 {
		t.Fatalf("got %+v", got)
	}
}
