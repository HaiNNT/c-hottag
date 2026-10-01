#!/usr/bin/env python3
"""PreToolUse guard (R77, F197): keep tool calls away from the real Home.

Every path is derived from the environment, never hard-coded: the real
home from $HOME, this project's auto-memory dir from $CLAUDE_PROJECT_DIR,
and chottag's tree from $CHOTTAG_HOME when the session sets it.

Denies, for Bash:
  - running install.sh, or `chottag setup|uninstall|install`, unless the
    command sets HOME= to a path other than the real home;
  - write-shaped commands (redirects, rm, mv, cp, ln, tee, sed -i, touch,
    mkdir, chmod, truncate, patch, dd, rsync) that name ~/.zshrc, ~/.chottag, ~/.claude.json
    or ~/.claude/ under the real home, or the session's $CHOTTAG_HOME
    (by path or as $CHOTTAG_HOME).
  - a chottag invocation (`chottag`, `./chottag`, an absolute path, `go run
    ./cmd/chottag` or its full module path, at command position -- past any
    NAME=value assignment or wrapper like env/sudo/exec) that is not one of
    R91's read-only forms, unless that one simple command's own leading
    CHOTTAG_HOME= names a path whose realpath is not the real ~/.chottag, or
    it is itself a scripts/dev-env or scripts/release invocation (R91: this
    repo's sessions never touch the maintainer's daily prod install). Each
    ;/&&/||/&/|/subshell-separated simple command on the line decides on
    its own.
Denies, for Write/Edit/NotebookEdit: any file under those paths, except this
project's auto-memory directory (derived from $CLAUDE_PROJECT_DIR), which
the harness owns.

Reads stay allowed. Prints a PreToolUse deny decision as JSON; silent otherwise.
"""
import json
import os
import re
import shlex
import sys

REAL_HOME = os.path.realpath(os.path.expanduser("~"))
PROTECTED = (".zshrc", ".chottag", ".claude.json", ".claude")


def memory_dir():
    """This project's auto-memory dir, which the harness owns: Claude Code
    names it after the project path with every non-alphanumeric character
    replaced by '-'. None when CLAUDE_PROJECT_DIR is unset: no exemption."""
    project = os.environ.get("CLAUDE_PROJECT_DIR", "")
    if not project:
        return None
    slug = re.sub(r"[^A-Za-z0-9]", "-", os.path.abspath(project))
    return os.path.join(REAL_HOME, ".claude", "projects", slug, "memory")


def chottag_home():
    """$CHOTTAG_HOME as this session sees it, when it names an absolute path
    other than the real home or /: chottag's tree, protected like ~/.chottag.
    Returns (as given, resolved), or None when unset or unusable."""
    raw = os.environ.get("CHOTTAG_HOME", "")
    if not raw:
        return None
    given = os.path.expanduser(raw)
    if not os.path.isabs(given):
        return None
    resolved = os.path.realpath(given)
    if resolved in (REAL_HOME, os.sep):
        return None
    return given.rstrip(os.sep) or os.sep, resolved


def deny(reason, tag="R77"):
    json.dump(
        {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "deny",
                "permissionDecisionReason": "guard-home (%s): %s" % (tag, reason),
            }
        },
        sys.stdout,
    )
    sys.exit(0)


def home_prefixes():
    h = re.escape(REAL_HOME)
    return r"(?:~|\$HOME|\$\{HOME\}|" + h + r")/"


def sandboxed_home(cmd):
    """True when the command sets HOME= to something other than the real home."""
    for m in re.finditer(r"(?:^|[\s;&|(])HOME=(\"[^\"]*\"|'[^']*'|\S+)", cmd):
        val = m.group(1).strip("\"'")
        if not val or val in ("~", "$HOME", "${HOME}"):
            continue
        if os.path.realpath(os.path.expanduser(val)) != REAL_HOME:
            return True
    return False


