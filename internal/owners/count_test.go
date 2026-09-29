package owners_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
)

func TestCountByAccountCountsEntriesAsStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.json")
	if err := owners.Edit(path, func(tx *owners.Tx) error {
		now := time.Now()
		tx.Reassign(router.KindArtifact, "a1", "B", now)
		tx.Reassign(router.KindSession, "s1", "B", now)
		tx.Reassign(router.KindArtifact, "a2", "A", now)
		tx.Reassign(router.KindConnector, "c1", "b", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := owners.CountByAccount(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"A": 1, "B": 2, "b": 1}; !maps.Equal(got, want) {
		t.Fatalf("counts = %v, want %v", got, want)
	}
}

func TestCountByAccountMissingFileIsEmpty(t *testing.T) {
	got, err := owners.CountByAccount(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil || len(got) != 0 {
		t.Fatalf("counts = %v, err = %v; want empty", got, err)
	}
}

func TestCountByAccountLeavesACorruptFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := owners.CountByAccount(path); !errors.Is(err, owners.ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "owners.json" {
		t.Fatalf("dir = %v, want owners.json alone: no .corrupt, no lock", entries)
	}
	if b, _ := os.ReadFile(path); string(b) != "{" {
		t.Fatalf("owners.json = %q, want it untouched", b)
	}
}
