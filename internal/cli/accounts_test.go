package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// runHome executes a chottag command with CHOTTAG_HOME pointed at dir.
//
// Named differently from the package's run(t, args...) helper (cli_test.go)
// because this one also sets CHOTTAG_HOME from a dir argument; both are used
// side by side across this package's test files.
func runHome(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("CHOTTAG_HOME", dir)
	var out, errb bytes.Buffer
	code := cli.Run("chottag", args, &out, &errb)
	return code, out.String(), errb.String()
}

func seed(t *testing.T, dir string, names ...string) store.Store {
	t.Helper()
	s := store.Store{Dir: dir}
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

func TestTagAndRemoteSetRoles(t *testing.T) {
	dir := t.TempDir()
	s := seed(t, dir, "B", "C")
	if code, out, errb := runHome(t, dir, "tag", "C"); code != 0 || !strings.Contains(out, "serving: C") {
		t.Fatalf("tag C = %d %q %q", code, out, errb)
	}
	st, _ := s.Load()
	if st.Serving != "C" {
		t.Fatalf("serving = %q", st.Serving)
	}
	if code, out, _ := runHome(t, dir, "remote", "B"); code != 0 || !strings.Contains(out, "remote: B") {
		t.Fatalf("remote B = %d %q", code, out)
	}
	if code, out, _ := runHome(t, dir, "remote"); code != 0 || !strings.Contains(out, "remote: B") {
		t.Fatalf("remote (show) = %d %q", code, out)
	}
	if code, _, errb := runHome(t, dir, "tag", "nope"); code != 1 || !strings.Contains(errb, "no such account") {
		t.Fatalf("tag nope = %d %q", code, errb)
	}
}

func TestNextRejectsExtraArgs(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "B", "C")
	if code, _, errb := runHome(t, dir, "next", "C"); code != 2 || !strings.Contains(errb, "next") {
		t.Fatalf("next C = %d %q, want exit 2", code, errb)
	}
}

