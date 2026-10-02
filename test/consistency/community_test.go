package consistency

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// Part 4: the community files. Every name this file declares starts with
// "community" (helpers) or "TestCommunity"/"TestContributing"/... (tests),
// so later part 4 test files can use the helpers without a clash.

// communityRootFiles are the root-level community files part 4 adds. The
// .github files are checked by githubforms_test.go.
var communityRootFiles = []string{
	"CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SUPPORT.md", "NOTICE",
	"CHANGELOG.md", "ROADMAP.md",
}

// communityOwner is the GitHub owner of the default repo (install.sh's
// REPO), "HaiNNT": the one handle a public file may name, and only where
// GitHub needs it (CODEOWNERS).
func communityOwner(t *testing.T) string {
	t.Helper()
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || owner == "" {
		t.Fatalf("install.sh REPO %q has no owner", repo)
	}
	return owner
}

// communityEmail mirrors scripts/leak-scan's email pattern; the example
// domains and git@github.com are the only addresses allowed.
var (
	communityEmail      = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	communityEmailAllow = regexp.MustCompile(`(?i)^(git@github\.com|.*@example\.(com|org|net))$`)
)

// communityCheckClean fails when the file rel names a real-looking email
// address, the owner's @handle (outside .github/CODEOWNERS), the private
// docs, or the maintainers' shorthand (docsJargon, as for docs pages).
func communityCheckClean(t *testing.T, rel string) {
	t.Helper()
	text := read(t, rel)
	handle := "@" + communityOwner(t)
	for i, l := range strings.Split(text, "\n") {
		for _, m := range communityEmail.FindAllString(l, -1) {
			if !communityEmailAllow.MatchString(m) {
				t.Errorf("%s:%d names an email address; report through GitHub instead", rel, i+1)
			}
		}
		if rel != ".github/CODEOWNERS" && strings.Contains(l, handle) {
			t.Errorf("%s:%d names %s; only .github/CODEOWNERS needs the handle", rel, i+1, handle)
		}
		if strings.Contains(l, "docs/"+"internal") {
			t.Errorf("%s:%d names the private docs, which a public reader never has", rel, i+1)
		}
		if m := docsJargon.FindString(l); m != "" {
			t.Errorf("%s:%d says %q: internal shorthand; say what it means instead", rel, i+1, m)
		}
	}
}

// communityCheckCode runs the docs pages' command and flag checks over the
// code spans and fences of rel: every `chottag <cmd> --flag` a reader could
// paste must exist.
func communityCheckCode(t *testing.T, rel string) {
	t.Helper()
	words := commandWords(t)
	sets, known, aliases := cliFlagSets(t), knownPaths(t), cliAliases(t)
	for _, c := range doccheck.Code(read(t, rel)) {
		for _, m := range chottagCmd.FindAllStringSubmatch(c, -1) {
			if !words[m[1]] {
				t.Errorf("%s names `chottag %s`, which is not a chottag command (in %q)", rel, m[1], c)
			}
		}
		for _, args := range chottagInvocations(c) {
			p := resolvePath(args, known, aliases)
			allowed := flagsFor(sets, p)
			for _, a := range args {
				tok := strings.Trim(a, "[](){}`'\",;.")
				if len(tok) < 2 || tok[0] != '-' || tok == "--" {
					continue
				}
				name, _, _ := strings.Cut(strings.TrimLeft(tok, "-"), "=")
				if name == "" || globalFlagNames[name] || allowed[name] {
					continue
				}
				t.Errorf("%s: `chottag %s` has no flag %s (in %q)", rel, p, tok, c)
			}
		}
	}
}

// communityCheckLinks checks every Markdown link in rel: a relative link
// names a file that exists (and a heading, for a .md#anchor); an absolute
// https://github.com/<repo>/blob/main/<path> link names a real file too.
func communityCheckLinks(t *testing.T, rel string) {
	t.Helper()
	prefix := "https://github.com/" + shellVar(t, read(t, "install.sh"), "REPO") + "/blob/main/"
	for _, k := range doccheck.Links(read(t, rel)) {
		target := k.Target
		if rest, ok := strings.CutPrefix(target, prefix); ok {
			target = "/" + rest
		} else if docsIsURL(target) {
			continue
		}
		file, anchor, _ := strings.Cut(target, "#")
		var p string
		switch {
		case file == "":
			p = rel
		case strings.HasPrefix(file, "/"):
			p = strings.TrimPrefix(file, "/")
		default:
			p = path.Clean(path.Join(path.Dir(rel), file))
		}
		if _, err := os.Stat(filepath.Join(root(t), filepath.FromSlash(p))); err != nil {
			t.Errorf("%s:%d links %s, which does not exist", rel, k.Line, k.Target)
			continue
		}
		if anchor != "" && strings.HasSuffix(p, ".md") && !doccheck.Anchors(read(t, p))[anchor] {
			t.Errorf("%s:%d links %s, but %s has no heading #%s", rel, k.Line, k.Target, p, anchor)
		}
	}
}

