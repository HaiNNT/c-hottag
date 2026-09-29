package rotate_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/rotate"
)

// TestWriterRotationBoundedByKeep pins the shift loop's bound directly: after
// enough writes to force repeated rotation, the highest surviving rotated
// index must equal Keep, for both Keep=1 (the off-by-one case: the loop must
// not run at all) and Keep=2 (the loop runs exactly once).
func TestWriterRotationBoundedByKeep(t *testing.T) {
	for _, keep := range []int{1, 2} {
		t.Run(fmt.Sprintf("Keep=%d", keep), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.log")
			w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 32, Keep: keep})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()

			line := strings.Repeat("x", 20) + "\n" // 21 bytes
			for i := 0; i < 6; i++ {
				if _, err := w.Write([]byte(line)); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := os.Stat(path); err != nil {
				t.Fatalf("live log missing: %v", err)
			}

			highest := 0
			for i := 1; ; i++ {
				if _, err := os.Stat(fmt.Sprintf("%s.%d", path, i)); err != nil {
					break
				}
				highest = i
			}
			if highest != keep {
				t.Fatalf("highest surviving rotated index = %d, want %d (the Keep bound)", highest, keep)
			}
		})
	}
}

func TestWriterCreatesTheLogModeOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 1 << 20, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("log mode = %04o, want 0600 (it may carry operational detail)", got)
	}
}

func TestWriterKeepsWritingAcrossARotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 16, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 4; i++ {
		if _, err := w.Write([]byte("0123456789\n")); err != nil {
			t.Fatalf("write %d after rotation: %v", i, err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("live log is empty after rotation; writes went nowhere")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("live log mode after rotation = %04o, want 0600", got)
	}

	rfi, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if got := rfi.Mode().Perm(); got != 0o600 {
		t.Fatalf("rotated log mode = %04o, want 0600 (it may carry operational detail)", got)
	}
}

// TestWriterSelfHealsAfterTransientRotationFailure reproduces the wedge a
// reviewer found: a rotation that fails partway (here, because the log's
// directory turns read-only) must not leave the writer permanently broken.
// A write during the failure must error; a write once the failure clears
// must succeed using the same *Writer.
func TestWriterSelfHealsAfterTransientRotationFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permission is not enforced the same way on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 16, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	line := []byte("0123456789\n") // 11 bytes

	// Under the cap: no rotation yet.
	if _, err := w.Write(line); err != nil {
		t.Fatal(err)
	}

	// Block rotation: no write permission on the log's directory means the
	// rename inside rotateLocked cannot succeed.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := w.Write(line); err == nil {
		t.Fatal("write that requires rotation succeeded despite a read-only directory; want an error")
	}

	// Clear the condition and confirm the writer recovered rather than
	// wedging on a closed handle.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(line); err != nil {
		t.Fatalf("write after the directory became writable again failed: %v", err)
	}
}

// TestWriteAfterCloseDoesNotReviveTheWriter pins a reviewer finding: without
// a closed flag, a Write after Close that happens to cross MaxBytes would
// rotate — reopening the file and resurrecting a writer its caller believed
// was shut down, leaking the new fd. A write after Close must error without
// rotating or reopening anything.
func TestWriteAfterCloseDoesNotReviveTheWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 16, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Enough to force rotation were the writer still live.
	line := strings.Repeat("z", 20) + "\n"
	if _, err := w.Write([]byte(line)); err == nil {
		t.Fatal("write after Close succeeded; want an error")
	}

	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("write after Close rotated the log; a closed writer must not reopen or rename anything")
	}
}

// TestCloseIsIdempotent pins that a second Close is a clean no-op rather
// than an error. Not because any current production caller double-closes
// one of these — every Close reaching a *Writer today (proxy.go's trace
// log, daemon.go's daemon.log) is a single defer — but because
// tracelog.Writer, the layer Task 3 stacks on top of a rotate.Writer in
// production, is itself a bare passthrough on Close with no guard of its
// own (see tracelog.Writer.Close's doc comment): this is the only layer
// that can provide the property at all, so it is pinned as a done
// criterion rather than left to whichever caller happens to need it first.
// A spurious error here would be reported to the operator as a shutdown
// failure that did not happen.
func TestCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 64, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close = %v, want nil", err)
	}
	for i := 2; i <= 4; i++ {
		if err := w.Close(); err != nil {
			t.Fatalf("Close call %d = %v, want nil: Close must be idempotent", i, err)
		}
	}
}

// TestWriteRecoversWhenLiveFileAlreadyRenamedAway pins recovery from a state
// that never self-heals on its own: a prior rotation renamed the live file
// away and then failed before reopening it, so cfg.Path is missing on disk
// while w.f is still a valid handle to the renamed inode. Nothing ever
// recreates the missing live file, so the next rotation's rename source is
// simply gone. Unless rotateLocked skips the rename when the source is
// already absent, every subsequent Write hits the same missing-source error
// forever and the writer wedges permanently.
func TestWriteRecoversWhenLiveFileAlreadyRenamedAway(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	w, err := rotate.Open(rotate.Config{Path: path, MaxBytes: 16, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Under the cap: no rotation yet, w.f is open on the live file.
	if _, err := w.Write([]byte("0123456789\n")); err != nil {
		t.Fatal(err)
	}

	// Simulate a prior rotation that renamed the live file away and then
	// failed before reopening it: rename cfg.Path out from under the
	// Writer from outside. Renaming an open file does not invalidate the
	// handle, so w.f still refers to the same (now-renamed) inode
	// afterward, exactly as it would after that kind of partial failure.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("setup invariant broken: live path still exists after rename")
	}

	// A write that forces rotation must recover: with the live file
	// already gone, the rename step must be skipped rather than failing on
	// a missing source.
	line := strings.Repeat("y", 20) + "\n" // forces size past MaxBytes
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write did not recover from a pre-vanished live file: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live log missing after recovery: %v", err)
	}

	// Confirm the recovery isn't a one-off fluke: the writer keeps working.
	if _, err := w.Write([]byte("more\n")); err != nil {
		t.Fatalf("write after recovery failed: %v", err)
	}
}