// writeStatusFile marshals f to status.Path(home), matching the helper of
// the same name in accounts_internal_test.go (package cli, unreachable from
// here since this file is package cli_test).
func writeStatusFile(t *testing.T, home string, f status.File) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(status.Path(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(status.Path(home), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The dispatch guard in cli.go's "next" case is the only place deciding
// whether --force ever reaches runTag when driven through cli.Run (the path
// users actually hit): candidate_test.go and accounts_internal_test.go only
// exercise nextCandidate/runTag directly, so a guard wired wrong here would
// pass every one of those tests. This drives it through cli.Run and asserts
// on the PERSISTED serving account, not just the exit code (Task 4 review:
// printed output can look right while the written state is wrong).
func TestNextForceReachesRunTagThroughCliRunAndSwitches(t *testing.T) {
	dir := t.TempDir()
	s := seed(t, dir, "B", "C")
	now := time.Now()
	writeStatusFile(t, dir, status.File{Accounts: []status.Account{
		{Name: "C", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: now}},
	}})
	// Without --force this would refuse: confirm the fixture actually bites.
	if code, _, errb := runHome(t, dir, "next"); code != 3 {
		t.Fatalf("next (unforced) = %d %q, want exit 3 so --force has something to override", code, errb)
	}
	if code, out, errb := runHome(t, dir, "next", "--force"); code != 0 {
		t.Fatalf("next --force = %d %q %q, want exit 0", code, out, errb)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "C" {
		t.Fatalf("serving = %q, want C: --force must reach runTag through cli.Run and actually persist the switch", st.Serving)
	}
}

// The guard's only sanctioned extra token is a bare "--force"; anything
// trailing it is still rejected, same as any other extra argument. Pins the
// guard's exact shape so a later loosening (e.g. to slices.Contains, which
// would accept "next --force extra") does not go unnoticed.
func TestNextForceWithTrailingArgRejected(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "B", "C")
	if code, _, errb := runHome(t, dir, "next", "--force", "extra"); code != 2 || !strings.Contains(errb, "next") {
		t.Fatalf("next --force extra = %d %q, want exit 2", code, errb)
	}
}

// An explicit `tag <name>` always switches, even onto a NoRotate account:
// only `next`/auto-switch skip those (R29).
func TestExplicitTagSwitchesOntoANoRotateAccount(t *testing.T) {
	dir := t.TempDir()
	s := seed(t, dir, "B", "C")
	if _, err := s.Update(func(st *store.State) error {
		st.Accounts[1].NoRotate = true // C
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runHome(t, dir, "tag", "C")
	if code != 0 || !strings.Contains(out, "serving: C") {
		t.Fatalf("tag C (no-rotate) = %d %q %q", code, out, errb)
	}
	// The out-of-rotation warning (accounts.go's second warning block,
	// sibling to the limit warning) is otherwise unexercised: deleting it
	// left the suite green.
	if !strings.Contains(errb, "out of rotation") {
		t.Errorf("stderr = %q, want a warning that C is out of rotation", errb)
	}
	st, _ := s.Load()
	if st.Serving != "C" {
		t.Fatalf("serving = %q, want the explicitly tagged no-rotate account", st.Serving)
	}
}

func TestRemoteControlAliases(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "B", "C")
	for _, alias := range []string{"rc", "remote-control"} {
		if code, out, errb := runHome(t, dir, alias, "C"); code != 0 || !strings.Contains(out, "remote: C") {
			t.Fatalf("%s C = %d %q %q", alias, code, out, errb)
		}
	}
}

func TestNextSkipsAccountsOutOfRotation(t *testing.T) {
	dir := t.TempDir()
	s := seed(t, dir, "A", "B", "C")
	if _, err := s.Update(func(st *store.State) error {
		st.Serving = "A"
		for i := range st.Accounts {
			if st.Accounts[i].Name == "B" {
				st.Accounts[i].NoRotate = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runHome(t, dir, "next")
	if code != 0 || !strings.Contains(out, "serving: C") {
		t.Fatalf("next = %d %q, want it to skip B", code, out)
	}
	if !strings.Contains(out, "skipped B") {
		t.Fatalf("next output does not say what it skipped: %q", out)
	}
}

func TestNextWithNoCandidateFails(t *testing.T) {
	dir := t.TempDir()
	s := seed(t, dir, "B")
	if _, err := s.Update(func(st *store.State) error {
		st.Accounts[0].NoRotate = true
		st.Serving = "B"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runHome(t, dir, "next"); code != 3 || !strings.Contains(errb, "no account to switch to") {
		t.Fatalf("next = %d %q, want exit 3", code, errb)
	}
}

func TestAdoptRegistersSlotsWithLogins(t *testing.T) {
	dir := t.TempDir()
	// two slots; only the second reports a login
	for _, n := range []string{"B", "C"} {
		if err := os.MkdirAll(filepath.Join(dir, "accounts", n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\ncase \"$CLAUDE_CONFIG_DIR\" in\n*/C) echo '{\"loggedIn\":true,\"email\":\"c@example.com\"}' ;;\n*) echo '{\"loggedIn\":false}' ;;\nesac\n"), 0o755)

	code, out, errb := runHome(t, dir, "adopt", "--claude", fake)
	if code != 0 {
		t.Fatalf("adopt = %d %q %q", code, out, errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Name != "C" || st.Accounts[0].Email != "c@example.com" {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
	if st.Serving != "C" || st.Remote != "C" {
		t.Fatalf("first adopted account did not take both roles: %+v", st)
	}
	// idempotent, and honest about it: a second run must not claim it
	// adopted an account it merely found already registered.
	code, out, errb = runHome(t, dir, "adopt", "--claude", fake)
	if code != 0 {
		t.Fatal("second adopt failed")
	}
	if strings.Contains(out, "adopted C") {
		t.Fatalf("second adopt claimed to adopt an already-registered account: %q", out)
	}
	if !strings.Contains(out, "already registered: C") {
		t.Fatalf("second adopt did not say C was already registered: %q %q", out, errb)
	}
	st, _ = (store.Store{Dir: dir}).Load()
	if len(st.Accounts) != 1 {
		t.Fatalf("adopt duplicated accounts: %+v", st.Accounts)
	}
}

// TestAdoptRecognizesARenamedAccountByDirAndNeverReRegistersIt is C1/F173's
// adopt half: `chottag rename B Bee` keeps the account's slot at
// accounts/B (Dir never follows a rename). A later `adopt` walks the same
// slot dirs and, going by the DIRECTORY name alone, would compute "B" as
// the candidate name — an unrelated, unregistered-looking name from
// adopt's point of view unless it also checks Dir first. Matching by Name
// only would either silently overwrite Bee's login by registering a
// second account named "B" on the very same slot, or (worse) run `claude
// auth login` against it. It must instead recognize the slot as Bee's own,
// report it unchanged under Bee, and add nothing.
func TestAdoptRecognizesARenamedAccountByDirAndNeverReRegistersIt(t *testing.T) {
	dir := t.TempDir()
	slotDir := filepath.Join(dir, "accounts", "B")
	if err := os.MkdirAll(slotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "Bee", Dir: slotDir, Email: "bee@example.com"})
	}); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"bee@example.com\"}'\n"), 0o755)

	code, out, errb := runHome(t, dir, "adopt", "--claude", fake)
	if code != 0 {
		t.Fatalf("adopt = %d %q %q", code, out, errb)
	}
	if strings.Contains(out, "adopted B") || strings.Contains(errb, "adopted B") {
		t.Fatalf("adopt re-registered the renamed account's slot under B: %q %q", out, errb)
	}
	if !strings.Contains(out, "already registered: Bee") {
		t.Fatalf("adopt did not report the slot unchanged under its registered name Bee: %q", out)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Name != "Bee" || st.Accounts[0].Dir != slotDir {
		t.Fatalf("accounts = %+v, want exactly Bee still, on its original slot", st.Accounts)
	}
}

// Two accounts can share an email and differ only by org (F16), so status
// needs to show both: adopt must store the orgName field `claude auth
// status --json` reports, not just email.
func TestAdoptStoresTheOrgFromAuthStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\",\"orgName\":\"Acme\"}'\n"), 0o755)

	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake); code != 0 {
		t.Fatalf("adopt = %d %q", code, errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Org != "Acme" {
		t.Fatalf("accounts = %+v, want org Acme stored", st.Accounts)
	}
}

// On a real install, every account is typically already adopted, so the
// "unchanged" branch is the one that runs every time — if it never
// backfills a field adopt learned to probe after the account was first
// registered (Org, here), that field stays blank forever without a
// hand-edit of state.json. adopt already has the freshly probed value in
// hand each time it runs; it must apply it even when the account already
// exists.
func TestAdoptBackfillsOrgOnAnAlreadyRegisteredAccount(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeNoOrg := filepath.Join(dir, "fake-claude-no-org")
	os.WriteFile(fakeNoOrg, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\"}'\n"), 0o755)
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fakeNoOrg); code != 0 {
		t.Fatalf("first adopt = %d %q", code, errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil || len(st.Accounts) != 1 || st.Accounts[0].Org != "" {
		t.Fatalf("accounts after first adopt = %+v err=%v, want no org yet", st.Accounts, err)
	}

	// Re-adopt the same slot, this time with an org in the probed result —
	// as if this account was first adopted before `orgName` was read, and
	// the user reran adopt after upgrading chottag.
	fakeWithOrg := filepath.Join(dir, "fake-claude-with-org")
	os.WriteFile(fakeWithOrg, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\",\"orgName\":\"Acme\"}'\n"), 0o755)
	if code, out, errb := runHome(t, dir, "adopt", "--claude", fakeWithOrg); code != 0 {
		t.Fatalf("second adopt = %d %q %q", code, out, errb)
	} else if !strings.Contains(out, "already registered: B") {
		t.Fatalf("second adopt = %q, want it to report B as already registered (not re-adopted)", out)
	}

	st, err = (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Org != "Acme" {
		t.Fatalf("accounts after second adopt = %+v, want the org backfilled to Acme", st.Accounts)
	}
}

// Re-pointing a registered name at a different identity is exactly the
// silent swap F16 exists to prevent (A and B can share an email and differ
// only by org, so identity is never one field) — adopt must say so rather
// than reporting "unchanged".
func TestAdoptReportsAChangedEmailRatherThanUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeOld := filepath.Join(dir, "fake-claude-old")
	os.WriteFile(fakeOld, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"old@example.com\"}'\n"), 0o755)
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fakeOld); code != 0 {
		t.Fatalf("first adopt = %d %q", code, errb)
	}

	// Re-adopt the same slot dir; the probed email has changed underneath
	// it (e.g. someone logged a different account into the same slot).
	fakeNew := filepath.Join(dir, "fake-claude-new")
	os.WriteFile(fakeNew, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"new@example.com\"}'\n"), 0o755)
	code, out, errb := runHome(t, dir, "adopt", "--claude", fakeNew)
	if code != 0 {
		t.Fatalf("second adopt = %d %q %q", code, out, errb)
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("output %q reports the re-point as unchanged; a name now pointing at a different identity must say so", out)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Email != "new@example.com" {
		t.Fatalf("accounts = %+v, want the email updated to new@example.com", st.Accounts)
	}
}

// The guard against over-correcting: an adopt that changes nothing must keep
// saying "unchanged", not start claiming every re-adopt "updated" something.
func TestAdoptStillReportsUnchangedWhenNothingMoved(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\",\"orgName\":\"Acme\"}'\n"), 0o755)
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake); code != 0 {
		t.Fatalf("first adopt = %d %q", code, errb)
	}

	code, out, errb := runHome(t, dir, "adopt", "--claude", fake)
	if code != 0 {
		t.Fatalf("second adopt = %d %q %q", code, out, errb)
	}
	if strings.Contains(out, "updated") {
		t.Fatalf("second adopt with nothing changed reported an update: %q", out)
	}
	if !strings.Contains(out, "already registered: B") {
		t.Fatalf("second adopt did not report B as unchanged: %q", out)
	}
}

// slotEmail passes doc.Email through verbatim from a JSON document chottag
// does not control, and nothing guarantees it is non-empty when loggedIn is
// true. A re-adopt whose probe comes back logged-in with a blank email must
// not report "updated" with an empty identity in the message — it must stay
// "unchanged", the same as any other probe that found nothing new.
func TestAdoptDoesNotReportAnUpdateForABlankProbedEmail(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\"}'\n"), 0o755)
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake); code != 0 {
		t.Fatalf("first adopt = %d %q", code, errb)
	}

	fakeBlank := filepath.Join(dir, "fake-claude-blank")
	os.WriteFile(fakeBlank, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"\"}'\n"), 0o755)
	code, out, errb := runHome(t, dir, "adopt", "--claude", fakeBlank)
	if code != 0 {
		t.Fatalf("second adopt = %d %q %q", code, out, errb)
	}
	if strings.Contains(out, "updated") {
		t.Fatalf("second adopt with a blank probed email reported an update: %q", out)
	}
	if !strings.Contains(out, "already registered: B") {
		t.Fatalf("second adopt did not report B as unchanged: %q", out)
	}
}

