// Package release pins the release config and CI to the code and to the
// gate (M3 spec §4, N7). YAML is read line by line: the project is stdlib
// only.
package release

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", r, err)
	}
	return r
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// section returns the block under a top-level YAML key: the key's line and
// every following line until the next line that starts in column 0.
func section(t *testing.T, yaml, key string) string {
	t.Helper()
	lines := strings.Split(yaml, "\n")
	for i, l := range lines {
		if l == key+":" || strings.HasPrefix(l, key+": ") {
			j := i + 1
			for j < len(lines) && (lines[j] == "" || strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t") || strings.HasPrefix(lines[j], "#")) {
				j++
			}
			return strings.Join(lines[i:j], "\n")
		}
	}
	t.Fatalf("no top-level %q key", key)
	return ""
}

// hasLine reports whether block has a line equal to want once trimmed.
func hasLine(block, want string) bool {
	for _, l := range strings.Split(block, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// braces normalises "{{ .Version }}" and "{{.Version}}" to one spelling.
var braces = regexp.MustCompile(`\{\{\s*([^}]*?)\s*\}\}`)

func norm(s string) string { return braces.ReplaceAllString(s, "{{$1}}") }

var xFlag = regexp.MustCompile(`-X ([A-Za-z0-9_./-]+)\.([A-Za-z_][A-Za-z0-9_]*)=\{\{\.Version\}\}`)

func TestGoreleaserStampsCliVersion(t *testing.T) {
	y := norm(read(t, ".goreleaser.yaml"))
	m := xFlag.FindAllStringSubmatch(y, -1)
	if len(m) != 1 {
		t.Fatalf("want exactly one -X …=={{.Version}} ldflag, found %d", len(m))
	}
	pkg, name := m[0][1], m[0][2]
	// The -X package must be go.mod's module plus /internal/cli, and the
	// var must still exist as a package-level string var: -X silently does
	// nothing otherwise, and every release would say "dev".
	mod := strings.TrimPrefix(strings.SplitN(read(t, "go.mod"), "\n", 2)[0], "module ")
	if pkg != mod+"/internal/cli" || name != "Version" {
		t.Fatalf("-X target = %s.%s, want %s/internal/cli.Version", pkg, name, mod)
	}
	dir := filepath.Join(root(t), filepath.FromSlash(strings.TrimPrefix(pkg, mod+"/")))
	if !hasStringVar(t, dir, name) {
		t.Fatalf("%s declares no package-level `var %s = \"…\"` (a string): -X would not stamp it", dir, name)
	}
}

func hasStringVar(t *testing.T, dir, name string) bool {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range es {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				for i, id := range v.Names {
					if id.Name != name {
						continue
					}
					if typ, ok := v.Type.(*ast.Ident); v.Type != nil && (!ok || typ.Name != "string") {
						return false
					}
					if i < len(v.Values) {
						lit, ok := v.Values[i].(*ast.BasicLit)
						return ok && lit.Kind == token.STRING
					}
					return v.Type != nil
				}
			}
		}
	}
	return false
}

func TestGoreleaserBuildsTheReleaseMatrix(t *testing.T) {
	y := read(t, ".goreleaser.yaml")
	if !hasLine(y, "version: 2") {
		t.Error("not a GoReleaser v2 config (version: 2)")
	}
	b := section(t, y, "builds")
	for _, want := range []string{"main: ./cmd/chottag", "binary: chottag", "- CGO_ENABLED=0", "- -trimpath", "goos: [darwin, linux]", "goarch: [amd64, arm64]"} {
		if !hasLine(b, want) {
			t.Errorf("builds lacks the line %q", want)
		}
	}
	// A release must never be a chottag_fakeusage build (M1e-a): no tags.
	if strings.Contains(y, "tags") || strings.Contains(y, "chottag_fakeusage") {
		t.Error(".goreleaser.yaml mentions build tags; a release is always the untagged build")
	}
}

