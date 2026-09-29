package leakscan

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// snapshotGuardEnv marks a go test invocation as already running INSIDE a
// simulated public snapshot this same test built. Without it, the snapshot
// (which necessarily includes this very test file, since test/ is public)
// would invoke itself again on a snapshot of a snapshot, forever: each
// level's "go test" blocks waiting for the next level's, so nothing times
// out on its own until every level individually hits go test's default
// per-package timeout, by which point many generations of orphaned
// processes exist. The guard, with snapshotMarker below, makes recursion
// stop at depth 1: the real (outer) run does the actual work once and
// sets both for its child; the child's own copy of this test sees both
// agree and skips immediately.
const snapshotGuardEnv = "CHOTTAG_SNAPSHOT_TEST_ALREADY_RUNNING"

// snapshotMarker is a file in the snapshot's own .git directory holding
// the token the outer run also puts in snapshotGuardEnv (part 5, re-review
// R-N5 and R-M1). The two travel separately, so a nested run needs both
// to agree before it skips: a guard variable left exported in a shell no
// longer skips the real check in silence, and a refactor that drops the
// variable from the child's environment still stops at depth 1, because
// the marker is there. A real checkout never has the marker (in a git
// worktree .git is a file, so the read fails there too).
const snapshotMarker = "chottag-snapshot-guard"

// snapshotGuardState decides from the guard variable's value and the
// marker's content (each "" when absent): run the real check (outer),
// skip as a nested copy (nested), or neither (problem != "").
func snapshotGuardState(token, marker string) (nested bool, problem string) {
	switch {
	case token == "" && marker == "":
		return false, ""
	case token != "" && token == marker:
		return true, ""
	case token != "" && marker == "":
		return false, snapshotGuardEnv + " is set, but this tree has no " + snapshotMarker + ": a guard left over in the environment; unset it"
	default:
		return false, snapshotMarker + " does not match " + snapshotGuardEnv + ": refusing to run a nested simulated snapshot"
	}
}

func TestSnapshotGuardStateNeedsBothHalves(t *testing.T) {
	for _, c := range []struct {
		token, marker string
		nested, fails bool
	}{
		{"", "", false, false},
		{"abc", "abc", true, false},
		{"abc", "", false, true}, // stale variable: fail, never skip silently
		{"", "abc", false, true}, // variable dropped: fail, never recurse
		{"abc", "xyz", false, true},
	} {
		nested, problem := snapshotGuardState(c.token, c.marker)
		if nested != c.nested || (problem != "") != c.fails {
			t.Errorf("snapshotGuardState(%q, %q) = (%t, %q), want nested %t, fails %t", c.token, c.marker, nested, problem, c.nested, c.fails)
		}
	}
}