// Same guard, on the org side: a re-adopt whose probe comes back logged-in
// with no orgName field must not report "updated" either.
func TestAdoptDoesNotReportAnUpdateForABlankProbedOrg(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeWithOrg := filepath.Join(dir, "fake-claude-with-org")
	os.WriteFile(fakeWithOrg, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\",\"orgName\":\"Acme\"}'\n"), 0o755)
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fakeWithOrg); code != 0 {
		t.Fatalf("first adopt = %d %q", code, errb)
	}

	fakeNoOrg := filepath.Join(dir, "fake-claude-no-org")
	os.WriteFile(fakeNoOrg, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"b@example.com\"}'\n"), 0o755)
	code, out, errb := runHome(t, dir, "adopt", "--claude", fakeNoOrg)
	if code != 0 {
		t.Fatalf("second adopt = %d %q %q", code, out, errb)
	}
	if strings.Contains(out, "updated") {
		t.Fatalf("second adopt with a blank probed org reported an update: %q", out)
	}
	if !strings.Contains(out, "already registered: B") {
		t.Fatalf("second adopt did not report B as unchanged: %q", out)
	}
}

// A prefix match ("Alpha" matching a lookup for "A") must never stand in
// for an exact-name check: adopting a slot dir named "A" must register it
// as its own account even though "Alpha" is already registered, not treat
// "Alpha" as if it were the already-registered account named "A" and
// silently drop the slot the user asked to adopt.
func TestAdoptDoesNotConfusePrefixMatchWithExactName(t *testing.T) {
	dir := t.TempDir()
	seed(t, dir, "Alpha")
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"a@example.com\"}'\n"), 0o755)

	code, out, errb := runHome(t, dir, "adopt", "--claude", fake)
	if code != 0 {
		t.Fatalf("adopt = %d %q %q", code, out, errb)
	}
	if !strings.Contains(out, "adopted A") {
		t.Fatalf("adopt did not register slot A because of the pre-existing Alpha: %q %q", out, errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range st.Accounts {
		names[a.Name] = true
	}
	if !names["Alpha"] || !names["A"] {
		t.Fatalf("accounts = %+v, want both Alpha and A registered", st.Accounts)
	}
}

// Two --name overrides that target the same account name is a genuine
// conflict (two different slot dirs cannot share one name): it must be
// reported, not silently resolved by treating the second dir as if it were
// already the first.
func TestAdoptNameCollisionIsReported(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"B", "C"} {
		if err := os.MkdirAll(filepath.Join(dir, "accounts", n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"x@example.com\"}'\n"), 0o755)

	code, out, errb := runHome(t, dir, "adopt", "--claude", fake, "--name", "B=X", "--name", "C=X")
	if code != 0 {
		t.Fatalf("adopt = %d %q %q", code, out, errb)
	}
	if !strings.Contains(errb, "X") {
		t.Fatalf("adopt did not report the name collision on stderr: %q", errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].Name != "X" || filepath.Base(st.Accounts[0].Dir) != "B" {
		t.Fatalf("accounts = %+v, want exactly one X registered from slot B", st.Accounts)
	}
}

// A --name override that names a slot directory that doesn't actually exist
// must not be silently ignored.
func TestAdoptWarnsOnNameOverrideForMissingSlot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":false}'\n"), 0o755)

	code, _, errb := runHome(t, dir, "adopt", "--claude", fake, "--name", "NOSUCH=Z")
	if code == 0 {
		t.Fatalf("adopt with no slots and no logins should fail: %q", errb)
	}
	if !strings.Contains(errb, "NOSUCH") {
		t.Fatalf("adopt did not warn about the missing slot dir NOSUCH: %q", errb)
	}
}