func TestGoreleaserArchiveAndChecksumNames(t *testing.T) {
	y := read(t, ".goreleaser.yaml")
	a := norm(section(t, y, "archives"))
	for _, want := range []string{"- formats: [tar.gz]", `name_template: "chottag_{{.Version}}_{{.Os}}_{{.Arch}}"`, "- README.md", "- LICENSE", "- NOTICE"} {
		if !hasLine(a, want) {
			t.Errorf("archives lacks the line %q", want)
		}
	}
	c := section(t, y, "checksum")
	for _, want := range []string{"name_template: checksums.txt", "algorithm: sha256"} {
		if !hasLine(c, want) {
			t.Errorf("checksum lacks the line %q", want)
		}
	}
	r := section(t, y, "release")
	if !hasLine(r, "draft: true") {
		t.Error(`release lacks the line "draft: true" (Ruling 33): release.yml's own "publish the release" step, not goreleaser, is what makes a release visible, and only after its attestation exists`)
	}
	if !hasLine(r, "replace_existing_draft: true") {
		t.Error(`release lacks the line "replace_existing_draft: true" (NEW-2, final re-review): without it, re-running the job after a failed attest creates a second draft with the same tag, and the publish step could un-draft the stale one`)
	}
}

// TestGoreleaserReleasesToTheTaggedRepo pins part 0 T2 (spec §2.2):
// release.github is gone, so a release goes to whichever repo the tag was
// pushed to, not a repo pinned in the config.
func TestGoreleaserReleasesToTheTaggedRepo(t *testing.T) {
	r := section(t, read(t, ".goreleaser.yaml"), "release")
	if hasLine(r, "github:") || strings.Contains(r, "owner:") || strings.Contains(r, "name:") {
		t.Errorf("release section still pins a repo:\n%s", r)
	}
}

// R63: the Homebrew tap section is written but never uploaded until the
// maintainer publishes a tap repo; the repo's visibility plays no part.
func TestGoreleaserDoesNotUploadTheTap(t *testing.T) {
	b := section(t, read(t, ".goreleaser.yaml"), "brews")
	if !hasLine(b, "skip_upload: true") {
		t.Error("brews lacks `skip_upload: true`; the tap is not uploaded until a tap repo exists (R63)")
	}
}

// gateFromClaudeMD returns the fenced block after CLAUDE.md's "The gate"
// line, one command per entry.
func gateFromClaudeMD(t *testing.T) []string {
	t.Helper()
	lines := strings.Split(read(t, "CLAUDE.md"), "\n")
	for i, l := range lines {
		if !strings.Contains(l, "The gate") {
			continue
		}
		j := i + 1
		for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
			j++
		}
		var gate []string
		for j++; j < len(lines) && strings.TrimSpace(lines[j]) != "```"; j++ {
			if s := strings.TrimSpace(lines[j]); s != "" {
				gate = append(gate, s)
			}
		}
		return gate
	}
	t.Fatal(`CLAUDE.md has no "The gate" line`)
	return nil
}

// gateFromWorkflow returns the run: | block of workflowRelPath's step named
// "gate" (ci.yml has one job with this step; release.yml's gate job does
// too — fix round item 11).
func gateFromWorkflow(t *testing.T, workflowRelPath string) []string {
	t.Helper()
	lines := strings.Split(read(t, workflowRelPath), "\n")
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
		return gate
	}
	t.Fatalf("%s has no `- name: gate` step followed by `run: |`", workflowRelPath)
	return nil
}

// N7: CI runs exactly the local gate, so the two cannot drift (F76's class).
func TestCIRunsExactlyTheGate(t *testing.T) {
	want := []string{
		`test -z "$(gofmt -l .)"`,
		"go vet ./...",
		"go test ./... -race -count=1 -shuffle=on",
		"go test -tags chottag_fakeusage -race -count=1 -shuffle=on ./internal/cli/",
	}
	md, ci := gateFromClaudeMD(t), gateFromWorkflow(t, ".github/workflows/ci.yml")
	if !reflect.DeepEqual(md, want) {
		t.Errorf("CLAUDE.md gate = %q, want %q", md, want)
	}
	if !reflect.DeepEqual(ci, md) {
		t.Errorf("ci.yml gate = %q, want CLAUDE.md's %q", ci, md)
	}
}

// gateAwk finds the awk program scripts/export-public runs over the
// exported CLAUDE.md: the text between its `awk '` line and the line that
// feeds it "$tmp/gate/CLAUDE.md". A single-quoted shell word cannot hold a
// quote, so [^']* never runs on into an earlier awk block.
var gateAwk = regexp.MustCompile("\n\tawk '\n([^']*)\n\t' \"\\$tmp/gate/CLAUDE\\.md\" >\"\\$tmp/gatecmds\"\n")

