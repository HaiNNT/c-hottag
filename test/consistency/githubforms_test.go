package consistency

import (
	"regexp"
	"strings"
	"testing"
)

// Part 4: the issue forms, the issue chooser's config, the pull request
// template and CODEOWNERS. GitHub parses the forms; the project is stdlib
// only, so these checks read the YAML as text (P4-R16).

var communityForms = []string{
	".github/ISSUE_TEMPLATE/bug_report.yml",
	".github/ISSUE_TEMPLATE/feature_idea.yml",
	".github/ISSUE_TEMPLATE/route_drift.yml",
}

// communityPrivacyOption is the checkbox every form requires (spec part 4).
const communityPrivacyOption = "This issue contains no personal data or tokens"

var (
	communityFormID = regexp.MustCompile(`^\s+id: (\S+)$`)
	// communityPlainPair is a `key: value` line whose value is a plain
	// (unquoted, non-block, non-flow) scalar.
	communityPlainPair = regexp.MustCompile(`^\s*(- )?[a-z_-]+: ([^"'|>\[{].*)$`)
)

// communityCheckYAMLText catches the mistakes that make GitHub drop a form
// from the chooser: a tab, or a plain scalar holding ": " or " #".
func communityCheckYAMLText(t *testing.T, rel string) {
	t.Helper()
	for i, l := range strings.Split(read(t, rel), "\n") {
		if strings.Contains(l, "\t") {
			t.Errorf("%s:%d has a tab; YAML indents with spaces", rel, i+1)
		}
		if m := communityPlainPair.FindStringSubmatch(l); m != nil {
			if strings.Contains(m[2], ": ") || strings.Contains(m[2], " #") {
				t.Errorf("%s:%d: %q needs quotes (a plain YAML value cannot hold \": \" or \" #\")", rel, i+1, strings.TrimSpace(l))
			}
		}
	}
}

// TestCommunityIssueFormsRequireNoPersonalData (Review Focus 4): every form
// has its name, description, label and body, a required privacy checkbox,
// and a pointer to SECURITY.md.
func TestCommunityIssueFormsRequireNoPersonalData(t *testing.T) {
	for _, rel := range communityForms {
		y := read(t, rel)
		for _, key := range []string{"name: ", "description: ", "labels: [", "body:"} {
			if !strings.Contains("\n"+y, "\n"+key) {
				t.Errorf("%s lacks a top-level %q", rel, key)
			}
		}
		lines := strings.Split(y, "\n")
		found := false
		for i, l := range lines {
			if !strings.Contains(l, "- label: "+communityPrivacyOption) {
				continue
			}
			for j := i + 1; j < len(lines); j++ {
				if s := strings.TrimSpace(lines[j]); s != "" {
					found = s == "required: true"
					break
				}
			}
		}
		if !found {
			t.Errorf("%s has no required checkbox %q", rel, communityPrivacyOption)
		}
		if !strings.Contains(y, "type: checkboxes") {
			t.Errorf("%s has no checkboxes block", rel)
		}
		if !strings.Contains(y, "/blob/main/SECURITY.md") {
			t.Errorf("%s does not send security reports to SECURITY.md", rel)
		}
		seen := map[string]bool{}
		for _, l := range lines {
			if m := communityFormID.FindStringSubmatch(l); m != nil {
				if seen[m[1]] {
					t.Errorf("%s repeats the id %q; GitHub rejects the form", rel, m[1])
				}
				seen[m[1]] = true
			}
		}
		communityCheckYAMLText(t, rel)
		communityCheckClean(t, rel)
		communityCheckCode(t, rel)
		communityCheckLinks(t, rel)
	}
}

// TestCommunityIssueFormsAskForWhatTriageNeeds: each form asks for the
// facts its kind of report needs, with the commands that print them.
func TestCommunityIssueFormsAskForWhatTriageNeeds(t *testing.T) {
	want := map[string][]string{
		".github/ISSUE_TEMPLATE/bug_report.yml": {
			"name: Bug report", `labels: ["bug"]`, "`chottag version`", "`claude --version`",
			"`chottag doctor`", "required: true",
		},
		".github/ISSUE_TEMPLATE/route_drift.yml": {
			"name: Route drift", `labels: ["route-drift"]`, "`chottag trace on`",
			"`chottag trace summarize`", "`claude --version`", "`chottag version`",
		},
		".github/ISSUE_TEMPLATE/feature_idea.yml": {
			"name: Feature idea", `labels: ["enhancement"]`, "/blob/main/ROADMAP.md",
		},
	}
	for rel, phrases := range want {
		requirePhrases(t, rel, phrases...)
	}
}

// TestCommunityIssueChooserHasNoBlankIssue: only the forms, plus links for
// security reports and account questions (P4-R12).
func TestCommunityIssueChooserHasNoBlankIssue(t *testing.T) {
	rel := ".github/ISSUE_TEMPLATE/config.yml"
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	y := read(t, rel)
	for _, want := range []string{
		"blank_issues_enabled: false",
		"url: https://github.com/" + repo + "/security/policy",
		"url: https://support.claude.com",
	} {
		if !communityHasYAMLLine(y, want) {
			t.Errorf("%s lacks the line %q", rel, want)
		}
	}
	communityCheckYAMLText(t, rel)
	communityCheckClean(t, rel)
}

// communityHasYAMLLine reports whether y has a line equal to want once trimmed and
// stripped of a leading "- ".
func communityHasYAMLLine(y, want string) bool {
	for _, l := range strings.Split(y, "\n") {
		if strings.TrimPrefix(strings.TrimSpace(l), "- ") == want {
			return true
		}
	}
	return false
}

// TestCommunityPullRequestTemplateStatesThePolicy (R85): the template says
// pull requests are by invitation, and what an invited one must carry.
func TestCommunityPullRequestTemplateStatesThePolicy(t *testing.T) {
	rel := ".github/PULL_REQUEST_TEMPLATE.md"
	requirePhrases(t, rel,
		"by invitation only", "/blob/main/CONTRIBUTING.md", "issues/new/choose",
		"closed automatically", "The gate passes", "t.TempDir()", "no personal data or tokens",
		"docs/release-notes/",
	)
	if n := strings.Count(read(t, rel), "- [ ] "); n < 5 {
		t.Errorf("%s has %d checklist items, want at least 5", rel, n)
	}
	communityCheckClean(t, rel)
	communityCheckCode(t, rel)
	communityCheckLinks(t, rel)
}

// TestCommunityCodeownersNamesTheOwner: one rule, every file, the repo's
// owner: the one place the handle appears (P4-R13).
func TestCommunityCodeownersNamesTheOwner(t *testing.T) {
	rel := ".github/CODEOWNERS"
	var rules []string
	for _, l := range strings.Split(read(t, rel), "\n") {
		if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
			rules = append(rules, s)
		}
	}
	if want := "* @" + communityOwner(t); len(rules) != 1 || rules[0] != want {
		t.Errorf("%s rules %q, want exactly %q", rel, rules, want)
	}
	communityCheckClean(t, rel)
}
