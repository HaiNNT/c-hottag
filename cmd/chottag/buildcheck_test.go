package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests drive the go tool itself: they check what `go build`
// produces, which no in-process test can see. `go test` puts
// $GOROOT/bin first on PATH, so "go" is the toolchain running this test.

// moduleRoot is the repository root, two levels above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// goToolChildEnv marks a process goTool started. goTool refuses to run
// inside one: a `go test` child that reached goTool again would be the
// F232 recursion. Today the only `go test` goTool runs is internal/cli's
// tagged suite, which never calls goTool, so this is a tripwire.
const goToolChildEnv = "CHOTTAG_GOTOOL_CHILD"

// goToolDeadline is goTool's hang guard when t itself carries no deadline
// (`go test` run without its own -timeout), and the ceiling goToolDeadlineFor
// never exceeds even when t.Deadline() would allow more: it sits under go
// test's own 10m default so a hung child is killed, with its whole process
// group, before this test binary is.
const goToolDeadline = 8 * time.Minute

// goToolMargin is subtracted from a deadline before it is handed to a
// child, twice over: once so goTool's own context expires before t's
// deadline would fail this test binary out from under the child, and
// again so a `go test` child's own -timeout fires (a clean per-test
// failure) before goTool's context would SIGKILL it (an ugly one).
const goToolMargin = 30 * time.Second

// goToolDeadlineFor is min(t.Deadline() - goToolMargin, goToolDeadline):
// tighter when t's own -timeout leaves less room, goToolDeadline when t
// carries no deadline at all.
func goToolDeadlineFor(t *testing.T) time.Duration {
	remaining := goToolDeadline
	if d, ok := t.Deadline(); ok {
		if left := time.Until(d) - goToolMargin; left < remaining {
			remaining = left
		}
	}
	return remaining
}

// goTool runs `go args...` in the module root and fails the test with the
// tool's output if it exits non-zero. env entries (KEY=VALUE) replace any
// inherited value. GOFLAGS is always cleared, so a developer's
// GOFLAGS=-tags=... can never turn a release build into a tagged one. When
// args starts "test", goTool also passes the child its own -timeout (the
// same deadline, less another goToolMargin), so a hung child test fails
// cleanly with its own stack trace before goTool's outer deadline would
// SIGKILL it instead.
//
// The child runs in its own process group. Past its deadline, the whole
// group gets SIGKILL: go, and the compilers and test binaries it started
// (F232: killing go alone orphans them). That group kill fires only when
// there is reason to think a member could still be alive — ctx was
// cancelled (the deadline fired), or Wait itself gave up waiting on a
// straggler's pipes (WaitDelay, wrapped as exec.ErrWaitDelay) — never
// after an ordinary reap, where the child (and by then, its own children)
// have already exited and the pid could in principle have been recycled.
func goTool(t *testing.T, env []string, args ...string) string {
	t.Helper()
	if os.Getenv(goToolChildEnv) != "" {
		t.Fatalf("goTool called inside a process goTool started (%s is set): refusing to nest go tool runs", goToolChildEnv)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is not on PATH: %v", err)
	}
	deadline := goToolDeadlineFor(t)
	if deadline <= 0 {
		t.Fatalf("go %s: t's own deadline leaves no budget for a go tool child (margin %s)", strings.Join(args, " "), goToolMargin)
	}
	if len(args) > 0 && args[0] == "test" {
		childTimeout := deadline - goToolMargin
		if childTimeout <= 0 {
			t.Fatalf("go %s: no budget left for the child's own -timeout after goTool's margin", strings.Join(args, " "))
		}
		withTimeout := make([]string, 0, len(args)+1)
		withTimeout = append(withTimeout, args[0], "-timeout="+childTimeout.String())
		withTimeout = append(withTimeout, args[1:]...)
		args = withTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Dir = moduleRoot(t)
	cmd.Env = withEnv(os.Environ(), append([]string{"GOFLAGS=", goToolChildEnv + "=1"}, env...))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if (ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay)) && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // leave no member of the group behind
	}
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("go %s did not finish within the %s hang guard:\n%s", strings.Join(args, " "), deadline, out)
	}
	if err != nil {
		t.Fatalf("go %s (%s): %v\n%s", strings.Join(args, " "), strings.Join(env, " "), err, out)
	}
	return string(out)
}

// touchSources os.ReadFiles every .go file under each dir (skipping vendor
// and testdata). Go's test cache can't see into a subprocess's own file
// reads, so a test that only drives the go tool in a subprocess (goTool,
// above) would otherwise report a cached "ok" for a `go test ./...` run
// after an edit to a file the subprocess reads but this test never opens
// itself. Reading the files here puts them in this test's own read set,
// which the cache DOES key on, so an edit anywhere in the module busts it.
func touchSources(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "vendor" || name == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			_, err = os.ReadFile(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// withEnv returns base with every KEY in set replaced by set's value.
func withEnv(base, set []string) []string {
	drop := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// TestLinuxVetAndBuild keeps the Linux build alive (spec §10.1, R53):
// nothing runs on Linux in M1, but a macOS-only change must still fail the
// gate. CGO_ENABLED=0 is what the release builds with. amd64 rather than
// the host's arch, so a file constrained to one architecture is caught too.
func TestLinuxVetAndBuild(t *testing.T) {
	t.Parallel()
	touchSources(t, moduleRoot(t))
	linux := []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0"}
	goTool(t, linux, "vet", "./...")
	goTool(t, linux, "build", "-o", filepath.Join(t.TempDir(), "chottag"), "./cmd/chottag")
}
