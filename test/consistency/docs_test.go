package consistency

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/autoswitch"
	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// docsPackPages are the pages spec §5 asks for, besides docs/migrating/.
var docsPackPages = []string{
	"index", "getting-started", "how-it-works", "commands", "configuration",
	"auto-switch", "updating", "troubleshooting", "faq", "security",
	"comparison", "uninstall", "known-limitations",
}

// TestTheDocsPackIsComplete (spec §5): every page exists, and at least one
// migration guide.
func TestTheDocsPackIsComplete(t *testing.T) {
	for _, p := range docsPackPages {
		if _, err := os.Stat(filepath.Join(root(t), "docs", p+".md")); err != nil {
			t.Errorf("docs/%s.md: %v", p, err)
		}
	}
	if len(comparisonGuides(t)) == 0 {
		t.Error("docs/migrating/ holds no guide")
	}
}

// docsResolve returns the repo-relative slash path a relative link target
// in page points at, and its #anchor ("" when none).
func docsResolve(page, target string) (string, string) {
	file, anchor, _ := strings.Cut(target, "#")
	if file == "" {
		return page, anchor
	}
	return path.Clean(path.Join(path.Dir(page), file)), anchor
}

func docsIsURL(target string) bool {
	return strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:")
}

// TestTheIndexLinksEveryPage: docs/index.md reaches every docs page.
func TestTheIndexLinksEveryPage(t *testing.T) {
	linked := map[string]bool{}
	for _, k := range doccheck.Links(read(t, "docs/index.md")) {
		if !docsIsURL(k.Target) {
			p, _ := docsResolve("docs/index.md", k.Target)
			linked[p] = true
		}
	}
	for _, p := range docPages(t) {
		if p != "README.md" && p != "docs/index.md" && !linked[p] {
			t.Errorf("docs/index.md does not link %s", p)
		}
	}
}

// TestDocsLinksResolve: every relative link in README and the docs names a
// file that exists, and every #anchor a heading that exists.
func TestDocsLinksResolve(t *testing.T) {
	for _, page := range docPages(t) {
		for _, k := range doccheck.Links(read(t, page)) {
			if docsIsURL(k.Target) {
				continue
			}
			p, anchor := docsResolve(page, k.Target)
			if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(p))); err != nil {
				t.Errorf("%s:%d links %s, which does not exist", page, k.Line, k.Target)
				continue
			}
			if anchor != "" && strings.HasSuffix(p, ".md") && !doccheck.Anchors(read(t, p))[anchor] {
				t.Errorf("%s:%d links %s, but %s has no heading #%s", page, k.Line, k.Target, p, anchor)
			}
		}
	}
}

// TestDocsLinksStayInsideDocs (P3-R5): a docs page links a page inside
// docs/ relatively, and anything else in the repo by its absolute GitHub
// URL, which must name a real file; so the pages publish unchanged.
func TestDocsLinksStayInsideDocs(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	for _, page := range docPages(t) {
		if page == "README.md" {
			continue
		}
		for _, k := range doccheck.Links(read(t, page)) {
			for _, pre := range []string{"https://github.com/" + repo + "/blob/main/", "https://github.com/" + repo + "/tree/main/"} {
				if rest, ok := strings.CutPrefix(k.Target, pre); ok {
					rest, _, _ = strings.Cut(rest, "#")
					if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(rest))); err != nil {
						t.Errorf("%s:%d links %s, which names no file in this repo", page, k.Line, k.Target)
					}
				}
			}
			if docsIsURL(k.Target) {
				continue
			}
			if p, _ := docsResolve(page, k.Target); !strings.HasPrefix(p, "docs/") {
				t.Errorf("%s:%d links %s outside docs/; use https://github.com/%s/blob/main/%s", page, k.Line, k.Target, repo, p)
			}
		}
	}
}