// A broken --claude path must be reported differently from a slot that
// simply has no login: one is an operator mistake, the other is expected.
func TestAdoptDistinguishesBrokenBinaryFromNoLogin(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runHome(t, dir, "adopt", "--claude", filepath.Join(dir, "no-such-claude-binary"))
	if code == 0 {
		t.Fatalf("adopt with a broken --claude should fail: %q", errb)
	}
	if strings.Contains(errb, "no login") {
		t.Fatalf("a broken --claude binary was reported as a logged-out slot: %q", errb)
	}
}

// Slot dirs are fixed when they are created, but the account names are not
// (roadmap D1, requirement R32), so adopt can name a slot differently from
// its directory.
func TestAdoptNameOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "C"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"team@example.com\"}'\n"), 0o755)

	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake, "--name", "C=A"); code != 0 {
		t.Fatalf("adopt --name = %d %q", code, errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil || len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v err=%v", st.Accounts, err)
	}
	a := st.Accounts[0]
	if a.Name != "A" || a.Email != "team@example.com" || filepath.Base(a.Dir) != "C" {
		t.Fatalf("account = %+v, want name A in dir .../C", a)
	}
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake, "--name", "C=../evil"); code != 2 {
		t.Fatalf("invalid override = %d %q, want exit 2", code, errb)
	}
}

