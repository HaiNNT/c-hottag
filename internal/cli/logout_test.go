package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func stubCredsDelete(t *testing.T, err error) *[]string {
	t.Helper()
	var deleted []string
	t.Cleanup(SetCredsDeleteForTest(func(slotDir string) error {
		deleted = append(deleted, slotDir)
		return err
	}))
	return &deleted
}

func seedLoggedInAccounts(t *testing.T) (home string, s store.Store) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s = store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: n + "@example.com", Dir: dir}); err != nil {
				return err
			}
		}
		st.Serving, st.Remote = "A", "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return home, s
}

func TestLogoutRevokesDeletesTheCredentialAndTheSlot(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "logout ") {
		t.Errorf("auth calls = %v, want one logout", *calls)
	}
	dir := filepath.Join(home, "accounts", "B")
	if len(*deleted) != 1 || (*deleted)[0] != dir {
		t.Errorf("creds deleted = %v, want [%s]", *deleted, dir)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir survives logout: stat err = %v", err)
	}
	st, _ := s.Load()
	if _, err := st.Find("B"); err == nil {
		t.Error("B is still registered after logout")
	}
}

// The ordering rule: a failed revoke must delete nothing, because a local
// delete while the server-side token stays live is the one outcome that
// re-running cannot fix.
func TestLogoutDeletesNothingWhenTheRevokeFails(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return errors.New("network down") })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogout = %d, want exit.Error", got)
	}
	if len(*deleted) != 0 {
		t.Errorf("creds deleted = %v, want none: a failed revoke must delete nothing", *deleted)
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "B")); err != nil {
		t.Errorf("slot dir was removed despite the revoke failing: %v", err)
	}
	st, _ := s.Load()
	if _, err := st.Find("B"); err != nil {
		t.Error("B was deregistered despite the revoke failing")
	}
}

func TestLogoutRefusesTheServingAccountWithoutForce(t *testing.T) {
	_, s := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.UserAction {
		t.Fatalf("runLogout = %d, want exit.UserAction", got)
	}
	if len(*deleted) != 0 {
		t.Errorf("creds deleted = %v, want none", *deleted)
	}
	st, _ := s.Load()
	if _, err := st.Find("A"); err != nil {
		t.Error("A was removed despite the refusal")
	}
}

func TestLogoutForceMovesTheRolesFirst(t *testing.T) {
	_, s := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d; stderr=%q", got, errBuf.String())
	}
	st, _ := s.Load()
	if st.Serving != "B" || st.Remote != "B" {
		t.Errorf("serving=%q remote=%q, want both moved to B", st.Serving, st.Remote)
	}
	if _, err := st.Find("A"); err == nil {
		t.Error("A is still registered")
	}
}

// Without --yes, a declined confirmation changes nothing.
func TestLogoutAbortsOnADeclinedConfirmation(t *testing.T) {
	interactiveStdin(t)
	home, s := seedLoggedInAccounts(t)
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"B"}, strings.NewReader("n\n"), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogout = %d, want exit.Error", got)
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("calls=%v deleted=%v, want nothing done", *calls, *deleted)
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "B")); err != nil {
		t.Errorf("slot removed after declining: %v", err)
	}
	st, _ := s.Load()
	if _, err := st.Find("B"); err != nil {
		t.Error("B deregistered after declining")
	}
}

// The RemoveAll is guarded: a slot dir outside <home>/accounts is never
// removed, whatever state.json says.
func TestLogoutRefusesToRemoveADirOutsideAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	outside := t.TempDir()
	keep := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(keep, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: outside})
	}); err != nil {
		t.Fatal(err)
	}
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf))
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("logout removed a directory outside <home>/accounts: %v", err)
	}
}

// Fix round 1, I1: name can hold ONLY Remote while a different account
// serves. B, A are the only two accounts, so the account after B in
// rotation order always wraps around to A — nextCandidate walking forward
// from Serving would land on A itself, and the EqualFold guard would
// (correctly) refuse it, leaving Remote empty even though the still-serving
// account B was sitting right there as a perfectly good replacement.
func TestLogoutForceMovesRemoteOffWithoutDisturbingAnUninvolvedServingAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: n + "@example.com", Dir: dir}); err != nil {
				return err
			}
		}
		st.Serving, st.Remote = "B", "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "B" {
		t.Errorf("serving = %q, want B untouched", st.Serving)
	}
	if st.Remote != "B" {
		t.Errorf("remote = %q, want B (the still-serving account), not emptied by nextCandidate wrapping back to A", st.Remote)
	}
	if _, err := st.Find("A"); err == nil {
		t.Error("A is still registered")
	}
}