// gateFromExportPublicsAwk runs scripts/export-public's own gate-extraction
// awk program, read out of the script itself (part 5, re-review R-N1: a
// hand-copied duplicate here could drift from the script unseen), over
// this repo's own CLAUDE.md.
func gateFromExportPublicsAwk(t *testing.T) []string {
	t.Helper()
	m := gateAwk.FindStringSubmatch(read(t, "scripts/export-public"))
	if m == nil {
		t.Fatal("scripts/export-public has no `awk '...' \"$tmp/gate/CLAUDE.md\" >\"$tmp/gatecmds\"` block; the extraction moved, so update gateAwk")
	}
	cmd := exec.Command("awk", m[1])
	cmd.Stdin = strings.NewReader(read(t, "CLAUDE.md"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("awk (export-public's gate-extraction program): %v", err)
	}
	var got []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" {
			got = append(got, l)
		}
	}
	return got
}

// TestExportPublicsGateExtractionMatchesCI pins N6: export-public's own
// extraction of CLAUDE.md's gate block (run here with its literal awk
// script, not the Go test helpers' more lenient "contains" match) must
// equal the block CI actually runs.
func TestExportPublicsGateExtractionMatchesCI(t *testing.T) {
	got := gateFromExportPublicsAwk(t)
	want := gateFromWorkflow(t, ".github/workflows/ci.yml")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("export-public's gate extraction = %q, want CI's %q", got, want)
	}
}

// TestReleaseWorkflowHasAGateJobGoreleaserNeeds pins fix round item 11:
// release.yml's own gate job runs exactly the same gate as CLAUDE.md/
// ci.yml, and goreleaser never runs unless it passes.
func TestReleaseWorkflowHasAGateJobGoreleaserNeeds(t *testing.T) {
	md := gateFromClaudeMD(t)
	rel := gateFromWorkflow(t, ".github/workflows/release.yml")
	if !reflect.DeepEqual(rel, md) {
		t.Errorf("release.yml gate = %q, want CLAUDE.md's %q", rel, md)
	}
	y := read(t, ".github/workflows/release.yml")
	jobs := section(t, y, "jobs")
	i := strings.Index(jobs, "goreleaser:")
	if i == -1 {
		t.Fatal("release.yml has no goreleaser job")
	}
	if !hasLine(jobs[i:], "needs: gate") {
		t.Error("release.yml's goreleaser job lacks `needs: gate`")
	}
}

// stepBlock returns the full YAML lines of the step named name (its
// "- name: <name>" line up to, but excluding, the next step at the same
// indentation, or EOF): everything about that one step, not just its
// run: | body — so a caller can check for sibling keys like
// continue-on-error or if: that gateFromWorkflow's narrower scan skips.
func stepBlock(t *testing.T, yaml, name string) string {
	t.Helper()
	lines := strings.Split(yaml, "\n")
	marker := "- name: " + name
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if trimmed != marker {
			continue
		}
		indent := len(l) - len(trimmed)
		j := i + 1
		for j < len(lines) {
			lt := strings.TrimLeft(lines[j], " ")
			li := len(lines[j]) - len(lt)
			if lt == "" {
				j++
				continue
			}
			if li <= indent {
				break
			}
			j++
		}
		return strings.Join(lines[i:j], "\n")
	}
	t.Fatalf("no step named %q", name)
	return ""
}

// stepBlocks is stepBlock's plural: every step named name in yaml, not just
// the first. A workflow with more than one job can repeat a step name (ci.yml
// has a "gate" step in both the test and test-arm64 jobs), and a check that
// only ever sees the first one misses the same defect in every later copy.
func stepBlocks(t *testing.T, yaml, name string) []string {
	t.Helper()
	lines := strings.Split(yaml, "\n")
	marker := "- name: " + name
	var out []string
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		if trimmed != marker {
			continue
		}
		indent := len(l) - len(trimmed)
		j := i + 1
		for j < len(lines) {
			lt := strings.TrimLeft(lines[j], " ")
			li := len(lines[j]) - len(lt)
			if lt == "" {
				j++
				continue
			}
			if li <= indent {
				break
			}
			j++
		}
		out = append(out, strings.Join(lines[i:j], "\n"))
	}
	if len(out) == 0 {
		t.Fatalf("no step named %q", name)
	}
	return out
}