// F258: an adopt that finds a registered slot unchanged confirms nothing new
// about the login, so it must not advance LoggedInAt: that stamp is what
// clears a needs-login, and `chottag update` (setup, adopt) re-runs it for
// every account. A changed identity is evidence of a re-login and does
// advance it.
func TestAdoptLeavesLoggedInAtAloneOnAnUnchangedSlotButAdvancesOnAnIdentityChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "accounts", "B"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := func(name, email string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\""+email+"\"}'\n"), 0o755)
		return p
	}
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake("c1", "b@example.com")); code != 0 {
		t.Fatalf("first adopt = %d %q", code, errb)
	}
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := (store.Store{Dir: dir}).Update(func(st *store.State) error {
		st.Accounts[0].LoggedInAt = t0
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake("c1", "b@example.com")); code != 0 {
		t.Fatalf("second adopt = %d %q", code, errb)
	}
	st, _ := (store.Store{Dir: dir}).Load()
	if got := st.Accounts[0].LoggedInAt; !got.Equal(t0) {
		t.Fatalf("LoggedInAt after an unchanged adopt = %v, want it left at %v", got, t0)
	}
	if code, _, errb := runHome(t, dir, "adopt", "--claude", fake("c2", "other@example.com")); code != 0 {
		t.Fatalf("third adopt = %d %q", code, errb)
	}
	st, _ = (store.Store{Dir: dir}).Load()
	if got := st.Accounts[0].LoggedInAt; !got.After(t0) {
		t.Fatalf("LoggedInAt after an identity change = %v, want it advanced past %v", got, t0)
	}
}
