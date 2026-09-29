package owners_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// Edit is the synchronous, locked transaction for a short-lived process.
// The lookup and the write must be ONE transaction: `chottag own` checks
// that an id exists and then reassigns it, and across processes those
// cannot be two separate steps.
func TestEditAppliesAndPersistsInOneTransaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:art1":{"account":"A","at":"2026-09-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var sawOwner string
	err := owners.Edit(path, func(tx *owners.Tx) error {
		owner, ok := tx.Lookup(router.KindArtifact, "art1")
		if !ok {
			return errors.New("art1 has no owner")
		}
		sawOwner = owner
		tx.Reassign(router.KindArtifact, "art1", "B", time.Now())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawOwner != "A" {
		t.Errorf("Lookup inside the transaction saw %q, want A", sawOwner)
	}
	if got := readOwnersFile(t, path)["artifact:art1"]; got != "B" {
		t.Errorf("artifact:art1 = %q on disk, want B", got)
	}
}

// An error from fn must abort the whole transaction: nothing is written.
func TestEditWritesNothingWhenFnFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:art1":{"account":"A","at":"2026-09-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := errors.New("no such id")

	if err := owners.Edit(path, func(tx *owners.Tx) error {
		tx.Reassign(router.KindArtifact, "art1", "B", time.Now())
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("Edit returned %v, want %v", err, want)
	}
	if got := readOwnersFile(t, path)["artifact:art1"]; got != "A" {
		t.Errorf("artifact:art1 = %q on disk, want A: a failed transaction must write nothing", got)
	}
}

// A corrupt file is quarantined and reported, exactly as Open does, so
// `chottag own` can tell the operator their map moved rather than blaming
// their typing.
func TestEditReportsARecoveredCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var recovered bool
	if err := owners.Edit(path, func(tx *owners.Tx) error {
		recovered = tx.Recovered
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Error("Tx.Recovered was false for a corrupt file")
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("the corrupt file was not moved aside: %v", err)
	}
}
