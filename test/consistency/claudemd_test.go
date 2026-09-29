package consistency

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateRepo is the development repo's name, joined at run time so this file
// never trips a private leak pattern that lists it.
var privateRepo = "c-hottag-" + "private"

// TestClaudeMDIsForContributors pins P2-R4: the public CLAUDE.md carries
// the layout, the gate, the safety rules and the doc-keeping rules a
// contributor or their agent needs, and none of the owner's own rules.
func TestClaudeMDIsForContributors(t *testing.T) {
	md := read(t, "CLAUDE.md")
	for _, want := range []string{
		"## Layout", "## Rules", "## Keeping docs true", "The gate",
		"Never write to Home", "Never copy a token out of a", "t.TempDir()", "47821",
		"guard-home", "scripts/leak-scan", "public-manifest.txt", "CLAUDE.local.md",
		"chottag@c-hottag", "docs/release-notes/",
		"CONTRIBUTING.md", "by invitation only", "CHANGELOG.md",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("CLAUDE.md lacks %q", want)
		}
	}
	for _, owner := range []string{
		"R77", "`coder` agent", "cmux", "Team account", privateRepo,
		"requirements-log", "superpowers", "Findings log", "The user pushes",
	} {
		if strings.Contains(md, owner) {
			t.Errorf("CLAUDE.md carries the owner's rule %q: it belongs in docs/internal/owner-claude.md", owner)
		}
	}
}

// TestOwnerRulesLiveInTheImportedFile: in the private repo, the rules the
// public CLAUDE.md dropped are in docs/internal/owner-claude.md, which the
// untracked CLAUDE.local.md imports. A public snapshot has no docs/internal/.
func TestOwnerRulesLiveInTheImportedFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), "docs", "internal", "owner-claude.md"))
	if errors.Is(err, fs.ErrNotExist) {
		if privateRepoMarker(t) {
			t.Fatal("docs/internal/ exists but docs/internal/owner-claude.md does not (M3): the owner's rules would be silently missing")
		}
		t.Skip("no docs/internal/owner-claude.md: a public snapshot")
	}
	if err != nil {
		t.Fatal(err)
	}
	owner := string(b)
	for _, want := range []string{
		"@docs/internal/owner-claude.md", "Read first", "Keep the record current",
		"Autonomy (R77)", "`coder` agent", "cmux", "Team account", privateRepo,
		"The user pushes", "docs/internal/superpowers/plans/", "docs/internal/leak-patterns.txt",
	} {
		if !strings.Contains(owner, want) {
			t.Errorf("docs/internal/owner-claude.md lacks %q", want)
		}
	}
}