# CHOTTAG_READONLY_FIRST_WORDS: the invocation's first argument word alone
# (no other rule needed) marks it read-only (R91).
CHOTTAG_READONLY_FIRST_WORDS = {
    "version",
    "help",
    "-h",
    "--help",
    "status",
    "ls",
    "statusline",
}
# CHOTTAG_READONLY_BARE: these, with no further word, are read-only.
CHOTTAG_READONLY_BARE = {"remote", "auto", "notify", "trace"}

# WRAPPER_WORDS: a command word that just runs what follows it (fix round
# 1, item 3): skipped, along with any NAME=value assignment, at the front
# of a segment's tokens, in any order and repeated, before deciding what
# the real command word is.
WRAPPER_WORDS = {"env", "command", "exec", "nohup", "time", "sudo", "xargs"}
ASSIGNMENT_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
SHELLS_WITH_DASH_C = {"sh", "bash", "zsh", "dash"}

# SEGMENT_BOUNDARIES: characters that split one Bash command line into
# independent simple commands (fix round 1, item 1): ;, &&, ||, &, |,
# newline (already one string thanks to strip_heredocs' caller), and a
# subshell/command-substitution's ( ) or backtick pair. Each character is
# its own boundary -- "&&" is just two adjacent "&" boundaries, which
# split off an empty (dropped) segment between them -- so no multi-char
# lookahead is needed.
SEGMENT_BOUNDARIES = set(";&|()`\n")


def split_segments(cmd):
    """cmd, split into simple-command segments at SEGMENT_BOUNDARIES,
    quote-aware: a boundary character inside '...' or "..." is not a split
    point (so `grep -rn "chottag tag" docs/`, with no boundary character
    outside or inside its quotes, stays one segment). Empty segments
    (adjacent boundaries, leading/trailing ones) are dropped."""
    segments, buf = [], []
    in_squote = in_dquote = False
    for c in cmd:
        if in_squote:
            buf.append(c)
            if c == "'":
                in_squote = False
            continue
        if in_dquote:
            buf.append(c)
            if c == '"':
                in_dquote = False
            continue
        if c == "'":
            in_squote = True
            buf.append(c)
        elif c == '"':
            in_dquote = True
            buf.append(c)
        elif c in SEGMENT_BOUNDARIES:
            segments.append("".join(buf))
            buf = []
        else:
            buf.append(c)
    segments.append("".join(buf))
    return [s.strip() for s in segments if s.strip()]


def tokenize(segment):
    """segment's words, shell-quote-aware (shlex, posix mode); falls back
    to a plain whitespace split on anything shlex can't parse (an
    unbalanced quote), which is still enough to see the first word."""
    try:
        return shlex.split(segment, posix=True)
    except ValueError:
        return segment.split()


def strip_leading(tokens):
    """The NAME=value assignments (env's own included) and WRAPPER_WORDS
    at the front of tokens, in any order, repeated -- e.g. `env FOO=1
    chottag next` and `sudo chottag next` both reach `chottag` as the
    real command word. Returns (assignments dict, the remaining tokens);
    remaining[0], if any, is that real command word."""
    assignments = {}
    i = 0
    while i < len(tokens):
        t = tokens[i]
        m = ASSIGNMENT_RE.match(t)
        if m:
            name, _, val = t.partition("=")
            assignments[name] = val
            i += 1
            continue
        if t in WRAPPER_WORDS:
            i += 1
            continue
        break
    return assignments, tokens[i:]


def is_dev_env_or_release_token(tok):
    """True when tok is how a segment invokes scripts/dev-env or
    scripts/release: those set their own env, and this hook only ever
    sees their own command line (fix round 1, item 1: only THAT segment
    is exempt, not the whole command)."""
    for name in ("dev-env", "release"):
        target = "scripts/" + name
        if tok in (target, "./" + target) or tok.endswith("/" + target):
            return True
    return False


