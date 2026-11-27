package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

func TestUsageTickRunsOnceAtStartAndOnEveryTick(t *testing.T) {
	home := t.TempDir()
	hp := t.TempDir()
	prev := homeProjectsDir
	homeProjectsDir = func() string { return hp }
	t.Cleanup(func() { homeProjectsDir = prev })
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h.jsonl", "2026-11-20T10:00:00Z", "t1")

	ticks := make(chan time.Time)
	passed := make(chan struct{})
	u := &usageTick{home: home, log: &bytes.Buffer{}, now: func() time.Time { return statsNow0 }, tick: ticks, passed: passed}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); u.Run(ctx) }()

	<-passed // the start pass
	if _, err := os.Stat(filepath.Join(home, "usage", "import.json")); err != nil {
		t.Fatalf("no import at start: %v", err)
	}
	select {
	case <-passed:
		t.Fatal("a second pass ran before any tick")
	default:
	}
	writeStatsTranscript(t, filepath.Join(hp, "p"), "h2.jsonl", "2026-11-21T10:00:00Z", "t2")
	ticks <- statsNow0
	<-passed
	days, err := usagelog.Open(home).Days(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %+v, want both transcripts imported", days)
	}
	cancel()
	<-done
}

func TestUsageTickLogsEachDistinctErrorOnce(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	u := &usageTick{home: home, log: &log, now: func() time.Time { return statsNow0 }}
	u.pass(context.Background())
	first := log.String()
	if first == "" {
		t.Fatal("a failing pass logged nothing")
	}
	u.pass(context.Background())
	if log.String() != first {
		t.Fatalf("the same error was logged twice: %q", log.String())
	}
	// A pass that works clears the memory, so the error is reported again.
	if err := os.WriteFile(filepath.Join(home, "state.json"), mustStateJSON(t), 0o600); err != nil {
		t.Fatal(err)
	}
	u.pass(context.Background())
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	u.pass(context.Background())
	if log.String() == first {
		t.Fatal("the error was not logged again after a good pass")
	}
}

func mustStateJSON(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	if _, err := (store.Store{Dir: dir}).Update(func(*store.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
