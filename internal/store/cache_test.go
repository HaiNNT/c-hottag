package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestCacheReloadsOnlyWhenTheFileChanges(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(st *store.State) error { return st.Add(acct("B", "")) }); err != nil {
		t.Fatal(err)
	}
	c := store.NewCache(s)
	st, err := c.State()
	if err != nil || st.Serving != "B" {
		t.Fatalf("first State = %+v err=%v", st, err)
	}
	if got := store.Loads(c); got != 1 {
		t.Fatalf("loads after first State = %d, want 1", got)
	}
	for i := 0; i < 5; i++ {
		if st, err := c.State(); err != nil || st.Serving != "B" {
			t.Fatalf("cached State = %+v err=%v", st, err)
		}
	}
	if got := store.Loads(c); got != 1 {
		t.Fatalf("unchanged file re-read %d times, want 1", got)
	}
	if _, err := s.Update(func(st *store.State) error { return st.Add(acct("C", "")) }); err != nil {
		t.Fatal(err)
	}
	st, err = c.State()
	if err != nil || len(st.Accounts) != 2 {
		t.Fatalf("State after change = %+v err=%v", st, err)
	}
	if got := store.Loads(c); got != 2 {
		t.Fatalf("loads after change = %d, want 2", got)
	}
}

func TestCacheMissingFileAndInvalidate(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	c := store.NewCache(s)
	st, err := c.State()
	if err != nil || len(st.Accounts) != 0 || st.Port != store.DefaultPort {
		t.Fatalf("missing file State = %+v err=%v", st, err)
	}
	if _, err := s.Update(func(st *store.State) error { return st.Add(acct("B", "")) }); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.State(); len(st.Accounts) != 1 {
		t.Fatal("cache did not notice a state file appearing")
	}
	os.Remove(filepath.Join(s.Dir, "state.json"))
	c.Invalidate()
	if st, _ := c.State(); len(st.Accounts) != 0 {
		t.Fatal("Invalidate did not force a re-read")
	}
}

func TestCacheReportsCorruptFile(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	os.WriteFile(filepath.Join(s.Dir, "state.json"), []byte("{broken"), 0o600)
	if _, err := store.NewCache(s).State(); err == nil {
		t.Fatal("corrupt state.json accepted")
	}
}
