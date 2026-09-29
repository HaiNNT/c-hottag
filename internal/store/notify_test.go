package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestNotifyIsOnWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Store{Dir: dir}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !st.NotifyOn() {
		t.Fatal("NotifyOn() = false with no state.json, want on (D12)")
	}
	for _, doc := range []string{
		`{"version":1,"accounts":[],"port":47821}`,
		`{"version":1,"accounts":[],"port":47821,"notify":null}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := store.Store{Dir: dir}.Load()
		if err != nil {
			t.Fatal(err)
		}
		if !st.NotifyOn() {
			t.Errorf("NotifyOn() = false for %s, want on: absent means on", doc)
		}
	}
}

func TestSetNotifyRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	for _, tc := range []struct {
		on   bool
		want string
	}{{false, `"notify": false`}, {true, `"notify": true`}} {
		if _, err := s.Update(func(st *store.State) error { st.SetNotify(tc.on); return nil }); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("state.json = %s, want it to contain %s", b, tc.want)
		}
		st, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		if st.NotifyOn() != tc.on {
			t.Errorf("NotifyOn() = %v after SetNotify(%v)", st.NotifyOn(), tc.on)
		}
	}
}

func TestDefaultStateWritesNoNotifyKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := (store.Store{Dir: dir}).Update(func(*store.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"notify"`) {
		t.Fatalf("state.json = %s, want no notify key until the user sets one", b)
	}
}

func TestSetNotifyDoesNotAliasACopy(t *testing.T) {
	var a store.State
	a.SetNotify(false)
	b := a
	b.SetNotify(true)
	if a.NotifyOn() {
		t.Fatal("SetNotify on a copy changed the original: it must store a fresh pointer")
	}
}
