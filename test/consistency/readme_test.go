package consistency

import (
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// TestREADMEIsALandingPage (spec §5): at most 150 lines, a text demo, six
// feature bullets, and links into the docs.
func TestREADMEIsALandingPage(t *testing.T) {
	readme := read(t, "README.md")
	if n := strings.Count(readme, "\n"); n > 150 {
		t.Errorf("README.md has %d lines, want at most 150: move detail into docs/", n)
	}
	demo := false
	for _, b := range doccheck.Fenced(readme, "text") {
		if strings.Contains(b, "$ chottag status") && strings.Contains(b, "serving:") && strings.Contains(b, "NAME") {
			demo = true
		}
	}
	if !demo {
		t.Error("README.md has no ```text demo of `chottag status` (P3-R4: a transcript, not a GIF)")
	}
	s, ok := doccheck.Find(readme, 2, "What it does")
	if !ok {
		t.Fatal("README.md has no ## What it does")
	}
	bullets := 0
	for _, l := range strings.Split(s.Lead, "\n") {
		if strings.HasPrefix(l, "- ") {
			bullets++
		}
	}
	if bullets != 6 {
		t.Errorf("## What it does has %d bullets, want 6", bullets)
	}
	linked := map[string]bool{}
	for _, k := range doccheck.Links(readme) {
		linked[k.Target] = true
	}
	for _, want := range []string{
		"docs/index.md", "docs/getting-started.md", "docs/commands.md",
		"docs/troubleshooting.md", "docs/comparison.md", "SECURITY.md",
		"CONTRIBUTING.md", "SUPPORT.md", "ROADMAP.md", "CHANGELOG.md",
	} {
		if !linked[want] {
			t.Errorf("README.md does not link %s", want)
		}
	}
}