// The same trap, reproduced with three accounts registered in an order
// where the departing account A immediately follows Serving (B) in
// rotation — the exact positional condition that made the old
// nextCandidate-only logic return the departing account itself.
func TestLogoutForceMovesRemoteOffWhenNameImmediatelyFollowsServingAmongThreeAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"B", "A", "C"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: n + "@example.com", Dir: dir}); err != nil {
				return err
			}
		}
		st.Serving, st.Remote = "B", "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "B" {
		t.Errorf("serving = %q, want B untouched", st.Serving)
	}
	if st.Remote != "B" {
		t.Errorf("remote = %q, want B (the still-serving account), not emptied by nextCandidate wrapping back to A", st.Remote)
	}
	if _, err := st.Find("A"); err == nil {
		t.Error("A is still registered")
	}
}

// Fix round 1, I2: a failed local credential delete must leave the slot and
// the registration intact. The revoke already succeeded, so re-running
// `logout --force` is the only thing that needs to happen next — silently
// having already removed the slot or the registration would make that
// re-run impossible to reason about.
func TestLogoutKeepsTheSlotWhenTheCredentialDeleteFails(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, errors.New("keychain busy"))

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogout = %d, want exit.Error; stderr=%q", got, errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "B")); err != nil {
		t.Errorf("slot dir was removed despite the credential delete failing: %v", err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Find("B"); err != nil {
		t.Error("B was deregistered despite the credential delete failing")
	}
	// M6: the message must point at --force, not a plain re-run, since the
	// revoke step is already done.
	if !strings.Contains(errBuf.String(), "--force") || !strings.Contains(errBuf.String(), "already revoked") {
		t.Errorf("stderr = %q, want it to say the login was already revoked and point at --force to finish local cleanup", errBuf.String())
	}
}

// Fix round 1, I3: spec §5 says --force moves the role to the next account
// FIRST. That move must be persisted in its own Update BEFORE the revoke
// even runs, so if the credential delete later fails and this command
// exits, the account is already demoted — not left serving/remote with a
// login that no longer works — while it is still registered (the final
// Remove never ran).
func TestLogoutForceMovesRolesBeforeAttemptingTheRevokeEvenIfTheDeleteLaterFails(t *testing.T) {
	_, s := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, errors.New("keychain busy"))

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogout = %d, want exit.Error; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "B" || st.Remote != "B" {
		t.Errorf("serving=%q remote=%q, want both already moved to B despite the later delete failure", st.Serving, st.Remote)
	}
	if _, err := st.Find("A"); err != nil {
		t.Error("A was deregistered despite the credential delete failing: only the role move should have persisted")
	}
}

// F2: the help text shows `logout NAME [--force] [--yes]`, name first — that
// order must work too. Go's flag package stops at the first non-flag token,
// so a bare fs.Parse would leave --force/--yes sitting unparsed and this
// call failing exit.Usage instead of logging B out.
func TestLogoutAcceptsFlagsAfterTheName(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"B", "--force", "--yes"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d, want exit.OK; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 1 {
		t.Errorf("auth calls = %v, want one logout", *calls)
	}
	if len(*deleted) != 1 {
		t.Errorf("creds deleted = %v, want one delete", *deleted)
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "B")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir survives logout: stat err = %v", err)
	}
	st, _ := s.Load()
	if _, err := st.Find("B"); err == nil {
		t.Error("B is still registered after logout")
	}
}

// F1: the slot's flock is taken after the confirmation prompt and held
// through the revoke/delete/RemoveAll — the same lock `chottag login` holds
// for its own whole flow. Holding it from the test proves runLogout actually
// waits for it rather than skipping straight past.
func TestLogoutWaitsForTheSlotLock(t *testing.T) {
	home, _ := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	dir := filepath.Join(home, "accounts", "B")
	unlock, err := fsutil.Lock(creds.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf))
	}()

	select {
	case <-done:
		t.Fatal("runLogout returned before the slot lock was released: it did not wait for it")
	case <-time.After(300 * time.Millisecond):
	}

	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got != exit.OK {
			t.Errorf("runLogout = %d after the lock was released, want exit.OK", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLogout never returned after the slot lock was released")
	}
}

