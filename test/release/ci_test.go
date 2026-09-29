package release

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// namedJob returns the block of the job keyed name under jobs:, from its
// key line up to the next job key.
func namedJob(t *testing.T, yaml, name string) string {
	t.Helper()
	lines := strings.Split(section(t, yaml, "jobs"), "\n")
	for i, l := range lines {
		if l != "  "+name+":" {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if jobKeyLine.MatchString(lines[j]) {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	t.Fatalf("no job %q", name)
	return ""
}

// gatesFromWorkflow returns the run: | block of every step named "gate".
func gatesFromWorkflow(t *testing.T, workflowRelPath string) [][]string {
	t.Helper()
	lines := strings.Split(read(t, workflowRelPath), "\n")
	var out [][]string
	for i, l := range lines {
		if strings.TrimSpace(l) != "- name: gate" || i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "run: |" {
			continue
		}
		indent := len(lines[i+1]) - len(strings.TrimLeft(lines[i+1], " "))
		var gate []string
		for j := i + 2; j < len(lines); j++ {
			s := strings.TrimSpace(lines[j])
			if s == "" {
				continue
			}
			if len(lines[j])-len(strings.TrimLeft(lines[j], " ")) <= indent {
				break
			}
			gate = append(gate, s)
		}
		out = append(out, gate)
	}
	return out
}

// Every gate step in ci.yml (the matrix job's and the arm64 leg's) runs
// exactly CLAUDE.md's gate.
func TestEveryCIGateStepRunsTheGate(t *testing.T) {
	md := gateFromClaudeMD(t)
	gates := gatesFromWorkflow(t, ".github/workflows/ci.yml")
	if len(gates) != 2 {
		t.Fatalf("ci.yml has %d gate steps, want 2 (test, test-arm64)", len(gates))
	}
	for i, g := range gates {
		if !reflect.DeepEqual(g, md) {
			t.Errorf("ci.yml gate step %d = %q, want CLAUDE.md's %q", i+1, g, md)
		}
	}
}

// F232: an image moves only when a workflow says so. No runner label may
// be a moving "-latest" one.
func TestWorkflowsPinTheirRunnerImages(t *testing.T) {
	latest := regexp.MustCompile(`\b[a-z]+-latest\b`)
	for _, f := range workflowFiles(t) {
		for i, l := range strings.Split(read(t, f), "\n") {
			s := strings.TrimSpace(l)
			if strings.HasPrefix(s, "#") {
				continue
			}
			// A matrix entry's first key rides a "- " list marker (e.g.
			// `include: - os: macos-latest`); strip it before matching, or
			// that entry's own os:/runs-on: line is missed entirely.
			trimmed := strings.TrimPrefix(s, "- ")
			if (strings.HasPrefix(trimmed, "runs-on:") || strings.HasPrefix(trimmed, "os:")) && latest.MatchString(trimmed) {
				t.Errorf("%s:%d: %q uses a moving -latest image", f, i+1, s)
			}
		}
	}
}

// jobNames returns every job key under jobs:, in file order, so a job
// timeout check (and anything else keyed on "every job") never depends on
// a hand-written list that a new job can slip past.
func jobNames(t *testing.T, yaml string) []string {
	t.Helper()
	var names []string
	for _, l := range strings.Split(section(t, yaml, "jobs"), "\n") {
		if jobKeyLine.MatchString(l) {
			names = append(names, strings.TrimSuffix(strings.TrimSpace(l), ":"))
		}
	}
	if len(names) == 0 {
		t.Fatalf("no jobs found under jobs:")
	}
	return names
}

// A hung job stops at its own timeout, not GitHub's 6-hour default. The job
// list comes from the workflow itself (jobNames), not a hand-written one,
// so a job added later is covered automatically.
func TestEveryJobHasATimeout(t *testing.T) {
	for _, f := range workflowFiles(t) {
		y := read(t, f)
		for _, j := range jobNames(t, y) {
			if !regexp.MustCompile(`(?m)^    timeout-minutes: \d+$`).MatchString(namedJob(t, y, j)) {
				t.Errorf("%s: job %s has no timeout-minutes", f, j)
			}
		}
	}
}

// P5-R8: the arm64 leg is skipped on the private repo unless the owner
// opts in, so it never waits on a runner or spends minutes unasked; once
// the repo is public it always runs.
func TestArm64LegIsOptInWhilePrivate(t *testing.T) {
	j := namedJob(t, read(t, ".github/workflows/ci.yml"), "test-arm64")
	// hasLine trims each line's leading whitespace, so it cannot tell a
	// job-level `if:` from the same text indented as a step's own `if:`
	// (which would leave the job itself unconditional). Anchor on the
	// job body's own indent (4 spaces, matching `runs-on:` below) instead.
	expr := "${{ github.event.repository.visibility == 'public' || vars.CHOTTAG_CI_ARM64 == 'true' }}"
	if !regexp.MustCompile(`(?m)^    if: ` + regexp.QuoteMeta(expr) + `$`).MatchString(j) {
		t.Errorf("the test-arm64 job lacks a job-level `if: %s`:\n%s", expr, j)
	}
	if !hasLine(j, "runs-on: ubuntu-24.04-arm") {
		t.Errorf("the test-arm64 job lacks \"runs-on: ubuntu-24.04-arm\":\n%s", j)
	}
	if strings.Contains(j, "continue-on-error") {
		t.Errorf("the arm64 leg, once it runs, must be able to fail:\n%s", j)
	}
}

// Every job that runs the gate installs a pinned Bun and requires it, so
// test/bunsmoke runs in CI instead of skipping.
func TestGateJobsInstallBunAndRequireIt(t *testing.T) {
	y := read(t, ".github/workflows/ci.yml")
	for _, name := range []string{"test", "test-arm64"} {
		j := namedJob(t, y, name)
		for _, want := range []string{
			"- uses: oven-sh/setup-bun@0c5077e51419868618aeaa5fe8019c62421857d6 # v2.2.0",
			"bun-version: 1.4.2",
			`CHOTTAG_REQUIRE_BUN: "1"`,
		} {
			if !hasLine(j, want) {
				t.Errorf("job %s lacks %q", name, want)
			}
		}
	}
}

// P5-R6: staticcheck and govulncheck run in CI at exact versions through
// `go run tool@vX.Y.Z`, never @latest and never from go.mod, and never in
// release.yml, whose gate a new vulnerability report must not block.
func TestLintJobRunsPinnedTools(t *testing.T) {
	j := namedJob(t, read(t, ".github/workflows/ci.yml"), "lint")
	for _, want := range []string{
		"go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...",
		"go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 -tags chottag_fakeusage ./...",
		"run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...",
	} {
		if !hasLine(j, want) {
			t.Errorf("the lint job lacks %q:\n%s", want, j)
		}
	}
	if strings.Contains(read(t, "go.mod"), "require") {
		t.Error("go.mod has a require block: the CI tools must stay out of it")
	}
	rel := read(t, ".github/workflows/release.yml")
	for _, tool := range []string{"staticcheck", "govulncheck"} {
		if strings.Contains(rel, tool) {
			t.Errorf("release.yml runs %s; a release must not wait on it", tool)
		}
	}
}
