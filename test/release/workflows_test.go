package release

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Part 4: every workflow, not only ci.yml and release.yml, is held to the
// supply-chain rules, and the pull-request policy workflow is checked
// statically (P4-R4, P4-R7). Nothing here runs gh or touches the network.

const prPolicy = ".github/workflows/pr-policy.yml"

// workflowFiles is every workflow file, as sorted slash paths from the repo
// root.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(root(t), ".github", "workflows", pat))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range m {
			out = append(out, ".github/workflows/"+filepath.Base(p))
		}
	}
	sort.Strings(out)
	if len(out) < 3 {
		t.Fatalf("workflows %q: want ci.yml, release.yml and pr-policy.yml at least", out)
	}
	return out
}

// runBlock returns the lines of the `run: |` block of the step named name,
// without the run: line itself.
func runBlock(t *testing.T, yaml, name string) string {
	t.Helper()
	b := stepBlock(t, yaml, name)
	_, body, ok := strings.Cut(b, "run: |\n")
	if !ok {
		t.Fatalf("step %q has no `run: |` block:\n%s", name, b)
	}
	return body
}

func TestEveryWorkflowSetsTopLevelPermissions(t *testing.T) {
	for _, f := range workflowFiles(t) {
		y := read(t, f)
		pre, _, _ := strings.Cut(y, "\njobs:")
		if !strings.Contains("\n"+pre, "\npermissions:") {
			t.Errorf("%s has no top-level permissions: key; least privilege starts there", f)
		}
	}
}

func TestWorkflowsUseNoSecretButTheToken(t *testing.T) {
	for _, f := range workflowFiles(t) {
		rest := read(t, f)
		for {
			i := strings.Index(rest, "secrets.")
			if i < 0 {
				break
			}
			if !strings.HasPrefix(rest[i:], "secrets.GITHUB_TOKEN") {
				t.Errorf("%s uses %q: no workflow reads a repository secret", f, rest[i:min(i+30, len(rest))])
			}
			rest = rest[i+len("secrets."):]
		}
	}
}

func TestOnlyThePRPolicyWorkflowUsesPullRequestTarget(t *testing.T) {
	for _, f := range workflowFiles(t) {
		if f != prPolicy && strings.Contains(read(t, f), "pull_request_target") {
			t.Errorf("%s uses pull_request_target: only %s may, and it never runs PR code", f, prPolicy)
		}
	}
}

