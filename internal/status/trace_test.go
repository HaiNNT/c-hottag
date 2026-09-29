package status_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

var tracedAt = time.Date(2026, 9, 25, 10, 40, 0, 0, time.FixedZone("+07", 7*3600))

func TestSetLastTracedKeepsTheFirstRequestOfAVersion(t *testing.T) {
	var f status.File
	if _, ok := f.LastTraced(); ok {
		t.Fatal("a fresh File has a lastTraced")
	}
	if !f.SetLastTraced("2.1.282", tracedAt.Add(123*time.Millisecond)) {
		t.Fatal("the first version reported no change")
	}
	if f.SetLastTraced("2.1.282", tracedAt.Add(time.Hour)) {
		t.Fatal("the same version again reported a change")
	}
	lt, ok := f.LastTraced()
	if !ok || lt.ClaudeVersion != "2.1.282" || !lt.At.Equal(tracedAt) {
		t.Fatalf("lastTraced = %+v, want 2.1.282 at the FIRST request, to the second (%v)", lt, tracedAt)
	}
	if !f.SetLastTraced("2.1.283", tracedAt.Add(2*time.Hour)) {
		t.Fatal("a different version reported no change")
	}
	if lt, _ := f.LastTraced(); lt.ClaudeVersion != "2.1.283" || !lt.At.Equal(tracedAt.Add(2*time.Hour)) {
		t.Fatalf("lastTraced = %+v, want the new pair", lt)
	}
}

func TestTraceMarshalsToTheSpecShape(t *testing.T) {
	var f status.File
	f.SetLastTraced("2.1.282", tracedAt)
	b, err := json.Marshal(f.Trace)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"lastTraced":{"claudeVersion":"2.1.282","at":"2026-09-25T10:40:00+07:00"}}`; string(b) != want {
		t.Fatalf("trace = %s, want %s", b, want)
	}
	doc, err := status.Marshal(status.File{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), `"trace"`) {
		t.Fatalf("a File with no trace wrote %s, want no trace key (additive only, §5.1)", doc)
	}
}

func TestLastTracedSurvivesAWriteAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", "status.json")
	var f status.File
	f.SetLastTraced("2.1.282", tracedAt)
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(path, b); err != nil {
		t.Fatal(err)
	}
	g, err := status.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if lt, ok := g.LastTraced(); !ok || lt.ClaudeVersion != "2.1.282" || !lt.At.Equal(tracedAt) {
		t.Fatalf("loaded lastTraced = %+v, %v", lt, ok)
	}
}

func TestSetLastTracedNeverChangesACopy(t *testing.T) {
	var a status.File
	a.SetLastTraced("2.1.282", tracedAt)
	b := a
	a.SetLastTraced("2.1.283", tracedAt)
	if lt, _ := b.LastTraced(); lt.ClaudeVersion != "2.1.282" {
		t.Fatalf("a copy saw a later SetLastTraced: %+v", lt)
	}
}

func TestClearDaemonKeepsLastTraced(t *testing.T) {
	var f status.File
	f.SetDaemon(47850, 0, 0, 0, tracedAt)
	f.SetLastTraced("2.1.282", tracedAt)
	f.ClearDaemon()
	if _, ok := f.LastTraced(); !ok {
		t.Fatal("a clean stop erased lastTraced")
	}
}
