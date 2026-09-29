package store_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func acct(name, email string) store.Account {
	return store.Account{Name: name, Email: email, Dir: "/slots/" + name}
}

func TestLoadMissingIsDefault(t *testing.T) {
	st, err := store.Store{Dir: t.TempDir()}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != store.Version || st.Port != store.DefaultPort || st.Auto != nil || !st.AutoOn() {
		t.Fatalf("default state = %+v", st)
	}
}

func TestResolvedPortIsStateOrDefaultAndNeverSearches(t *testing.T) {
	cases := []struct {
		name string
		port int
		want int
	}{
		{"zero means the default", 0, store.DefaultPort},
		{"an explicit port is used verbatim", 51000, 51000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (store.State{Port: c.port}).ResolvedPort(); got != c.want {
				t.Errorf("ResolvedPort() for Port=%d = %d, want %d", c.port, got, c.want)
			}
		})
	}
}

func TestUpdatePersistsWith0600(t *testing.T) {
	s := store.Store{Dir: filepath.Join(t.TempDir(), "chottag")}
	if _, err := s.Update(func(st *store.State) error { return st.Add(acct("B", "b@example.com")) }); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 1 || st.Serving != "B" || st.Remote != "B" {
		t.Fatalf("state = %+v", st)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "state.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json perm = %v err = %v", fi.Mode().Perm(), err)
	}
	di, err := os.Stat(s.Dir)
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %v err = %v", di.Mode().Perm(), err)
	}
}