// jobBlock returns the whole YAML block of the job (a 2-space-indented key
// directly under `jobs:`) that contains a step named stepName: from the
// job's own key line up to, but excluding, the next 2-space-indented job
// key, or EOF. Unlike stepBlock, this also covers job-level keys such as
// continue-on-error or if: that sit as siblings of `steps:`, not inside
// any one step (fix round 2, N3).
var jobKeyLine = regexp.MustCompile(`^  [A-Za-z0-9_-]+:\s*$`)

func jobBlock(t *testing.T, yaml, stepName string) string {
	t.Helper()
	jobs := section(t, yaml, "jobs")
	lines := strings.Split(jobs, "\n")
	start := -1
	for i, l := range lines {
		if jobKeyLine.MatchString(l) {
			start = i
		}
		if strings.TrimSpace(l) == "- name: "+stepName {
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if jobKeyLine.MatchString(lines[j]) {
					end = j
					break
				}
			}
			return strings.Join(lines[start:end], "\n")
		}
	}
	t.Fatalf("no job in %q contains a step named %q", yaml[:min(40, len(yaml))], stepName)
	return ""
}

// TestGateJobCannotBeSkipped pins fix round 2's N3: continue-on-error or
// an if: condition on the JOB that holds the gate step (not just the step
// itself) would let the whole gate silently not run, e.g. on a schedule
// or workflow_dispatch input — stepBlock's narrower scan cannot see that.
func TestGateJobCannotBeSkipped(t *testing.T) {
	for _, f := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		b := jobBlock(t, read(t, f), "gate")
		if strings.Contains(b, "continue-on-error") {
			t.Errorf("%s: the gate job has continue-on-error:\n%s", f, b)
		}
		for _, l := range strings.Split(b, "\n") {
			li := len(l) - len(strings.TrimLeft(l, " "))
			// Only the job's OWN indentation (4 spaces: 2 for the job key,
			// 2 more for its direct children like runs-on/steps) counts as
			// job level; deeper lines belong to a step and are stepBlock's
			// job to police.
			if li == 4 && strings.HasPrefix(strings.TrimSpace(l), "if:") {
				t.Errorf("%s: the gate job has an if: condition:\n%s", f, b)
			}
		}
	}
}

// TestReleaseWorkflowScopesPermissionsPerJob pins fix round 2's N4: the
// gate job never writes to the repo, so contents: write must sit only on
// the goreleaser job that actually creates the release — not at the
// workflow level, where gate would inherit it too.
func TestReleaseWorkflowScopesPermissionsPerJob(t *testing.T) {
	y := read(t, ".github/workflows/release.yml")
	pre, _, ok := strings.Cut(y, "\njobs:")
	if !ok {
		t.Fatal("release.yml has no jobs: key")
	}
	if strings.Contains(pre, "contents: write") {
		t.Error("release.yml grants contents: write above jobs: (workflow level); it must sit only on the goreleaser job")
	}
	gate := jobBlock(t, y, "gate")
	if !hasLine(gate, "permissions:") || !hasLine(gate, "contents: read") {
		t.Errorf("release.yml's gate job lacks `permissions: contents: read`:\n%s", gate)
	}
	jobs := section(t, y, "jobs")
	i := strings.Index(jobs, "goreleaser:")
	if i == -1 {
		t.Fatal("release.yml has no goreleaser job")
	}
	gr := jobs[i:]
	if !hasLine(gr, "permissions:") || !hasLine(gr, "contents: write") {
		t.Errorf("release.yml's goreleaser job lacks `permissions: contents: write`:\n%s", gr)
	}
}

// TestGateStepsCannotBeSkipped pins fix round item 12: nothing may let a
// gate step "pass" without actually passing (continue-on-error) or be
// skipped outright (an if: condition) in either workflow. Checks every
// "gate" step (stepBlocks), not just the first: ci.yml now has one in each
// of the test and test-arm64 jobs (part 5 fix round 1).
func TestGateStepsCannotBeSkipped(t *testing.T) {
	for _, f := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		for _, b := range stepBlocks(t, read(t, f), "gate") {
			if strings.Contains(b, "continue-on-error") {
				t.Errorf("%s: a gate step has continue-on-error:\n%s", f, b)
			}
			for _, l := range strings.Split(b, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "if:") {
					t.Errorf("%s: a gate step has an if: condition:\n%s", f, b)
				}
			}
		}
	}
}

