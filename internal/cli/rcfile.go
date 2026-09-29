package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/fsutil"
)

// The fence lets uninstall remove EXACTLY what setup added, so the rc file
// comes back byte-for-byte (§6.1 invariant 5). It is also why setup is
// idempotent: a second run replaces the fenced block rather than appending
// a second one.
const (
	rcStart = "# >>> chottag >>>"
	rcEnd   = "# <<< chottag <<<"
)

// dquoteEscaper escapes the four characters a POSIX double-quoted string
// treats specially (backslash first, so escaping the others never doubles
// up), so chottagHome always embeds safely in rcBlock's export line no
// matter what bytes CHOTTAG_HOME happens to hold.
var dquoteEscaper = strings.NewReplacer(
	`\`, `\\`,
	`"`, `\"`,
	`$`, `\$`,
	"`", "\\`",
)

// rcBlock is the exact text setup writes. chottagHome is the absolute
// $CHOTTAG_HOME to also export, or "" when CHOTTAG_HOME was unset at setup
// time — then the block is byte-identical to before CHOTTAG_HOME
// persistence existed (M3 fix round, item 9): a plain PATH export is still
// the only thing most installs need, and the smaller the footprint the
// less there is to restore. When chottagHome is set, its export line comes
// BEFORE the PATH line, so a new shell resolves the right home even though
// $HOME/.chottag would otherwise look like a plausible (but wrong,
// separate and empty) default.
func rcBlock(binDir, chottagHome string) string {
	b := rcStart + "\n"
	if chottagHome != "" {
		b += "export CHOTTAG_HOME=\"" + dquoteEscaper.Replace(chottagHome) + "\"\n"
	}
	return b + "export PATH=\"" + dquoteEscaper.Replace(binDir) + ":$PATH\"\n" + rcEnd + "\n"
}

