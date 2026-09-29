package consistency

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// TestAbsoluteRepoLinksAnchorsResolve (fix round 1, T10 review): a docs page
// that links a repo file outside docs/ by its absolute
// https://github.com/<repo>/blob/main/<path>#<anchor> URL must name a real
// heading in that file, checked with the same GitHub slug rules
// doccheck.Anchors uses for relative links. TestDocsLinksStayInsideDocs
// already checks that <path> names a real file; this only adds the
// <anchor> half, which no existing test covers.
func TestAbsoluteRepoLinksAnchorsResolve(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	prefix := "https://github.com/" + repo + "/blob/main/"
	for _, page := range docPages(t) {
		for _, k := range doccheck.Links(read(t, page)) {
			rest, ok := strings.CutPrefix(k.Target, prefix)
			if !ok {
				continue
			}
			p, anchor, hasAnchor := strings.Cut(rest, "#")
			if !hasAnchor || !strings.HasSuffix(p, ".md") {
				continue
			}
			if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(p))); err != nil {
				continue // TestDocsLinksStayInsideDocs already reports this
			}
			if !doccheck.Anchors(read(t, p))[anchor] {
				t.Errorf("%s:%d links %s, but %s has no heading #%s", page, k.Line, k.Target, p, anchor)
			}
		}
	}
}