func TestCIRunsOnMacAndLinux(t *testing.T) {
	y := read(t, ".github/workflows/ci.yml")
	for _, want := range []string{"push:", "branches: [main]", "pull_request:", "os: [macos-26, ubuntu-24.04]", "runs-on: ${{ matrix.os }}", "go-version-file: go.mod"} {
		if !hasLine(y, want) {
			t.Errorf("ci.yml lacks the line %q", want)
		}
	}
}

// TestCIPushIsLimitedToMain pins fix round item 13: push is limited to
// main (a feature branch is covered by pull_request instead; a tag push by
// release.yml), so a push to a throwaway/worktree branch does not also
// burn a CI run for it.
func TestCIPushIsLimitedToMain(t *testing.T) {
	y := read(t, ".github/workflows/ci.yml")
	push := section(t, y, "on")
	if i := strings.Index(push, "push:"); i == -1 || !hasLine(push[i:], "branches: [main]") {
		t.Errorf("ci.yml's push trigger lacks `branches: [main]`:\n%s", push)
	}
}

func TestReleaseWorkflow(t *testing.T) {
	y := read(t, ".github/workflows/release.yml")
	for _, want := range []string{
		"- 'v*'",
		"contents: write",
		"fetch-depth: 0",
		"go-version-file: go.mod",
		"uses: goreleaser/goreleaser-action@e435ccd777264be153ace6237001ef4d979d3a7a # v6.4.0",
		"version: v2.18.2",
		"args: release --clean --release-notes docs/release-notes/${{ github.ref_name }}.md",
		"GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
	} {
		if !hasLine(y, want) {
			t.Errorf("release.yml lacks the line %q", want)
		}
	}
}

// The "- " sequence marker is optional: it is only present when uses: is
// the step's first key. A step that also carries a name: (so stepBlock can
// find it) puts uses: on its own indented line with no dash.
var pinnedUses = regexp.MustCompile(`^(- )?uses: [A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40} # v\d+\.\d+\.\d+$`)

// M2: every action is pinned by full commit SHA with its tag in a
// comment; a mutable tag can be moved under us. Every workflow is scanned
// (P4-R7); pr-policy.yml has no uses: line by design, so the "scan is
// broken" guard applies to ci.yml and release.yml only.
func TestActionsArePinnedBySHA(t *testing.T) {
	for _, f := range workflowFiles(t) {
		n := 0
		for _, l := range strings.Split(read(t, f), "\n") {
			s := strings.TrimSpace(l)
			if !strings.Contains(s, "uses:") {
				continue
			}
			n++
			if !pinnedUses.MatchString(s) {
				t.Errorf("%s: %q is not pinned by SHA with a # vX.Y.Z comment", f, s)
			}
		}
		if n == 0 && (f == ".github/workflows/ci.yml" || f == ".github/workflows/release.yml") {
			t.Errorf("%s: no uses: lines found; the scan is broken", f)
		}
	}
}

func TestGoreleaserVersionIsExact(t *testing.T) {
	b := stepBlock(t, read(t, ".github/workflows/release.yml"), "goreleaser")
	if !regexp.MustCompile(`(?m)^\s+version: v\d+\.\d+\.\d+$`).MatchString(b) {
		t.Errorf("the goreleaser step does not pin an exact version:\n%s", b)
	}
}

func TestCheckoutNeverPersistsCredentials(t *testing.T) {
	for _, f := range workflowFiles(t) {
		lines := strings.Split(read(t, f), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "actions/checkout@") {
				continue
			}
			ok := false
			for j := i + 1; j < len(lines) && j < i+5; j++ {
				if strings.TrimSpace(lines[j]) == "persist-credentials: false" {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s:%d: checkout without persist-credentials: false", f, i+1)
			}
		}
	}
}

