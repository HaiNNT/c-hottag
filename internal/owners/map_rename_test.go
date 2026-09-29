package owners_test

import (
	"path/filepath"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// TestMapRenameAccountRewritesInMemoryAndPersists is I1 (F174): the
// daemon's in-memory Map, not just the CLI's short-lived owners.Tx, must be
// able to rewrite every entry naming an old account to its new name, and
// have that reach disk through the Map's normal (asynchronous) write path
// — the same one Forget and Reassign use — not a second, ad hoc writer.
func TestMapRenameAccountRewritesInMemoryAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "owners.json")
	m, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"a1"}, "B", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindSession, []string{"s1"}, "b", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Record(router.KindArtifact, []string{"a2"}, "A", t0); err != nil {
		t.Fatal(err)
	}

	n, err := m.RenameAccount("B", "Bee")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rewrote %d, want 2 (both spellings of B)", n)
	}
	// In-memory reflects the rename immediately.
	if a, ok := m.Lookup(router.KindArtifact, "a1"); !ok || a != "Bee" {
		t.Fatalf("Lookup(a1) = %q, %v; want Bee", a, ok)
	}
	if a, ok := m.Lookup(router.KindSession, "s1"); !ok || a != "Bee" {
		t.Fatalf("Lookup(s1) = %q, %v; want Bee", a, ok)
	}
	if a, ok := m.Lookup(router.KindArtifact, "a2"); !ok || a != "A" {
		t.Fatalf("Lookup(a2) = %q, %v; want A untouched", a, ok)
	}

	m.Close() // the write is asynchronous; drain before reopening below
	m2, err := owners.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if a, ok := m2.Lookup(router.KindArtifact, "a1"); !ok || a != "Bee" {
		t.Fatalf("persisted Lookup(a1) = %q, %v; want Bee", a, ok)
	}
	if a, ok := m2.Lookup(router.KindSession, "s1"); !ok || a != "Bee" {
		t.Fatalf("persisted Lookup(s1) = %q, %v; want Bee", a, ok)
	}
	if a, ok := m2.Lookup(router.KindArtifact, "a2"); !ok || a != "A" {
		t.Fatalf("persisted Lookup(a2) = %q, %v; want A untouched", a, ok)
	}
}

// TestMapRenameAccountIsIdempotent mirrors owners.Tx.RenameAccount's own
// idempotence (rename_test.go): re-running a finished rename — or running
// it a second time under a differently-cased spelling of the old name —
// rewrites nothing and triggers no write.
func TestMapRenameAccountIsIdempotent(t *testing.T) {
	m, err := owners.Open(filepath.Join(t.TempDir(), "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Record(router.KindArtifact, []string{"a1"}, "Bee", t0); err != nil {
		t.Fatal(err)
	}
	before := owners.Writes(m)

	n1, err := m.RenameAccount("B", "Bee")
	if err != nil {
		t.Fatal(err)
	}
	n2, err := m.RenameAccount("bee", "Bee")
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 0 || n2 != 0 {
		t.Fatalf("n1 = %d, n2 = %d; want nothing rewritten when every entry is already spelled Bee", n1, n2)
	}
	if got := owners.Writes(m); got != before {
		t.Fatalf("Writes = %d, want %d (a no-op rename must not queue a write)", got, before)
	}
	if a, ok := m.Lookup(router.KindArtifact, "a1"); !ok || a != "Bee" {
		t.Fatalf("Lookup(a1) = %q, %v; want Bee untouched", a, ok)
	}
}
