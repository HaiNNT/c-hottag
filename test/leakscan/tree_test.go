package leakscan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheManifestSelectedTreeHasNoLeaks runs what CI runs (P2-R6) over this
// repo's tracked, manifest-selected files, plus the owner's private
// patterns when docs/internal/leak-patterns.txt exists: the private repo
// only, since it is never exported. So every commit to the private repo is
// checked against the terms the export will check. A hit reports
// file:line: category, never the text. Outside a git work tree (an
// exported snapshot, already scanned by export-public) it skips.
func TestTheManifestSelectedTreeHasNoLeaks(t *testing.T) {
	r := root(t)
	files := tracked(t)
	sel, errb, code := script(t, r, strings.Join(files, "\n")+"\n", "manifest-select", filepath.Join(r, "public-manifest.txt"))
	if code != 0 || sel == "" {
		t.Fatalf("manifest-select: exit %d, %d bytes selected: %s", code, len(sel), errb)
	}
	args := []string{"-C", r}
	pats := filepath.Join(r, "docs", "internal", "leak-patterns.txt")
	switch {
	case fileExists(pats):
		args = append(args, "-p", pats)
	case privateRepoMarker(t):
		// M3: docs/internal/ exists but its own patterns file does not --
		// fail, don't silently scan with the generic patterns only.
		t.Fatalf("docs/internal/ exists but %s does not: the private patterns would be silently skipped", pats)
	}
	out, errb, code := script(t, r, sel, "leak-scan", args...)
	if code != 0 {
		t.Fatalf("leak-scan exit %d over the manifest-selected files (file:line: category; the text is never shown):\n%s%s", code, out, errb)
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
