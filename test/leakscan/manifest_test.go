package leakscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestSelectDeniesByDefault(t *testing.T) {
	dir := tree(t, map[string]string{"m.txt": "# c\n\ninternal/\nREADME.md\n./docs/release-notes/\n!internal/secret/\n!internal/x.go\n"})
	in := "internal/a.go\ninternal/x.go\ninternal/secret/k\nREADME.md\nREADME.mdx\ndocs/release-notes/v1.md\n./internal/b.go\nsecret.txt\ndocs/internal/p.txt\n"
	out, errb, code := script(t, dir, in, "manifest-select", "m.txt")
	if want := "internal/a.go\nREADME.md\ndocs/release-notes/v1.md\ninternal/b.go\n"; code != 0 || out != want {
		t.Errorf("exit %d, stdout %q (stderr %q); want 0 and %q", code, out, errb, want)
	}
}

func TestManifestSelectFailsWithoutAUsableManifest(t *testing.T) {
	dir := tree(t, map[string]string{"only-bangs.txt": "# c\n!docs/\n"})
	for _, c := range []struct {
		name string
		args []string
		want int
	}{
		{"a missing manifest", []string{"nope.txt"}, 1},
		{"a manifest that selects nothing", []string{"only-bangs.txt"}, 1},
		{"no manifest argument", nil, 2},
	} {
		out, _, code := script(t, dir, "README.md\n", "manifest-select", c.args...)
		if code != c.want || out != "" {
			t.Errorf("%s: exit %d, stdout %q; want %d and no output", c.name, code, out, c.want)
		}
	}
}

// TestManifestSelectDenyLineWithoutASlashAlsoDeniesTheSubtree pins N1:
// !x must deny x/… too, not only the exact path x, so a bare (no
// trailing slash) deny entry behaves the same as !x/.
func TestManifestSelectDenyLineWithoutASlashAlsoDeniesTheSubtree(t *testing.T) {
	dir := tree(t, map[string]string{"m.txt": "docs/\n!docs/internal\n"})
	in := "docs/internal/k.txt\ndocs/other.txt\n"
	out, errb, code := script(t, dir, in, "manifest-select", "m.txt")
	if want := "docs/other.txt\n"; code != 0 || out != want {
		t.Errorf("exit %d, stdout %q (stderr %q); want 0 and %q", code, out, errb, want)
	}
}

// TestManifestSelectHandlesACRLFManifest pins the M8 mutation "strip()
// removed": a CRLF manifest file must parse the same as an LF one.
func TestManifestSelectHandlesACRLFManifest(t *testing.T) {
	dir := tree(t, map[string]string{"m.txt": "internal/\r\nREADME.md\r\n!internal/secret/\r\n"})
	in := "internal/a.go\ninternal/secret/k\nREADME.md\n"
	out, errb, code := script(t, dir, in, "manifest-select", "m.txt")
	if want := "internal/a.go\nREADME.md\n"; code != 0 || out != want {
		t.Errorf("exit %d, stdout %q (stderr %q); want 0 and %q", code, out, errb, want)
	}
}

// TestManifestSelectRejectsAQuotedPath pins N3 (ruling): a C-quoted git
// path (from plain "git ls-files", not "-z") is a usage error, never a
// silently dropped path. NEW-5: the message names the stdin line and
// advises "-z", not "-c core.quotePath=false" (which does not stop the
// quoting of a `"`, a backslash or a tab).
func TestManifestSelectRejectsAQuotedPath(t *testing.T) {
	dir := tree(t, map[string]string{"m.txt": "docs/\n"})
	out, errb, code := script(t, dir, "\"a b\".txt\n", "manifest-select", "m.txt")
	if code != 2 || out != "" {
		t.Errorf("exit %d, stdout %q (stderr %q); want 2 and no output for a quoted path", code, out, errb)
	}
	if !strings.Contains(errb, "stdin line 1") || !strings.Contains(errb, "-z") {
		t.Errorf("stderr %q: want it to name the stdin line and advise -z", errb)
	}
	if strings.Contains(errb, "core.quotePath") {
		t.Errorf("stderr %q: core.quotePath alone does not fix this; -z does", errb)
	}
}

func realManifest(t *testing.T) string { return filepath.Join(root(t), "public-manifest.txt") }

