package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// writeFakeClaude writes a `#!/bin/sh` script that prints statusJSON and
// exits 0, so `claude auth status --json` (slotEmail) sees a canned
// identity without ever touching a real claude installation.
func writeFakeClaude(t *testing.T, statusJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat <<'EOF'\n" + statusJSON + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubAuthExec installs a fake `claude auth <sub>` for one test and records
// what would have been run. SetAuthExecForTest's return value is the
// restore closure to undo exactly this swap (mirroring
// shim.SetSeamsForTest) — passing t.Cleanup a func that reinstalled the
// real exec instead would disarm TestMain's panicking default for every
// test that runs after the first stubbed one (see
// TestAuthExecSeamStillPanicsAfterAStubbedTestRestores below).
func stubAuthExec(t *testing.T, fn func(bin, slotDir, sub string) error) *[]string {
	t.Helper()
	var calls []string
	t.Cleanup(SetAuthExecForTest(func(bin, slotDir, sub string, _ io.Reader, _, _ io.Writer) error {
		calls = append(calls, sub+" "+slotDir)
		return fn(bin, slotDir, sub)
	}))
	return &calls
}

func TestLoginCreatesTheSlotAndRegistersTheAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com","orgName":"Org"}`)
	calls := stubAuthExec(t, func(_, slotDir, _ string) error {
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	})

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "login ") {
		t.Fatalf("auth calls = %v, want one login", *calls)
	}
	st, err := (store.Store{Dir: home}).Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Find("A")
	if err != nil {
		t.Fatalf("account A not registered: %v", err)
	}
	if a.Email != "a@example.com" || a.Org != "Org" {
		t.Errorf("account = %+v, want the probed email and org", *a)
	}
	// First account takes both roles (store.State.Add already does this).
	if st.Serving != "A" || st.Remote != "A" {
		t.Errorf("serving=%q remote=%q, want both A", st.Serving, st.Remote)
	}
	// Mutation 5: a slot holds a login and must not be world-readable.
	info, err := os.Stat(filepath.Join(home, "accounts", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("slot dir mode = %o, want 0700", mode)
	}
}

// F2: the help text shows `login NAME [--claude PATH]`, name first — that
// order must work too, not just the flags-first form every other test here
// happens to use. Go's flag package stops at the first non-flag token, so a
// bare fs.Parse would leave --claude sitting unparsed in fs.Args() and this
// call failing exit.Usage.
func TestLoginAcceptsFlagsAfterTheName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com","orgName":"Org"}`)
	calls := stubAuthExec(t, func(_, slotDir, _ string) error {
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	})

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"A", "--claude", fake}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d, want exit.OK; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "login ") {
		t.Fatalf("auth calls = %v, want one login", *calls)
	}
	st, err := (store.Store{Dir: home}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Find("A"); err != nil {
		t.Errorf("account A not registered: %v", err)
	}
}

// F4/P9: an existing account's registered Dir can be spelled through a path
// ALIAS of <h>/accounts (e.g. CHOTTAG_HOME was a symlinked spelling when it
// was first registered) — cleanup must still recognise a dir it just
// created there and remove it, not just a canonical <h>/accounts/<name>
// spelling.
func TestLoginCleansUpAnAliasedSlotDirItCreatedWhenLoginFails(t *testing.T) {
	realHome := t.TempDir()
	aliasParent := t.TempDir()
	aliasHome := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(realHome, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHOTTAG_HOME", realHome)

	aliasedDir := filepath.Join(aliasHome, "accounts", "A")
	s := store.Store{Dir: realHome}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Email: "old@example.com", Dir: aliasedDir})
	}); err != nil {
		t.Fatal(err)
	}

	fake := writeFakeClaude(t, `{"loggedIn":false}`)
	stubAuthExec(t, func(string, string, string) error { return errors.New("user cancelled") })

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogin = %d, want exit.Error; stderr=%q", got, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(realHome, "accounts", "A")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("aliased slot dir survives a failed re-login: stat err = %v (P9)", err)
	}
}

