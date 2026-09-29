package session_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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