def chottag_invocation_args(remaining):
    """None when `remaining` (a segment's tokens, past its leading
    assignments/wrappers) is not a chottag invocation; else the argument
    tokens after it. An invocation is remaining[0]'s basename being
    exactly "chottag" (./chottag, ~/.chottag/bin/chottag, an absolute
    path), or `go run <path> ...` whose path's last element is exactly
    "chottag" (go run's own flags before the path are skipped) -- fix
    round 1, item 3: a basename/path-element check, never a substring
    match, so `chottag-dev`, `.../chottag/...` as a mere argument, and
    `go build -o .../chottag ...` (not `go run`) all read as NOT chottag."""
    if not remaining:
        return None
    if os.path.basename(remaining[0].rstrip("/")) == "chottag":
        return remaining[1:]
    if remaining[0] == "go" and len(remaining) >= 2 and remaining[1] == "run":
        i = 2
        while i < len(remaining) and remaining[i].startswith("-"):
            i += 1
        if i < len(remaining) and os.path.basename(remaining[i].rstrip("/")) == "chottag":
            return remaining[i + 1 :]
    return None


def expand_real_home_refs(val):
    """val with a leading ~, or any $HOME/${HOME} reference, replaced by
    the real home (fix round 2, item 1: comparing an unexpanded value let
    CHOTTAG_HOME=$HOME/.chottag masquerade as a dev override). None if val
    still contains a $ or a backtick afterwards -- an unresolved variable
    or a command substitution this can't safely resolve -- so it is
    treated as NOT a home-relative value at all (fail-safe)."""
    if val == "~" or val.startswith("~/"):
        val = REAL_HOME + val[1:]
    val = val.replace("${HOME}", REAL_HOME).replace("$HOME", REAL_HOME)
    if "$" in val or "`" in val:
        return None
    return val


def chottag_home_exempts(assignments):
    """True when THIS segment's own leading assignments (fix round 1,
    item 1) set CHOTTAG_HOME= to a path whose realpath is not the real,
    prod ~/.chottag, once ~/$HOME/${HOME} are expanded (fix round 2,
    item 1)."""
    val = assignments.get("CHOTTAG_HOME")
    if not val:
        return False
    expanded = expand_real_home_refs(val)
    if expanded is None:
        return False
    prod = os.path.realpath(os.path.join(REAL_HOME, ".chottag"))
    return os.path.realpath(expanded) != prod


def chottag_is_readonly(args, installed=False):
    """R91's read-only forms, kept short and exact, once "--json" (a
    global output flag most commands accept, not a read-only marker by
    itself -- fix round 1, item 2) is dropped from args. Anything else,
    or any doubt, is NOT read-only (fail-safe: deny)."""
    if not args:
        return True  # bare `chottag`
    words = [w for w in args if w != "--json"]
    if not words:
        return True  # bare `chottag --json`
    first = words[0]
    if first in CHOTTAG_READONLY_FIRST_WORDS:
        return True
    if first == "doctor":
        return "--fix" not in words
    if first in CHOTTAG_READONLY_BARE and len(words) == 1:
        return True
    if first == "trace" and len(words) >= 2 and words[1] == "summarize":
        return True
    if first == "daemon" and len(words) >= 2 and words[1] == "logs":
        return True
    if first == "update":
        # Fix round 2, item 2: read-only only for exactly `update --check`
        # -- --check plus another flag (e.g. --restart) still installs.
        # R123: prod is updated with `chottag update` like any install, so
        # a bare `update` and `update --version vX.Y.Z` are allowed too
        # (the product backs up state.json and the rc first). --restart,
        # --repo and everything else stay denied.
        # The mutating forms only run the INSTALLED chottag (`chottag` on
        # PATH or ~/.chottag/bin/chottag), never a dev build, an unstamped
        # `go run` or an arbitrary path: those report "dev", which is
        # always older than any release. --check is harmless from any path.
        if words == ["update", "--check"]:
            return True
        if not installed:
            return False
        if words == ["update"]:
            return True
        return (len(words) == 3 and words[1] == "--version"
                and re.fullmatch(r"v\d+\.\d+\.\d+", words[2]) is not None)
    if first == "own" and len(words) == 3:
        return True
    if first == "rotate" and len(words) == 2:
        return True
    return False