// A failed login must not leave a half-made slot behind: an empty slot dir
// would later look adoptable.
func TestLoginRemovesTheSlotItCreatedWhenLoginFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	fake := writeFakeClaude(t, `{"loggedIn":false}`)
	stubAuthExec(t, func(string, string, string) error { return errors.New("user cancelled") })

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogin = %d, want exit.Error", got)
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "A")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir survives a failed login: stat err = %v", err)
	}
	st, _ := (store.Store{Dir: home}).Load()
	if _, err := st.Find("A"); err == nil {
		t.Error("A was registered despite the login failing")
	}
}

// F5: a real browser login can still leave nothing registered, if the FINAL
// state.json write fails after everything else succeeded — the slot cleanup
// above must also run from there, or a slot this call itself created
// survives to later look adoptable despite holding a login nothing ever
// registered.
func TestLoginCleansUpTheSlotWhenTheFinalUpdateFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission bits")
	}
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com","orgName":"Org"}`)
	stubAuthExec(t, func(_, slotDir, _ string) error {
		if err := os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
			return err
		}
		// The slot dir and its credential are already real by this point;
		// only now does state.json's own write need to start failing, or
		// the earlier os.Stat/MkdirAll calls that create the slot itself
		// would fail instead and this test would prove nothing about the
		// final Update specifically.
		return os.Chmod(home, 0o500)
	})
	t.Cleanup(func() { os.Chmod(home, 0o700) }) // restore before t.TempDir()'s own cleanup removes it

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogin = %d, want exit.Error; stderr=%q", got, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "A")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir survives a failed final Update: stat err = %v", err)
	}
}

// F15: a membership test must match exactly. st.Find resolves a unique name
// PREFIX, so using it here would treat "Alpha" as the registered "A".
func TestLoginWarnsWhenTheEmailIsAlreadyRegisteredUnderAnotherName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Email: "shared@example.com", Dir: filepath.Join(home, "accounts", "A")})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"shared@example.com","orgName":"Org"}`)
	stubAuthExec(t, func(_, slotDir, _ string) error { return nil })

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "shared@example.com") || !strings.Contains(errBuf.String(), "already registered as account A") {
		t.Errorf("stderr = %q, want a warning naming the email and the account already holding it", errBuf.String())
	}
}

// Re-login on an existing name reuses its slot rather than making a second.
func TestLoginOnAnExistingNameReusesItsSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	dir := filepath.Join(home, "accounts", "A")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Email: "old@example.com", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"new@example.com","orgName":"Org"}`)
	var seenDir string
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, _, _ io.Writer) error {
		seenDir = slotDir
		return nil
	}))

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if seenDir != dir {
		t.Errorf("login ran against %q, want the existing slot %q", seenDir, dir)
	}
	st, _ := s.Load()
	if len(st.Accounts) != 1 {
		t.Errorf("accounts = %d, want 1: re-login must not add a second entry", len(st.Accounts))
	}
	a, _ := st.Find("A")
	if a.Email != "new@example.com" {
		t.Errorf("email = %q, want the re-probed new@example.com", a.Email)
	}
	// Mutation 3: re-logging into the same account must never warn that its
	// own email is "already registered" under another name.
	if strings.Contains(errBuf.String(), "already registered") {
		t.Errorf("stderr = %q, want no duplicate-email warning for a re-login onto the same name", errBuf.String())
	}
}

// Mutation 3, direct case: the previous test's re-probed email always
// differs from the stored one, so the duplicate-email warning's email
// comparison never even matches there — it cannot observe the name guard
// being dropped. Here the re-login reports the SAME email the account
// already has, so a name guard that always evaluates true would wrongly
// warn that the account's own email is "already registered" under itself.
func TestLoginReLoginWithUnchangedEmailNeverWarnsAboutItself(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	dir := filepath.Join(home, "accounts", "A")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Email: "same@example.com", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"same@example.com","orgName":"Org"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if strings.Contains(errBuf.String(), "already registered") {
		t.Errorf("stderr = %q, want no self-referential duplicate-email warning", errBuf.String())
	}
}

