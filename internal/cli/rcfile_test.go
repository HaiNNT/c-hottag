package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setup must be idempotent: running it twice leaves the rc file byte-identical.
func TestWriteRCBlockIsIdempotent(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	once, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	twice, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Errorf("second writeRCBlock changed the file:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
	if strings.Count(string(twice), rcStart) != 1 {
		t.Errorf("fence appears %d times, want exactly 1", strings.Count(string(twice), rcStart))
	}
	if !strings.HasPrefix(string(twice), original) {
		t.Error("the user's own rc content must be preserved ahead of our block")
	}
}

// TestRCBlockCarriesChottagHomeWhenSet pins fix round item 9: the export
// line for CHOTTAG_HOME comes before the PATH line, and is absent
// altogether — leaving the block byte-identical to before item 9 — when
// chottagHome is "".
func TestRCBlockCarriesChottagHomeWhenSet(t *testing.T) {
	got := rcBlock("/home/u/.chottag/bin", "/x/chottag-home")
	want := rcStart + "\n" +
		`export CHOTTAG_HOME="/x/chottag-home"` + "\n" +
		`export PATH="/home/u/.chottag/bin:$PATH"` + "\n" +
		rcEnd + "\n"
	if got != want {
		t.Errorf("rcBlock with chottagHome set = %q, want %q", got, want)
	}

	unset := rcBlock("/home/u/.chottag/bin", "")
	old := rcStart + "\n" + `export PATH="/home/u/.chottag/bin:$PATH"` + "\n" + rcEnd + "\n"
	if unset != old {
		t.Errorf("rcBlock with chottagHome unset = %q, want the pre-item-9 block %q", unset, old)
	}
}

// TestRCBlockEscapesUnsafeCharactersInChottagHome: a CHOTTAG_HOME holding a
// double quote, `$`, a backtick or a backslash must not let the rc block
// break out of its export statement, or run as a command when the rc is
// sourced. Escaping (rather than refusing) keeps setup working for any
// literal path, matching a POSIX double-quoted string's own escaping rules.
// It proves the escaping is correct by actually sourcing the line with a
// real shell (fixed, hardcoded script content — never a runtime-built -c
// string) and reading CHOTTAG_HOME back out.
func TestRCBlockEscapesUnsafeCharactersInChottagHome(t *testing.T) {
	unsafe := `/x/weird"$(rm -rf ~)` + "`" + `\home`
	block := rcBlock("/home/u/.chottag/bin", unsafe)
	wantLine := `export CHOTTAG_HOME="/x/weird\"\$(rm -rf ~)\` + "`" + `\\home"`
	if !strings.Contains(block, wantLine+"\n") {
		t.Errorf("rcBlock did not escape the unsafe chottagHome; got:\n%s\nwant the line:\n%s", block, wantLine)
	}

	script := filepath.Join(t.TempDir(), "check.sh")
	body := "#!/bin/sh\n" + wantLine + "\nprintf '%s' \"$CHOTTAG_HOME\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != unsafe {
		t.Errorf("a shell sourcing the escaped export line got CHOTTAG_HOME=%q, want %q", out, unsafe)
	}
}

// TestRCBlockEscapesUnsafeCharactersInBinDir pins fix round 2's N2:
// rcBlock's PATH line embedded binDir unescaped, so the same unsafe
// characters CHOTTAG_HOME already guards against could break out of the
// PATH export too. An ordinary path must stay byte-identical to before.
func TestRCBlockEscapesUnsafeCharactersInBinDir(t *testing.T) {
	ordinary := rcBlock("/home/u/.chottag/bin", "")
	if want := rcStart + "\n" + `export PATH="/home/u/.chottag/bin:$PATH"` + "\n" + rcEnd + "\n"; ordinary != want {
		t.Errorf("rcBlock with an ordinary binDir = %q, want %q unchanged", ordinary, want)
	}

	unsafe := `/x/weird"$(rm -rf ~)` + "`" + `\bin`
	block := rcBlock(unsafe, "")
	wantLine := `export PATH="/x/weird\"\$(rm -rf ~)\` + "`" + `\\bin:$PATH"`
	if !strings.Contains(block, wantLine+"\n") {
		t.Errorf("rcBlock did not escape the unsafe binDir; got:\n%s\nwant the line:\n%s", block, wantLine)
	}

	script := filepath.Join(t.TempDir(), "check.sh")
	body := "#!/bin/sh\n" + wantLine + "\nprintf '%s' \"$PATH\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := unsafe + ":" + os.Getenv("PATH"); string(out) != want {
		t.Errorf("a shell sourcing the escaped export line got PATH=%q, want %q", out, want)
	}
}

