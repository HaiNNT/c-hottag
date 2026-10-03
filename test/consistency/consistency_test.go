// Package consistency checks that the README, the skill, install.sh and
// .goreleaser.yaml agree on the names each of them hard-codes: the release
// asset, the repo, the -X stamp, the plugin install id, and the chottag
// commands they tell a person or a Claude session to run.
package consistency

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

func root(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
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

// shellVar returns the value of a top-level NAME=value line in install.sh.
func shellVar(t *testing.T, script, name string) string {
	t.Helper()
	for _, l := range strings.Split(script, "\n") {
		if v, ok := strings.CutPrefix(l, name+"="); ok {
			return v
		}
	}
	t.Fatalf("install.sh has no %s= line", name)
	return ""
}

func TestInstallAssetNameMatchesGoReleaser(t *testing.T) {
	sh := read(t, "install.sh")
	m := regexp.MustCompile(`asset="(chottag_[^"]*)"`).FindStringSubmatch(sh)
	if m == nil {
		t.Fatal(`install.sh has no asset="…" line`)
	}
	fromSh := strings.NewReplacer("${ver}", "V", "${os}", "O", "${arch}", "A").Replace(m[1])

	y := read(t, ".goreleaser.yaml")
	g := regexp.MustCompile(`name_template: "(chottag_[^"]*)"`).FindStringSubmatch(y)
	if g == nil || !strings.Contains(y, "formats: [tar.gz]") {
		t.Fatal(".goreleaser.yaml has no chottag_ archive name_template with formats: [tar.gz]")
	}
	fromGR := regexp.MustCompile(`\s+`).ReplaceAllString(g[1], "")
	fromGR = strings.NewReplacer("{{.Version}}", "V", "{{.Os}}", "O", "{{.Arch}}", "A").Replace(fromGR) + ".tar.gz"

	if fromSh != fromGR || fromSh != "chottag_V_O_A.tar.gz" {
		t.Errorf("install.sh asks for %q, GoReleaser publishes %q", fromSh, fromGR)
	}
}

func TestInstallStampsTheSameVariableAsGoReleaser(t *testing.T) {
	sh := read(t, "install.sh")
	mod := shellVar(t, sh, "MODULE")
	if !strings.Contains(sh, `-X $MODULE/internal/cli.Version=$ver`) {
		t.Fatal(`install.sh does not stamp -X $MODULE/internal/cli.Version=$ver`)
	}
	want := "-X " + mod + "/internal/cli.Version={{.Version}}"
	if !strings.Contains(read(t, ".goreleaser.yaml"), want) {
		t.Errorf(".goreleaser.yaml lacks %q (install.sh's MODULE is %q)", want, mod)
	}
}

func TestOneRepoEverywhere(t *testing.T) {
	repo := shellVar(t, read(t, "install.sh"), "REPO")
	// A release goes to whichever repo the tag was pushed to (part 0 T2):
	// .goreleaser.yaml must pin no release.github block, so it never
	// overrides that.
	lines := strings.Split(read(t, ".goreleaser.yaml"), "\n")
	for i, l := range lines {
		if l != "release:" {
			continue
		}
		for j := i + 1; j < len(lines) && (lines[j] == "" || strings.HasPrefix(lines[j], " ")); j++ {
			if strings.TrimSpace(lines[j]) == "github:" {
				t.Error(".goreleaser.yaml has a release.github block; a release must go to the repo the tag was pushed to")
			}
		}
	}
	readme := read(t, "README.md")
	for _, want := range []string{
		"gh repo clone " + repo,
		"repos/" + repo + "/contents/install.sh",
		"/plugin marketplace add " + repo,
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	skill := read(t, "plugin/skills/chottag/SKILL.md")
	if want := "github.com/" + repo; !strings.Contains(skill, want) {
		t.Errorf("SKILL.md lacks %q (fix round item 14)", want)
	}
}

func TestREADMEInstallsThePluginByItsRealId(t *testing.T) {
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name string `json:"name"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(read(t, ".claude-plugin/marketplace.json")), &m); err != nil || len(m.Plugins) != 1 {
		t.Fatalf("marketplace.json: %v (%d plugins)", err, len(m.Plugins))
	}
	id := m.Plugins[0].Name + "@" + m.Name
	if !strings.Contains(read(t, "README.md"), "claude plugin install "+id) {
		t.Errorf("README lacks `claude plugin install %s`", id)
	}
}

// commandWords is every command chottag's usage lists (internal/cli/cli.go's
// usage const, read as text) plus help and the aliases dispatch accepts.
func commandWords(t *testing.T) map[string]bool {
	t.Helper()
	src := read(t, "internal/cli/cli.go")
	const open = "const usage = `"
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatal("cli.go has no usage const")
	}
	body := src[i+len(open):]
	body = body[:strings.Index(body, "`")]
	words := map[string]bool{"help": true, "ls": true, "rc": true, "remote-control": true}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "  ") {
			if f := strings.Fields(l); len(f) > 0 {
				words[f[0]] = true
			}
		}
	}
	return words
}