func TestUpdateErrorWritesNothing(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	boom := errors.New("boom")
	if _, err := s.Update(func(st *store.State) error { st.Port = 1; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("state.json written despite error: %v", err)
	}
}

func TestConcurrentUpdatesAllLand(t *testing.T) {
	s := store.Store{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.Update(func(st *store.State) error { return st.Add(acct(fmt.Sprintf("a%d", i), "")) }); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	st, err := s.Load()
	if err != nil || len(st.Accounts) != 16 {
		t.Fatalf("accounts = %d err = %v", len(st.Accounts), err)
	}
}

func TestLoadRejectsNewerVersionAndCorruptFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	os.WriteFile(p, []byte(`{"version": 99}`), 0o600)
	if _, err := (store.Store{Dir: dir}).Load(); err == nil {
		t.Fatal("newer version accepted")
	}
	os.WriteFile(p, []byte(`{not json`), 0o600)
	if _, err := (store.Store{Dir: dir}).Load(); err == nil {
		t.Fatal("corrupt file accepted")
	}
}

func TestFind(t *testing.T) {
	st := store.Default()
	for _, a := range []store.Account{acct("AccountA", "a@example.com"), acct("AccountB", "b@example.com"), acct("Work", "w@example.com")} {
		if err := st.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	for q, want := range map[string]string{
		"accountb":      "AccountB", // exact name, case-insensitive
		"A@Example.com": "AccountA", // email
		"wo":            "Work",     // unique prefix
	} {
		a, err := st.Find(q)
		if err != nil || a.Name != want {
			t.Errorf("Find(%q) = %v, %v; want %s", q, a, err, want)
		}
	}
	if _, err := st.Find("acc"); !errors.Is(err, store.ErrAmbiguous) {
		t.Errorf("Find(acc) err = %v, want ErrAmbiguous", err)
	}
	if _, err := st.Find("zzz"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Find(zzz) err = %v, want ErrNotFound", err)
	}
	if _, err := st.Find(" "); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Find(blank) err = %v, want ErrNotFound", err)
	}
}

func TestFindAmbiguousEmail(t *testing.T) {
	st := store.Default()
	for _, a := range []store.Account{
		acct("Acme", "alice@example.com"),
		acct("AcmeOrg", "alice@example.com"),
		acct("Solo", "solo@example.com"),
	} {
		if err := st.Add(a); err != nil {
			t.Fatal(err)
		}
	}

	_, err := st.Find("alice@example.com")
	if !errors.Is(err, store.ErrAmbiguous) {
		t.Fatalf("Find(shared email) err = %v, want ErrAmbiguous", err)
	}
	if !strings.Contains(err.Error(), "Acme") || !strings.Contains(err.Error(), "AcmeOrg") {
		t.Errorf("Find(shared email) err = %q, want it to name both Acme and AcmeOrg", err)
	}

	// A single match on the email still resolves (no regression).
	a, err := st.Find("solo@example.com")
	if err != nil || a.Name != "Solo" {
		t.Fatalf("Find(unique email) = %v, %v; want Solo", a, err)
	}
}

func TestFindExactNameBeatsAmbiguousEmailMatch(t *testing.T) {
	// An exact name match wins before the email arm even runs, so a query
	// that is both another account's exact name and one of several
	// accounts' shared email still resolves unambiguously by name.
	st := store.Default()
	for _, a := range []store.Account{
		acct("shared", ""),
		acct("other-a", "shared"),
		acct("other-b", "shared"),
	} {
		if err := st.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	a, err := st.Find("shared")
	if err != nil || a.Name != "shared" {
		t.Fatalf("Find(shared) = %v, %v; want exact name match \"shared\"", a, err)
	}
}

func TestNextWrapsInRegistrationOrder(t *testing.T) {
	st := store.Default()
	if _, err := st.Next(""); !errors.Is(err, store.ErrNoAccounts) {
		t.Fatalf("empty Next err = %v", err)
	}
	for _, n := range []string{"A", "B", "C"} {
		st.Add(acct(n, ""))
	}
	for cur, want := range map[string]string{"A": "B", "b": "C", "C": "A", "": "A", "gone": "A"} {
		a, err := st.Next(cur)
		if err != nil || a.Name != want {
			t.Errorf("Next(%q) = %s, %v; want %s", cur, a.Name, err, want)
		}
	}
}

func TestAddValidatesAndKeepsRoles(t *testing.T) {
	st := store.Default()
	for _, bad := range []string{"", ".hidden", "../x", "a/b", "has space", "x234567890123456789012345678901234"} {
		if err := st.Add(acct(bad, "")); err == nil {
			t.Errorf("Add(%q) accepted", bad)
		}
	}
	if err := st.Add(store.Account{Name: "NoDir"}); err == nil {
		t.Error("Add without Dir accepted")
	}
	st.Add(acct("B", ""))
	if err := st.Add(acct("b", "")); !errors.Is(err, store.ErrExists) {
		t.Errorf("duplicate err = %v", err)
	}
	st.Add(acct("C", ""))
	if st.Serving != "B" || st.Remote != "B" {
		t.Fatalf("roles moved on second Add: %+v", st)
	}
}

func TestAddRefusesADuplicateDir(t *testing.T) {
	st := store.Default()
	if err := st.Add(store.Account{Name: "B", Dir: "/slots/B"}); err != nil {
		t.Fatal(err)
	}
	err := st.Add(store.Account{Name: "Bee", Dir: "/slots/B"})
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if !strings.Contains(err.Error(), "B") {
		t.Errorf("err = %v, want it to name the account that owns the dir (B)", err)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want the duplicate refused", st.Accounts)
	}

	// filepath.Clean: an unclean spelling of the same dir is still caught.
	err = st.Add(store.Account{Name: "Bee2", Dir: "/slots/./B/"})
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("uncleaned dup err = %v, want ErrExists", err)
	}
}

// TestAddRefusesADuplicateDirByFileIdentityOnACaseInsensitiveFS is NEW-2: a
// cleaned STRING compare alone misses a case-insensitive filesystem's
// accounts/B and accounts/b, which are one inode (macOS's default APFS,
// Windows). Two differently-cased paths to the same real directory must
// still be refused. Elsewhere (a case-sensitive filesystem, e.g. Linux's
// usual ext4), they are genuinely two different directories and the test
// skips rather than asserting behaviour the OS does not exhibit.
func TestAddRefusesADuplicateDirByFileIdentityOnACaseInsensitiveFS(t *testing.T) {
	dir := t.TempDir()
	lower := filepath.Join(dir, "b")
	if err := os.Mkdir(lower, 0o700); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(dir, "B")
	li, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}
	ui, err := os.Stat(upper)
	if err != nil || !os.SameFile(li, ui) {
		t.Skip("this filesystem is case-sensitive; NEW-2 does not apply")
	}

	st := store.Default()
	if err := st.Add(store.Account{Name: "Bee", Dir: upper}); err != nil {
		t.Fatal(err)
	}
	if err := st.Add(store.Account{Name: "b", Dir: lower}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("err = %v, want ErrExists: %s and %s are the same real directory", err, upper, lower)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want the case-only duplicate refused", st.Accounts)
	}
}

