package shim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The shim must never resolve to itself, or it would exec in a loop.
func TestResolveSkipsItsOwnDirAndCmuxShims(t *testing.T) {
	root := t.TempDir()
	selfDir := filepath.Join(root, "chottag", "bin")
	cmux := filepath.Join(root, "cmux-cli-shims")
	real := filepath.Join(root, "usr", "bin")
	for _, d := range []string{selfDir, cmux, real} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(d, "claude"))
	}

	pathEnv := strings.Join([]string{selfDir, cmux, real}, string(os.PathListSeparator))
	got, err := ResolveClaude(pathEnv, selfDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "claude"); got != want {
		t.Errorf("ResolveClaude = %q, want %q: it must skip its own dir and any */cmux-cli-shims/*", got, want)
	}
}

func TestResolveUsesTheCacheOnlyWhileItIsStillExecutable(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(root, "gone", "claude")
	writeExecutable(t, filepath.Join(real, "claude"))

	got, err := ResolveClaude(real, filepath.Join(root, "self"), cached)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "claude"); got != want {
		t.Errorf("ResolveClaude = %q, want %q: a cached path that no longer exists must be re-resolved, not returned", got, want)
	}
}

func TestResolveFailsClosedWhenNothingResolves(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveClaude(empty, filepath.Join(root, "self"), ""); err == nil {
		t.Fatal("ResolveClaude succeeded with no claude on PATH, want an error: the shim must fail closed rather than exec something arbitrary")
	}
}

// A directory that happens to be named "claude" must never be mistaken for
// the executable: isExecutableFile has to reject non-regular files.
func TestResolveIgnoresADirectoryNamedClaude(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(dir, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveClaude(dir, filepath.Join(root, "self"), ""); err == nil {
		t.Fatal("ResolveClaude succeeded with a directory named claude on PATH, want an error: a directory is not an executable")
	}
}

// A PATH entry that is not chottag's own dir, and not a cmux shim dir, can
// still resolve back to chottag's own binary via a symlink chain (an
// `ln -s`, an npm global link, or a Homebrew link shape). That must be
// rejected too, or the shim execs itself.
func TestResolveRejectsAPathEntryThatResolvesToItsOwnBinary(t *testing.T) {
	root := t.TempDir()
	selfBin := filepath.Join(root, "chottag-bin")
	writeExecutable(t, selfBin)

	orig := executablePath
	executablePath = func() (string, error) { return selfBin, nil }
	defer func() { executablePath = orig }()

	evil := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(selfBin, filepath.Join(evil, "claude")); err != nil {
		t.Fatal(err)
	}

	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(real, "claude"))

	pathEnv := strings.Join([]string{evil, real}, string(os.PathListSeparator))
	got, err := ResolveClaude(pathEnv, filepath.Join(root, "self"), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "claude"); got != want {
		t.Errorf("ResolveClaude = %q, want %q: a PATH entry whose claude resolves to chottag's own binary must be skipped", got, want)
	}
}

// ResolveClaude cannot promise it never resolves to chottag itself without
// knowing what chottag itself is: if it can't determine its own executable
// path, it must fail closed rather than fall back to the weaker directory
// guards alone.
func TestResolveFailsClosedWhenItCannotDetermineItsOwnBinary(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(real, "claude"))

	orig := executablePath
	executablePath = func() (string, error) { return "", errors.New("boom") }
	defer func() { executablePath = orig }()

	if _, err := ResolveClaude(real, filepath.Join(root, "self"), ""); err == nil {
		t.Fatal("ResolveClaude succeeded despite not knowing its own executable path, want an error")
	}
}

// The cache is subject to the same self-identity guard as the PATH walk: a
// cached path that resolves to chottag's own binary must be ignored and the
// PATH walk must run instead of returning it forever.
func TestResolveCacheIsRejectedWhenItResolvesToItsOwnBinary(t *testing.T) {
	root := t.TempDir()
	selfBin := filepath.Join(root, "chottag-bin")
	writeExecutable(t, selfBin)

	orig := executablePath
	executablePath = func() (string, error) { return selfBin, nil }
	defer func() { executablePath = orig }()

	selfDir := filepath.Join(root, "self")
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(selfDir, "claude")
	if err := os.Symlink(selfBin, cached); err != nil {
		t.Fatal(err)
	}

	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(real, "claude"))

	got, err := ResolveClaude(real, selfDir, cached)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "claude"); got != want {
		t.Errorf("ResolveClaude = %q, want %q: a cache that resolves to chottag's own binary must be ignored and the PATH walk must run", got, want)
	}
}

// filepath.EvalSymlinks never absolutises, so a relative PATH entry naming
// chottag's own (absolute) bin dir must still be recognized as chottag's
// own dir once resolved.
func TestResolveAbsolutisesARelativePathEntryBeforeComparing(t *testing.T) {
	root := t.TempDir()
	selfDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(selfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(selfDir, "claude"))

	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(real, "claude"))

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatal(err)
		}
	}()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}

	pathEnv := strings.Join([]string{"bin", real}, string(os.PathListSeparator))
	got, err := ResolveClaude(pathEnv, selfDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "claude"); got != want {
		t.Errorf("ResolveClaude = %q, want %q: a relative PATH entry naming chottag's own bin dir must still be skipped", got, want)
	}
}

// isExecutableFile's execute-bit mask must reject a regular file with no
// execute bit set, not just non-regular files.
func TestResolveRejectsANonExecutableRegularFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveClaude(dir, filepath.Join(root, "self"), ""); err == nil {
		t.Fatal("ResolveClaude succeeded with a non-executable claude on PATH, want an error: no execute bit means it is not the executable")
	}
}

// filepath.EvalSymlinks cannot resolve a hard link to a canonical path — a
// hard link to chottag's own binary is a different path but the same inode,
// so isSelf must compare inodes (os.SameFile), not resolved path strings.
func TestResolveRejectsAHardLinkToItsOwnBinary(t *testing.T) {
	root := t.TempDir()
	selfBin := filepath.Join(root, "chottag-bin")
	writeExecutable(t, selfBin)

	linked := filepath.Join(root, "usr", "local", "bin")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(selfBin, filepath.Join(linked, "claude")); err != nil {
		t.Skipf("os.Link unavailable on this filesystem: %v", err)
	}

	orig := executablePath
	executablePath = func() (string, error) { return selfBin, nil }
	defer func() { executablePath = orig }()

	real := filepath.Join(root, "usr", "bin")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(real, "claude"))

	pathEnv := strings.Join([]string{linked, real}, string(os.PathListSeparator))
	got, err := ResolveClaude(pathEnv, filepath.Join(root, "self"), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(real, "claude"); got != want {
		t.Errorf("ResolveClaude = %q, want %q: a hard link to chottag's own binary must be skipped", got, want)
	}
}