// Fix round 4, item 4: `login`'s JSON result must report the account's own
// stored casing, not whatever the user typed at the prompt — the text line
// keeps echoing the typed spelling unchanged.
func TestLoginJSONReportsTheCanonicalStoredNameNotWhatWasTyped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	dir := filepath.Join(home, "accounts", "Alice")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "Alice", Email: "old@example.com", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"new@example.com","orgName":"Org"}`)
	stubAuthExec(t, func(_, _, _ string) error { return nil })

	var jsonOut, jsonErr bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "alice"}, strings.NewReader(""), newReporter(true, &jsonOut, &jsonErr)); got != exit.OK {
		t.Fatalf("runLogin (json) = %d; stderr=%q", got, jsonErr.String())
	}
	doc := decodeOneDocument(t, jsonOut.String())
	if doc["account"] != "Alice" {
		t.Errorf("account = %v, want the canonical stored casing %q, not what was typed", doc["account"], "Alice")
	}

	var textOut, textErr bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "alice"}, strings.NewReader(""), newReporter(false, &textOut, &textErr)); got != exit.OK {
		t.Fatalf("runLogin (text) = %d; stderr=%q", got, textErr.String())
	}
	if want := "logged in: alice (new@example.com)\n"; textOut.String() != want {
		t.Errorf("text stdout = %q, want %q: the text line must keep echoing what was typed", textOut.String(), want)
	}
}

func TestLoginRejectsAnInvalidName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"../escape"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got == exit.OK {
		t.Error("runLogin accepted a name that is not a ValidName")
	}
}

// F15 gap check (mutation 2): registering "Alpha" first must never make a
// login for "A" reuse Alpha's slot — EqualFold, not HasPrefix, must gate the
// existing-slot lookup.
func TestLoginOnANewNameThatIsAPrefixOfAnExistingNameMakesANewSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	alphaDir := filepath.Join(home, "accounts", "Alpha")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "Alpha", Email: "alpha@example.com", Dir: alphaDir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com","orgName":"Org"}`)
	var seenDir string
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, _, _ io.Writer) error {
		seenDir = slotDir
		return nil
	}))

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if seenDir == alphaDir {
		t.Fatalf("login for A ran against Alpha's slot %q: name lookup must be exact, never a prefix match (F15)", alphaDir)
	}
	st, _ := s.Load()
	if len(st.Accounts) != 2 {
		t.Errorf("accounts = %d, want 2 (Alpha and a new A)", len(st.Accounts))
	}
}