// mutatingReader mutates the store on its FIRST Read then returns line,
// standing in for a concurrent `chottag tag`/`chottag remote` that fires
// while the operator is answering logout's [y/N] prompt (F3): the first Read
// is where logout blocks waiting for stdin, so that is the moment to slip
// the state.json edit in, before the confirmation itself is even returned.
type mutatingReader struct {
	mutate func()
	done   bool
	r      io.Reader
}

func (m *mutatingReader) Read(p []byte) (int, error) {
	if !m.done {
		m.done = true
		m.mutate()
	}
	return m.r.Read(p)
}

// F3: B does not hold a role at the pre-prompt check, but picks one up (a
// concurrent `chottag tag B`) while the operator is still answering the
// confirmation prompt. The post-prompt re-check must catch this on FRESH
// state and refuse exactly like the pre-prompt check would have, deleting
// nothing.
func TestLogoutRechecksRolesAfterThePromptOnFreshState(t *testing.T) {
	interactiveStdin(t)
	home, s := seedLoggedInAccounts(t)
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	stdin := &mutatingReader{
		r: strings.NewReader("y\n"),
		mutate: func() {
			if _, err := s.Update(func(st *store.State) error {
				st.Serving = "B"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
	}

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"B"}, stdin, newReporter(false, &out, &errBuf)); got != exit.UserAction {
		t.Fatalf("runLogout = %d, want exit.UserAction; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("calls=%v deleted=%v, want nothing done once B picked up a role mid-prompt", *calls, *deleted)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Find("B"); err != nil {
		t.Error("B was deregistered despite holding a role by the time of the re-check")
	}
	if _, err := os.Stat(filepath.Join(home, "accounts", "B")); err != nil {
		t.Errorf("slot dir removed despite the refusal: %v", err)
	}
}

// F4: an account whose Dir is genuinely outside chottag's accounts tree is
// refused without --force, exactly like the holds-a-role case — no revoke,
// no credential delete, still registered.
func TestLogoutRefusesAnOutOfTreeDirWithoutForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	outside := t.TempDir()
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Dir: outside})
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.UserAction {
		t.Fatalf("runLogout = %d, want exit.UserAction; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("calls=%v deleted=%v, want nothing done for an out-of-tree dir", *calls, *deleted)
	}
	st, _ := s.Load()
	if _, err := st.Find("A"); err != nil {
		t.Error("A was deregistered despite the refusal")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("outside dir was removed: %v", err)
	}
}

// F4: --force on an out-of-tree account deregisters it ONLY — no revoke, no
// credential delete, no RemoveAll — leaving the login and directory in
// place, exactly as the message says.
func TestLogoutForceDeregistersAnOutOfTreeDirWithoutTouchingIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	outside := t.TempDir()
	keep := filepath.Join(outside, "important.txt")
	if err := os.WriteFile(keep, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Dir: outside})
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d, want exit.OK; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("calls=%v deleted=%v, want no revoke/delete for an out-of-tree dir even with --force", *calls, *deleted)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("out-of-tree dir was touched: %v", err)
	}
	st, _ := s.Load()
	if _, err := st.Find("A"); err == nil {
		t.Error("A is still registered despite --force")
	}
	if !strings.Contains(out.String(), "left in place") {
		t.Errorf("stdout = %q, want it to say the login and directory were left in place", out.String())
	}
}

// F4: a symlinked slot (accounts/X -> elsewhere) is treated as out-of-tree —
// IsSlotDir alone is a path-string check and does not see the symlink.
func TestLogoutTreatsASymlinkedSlotAsOutOfTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	real := t.TempDir()
	dir := filepath.Join(home, "accounts", "A")
	if err := os.Symlink(real, dir); err != nil {
		t.Fatal(err)
	}
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		// B first, so it (not A) picks up the automatic Serving/Remote
		// roles a first Add always gets — this test is about the symlink
		// check specifically, not the holds-a-role refusal.
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.UserAction {
		t.Fatalf("runLogout = %d, want exit.UserAction; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("calls=%v deleted=%v, want nothing done for a symlinked slot", *calls, *deleted)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("symlink was removed: %v", err)
	}
}

