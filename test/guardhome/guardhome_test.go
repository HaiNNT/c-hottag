// Package guardhome runs the committed .claude/hooks/guard-home.py hook under
// python3 with synthetic PreToolUse payloads. It never touches the real
// HOME: the child's HOME is always a t.TempDir().
package guardhome

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func hook(t *testing.T) string {
	p, _ := filepath.Abs(filepath.Join("..", "..", ".claude", "hooks", "guard-home.py"))
	return p
}

// run feeds one PreToolUse event to the hook and reports whether it denied.
// extraEnv (KEY=VALUE) is added to the hook's environment, which stands for
// the Claude Code session's own environment.
func run(t *testing.T, home, projectDir string, event map[string]any, extraEnv ...string) bool {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	in, _ := json.Marshal(event)
	cmd := exec.Command(py, hook(t))
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	if projectDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_PROJECT_DIR="+projectDir)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(string(in))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hook failed: %v", err)
	}
	return strings.Contains(string(out), `"permissionDecision": "deny"`)
}

func bash(c string) map[string]any {
	return map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": c}}
}
func write(p string) map[string]any {
	return map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": p}}
}

func TestGuardHomeKeepsItsRules(t *testing.T) {
	home := t.TempDir()
	proj := "/work/my.proj"
	cases := []struct {
		name  string
		event map[string]any
		deny  bool
	}{
		{"installer", bash("./install.sh"), true},
		{"installer in a sandbox HOME", bash("HOME=/tmp/sb ./install.sh"), false},
		{"setup", bash("chottag setup"), true},
		{"setup in a sandbox HOME", bash("HOME=/tmp/sb CHOTTAG_HOME=/tmp/sb/c chottag setup"), false},
		{"redirect into zshrc", bash("echo x >> ~/.zshrc"), true},
		{"read zshrc", bash("cat ~/.zshrc"), false},
		{"rm chottag home", bash("rm -rf ~/.chottag"), true},
		{"patch claude", bash("patch ~/.claude/settings.json < p.diff"), true},
		{"heredoc body is data", bash("cat <<EOF\necho x > ~/.zshrc\nEOF"), false},
		{"write claude settings", write(filepath.Join(home, ".claude", "settings.json")), true},
		{"write this project's memory", write(filepath.Join(home, ".claude", "projects", "-work-my-proj", "memory", "x.md")), false},
		{"write another project's memory", write(filepath.Join(home, ".claude", "projects", "-work-other", "memory", "x.md")), true},
		{"write elsewhere", write(filepath.Join(t.TempDir(), "x")), false},
	}
	for _, c := range cases {
		if got := run(t, home, proj, c.event); got != c.deny {
			t.Errorf("%s: deny = %v, want %v", c.name, got, c.deny)
		}
	}
	if run(t, home, "", write(filepath.Join(home, ".claude", "projects", "-work-my-proj", "memory", "x.md"))) != true {
		t.Error("without CLAUDE_PROJECT_DIR there is no memory exemption")
	}
}

// TestGuardHomeFailsSafeOnMalformedInput: a hook that crashes on unexpected
// input exits non-zero (but not 2), which Claude Code treats as a
// *non-blocking* error: the tool call then goes through unvetted. Malformed
// JSON, a missing CLAUDE_PROJECT_DIR, a non-dict tool_input and a
// non-string command/file_path must all exit 0, print no traceback, and
// never deny.
func TestGuardHomeFailsSafeOnMalformedInput(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	home := t.TempDir()
	cases := []string{
		"",
		"not json",
		"{",
		`{"tool_name": "Bash"}`,
		`null`,
		`{"tool_name": "Bash", "tool_input": "oops"}`,
		`{"tool_name": "Bash", "tool_input": [1]}`,
		`{"tool_name": "Bash", "tool_input": 5}`,
		`{"tool_name": "Bash", "tool_input": {"command": ["a", "b"]}}`,
		`{"tool_name": "Write", "tool_input": {"file_path": ["x"]}}`,
	}
	for _, in := range cases {
		cmd := exec.Command(py, hook(t))
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		cmd.Stdin = strings.NewReader(in)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("input %q: hook exited non-zero: %v (stderr: %s)", in, err, stderr.String())
			continue
		}
		if stderr.Len() != 0 {
			t.Errorf("input %q: hook wrote to stderr: %s", in, stderr.String())
		}
		if strings.Contains(string(out), `"permissionDecision": "deny"`) {
			t.Errorf("input %q: hook denied on malformed/incomplete input", in)
		}
	}
}