// TestLoginForANewNameWhoseSlotDirIsARenamedAccountsDirUsesTheNextFreeSlot
// is C1(c)/F173: Bee's slot is accounts/B (Dir never follows a rename, so
// this is exactly the shape `chottag rename B Bee` leaves behind). A login
// for the brand new name "B" must not run inside Bee's own slot — it must
// land on the next free accounts/B-N instead.
func TestLoginForANewNameWhoseSlotDirIsARenamedAccountsDirUsesTheNextFreeSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	beeDir := filepath.Join(home, "accounts", "B")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "Bee", Email: "bee@example.com", Dir: beeDir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"b@example.com","orgName":"Org"}`)
	var seenDir string
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, _, _ io.Writer) error {
		seenDir = slotDir
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	}))

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	wantDir := filepath.Join(home, "accounts", "B-2")
	if seenDir != wantDir {
		t.Fatalf("login ran against %q, want the next free slot %q, never Bee's own %q", seenDir, wantDir, beeDir)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want Bee plus a new B", st.Accounts)
	}
	b, err := st.Find("B")
	if err != nil {
		t.Fatalf("B not registered: %v", err)
	}
	if b.Dir != wantDir {
		t.Fatalf("B.Dir = %q, want %q", b.Dir, wantDir)
	}
	bee, err := st.Find("Bee")
	if err != nil || bee.Dir != beeDir || bee.Email != "bee@example.com" {
		t.Fatalf("Bee = %+v, %v; want it untouched on its own slot", bee, err)
	}
}

// TestLoginForANewNameWhoseSlotDirExistsOnDiskUnregisteredReusesIt pins the
// behaviour C1(c) must NOT change: a slot dir that merely exists on disk,
// with no account registered on it (the shape `adopt` exists for — a slot
// created before chottag had a CLI, or left behind after some other
// process), is used directly, not skipped for accounts/<name>-2.
func TestLoginForANewNameWhoseSlotDirExistsOnDiskUnregisteredReusesIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	dir := filepath.Join(home, "accounts", "C")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"c@example.com","orgName":"Org"}`)
	var seenDir string
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, _, _ io.Writer) error {
		seenDir = slotDir
		return nil
	}))

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "C"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if seenDir != dir {
		t.Fatalf("login ran against %q, want the pre-existing unregistered slot %q reused directly", seenDir, dir)
	}
}

// caseInsensitiveFS reports whether dir's filesystem folds case (macOS's
// default APFS and Windows do; Linux's usual ext4 does not) by statting an
// upper/lower-cased sibling of a file this creates and checking os.SameFile.
// NEW-2 (a login/adopt slot-dir collision on a case-insensitive FS) can
// only be reproduced for real where this is true; elsewhere the test skips
// rather than asserting behaviour the OS itself does not exhibit.
func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	marker := filepath.Join(dir, "chottag-fs-case-probe")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	lower, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := os.Stat(filepath.Join(dir, "CHOTTAG-FS-CASE-PROBE"))
	if err != nil {
		return false
	}
	return os.SameFile(lower, upper)
}