// F4/P9: a slot reached through a path ALIAS of <h>/accounts (here, a
// symlinked $CHOTTAG_HOME) is genuinely in-tree, and logout must remove it —
// the old s.IsSlotDir-only guard treated it as outside the tree by path
// string and silently left it (and its credential) behind.
func TestLogoutRemovesAnAliasedSlotDir(t *testing.T) {
	realHome := t.TempDir()
	aliasParent := t.TempDir()
	aliasHome := filepath.Join(aliasParent, "alias")
	if err := os.Symlink(realHome, aliasHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHOTTAG_HOME", realHome)
	realDir := filepath.Join(realHome, "accounts", "A")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasedDir := filepath.Join(aliasHome, "accounts", "A")
	s := store.Store{Dir: realHome}
	if _, err := s.Update(func(st *store.State) error {
		// B first, so it (not A) picks up the automatic Serving/Remote
		// roles a first Add always gets — this test is about the alias
		// resolution, not about --force.
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(realHome, "accounts", "B")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Dir: aliasedDir})
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d, want exit.OK; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 1 {
		t.Errorf("auth calls = %v, want one logout", *calls)
	}
	if len(*deleted) != 1 || (*deleted)[0] != aliasedDir {
		t.Errorf("creds deleted = %v, want [%s] (the alias spelling, F133)", *deleted, aliasedDir)
	}
	if _, err := os.Stat(realDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("aliased slot dir survives logout: stat err = %v (P9)", err)
	}
}

// F6: Remote has no rotation requirement (spec §5) — an account excluded
// from rotation still works as remote — so if nextCandidate finds nobody
// eligible to SERVE (every other account is excluded from rotation),
// Remote must still fall back to the other account rather than going empty
// too.
func TestLogoutForceFallsBackToTheOtherAccountForRemoteWhenNoCandidateIsEligibleToServe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: n + "@example.com", Dir: dir}); err != nil {
				return err
			}
		}
		// A becomes Serving and Remote automatically (first Add). B is
		// excluded from rotation, so nextCandidate finds nobody eligible to
		// serve once A departs.
		st.Accounts[1].NoRotate = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "" {
		t.Errorf("serving = %q, want empty: B is excluded from rotation and nothing else is eligible", st.Serving)
	}
	if st.Remote != "B" {
		t.Errorf("remote = %q, want B: an excluded account still works as remote (spec §5, F6)", st.Remote)
	}
}

// R1 (residual fix): fsutil.Lock opens <dir>/.chottag.lock with O_CREATE,
// which needs the slot DIR itself to already exist — it fails with
// os.ErrNotExist if the dir was removed by hand, or by an earlier logout
// that finished RemoveAll but whose own final deregistering Update then
// failed. logout must proceed without the lock in that case (there is
// nothing left to protect), not exit 1 and leave the account permanently
// stuck. No role held, so no --force needed.
func TestLogoutProceedsWhenTheSlotDirIsAlreadyGone(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	dir := filepath.Join(home, "accounts", "B")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d, want exit.OK; stderr=%q", got, errBuf.String())
	}
	if len(*calls) != 1 || len(*deleted) != 1 {
		t.Errorf("calls=%v deleted=%v, want one revoke and one delete: both are idempotent and must still run", *calls, *deleted)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Find("B"); err == nil {
		t.Error("B is still registered")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir was recreated just to lock it: stat err = %v", err)
	}
}

// R1, --force variant: the same missing-dir scenario, but for a role holder
// that needs --force to log out at all.
func TestLogoutForceProceedsWhenTheSlotDirIsAlreadyGone(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)

	dir := filepath.Join(home, "accounts", "A")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "--force", "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d, want exit.OK; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Find("A"); err == nil {
		t.Error("A is still registered")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slot dir was recreated just to lock it: stat err = %v", err)
	}
}

