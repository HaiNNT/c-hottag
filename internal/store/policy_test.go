package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestPolicyAbsentIsSerialAndWritesNoKey(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	st, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: filepath.Join(dir, "accounts", "A")})
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.PolicySpread() || st.Policy != "" || st.Pin != "" {
		t.Fatalf("fresh state = policy %q pin %q, want both absent", st.Policy, st.Pin)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if strings.Contains(string(b), `"policy"`) || strings.Contains(string(b), `"pin"`) {
		t.Fatalf("state.json carries policy or pin keys by default: %s", b)
	}
}

func TestPolicyAndPinRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		st.Policy = "spread"
		st.Pin = "B"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !st.PolicySpread() || st.Pin != "B" {
		t.Fatalf("loaded policy %q pin %q, want spread and B", st.Policy, st.Pin)
	}
	if _, err := s.Update(func(st *store.State) error { st.SetPolicy("serial"); return nil }); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Load()
	if st.PolicySpread() || st.Policy != "" || st.Pin != "B" {
		t.Fatalf("after serial: policy %q pin %q, want serial stored as absent and the pin kept", st.Policy, st.Pin)
	}
}

func TestRenameAndRemoveFollowThePin(t *testing.T) {
	dir := t.TempDir()
	st := store.Default()
	for _, n := range []string{"A", "B"} {
		if err := st.Add(store.Account{Name: n, Dir: filepath.Join(dir, "accounts", n)}); err != nil {
			t.Fatal(err)
		}
	}
	st.Pin = "B"
	if _, _, err := st.Rename("B", "work"); err != nil {
		t.Fatal(err)
	}
	if st.Pin != "work" {
		t.Fatalf("pin after rename = %q, want work", st.Pin)
	}
	if err := st.Remove("work"); err != nil {
		t.Fatal(err)
	}
	if st.Pin != "" {
		t.Fatalf("pin after the pinned account was removed = %q, want cleared", st.Pin)
	}
}
