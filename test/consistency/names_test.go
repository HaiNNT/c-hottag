package consistency

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The project is c-hottag, its repo HaiNNT/c-hottag and its module
// github.com/HaiNNT/c-hottag (R84, part 2). install.sh's REPO is the one
// constant everything else is checked against (P2-R8).
const wantRepo = "HaiNNT/c-hottag"

// privateRepoMarker is the one thing that tells this checkout apart from a
// public snapshot (M3): docs/internal/ exists only here, never in an
// export-public snapshot. Where it holds, a test that is private-repo-only
// (the owner rules, the tracker) must fail, not silently skip, when its own
// file is missing: a later reorganisation or a bad merge must not drop
// private-repo coverage unnoticed.
func privateRepoMarker(t *testing.T) bool {
	t.Helper()
	fi, err := os.Stat(filepath.Join(root(t), "docs", "internal"))
	return err == nil && fi.IsDir()
}

// maintainerOnly are the paths a public reader never sees: the internal docs
// (export-public keeps them out) and local build or agent state.
var maintainerOnly = []string{
	".git", ".superpowers", ".worktrees", "bin", "dist", "CLAUDE.local.md", "docs/internal",
}

// publicFiles is what a public reader has: the tracked files
// public-manifest.txt selects (N2), computed the same way export-public
// computes it, from git ls-files piped through scripts/manifest-select.
// Outside a git work tree (an exported snapshot has no .git only if it was
// copied out of one by hand; the normal case still is one) it falls back
// to walking the whole tree outside maintainerOnly, since there is no
// tracked-file list to ask git for.
func publicFiles(t *testing.T) []string {
	t.Helper()
	r := root(t)
	if files, ok := manifestSelectedFiles(t, r); ok {
		return files
	}
	skip := map[string]bool{}
	for _, p := range maintainerOnly {
		skip[p] = true
	}
	for _, p := range ignoredFiles(r) {
		skip[p] = true
	}
	var files []string
	err := filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(r, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// manifestSelectedFiles runs git ls-files -z | scripts/manifest-select
// public-manifest.txt, exactly as export-public and CI's leak-scan job
// select paths. The second return is false outside a git work tree (git
// ls-files itself fails there), so the caller can fall back to a walk.
func manifestSelectedFiles(t *testing.T, r string) ([]string, bool) {
	t.Helper()
	tracked := exec.Command("git", "-C", r, "-c", "core.quotePath=false", "ls-files", "-z")
	out, err := tracked.Output()
	if err != nil {
		return nil, false
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	sel := exec.Command("/bin/sh", filepath.Join(r, "scripts", "manifest-select"), filepath.Join(r, "public-manifest.txt"))
	sel.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	selOut, err := sel.Output()
	if err != nil {
		t.Fatalf("manifest-select: %v", err)
	}
	var files []string
	for _, l := range strings.Split(string(selOut), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, true
}

// ignoredFiles lists the files and directories git ignores under r, as
// slash paths (directories without their trailing slash). Outside a git
// work tree (an exported snapshot) it returns nothing.
func ignoredFiles(r string) []string {
	cmd := exec.Command("git", "-C", r, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var ps []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p = strings.TrimSuffix(p, "/"); p != "" {
			ps = append(ps, p)
		}
	}
	return ps
}

func TestDefaultRepoIsTheRenamedRepo(t *testing.T) {
	if repo := shellVar(t, read(t, "install.sh"), "REPO"); repo != wantRepo {
		t.Fatalf("install.sh REPO=%s, want %s (F226: the old name redirects, and attestation checks fail closed on a moved repo)", repo, wantRepo)
	}
}

func TestModulePathIsTheDefaultRepo(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	first, _, _ := strings.Cut(read(t, "go.mod"), "\n")
	mod := strings.TrimPrefix(first, "module ")
	if mod != "github.com/"+repo {
		t.Errorf("go.mod module %q, want github.com/%s so `go install github.com/%s/cmd/chottag@latest` resolves", mod, repo, repo)
	}
	if m := shellVar(t, read(t, "install.sh"), "MODULE"); m != mod {
		t.Errorf("install.sh MODULE=%s, want go.mod's %s (find_clone and the -X stamp use it)", m, mod)
	}
}

func TestEveryPublicURLNamesTheDefaultRepo(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	url := "https://github.com/" + repo
	var p struct {
		Homepage   string `json:"homepage"`
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal([]byte(read(t, "plugin/.claude-plugin/plugin.json")), &p); err != nil {
		t.Fatal(err)
	}
	if p.Homepage != url || p.Repository != url {
		t.Errorf("plugin.json homepage %q, repository %q; want %q", p.Homepage, p.Repository, url)
	}
	if !strings.Contains(read(t, ".goreleaser.yaml"), "homepage: "+url+"\n") {
		t.Errorf(".goreleaser.yaml brews homepage is not %s", url)
	}
	sec := read(t, "SECURITY.md")
	for _, want := range []string{"--repo " + repo + "\n", "repos/" + repo + "/contents/install.sh?ref="} {
		if !strings.Contains(sec, want) {
			t.Errorf("SECURITY.md lacks %q", want)
		}
	}
	name := repo[strings.Index(repo, "/")+1:]
	if !strings.Contains(read(t, "README.md"), "cd "+name) {
		t.Errorf("README lacks `cd %s` after the clone", name)
	}
}

// TestNoPublicFileNamesAMovedDocPath pins P2-R3: the internal docs live
// under docs/internal/, and no public file still points at where they were.
func TestNoPublicFileNamesAMovedDocPath(t *testing.T) {
	old := regexp.MustCompile(`docs/(superpowers|reviews|m0|m1|m4)/|docs/(requirements-log|release-checklist)\.md`)
	for _, rel := range publicFiles(t) {
		b, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		s := strings.ReplaceAll(string(b), "docs/internal/", "")
		if loc := old.FindStringIndex(s); loc != nil {
			t.Errorf("%s:%d names %q; the internal docs moved under docs/internal/", rel, strings.Count(s[:loc[0]], "\n")+1, s[loc[0]:loc[1]])
		}
	}
}

// TestNoPublicFileNamesDocsInternal pins M1: outside the curation tooling
// itself and the test packages that necessarily talk about docs/internal/
// to test the private/public split (M3, the export), no manifest-selected
// file names docs/internal/ at all. A public reader has no docs/internal/
// to follow, so a comment must never send them there; the exception list
// is kept explicit and small on purpose.
func TestNoPublicFileNamesDocsInternal(t *testing.T) {
	exemptFile := map[string]bool{
		"scripts/export-public":   true,
		"scripts/leak-scan":       true,
		"scripts/manifest-select": true,
		"public-manifest.txt":     true,
	}
	exemptDir := []string{"test/leakscan/", "test/export/", "test/consistency/"}
	for _, rel := range publicFiles(t) {
		if exemptFile[rel] {
			continue
		}
		exempt := false
		for _, d := range exemptDir {
			if strings.HasPrefix(rel, d) {
				exempt = true
				break
			}
		}
		if exempt {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "docs/internal") {
				continue
			}
			if rel == ".github/workflows/ci.yml" && strings.Contains(line, "leak-patterns.txt where it exists") {
				continue // the leak-scan job's own comment
			}
			t.Errorf("%s:%d names docs/internal/, which a public reader never has: %q", rel, i+1, strings.TrimSpace(line))
		}
	}
}

// TestTheTrackerPointsAtTheRoadmap: the owner's status line reads
// .claude/tracker.json (R79), which is private and never exported.
func TestTheTrackerPointsAtTheRoadmap(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".claude", "tracker.json"))
	if errors.Is(err, fs.ErrNotExist) {
		if privateRepoMarker(t) {
			t.Fatal("docs/internal/ exists but .claude/tracker.json does not (M3): the private repo's own status pointer would be silently missing")
		}
		t.Skip("no .claude/tracker.json: a public snapshot")
	}
	if err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Roadmap string `json:"roadmap"`
	}
	if err := json.Unmarshal(b, &tr); err != nil || tr.Roadmap == "" {
		t.Fatalf(".claude/tracker.json: %v (roadmap %q)", err, tr.Roadmap)
	}
	if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(tr.Roadmap))); err != nil {
		t.Errorf(".claude/tracker.json names %s: %v", tr.Roadmap, err)
	}
}

// TestNoStaleRepoOrModuleNames: after the rename (P2-R1), and the plugin
// id rename to chottag@c-hottag (R92), the old name survives only in the
// release notes, which record what each release shipped and how to move
// off the old plugin id. N1: no design-spec-filename entry here — publicFiles skips
// docs/internal/, and no public file names the design spec, so such an
// entry could never fire and would only mask a future stale name.
func TestNoStaleRepoOrModuleNames(t *testing.T) {
	old := "claude-" + "hottag" // built, so this file never matches itself
	for _, rel := range publicFiles(t) {
		if strings.HasPrefix(rel, "docs/release-notes/v") {
			continue // release history and the move off the old plugin id
		}
		b, err := os.ReadFile(filepath.Join(root(t), filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if i := strings.Index(s, old); i >= 0 {
			n := strings.Count(s[:i], "\n") + 1
			t.Errorf("%s:%d still says %s (rename it; only the release notes keep the old name)", rel, n, old)
		}
	}
}