func TestThePublicManifestKeepsPrivatePathsOut(t *testing.T) {
	// docs/internal/ is the only private subtree under docs/ (part 3,
	// P3-R8): docs/ itself is public, so this no longer also checks
	// non-internal docs/ paths.
	in := strings.Join([]string{
		"docs/internal/leak-patterns.txt", "docs/internal/owner-claude.md",
		"docs/internal/superpowers/plans/x.md", "CLAUDE.local.md", ".claude/tracker.json",
		".superpowers/sdd/x/progress.md", "bin/chottag", "dist/x",
	}, "\n") + "\n"
	out, errb, code := script(t, t.TempDir(), in, "manifest-select", realManifest(t))
	if code != 0 || out != "" {
		t.Errorf("exit %d (stderr %q); the manifest selects private paths:\n%s", code, errb, out)
	}
}

func TestThePublicManifestSelectsWhatTheGateNeeds(t *testing.T) {
	need := []string{
		"go.mod", "CLAUDE.md", "README.md", "LICENSE", "SECURITY.md", "install.sh", ".goreleaser.yaml",
		".gitignore", ".github/workflows/ci.yml", ".claude/settings.json", ".claude/hooks/guard-home.py",
		".claude-plugin/marketplace.json", "plugin/.claude-plugin/plugin.json", "cmd/chottag/main.go",
		"internal/cli/cli.go", "docs/release-notes/README.md", "packaging/README.md", "routes/2.1.282.txt",
		"scripts/leak-scan", "scripts/manifest-select", "public-manifest.txt", "test/release/release_test.go",
		"CHANGELOG.md", "CODE_OF_CONDUCT.md", "CONTRIBUTING.md", "NOTICE", "ROADMAP.md", "SUPPORT.md",
	}
	in := strings.Join(need, "\n") + "\n"
	out, errb, code := script(t, t.TempDir(), in, "manifest-select", realManifest(t))
	if code != 0 || out != in {
		t.Errorf("exit %d (stderr %q); selected:\n%s\nwant:\n%s", code, errb, out, in)
	}
}

// entry is one public-manifest.txt line, parsed the way manifest-select
// reads it.
type entry struct {
	path string
	deny bool
}

// hit mirrors manifest-select's own hitAllow/hitDeny (NEW-6): a deny
// entry without a trailing slash also denies the subtree under it (N1),
// which an allow entry does not.
func (e entry) hit(p string) bool {
	if strings.HasSuffix(e.path, "/") {
		return strings.HasPrefix(p, e.path)
	}
	if e.deny {
		return p == e.path || strings.HasPrefix(p, e.path+"/")
	}
	return p == e.path
}

func manifestEntries(t *testing.T) []entry {
	t.Helper()
	b, err := os.ReadFile(realManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	var es []entry
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		e := entry{path: l}
		if rest, ok := strings.CutPrefix(l, "!"); ok {
			e = entry{path: rest, deny: true}
		}
		es = append(es, entry{path: strings.TrimPrefix(e.path, "./"), deny: e.deny})
	}
	return es
}

// tracked lists the repo's tracked files, or skips outside a git work tree
// (an exported snapshot: export-public has already applied the manifest).
func tracked(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command("git", "-c", "core.quotePath=false", "ls-files", "-z")
	cmd.Dir = root(t)
	out, err := cmd.Output()
	if err != nil {
		t.Skip("not a git work tree (an exported snapshot)")
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

func TestEveryTrackedPathIsClassified(t *testing.T) {
	es := manifestEntries(t)
	for _, f := range tracked(t) {
		classified := false
		for _, e := range es {
			if e.hit(f) {
				classified = true
				break
			}
		}
		if !classified {
			t.Errorf("%s matches no line of public-manifest.txt: list it (public) or add a ! line (private)", f)
		}
	}
}

// privateRepoMarker is the one thing that tells a checkout of this tree
// apart from a public snapshot (M3): docs/internal/ exists here, and
// export-public always keeps it out. A public checkout's own
// public-manifest.txt still carries its "!" lines (they came along
// verbatim from the tag), but the files they deny were never exported, so
// they are expected to match nothing there; the private repo, where the
// denied paths are still tracked, has no such excuse.
func privateRepoMarker(t *testing.T) bool {
	t.Helper()
	fi, err := os.Stat(filepath.Join(root(t), "docs", "internal"))
	return err == nil && fi.IsDir()
}

func TestEveryManifestEntryMatchesATrackedPath(t *testing.T) {
	files := tracked(t)
	private := privateRepoMarker(t)
	for _, e := range manifestEntries(t) {
		if e.deny && !private {
			continue // I1(a): a public snapshot's own "!" lines select nothing
		}
		found := false
		for _, f := range files {
			if e.hit(f) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("public-manifest.txt line %q matches no tracked file: remove it", e.path)
		}
	}
}
