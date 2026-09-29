package owners

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// TestEditWaitsForTheLock pins the actual cross-process guarantee Edit
// exists to provide (§4.7): fsutil.Lock is per-open-file, so a lock taken
// directly here, in this goroutine, still excludes Edit running in another
// goroutine — no subprocess needed. Without Edit's own Lock/unlock pair,
// nothing would notice the exclusion vanish (mutation: drop them and this
// is the only test in the suite that fails).
func TestEditWaitsForTheLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte(`{"artifact:art1":{"account":"A","at":"2026-09-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	unlock, err := fsutil.Lock(lockPathFor(path))
	if err != nil {
		t.Fatal(err)
	}

	fnRan := make(chan struct{})
	editDone := make(chan error, 1)
	go func() {
		editDone <- Edit(path, func(tx *Tx) error {
			close(fnRan)
			tx.Reassign(router.KindArtifact, "art1", "B", time.Now())
			return nil
		})
	}()

	select {
	case <-fnRan:
		t.Fatal("Edit's fn ran while this goroutine still held owners.lock: Edit did not wait for the lock")
	case <-time.After(200 * time.Millisecond):
		// Expected: Edit is blocked in fsutil.Lock.
	}

	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-editDone:
		if err != nil {
			t.Fatalf("Edit returned %v after the lock was released", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Edit did not complete after the lock was released")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]entry
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if got := doc["artifact:art1"].Account; got != "B" {
		t.Errorf("artifact:art1 = %q on disk, want B: Edit's write did not land after the lock was released", got)
	}
}
