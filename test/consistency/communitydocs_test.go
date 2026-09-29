package consistency

import (
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// TestCommunityFAQPointsAtTheTerms: "Is this allowed?" sends the reader to
// the README's terms section, which no longer reassures (P4-R8).
func TestCommunityFAQPointsAtTheTerms(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	requirePhrases(t, "docs/faq.md",
		"https://github.com/"+repo+"/blob/main/README.md#terms-of-use-and-risk",
		"decide for yourself", "fall outside", "ordinary, individual usage", "without prior notice",
		"every account linked to the same install", "through the use of a different account",
		"developers may not collect, store, or intermediate",
		"https://claude.ai/restricted",
	)
	// Anthropic's error reference sends a held account to its review and
	// appeal page; it never says to keep working on another account, and the
	// FAQ must not say so either.
	if strings.Contains(flat(read(t, "docs/faq.md")), "with another account") {
		t.Error("docs/faq.md tells a held account to keep working on another account; Anthropic's error reference says to see claude.ai/restricted and wait")
	}
	s, ok := doccheck.Find(read(t, "docs/faq.md"), 2, "Is this allowed?")
	if !ok {
		t.Fatal("docs/faq.md has no ## Is this allowed?")
	}
	if flat(s.Body) == "" {
		t.Error("docs/faq.md ## Is this allowed? is empty")
	}
}

// TestCommunityDocsIndexLinksTheProjectFiles: the docs home reaches the
// community files, by absolute URL since they sit outside docs/ (P3-R12).
func TestCommunityDocsIndexLinksTheProjectFiles(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	linked := map[string]bool{}
	for _, k := range doccheck.Links(read(t, "docs/index.md")) {
		linked[k.Target] = true
	}
	for _, f := range []string{
		"CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SUPPORT.md", "ROADMAP.md",
		"CHANGELOG.md", "README.md#terms-of-use-and-risk",
	} {
		if u := "https://github.com/" + repo + "/blob/main/" + f; !linked[u] {
			t.Errorf("docs/index.md does not link %s", u)
		}
	}
}
