package fsutil_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

func TestLockExcludesSecondHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock1, err := fsutil.Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func() error, 1)
	go func() {
		u, err := fsutil.Lock(p)
		if err != nil {
			t.Error(err)
			return
		}
		got <- u
	}()
	select {
	case <-got:
		t.Fatal("second Lock acquired while the first was held")
	case <-time.After(100 * time.Millisecond):
	}
	if err := unlock1(); err != nil {
		t.Fatal(err)
	}
	select {
	case u := <-got:
		u()
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock never acquired after unlock")
	}
}

func TestTryLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	unlock1, ok, err := fsutil.TryLock(p)
	if err != nil || !ok {
		t.Fatalf("first TryLock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := fsutil.TryLock(p); ok || err != nil {
		t.Fatalf("second TryLock while held: ok=%v err=%v", ok, err)
	}
	if err := unlock1(); err != nil {
		t.Fatal(err)
	}
	unlock2, ok, err := fsutil.TryLock(p)
	if err != nil || !ok {
		t.Fatalf("TryLock after unlock: ok=%v err=%v", ok, err)
	}
	unlock2()
}
