package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// mustExecutable returns what os.Executable() reports for this test binary,
// the same value runSetup's bin/claude symlink points at.
func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestSetupCreatesTheTreeAndSymlinks(t *testing.T) {
	home, fakeHome := t.TempDir(), t.TempDir() // fakeHome stands in for $HOME
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/bin/zsh")

	var stdout bytes.Buffer
	code := runSetup(nil, newReporter(false, &stdout, io.Discard))
	if code != 0 {
		t.Fatalf("runSetup = %d, want 0", code)
	}

	for _, p := range []string{"ca/ca.pem", "bin/chottag", "bin/claude", "accounts", "run"} {
		if _, err := os.Lstat(filepath.Join(home, p)); err != nil {
			t.Errorf("setup did not create %s: %v", p, err)
		}
	}
	target, err := os.Readlink(filepath.Join(home, "bin", "claude"))
	if err != nil {
		t.Fatalf("bin/claude is not a symlink: %v", err)
	}
	if filepath.Base(target) != "chottag" && target != mustExecutable(t) {
		t.Errorf("bin/claude -> %q, want the chottag binary: one binary, dispatched by argv[0]", target)
	}

	// F221: setup creates the proxy secret too, so the doctor's
	// proxy-secret row and the first `claude` launch both find one already
	// in place.
	s, err := proxyauth.Load(home)
	if err != nil || s.IsZero() {
		t.Fatalf("proxyauth.Load after setup = %v, %v; want a secret", s, err)
	}
	fi, err := os.Stat(proxyauth.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("proxy.secret mode = %o, want 0600", fi.Mode().Perm())
	}
}

// TestProvisionBinSymlinksSkipsWhenTheBinaryAlreadyLivesAtTheLink pins fix
// round 3's D7/F-J directly against the real code path: when exe IS
// already the regular file at binDir/chottag (the plausible shape — `go
// build -o $CHOTTAG_HOME/bin/chottag` — not merely a symlink to it),
// provisionBinSymlinks must leave it alone rather than os.Remove-ing the
// running binary and os.Symlink-ing it to the now-empty path it just
// vacated (a self-referential link, ELOOP on every subsequent invocation).
func TestProvisionBinSymlinksSkipsWhenTheBinaryAlreadyLivesAtTheLink(t *testing.T) {
	binDir := t.TempDir()
	exe := filepath.Join(binDir, "chottag")
	const marker = "not actually a binary, just needs to exist"
	if err := os.WriteFile(exe, []byte(marker), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := provisionBinSymlinks(exe, binDir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(exe)
	if err != nil {
		t.Fatalf("chottag is gone after provisionBinSymlinks: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("provisionBinSymlinks replaced the binary at its own link path with a symlink to itself (ELOOP)")
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != marker {
		t.Fatalf("chottag content = %q, err %v; want the original binary untouched", got, err)
	}

	// The OTHER link (claude), which does not coincide with exe, must still
	// be created normally — the guard is specific to the coinciding case.
	claudeLink := filepath.Join(binDir, "claude")
	target, err := os.Readlink(claudeLink)
	if err != nil {
		t.Fatalf("bin/claude is not a symlink: %v", err)
	}
	if target != exe {
		t.Errorf("bin/claude -> %q, want %q", target, exe)
	}
}

// TestProvisionBinSymlinksReplacesAnOrdinaryPriorLink pins the ordinary
// case still works: a link pointing somewhere else (an older chottag
// binary, a stale build) is replaced, not skipped.
func TestProvisionBinSymlinksReplacesAnOrdinaryPriorLink(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldExe := filepath.Join(dir, "old-chottag")
	if err := os.WriteFile(oldExe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(binDir, "chottag")
	if err := os.Symlink(oldExe, link); err != nil {
		t.Fatal(err)
	}
	newExe := filepath.Join(dir, "new-chottag")
	if err := os.WriteFile(newExe, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := provisionBinSymlinks(newExe, binDir); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("bin/chottag is not a symlink: %v", err)
	}
	if target != newExe {
		t.Errorf("bin/chottag -> %q, want the new binary %q", target, newExe)
	}
}

func TestSetupWithAnUnknownShellPrintsAndWritesNothing(t *testing.T) {
	home, fakeHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", fakeHome)
	t.Setenv("SHELL", "/usr/bin/fish")

	var stdout bytes.Buffer
	if code := runSetup(nil, newReporter(false, &stdout, io.Discard)); code != 0 {
		t.Fatalf("runSetup = %d, want 0", code)
	}
	entries, err := os.ReadDir(fakeHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("setup wrote %v into HOME for an unrecognised shell; it must print the line instead", entries)
	}
	if !strings.Contains(stdout.String(), filepath.Join(home, "bin")) {
		t.Error("setup should print the PATH line the user must add themselves")
	}
}
