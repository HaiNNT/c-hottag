package shim

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cmuxHandoffEnvVar marks that this launch has already been handed off,
// once, to cmux's own claude wrapper (F243). Run drops it from the
// environment the real Claude Code receives, so a nested `claude` started
// from inside that session sees a clean environment and, if it too is
// inside a cmux surface, can hand off again on its own.
const cmuxHandoffEnvVar = "CHOTTAG_CMUX_HANDOFF"

// getpid is os.Getpid, a seam: cmux's wrapper exports CMUX_CLAUDE_PID=$$
// before exec'ing the next `claude` on PATH, and exec preserves the pid, so
// a process the wrapper just exec'd sees CMUX_CLAUDE_PID equal to its own
// pid. cmuxHandoffTarget uses this seam so a test can pin "this process's
// own pid" without forking one.
var getpid = os.Getpid

// cmuxHandoffTarget is Run's F243 fix: it reports the path to hand off to —
// cmux's own claude wrapper shim — or "" when Run should proceed with its
// normal launch instead. Run calls it once, early and cheaply (fix round 1,
// F243-R1: outside cmux — the common case — the decision is one env lookup,
// CMUX_SURFACE_ID's own absence, not a stat call in sight), but only acts on
// a non-"" result LATE — see Run's own doc comment for why.
//
// cmux, a terminal, wraps `claude`. In zsh, cmux (re-)installs a `claude`
// shell function every prompt that runs CMUX_CLAUDE_WRAPPER_SHIM directly.
// That wrapper adds `--session-id` and a `--settings` carrying its
// notification/status/restore hooks, exports CMUX_CLAUDE_PID=$$ and two
// CMUX_AGENT_LAUNCH_* variables, and then launches Claude Code: its own
// "Claude Binary Path" setting (CMUX_CUSTOM_CLAUDE_PATH), if the user set
// one, wins; otherwise it execs the first `claude` on PATH that is not one
// of cmux's own shims — with chottag installed, and no custom path set,
// that is chottag's own shim, which the wrapper treats as the real claude.
//
// In a shell where that function is absent (a kiro-cli terminal's
// `zsh --login` is the case F243 was found in), `claude` instead resolves
// by PATH. chottag's rc block puts its own bin first, so chottag's shim
// runs BEFORE cmux's wrapper ever does: cmux's hooks are silently lost.
// Handing off here, once, restores cmux -> chottag -> Claude Code in every
// shell: this exec's cmux's wrapper, which (absent a custom path pointing
// elsewhere) finds chottag's shim again as "the real claude" and execs it a
// second time — this time carrying its hooks, CMUX_CLAUDE_PID equal to
// (what is now) our own pid, and the marker Run set, so THIS second pass
// falls through to the normal path instead of handing off again.
//
// All conditions below must hold for a hand-off:
//  1. CMUX_SURFACE_ID is set in env: we are inside a cmux surface at all.
//  2. CMUX_CLAUDE_WRAPPER_SHIM names an existing, executable regular file
//     that is not chottag itself — isSelf, the same check ResolveClaude
//     uses, against this home's own bin dir and executablePath().
//  3. cmuxHandoffEnvVar is not "1" in env: this launch has not already
//     handed off (the marker; needed because not every path back into
//     chottag's shim carries CMUX_CLAUDE_PID — see point 4).
//  4. CMUX_CLAUDE_PID in env is not this process's own pid (the getpid
//     seam): equal means cmux's wrapper just exec'd us directly (its hooks
//     path), so handing off again would exec cmux's wrapper a second time
//     for the one launch it already hooked. Absent, or naming a different
//     pid (a nested session inside an already-hooked one, say), means the
//     wrapper never ran for THIS launch yet — the marker in point 3 alone
//     covers the wrapper's other paths, ones that exec without exporting
//     CMUX_CLAUDE_PID at all (its passthrough or hooks-disabled paths).
//  5. CMUX_CLAUDE_HOOKS_DISABLED is not "1": the wrapper adds no hooks in
//     that mode, so a hand-off would exec it for nothing.
//  6. CMUX_CUSTOM_CLAUDE_PATH, trimmed, is either unset/empty, or does not
//     name an existing, executable, non-chottag regular file DIFFERENT from
//     CMUX_CLAUDE_WRAPPER_SHIM itself. When it DOES (the user pointed
//     cmux's own "Claude Binary Path" setting straight at the real Claude
//     Code), the wrapper never comes back to chottag no matter what this
//     function does — handing off would only exec cmux's wrapper for a
//     launch that was always going to skip chottag, and this function does
//     not replicate the rest of the wrapper's own PATH walk to try to
//     out-guess it. Two cases are NOT this one, and still hand off: a
//     custom path naming chottag itself (isSelf true) comes back exactly as
//     it would with no custom path set; and a custom path that is the SAME
//     FILE (sameFile, os.SameFile) as CMUX_CLAUDE_WRAPPER_SHIM is what
//     cmux's own find_real_claude treats as no custom path at all — it
//     ignores a "custom" path that only points back at its own shim or
//     wrapper, and walks PATH the normal way instead (F243-R2).
func cmuxHandoffTarget(env []string, home string) string {
	if envGet(env, "CMUX_SURFACE_ID") == "" {
		return ""
	}
	shimPath := envGet(env, "CMUX_CLAUDE_WRAPPER_SHIM")
	if shimPath == "" || !isExecutableFile(shimPath) {
		return ""
	}
	if envGet(env, cmuxHandoffEnvVar) == "1" {
		return ""
	}
	if pidStr := envGet(env, "CMUX_CLAUDE_PID"); pidStr != "" {
		if pid, err := strconv.Atoi(pidStr); err == nil && pid == getpid() {
			return ""
		}
	}
	if envGet(env, "CMUX_CLAUDE_HOOKS_DISABLED") == "1" {
		return ""
	}
	selfExe, err := executablePath()
	if err != nil {
		return ""
	}
	selfReal := realPath(filepath.Join(home, "bin"))
	if isSelf(shimPath, selfReal, selfExe) {
		return ""
	}
	if customPath := strings.TrimSpace(envGet(env, "CMUX_CUSTOM_CLAUDE_PATH")); customPath != "" &&
		isExecutableFile(customPath) && !isSelf(customPath, selfReal, selfExe) && !sameFile(customPath, shimPath) {
		return ""
	}
	return shimPath
}

// sameFile reports whether a and b — both already known to exist (every
// caller here has just called isExecutableFile on both) — are the same
// file: a symlink chain or a hard link to the same inode, the same
// technique isSelf uses (resolve.go's own doc comment) and internal/cli's
// own sameFile (daemon.go) uses for the same reason.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
