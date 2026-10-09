// Package plugin validates the Claude Code plugin in this repo without
// running claude (CI has none; `claude plugin validate` runs in the live
// check only, M3 spec §2): the marketplace file at the repo root, the
// plugin manifest it points at, and the chottag skill.
package plugin

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// decodeOne decodes exactly one JSON document from path into v; trailing
// content is an error.
func decodeOne(t *testing.T, path string, v any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("%s: trailing content after the JSON document (%v)", path, err)
	}
}

type marketplace struct {
	Name  string `json:"name"`
	Owner struct {
		Name string `json:"name"`
	} `json:"owner"`
	Plugins []struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		Description string `json:"description"`
	} `json:"plugins"`
}

type manifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
}

func loadMarketplace(t *testing.T) marketplace {
	t.Helper()
	var m marketplace
	decodeOne(t, filepath.Join(repoRoot(t), ".claude-plugin", "marketplace.json"), &m)
	return m
}

func TestMarketplaceListsTheOnePlugin(t *testing.T) {
	m := loadMarketplace(t)
	if m.Name != "c-hottag" {
		t.Errorf("marketplace name = %q, want %q (the README's install id is chottag@c-hottag)", m.Name, "c-hottag")
	}
	if m.Owner.Name == "" {
		t.Error("marketplace owner.name is empty; Claude Code requires it")
	}
	if len(m.Plugins) != 1 {
		t.Fatalf("plugins = %d entries, want exactly 1", len(m.Plugins))
	}
	p := m.Plugins[0]
	if p.Name != "chottag" {
		t.Errorf("plugins[0].name = %q, want %q", p.Name, "chottag")
	}
	// A relative source must start with "./" and is resolved from the
	// marketplace root (the repo root), not from .claude-plugin/.
	if p.Source != "./plugin" {
		t.Errorf("plugins[0].source = %q, want %q", p.Source, "./plugin")
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), p.Source, ".claude-plugin", "plugin.json")); err != nil {
		t.Errorf("source %q holds no .claude-plugin/plugin.json: %v", p.Source, err)
	}
}

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func TestPluginManifest(t *testing.T) {
	var p manifest
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", ".claude-plugin", "plugin.json"), &p)
	if want := loadMarketplace(t).Plugins[0].Name; p.Name != want {
		t.Errorf("plugin.json name = %q, want the marketplace's %q", p.Name, want)
	}
	if p.Description == "" || p.Author.Name == "" {
		t.Errorf("plugin.json needs a description and author.name: %+v", p)
	}
	if !semver.MatchString(p.Version) {
		t.Errorf("plugin.json version = %q, want MAJOR.MINOR.PATCH", p.Version)
	}
}

// No legacy commands/ dir (N3): the one skill is both /chottag:chottag
// and model-invocable.
func TestNoLegacyCommandsDir(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot(t), "plugin", "commands")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plugin/commands exists (err %v); M3 ships one skill and no commands/ (N3)", err)
	}
}