// TestPRPolicyRunsOnPullRequestOpenOrReopen (Review Focus 2, revised by
// P4-X5): a reopened pull request is checked again too, but against
// whoever reopened it, not the original author — see
// TestPRPolicyChecksTheSenderOnReopen.
func TestPRPolicyRunsOnPullRequestOpenOrReopen(t *testing.T) {
	on := section(t, read(t, prPolicy), "on")
	var got []string
	for _, l := range strings.Split(on, "\n") {
		if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
			got = append(got, s)
		}
	}
	want := []string{"on:", "pull_request_target:", "types: [opened, reopened]"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("pr-policy.yml on: is\n%s\nwant exactly\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestPRPolicyHasLeastPrivilege: nothing at the top, and the one job may
// only write pull requests (to comment and close).
func TestPRPolicyHasLeastPrivilege(t *testing.T) {
	y := read(t, prPolicy)
	pre, _, _ := strings.Cut(y, "\njobs:")
	if !hasLine(pre, "permissions: {}") {
		t.Error("pr-policy.yml lacks a top-level `permissions: {}`")
	}
	var grants []string
	for _, l := range strings.Split(section(t, y, "jobs"), "\n") {
		s := strings.TrimSpace(l)
		if strings.HasSuffix(s, ": write") || strings.HasSuffix(s, ": read") {
			grants = append(grants, s)
		}
	}
	if strings.Join(grants, "|") != "pull-requests: write" {
		t.Errorf("pr-policy.yml grants %q; want only `pull-requests: write`", grants)
	}
}

// TestPRPolicyNeverTouchesPullRequestCode (Review Focus 3): under
// pull_request_target the token can write, so the workflow must never check
// out, build or run what the pull request contains. This denylist is
// belt-and-braces on top of TestPRPolicyExpressionsAreAllowlisted,
// TestPRPolicyHasExactlyOneStep and TestPRPolicyOnlyCallsAllowedGhCommands
// (P4-X4), which do the same job by allowlist instead.
func TestPRPolicyNeverTouchesPullRequestCode(t *testing.T) {
	y := read(t, prPolicy)
	for _, bad := range []string{
		"uses:", "actions/checkout", "pull_request.head", "git clone", "git fetch",
		"git checkout", "go build", "go test", "go run", "npm ", "make ",
		"pull_request.title", "pull_request.body", "head_ref",
		"gh pr checkout", "gh repo clone", "refs/pull", "merge_commit_sha", "head.sha",
	} {
		if strings.Contains(y, bad) {
			t.Errorf("pr-policy.yml contains %q: it must only call the GitHub API", bad)
		}
	}
	step := stepBlock(t, y, "close unless invited")
	for _, want := range []string{
		"GH_TOKEN: ${{ github.token }}",
		"REPO: ${{ github.repository }}",
		"PR: ${{ github.event.pull_request.number }}",
		"ACTION: ${{ github.event.action }}",
		"AUTHOR: ${{ github.event.pull_request.user.login }}",
		"SENDER: ${{ github.event.sender.login }}",
	} {
		if !hasLine(step, want) {
			t.Errorf("pr-policy.yml's step lacks the env line %q", want)
		}
	}
}

// prPolicyExpr matches a literal ${{ … }} expression anywhere in the file,
// capturing its trimmed contents.
var prPolicyExpr = regexp.MustCompile(`\$\{\{\s*([^}]*?)\s*\}\}`)

// prPolicyAllowedExprs is exactly the six expressions the job's one step
// assigns to a shell variable in its env: block (P4-X4, ruling after an
// attacker review): an allowlist, not a denylist, so a ${{ }}
// expression this workflow has no reason to use is refused outright, not
// merely the specific ones a denylist happened to name.
var prPolicyAllowedExprs = map[string]bool{
	"github.token":                         true,
	"github.repository":                    true,
	"github.event.pull_request.number":     true,
	"github.event.action":                  true,
	"github.event.pull_request.user.login": true,
	"github.event.sender.login":            true,
}

func TestPRPolicyExpressionsAreAllowlisted(t *testing.T) {
	y := read(t, prPolicy)
	for _, m := range prPolicyExpr.FindAllStringSubmatch(y, -1) {
		if !prPolicyAllowedExprs[m[1]] {
			t.Errorf("pr-policy.yml uses ${{ %s }}, which is not one of the env: block's exact expressions", m[1])
		}
	}
}

// TestPRPolicyHasExactlyOneStep: the job's one step is the only unit of
// trust with its job-level permissions and token; a second step could do
// anything with the same grant.
func TestPRPolicyHasExactlyOneStep(t *testing.T) {
	job := jobBlock(t, read(t, prPolicy), "close unless invited")
	steps := regexp.MustCompile(`(?m)^\s*- (name|run):`).FindAllString(job, -1)
	if len(steps) != 1 {
		t.Errorf("pr-policy.yml's job has %d step markers, want exactly 1:\n%s", len(steps), job)
	}
}

// TestPRPolicyOnlyCallsAllowedGhCommands (P4-X4): every gh invocation in
// the run block is one of the two the workflow needs — reading the
// permission, and closing the pull request — allowlisted, so a call this
// workflow has no reason to make (gh pr checkout, gh repo clone, gh api on
// a different endpoint...) is refused outright.
func TestPRPolicyOnlyCallsAllowedGhCommands(t *testing.T) {
	run := runBlock(t, read(t, prPolicy), "close unless invited")
	allowed := map[string]bool{
		`if ! perm=$(gh api "repos/$REPO/collaborators/$who/permission" --jq .permission); then`:    true,
		`if ! perm=$(gh api "repos/$REPO/collaborators/$AUTHOR/permission" --jq .permission); then`: true,
		`gh pr close "$PR" --repo "$REPO" --comment "$msg"`:                                         true,
	}
	ghLine := regexp.MustCompile(`\bgh\s`)
	for _, l := range strings.Split(run, "\n") {
		s := strings.TrimSpace(l)
		if s == "" || strings.HasPrefix(s, "#") || !ghLine.MatchString(s) {
			continue
		}
		if !allowed[s] {
			t.Errorf("pr-policy.yml's run block calls gh with an unlisted line %q", s)
		}
	}
}

// TestPRPolicyChecksTheSenderOnReopen (P4-X5, revised by the whole-branch
// final review): on reopen, the first permission check runs against
// whoever reopened the pull request (github.event.sender), not its
// original author — a collaborator reopening an uninvited pull request is
// accepting it, and an uninvited author must not evade the check by
// reopening their own pull request. The review found this alone too
// strict: a collaborator with write access could reopen someone else's
// invited pull request and have it closed anyway, since only the sender's
// access was checked. So a reopen that fails the sender check falls back
// to a second permission check against the pull request's original
// author, and the pull request stays open if either has write access.
func TestPRPolicyChecksTheSenderOnReopen(t *testing.T) {
	run := runBlock(t, read(t, prPolicy), "close unless invited")
	const branch = `if [ "$ACTION" = "reopened" ]; then`
	if strings.Count(run, branch) != 2 {
		t.Errorf("pr-policy.yml's run block does not branch on ACTION = reopened exactly twice (the who=$SENDER switch and the author fallback):\n%s", run)
	}
	if !hasLine(run, `who="$SENDER"`) {
		t.Errorf("pr-policy.yml's run block does not switch to $SENDER on reopen:\n%s", run)
	}
	senderCall := `gh api "repos/$REPO/collaborators/$who/permission" --jq .permission`
	authorCall := `gh api "repos/$REPO/collaborators/$AUTHOR/permission" --jq .permission`
	branchAt, senderAt := strings.Index(run, branch), strings.Index(run, senderCall)
	if branchAt < 0 || senderAt < 0 || branchAt > senderAt {
		t.Errorf("pr-policy.yml must decide who=$AUTHOR or $SENDER before it checks the permission:\n%s", run)
	}
	authorAt := strings.Index(run, authorCall)
	closeAt := strings.Index(run, "gh pr close")
	if authorAt < 0 || closeAt < 0 || authorAt < senderAt || authorAt > closeAt {
		t.Errorf("pr-policy.yml must fall back to checking the pull request's original author, after the sender check and before it closes:\n%s", run)
	}
	if secondBranchAt := strings.LastIndex(run, branch); secondBranchAt < senderAt || secondBranchAt > authorAt {
		t.Errorf("pr-policy.yml's author fallback must be guarded by its own ACTION = reopened check, between the sender check and the author check:\n%s", run)
	}
}

// TestPRPolicySkipsWritersAndDependabot (Review Focus 1): the maintainer, a
// collaborator with write access and Dependabot keep their pull requests
// open; an API error leaves the pull request open too.
func TestPRPolicySkipsWritersAndDependabot(t *testing.T) {
	y := read(t, prPolicy)
	job := jobBlock(t, y, "close unless invited")
	if !hasLine(job, "if: github.event.pull_request.user.login != 'dependabot[bot]'") {
		t.Errorf("pr-policy.yml's job does not skip Dependabot:\n%s", job)
	}
	run := runBlock(t, y, "close unless invited")
	for _, want := range []string{
		`if ! perm=$(gh api "repos/$REPO/collaborators/$who/permission" --jq .permission); then`,
		"admin | maintain | write)",
		`gh pr close "$PR" --repo "$REPO" --comment "$msg"`,
	} {
		if !hasLine(run, want) {
			t.Errorf("pr-policy.yml's run block lacks the line %q", want)
		}
	}
	check, closeAt := strings.Index(run, "if ! perm="), strings.Index(run, "gh pr close")
	failed := strings.Index(run, "exit 1")
	allowed := strings.Index(run, "exit 0")
	if check < 0 || closeAt < 0 || failed < 0 || allowed < 0 || !(check < failed && failed < allowed && allowed < closeAt) {
		t.Errorf("pr-policy.yml must check the permission, exit 1 on an API error and exit 0 for a writer, all before it closes:\n%s", run)
	}
	for _, want := range []string{"by invitation only", "CONTRIBUTING.md", "Issues are welcome"} {
		if !strings.Contains(run, want) {
			t.Errorf("pr-policy.yml's closing comment lacks %q", want)
		}
	}
	if strings.Contains(y, "HaiNNT") {
		t.Error("pr-policy.yml names the owner; it reads the repo from $GITHUB_REPOSITORY and the access from the API")
	}
}

// TestDependabotUpdatesOnlyGitHubActions (spec part 4): the module has no
// dependencies, so the actions' pins are the only thing to keep current.
func TestDependabotUpdatesOnlyGitHubActions(t *testing.T) {
	y := read(t, ".github/dependabot.yml")
	for _, want := range []string{"version: 2", `- package-ecosystem: "github-actions"`, `directory: "/"`, `interval: "weekly"`} {
		if !hasLine(y, want) {
			t.Errorf("dependabot.yml lacks the line %q", want)
		}
	}
	if n := strings.Count(y, "package-ecosystem:"); n != 1 {
		t.Errorf("dependabot.yml has %d package-ecosystem entries, want 1 (github-actions)", n)
	}
}
