package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// cliReadOwnersFile reads owners.json and returns key -> account. Local
// equivalent of internal/owners' test helper `readOwnersFile`: that one
// lives in the owners_test package and is not exported for this package to
// reuse (F103 — the packages genuinely cannot share it). Named with a "cli"
// prefix, not the same name with a different signature, so the two cannot
// be mistaken for each other by a reader jumping between packages (whole-
// branch review, Item 7 — they had drifted at birth: same name, this one
// taking the extra `why` parameter below).
//
// why names what the caller is checking for, so a failure says WHAT was
// expected on disk rather than just that a read or parse failed.
func cliReadOwnersFile(t *testing.T, path, why string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (%s): %v", path, why, err)
	}
	var doc map[string]struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parsing %s (%s): %v", path, why, err)
	}
	out := map[string]string{}
	for k, v := range doc {
		out[k] = v.Account
	}
	return out
}

// cliWaitForOwnersWrite polls the file at path until want reports true or
// the bound expires. Local equivalent of internal/owners' test helper
// `waitForWrite` (see cliReadOwnersFile above for the naming rationale and
// why this is not shared). why names what the caller is waiting for, so a
// timeout says WHICH write never landed rather than just that some write
// didn't.
func cliWaitForOwnersWrite(t *testing.T, path, why string, want func(map[string]string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			var doc map[string]struct {
				Account string `json:"account"`
			}
			if json.Unmarshal(b, &doc) == nil {
				got := map[string]string{}
				for k, v := range doc {
					got[k] = v.Account
				}
				if want(got) {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: write never landed within the bound", why)
}

// ownersTick must do BOTH halves. Mutation: delete either line in
// ownersTick's returned closure; one of these assertions must fail.
//
// Two Maps on one path is the faithful reproduction of F96 (two independent
// in-memory maps, not two OS processes), so this needs no subprocess.
//
// owners.SetWrite is package-private to internal/owners' own tests (its
// export_test.go is not visible from here), unlike internal/owners'
// version of this test which the brief modelled this on. This package
// fails the write the same way TestRunProxyWiresOwnersSaveErrorThrottleIntoOwnSetOnError
// already does: chmod the directory unwritable so fsutil.WriteFileAtomic's
// os.CreateTemp in that directory genuinely fails. owners.lock already
// exists by then (created by the first successful write's fsutil.Lock), so
// only the temp-file creation — not the lock acquisition or the read of
// the existing file — is what the chmod blocks.
func TestOwnersTickReloadsAndFlushes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	t0 := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	t.Cleanup(func() { os.Chmod(dir, 0o700) }) // restore before t.TempDir()'s own cleanup removes it

	daemon, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	reported := make(chan error, 4)
	daemon.SetOnError(func(err error) {
		select {
		case reported <- err:
		default:
		}
	})

	if err := daemon.Record(router.KindArtifact, []string{"art1"}, "A", t0); err != nil {
		t.Fatal(err)
	}
	cliWaitForOwnersWrite(t, path, "the seeding Record(art1, A) must land before the second process opens the file", func(got map[string]string) bool {
		return got["artifact:art1"] == "A"
	})

	// A second process corrects the attribution and exits.
	other, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Reassign(router.KindArtifact, "art1", "B", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if got := cliReadOwnersFile(t, path, "precondition: the other process's Reassign must be on disk before the daemon's next write"); got["artifact:art1"] != "B" {
		t.Fatalf("precondition: disk artifact:art1 = %q, want B", got["artifact:art1"])
	}

	// Meanwhile the daemon's own next write fails, folding its change back
	// into the dirty set with no wake queued.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := daemon.Record(router.KindArtifact, []string{"art2"}, "C", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		// Pin WHICH step failed: a permission-denied error from the chmod
		// above, not some unrelated failure the bare-channel-fire form
		// could not distinguish from.
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("reported error = %v, want a permission-denied error from the chmod 0500 directory", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed write was never reported")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// One tick must do both halves.
	if err := ownersTick(daemon, nil)(); err != nil {
		t.Fatal(err)
	}

	if got, ok := daemon.Lookup(router.KindArtifact, "art1"); got != "B" {
		t.Errorf("daemon Lookup(art1) = %q (found=%v) after one tick, want B: the reload half did not run", got, ok)
	}
	cliWaitForOwnersWrite(t, path, "NudgePending's flush of the folded-back art2 write must land", func(got map[string]string) bool {
		return got["artifact:art2"] == "C"
	})
	if got := cliReadOwnersFile(t, path, "the flush must not clobber the other process's correction"); got["artifact:art1"] != "B" {
		t.Errorf("disk artifact:art1 = %q after the flush, want B: the flush clobbered the other process's correction", got["artifact:art1"])
	}
}

// Item 6 (fix round 1): a Reload error (e.g. a corrupt owners.json) must
// reach ownersTick's OWN onReloadError callback, not watchRoster's generic
// onError — that one is unthrottled and labelled for the roster watch, so a
// persistently corrupt file errored on every 5s tick forever under the
// wrong name. Calling ownersTick directly, rather than through watchRoster,
// isolates this from the roster's own onError entirely.
func TestOwnersTickReportsAReloadErrorThroughItsOwnCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "owners.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := owners.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// The corruption must land AFTER Open reads the file cleanly: Open's
	// own corrupt-file handling renames a corrupt file aside before Reload
	// ever gets a chance to see one.
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	reported := make(chan error, 1)
	if err := ownersTick(m, func(err error) {
		select {
		case reported <- err:
		default:
		}
	})(); err == nil {
		t.Fatal("ownersTick returned nil for a corrupt owners.json")
	}
	select {
	case err := <-reported:
		if err == nil || !strings.Contains(err.Error(), "is corrupt") {
			t.Fatalf("reported error = %v, want Reload's own \"...is corrupt\" error, not some other failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ownersTick did not report the reload error through its own onReloadError callback")
	}
}

// The roster tick must run its owners-map work. Mutation: deleting the
// ownersTick() call from watchRoster's tick body must fail this.
//
// This pins only that the tick CALLS the seam; that the seam does both
// halves is TestOwnersTickReloadsAndFlushes' job. Neither test substitutes
// for the other.
func TestRosterTickReloadsTheOwnerMap(t *testing.T) {
	ticked := make(chan struct{}, 4)
	tick := make(chan time.Time, 1)
	processed := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	cache := store.NewCache(store.Store{Dir: dir})
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchRoster(ctx, cache, func(string) error { return nil }, func(string, string) (int, error) { return 0, nil }, tick,
			func(error) {}, processed, nil, func() error {
				select {
				case ticked <- struct{}{}:
				default:
				}
				return nil
			}, nil)
	}()

	tick <- time.Now()
	select {
	case <-ticked:
	case <-time.After(5 * time.Second):
		t.Fatal("the roster tick did not run its owners-map work")
	}
	cancel()
	<-done
}