def is_installed_chottag(tok):
    """True when tok is exactly `chottag` (resolved from PATH) or the
    installed ~/.chottag/bin/chottag (spelled with ~, $HOME or the real
    home)."""
    if tok == "chottag":
        return True
    expanded = expand_real_home_refs(tok)
    if expanded is None:
        return False
    return os.path.normpath(expanded) == os.path.join(REAL_HOME, ".chottag", "bin", "chottag")


def segment_denies(segment):
    """True when this one simple command is a chottag invocation R91
    denies: not read-only, its own leading assignments don't point
    CHOTTAG_HOME= off prod, and it isn't a scripts/dev-env or
    scripts/release invocation. `sh|bash|zsh|dash -c STRING` recurses on
    STRING (its own segments, evaluated the same way)."""
    remaining = tokenize(segment)
    if not remaining:
        return False
    assignments, remaining = strip_leading(remaining)
    if not remaining:
        return False
    if is_dev_env_or_release_token(remaining[0]):
        return False
    if remaining[0] in SHELLS_WITH_DASH_C and len(remaining) >= 3 and remaining[1] == "-c":
        return cmd_denies(remaining[2])
    args = chottag_invocation_args(remaining)
    if args is None:
        return False
    if chottag_home_exempts(assignments):
        return False
    return not chottag_is_readonly(args, installed=is_installed_chottag(remaining[0]))


def cmd_denies(cmd):
    return any(segment_denies(seg) for seg in split_segments(cmd))


def deny_chottag_mutation(cmd):
    """R91: this repo's sessions never change the prod chottag. Denies a
    command whose any simple command (segment) is a chottag invocation
    that is neither read-only, nor pointed at a non-prod CHOTTAG_HOME,
    nor run through scripts/dev-env or scripts/release (which manage
    their own env)."""
    if cmd_denies(cmd):
        deny(
            "this repo's sessions never change the prod chottag "
            "(~/.chottag). Use scripts/dev-env run ... for the dev "
            "sandbox, or manage prod from a session outside this repo.",
            tag="R91",
        )


def strip_heredocs(cmd):
    """Drop heredoc bodies: text fed to a command's stdin is data, not a command."""
    out, lines, i = [], cmd.split("\n"), 0
    while i < len(lines):
        line = lines[i]
        out.append(line)
        m = re.search(r"<<-?\s*[\"']?(\w+)[\"']?", line)
        i += 1
        if m:
            end = m.group(1)
            while i < len(lines) and lines[i].strip() != end:
                i += 1
            i += 1
    return "\n".join(out)


