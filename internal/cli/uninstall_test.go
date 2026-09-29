package cli

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestUninstallRestoresTheShellAndRemovesTheShim(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(userHome, ".zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	if code := runUninstall(nil, strings.NewReader(""), newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}

	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("rc = %q, want the pre-setup original %q: §6.1 invariant 5", got, original)
	}
	if _, err := os.Lstat(filepath.Join(home, "bin", "claude")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("bin/claude still exists after uninstall")
	}
	// Accounts survive without --purge.
	if _, err := os.Stat(filepath.Join(home, "accounts")); err != nil {
		t.Errorf("uninstall without --purge removed accounts/: %v", err)
	}
}

// TestSetupThenUninstallWithChottagHomeSetRestoresRCByteIdentical pins fix
// round item 9's other half: `uninstall` needs no change of its own —
// removeRCBlock already strips whatever fenced block writeRCBlock wrote,
// CHOTTAG_HOME export line included — but that must actually hold when
// CHOTTAG_HOME was set at setup time, not merely when it was not.
func TestSetupThenUninstallWithChottagHomeSetRestoresRCByteIdentical(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(userHome, ".zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	afterSetup, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterSetup), `export CHOTTAG_HOME="`+home+`"`+"\n") {
		t.Fatalf("rc after setup = %q, want an export CHOTTAG_HOME line for %q (otherwise this test proves nothing about item 9)", afterSetup, home)
	}

	if code := runUninstall(nil, strings.NewReader(""), newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("rc after uninstall = %q, want the pre-setup original %q byte-for-byte: §6.1 invariant 5", got, original)
	}
}

// Fix round 4, item 1: `uninstall` used a bare fs.Parse, which stops at the
// first non-flag token and leaves everything after it — --purge included —
// sitting unparsed in fs.Args() without erroring. `uninstall stray` must be
// refused as a usage error, not silently run as a no-op uninstall (F2/§5.3).
func TestUninstallRejectsAStrayPositional(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	rc := filepath.Join(userHome, ".zshrc")
	before, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	var errBuf bytes.Buffer
	code := runUninstall([]string{"stray"}, strings.NewReader(""), newReporter(false, io.Discard, &errBuf))
	if code != exit.Usage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Usage, errBuf.String())
	}
	after, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("rc changed despite the stray positional being refused")
	}
	if _, err := os.Lstat(filepath.Join(home, "bin", "claude")); err != nil {
		t.Errorf("bin/claude is gone despite the stray positional being refused: %v", err)
	}
}

// The exact shape of the review's repro: a stray positional AHEAD of
// --purge must not silently drop --purge while the ordinary removal steps
// still run (the bug's "ok:true, purged:false, exit 0"). Both are refused
// together, before anything is touched.
func TestUninstallRejectsAStrayPositionalBeforePurge(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	rc := filepath.Join(userHome, ".zshrc")
	before, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}

	var errBuf bytes.Buffer
	code := runUninstall([]string{"stray", "--purge"}, strings.NewReader("purge\n"), newReporter(false, io.Discard, &errBuf))
	if code != exit.Usage {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Usage, errBuf.String())
	}
	after, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("rc changed despite the stray positional being refused")
	}
	if _, err := os.Lstat(filepath.Join(home, "bin", "claude")); err != nil {
		t.Errorf("bin/claude is gone despite the stray positional being refused: %v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("home was removed despite the stray positional being refused: %v", err)
	}
}

// --purge destroys every slot login, so it must not proceed on a bare yes.
func TestPurgeRequiresExplicitConfirmation(t *testing.T) {
	interactiveStdin(t)
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}

	var stderr bytes.Buffer
	code := runUninstall([]string{"--purge"}, strings.NewReader("y\n"), newReporter(false, io.Discard, &stderr))

	if code != exit.Error {
		t.Errorf("--purge proceeded on \"y\": it destroys every slot login and must require the explicit word (code = %d, stderr = %q)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "confirmation did not match") {
		t.Errorf("stderr = %q, want it to say the confirmation did not match", stderr.String())
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("--purge deleted %s despite unconfirmed input: %v", home, err)
	}
}

// TestUninstallResolvesRCSymlink pins that the rc file coming back
// byte-for-byte holds even when the rc path is a symlink into another
// directory (e.g. a dotfiles repo) — the same hazard Task 6 hit with
// writeRCBlock (F121/F122).
func TestUninstallResolvesRCSymlink(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")

	repoDir := t.TempDir()
	target := filepath.Join(repoDir, "zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(userHome, ".zshrc")
	if err := os.Symlink(target, rc); err != nil {
		t.Fatal(err)
	}

	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	if code := runUninstall(nil, strings.NewReader(""), newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("uninstall = %d", code)
	}

	info, err := os.Lstat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("uninstall replaced the symlink at ~/.zshrc with a plain file")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("symlink target = %q, want the pre-setup original %q", got, original)
	}
}

