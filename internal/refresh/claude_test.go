package refresh_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/refresh"
)

func TestRefreshBuildsTheChildEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47821")
	t.Setenv("https_proxy", "http://127.0.0.1:47821")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/x/ca.pem")
	t.Setenv("PATH", "/usr/bin")
	var gotBin string
	var gotArgs, gotEnv []string
	c := refresh.Claude{
		Bin: "/usr/local/bin/claude",
		Run: func(_ context.Context, bin string, args, env []string) error {
			gotBin, gotArgs, gotEnv = bin, args, env
			return nil
		},
	}
	if err := c.Refresh(context.Background(), "/slots/C"); err != nil {
		t.Fatal(err)
	}
	if gotBin != "/usr/local/bin/claude" || strings.Join(gotArgs, " ") != "mcp list" {
		t.Fatalf("bin=%q args=%v", gotBin, gotArgs)
	}
	env := strings.Join(gotEnv, "\n")
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR=/slots/C") {
		t.Fatalf("CLAUDE_CONFIG_DIR missing: %v", gotEnv)
	}
	for _, banned := range []string{"HTTPS_PROXY=", "https_proxy=", "NODE_EXTRA_CA_CERTS="} {
		if strings.Contains(env, banned) {
			t.Errorf("child env still carries %s: %v", banned, gotEnv)
		}
	}
	if !strings.Contains(env, "PATH=/usr/bin") {
		t.Errorf("child env lost PATH: %v", gotEnv)
	}
}

func TestRefreshRunsTheBinaryAndReportsFailure(t *testing.T) {
	dir := t.TempDir()
	slot := t.TempDir() // the default Run now sets cmd.Dir = slot, so it must actually exist
	out := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "claude")
	os.WriteFile(script, []byte("#!/bin/sh\nprintenv CLAUDE_CONFIG_DIR > "+out+"\nexit ${FAIL:-0}\n"), 0o755)

	c := refresh.Claude{Bin: script}
	if err := c.Refresh(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil || strings.TrimSpace(string(b)) != slot {
		t.Fatalf("child saw CLAUDE_CONFIG_DIR=%q err=%v", b, err)
	}

	t.Setenv("FAIL", "3")
	err = c.Refresh(context.Background(), slot)
	if err == nil || !strings.Contains(err.Error(), slot) {
		t.Fatalf("failing child err = %v", err)
	}
}

// TestRefreshSucceedsWhenOnlyAGrandchildHoldsThePipesOpen: the script exits 0
// at once, and a backgrounded grandchild inherits the discard pipes and
// outlives WaitDelay. A real refresh can do the same (an MCP server
// subprocess spawned by `claude mcp list`).
//
// The property is an event, not a duration (F136): Refresh returns while
// the grandchild is still alive, so it did not wait for the grandchild to
// release the pipes. The script writes the grandchild's pid ($!) to a file
// before it exits, and Refresh cannot return before the script exits, so
// the pid is always there to read. The only timeout is a 60s hang guard,
// well short of the grandchild's 120s sleep, so a Refresh that waited for
// the grandchild fails the guard instead of passing late. The grandchild
// is this test's own descendant, and cleanup kills it.
func TestRefreshSucceedsWhenOnlyAGrandchildHoldsThePipesOpen(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	pidFile := filepath.Join(dir, "grandchild.pid")
	body := "#!/bin/sh\nsleep 120 &\necho $! > '" + pidFile + "'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if pid, err := readPID(pidFile); err == nil {
			syscall.Kill(pid, syscall.SIGKILL) // the test's own grandchild
		}
	})

	c := refresh.Claude{Bin: script, WaitDelay: 150 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- c.Refresh(context.Background(), t.TempDir()) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Refresh has not returned after 60s: it is waiting for the grandchild that holds its pipes (sleep 120) instead of returning after WaitDelay")
	}
	if err != nil {
		t.Fatalf("refresh reported failure despite a successful exit: %v", err)
	}

	pid, perr := readPID(pidFile)
	if perr != nil {
		t.Fatalf("the script's grandchild pid was not recorded: %v", perr)
	}
	if kerr := syscall.Kill(pid, 0); kerr != nil {
		t.Fatalf("grandchild pid %d was already gone when Refresh returned (%v): the test did not exercise a live pipe holder", pid, kerr)
	}
}

// readPID reads a pid written by `echo $! > file`.
func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("bad pid %d in %s", pid, path)
	}
	return pid, nil
}

func TestRefreshTimesOut(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	c := refresh.Claude{Bin: script, Timeout: 200 * time.Millisecond}
	start := time.Now()
	if err := c.Refresh(context.Background(), t.TempDir()); err == nil {
		t.Fatal("timed-out child reported success")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("timeout not enforced: took %s", d)
	}
}