// skill reads SKILL.md and splits it into its frontmatter fields and body.
// The frontmatter is the lines between a first line "---" and the next
// "---"; each is "key: value", and a value in double quotes is unquoted.
// Only single-line values are supported, which the test also requires.
func skill(t *testing.T) (fields map[string]string, body string, lines int) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "plugin", "skills", "chottag", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	all := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(all) == 0 || all[0] != "---" {
		t.Fatal("SKILL.md does not start with a --- frontmatter line")
	}
	end := -1
	for i := 1; i < len(all); i++ {
		if all[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatal("SKILL.md frontmatter has no closing ---")
	}
	fields = map[string]string{}
	for _, l := range all[1:end] {
		k, v, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(l, " ") {
			t.Fatalf("frontmatter line %q is not a single-line key: value", l)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		fields[strings.TrimSpace(k)] = v
	}
	return fields, strings.Join(all[end+1:], "\n"), len(all)
}

func TestSkillFrontmatter(t *testing.T) {
	f, _, lines := skill(t)
	if f["name"] != "chottag" {
		t.Errorf("name = %q, want %q (it must match the directory)", f["name"], "chottag")
	}
	d := f["description"]
	// Claude Code truncates a skill description past 1,536 characters
	// (M3 spec §2). The test counts runes, so 1,536 passes and 1,537 fails.
	if n := len([]rune(d)); n == 0 || n > 1536 {
		t.Errorf("description is %d characters, want 1..1536", n)
	}
	for _, w := range []string{"account", "chottag", "limit"} {
		if !strings.Contains(strings.ToLower(d), w) {
			t.Errorf("description lacks %q: it is what makes the skill model-invocable", w)
		}
	}
	want := "Bash(chottag status:*), Bash(chottag doctor --json), Bash(chottag update --check --json), Bash(chottag version:*), Bash(chottag tag:*), Bash(chottag next:*), Bash(chottag remote:*), Bash(chottag rotate:*), Bash(chottag auto:*), Bash(chottag notify:*), Bash(chottag plan:*), Bash(chottag sessions:*), Bash(chottag names), Bash(chottag names --json)"
	if f["allowed-tools"] != want {
		t.Errorf("allowed-tools = %q, want %q", f["allowed-tools"], want)
	}
	for _, bad := range []string{"chottag:*", "chottag logout", "chottag login", "chottag uninstall", "chottag setup", "chottag doctor:*", "chottag update:*", "chottag daemon", "chottag trace", "chottag proxy", "chottag own", "chottag adopt"} {
		if strings.Contains(f["allowed-tools"], bad) {
			t.Errorf("allowed-tools pre-approves %q (L6)", bad)
		}
	}
	// Under 500 lines (spec §2): 499 passes, 500 fails.
	if lines >= 500 {
		t.Errorf("SKILL.md has %d lines, want fewer than 500", lines)
	}
}

// TestSkillCarriesTheRules pins each rule of M3 spec §2 to a phrase the
// skill must contain. Rewording is fine; dropping a rule is not.
func TestSkillCarriesTheRules(t *testing.T) {
	_, body, _ := skill(t)
	for _, c := range []struct{ phrase, rule string }{
		{"--json", "always pass --json"},
		{"error.code", "branch on ok / error.code, never on the message"},
		{"chottag login", "log accounts in with chottag login"},
		{"600000", "login's Bash timeout is the 10-minute maximum"},
		{"/login", "never in-session /login"},
		{"/logout", "never in-session /logout"},
		{"next request", "tag/next apply from the session's next request"},
		{"~/.claude.json", "never edit ~/.claude or ~/.claude.json"},
		{"statusLine", "the status-line snippet the user applies"},
		{"chottag doctor --json", "troubleshooting"},
		{"chottag $ARGUMENTS --json", "the with-arguments form"},
		{"uninstall --purge", "never run --purge for the user"},
		{"chottag auto off --json", "auto-switch is on by default and can be turned off (M4)"},
		{"chottag plan <name>", "set a plan size (M4)"},
		{"only needs resending", "a message refused before a switch just needs resending (M4)"},
	} {
		if !strings.Contains(body, c.phrase) {
			t.Errorf("SKILL.md lacks %q (%s)", c.phrase, c.rule)
		}
	}
}

// TestDisplayNamesSayCHottagAndIdsAreChottag pins R92: the project's
// display name is c-hottag (R84), and the machine ids are chottag@c-hottag
// with skill chottag. The old name appears in no description.
func TestDisplayNamesSayCHottagAndIdsAreChottag(t *testing.T) {
	old := "claude-" + "hottag"
	m := loadMarketplace(t)
	if m.Name != "c-hottag" || m.Plugins[0].Name != "chottag" {
		t.Errorf("ids = %s@%s, want chottag@c-hottag (R92)", m.Plugins[0].Name, m.Name)
	}
	var mk struct {
		Metadata struct {
			Description string `json:"description"`
		} `json:"metadata"`
	}
	decodeOne(t, filepath.Join(repoRoot(t), ".claude-plugin", "marketplace.json"), &mk)
	if !strings.HasPrefix(mk.Metadata.Description, "c-hottag: ") {
		t.Errorf("marketplace description = %q, want it to start with \"c-hottag: \"", mk.Metadata.Description)
	}
	var p manifest
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", ".claude-plugin", "plugin.json"), &p)
	if !strings.Contains(p.Description, "(c-hottag)") || strings.Contains(p.Description, old) {
		t.Errorf("plugin.json description = %q, want the display name c-hottag", p.Description)
	}
	fields, _, _ := skill(t)
	if fields["name"] != "chottag" || !strings.Contains(fields["description"], "(c-hottag)") || strings.Contains(fields["description"], old) {
		t.Errorf("SKILL.md name %q, description %q; want name chottag and the display name c-hottag", fields["name"], fields["description"])
	}
}

// usageConst is internal/cli/cli.go's usage const, read as text.
func usageConst(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "cli", "cli.go"))
	if err != nil {
		t.Fatal(err)
	}
	const open = "const usage = `"
	src := string(b)
	i := strings.Index(src, open)
	if i < 0 {
		t.Fatal("cli.go has no usage const")
	}
	body := src[i+len(open):]
	return body[:strings.Index(body, "`")]
}

// notInSkill lists usage items the plugin skill legitimately does not
// document, each with a reason. Keep it short: add the line to the skill
// instead (R128).
var notInSkill = map[string]string{
	"--listen": "trace env's flag; trace is a developer tool the skill tells the agent not to run (rule 7)",
}

// TestSkillAndCommandsPageCoverTheUsageText (R128): every top-level command
// and every --flag in the usage text is named in both the plugin skill and
// docs/commands.md, so a new command or flag cannot ship undocumented.
func TestSkillAndCommandsPageCoverTheUsageText(t *testing.T) {
	root := repoRoot(t)
	docs := map[string]string{}
	for _, rel := range []string{"plugin/skills/chottag/SKILL.md", "docs/commands.md"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		docs[rel] = string(b)
	}
	cmdWord := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	flagTok := regexp.MustCompile(`--[a-z][a-z-]*`)
	cmds := map[string]bool{}
	flags := map[string]bool{}
	for _, l := range strings.Split(usageConst(t), "\n") {
		rest, ok := strings.CutPrefix(l, "  ")
		if !ok {
			continue
		}
		syn, _, _ := strings.Cut(rest, "  ")
		if f := strings.Fields(syn); len(f) > 0 && cmdWord.MatchString(f[0]) {
			cmds[f[0]] = true
		}
		for _, f := range flagTok.FindAllString(syn, -1) {
			flags[f] = true
		}
	}
	if len(cmds) < 10 || len(flags) < 10 {
		t.Fatalf("parsed %d commands and %d flags from the usage text: the parse is broken", len(cmds), len(flags))
	}
	for rel, text := range docs {
		for c := range cmds {
			if _, skip := notInSkill[c]; skip && strings.HasSuffix(rel, "SKILL.md") {
				continue
			}
			if !regexp.MustCompile("(chottag |`)" + regexp.QuoteMeta(c) + `\b`).MatchString(text) {
				t.Errorf("command %q from the usage text is not in %s: document it (R128)", c, rel)
			}
		}
		for f := range flags {
			if _, skip := notInSkill[f]; skip && strings.HasSuffix(rel, "SKILL.md") {
				continue
			}
			if !strings.Contains(text, f) {
				t.Errorf("flag %q from the usage text is not in %s: document it (R128)", f, rel)
			}
		}
	}
}