// TestPurgeRefusesADirectoryThatDoesNotLookLikeAChottagHome pins fix round
// 3's D7/F-I: --purge is an unconditional os.RemoveAll($CHOTTAG_HOME), and
// the typed confirmation guards against an ACCIDENTAL purge, not against
// $CHOTTAG_HOME naming the wrong directory entirely (a stale `export
// CHOTTAG_HOME=$HOME`, or a leaked test variable). A directory with none of
// state.json, accounts/, ca/ca.pem must be refused before RemoveAll ever
// runs, and must survive.
func TestPurgeRefusesADirectoryThatDoesNotLookLikeAChottagHome(t *testing.T) {
	interactiveStdin(t)
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	// No runSetup: home exists (t.TempDir already created it) but holds
	// none of the markers a real chottag home would.
	decoy := filepath.Join(home, "not-chottags.txt")
	if err := os.WriteFile(decoy, []byte("unrelated file"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, io.Discard, &stderr))

	if code != exit.Error {
		t.Fatalf("--purge proceeded against a directory with no chottag markers (code = %d, stderr = %q)", code, stderr.String())
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("--purge removed %s despite it not looking like a chottag home: %v", home, err)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("--purge removed the decoy file: %v", err)
	}
	if !strings.Contains(stderr.String(), "does not look like a chottag home") {
		t.Errorf("stderr %q does not explain the refusal", stderr.String())
	}
}

// --purge takes the exact word "purge" and nothing else: destroying every
// slot login must not ride on a global --yes or a loose confirmation.
func TestPurgeDeletesHomeOnExactConfirmation(t *testing.T) {
	interactiveStdin(t)
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}

	if code := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("uninstall --purge = %d", code)
	}
	if _, err := os.Stat(home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--purge did not remove %s: %v", home, err)
	}
}

// --purge promises it deletes every slot's login. On darwin the credential
// is a Keychain item, not a file in the slot, so removing the tree alone
// leaves every login alive and unreachable.
//
// An orphan dir named "0" rides along so the target set has three distinct
// keys instead of two: with only two, dropping sort.Strings can still
// happen to match the map's (randomised) iteration order about half the
// time, so a missing-sort mutation would only sometimes fail this test. "0"
// sorts before "A" and "B" (ASCII), so the exact-order assertion below
// requires the sort to have actually run.
func TestPurgeDeletesEachSlotsCredentialBeforeRemovingTheTree(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Dir: dir}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(home, "accounts", "0")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runUninstall --purge = %d; stderr=%q", got, errBuf.String())
	}
	want := []string{
		filepath.Join(home, "accounts", "0"),
		filepath.Join(home, "accounts", "A"),
		filepath.Join(home, "accounts", "B"),
	}
	if !reflect.DeepEqual(*deleted, want) {
		t.Errorf("credentials deleted = %v, want %v in sorted order", *deleted, want)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home survives --purge: %v", err)
	}
}

// A credential that cannot be deleted must not be silently swallowed by the
// RemoveAll: the user would be told every login was destroyed. The message
// must say the tree was NOT removed and point at --purge to re-run, not the
// earlier round's "nothing was removed", since the rc block and shim
// symlinks are already gone by the time this can fire.
func TestPurgeReportsACredentialItCouldNotDelete(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		dir := filepath.Join(home, "accounts", "A")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, errors.New("keychain locked"))

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got == exit.OK {
		t.Error("runUninstall --purge reported success despite a credential it could not delete")
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("home was removed despite the credential failure: %v", err)
	}
	want := []string{filepath.Join(home, "accounts", "A")}
	if !reflect.DeepEqual(*deleted, want) {
		t.Errorf("credsDelete calls = %v, want exactly %v", *deleted, want)
	}
	if !strings.Contains(errBuf.String(), "NOT removed") || !strings.Contains(errBuf.String(), "--purge") {
		t.Errorf("stderr = %q, want it to say the tree was NOT removed and to re-run --purge", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "nothing was removed") {
		t.Errorf("stderr = %q, want it not to say \"nothing was removed\": the rc block and shim were already gone by this point", errBuf.String())
	}
}

// A failed run can leave a slot dir on disk with no matching state.json
// entry. --purge is about to delete that dir too, so its credential must not
// survive just because nothing ever registered it.
func TestPurgeDeletesAnOrphanSlotDirsCredentialToo(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		dir := filepath.Join(home, "accounts", "A")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(home, "accounts", "C")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runUninstall --purge = %d; stderr=%q", got, errBuf.String())
	}
	found := false
	for _, p := range *deleted {
		if p == orphan {
			found = true
		}
	}
	if !found {
		t.Errorf("credentials deleted = %v, want the orphan slot dir %s among them", *deleted, orphan)
	}
}

// An account whose Dir sits outside the tree --purge removes must keep its
// login reachable: RemoveAll(h) never touches that dir, so deleting its
// credential too would just destroy an otherwise-untouched login. The
// operator is told, by name, that it was left alone.
func TestPurgeLeavesAnOutOfTreeAccountsLoginInPlace(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	outside := t.TempDir()
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: outside})
	}); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runUninstall --purge = %d; stderr=%q", got, errBuf.String())
	}
	for _, p := range *deleted {
		if p == outside {
			t.Errorf("credentials deleted = %v, want B's out-of-tree dir %s excluded", *deleted, outside)
		}
	}
	if !strings.Contains(out.String(), "B's login is outside") {
		t.Errorf("stdout = %q, want it to name B as left in place", out.String())
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("out-of-tree dir %s no longer exists after --purge: %v", outside, err)
	}
}

