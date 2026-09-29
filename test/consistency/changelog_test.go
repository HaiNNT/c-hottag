package consistency

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CHANGELOG.md is kept alongside docs/release-notes/ (P4-R2): one Keep a
// Changelog entry per release-notes file, newest first, each linked to its
// notes. A version is "Unreleased" until it is dated in the commit that
// bumps plugin.json to it (scripts/release bump does both in one commit).
// A version whose notes exist but which was never tagged (0.3.0) has no
// entry of its own: the entry that ships its changes says "[X.Y.Z] was never
// released" (P4-R18).

var (
	communityChangelogHeading       = regexp.MustCompile(`^## \[(\d+)\.(\d+)\.(\d+)\] - (\d{4}-\d{2}-\d{2}|Unreleased)$`)
	communityChangelogNeverReleased = regexp.MustCompile(`\[(\d+\.\d+\.\d+)\] was never released`)
	communityChangelogKinds         = map[string]bool{
		"Added": true, "Changed": true, "Deprecated": true,
		"Removed": true, "Fixed": true, "Security": true,
	}
)

// communityChangelogEntry is one "## [X.Y.Z] - date" section.
type communityChangelogEntry struct {
	version string
	parts   [3]int
	date    string
	body    string
}

func communityChangelog(t *testing.T) []communityChangelogEntry {
	t.Helper()
	var out []communityChangelogEntry
	var cur *communityChangelogEntry
	for _, l := range strings.Split(read(t, "CHANGELOG.md"), "\n") {
		if strings.HasPrefix(l, "## ") {
			m := communityChangelogHeading.FindStringSubmatch(l)
			if m == nil {
				t.Errorf("CHANGELOG.md heading %q: want `## [X.Y.Z] - YYYY-MM-DD` or `## [X.Y.Z] - Unreleased`", l)
				cur = nil
				continue
			}
			e := communityChangelogEntry{version: m[1] + "." + m[2] + "." + m[3], date: m[4]}
			for i := range e.parts {
				e.parts[i], _ = strconv.Atoi(m[i+1])
			}
			out = append(out, e)
			cur = &out[len(out)-1]
			continue
		}
		if strings.HasPrefix(l, "[") && strings.Contains(l, "]: ") {
			cur = nil // the link references close the last entry
		}
		if cur != nil {
			cur.body += l + "\n"
		}
	}
	return out
}

// communityCompare orders two X.Y.Z versions: -1, 0 or 1.
func communityCompare(a, b [3]int) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

// communityReleaseNotesVersions lists X.Y.Z for every docs/release-notes/vX.Y.Z.md.
func communityReleaseNotesVersions(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root(t), "docs", "release-notes", "v*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range m {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "v"), ".md"))
	}
	sort.Strings(out)
	return out
}

// TestChangelogHasAnEntryForEveryReleaseNotesFile: the two lists of
// versions are the same, both ways. A version the CHANGELOG names as never
// released counts in place of an entry, and has no heading of its own.
func TestChangelogHasAnEntryForEveryReleaseNotesFile(t *testing.T) {
	md := read(t, "CHANGELOG.md")
	headed := map[string]bool{}
	var got []string
	for _, e := range communityChangelog(t) {
		headed[e.version] = true
		got = append(got, e.version)
	}
	for _, m := range communityChangelogNeverReleased.FindAllStringSubmatch(md, -1) {
		if headed[m[1]] {
			t.Errorf("CHANGELOG.md says [%s] was never released, but also has an entry for it", m[1])
		}
		if ref := "[" + m[1] + "]: docs/release-notes/v" + m[1] + ".md"; !strings.Contains(md, "\n"+ref+"\n") {
			t.Errorf("CHANGELOG.md lacks the link line %q", ref)
		}
		got = append(got, m[1])
	}
	sort.Strings(got)
	want := communityReleaseNotesVersions(t)
	if len(want) == 0 {
		t.Fatal("no docs/release-notes/v*.md: the scan is broken")
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("CHANGELOG.md versions %q, docs/release-notes/ versions %q: each release has both", got, want)
	}
}

