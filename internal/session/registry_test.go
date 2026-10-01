package session_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/session"
)

func TestLiveReportsAnAddedSessionWhoseProcessExists(t *testing.T) {
	r, err := session.Open(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	// Our own PID is certainly alive.
	if err := r.Add(os.Getpid(), 47821); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].PID != os.Getpid() || live[0].Port != 47821 {
		t.Fatalf("Live() = %+v, want one entry for pid %d port 47821", live, os.Getpid())
	}
}

// TestLivePrunesADeadSession pins the property that makes automatic port
// reassignment safe: a crashed session must not hold the port forever.
func TestLivePrunesADeadSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A PID that cannot be running: spawn a process and reap it.
	dead := spawnAndReap(t)
	if err := r.Add(dead, 47821); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("Live() = %+v, want empty: a dead session must be pruned", live)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("%d stale files left in run dir; Live must prune what it skips", len(ents))
	}
}

func TestRemoveDropsASession(t *testing.T) {
	r, err := session.Open(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(os.Getpid(), 47821); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("Live() = %+v after Remove, want empty", live)
	}
}

// spawnAndReap returns the PID of a process that has certainly exited.
func spawnAndReap(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestPutThenLiveRoundTripsEveryField(t *testing.T) {
	r, err := session.Open(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	want := session.Session{
		PID:     os.Getpid(),
		Port:    47850,
		SID:     "0123456789abcdef0123456789abcdef",
		Pool:    "default",
		Started: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	if err := r.Put(want); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].PID != want.PID || live[0].Port != want.Port ||
		live[0].SID != want.SID || live[0].Pool != want.Pool || !live[0].Started.Equal(want.Started) {
		t.Fatalf("Live() = %+v, want %+v", live, want)
	}
}

func TestLiveReadsAnOldEntryWithAnEmptySID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`{"pid":%d,"port":47821}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := r.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].SID != "" || live[0].Pool != "" || !live[0].Started.IsZero() || live[0].Port != 47821 {
		t.Fatalf("Live() = %+v, want one old-format entry with empty SID", live)
	}
}

func TestPutWritesMode0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Put(session.Session{PID: os.Getpid(), Port: 1, SID: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestPutRewritesAWiderExistingFileTo0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid()))
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(session.Session{PID: os.Getpid(), Port: 1}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("%d files in run dir, want 1 (no temp left)", len(ents))
	}
}

func TestAddWritesNoIdentityKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	r, err := session.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(os.Getpid(), 47821); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sid", "pool", "started"} {
		if _, ok := m[k]; ok {
			t.Fatalf("Add wrote key %q: %s", k, b)
		}
	}
}