// R2 (residual fix): the F3 re-check must run AFTER the slot lock is
// granted, not before it — otherwise the window between the (now-stale)
// pre-lock check and the lock actually being granted, which can block for
// as long as another holder needs it, is exactly when a concurrent
// `chottag tag B` can grant B a role the check already passed. Simulated by
// holding the lock externally, starting logout (which must block waiting
// for it), mutating state.json to make B serving while it waits, then
// releasing the lock: logout must re-check on the fresh state once it gets
// the lock and refuse — not barrel through to a revoke/delete/RemoveAll it
// can't finish (the final Remove would be refused by store.State.Remove's
// own role guard).
func TestLogoutRechecksRolesAfterAcquiringTheSlotLock(t *testing.T) {
	home, s := seedLoggedInAccounts(t)
	calls := stubAuthExec(t, func(string, string, string) error { return nil })
	deleted := stubCredsDelete(t, nil)

	dir := filepath.Join(home, "accounts", "B")
	unlock, err := fsutil.Lock(creds.LockPath(dir))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)
	go func() {
		var out, errBuf bytes.Buffer
		done <- runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf))
	}()

	select {
	case <-done:
		t.Fatal("runLogout returned before the slot lock was released: it did not wait for it")
	case <-time.After(300 * time.Millisecond):
	}

	// Simulate a concurrent `chottag tag B` landing while logout is still
	// blocked waiting for the lock.
	if _, err := s.Update(func(st *store.State) error {
		st.Serving = "B"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-done:
		if got != exit.UserAction {
			t.Errorf("runLogout = %d, want exit.UserAction: the F3 re-check must run AFTER the lock is granted and see B now holding a role", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLogout never returned after the slot lock was released")
	}
	if len(*calls) != 0 || len(*deleted) != 0 {
		t.Errorf("calls=%v deleted=%v, want nothing done once B picked up a role while logout waited for the lock", *calls, *deleted)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Find("B"); err != nil {
		t.Error("B was deregistered despite holding a role by the time of the re-check")
	}
}

// M4: a small table of cases not otherwise covered above.
func TestLogoutTableCases(t *testing.T) {
	t.Run("force proceeds and deletes despite a failing revoke", func(t *testing.T) {
		_, s := seedLoggedInAccounts(t)
		stubAuthExec(t, func(string, string, string) error { return errors.New("network down") })
		deleted := stubCredsDelete(t, nil)

		var out, errBuf bytes.Buffer
		if got := runLogout([]string{"--yes", "--force", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
			t.Fatalf("runLogout = %d, want exit.OK; stderr=%q", got, errBuf.String())
		}
		if len(*deleted) != 1 {
			t.Errorf("creds deleted = %v, want exactly one delete despite the failing revoke", *deleted)
		}
		if !strings.Contains(errBuf.String(), "warning") {
			t.Errorf("stderr = %q, want a warning that the revoke failed", errBuf.String())
		}
		st, err := s.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Find("B"); err == nil {
			t.Error("B is still registered despite --force completing the logout")
		}
	})

	t.Run("unknown name", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CHOTTAG_HOME", home)

		var out, errBuf bytes.Buffer
		if got := runLogout([]string{"--yes", "nosuchaccount"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Error {
			t.Fatalf("runLogout = %d, want exit.Error; stderr=%q", got, errBuf.String())
		}
	})

	t.Run("wrong arg count", func(t *testing.T) {
		var out, errBuf bytes.Buffer
		if got := runLogout(nil, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.Usage {
			t.Fatalf("runLogout = %d, want exit.Usage; stderr=%q", got, errBuf.String())
		}
	})
}

// I1: SetCredsDeleteForTest must restore whatever was installed at the
// moment of the call, not production's real delete — otherwise the FIRST
// stubbed test's cleanup disarms TestMain's panicking default for the rest
// of this binary, and every later test that reaches credsDelete without
// stubbing it runs the real one against the user's Keychain instead of
// panicking (mirrors TestAuthExecSeamStillPanicsAfterAStubbedTestRestores
// in login_test.go, F130).
func TestCredsDeleteSeamStillPanicsAfterAStubbedTestRestores(t *testing.T) {
	t.Run("stubbed", func(t *testing.T) {
		restore := SetCredsDeleteForTest(func(string) error { return nil })
		t.Cleanup(restore)
		if err := credsDelete(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})

	defer func() {
		if recover() == nil {
			t.Error("credsDelete did not panic after the stubbed subtest's cleanup ran: the seam fell back to the real delete instead of TestMain's panicking default")
		}
	}()
	credsDelete(t.TempDir())
}