var chottagCmd = regexp.MustCompile(`(?:^|[\s(|;&])chottag\s+([a-z][a-z-]*)`)

func TestDocsNameOnlyRealCommands(t *testing.T) {
	words := commandWords(t)
	for _, doc := range append(docPages(t), "plugin/skills/chottag/SKILL.md") {
		for _, c := range doccheck.Code(read(t, doc)) {
			// The mod's /login guard text says "a chottag account" in prose
			// (M8b spec §4: verbatim). Its one command, `chottag login`, is
			// checked below.
			if strings.Contains(c, "Claude Code's own login (your Home account)") {
				c = strings.ReplaceAll(c, "a chottag account", "an account")
			}
			for _, m := range chottagCmd.FindAllStringSubmatch(c, -1) {
				if !words[m[1]] {
					t.Errorf("%s tells the reader to run `chottag %s`, which is not a chottag command (in %q)", doc, m[1], c)
				}
			}
		}
	}
}

func TestREADMECarriesTheSpecSections(t *testing.T) {
	readme := read(t, "README.md")
	for _, want := range []string{
		"## Install", "## Quick start", "## For Claude Code", "## Terms of use and risk", "## Uninstall", "## Credits",
		"./install.sh",
		"ordinary, individual usage", "cswap-pin", "--purge", "~/.chottag/versions",
		"600000", "/login", "~/.claude.json",
		"## Auto-switch", "chottag auto off", "cache-optimize", "chottag plan B max20x",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q (M3 spec §5)", want)
		}
	}
}

// TestREADMETermsOfUseAndRisk (spec part 4, P4-R8): the old Policy note
// read as reassurance ("no rule was found"). The section that replaces it
// sends the reader to Anthropic's own terms, names the credential rules on
// the Claude Code legal page, disclaims warranty, and still says auto-switch
// is on and how to turn it off (R73).
func TestREADMETermsOfUseAndRisk(t *testing.T) {
	readme := read(t, "README.md")
	if strings.Contains(readme, "(planned)") {
		t.Error("README still calls auto-switch planned")
	}
	s, ok := doccheck.Find(readme, 2, "Terms of use and risk")
	if !ok {
		t.Fatal("README has no ## Terms of use and risk")
	}
	body := flat(s.Body)
	for _, want := range []string{
		"accounts that are yours", "never to share or resell",
		"https://www.anthropic.com/legal/consumer-terms",
		"https://www.anthropic.com/legal/aup",
		"https://code.claude.com/docs/en/legal-and-compliance",
		"ordinary, individual usage", "may not collect, store, or intermediate", "credentials",
		"swaps in each account's token", "decide for yourself", "suspend", "No warranty", "Apache-2.0, sections 7 and 8",
		"your own risk", "on by default", "chottag auto off", "only if that organization",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("## Terms of use and risk lacks %q:\n%s", want, body)
		}
	}
	for _, reassurance := range []string{"was found against", "No rule", "## Policy"} {
		if strings.Contains(readme, reassurance) {
			t.Errorf("README still says %q: the terms section must not read as reassurance", reassurance)
		}
	}
}

// A Claude session that reads only the top of the README, or only its Install
// section, must still be sent to "For Claude Code", which is the only place the
// plugin step lives (M3 live check: a session read only "## Install" and
// skipped the plugin).
func TestREADMEPointsClaudeAtItsSection(t *testing.T) {
	readme := read(t, "README.md")
	install := strings.Index(readme, "## Install")
	if install < 0 {
		t.Fatal("README has no ## Install")
	}
	next := strings.Index(readme[install+len("## Install"):], "\n## ")
	if next < 0 {
		t.Fatal("## Install is the last section")
	}
	for name, part := range map[string]string{
		"the intro":           readme[:install],
		"the Install section": readme[install : install+len("## Install")+next],
	} {
		if !strings.Contains(part, "(#for-claude-code)") {
			t.Errorf("%s does not link to #for-claude-code", name)
		}
	}
}
