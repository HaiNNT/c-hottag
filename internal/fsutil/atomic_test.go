package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

func TestWriteFileAtomicWritesContentAndPerm(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := fsutil.WriteFileAtomic(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "hello" {
		t.Fatalf("content = %q, err = %v", b, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	_ = fsutil.WriteFileAtomic(p, []byte("one"), 0o644)
	if err := fsutil.WriteFileAtomic(p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "two" {
		t.Fatalf("got %q", b)
	}
}