// TestLoginForANewNameCaseOnlyCollisionUsesFileIdentityNotStrings is NEW-2:
// on a case-insensitive filesystem, accounts/B and accounts/b are the same
// directory (one inode). After `chottag rename B Bee` (Dir stays
// accounts/B), a login for the brand new name "b" must not take over
// Bee's own slot just because "accounts/b" != "accounts/B" as STRINGS —
// dirRegistered must compare by file identity, exactly like the
// case-DIFFERENT scenario already covered
// (TestLoginForANewNameWhoseSlotDirIsARenamedAccountsDirUsesTheNextFreeSlot),
// so this lands on the next FREE slot (accounts/b-2, "free" also judged by
// identity) instead.
func TestLoginForANewNameCaseOnlyCollisionUsesFileIdentityNotStrings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	beeDir := filepath.Join(home, "accounts", "B")
	if err := os.MkdirAll(beeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !caseInsensitiveFS(t, filepath.Join(home, "accounts")) {
		t.Skip("this filesystem is case-sensitive; NEW-2 does not apply")
	}

	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "Bee", Email: "bee@example.com", Dir: beeDir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"b@example.com","orgName":"Org"}`)
	var seenDir string
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, _, _ io.Writer) error {
		seenDir = slotDir
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	}))

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "b"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	wantDir := filepath.Join(home, "accounts", "b-2")
	if seenDir != wantDir {
		t.Fatalf("login ran against %q, want the next free slot %q (by identity), never Bee's own slot %q", seenDir, wantDir, beeDir)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want Bee plus a new b", st.Accounts)
	}
	bAcct, err := st.Find("b")
	if err != nil {
		t.Fatalf("b not registered: %v", err)
	}
	if bAcct.Dir != wantDir {
		t.Fatalf("b.Dir = %q, want %q", bAcct.Dir, wantDir)
	}
	bee, err := st.Find("Bee")
	if err != nil || bee.Dir != beeDir || bee.Email != "bee@example.com" {
		t.Fatalf("Bee = %+v, %v; want it untouched on its own slot", bee, err)
	}
}

// TestDirRegisteredDetectsACaseOnlyCollisionEvenWhenBothDirsAreMissing is
// NEW-2's missing-path fallback (re-review Minor), isolated to dirRegistered
// itself: os.SameFile can only prove file identity when both paths exist,
// so when neither does, the fallback must still compare the cleaned paths
// case-INSENSITIVELY, not report "not registered" just because nothing
// could be stat'd. This holds on every OS — it is a name-string fallback,
// not a real filesystem behaviour — so, unlike the sibling tests that
// exercise real case-folding, this one is never skipped.
func TestDirRegisteredDetectsACaseOnlyCollisionEvenWhenBothDirsAreMissing(t *testing.T) {
	home := t.TempDir()
	st := store.Default()
	if err := st.Add(store.Account{Name: "Bee", Dir: filepath.Join(home, "accounts", "B")}); err != nil {
		t.Fatal(err)
	}
	// accounts/B is never created on disk.
	if !dirRegistered(st, filepath.Join(home, "accounts", "b")) {
		t.Fatal("dirRegistered = false, want true: a same-spelled, differently-cased dir must be treated as a collision when file identity cannot be proven either way")
	}
	if dirRegistered(st, filepath.Join(home, "accounts", "b-2")) {
		t.Fatal("dirRegistered = true for an unrelated name, want false")
	}
}

// TestLoginForANewNameCaseOnlyCollisionMissingOnDiskStillDetected is NEW-2's
// missing-path fallback (re-review Minor): file identity (os.SameFile)
// only proves anything when both paths actually exist. A registered
// account's Dir can be missing on disk (its slot removed by hand, or —
// deliberately, here — never created at all, so this test needs no
// case-insensitive filesystem to reproduce), and dirRegistered must still
// treat a same-spelled-but-differently-cased dir as a collision rather
// than silently reporting "not registered" just because it couldn't stat
// either side. This is what runs on EVERY OS, unlike the sibling test
// above, which needs a case-insensitive filesystem to exercise the
// file-identity path at all.
func TestLoginForANewNameCaseOnlyCollisionMissingOnDiskStillDetected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	beeDir := filepath.Join(home, "accounts", "B")
	// Deliberately never created: both sides of the identity check are
	// about to be missing.

	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "Bee", Email: "bee@example.com", Dir: beeDir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"b@example.com","orgName":"Org"}`)
	var seenDir string
	t.Cleanup(SetAuthExecForTest(func(_, slotDir, _ string, _ io.Reader, _, _ io.Writer) error {
		seenDir = slotDir
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	}))

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "b"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	wantDir := filepath.Join(home, "accounts", "b-2")
	if seenDir != wantDir {
		t.Fatalf("login ran against %q, want the next free slot %q (missing-path fallback), never Bee's own (missing-on-disk) slot %q", seenDir, wantDir, beeDir)
	}
}

// Mutation 4: a `claude auth login` that exits 0 but leaves the slot still
// logged out must not be registered.
func TestLoginFailsWhenStillNotLoggedInAfterAuthLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	fake := writeFakeClaude(t, `{"loggedIn":false}`)
	stubAuthExec(t, func(string, string, string) error { return nil })

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogin = %d, want exit.Error", got)
	}
	st, _ := (store.Store{Dir: home}).Load()
	if _, err := st.Find("A"); err == nil {
		t.Error("A was registered despite still reporting no login")
	}
	// Same cleanup obligation as a failing exec: the slot this invocation
	// created must not survive an "auth login exited 0 but still not
	// logged in" outcome either.
	if _, err := os.Stat(filepath.Join(home, "accounts", "A")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir survives login succeeding but still reporting no login: stat err = %v", err)
	}
}

