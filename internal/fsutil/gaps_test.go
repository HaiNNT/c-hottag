package fsutil_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// tempLeftovers lists dir's WriteFileAtomic temp files (".<name>.tmp-*").
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestWriteFileAtomicFailsInAMissingDir: no directory, no temp file to
// write through, so the error comes back and nothing is created.
func TestWriteFileAtomicFailsInAMissingDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing", "state.json")
	err := fsutil.WriteFileAtomic(p, []byte("{}"), 0o600)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if _, serr := os.Stat(p); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("%s exists after a failed write (stat err %v)", p, serr)
	}
}

// TestWriteFileAtomicCleansUpItsTempFileWhenTheRenameFails: the rename is
// the last step; when it fails (here the target is a non-empty directory)
// the temp file must not be left behind, and the target is untouched.
func TestWriteFileAtomicCleansUpItsTempFileWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Join(p, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFileAtomic(p, []byte("{}"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic onto a non-empty directory succeeded")
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %q", left)
	}
	if fi, err := os.Stat(filepath.Join(p, "keep")); err != nil || !fi.IsDir() {
		t.Fatalf("the target directory was disturbed: %v", err)
	}
}

// TestWriteFileAtomicFailsInAReadOnlyDirAndLeavesTheOldFile: a directory
// that refuses new entries refuses the temp file, so the old content
// stays exactly as it was.
func TestWriteFileAtomicFailsInAReadOnlyDirAndLeavesTheOldFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := fsutil.WriteFileAtomic(p, []byte("new"), 0o600); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want fs.ErrPermission", err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "old" {
		t.Fatalf("content = %q (%v), want the old content", b, err)
	}
	if left := tempLeftovers(t, dir); len(left) != 0 {
		t.Fatalf("temp files left behind: %q", left)
	}
}

// TestLockAndTryLockFailWhenTheLockFileCannotBeOpened: both report the
// open error rather than claiming, or silently skipping, the lock.
func TestLockAndTryLockFailWhenTheLockFileCannotBeOpened(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing", "x.lock")
	if unlock, err := fsutil.Lock(p); !errors.Is(err, fs.ErrNotExist) || unlock != nil {
		t.Fatalf("Lock = (unlock set %t, %v), want (false, fs.ErrNotExist)", unlock != nil, err)
	}
	unlock, ok, err := fsutil.TryLock(p)
	if !errors.Is(err, fs.ErrNotExist) || ok || unlock != nil {
		t.Fatalf("TryLock = (unlock set %t, %t, %v), want (false, false, fs.ErrNotExist)", unlock != nil, ok, err)
	}
}