// stripRCBlock removes a fenced chottag block from s, along with the single
// blank-line separator writeRCBlock inserts ahead of it, returning s exactly
// as it was before writeRCBlock ever touched it. writeRCBlock and
// removeRCBlock both go through this one routine so they can never drift
// apart: whatever writeRCBlock adds, this undoes.
func stripRCBlock(s string) string {
	start := strings.Index(s, rcStart)
	if start == -1 {
		return s
	}
	rest := s[start:]
	end := strings.Index(rest, rcEnd)
	if end == -1 {
		// No matching end fence: leave the (malformed) content alone rather
		// than guess how much to delete.
		return s
	}
	end += len(rcEnd)
	if end < len(rest) && rest[end] == '\n' {
		end++
	}
	prefix, suffix := s[:start], rest[end:]
	// writeRCBlock separates its block from any pre-existing content with a
	// single blank line (one extra "\n" beyond the pre-existing content's
	// own trailing newline). Drop exactly that one byte, no more.
	if strings.HasSuffix(prefix, "\n\n") {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}

// resolveRCPath returns the file writeRCBlock/removeRCBlock should actually
// read and write. rcPath itself is frequently a symlink: stow, chezmoi,
// yadm and hand-rolled dotfiles repos all commonly make ~/.zshrc a link into
// a repo elsewhere. fsutil.WriteFileAtomic ends in os.Rename(tmp, path),
// and rename replaces whatever sits AT path — for a symlink, that means
// deleting the link and putting a plain file in its place, silently
// severing the user's repo from their shell. Resolving to the symlink's
// target first means the rename lands on the real file instead, so the
// link (and whatever manages it) survives untouched.
//
// A path that does not exist yet, or one filepath.EvalSymlinks otherwise
// can't resolve (permission error, broken link, ...), falls back to the
// literal path: there is no target to preserve, so writing rcPath directly
// is correct. Same technique as internal/shim/resolve.go's realPath and
// internal/cli/daemon.go's sameFile (F114).
func resolveRCPath(rcPath string) string {
	resolved, err := filepath.EvalSymlinks(rcPath)
	if err != nil {
		return rcPath
	}
	return resolved
}

// writeRCBlock adds or replaces the fenced chottag block in rcPath (or, if
// rcPath is a symlink, in its target — see resolveRCPath), leaving every
// other byte of the file untouched — including its permission bits.
// fsutil.WriteFileAtomic applies whatever mode it is given unconditionally
// (it Chmods the temp file before the rename), so a hardcoded 0o644 here
// used to widen a deliberately-locked-down rc (e.g. a user's own `chmod 600
// ~/.zshrc`, because shell rc files routinely hold exported API keys) to
// world-readable on every `chottag setup` (whole-branch review D5). The
// target's own existing mode is reused instead; 0o644 is now only ever the
// default for a file that does not exist yet to have a mode of its own.
// Running it twice with the same binDir and chottagHome leaves the file
// byte-identical (idempotence pins this at TestWriteRCBlockIsIdempotent) —
// and, since fix round item 10, does not even rewrite it: when the content
// it would write already equals what is on disk, it returns without
// touching the file at all, so a repeat `chottag setup` never bumps the
// rc's mtime or gives it a new inode.
func writeRCBlock(rcPath, binDir, chottagHome string) error {
	target := resolveRCPath(rcPath)
	perm := os.FileMode(0o644)
	if info, err := os.Stat(target); err == nil {
		perm = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	base := stripRCBlock(string(data))
	content := base
	if content != "" {
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += "\n" // blank line ahead of chottag's block
	}
	content += rcBlock(binDir, chottagHome)
	if content == string(data) {
		return nil
	}
	return fsutil.WriteFileAtomic(target, []byte(content), perm)
}

// removeRCBlock undoes what writeRCBlock did, restoring rcPath (or, if it is
// a symlink, its target) as closely as possible to what it held before
// (§6.1 invariant 5). A missing file is not an error: there is nothing to
// remove.
//
// It never deletes the file, even when stripping the block leaves nothing
// behind. At this point "the file did not exist before writeRCBlock" and
// "the file existed but was empty" are indistinguishable — chottag records
// no state to tell them apart — so one of the two must come back wrong.
// Deleting gets the first case right and the second case wrong by
// destroying a file the user had; worse, if rcPath is a symlink (see
// resolveRCPath), os.Remove would unlink it from their dotfiles repo, which
// they may not notice until their shell config stops loading. Leaving a
// zero-byte file gets the second case wrong instead, by the much smaller
// margin of an empty file the user can delete in a second — and never
// touches a symlink at all. That asymmetry is why this always writes rather
// than ever removing.
//
// It never rewrites a file stripping changed nothing in, either — reachable
// whenever $SHELL names a shell whose rc chottag never actually wrote to (a
// zsh user whose real config lives in ~/.zprofile), on a second `uninstall`,
// or on `uninstall` without a prior `setup`. Rewriting such a file would
// give it a new inode (breaking any hardlink) and — before this fix — a
// hardcoded 0o644 mode, on a file uninstall never touched at all
// (whole-branch review D5). When it does rewrite, it reuses the target's
// existing mode rather than hardcoding 0o644, for the identical reason
// writeRCBlock does (see its doc comment).
func removeRCBlock(rcPath string) error {
	target := resolveRCPath(rcPath)
	data, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	content := stripRCBlock(string(data))
	if content == string(data) {
		return nil
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(target, []byte(content), info.Mode().Perm())
}

// rcPathFor maps a $SHELL value to the rc file setup should edit. An
// unrecognised shell returns "": the caller prints the PATH line instead of
// guessing which file the user's shell actually reads.
func rcPathFor(shell, userHome string) string {
	switch filepath.Base(shell) {
	case "zsh":
		return filepath.Join(userHome, ".zshrc")
	case "bash":
		return filepath.Join(userHome, ".bashrc")
	default:
		return ""
	}
}