// A slot reached through a path ALIAS of <h>/accounts — not a different
// directory, the SAME directory under a different spelling — is still one
// RemoveAll(h) deletes, so its credential must be deleted too. The Keychain
// service name is keyed by the path STRING (F133), so the alias spelling
// (not the canonical one) is the key that must be deleted; the canonical
// spelling gets deleted anyway by the orphan sweep, which is fine because
// Delete is idempotent. A symlinked alias is used here rather than a
// case-folded one so the test is portable to filesystems that don't fold
// case.
func TestPurgeDeletesACredentialReachedThroughAPathAlias(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "accounts", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(alias, "accounts", "A")

	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: aliasDir})
	}); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runUninstall --purge = %d; stderr=%q", got, errBuf.String())
	}
	found := false
	for _, p := range *deleted {
		if p == aliasDir {
			found = true
		}
	}
	if !found {
		t.Errorf("credentials deleted = %v, want the alias spelling %s among them: the Keychain key is the path string", *deleted, aliasDir)
	}
	if strings.Contains(out.String(), "left in place") {
		t.Errorf("stdout = %q, want no \"left in place\" line: RemoveAll can reach this dir through the alias", out.String())
	}
}

// F4: a slot that is itself a symlink is left in place, exactly like a
// genuinely out-of-tree one — RemoveAll(h) only unlinks the symlink entry,
// never whatever it points at, so there is no real slot here for
// credsDelete to destroy.
func TestPurgeLeavesASymlinkedSlotInPlace(t *testing.T) {
	interactiveStdin(t)
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
		return st.Add(store.Account{Name: "A", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runUninstall --purge = %d; stderr=%q", got, errBuf.String())
	}
	for _, p := range *deleted {
		if p == dir {
			t.Errorf("credentials deleted = %v, want A's symlinked slot %s excluded", *deleted, dir)
		}
	}
	if !strings.Contains(out.String(), "A's login at "+dir+" is a symlink") {
		t.Errorf("stdout = %q, want it to name A's slot as a symlink left in place", out.String())
	}
}

// A state.json Load error must stop the purge before anything is deleted:
// ignoring it would silently proceed as if no accounts were registered.
func TestPurgeStopsWithTheTreeIntactWhenStateJSONIsCorrupt(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "accounts", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got == exit.OK {
		t.Fatal("runUninstall --purge succeeded despite a corrupt state.json")
	}
	if len(*deleted) != 0 {
		t.Errorf("credsDelete calls = %v, want none: state.json never loaded", *deleted)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("home was removed despite state.json being corrupt: %v", err)
	}
	if !strings.Contains(errBuf.String(), "NOT removed") || !strings.Contains(errBuf.String(), "--purge") {
		t.Errorf("stderr = %q, want it to say the tree was NOT removed and to re-run --purge", errBuf.String())
	}
}

// A failure listing <h>/accounts/ for anything other than "it doesn't
// exist" must stop the purge the same way a credential failure does.
func TestPurgeStopsWithTheTreeIntactWhenAccountsCannotBeListed(t *testing.T) {
	interactiveStdin(t)
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// "accounts" is a plain file, not a directory: os.ReadDir fails with
	// ENOTDIR, which is not the tolerated os.IsNotExist case.
	if err := os.WriteFile(filepath.Join(home, "accounts"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	var out, errBuf bytes.Buffer
	if got := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, &out, &errBuf)); got == exit.OK {
		t.Fatal("runUninstall --purge succeeded despite an unreadable accounts/ dir")
	}
	if len(*deleted) != 0 {
		t.Errorf("credsDelete calls = %v, want none", *deleted)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("home was removed despite accounts/ being unreadable: %v", err)
	}
	if !strings.Contains(errBuf.String(), "NOT removed") || !strings.Contains(errBuf.String(), "--purge") {
		t.Errorf("stderr = %q, want it to say the tree was NOT removed and to re-run --purge", errBuf.String())
	}
}

// uninstall without --purge must never reach a credential: it is the flag
// that opts into destroying a login, and TestMain's panicking default for
// credsDelete exists precisely so a path that skips SetCredsDeleteForTest
// but still reaches the seam is caught.
func TestUninstallWithoutPurgeNeverCallsCredsDelete(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		return st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")})
	}); err != nil {
		t.Fatal(err)
	}
	deleted := stubCredsDelete(t, nil)

	if code := runUninstall(nil, strings.NewReader(""), newReporter(false, io.Discard, io.Discard)); code != exit.OK {
		t.Fatalf("uninstall = %d", code)
	}
	if len(*deleted) != 0 {
		t.Errorf("credsDelete calls = %v, want none without --purge", *deleted)
	}
}