// TestSimulatedPublicSnapshotPassesItsOwnGitDependentChecks pins I1: a git
// repository holding only the tracked, manifest-selected files -- exactly
// what a public CI checkout of an exported tag holds, and never a
// docs/internal/ or .claude/tracker.json -- must pass its own copies of
// TestEveryManifestEntryMatchesATrackedPath (this package) and
// TestNoPublicFileNamesAMovedDocPath (test/consistency), the git-dependent
// checks I1 found silently skipping outside a git work tree. This never
// runs scripts/export-public itself (test/export already covers that end
// to end); what it adds is running the *public* checks inside a real git
// checkout that has neither private-only file, which the private repo's
// own gate otherwise never exercises.
func TestSimulatedPublicSnapshotPassesItsOwnGitDependentChecks(t *testing.T) {
	// TestSnapshotGuardStateNeedsBothHalves pins snapshotGuardState's own
	// decision table only; it never runs this function. This ReadFile of
	// snapshotMarker, and the runEnv token below (near "token :=
	// rand.Text()") that writes it into the child's environment, are what
	// actually wire the marker to that table, so a change to either one
	// still needs its own review even though the table test stays green.
	marker, _ := os.ReadFile(filepath.Join(root(t), ".git", snapshotMarker))
	nested, problem := snapshotGuardState(os.Getenv(snapshotGuardEnv), strings.TrimSpace(string(marker)))
	if problem != "" {
		t.Fatal(problem)
	}
	if nested {
		t.Skip("already running inside a simulated public snapshot; not recursing into another one")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not installed")
	}
	r := root(t)
	files := tracked(t)
	sel, errb, code := script(t, r, strings.Join(files, "\n")+"\n", "manifest-select", filepath.Join(r, "public-manifest.txt"))
	if code != 0 || sel == "" {
		t.Fatalf("manifest-select: exit %d, %d bytes selected: %s", code, len(sel), errb)
	}

	dir := t.TempDir()
	for _, rel := range strings.Split(strings.TrimRight(sel, "\n"), "\n") {
		src := filepath.Join(r, filepath.FromSlash(rel))
		fi, lerr := os.Lstat(src)
		if lerr != nil {
			t.Fatal(lerr)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			t.Fatal(merr)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(src)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if serr := os.Symlink(target, dst); serr != nil {
				t.Fatal(serr)
			}
			continue
		}
		b, rerr := os.ReadFile(src)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(dst, b, fi.Mode().Perm()); werr != nil {
			t.Fatal(werr)
		}
	}

	home := t.TempDir()
	if werr := os.WriteFile(filepath.Join(home, "gitconfig"), nil, 0o600); werr != nil {
		t.Fatal(werr)
	}
	gitEnv := []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(home, "gitconfig"),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), gerr, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "public snapshot fixture")

	token := rand.Text()
	if werr := os.WriteFile(filepath.Join(dir, ".git", snapshotMarker), []byte(token+"\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}

	// Reuse the host's build cache (never its HOME): the snapshot's own
	// go test run otherwise recompiles the standard library from a bare
	// GOCACHE, which says nothing about I1 and only costs time.
	gocache, cerr := exec.Command(goBin, "env", "GOCACHE").Output()
	if cerr != nil {
		t.Fatal(cerr)
	}
	runEnv := []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "LC_ALL=C",
		"GOFLAGS=", "GOPROXY=off", "GOTOOLCHAIN=local",
		"GOCACHE=" + strings.TrimSpace(string(gocache)),
		snapshotGuardEnv + "=" + token,
	}
	// A hard, self-enforced deadline is a hang guard, not the recursion
	// guard: if the recursion guard above ever failed to stop a nested
	// copy of this very test, go test's own default per-package timeout
	// (10m) would let it run that long before its process exits WITHOUT
	// killing whatever it had itself spawned, orphaning it. The child runs
	// in its own process group, and the deadline kills that whole group
	// (go, its test binaries and anything they started), not only the go
	// process. WaitDelay stops CombinedOutput from waiting on pipes a
	// straggler still holds.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	run := exec.CommandContext(ctx, goBin, "test", "-timeout=45s", "./test/leakscan/...", "./test/consistency/...", "-count=1")
	run.Dir = dir
	run.Env = runEnv
	run.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	run.Cancel = func() error { return syscall.Kill(-run.Process.Pid, syscall.SIGKILL) }
	run.WaitDelay = 5 * time.Second
	defer func() {
		// Whatever happened above, leave no member of the group behind.
		if run.Process != nil {
			_ = syscall.Kill(-run.Process.Pid, syscall.SIGKILL)
		}
	}()
	start := time.Now()
	out, rerr := run.CombinedOutput()
	t.Logf("go test ./test/leakscan/... ./test/consistency/... in the simulated public snapshot took %s", time.Since(start))
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("the simulated public snapshot's own go test run did not finish within the 60s hang guard:\n%s", out)
	}
	if rerr != nil {
		t.Fatalf("the simulated public snapshot's own leakscan/consistency checks failed:\n%s", out)
	}
}