// TestDocsHaveNoImagesOrBinaryMedia (P3-R4): no image link, and no image,
// video or cast file under docs/.
func TestDocsHaveNoImagesOrBinaryMedia(t *testing.T) {
	for _, page := range docPages(t) {
		for _, k := range doccheck.Links(read(t, page)) {
			if k.Image {
				t.Errorf("%s:%d shows an image (%s); use a Mermaid or ASCII diagram, or a text transcript", page, k.Line, k.Target)
			}
		}
	}
	media := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".webp": true, ".cast": true, ".mp4": true, ".mov": true, ".webm": true, ".pdf": true}
	err := filepath.WalkDir(filepath.Join(root(t), "docs"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "internal" {
			return filepath.SkipDir
		}
		if !d.IsDir() && media[strings.ToLower(filepath.Ext(p))] {
			t.Errorf("%s: no binary media in docs/ (P3-R4)", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDocsCarryTheDisclaimer (spec §0, R84): the landing page and the docs
// home say the project is not Anthropic's.
func TestDocsCarryTheDisclaimer(t *testing.T) {
	for _, p := range []string{"README.md", "docs/index.md"} {
		requirePhrases(t, p, "not affiliated with or endorsed by Anthropic")
	}
}

// TestSkillLinksTheCommandReference (spec §5): the skill stays the manual
// for Claude Code and points at the full reference by its absolute URL,
// since an installed plugin has no docs/ beside it.
func TestSkillLinksTheCommandReference(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	requirePhrases(t, "plugin/skills/chottag/SKILL.md", "https://github.com/"+repo+"/blob/main/docs/commands.md")
}

// TestPagesThatNameTheHomeTreeMentionChottagHome (Review Focus 3): a page
// that gives a ~/.chottag path also says CHOTTAG_HOME can move it.
func TestPagesThatNameTheHomeTreeMentionChottagHome(t *testing.T) {
	for _, p := range docPages(t) {
		md := read(t, p)
		if strings.Contains(md, "~/.chottag") && !strings.Contains(md, "CHOTTAG_HOME") {
			t.Errorf("%s names ~/.chottag but never CHOTTAG_HOME", p)
		}
	}
}

// findSwitchPoints appends every object found under a "switchPoints" key,
// anywhere in v, to *found.
func findSwitchPoints(v any, found *[]map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			if k == "switchPoints" {
				if m, ok := vv.(map[string]any); ok {
					*found = append(*found, m)
				}
			}
			findSwitchPoints(vv, found)
		}
	case []any:
		for _, vv := range x {
			findSwitchPoints(vv, found)
		}
	}
}

// TestDocsSwitchPointKeysAreReal (final review item 2): every key inside a
// "switchPoints" object in a docs JSON example is one of
// autoswitch.SettingKeys' window.tier keys — a key like "sonnet" or "5h"
// alone is silently dropped by chottag, never applied (autoswitch.Params.With).
func TestDocsSwitchPointKeysAreReal(t *testing.T) {
	valid := map[string]bool{}
	for _, k := range autoswitch.SettingKeys() {
		if strings.Contains(k, ".") {
			valid[k] = true
		}
	}
	found := 0
	for _, page := range docPages(t) {
		for _, b := range doccheck.Fenced(read(t, page), "json") {
			var v any
			if err := json.Unmarshal([]byte(b), &v); err != nil {
				continue
			}
			var sps []map[string]any
			findSwitchPoints(v, &sps)
			for _, sp := range sps {
				found++
				for k := range sp {
					if !valid[k] {
						t.Errorf("%s: a switchPoints example has key %q, which is not one of %v and would be silently dropped", page, k, sortedKeys(valid))
					}
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no switchPoints JSON example found in the docs to check")
	}
}

// docsJargon is the project's internal shorthand: requirement and finding
// numbers (R77, F221), milestone tags in parentheses ("(M0)"), spec section
// marks, a reference to this internal docs project by name ("docs pack"),
// and a source citation naming a Go file and line ("router.go:15"). A
// public page never needs them (docs review A6, final review item 12).
var docsJargon = regexp.MustCompile(`\b[RF][0-9]{2,3}\b|\(M[0-9][a-e]?\)|§|(?i:docs pack)|\.go:\d+`)

// TestDocsJargonCatchesDocsPackAndGoCitations (final review item 12): the
// two additions to docsJargon actually match.
func TestDocsJargonCatchesDocsPackAndGoCitations(t *testing.T) {
	for _, line := range []string{
		"a later part of this docs pack extends it",
		"a later part of this Docs Pack extends it",
		"(Source: `internal/router/router.go:15-17`.)",
	} {
		if docsJargon.FindString(line) == "" {
			t.Errorf("docsJargon does not match %q", line)
		}
	}
}

// TestDocsCarryNoInternalJargon: no public page speaks the maintainers'
// shorthand.
func TestDocsCarryNoInternalJargon(t *testing.T) {
	for _, p := range append(docPages(t), "plugin/skills/chottag/SKILL.md") {
		for i, l := range strings.Split(read(t, p), "\n") {
			if m := docsJargon.FindString(l); m != "" {
				t.Errorf("%s:%d says %q: internal shorthand; say what it means instead", p, i+1, m)
			}
		}
	}
}