// communityGate returns the trimmed, non-empty lines of the first ```sh
// fence in rel that runs `go vet ./...`: the gate as that file states it.
func communityGate(t *testing.T, rel string) []string {
	t.Helper()
	for _, b := range doccheck.Fenced(read(t, rel), "sh") {
		if !strings.Contains(b, "go vet ./...") {
			continue
		}
		var out []string
		for _, l := range strings.Split(b, "\n") {
			if s := strings.TrimSpace(l); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	t.Fatalf("%s has no ```sh block with the gate", rel)
	return nil
}

func TestCommunityRootFilesAreCleanAndTheirCommandsReal(t *testing.T) {
	for _, rel := range communityRootFiles {
		communityCheckClean(t, rel)
		communityCheckCode(t, rel)
		communityCheckLinks(t, rel)
	}
}

// TestContributingStatesTheInvitationModel pins R85: issues and forks are
// welcome, pull requests only by invitation, and how a good issue is filed.
func TestContributingStatesTheInvitationModel(t *testing.T) {
	requirePhrases(t, "CONTRIBUTING.md",
		"Issues are welcome", "Forks are welcome", "by invitation only",
		"closed automatically", "reopen", "issues/new/choose",
		"chottag version", "claude --version", "chottag doctor",
		"no personal data or tokens", "SECURITY.md", "support.claude.com",
		"CODE_OF_CONDUCT.md", "Apache-2.0", "release notes",
	)
}

// TestContributingCarriesTheGateAndTheTestSafetyRules: the gate is
// CLAUDE.md's, line for line (P4-R11), and the sandbox rules are stated.
func TestContributingCarriesTheGateAndTheTestSafetyRules(t *testing.T) {
	got, want := communityGate(t, "CONTRIBUTING.md"), communityGate(t, "CLAUDE.md")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("CONTRIBUTING.md's gate:\n%s\nCLAUDE.md's gate:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	requirePhrases(t, "CONTRIBUTING.md",
		"CLAUDE.md", "standard library only", "t.TempDir()", "CHOTTAG_HOME",
		"47821", "install.sh", "chottag setup", "Keychain", "TestMain",
		"~/.claude", "alice@example.com", "scripts/leak-scan", "public-manifest.txt",
		"-shuffle=N", "scripts/dev-env", "claude --plugin-dir ./plugin",
	)
}

// TestCodeOfConductIsContributorCovenant21 (spec part 4): the Covenant 2.1
// text, its contact filled in with a GitHub route, never an email (P4-R9).
func TestCodeOfConductIsContributorCovenant21(t *testing.T) {
	requirePhrases(t, "CODE_OF_CONDUCT.md",
		"# Contributor Covenant Code of Conduct", "## Our Pledge", "## Our Standards",
		"## Enforcement Responsibilities", "## Scope", "## Enforcement",
		"## Enforcement Guidelines", "### 1. Correction", "### 2. Warning",
		"### 3. Temporary Ban", "### 4. Permanent Ban", "## Attribution",
		"version 2.1", "https://www.contributor-covenant.org/version/2/1/code_of_conduct.html",
		"Report a vulnerability", "Code of Conduct report", "Report content",
	)
	if strings.Contains(read(t, "CODE_OF_CONDUCT.md"), "INSERT CONTACT METHOD") {
		t.Error("CODE_OF_CONDUCT.md still has the Covenant's contact placeholder")
	}
}

// TestSupportIsBestEffortThroughIssues (spec part 4): best effort, through
// Issues; security and account questions go elsewhere.
func TestSupportIsBestEffortThroughIssues(t *testing.T) {
	requirePhrases(t, "SUPPORT.md",
		"best effort", "no support promise", "issues/new/choose", "chottag doctor",
		"troubleshooting", "SECURITY.md", "support.claude.com", "no personal data or tokens",
	)
}

// TestNoticeCarriesTheAttribution (P4-R10): Apache-2.0, the disclaimer, the
// trademark line and the cswap-pin credit from the README.
func TestNoticeCarriesTheAttribution(t *testing.T) {
	requirePhrases(t, "NOTICE",
		"c-hottag", "Copyright 2026 the c-hottag authors", "Apache License, Version 2.0",
		"not affiliated with or endorsed by Anthropic", "trademarks of Anthropic",
		"cswap-pin", "MIT", "No cswap-pin code is included",
	)
}

// TestRoadmapHasItsSections (spec part 4; R143): Shipped recently, Now,
// Next, Later, Not planned, in that order, each with items; the macOS app is
// planned under Next.
func TestRoadmapHasItsSections(t *testing.T) {
	md := read(t, "ROADMAP.md")
	var got []string
	for _, s := range doccheck.Sections(md) {
		if s.Level == 2 {
			got = append(got, s.Title)
			bullets := 0
			for _, l := range strings.Split(s.Lead, "\n") {
				if strings.HasPrefix(l, "- ") {
					bullets++
				}
			}
			if bullets == 0 {
				t.Errorf("ROADMAP.md ## %s lists nothing", s.Title)
			}
		}
	}
	if want := []string{"Shipped recently", "Now", "Next", "Later", "Not planned"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ROADMAP.md sections %q, want %q", got, want)
	}
	next, _ := doccheck.Find(md, 2, "Next")
	if !strings.Contains(flat(next.Body), "macOS app") {
		t.Error("ROADMAP.md ## Next lacks the macOS app (R143)")
	}
	requirePhrases(t, "ROADMAP.md", "no dates", "issues/new/choose")
}