// TestAddRefusesACaseOnlyDuplicateDirMissingOnDisk is NEW-2's missing-path
// fallback (re-review Minor): file identity (os.SameFile) only proves
// anything when both paths exist. Neither accounts/B nor accounts/b exists
// here, so the fallback must compare the cleaned paths case-INSENSITIVELY
// instead of treating "could not stat either side" as "not the same dir".
// This holds on every OS (it is a name-string fallback, not real
// filesystem behaviour), so it is never skipped.
func TestAddRefusesACaseOnlyDuplicateDirMissingOnDisk(t *testing.T) {
	st := store.Default()
	if err := st.Add(store.Account{Name: "Bee", Dir: "/nonexistent/accounts/B"}); err != nil {
		t.Fatal(err)
	}
	err := st.Add(store.Account{Name: "b", Dir: "/nonexistent/accounts/b"})
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want the case-only duplicate refused", st.Accounts)
	}
	// An unrelated, differently-named dir is still accepted.
	if err := st.Add(store.Account{Name: "C", Dir: "/nonexistent/accounts/C"}); err != nil {
		t.Fatalf("unrelated dir rejected: %v", err)
	}
}

func TestRemoveRefusesRoleHolder(t *testing.T) {
	st := store.Default()
	st.Add(acct("B", ""))
	st.Add(acct("C", ""))
	if err := st.Remove("B"); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("Remove(serving) err = %v", err)
	}
	if err := st.Remove("C"); err != nil {
		t.Fatal(err)
	}
	if err := st.Remove("C"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Remove(missing) err = %v", err)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
}

func TestSlotDir(t *testing.T) {
	s := store.Store{Dir: "/h/.chottag"}
	got, err := s.SlotDir("B")
	if err != nil || got != "/h/.chottag/accounts/B" {
		t.Fatalf("SlotDir = %s, %v", got, err)
	}
	if _, err := s.SlotDir("../x"); err == nil {
		t.Fatal("SlotDir(\"../x\") accepted an invalid name")
	}
}

func TestAddRejectsRelativeDir(t *testing.T) {
	st := store.Default()
	if err := st.Add(store.Account{Name: "B", Dir: "slots/B"}); err == nil {
		t.Fatal("Add with a relative Dir accepted")
	}
	if len(st.Accounts) != 0 {
		t.Fatalf("relative Dir was registered: %+v", st.Accounts)
	}
}

func TestAccountRotatesByDefault(t *testing.T) {
	a := acct("B", "")
	if !a.Rotates() {
		t.Fatal("a new account must rotate by default")
	}
	a.NoRotate = true
	if a.Rotates() {
		t.Fatal("NoRotate account must not rotate")
	}
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(st *store.State) error { return st.Add(a) }); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil || st.Accounts[0].Rotates() {
		t.Fatalf("NoRotate did not round-trip: %+v err=%v", st.Accounts, err)
	}
}

func TestIsSlotDir(t *testing.T) {
	s := store.Store{Dir: "/h/.chottag"}
	cases := []struct {
		dir  string
		want bool
	}{
		{"/h/.chottag/accounts/B", true},
		{"/h/.chottag/accounts", false},
		{"/h/.chottag/accounts/B/sub", false},
		{"/h/.chottag/accounts/../x", false},
		{"/Users/x/.claude", false},
		{"accounts/B", false},
		{"/h/.chottag/accounts/.hidden", false},
	}
	for _, c := range cases {
		if got := s.IsSlotDir(c.dir); got != c.want {
			t.Errorf("IsSlotDir(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}
