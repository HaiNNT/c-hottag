package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// seedOwnState registers accounts named by names and returns the store, so
// an own_test.go test can seed both state.json (accounts) and owners.json
// (existing attributions) the way runOwn will read them.
func seedOwnState(t *testing.T, home string, names ...string) store.Store {
	t.Helper()
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range names {
			d, err := s.SlotDir(n)
			if err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Dir: d}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// seedOwners writes owners.json under home with a single kind/id -> account
// entry, then closes the map so the write reaches disk before runOwn opens
// it.
func seedOwners(t *testing.T, home string, kind router.Kind, id, account string) {
	t.Helper()
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := own.Reassign(kind, id, account, time.Now()); err != nil {
		t.Fatal(err)
	}
	own.Close()
}

// seedDaemonHeartbeat writes cache/status.json with a Daemon object stamped
// at now-age, the on-disk shape a running (or recently-killed) daemon
// leaves behind. Built directly as a status.File{Daemon: &status.Daemon{...}}
// literal rather than via status.File.SetDaemon — not because SetDaemon
// couldn't express this: its own `now` argument sets Heartbeat just as
// directly, to whatever age a caller passes. The only thing SetDaemon adds
// is unconditionally clearing Running, which is irrelevant either way at
// this layer: runOwn never reads a stored Running bit, only
// DaemonRunningAt(now)'s recomputation of it from Heartbeat's age.
func seedDaemonHeartbeat(t *testing.T, home string, age time.Duration) {
	t.Helper()
	f := status.File{Daemon: &status.Daemon{Heartbeat: time.Now().Add(-age)}}
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

func TestOwnReassignsAKnownID(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	seedOwners(t, home, router.KindArtifact, "art1", "A")

	var out, errb bytes.Buffer
	if code := runOwn(home, []string{"artifact", "art1", "B"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("runOwn = %d, want 0; stderr=%q", code, errb.String())
	}
	// Reopen from disk: the point is that the change was persisted.
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if acct, ok := own.Lookup(router.KindArtifact, "art1"); !ok || acct != "B" {
		t.Fatalf("owner = %q/%v, want B/true", acct, ok)
	}
}

func TestOwnRejectsAnUnknownID(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")

	var out, errb bytes.Buffer
	code := runOwn(home, []string{"artifact", "typo", "B"}, newReporter(false, &out, &errb))
	if code == 0 {
		t.Fatal("runOwn accepted an id the map does not know; a typo must not create an owner row")
	}
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if _, ok := own.Lookup(router.KindArtifact, "typo"); ok {
		t.Error("the rejected id was created anyway")
	}
}

func TestOwnRejectsAnUnknownKind(t *testing.T) {
	home := t.TempDir()
	var out, errb bytes.Buffer
	// "routine" is the trap: §4.1's prose names it, router.Kind does not.
	if code := runOwn(home, []string{"routine", "r1", "B"}, newReporter(false, &out, &errb)); code != 2 {
		t.Fatalf("runOwn = %d for an unknown kind, want 2 (usage error)", code)
	}
	if !strings.Contains(errb.String(), "connector") {
		t.Errorf("the error should list the valid kinds; got %q", errb.String())
	}
}

// TestOwnReportsACorruptOwnersMapOnTheQueryForm pins fix round 2, item 4:
// owners.Open (called above, before either form) renames a corrupt
// owners.json aside and starts empty, setting Recovered — runOwn used to
// ignore that, so the very next thing an operator saw was "has no recorded
// owner" about an id that was there a moment ago, with nothing saying their
// map, not their typing, is why.
func TestOwnReportsACorruptOwnersMapOnTheQueryForm(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	if err := os.WriteFile(filepath.Join(home, "owners.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	runOwn(home, []string{"artifact", "art1"}, newReporter(false, &out, &errb))
	if !strings.Contains(errb.String(), "owners.json was corrupt and has been moved aside") {
		t.Errorf("stderr = %q, want it to report the corrupt owners.json", errb.String())
	}
}

// TestOwnReportsACorruptOwnersMapOnTheReassignForm is the write form's
// sibling of the test above: Open's rename happens before either form runs,
// so the reassign form must report it too, not just the query form.
func TestOwnReportsACorruptOwnersMapOnTheReassignForm(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	if err := os.WriteFile(filepath.Join(home, "owners.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	runOwn(home, []string{"artifact", "art1", "B"}, newReporter(false, &out, &errb))
	if !strings.Contains(errb.String(), "owners.json was corrupt and has been moved aside") {
		t.Errorf("stderr = %q, want it to report the corrupt owners.json", errb.String())
	}
}

func TestOwnRejectsAnUnknownAccount(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	seedOwners(t, home, router.KindArtifact, "art1", "A")

	var out, errb bytes.Buffer
	if code := runOwn(home, []string{"artifact", "art1", "NoSuchAccount"}, newReporter(false, &out, &errb)); code == 0 {
		t.Fatal("runOwn accepted an account that is not registered")
	}
}

func TestOwnWithTwoArgsPrintsTheCurrentOwner(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	// Seeded owner is "B", not "A": a stub that hardcoded the first
	// account's name (or any other coincidental default) would fail this,
	// where the original "contains A" assertion could not tell a real
	// Lookup result from a hardcoded "A" — both accounts were named A/B,
	// so "A" was always in the seed regardless of what runOwn actually did.
	seedOwners(t, home, router.KindArtifact, "art1", "B")

	var out, errb bytes.Buffer
	if code := runOwn(home, []string{"artifact", "art1"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("runOwn = %d, want 0; stderr=%q", code, errb.String())
	}
	if want := "artifact art1: B\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// TestOwnReassignsWhileADaemonLooksLive pins the M1c5b Task 4 change: M1c5
// refused with exit 3 here (a fresh heartbeat meant a daemon was very likely
// running and holding a stale in-memory copy of owners.json, whose next
// unrelated write would silently revert the correction, F96). §4.7's
// owners.lock closes that race — owners.Edit and the daemon's writer take
// the same lock, and the daemon adopts the correction on its next roster
// tick — so refusing here would now just be withholding a working feature.
func TestOwnReassignsWhileADaemonLooksLive(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	seedOwners(t, home, router.KindArtifact, "art1", "A")
	seedDaemonHeartbeat(t, home, time.Second)

	var out, errb bytes.Buffer
	if code := runOwn(home, []string{"artifact", "art1", "B"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("runOwn = %d, want 0; stderr=%q", code, errb.String())
	}
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if acct, ok := own.Lookup(router.KindArtifact, "art1"); !ok || acct != "B" {
		t.Fatalf("owner = %q/%v, want the reassignment to have applied and persisted as B", acct, ok)
	}
}

// TestOwnReassignsAfterAServeCleanShutdown pins fix round 2, item 1: nothing
// used to clear the daemon object on a clean shutdown, so `chottag own`
// refused (F96's live-daemon case — the refusal itself is gone now; see
// TestOwnReassignsWhileADaemonLooksLive above, which replaced it once
// owners.lock made refusing unnecessary) for up to status.DaemonStaleAfter
// (90s) after the operator had already run `chottag daemon stop` — exactly
// the remedy the refusal told them to run. This drives serve() itself, the
// same way a real daemon's last two
// actions do (setDaemon, then a clean shutdown that calls sink.clearDaemon()
// immediately before sink.Close()), and then asserts runOwn's reassign form
// succeeds immediately afterward, with no wait.
func TestOwnReassignsAfterAServeCleanShutdown(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	seedOwners(t, home, router.KindArtifact, "art1", "A")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sink, err := newStatusSink(home, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	sink.setDaemon(0, 0, 0, 0, 0, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ln, http.NewServeMux(), func() int { return 0 }, func() int { return 0 }, func() {}, sink)
	}()
	waitForListening(t, ln.Addr().String())
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("serve did not return after its context was cancelled")
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	f.DaemonRunningAt(time.Now())
	if f.Daemon != nil && f.Daemon.Running {
		t.Fatalf("Daemon = %+v after a clean shutdown, want it not running", f.Daemon)
	}

	var out, errb bytes.Buffer
	if code := runOwn(home, []string{"artifact", "art1", "B"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("runOwn = %d after a clean shutdown, want 0; stderr=%q", code, errb.String())
	}
}

// TestOwnQueryWorksEvenWithALiveDaemon pins the read-only two-arg form's
// exemption from the F96 guard: it never writes, so a live daemon's stale
// in-memory copy of owners.json is nothing it needs to worry about.
func TestOwnQueryWorksEvenWithALiveDaemon(t *testing.T) {
	home := t.TempDir()
	seedOwnState(t, home, "A", "B")
	seedOwners(t, home, router.KindArtifact, "art1", "A")
	seedDaemonHeartbeat(t, home, time.Second)

	var out, errb bytes.Buffer
	if code := runOwn(home, []string{"artifact", "art1"}, newReporter(false, &out, &errb)); code != 0 {
		t.Fatalf("runOwn = %d, want 0; stderr=%q", code, errb.String())
	}
	if want := "artifact art1: A\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}
