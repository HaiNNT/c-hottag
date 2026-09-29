package owners_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
)

func TestRenameAccountRewritesOnlyThatAccountsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.json")
	at := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	if err := owners.Edit(path, func(tx *owners.Tx) error {
		tx.Reassign(router.KindArtifact, "a1", "B", at)
		tx.Reassign(router.KindSession, "s1", "b", at)
		tx.Reassign(router.KindArtifact, "a2", "A", at)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := owners.Edit(path, func(tx *owners.Tx) error { n = tx.RenameAccount("B", "Bee"); return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rewrote %d, want 2 (both spellings of B)", n)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]struct {
		Account string    `json:"account"`
		At      time.Time `json:"at"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		owners.Key(router.KindArtifact, "a1"): "Bee",
		owners.Key(router.KindSession, "s1"):  "Bee",
		owners.Key(router.KindArtifact, "a2"): "A",
	} {
		if raw[key].Account != want || !raw[key].At.Equal(at) {
			t.Errorf("%s = %+v, want %s with its original time", key, raw[key], want)
		}
	}
}

func TestRenameAccountIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.json")
	if err := owners.Edit(path, func(tx *owners.Tx) error {
		tx.Reassign(router.KindArtifact, "a1", "Bee", time.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("read only")
	var n int
	err := owners.Edit(path, func(tx *owners.Tx) error {
		n = tx.RenameAccount("B", "Bee") + tx.RenameAccount("bee", "Bee")
		return stop
	})
	if !errors.Is(err, stop) || n != 0 {
		t.Fatalf("n = %d, err = %v; want nothing rewritten on a re-run", n, err)
	}
}