def check_bash(cmd):
    if not isinstance(cmd, str):
        return
    cmd = strip_heredocs(cmd)
    deny_chottag_mutation(cmd)
    runs_installer = re.search(r"(?:^|[\s/;&|(])install\.sh\b", cmd) and re.search(
        r"(?:\b(?:sh|bash|dash|zsh)\b[^;&|]*install\.sh|\./install\.sh|/install\.sh\b|\|\s*(?:sh|bash|dash|zsh)\b)",
        cmd,
    )
    runs_setup = re.search(r"(?:^|[\s/;&|(])chottag\s+(?:setup|uninstall|install)\b", cmd)
    if (runs_installer or runs_setup) and not sandboxed_home(cmd):
        deny(
            "installer/setup command without a sandbox HOME=. Run it only as "
            "HOME=<sandbox> CHOTTAG_HOME=<sandbox>/... inside a test home."
        )
    prot = home_prefixes() + r"(?:" + "|".join(re.escape(p) for p in PROTECTED) + r")\b"
    # A protected path counts only as a write's target: a redirect into it, or
    # an argument of a writing verb within the same simple command. Reading
    # from it (a script under ~/.claude/plugins run with >/dev/null) is fine.
    redirect_into = r">>?\s*[\"']?" + prot
    verb_on = (
        r"\b(?:rm|mv|cp|ln|tee|touch|mkdir|chmod|chown|truncate|install|patch|dd|rsync|sed\s+-i)\b[^;&|\n]*" + prot
    )
    # No HOME= exemption here: ~ and $HOME expand in the calling shell,
    # before a HOME= prefix applies, so they still name the real home.
    # IGNORECASE: on macOS's default case-insensitive, case-preserving
    # filesystem, a differently-cased spelling (~/.CHOTTAG, ~/.ZSHRC, …)
    # names the same file. Matching case-insensitively on every platform
    # (not just Darwin) denies a little more on a case-sensitive filesystem,
    # which is fail-safe, and keeps this check's behaviour, and its tests,
    # platform-independent.
    if re.search(redirect_into, cmd, re.IGNORECASE) or re.search(verb_on, cmd, re.IGNORECASE):
        deny("write-shaped command naming a protected Home path (~/.zshrc, ~/.chottag, ~/.claude*).")
    ch = chottag_home()
    if ch:
        # A symlinked $CHOTTAG_HOME can still be renamed by a command that
        # spells the symlink target's own raw path (neither `given` nor
        # `resolved`) — a known, pre-existing limitation shared with $HOME
        # (see check_path's PROTECTED loop); not fixed here.
        names = [r"\$CHOTTAG_HOME\b", r"\$\{CHOTTAG_HOME\}"] + [re.escape(p) + r"(?![\w.-])" for p in sorted(set(ch))]
        target = r"[\"']?(?:" + "|".join(names) + r")"
        if re.search(r">>?\s*" + target, cmd, re.IGNORECASE) or re.search(
            r"\b(?:rm|mv|cp|ln|tee|touch|mkdir|chmod|chown|truncate|install|patch|dd|rsync|sed\s+-i)\b[^;&|\n]*" + target,
            cmd,
            re.IGNORECASE,
        ):
            deny("write-shaped command naming this session's $CHOTTAG_HOME.")


# The memory-dir exemption folds case only on macOS, whose default
# filesystem does: there a differently-cased spelling names the memory dir
# itself. Elsewhere it is a different directory inside the protected tree,
# so it stays protected (F230). The protected paths themselves are always
# matched case-insensitively, which only ever denies more.
MEMORY_FOLDS_CASE = sys.platform == "darwin"


def under(p, base, fold=True):
    """True when p is base, or a path under it. fold compares
    case-insensitively: os.path.realpath does not fold case, but macOS's
    default filesystem is case-insensitive, so a differently-cased spelling
    names the same file."""
    if fold:
        p, base = p.casefold(), base.casefold()
    return p == base or p.startswith(base + os.sep)


def check_path(path):
    if not path:
        return
    if not isinstance(path, str):
        return
    p = os.path.realpath(os.path.expanduser(path))
    md = memory_dir()
    if md and under(p, md, fold=MEMORY_FOLDS_CASE):
        return
    ch = chottag_home()
    if ch and under(p, ch[1]):
        deny("file write under this session's $CHOTTAG_HOME: " + path)
    for name in PROTECTED:
        base = os.path.join(REAL_HOME, name)
        if under(p, base):
            deny("file write under a protected Home path: " + path)


def main():
    try:
        data = json.load(sys.stdin)
    except Exception:
        return
    if not isinstance(data, dict):
        return
    tool = data.get("tool_name", "")
    inp = data.get("tool_input")
    if not isinstance(inp, dict):
        inp = {}
    if tool == "Bash":
        check_bash(inp.get("command", ""))
    elif tool in ("Write", "Edit", "MultiEdit", "NotebookEdit"):
        check_path(inp.get("file_path") or inp.get("notebook_path"))


if __name__ == "__main__":
    main()
