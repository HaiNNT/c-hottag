package cli

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	usagehdr "github.com/HaiNNT/c-hottag/internal/usage"
)

// renameHome registers A and B in a fresh CHOTTAG_HOME, with B serving
// and A remote. Artifact a1 and session s1 are recorded as B's, and
// artifact a2 as A's.
func renameHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := seedOwnState(t, home, "A", "B")
	if _, err := s.Update(func(st *store.State) error { st.Serving, st.Remote = "B", "A"; return nil }); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	if err := owners.Edit(filepath.Join(home, "owners.json"), func(tx *owners.Tx) error {
		tx.Reassign(router.KindArtifact, "a1", "B", at)
		tx.Reassign(router.KindSession, "s1", "B", at)
		tx.Reassign(router.KindArtifact, "a2", "A", at)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return home
}

// renameOwnerOf reads one owner back through the daemon's own reader.
func renameOwnerOf(t *testing.T, home string, kind router.Kind, id string) string {
	t.Helper()
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	acct, _ := own.Lookup(kind, id)
	return acct
}

func renameState(t *testing.T, home string) store.State {
	t.Helper()
	st, err := store.Store{Dir: home}.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// failOwnersEdit makes rename's owners step fail until the returned
// restore runs. t.Cleanup restores it too, in case the test stops early.
func failOwnersEdit(t *testing.T) (restore func()) {
	t.Helper()
	prev := ownersEdit
	ownersEdit = func(string, func(*owners.Tx) error) error { return errors.New("disk full") }
	restore = func() { ownersEdit = prev }
	t.Cleanup(restore)
	return restore
}

func TestRenameMovesRolesAndOwnerEntries(t *testing.T) {
	home := renameHome(t)
	code, out, errs := runChottag(t, "rename", "B", "Bee")
	if code != exit.OK {
		t.Fatalf("rename = %d; stderr = %q", code, errs)
	}
	if want := "renamed B -> Bee (roles: serving; owner entries: 2)\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	st := renameState(t, home)
	if b := st.Accounts[1]; b.Name != "Bee" || b.Dir != filepath.Join(home, "accounts", "B") {
		t.Errorf("account = %+v, want Bee in the unchanged slot accounts/B", b)
	}
	if st.Serving != "Bee" || st.Remote != "A" {
		t.Errorf("roles = %s/%s, want Bee/A", st.Serving, st.Remote)
	}
	for _, c := range []struct {
		kind      router.Kind
		id, owner string
	}{{router.KindArtifact, "a1", "Bee"}, {router.KindSession, "s1", "Bee"}, {router.KindArtifact, "a2", "A"}} {
		if got := renameOwnerOf(t, home, c.kind, c.id); got != c.owner {
			t.Errorf("owner of %s %s = %q, want %q", c.kind, c.id, got, c.owner)
		}
	}
}

func TestRenameAllowsACaseOnlyChange(t *testing.T) {
	home := renameHome(t)
	code, out, errs := runChottag(t, "rename", "B", "b")
	if code != exit.OK || out != "renamed B -> b (roles: serving; owner entries: 2)\n" {
		t.Fatalf("rename = %d %q; stderr = %q", code, out, errs)
	}
	if st := renameState(t, home); st.Accounts[1].Name != "b" || st.Serving != "b" {
		t.Fatalf("state = %+v", st)
	}
	if got := renameOwnerOf(t, home, router.KindArtifact, "a1"); got != "b" {
		t.Fatalf("owner = %q, want b", got)
	}
}

func TestRenameRefusesANameAnotherAccountHolds(t *testing.T) {
	home := renameHome(t)
	statePath, ownersPath := filepath.Join(home, "state.json"), filepath.Join(home, "owners.json")
	stateBefore, ownersBefore := hashPath(t, statePath), hashPath(t, ownersPath)
	for _, newName := range []string{"A", "a"} {
		code, _, errs := runChottag(t, "rename", "B", newName)
		if code != exit.Usage || !strings.Contains(errs, "cannot rename B to "+newName) {
			t.Errorf("rename B %s = %d, stderr %q; want exit 2 naming the clash", newName, code, errs)
		}
	}
	if hashPath(t, statePath) != stateBefore || hashPath(t, ownersPath) != ownersBefore {
		t.Fatal("a refused rename wrote state.json or owners.json")
	}
}

func TestRenameRefusesBadArguments(t *testing.T) {
	home := renameHome(t)
	statePath := filepath.Join(home, "state.json")
	before := hashPath(t, statePath)
	for _, args := range [][]string{
		{"rename", "B", "bad/name"}, {"rename", "B", ".dot"}, {"rename", "B"},
		{"rename", "B", "C", "D"}, {"rename", "-x", "B", "C"},
	} {
		if code, _, _ := runChottag(t, args...); code != exit.Usage {
			t.Errorf("%v = %d, want %d", args, code, exit.Usage)
		}
	}
	if hashPath(t, statePath) != before {
		t.Fatal("a refused rename wrote state.json")
	}
}

func TestRenameNeverResolvesAPrefixOrAnEmail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := seedOwnState(t, home, "Alpha", "Beta")
	if _, err := s.Update(func(st *store.State) error { st.Accounts[0].Email = "al@example.com"; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"Al", "al@example.com", "Gamma"} {
		if code, _, errs := runChottag(t, "rename", old, "Delta"); code != exit.Error || !strings.Contains(errs, "no such account") {
			t.Errorf("rename %s = %d %q, want exit 1 no such account", old, code, errs)
		}
	}
	if st := renameState(t, home); st.Accounts[0].Name != "Alpha" || st.Accounts[1].Name != "Beta" {
		t.Fatalf("names changed: %+v", st.Accounts)
	}
}

func TestRenameResumesAfterTheOwnersStepFails(t *testing.T) {
	home := renameHome(t)
	restore := failOwnersEdit(t)
	code, _, errs := runChottag(t, "rename", "B", "Bee")
	restore()
	if code != exit.Error || !strings.Contains(errs, "run `chottag rename B Bee` again to finish") {
		t.Fatalf("interrupted rename = %d, stderr %q; want exit 1 and the re-run instruction", code, errs)
	}
	if st := renameState(t, home); st.Accounts[1].Name != "Bee" || st.Serving != "Bee" {
		t.Fatalf("step 1 did not land: %+v", st)
	}
	if got := renameOwnerOf(t, home, router.KindArtifact, "a1"); got != "B" {
		t.Fatalf("owner = %q, want B still (step 2 failed)", got)
	}

	code, out, errs := runChottag(t, "rename", "B", "Bee")
	if code != exit.OK || out != "renamed B -> Bee (resumed; owner entries: 2)\n" {
		t.Fatalf("re-run = %d %q; stderr %q", code, out, errs)
	}
	if got := renameOwnerOf(t, home, router.KindSession, "s1"); got != "Bee" {
		t.Fatalf("owner = %q, want Bee after the resume", got)
	}

	if code, _, errs := runChottag(t, "rename", "B", "Bee"); code != exit.Error || !strings.Contains(errs, "no such account") {
		t.Fatalf("third run = %d %q, want exit 1: nothing is left to resume", code, errs)
	}
}

func TestRenameResumesACaseOnlyChange(t *testing.T) {
	home := renameHome(t)
	restore := failOwnersEdit(t)
	if code, _, _ := runChottag(t, "rename", "B", "b"); code != exit.Error {
		t.Fatalf("interrupted rename = %d, want 1", code)
	}
	restore()
	code, out, errs := runChottag(t, "rename", "B", "b")
	if code != exit.OK || out != "renamed B -> b (resumed; owner entries: 2)\n" {
		t.Fatalf("re-run = %d %q; stderr %q", code, out, errs)
	}
	if got := renameOwnerOf(t, home, router.KindArtifact, "a1"); got != "b" {
		t.Fatalf("owner = %q, want b", got)
	}
}

// TestRenameIsANoOpWhenNothingIsPending pins fix round 1's minor 1: old
// still resolving (case-insensitively) to an account already spelled
// exactly new is ambiguous on its own — it also happens when a case-only
// rename's step 2 is genuinely resuming. Here nothing is pending in
// owners.json either (renameHome's entries for B are already spelled
// "B"), so this run must say "nothing to do", not "resumed".
func TestRenameIsANoOpWhenNothingIsPending(t *testing.T) {
	home := renameHome(t)
	statePath, ownersPath := filepath.Join(home, "state.json"), filepath.Join(home, "owners.json")
	stateBefore, ownersBefore := hashPath(t, statePath), hashPath(t, ownersPath)

	code, out, errs := runChottag(t, "rename", "B", "B")
	if code != exit.OK || out != "nothing to do: B is already named B\n" {
		t.Fatalf("rename = %d %q; stderr %q", code, out, errs)
	}
	if hashPath(t, statePath) != stateBefore || hashPath(t, ownersPath) != ownersBefore {
		t.Fatal("a no-op rename wrote state.json or owners.json")
	}
}

// TestRenameResumeWritesTheRegisteredSpellingNotTheTypedOne pins fix round
// 1's minor 2: once step 1 has landed, the account's registered name is
// authoritative. Resuming with a differently-cased spelling of new must
// still write owners.json (and report) the REGISTERED spelling, not
// whatever case was typed this time — otherwise a later, truly finished
// rename's exact-spelling "already done" check in RenameAccount would
// never recognize an entry written under the typed, unregistered case.
func TestRenameResumeWritesTheRegisteredSpellingNotTheTypedOne(t *testing.T) {
	home := renameHome(t)
	restore := failOwnersEdit(t)
	if code, _, _ := runChottag(t, "rename", "B", "Bee"); code != exit.Error {
		t.Fatalf("interrupted rename = %d, want 1", code)
	}
	restore()

	// Typed with a different case than the account actually registered
	// under ("Bee"): old (B) no longer resolves, so this hits the resume
	// path, which must resolve new's registered spelling itself.
	code, out, errs := runChottag(t, "rename", "B", "BEE")
	if code != exit.OK || out != "renamed B -> Bee (resumed; owner entries: 2)\n" {
		t.Fatalf("resume = %d %q; stderr %q; want the registered spelling Bee, not the typed BEE", code, out, errs)
	}
	if got := renameOwnerOf(t, home, router.KindArtifact, "a1"); got != "Bee" {
		t.Fatalf("owner = %q, want the registered spelling Bee", got)
	}
	if got := renameOwnerOf(t, home, router.KindSession, "s1"); got != "Bee" {
		t.Fatalf("owner = %q, want the registered spelling Bee", got)
	}
}

func TestRenameRepairsAHandEditedName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		st.Accounts = []store.Account{{Name: "old name", Dir: filepath.Join(home, "accounts", "x")}}
		st.Serving, st.Remote = "old name", "old name"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runChottag(t, "rename", "old name", "fixed")
	if code != exit.OK || out != "renamed old name -> fixed (roles: serving, remote; owner entries: 0)\n" {
		t.Fatalf("rename = %d %q; stderr %q", code, out, errs)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "rename moves roles and owner entries", command: "rename",
			setup: func(t *testing.T) []string { renameHome(t); return []string{"rename", "B", "Bee"} },
			check: func(t *testing.T, doc map[string]any) {
				roles, _ := doc["roles"].([]any)
				if doc["from"] != "B" || doc["to"] != "Bee" || len(roles) != 1 || roles[0] != "serving" ||
					doc["ownerEntries"] != float64(2) || doc["resumed"] != false || doc["changed"] != true {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "rename is a no-op when nothing is pending", command: "rename",
			setup: func(t *testing.T) []string { renameHome(t); return []string{"rename", "B", "B"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["from"] != "B" || doc["to"] != "B" || doc["resumed"] != false || doc["changed"] != false {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "rename an unknown account", command: "rename",
			setup:    func(t *testing.T) []string { renameHome(t); return []string{"rename", "Zed", "Zoe"} },
			wantExit: exit.Error, wantCode: codeUnknownAccount,
		},
		jsonCase{
			name: "rename onto another account's name", command: "rename",
			setup:    func(t *testing.T) []string { renameHome(t); return []string{"rename", "B", "a"} },
			wantExit: exit.Usage, wantCode: codeNameTaken,
		},
	)
}

// F171: a rename keeps the slot dir, so the daemon's next roster seeding
// (every roster tick and every usage hook call) carries the account's
// status row, recorded limit included, across to the new name, and
// `status --json` reports it under that name without the dir.
func TestARenamedAccountKeepsItsStatusRow(t *testing.T) {
	home := renameHome(t)
	state := func() (store.State, error) { return store.Store{Dir: home}.Load() }
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	seedRosterAtStartup(state, sink)
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	sink.observe("A", usagehdr.Snapshot{Known: true, At: time.Now()}, usagehdr.Verdict{Limited: true, Until: until, Window: "five_hour"})

	if code, _, errs := runChottag(t, "rename", "A", "work"); code != exit.OK {
		t.Fatalf("rename = %d; stderr = %q", code, errs)
	}
	seedRosterAtStartup(state, sink)
	sink.Close()

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	var work *status.Account
	for i := range f.Accounts {
		switch f.Accounts[i].Name {
		case "work":
			work = &f.Accounts[i]
		case "A":
			t.Fatalf("an A row survived the rename: %+v", f.Accounts)
		}
	}
	if work == nil || !work.Limited || !work.LimitedUntil.Equal(until) || work.Dir != renameState(t, home).Accounts[0].Dir {
		t.Fatalf("work row = %+v, want A's limited row, carried by its slot dir", work)
	}

	code, out, errs := runChottag(t, "status", "--json")
	if code != exit.OK {
		t.Fatalf("status --json = %d; stderr = %q", code, errs)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	found := false
	for _, a := range doc.Accounts {
		if _, ok := a["dir"]; ok {
			t.Fatalf("status --json row carries the slot dir: %v", a)
		}
		if a["name"] == "work" {
			found = a["limited"] == true
		}
	}
	if !found {
		t.Fatalf("status --json = %s, want work reported limited", out)
	}
}

// F171 review: `next` reads status.json itself, so it must match rows to
// the roster by slot dir too. With no daemon to rewrite the file, A's
// limited row still says "A" after `rename A work`; next from B must still
// skip work as limited, not read it as unknown and switch to it.
func TestNextSkipsARenamedLimitedAccountWithNoDaemon(t *testing.T) {
	home := renameHome(t) // A and B, B serving
	var f status.File
	f.EnsureRoster(accountMembers(renameState(t, home)))
	now := time.Now()
	f.Observe("A", usagehdr.Snapshot{Known: true, At: now}, usagehdr.Verdict{Limited: true, Until: now.Add(time.Hour), Window: "five_hour"})
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runChottag(t, "rename", "A", "work"); code != exit.OK {
		t.Fatalf("rename = %d; stderr = %q", code, errs)
	}
	code, out, errs := runChottag(t, "next")
	if code == exit.OK || renameState(t, home).Serving != "B" {
		t.Fatalf("next = %d %q (stderr %q), serving %q: want no switch to the limited, renamed account", code, out, errs, renameState(t, home).Serving)
	}
}
