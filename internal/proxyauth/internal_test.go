package proxyauth

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestPermProblemForeignOwner exercises permProblem's ForeignOwner branch
// on the secret file itself, which real CI cannot reach without a second
// uid: it swaps the getuid seam (F130: the restore func restores whatever
// was captured at swap time, not a named default) to a value that can
// never be the file's real owner, and calls permProblem directly so the
// ca/ directory's own (identical) check never gets in the way first.
func TestPermProblemForeignOwner(t *testing.T) {
	home := t.TempDir()
	if _, err := LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	p := Path(home)
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	orig := getuid
	getuid = func() int { return orig() + 1 }
	defer func() { getuid = orig }()

	pe := permProblem(p, info)
	if pe == nil || !pe.ForeignOwner || pe.NotRegular || pe.Path != p {
		t.Fatalf("permProblem with a foreign getuid = %v, want *PermError{Path: %q, ForeignOwner: true}", pe, p)
	}
	// The file-level message never gets the ca/ directory's uid-number
	// wording (D2): it stays the original, generic text.
	// The path is dropped first: a random t.TempDir name can hold any
	// digits or letters, so only the message's own words are checked (F246).
	if msg := strings.ReplaceAll(pe.Error(), p, "PATH"); !strings.Contains(msg, "is owned by another user") || strings.Contains(msg, "uid") {
		t.Errorf("file-level ForeignOwner .Error() = %q, want the generic file text, not the dir's uid wording", msg)
	}
}

// TestCheckDirPermRefusesAWritableOrForeignDir covers checkDirPerm's own
// three refusal branches, each with its own PermError text (D2: the ca/
// directory never reuses the secret file's "want 0600" wording): a
// directory that is group- or other-writable, one owned by another user
// (via the same getuid seam as the file-level check), and a path that
// isn't a directory at all.
func TestCheckDirPermRefusesAWritableOrForeignDir(t *testing.T) {
	home := t.TempDir()
	if _, err := LoadOrCreate(home); err != nil {
		t.Fatal(err)
	}
	dir := home + "/ca"

	if err := os.Chmod(dir, 0o707); err != nil {
		t.Fatal(err)
	}
	err := checkDirPerm(dir)
	pe, ok := err.(*PermError)
	if !ok || pe.ForeignOwner || pe.NotRegular {
		t.Fatalf("checkDirPerm(0707) = %v, want a plain *PermError", err)
	}
	// The path is dropped first: a t.TempDir name such as …1910606008 can
	// itself contain "0600" (F246).
	if msg := strings.ReplaceAll(err.Error(), dir, "PATH"); !strings.Contains(msg, "group/other-writable") || !strings.Contains(msg, "0707") || !strings.Contains(msg, "want 0700") || strings.Contains(msg, "0600") {
		t.Errorf("checkDirPerm(0707).Error() = %q, want the dir-specific group/other-writable text", msg)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkDirPerm(dir); err != nil {
		t.Fatalf("checkDirPerm on a clean 0700 dir = %v, want nil", err)
	}

	orig := getuid
	getuid = func() int { return orig() + 1 }
	defer func() { getuid = orig }()
	err = checkDirPerm(dir)
	pe, ok = err.(*PermError)
	if !ok || !pe.ForeignOwner {
		t.Fatalf("checkDirPerm with a foreign getuid = %v, want *PermError{ForeignOwner: true}", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "owned by uid "+strconv.Itoa(orig())) {
		t.Errorf("checkDirPerm foreign-owner .Error() = %q, want it to name uid %d", msg, orig())
	}
}

// TestCheckDirPermRefusesANonDirectory covers checkDirPerm's third branch:
// a ca/ that isn't a directory at all, with its own "is not a directory"
// text (never the secret file's "is not a regular file").
func TestCheckDirPermRefusesANonDirectory(t *testing.T) {
	home := t.TempDir()
	dir := home + "/ca"
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkDirPerm(dir)
	pe, ok := err.(*PermError)
	if !ok || !pe.NotRegular || pe.ForeignOwner {
		t.Fatalf("checkDirPerm(a file) = %v, want *PermError{NotRegular: true}", err)
	}
	if msg := strings.ReplaceAll(err.Error(), dir, "PATH"); !strings.Contains(msg, "is not a directory") || strings.Contains(msg, "regular file") {
		t.Errorf("checkDirPerm(a file).Error() = %q, want the dir-specific \"is not a directory\" text", msg)
	}
}
