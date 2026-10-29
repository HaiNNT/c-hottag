package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

var moveNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func TestSetMoveRoundTrip(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	m := store.Move{Account: "alice", Dir: "/Users/alice/proj", At: moveNow}
	if _, err := s.Update(func(st *store.State) error { st.SetMove("sid-1", m); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Moves["sid-1"]; got.Account != m.Account || got.Dir != m.Dir || !got.At.Equal(m.At) {
		t.Errorf("Moves[sid-1] = %+v, want %+v", got, m)
	}
}

func TestPruneMovesDropsDeadAndOld(t *testing.T) {
	var st store.State
	st.SetMove("keep", store.Move{Account: "alice", At: moveNow.Add(-time.Hour)})
	st.SetMove("dead", store.Move{Account: "alice", At: moveNow})
	st.SetMove("old", store.Move{Account: "bob", At: moveNow.Add(-8 * 24 * time.Hour)})
	st.PruneMoves(func(sid string) bool { return sid != "dead" }, moveNow)
	if len(st.Moves) != 1 {
		t.Fatalf("Moves = %v, want only keep", st.Moves)
	}
	if _, ok := st.Moves["keep"]; !ok {
		t.Errorf("keep was dropped: %v", st.Moves)
	}
}

func TestPruneMovesEmptyWritesNoKey(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		st.SetMove("a", store.Move{Account: "alice", At: moveNow})
		st.PruneMoves(func(string) bool { return false }, moveNow)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"moves"`) {
		t.Errorf("state.json has a moves key: %s", b)
	}
}
