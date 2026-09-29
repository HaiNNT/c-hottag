package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

var traceNow = time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)

func TestTraceRoundTripsAndClearLeavesNoKey(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	until := traceNow.Add(time.Hour)
	if _, err := s.Update(func(st *store.State) error { st.SetTraceUntil(until); return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"until": "2026-09-25T11:00:00Z"`) {
		t.Fatalf("state.json = %s, want trace.until 2026-09-25T11:00:00Z", b)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Trace == nil || !st.Trace.Until.Equal(until) {
		t.Fatalf("loaded Trace = %+v, want until %v", st.Trace, until)
	}
	if _, err := s.Update(func(st *store.State) error { st.ClearTrace(); return nil }); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"trace"`) {
		t.Fatalf("state.json = %s, want no trace key after ClearTrace", b)
	}
}

func TestDefaultStateWritesNoTraceKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := (store.Store{Dir: dir}).Update(func(*store.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"trace"`) {
		t.Fatalf("state.json = %s, want no trace key until trace on", b)
	}
}

// TestTracingAtIsTrueOnlyBeforeUntil is Review Focus 1: the window ends by
// itself. At until exactly it is already off (After is strict).
func TestTracingAtIsTrueOnlyBeforeUntil(t *testing.T) {
	var st store.State
	if st.TracingAt(traceNow) {
		t.Fatal("TracingAt = true with no trace window")
	}
	until := traceNow.Add(time.Hour)
	st.SetTraceUntil(until)
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{traceNow, true},
		{until.Add(-time.Nanosecond), true},
		{until, false},
		{until.Add(time.Nanosecond), false},
		{until.Add(24 * time.Hour), false},
	} {
		if got := st.TracingAt(c.now); got != c.want {
			t.Errorf("TracingAt(until%+v) = %v, want %v", c.now.Sub(until), got, c.want)
		}
	}
}

func TestNullTraceIsOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"version":1,"accounts":[],"port":47821,"trace":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Store{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.TracingAt(traceNow) {
		t.Fatal(`"trace": null reads as tracing, want off`)
	}
}

func TestSetTraceUntilDoesNotAliasACopy(t *testing.T) {
	var a store.State
	a.SetTraceUntil(traceNow.Add(time.Hour))
	b := a
	a.SetTraceUntil(traceNow.Add(2 * time.Hour))
	if !b.Trace.Until.Equal(traceNow.Add(time.Hour)) {
		t.Fatalf("a copy saw a later SetTraceUntil: %v", b.Trace.Until)
	}
}
