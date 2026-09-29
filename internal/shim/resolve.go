// Package shim resolves the real `claude` binary on behalf of chottag's own
// `claude` symlink, so a session runs through the account currently selected
// in state.json without the user changing anything about how they invoke
// Claude Code. This package only resolves the path; the caller execs it.
package shim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// executablePath is os.Executable, overridable so a test can point
// ResolveClaude at a fixture file instead of the real test binary.
var executablePath = os.Executable

// ResolveClaude finds the real `claude` binary.
//
// cached is state.json's RealClaude, used only while it still names an
// executable file that is not chottag itself — a cache that outlives an
// upgrade, an uninstall, or a prior bad resolution would send every session
// at a path that no longer works, or worse, back at chottag's own binary.
//
// The search skips selfDir (chottag's own bin, where OUR claude symlink
// lives) by resolved path, any */cmux-cli-shims/* entry, and any candidate
// that IS chottag's own executable — a symlink chain or a hard link to it,
// see isSelf — all three would resolve back to chottag and exec in a loop.
// The cache is subject to the same self-identity check as the PATH walk.
func ResolveClaude(pathEnv, selfDir, cached string) (string, error) {
	selfExe, err := executablePath()
	if err != nil {
		// Cannot promise "never resolves to chottag itself" without knowing
		// what chottag itself is — fail closed rather than falling back to
		// the directory guard alone.
		return "", fmt.Errorf("chottag: could not determine its own executable path: %w", err)
	}
	selfReal := realPath(selfDir)

	if cached != "" && isExecutableFile(cached) && !isSelf(cached, selfReal, selfExe) {
		return cached, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		if realPath(dir) == selfReal || isCmuxShimDir(dir) {
			continue
		}
		cand := filepath.Join(dir, "claude")
		if !isExecutableFile(cand) {
			continue
		}
		if isSelf(cand, selfReal, selfExe) {
			continue
		}
		return cand, nil
	}
	return "", fmt.Errorf("chottag: no real `claude` found on PATH (searched %q, skipping chottag's own %s)", pathEnv, selfDir)
}

// isSelf reports whether path IS chottag — the thing this function exists
// to never return, because the caller execs the result and chottag
// re-execing itself never terminates.
//
// The binary arm compares INODES, not resolved path strings: os.Stat
// follows symlinks, so os.SameFile catches a symlink chain AND a hard link
// to the same inode, which realPath's filepath.EvalSymlinks cannot — it has
// no canonical path to resolve a hardlink to. Same technique, and same
// reason, as sameFile in internal/cli/daemon.go. If either os.Stat fails,
// that is treated as "not us": both call sites already checked
// isExecutableFile(path), so a failure here means path vanished between the
// two calls, and the PATH walk simply moves on to the next entry.
//
// The directory arm still compares resolved path strings — it is what
// covers the cache branch pointing directly into chottag's own bin dir
// (selfReal), and it is cheap.
func isSelf(path, selfReal, selfExe string) bool {
	if realPath(filepath.Dir(path)) == selfReal {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	selfInfo, err := os.Stat(selfExe)
	if err != nil {
		return false
	}
	return os.SameFile(info, selfInfo)
}

// isExecutableFile reports whether path names a regular file with any
// execute bit set.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// realPath resolves path to an absolute, symlink-free form so two
// differently-spelled paths that point at the same place compare equal —
// including a relative PATH entry compared against an absolute directory.
// If path can't be made absolute, or its symlinks can't be resolved
// (doesn't exist, permission error, ...), it falls back to comparing path
// literally.
func realPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// isCmuxShimDir reports whether dir has a path element named
// "cmux-cli-shims" — another wrapper directory, on this user's machines,
// that would resolve back to a shim and exec in a loop.
func isCmuxShimDir(dir string) bool {
	for _, part := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if part == "cmux-cli-shims" {
			return true
		}
	}
	return false
}