// The dangerous case: an existing, already-live slot (a real credential file
// inside it) must survive a re-login that fails, whether the exec itself
// fails or the exec succeeds but the post-login probe still reports no
// login. Earlier tests only ever exercised a slot THIS invocation created
// (so `created` was always true); this is the one that proves the
// `created &&` half of the cleanup guard, not just the `IsSlotDir` half.
func TestLoginFailingReLoginNeverDeletesAnExistingSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	dir := filepath.Join(home, "accounts", "A")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(dir, ".credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"live":"credential"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Email: "a@example.com", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":false}`)

	assertSlotSurvives := func(t *testing.T) {
		t.Helper()
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("existing slot dir was removed: %v", err)
		}
		if _, err := os.Stat(credPath); err != nil {
			t.Errorf("existing slot's credential file was removed: %v", err)
		}
		st, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Find("A"); err != nil {
			t.Errorf("A was unregistered by a failing re-login: %v", err)
		}
	}

	t.Run("exec fails", func(t *testing.T) {
		t.Cleanup(SetAuthExecForTest(func(string, string, string, io.Reader, io.Writer, io.Writer) error {
			return errors.New("user cancelled")
		}))
		var out, errBuf bytes.Buffer
		if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
			t.Fatalf("runLogin = %d, want exit.Error", got)
		}
		assertSlotSurvives(t)
	})

	t.Run("still not logged in", func(t *testing.T) {
		t.Cleanup(SetAuthExecForTest(func(string, string, string, io.Reader, io.Writer, io.Writer) error {
			return nil
		}))
		var out, errBuf bytes.Buffer
		if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
			t.Fatalf("runLogin = %d, want exit.Error", got)
		}
		assertSlotSurvives(t)
	})
}

// The IsSlotDir half of the same cleanup guard: a directory this invocation
// DID create, but that lives outside <home>/accounts, must also survive a
// failing login — cleanup must never remove anything that isn't a path
// store.Store.SlotDir could have produced.
func TestLoginFailedLoginNeverDeletesADirOutsideTheSlotsTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	outside := filepath.Join(t.TempDir(), "external-slot")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Email: "a@example.com", Dir: outside})
	}); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":false}`)
	stubAuthExec(t, func(string, string, string) error { return errors.New("user cancelled") })

	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogin = %d, want exit.Error", got)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("directory outside <home>/accounts was removed by cleanup: %v", err)
	}
}

// I1: SetAuthExecForTest must restore whatever was installed at the moment
// of the call, not production's real exec — otherwise the FIRST stubbed
// test's cleanup disarms TestMain's panicking default for the rest of this
// binary, and every later test that reaches claudeAuthExec without stubbing
// it runs the real, stdio-inheriting exec instead of panicking.
func TestAuthExecSeamStillPanicsAfterAStubbedTestRestores(t *testing.T) {
	// A nonexistent bin, never "claude": if the mutation this test is
	// designed to catch is live, this call reaches the real,
	// stdio-inheriting runClaudeAuth instead of panicking, and it must fail
	// fast with an exec error rather than actually starting some binary
	// found on $PATH and blocking on a browser prompt.
	noSuchBin := filepath.Join(t.TempDir(), "no-such-claude-binary")

	t.Run("stubbed", func(t *testing.T) {
		restore := SetAuthExecForTest(func(string, string, string, io.Reader, io.Writer, io.Writer) error {
			return nil
		})
		t.Cleanup(restore)
		if err := claudeAuthExec(noSuchBin, t.TempDir(), "login", strings.NewReader(""), io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	})

	defer func() {
		if recover() == nil {
			t.Error("claudeAuthExec did not panic after the stubbed subtest's cleanup ran: the seam fell back to the real exec instead of TestMain's panicking default")
		}
	}()
	claudeAuthExec(noSuchBin, t.TempDir(), "login", strings.NewReader(""), io.Discard, io.Discard)
}