// TestGuardHomeDenyStillExitsZero: the fix must not change the deny path
// (deny reports its decision via stdout JSON, exit 0 — a non-zero or
// exit-2 exit here would be a regression the harness could treat
// differently). A crashing hook is the bug this fix round closes, not
// the deny signal itself.
func TestGuardHomeDenyStillExitsZero(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	home := t.TempDir()
	cmd := exec.Command(py, hook(t))
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(`{"tool_name": "Bash", "tool_input": {"command": "./install.sh"}}`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("deny case exited non-zero: %v", err)
	}
	if !strings.Contains(string(out), `"permissionDecision": "deny"`) {
		t.Fatalf("expected a deny decision, got %s", out)
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Errorf("deny exit code = %d, want 0", cmd.ProcessState.ExitCode())
	}
}

// TestGuardHomeProtectsTheSessionsChottagHome: when the session's own
// environment sets CHOTTAG_HOME (an install outside ~/.chottag), that tree
// is protected exactly like ~/.chottag; reads stay allowed, and an unset,
// relative, "/" or real-home CHOTTAG_HOME protects nothing extra.
func TestGuardHomeProtectsTheSessionsChottagHome(t *testing.T) {
	home := t.TempDir()
	ch := t.TempDir()
	set := "CHOTTAG_HOME=" + ch
	cases := []struct {
		name  string
		event map[string]any
		env   []string
		deny  bool
	}{
		{"write under it", write(filepath.Join(ch, "state.json")), []string{set}, true},
		{"rm it by path", bash("rm -rf " + ch), []string{set}, true},
		{"rm under it by variable", bash(`rm -rf "$CHOTTAG_HOME"/accounts`), []string{set}, true},
		{"redirect into it", bash("echo x > " + filepath.Join(ch, "state.json")), []string{set}, true},
		{"read it", bash("cat " + filepath.Join(ch, "state.json")), []string{set}, false},
		{"list it by variable", bash("ls ${CHOTTAG_HOME}"), []string{set}, false},
		{"a sibling with a longer name", bash("rm -rf " + ch + "x"), []string{set}, false},
		{"write elsewhere", write(filepath.Join(t.TempDir(), "x")), []string{set}, false},
		{"unset: a temp dir is just a temp dir", write(filepath.Join(ch, "state.json")), nil, false},
		{"relative: ignored", bash("rm -rf rel"), []string{"CHOTTAG_HOME=rel"}, false},
		{"the real home: no blanket block", write(filepath.Join(home, "notes.txt")), []string{"CHOTTAG_HOME=" + home}, false},
		{"root: no blanket block", write(filepath.Join(t.TempDir(), "x")), []string{"CHOTTAG_HOME=/"}, false},
	}
	for _, c := range cases {
		if got := run(t, home, "", c.event, c.env...); got != c.deny {
			t.Errorf("%s: deny = %v, want %v", c.name, got, c.deny)
		}
	}
}

// TestGuardHomeMatchesProtectedPathsCaseInsensitively: on macOS's default
// case-insensitive, case-preserving filesystem, a differently-cased spelling
// of a protected path (~/.CLAUDE, ~/.Claude.Json, ~/.ZSHRC, ~/.CHOTTAG, or a
// differently-cased $CHOTTAG_HOME) names the identical file, so it must be
// denied exactly like the canonical spelling — on every platform, since
// denying more on a case-sensitive filesystem (Linux) is fail-safe and keeps
// this part of the test platform-independent. The project-memory exemption
// folds case only on macOS (F230): there a differently-cased spelling is the
// memory dir itself and is allowed; elsewhere it is another directory inside
// ~/.claude and is denied. A sibling project's memory dir is denied on both.
func TestGuardHomeMatchesProtectedPathsCaseInsensitively(t *testing.T) {
	home := t.TempDir()
	proj := "/work/my.proj"
	ch := t.TempDir()
	set := "CHOTTAG_HOME=" + ch
	cases := []struct {
		name  string
		event map[string]any
		env   []string
		deny  bool
	}{
		{"write ~/.CLAUDE differently cased", write(filepath.Join(home, ".CLAUDE", "settings.json")), nil, true},
		{"write ~/.Claude.Json differently cased", write(filepath.Join(home, ".Claude.Json")), nil, true},
		{"bash redirect into ~/.ZSHRC differently cased", bash("echo x >> ~/.ZSHRC"), nil, true},
		{"bash rm ~/.CHOTTAG differently cased", bash("rm -rf ~/.CHOTTAG"), nil, true},
		{"write CHOTTAG_HOME differently cased", write(filepath.Join(strings.ToUpper(ch), "state.json")), []string{set}, true},
		{"bash rm CHOTTAG_HOME differently cased", bash("rm -rf " + strings.ToUpper(ch)), []string{set}, true},
		{"write this project's memory, differently cased", write(filepath.Join(home, ".Claude", "Projects", "-work-my-proj", "Memory", "x.md")), nil, runtime.GOOS != "darwin"},
		{"write a MEMORY-cased sibling in this project's dir", write(filepath.Join(home, ".claude", "projects", "-work-my-proj", "MEMORY", "x.md")), nil, runtime.GOOS != "darwin"},
		{"write a sibling project's memory, differently cased", write(filepath.Join(home, ".Claude", "Projects", "-work-other", "Memory", "x.md")), nil, true},
	}
	for _, c := range cases {
		if got := run(t, home, proj, c.event, c.env...); got != c.deny {
			t.Errorf("%s: deny = %v, want %v", c.name, got, c.deny)
		}
	}
}

