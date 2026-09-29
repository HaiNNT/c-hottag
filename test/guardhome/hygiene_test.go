package guardhome

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoCompiledPythonIsTracked: a .pyc embeds the absolute path of the
// machine that compiled it (F223), so none may be tracked.
func TestNoCompiledPythonIsTracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command("git", "-c", "core.quotePath=false", "ls-files", "-z")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Skip("not a git work tree (an exported snapshot)")
	}
	for _, f := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(f, ".pyc") || strings.Contains("/"+f, "/__pycache__/") {
			t.Errorf("%s is tracked: git rm it (a .pyc embeds the compiling machine's home path)", f)
		}
	}
}

// TestGitignoreKeepsBuildAndLocalFilesOut: __pycache__/ anywhere, and the
// owner's untracked CLAUDE.local.md (P2-R4), never reach a commit.
func TestGitignoreKeepsBuildAndLocalFilesOut(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, want := range []string{"__pycache__/", "CLAUDE.local.md"} {
		if !lines[want] {
			t.Errorf(".gitignore lacks the line %q", want)
		}
	}
}
