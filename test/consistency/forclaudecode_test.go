package consistency

import (
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// TestForClaudeCodePageIsReachable (R100): the install-and-manage guide for
// an AI agent exists, README's "For Claude Code" section and the plugin
// skill both point at it, and it names the real no-clone install command.
func TestForClaudeCodePageIsReachable(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")

	page := read(t, "docs/for-claude-code.md")
	want := "repos/" + repo + "/contents/install.sh"
	if !strings.Contains(page, want) {
		t.Errorf("docs/for-claude-code.md lacks %q", want)
	}

	readme := read(t, "README.md")
	s, ok := doccheck.Find(readme, 2, "For Claude Code")
	if !ok {
		t.Fatal("README.md has no ## For Claude Code")
	}
	linksNewPage := false
	for _, k := range doccheck.Links(s.Lead) {
		if k.Target == "docs/for-claude-code.md" {
			linksNewPage = true
		}
	}
	if !linksNewPage {
		t.Error("README.md's ## For Claude Code does not link docs/for-claude-code.md")
	}

	skill := read(t, "plugin/skills/chottag/SKILL.md")
	if want := "https://github.com/" + repo + "/blob/main/docs/for-claude-code.md"; !strings.Contains(skill, want) {
		t.Errorf("SKILL.md lacks %q", want)
	}
}
