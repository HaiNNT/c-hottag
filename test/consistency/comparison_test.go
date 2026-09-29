package consistency

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

var (
	comparisonVerified = regexp.MustCompile(`Last verified: (\d{4}-\d{2}-\d{2})`)
	comparisonAccessed = regexp.MustCompile(`\(accessed (\d{4}-\d{2}-\d{2})\)`)
)

// comparisonGuides are docs/migrating/*.md, sorted, as slash paths.
func comparisonGuides(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root(t), "docs", "migrating", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(m)
	var out []string
	for _, p := range m {
		out = append(out, "docs/migrating/"+filepath.Base(p))
	}
	return out
}

// comparisonExternal reports whether a link target is another project's
// page: an http(s) URL outside this repo.
func comparisonExternal(t *testing.T, target string) bool {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	return (strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://")) &&
		!strings.HasPrefix(target, "https://github.com/"+repo)
}

// comparisonDate parses a YYYY-MM-DD date that must not be in the future.
func comparisonDate(t *testing.T, where, s string) {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Errorf("%s: bad date %q", where, s)
		return
	}
	if d.After(time.Now()) {
		t.Errorf("%s: date %s is in the future", where, s)
	}
}

// TestComparisonPagesAreDatedAndSourced (P3-R2): the comparison page and
// every migration guide carry a "Last verified" date and a ## Sources list;
// every source has an access date, and every outside link the page cites
// is one of its sources.
func TestComparisonPagesAreDatedAndSourced(t *testing.T) {
	for _, page := range append([]string{"docs/comparison.md"}, comparisonGuides(t)...) {
		md := read(t, page)
		m := comparisonVerified.FindStringSubmatch(md)
		if m == nil {
			t.Errorf("%s has no \"Last verified: YYYY-MM-DD\"", page)
		} else {
			comparisonDate(t, page, m[1])
		}
		src, ok := doccheck.Find(md, 2, "Sources")
		if !ok {
			t.Errorf("%s has no ## Sources", page)
			continue
		}
		sourced := map[string]bool{}
		for _, l := range strings.Split(src.Lead, "\n") {
			links := doccheck.Links(l)
			if len(links) == 0 {
				continue
			}
			a := comparisonAccessed.FindStringSubmatch(l)
			if a == nil {
				t.Errorf("%s: source %q has no \"(accessed YYYY-MM-DD)\"", page, links[0].Target)
			} else {
				comparisonDate(t, page, a[1])
			}
			for _, k := range links {
				sourced[k.Target] = true
			}
		}
		if len(sourced) == 0 {
			t.Errorf("%s: ## Sources lists no source", page)
		}
		for _, k := range doccheck.Links(md) {
			if k.Line < src.Line && comparisonExternal(t, k.Target) && !sourced[k.Target] {
				t.Errorf("%s:%d cites %s, which ## Sources does not list", page, k.Line, k.Target)
			}
		}
	}
}

// TestComparisonTableCitesEveryRow (P3-R2): no claim about another tool
// without a source in its own row.
func TestComparisonTableCitesEveryRow(t *testing.T) {
	s, ok := doccheck.Find(read(t, "docs/comparison.md"), 2, "At a glance")
	if !ok {
		t.Fatal("docs/comparison.md has no ## At a glance")
	}
	rows := 0
	for _, row := range tableRows(s.Lead) {
		name := strings.ToLower(row[0])
		if strings.Contains(name, "chottag") || strings.Contains(name, "c-hottag") {
			continue
		}
		rows++
		cited := false
		for _, k := range doccheck.Links("|" + strings.Join(row, "|") + "|") {
			if comparisonExternal(t, k.Target) {
				cited = true
			}
		}
		if !cited {
			t.Errorf("docs/comparison.md: the %q row cites no source", row[0])
		}
	}
	if rows == 0 {
		t.Error("docs/comparison.md: ## At a glance compares no tool")
	}
}

// TestMigrationGuidesFollowTheTemplate (P3-R2): one to four guides, each
// with the same sections, the chottag commands that import logins, and a
// link both ways with the comparison page.
func TestMigrationGuidesFollowTheTemplate(t *testing.T) {
	guides := comparisonGuides(t)
	if len(guides) == 0 || len(guides) > 4 {
		t.Fatalf("docs/migrating/ holds %d guides, want 1 to 4", len(guides))
	}
	comparison := read(t, "docs/comparison.md")
	for _, g := range guides {
		md := read(t, g)
		for _, h := range []string{"Concepts", "Before you start", "Move your accounts", "Undo", "Sources"} {
			if _, ok := doccheck.Find(md, 2, h); !ok {
				t.Errorf("%s has no ## %s", g, h)
			}
		}
		requirePhrases(t, g, "chottag login", "chottag adopt", "](../comparison.md")
		if !strings.Contains(comparison, "](migrating/"+filepath.Base(g)) {
			t.Errorf("docs/comparison.md does not link %s", g)
		}
	}
}

// TestClaudeSwapGuideWarnsPurgeIsDestructive (fix round 1, review HIGH item
// 4; fix round 2, N2): `cswap purge` deletes all of claude-swap's own data,
// its saved logins included — not Claude Code's live login — so the guide
// must call it out as destructive and make it a last, optional step, not a
// required part of the move.
func TestClaudeSwapGuideWarnsPurgeIsDestructive(t *testing.T) {
	requirePhrases(t, "docs/migrating/claude-swap.md",
		"`cswap purge` is destructive",
		"deletes all of claude-swap's data, including its saved logins",
		"last, optional step",
	)
}

// TestSwapdexGuideRequiresRemovingItsShimFirst (fix round 1, review HIGH
// item 2; fix round 2, N3): putting chottag's shim first on PATH while
// swapdex's own shim is still installed is not safe — internal/shim.
// ResolveClaude walks PATH and would resolve "the real claude" to swapdex's
// shim, chaining two proxies. The guide must require removing swapdex's
// shim from PATH and separately stopping its service (`swapdex service
// uninstall`) before `chottag setup`, and must not still offer "put
// chottag's shim first on PATH" as an alternative.
func TestSwapdexGuideRequiresRemovingItsShimFirst(t *testing.T) {
	requirePhrases(t, "docs/migrating/swapdex.md",
		"swapdex service uninstall",
		"is not safe",
	)
	if strings.Contains(flat(read(t, "docs/migrating/swapdex.md")), flat("chottag's shim comes first on")) {
		t.Error("docs/migrating/swapdex.md still offers putting chottag's shim first on PATH as an alternative to removing swapdex's shim")
	}
}