func TestReleaseAttestsItsAssetsWhenPublic(t *testing.T) {
	y := read(t, ".github/workflows/release.yml")
	pre, _, _ := strings.Cut(y, "\njobs:")
	if !hasLine(pre, "permissions: {}") {
		t.Error("release.yml lacks a top-level `permissions: {}`")
	}
	jobs := section(t, y, "jobs")
	gr := jobs[strings.Index(jobs, "goreleaser:"):]
	for _, want := range []string{"id-token: write", "attestations: write", "contents: write"} {
		if !hasLine(gr, want) {
			t.Errorf("goreleaser job lacks %q", want)
		}
	}
	att := stepBlock(t, y, "attest release assets")
	for _, want := range []string{"if: ${{ github.event.repository.visibility == 'public' }}", "subject-checksums: ./dist/checksums.txt"} {
		if !hasLine(att, want) {
			t.Errorf("attest step lacks %q:\n%s", want, att)
		}
	}
	if strings.Index(gr, "- name: attest release assets") < strings.Index(gr, "goreleaser/goreleaser-action") {
		t.Error("the attest step must run after goreleaser has built dist/")
	}
	for _, bad := range []string{"id-token", "attestations"} {
		if strings.Contains(jobBlock(t, y, "gate"), bad) {
			t.Errorf("the gate job must not hold %s", bad)
		}
	}
}

// TestReleasePublishesOnlyAfterAttestation pins Ruling 33 (final review M3):
// goreleaser always creates a draft, and the one step that un-drafts it runs
// AFTER the (public-only) attest step, unconditionally — on the private
// repo too, where attest is skipped — so a failed attestation leaves the
// release a draft instead of a published, unverifiable "latest".
func TestReleasePublishesOnlyAfterAttestation(t *testing.T) {
	y := read(t, ".github/workflows/release.yml")
	jobs := section(t, y, "jobs")
	gr := jobs[strings.Index(jobs, "goreleaser:"):]

	attest := strings.Index(gr, "- name: attest release assets")
	publish := strings.Index(gr, "- name: publish the release")
	if attest == -1 || publish == -1 {
		t.Fatal("release.yml's goreleaser job lacks the attest and/or publish steps")
	}
	if publish < attest {
		t.Error("the publish step must run after the attest step, so a failed attest never leaves a published release")
	}

	pub := stepBlock(t, y, "publish the release")
	if strings.Contains(pub, "if:") {
		t.Errorf("the publish step must run unconditionally (on the private repo too, where attest is skipped):\n%s", pub)
	}
	for _, want := range []string{
		`run: gh release edit "$GITHUB_REF_NAME" --draft=false --repo "$GITHUB_REPOSITORY"`,
		"GH_TOKEN: ${{ github.token }}",
	} {
		if !hasLine(pub, want) {
			t.Errorf("the publish step lacks the line %q:\n%s", want, pub)
		}
	}
}

// A release carries the plugin version its tag names (Claude Code notices a
// plugin update by its version) and notes written for it: release.yml
// checks the tag against plugin.json before goreleaser runs, and passes
// docs/release-notes/<tag>.md as the release notes, so a release with no
// notes fails instead of publishing every commit as its changelog.
func TestReleaseChecksThePluginVersionAndUsesWrittenNotes(t *testing.T) {
	y := read(t, ".github/workflows/release.yml")
	jobs := section(t, y, "jobs")
	i := strings.Index(jobs, "goreleaser:")
	if i == -1 {
		t.Fatal("release.yml has no goreleaser job")
	}
	gr := jobs[i:]
	check := strings.Index(gr, "- name: plugin version matches tag")
	action := strings.Index(gr, "goreleaser/goreleaser-action")
	if check == -1 || action == -1 || check > action {
		t.Fatal("release.yml's goreleaser job must check the plugin version against the tag before goreleaser runs")
	}
	if !strings.Contains(stepBlock(t, y, "plugin version matches tag"), `test "v$(jq -r .version plugin/.claude-plugin/plugin.json)" = "$GITHUB_REF_NAME"`) {
		t.Error("the plugin version step does not compare plugin.json's version with the tag")
	}
	if !strings.Contains(gr, "--release-notes docs/release-notes/${{ github.ref_name }}.md") {
		t.Error("goreleaser does not take docs/release-notes/<tag>.md as the release notes")
	}
}

// plugin.json names the version being prepared, so its notes must already
// exist: writing them is part of bumping the version, not of tagging.
func TestTheCurrentPluginVersionHasReleaseNotes(t *testing.T) {
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(read(t, "plugin/.claude-plugin/plugin.json")), &p); err != nil || p.Version == "" {
		t.Fatalf("plugin.json version: %q, %v", p.Version, err)
	}
	notes := read(t, filepath.Join("docs", "release-notes", "v"+p.Version+".md"))
	if strings.TrimSpace(notes) == "" {
		t.Errorf("docs/release-notes/v%s.md is empty", p.Version)
	}
}