func TestGuardHomeHardcodesNoUserPath(t *testing.T) {
	b, _ := os.ReadFile(hook(t))
	for _, bad := range []string{"/Users/", "/home/", "-Users-"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("guard-home.py contains %q: derive it from $HOME and $CLAUDE_PROJECT_DIR (L7)", bad)
		}
	}
}

// TestGuardHomeKeepsProdChottagOffLimits (R91): a chottag invocation from
// this repo's sessions is denied unless it is one of the read-only forms,
// or its OWN simple command's leading CHOTTAG_HOME= points somewhere
// other than the real ~/.chottag, or that simple command runs
// scripts/dev-env or scripts/release. Each ;/&&/||/&/|/subshell-separated
// simple command decides on its own (fix round 1, item 1): an exemption
// or an unrelated command earlier in the line never covers a chottag
// invocation later in the same line. "real home" is the t.TempDir() HOME
// (see run), so CHOTTAG_HOME=~/.chottag names it too.
func TestGuardHomeKeepsProdChottagOffLimits(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name string
		cmd  string
		deny bool
	}{
		{"tag", "chottag tag B", true},
		{"names on sets the mode", "chottag names on", true},
		{"names off --json sets the mode", "chottag names off --json", true},
		{"absolute path daemon stop", "~/.chottag/bin/chottag daemon stop", true},
		{"relative path plan", "./chottag plan C max20x", true},
		{"go run next", "go run ./cmd/chottag next", true},
		{"doctor --fix", "chottag doctor --fix", true},
		{"rotate with a value", "chottag rotate A off", true},
		{"own with an explicit account", "chottag own artifact x1 B", true},
		{"update --restart still restarts prod's daemon", "chottag update --restart", true},
		{"update --repo is denied", "chottag update --repo X/y", true},
		{"update --version needs a vX.Y.Z tag", "chottag update --version latest", true},
		{"update --version with another flag", "chottag update --version v0.5.0 --restart", true},
		{"go run update is a dev build", "go run ./cmd/chottag update", true},
		{"./chottag update is not the installed one", "./chottag update", true},
		{"another path update --version", "/tmp/x/chottag update --version v9.9.9", true},
		{"uninstall on prod stays denied", "chottag uninstall", true},
		{"setup on prod stays denied", "chottag setup", true},
		{"login", "chottag login foo", true},
		{"CHOTTAG_HOME= the real prod home", "CHOTTAG_HOME=~/.chottag chottag tag B", true},
		{"chained after another command", "echo hi && chottag next", true},
		// Fix round 1, item 1: a dev-env/release exemption or a
		// CHOTTAG_HOME= override earlier in the line is per simple
		// command, not global.
		{"dev-env exemption doesn't leak to the next simple command", "scripts/dev-env status; chottag tag B", true},
		{"CHOTTAG_HOME= on an unrelated command doesn't leak either", "CHOTTAG_HOME=/tmp/x true; chottag next", true},
		// Fix round 1, item 2: --json is a global output flag, not a
		// read-only marker by itself.
		{"a mutating command with --json", "chottag tag B --json", true},
		// Fix round 1, item 3: an invocation is detected by command
		// position (tokenized), so it is found inside $(...), and a
		// wrapper word (env, sudo, ...) is skipped to find it.
		{"inside a command substitution", "echo $(chottag next)", true},
		{"through bash -c", `bash -c "chottag tag B"`, true},
		{"through env with an assignment", "env FOO=1 chottag next", true},
		{"through sudo", "sudo chottag next", true},
		// Fix round 2, item 1: compare CHOTTAG_HOME= after expanding a
		// leading ~ or a $HOME/${HOME} reference, not the raw text.
		{"CHOTTAG_HOME=$HOME/.chottag is still prod", "CHOTTAG_HOME=$HOME/.chottag chottag next", true},
		{"CHOTTAG_HOME=${HOME}/.chottag is still prod", "CHOTTAG_HOME=${HOME}/.chottag chottag next", true},
		{"CHOTTAG_HOME=$FOO is an unresolved var, not an override", "CHOTTAG_HOME=$FOO chottag next", true},
		// Fix round 2, item 2: --check plus another flag still installs.
		{"update --check --restart still installs", "chottag update --check --restart", true},

		{"status", "chottag status", false},
		{"statusline (read-only, R111)", "chottag statusline", false},
		{"ls alias", "chottag ls", false},
		{"doctor without --fix", "chottag doctor", false},
		{"version", "chottag version", false},
		{"bare remote", "chottag remote", false},
		{"rotate with no value", "chottag rotate A", false},
		{"own with no account (prints the owner)", "chottag own artifact x1", false},
		{"update --check", "chottag update --check", false},
		{"update (R123)", "chottag update", false},
		{"update via the installed path", "~/.chottag/bin/chottag update", false},
		{"update --check from any path", "./chottag update --check", false},
		{"update --json (R123)", "chottag update --json", false},
		{"update --version vX.Y.Z (R123)", "chottag update --version v0.5.0", false},
		{"update --version vX.Y.Z --json (R123)", "chottag update --version v0.5.0 --json", false},
		{"daemon logs", "chottag daemon logs -n 50", false},
		{"trace summarize", "chottag trace summarize", false},
		{"names (show)", "chottag names", false},
		{"names --json (show)", "chottag names --json", false},
		{"CHOTTAG_HOME= a dev sandbox", "CHOTTAG_HOME=/tmp/devx chottag tag B", false},
		{"scripts/dev-env run", "scripts/dev-env run tag B", false},
		{"scripts/release status", "scripts/release status", false},
		{"go test, not go run", "go test ./internal/cli/", false},
		{"git log --grep", "git log --grep chottag", false},
		// "chottag" here sits inside a quoted string after grep, so it is
		// never the first token of the (tokenized) command: allowed
		// without a special case for grep/quoting.
		{`grep for the literal words "chottag tag"`, `grep -rn "chottag tag" docs/`, false},
		// Fix round 1, item 3: "chottag" as a mere substring of a path or
		// argument -- never at command position -- is not an invocation.
		{"ls into the dev sandbox's own root", "ls " + filepath.Join(home, "chottag-dev", "bin"), false},
		{"rm on the dev sandbox's own root", "rm -rf /tmp/chottag-dev", false},
		{"go build, not go run, naming a chottag path", "go build -o /tmp/x/chottag ./cmd/chottag", false},
		{"read-only with --json", "chottag status --json", false},
		{"bare remote with --json", "chottag remote --json", false},
		{"CHOTTAG_HOME= a dev sandbox, next", "CHOTTAG_HOME=/tmp/devx chottag next", false},
		{"chottag as a quoted commit message word", `git commit -m "chottag next is fixed"`, false},
		// Fix round 2, item 1: $HOME/... resolving OUTSIDE ~/.chottag is
		// still a real dev override, once expanded. (Round 2's other
		// allow row, `chottag update --check` alone, is already the
		// "update --check" row above.) The path avoids a literal
		// slash-"home"-slash substring on purpose: leak-scan's generic
		// home-path pattern would otherwise false-positive on it, spelled
		// out in test source, the same way scripts/dev-env's own layout
		// comment once did.
		{"CHOTTAG_HOME=$HOME/... is a dev override", "CHOTTAG_HOME=$HOME/chottag-dev/sandbox/.chottag chottag next", false},
	}
	for _, c := range cases {
		if got := run(t, home, "", bash(c.cmd)); got != c.deny {
			t.Errorf("%s (%q): deny = %v, want %v", c.name, c.cmd, got, c.deny)
		}
	}
}