// TestChangelogFollowsKeepAChangelog: the header, newest first, real dates,
// only the six kinds of change, each with items, and a link to the notes.
func TestChangelogFollowsKeepAChangelog(t *testing.T) {
	md := read(t, "CHANGELOG.md")
	if !strings.HasPrefix(md, "# Changelog\n") {
		t.Error("CHANGELOG.md does not start with `# Changelog`")
	}
	requirePhrases(t, "CHANGELOG.md", "https://keepachangelog.com/en/1.1.0/", "https://semver.org/spec/v2.0.0.html", "docs/release-notes/")
	es := communityChangelog(t)
	for i, e := range es {
		if i > 0 && communityCompare(es[i-1].parts, e.parts) <= 0 {
			t.Errorf("CHANGELOG.md lists %s after %s: newest first", e.version, es[i-1].version)
		}
		if e.date != "Unreleased" {
			if _, err := time.Parse("2006-01-02", e.date); err != nil {
				t.Errorf("CHANGELOG.md %s date %q: %v", e.version, e.date, err)
			}
		}
		kinds := 0
		var kind string
		items := map[string]int{}
		for _, l := range strings.Split(e.body, "\n") {
			if k, ok := strings.CutPrefix(l, "### "); ok {
				if !communityChangelogKinds[k] {
					t.Errorf("CHANGELOG.md %s has `### %s`; use Added, Changed, Deprecated, Removed, Fixed or Security", e.version, k)
				}
				kind = k
				kinds++
				continue
			}
			if strings.HasPrefix(l, "- ") && kind != "" {
				items[kind]++
			}
		}
		if kinds == 0 {
			t.Errorf("CHANGELOG.md %s has no `### Added`/`### Changed`/... subsection", e.version)
		}
		for k := range communityChangelogKinds {
			if strings.Contains(e.body, "### "+k+"\n") && items[k] == 0 {
				t.Errorf("CHANGELOG.md %s `### %s` lists nothing", e.version, k)
			}
		}
		ref := "[" + e.version + "]: docs/release-notes/v" + e.version + ".md"
		if !strings.Contains(md, "\n"+ref+"\n") {
			t.Errorf("CHANGELOG.md lacks the link line %q", ref)
		}
	}
}

// TestChangelogDatesFollowThePluginVersion (P4-R18, adapted per P4-X1):
// plugin.json names the version most recently bumped, and scripts/release
// bump dates that version's CHANGELOG entry in the same commit that bumps
// plugin.json to it, so an entry is dated iff its version is not newer than
// plugin.json's (a version newer than plugin.json has not shipped yet, so
// it reads Unreleased). At most one entry is Unreleased, and it is the
// newest (topmost) one.
func TestChangelogDatesFollowThePluginVersion(t *testing.T) {
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(read(t, "plugin/.claude-plugin/plugin.json")), &p); err != nil {
		t.Fatal(err)
	}
	var cur [3]int
	fs := strings.Split(p.Version, ".")
	if len(fs) != 3 {
		t.Fatalf("plugin.json version %q is not X.Y.Z", p.Version)
	}
	for i, f := range fs {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("plugin.json version %q is not X.Y.Z", p.Version)
		}
		cur[i] = n
	}
	unreleased := 0
	for i, e := range communityChangelog(t) {
		isUnreleased := e.date == "Unreleased"
		if isUnreleased {
			unreleased++
			if i != 0 {
				t.Errorf("CHANGELOG.md %s reads Unreleased, but is not the top entry: only the newest entry may be Unreleased", e.version)
			}
		}
		switch c := communityCompare(e.parts, cur); {
		case c <= 0 && isUnreleased:
			t.Errorf("CHANGELOG.md %s reads Unreleased, but plugin.json is already at %s: it is dated in the commit that bumps plugin.json", e.version, p.Version)
		case c > 0 && !isUnreleased:
			t.Errorf("CHANGELOG.md %s is dated %s, but plugin.json is still at %s: a version is dated in the commit that bumps plugin.json to it", e.version, e.date, p.Version)
		}
	}
	if unreleased > 1 {
		t.Errorf("CHANGELOG.md has %d Unreleased entries, want at most 1", unreleased)
	}
}

// TestReleaseProcedureKeepsTheChangelog: the release steps say to add the
// entry and to date it in the commit that bumps plugin.json (P4-R2), and
// the next release's notes carry this part's changes (P4-R17).
func TestReleaseProcedureKeepsTheChangelog(t *testing.T) {
	requirePhrases(t, "docs/release-notes/README.md", "CHANGELOG.md", "Unreleased", "the date")
	requirePhrases(t, "docs/release-notes/v0.4.2.md", "CONTRIBUTING.md", "by invitation only", "CHANGELOG.md")
}