// installShRepo is install.sh's REPO: the one default-repo constant every
// repo name in the release config is checked against (P2-R8).
func installShRepo(t *testing.T) string {
	t.Helper()
	for _, l := range strings.Split(read(t, "install.sh"), "\n") {
		if v, ok := strings.CutPrefix(l, "REPO="); ok {
			return v
		}
	}
	t.Fatal("install.sh has no REPO= line")
	return ""
}

func TestGoreleaserTapPointsAtTheDefaultRepo(t *testing.T) {
	b := section(t, read(t, ".goreleaser.yaml"), "brews")
	if want := "homepage: https://github.com/" + installShRepo(t); !hasLine(b, want) {
		t.Errorf("brews lacks the line %q", want)
	}
}

// TestReleaseLdflagsStampTheBuiltBinary pins P2-R9: the -X flag GoReleaser
// passes still reaches cli.Version after the module rename. It builds
// ./cmd/chottag with .goreleaser.yaml's own ldflags line ({{.Version}}
// filled in) and runs `chottag version` in a throwaway HOME: a stale module
// path in -X is silently ignored by the linker, and every release would
// then say "dev".
func TestReleaseLdflagsStampTheBuiltBinary(t *testing.T) {
	var ldflags string
	for _, l := range strings.Split(norm(section(t, read(t, ".goreleaser.yaml"), "builds")), "\n") {
		if s := strings.TrimSpace(l); strings.HasPrefix(s, "- ") && strings.Contains(s, "-X ") {
			ldflags = strings.TrimPrefix(s, "- ")
		}
	}
	if ldflags == "" {
		t.Fatal("builds has no ldflags line with -X")
	}
	const ver = "9.8.7-stamp"
	ldflags = strings.ReplaceAll(ldflags, "{{.Version}}", ver)
	touchGoSources(t, root(t))
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go tool is not on PATH: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "chottag")
	build := exec.Command(goBin, "build", "-trimpath", "-ldflags", ldflags, "-o", bin, "./cmd/chottag")
	build.Dir = root(t)
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -ldflags %q: %v\n%s", ldflags, err, out)
	}
	home := t.TempDir()
	run := exec.Command(bin, "version")
	run.Env = []string{"HOME=" + home, "CHOTTAG_HOME=" + filepath.Join(home, "chottag"), "PATH=/usr/bin:/bin"}
	out, err := run.Output()
	if err != nil {
		t.Fatalf("chottag version: %v", err)
	}
	if got, want := string(out), "chottag "+ver+"\n"; got != want {
		t.Errorf("chottag version printed %q, want %q: the -X path in .goreleaser.yaml does not reach cli.Version", got, want)
	}
}

// touchGoSources reads every .go file under dir, so a change to one busts
// this package's test cache even though only the go subprocess compiles it
// (the same reason as cmd/chottag's touchSources).
func touchGoSources(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != dir && (n == ".git" || n == "testdata" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") {
			_, err = os.ReadFile(p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCIScansTheManifestForLeaks pins P2-R6: CI runs the generic leak scan
// over exactly the manifest-selected paths (never a path the manifest
// denies, which may hold such strings on purpose in the private repo),
// with no private patterns file, under bash's pipefail, and nothing can
// skip it.
func TestCIScansTheManifestForLeaks(t *testing.T) {
	y := read(t, ".github/workflows/ci.yml")
	s := stepBlock(t, y, "leak scan")
	for _, want := range []string{
		"shell: bash",
		"run: git ls-files -z | tr '\\0' '\\n' | scripts/manifest-select public-manifest.txt | scripts/leak-scan",
	} {
		if !hasLine(s, want) {
			t.Errorf("the leak scan step lacks the line %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, " -p ") || strings.Contains(s, "leak-patterns") {
		t.Errorf("CI must not use a private patterns file:\n%s", s)
	}
	job := jobBlock(t, y, "leak scan")
	if strings.Contains(job, "continue-on-error") || strings.Contains(job, "if:") {
		t.Errorf("the leak-scan job can be skipped:\n%s", job)
	}
}