// TestWriteRCBlockSkipsTheWriteWhenContentIsUnchanged pins fix round item
// 10: a second `chottag setup` with the same binDir and chottagHome must
// not touch the rc file at all — not its mtime, not its inode — so a
// repeat install/setup is a true no-op on the filesystem.
func TestWriteRCBlockSkipsTheWriteWhenContentIsUnchanged(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("export EDITOR=vim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", "/home/u/.chottag"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", "/home/u/.chottag"); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the second writeRCBlock gave the rc a new inode; want the exact same file left untouched")
	}
	if before.ModTime() != after.ModTime() {
		t.Errorf("mtime changed from %v to %v; an unchanged rewrite must not bump it", before.ModTime(), after.ModTime())
	}
}

// removeRCBlock must restore the file byte-for-byte (§6.1 invariant 5).
func TestRemoveRCBlockRestoresTheFileExactly(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "export EDITOR=vim\nalias ll='ls -la'\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	if err := removeRCBlock(rc); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("rc after remove = %q, want the original %q byte-for-byte", got, original)
	}
}

// removeRCBlock must never delete the rc file, even when nothing remains
// after stripping the block — e.g. because the rc did not exist before
// writeRCBlock. "did not exist" and "existed but was empty" are
// indistinguishable without recorded state, and removeRCBlock's doc comment
// explains why it chooses to get the empty case "wrong" (a leftover
// zero-byte file) rather than the missing-file case "wrong" (deleting
// something the user had, or unlinking a dotfiles symlink).
func TestRemoveRCBlockNeverDeletesEvenWhenNothingRemains(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	if err := removeRCBlock(rc); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("removeRCBlock deleted the file instead of leaving it empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("rc after remove = %q, want empty", got)
	}
}

// TestWriteAndRemoveRCBlockPreserveA0600Mode pins fix round 3's D5: a user
// who deliberately locked their rc down (shell rc files routinely hold
// exported API keys) must not get it silently widened to world-readable by
// `chottag setup`, or by `chottag uninstall` restoring it afterwards.
func TestWriteAndRemoveRCBlockPreserveA0600Mode(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	if err := os.WriteFile(rc, []byte("export EDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode after writeRCBlock = %o, want 0600 (setup must not widen a locked-down rc)", got)
	}

	if err := removeRCBlock(rc); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode after removeRCBlock = %o, want 0600 (uninstall must not widen it either)", got)
	}
}

// TestRemoveRCBlockLeavesAnUntouchedRCsInodeAlone pins the other half of
// fix round 3's D5: when stripping the fenced block changes nothing —
// because the rc never had one (uninstall without a prior setup, a second
// uninstall, or a $SHELL whose real config lives elsewhere) — removeRCBlock
// must not rewrite the file at all. A rewrite would give it a new inode
// (breaking a hardlink) even though byte content alone looks unchanged.
func TestRemoveRCBlockLeavesAnUntouchedRCsInodeAlone(t *testing.T) {
	rc := filepath.Join(t.TempDir(), ".zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}

	if err := removeRCBlock(rc); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("removeRCBlock rewrote a file that never held a chottag block: it must leave a no-op rc's inode alone")
	}
	got, err := os.ReadFile(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("rc content = %q, want the original %q unchanged", got, original)
	}
}

// Dotfiles managed by stow/chezmoi/yadm/a hand-rolled repo commonly make the
// rc path a symlink into the repo. writeRCBlock must write THROUGH that
// symlink (into the repo file it points at) rather than replacing the link
// itself with a plain file, which would silently sever the repo from the
// user's shell. Idempotence must hold in this case too (D1, D3).
func TestWriteRCBlockPreservesASymlinkedRC(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-zshrc")
	original := "export EDITOR=vim\n"
	if err := os.WriteFile(real, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(real, rc); err != nil {
		t.Fatal(err)
	}

	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("writeRCBlock replaced the symlink with a plain file")
	}
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(got), rcStart) != 1 {
		t.Errorf("fence appears %d times in the symlink target, want exactly 1", strings.Count(string(got), rcStart))
	}
	if !strings.HasPrefix(string(got), original) {
		t.Error("the user's own rc content must be preserved ahead of our block in the symlink target")
	}
}

// removeRCBlock must leave a symlinked rc path exactly as writeRCBlock found
// it: still a symlink, target restored (§6.1 invariant 5, D1/D2/D3).
func TestRemoveRCBlockPreservesASymlinkedRC(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles-zshrc")
	original := "export EDITOR=vim\nalias ll='ls -la'\n"
	if err := os.WriteFile(real, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(dir, ".zshrc")
	if err := os.Symlink(real, rc); err != nil {
		t.Fatal(err)
	}

	if _, _, err := writeRCBlock(t.TempDir(), rc, "/home/u/.chottag/bin", ""); err != nil {
		t.Fatal(err)
	}
	if err := removeRCBlock(rc); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(rc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("removeRCBlock replaced the symlink with a plain file")
	}
	target, err := os.Readlink(rc)
	if err != nil || target != real {
		t.Fatalf("removeRCBlock changed the symlink target: %q, err %v", target, err)
	}
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("symlink target after remove = %q, want the original %q byte-for-byte", got, original)
	}
}
