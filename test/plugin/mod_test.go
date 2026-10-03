package plugin

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The mod (plugin/hooks) is plain JavaScript and the gate has no JS runtime
// (CI has no claude), so these are file reads. The mod's own behaviour is
// tested by plugin/hooks/chottag-mod.test.ts, run by `claude plugin test
// plugin` before a release.

func readFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func modSource(t *testing.T) string {
	t.Helper()
	return readFile(t, "plugin", "hooks", "chottag-mod.js")
}

func TestModFilesExistAndHooksJSONNamesTheModule(t *testing.T) {
	var h struct {
		Description string   `json:"description"`
		Modules     []string `json:"modules"`
	}
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", "hooks", "hooks.json"), &h)
	if len(h.Modules) != 1 || h.Modules[0] != "./chottag-mod.js" || h.Description != "chottag mod" {
		t.Fatalf("hooks.json = %+v, want the one module ./chottag-mod.js", h)
	}
	for _, f := range []string{"chottag-mod.js", "chottag-mod.test.ts"} {
		if _, err := os.Stat(filepath.Join(repoRoot(t), "plugin", "hooks", f)); err != nil {
			t.Errorf("plugin/hooks/%s: %v", f, err)
		}
	}
}

func TestModNeverNamesChottagFilesOrASecret(t *testing.T) {
	src := modSource(t)
	for _, bad := range []string{".chottag/accounts", "credentials", "proxy.secret", "state.json", "status.json", "Keychain"} {
		if strings.Contains(src, bad) {
			t.Errorf("chottag-mod.js mentions %q: the mod talks to chottag only through the CLI", bad)
		}
	}
}

// The proxy variable is read once, and only to test its user part.
func TestModOnlyTestsTheProxyVariablesUserPart(t *testing.T) {
	src := modSource(t)
	if !strings.Contains(src, "const ROUTED_USER = /^https?:\\/\\/chottag[.:@]/") {
		t.Error("the user-part test must be /^https?:\\/\\/chottag[.:@]/")
	}
	n := 0
	for _, l := range strings.Split(src, "\n") {
		if !strings.Contains(l, "HTTPS_PROXY") || strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		n++
		if !strings.Contains(l, "isChottagProxy(await $.env.get('HTTPS_PROXY'))") {
			t.Errorf("HTTPS_PROXY is used beyond the user-part test: %s", strings.TrimSpace(l))
		}
	}
	if n != 1 {
		t.Errorf("HTTPS_PROXY appears on %d code lines, want exactly the one test", n)
	}
	// Whatever isChottagProxy gets is only matched: never logged, shown or stored.
	body := src[strings.Index(src, "export function isChottagProxy"):]
	body = body[:strings.Index(body, "\n}\n")]
	if strings.Count(body, "\n") != 1 || !strings.Contains(body, "return ROUTED_USER.test(value || '')") {
		t.Errorf("isChottagProxy does more than test the value:\n%s", body)
	}
}

func TestModProcessRunTakesAnArrayAndNoShell(t *testing.T) {
	src := modSource(t)
	calls := regexp.MustCompile(`\$\.process\.run\(\s*([^\s])`).FindAllStringSubmatch(src, -1)
	if len(calls) == 0 {
		t.Fatal("no $.process.run call found")
	}
	for _, c := range calls {
		if c[1] != "[" {
			t.Errorf("$.process.run must take an argument array, found %q", c[0])
		}
	}
	for _, bad := range []string{"'sh'", "\"sh\"", "bash", "zsh", "'-c'", "exec(", "$.process.spawn", "shell:"} {
		if strings.Contains(src, bad) {
			t.Errorf("chottag-mod.js uses %q: arguments go to the CLI without a shell", bad)
		}
	}
}

func modSection(t *testing.T) string {
	t.Helper()
	doc := readFile(t, "docs", "commands.md")
	i := strings.Index(doc, "\n## Claude Code mod\n")
	if i < 0 {
		t.Fatal("docs/commands.md has no ## Claude Code mod")
	}
	rest := doc[i+1:]
	if j := strings.Index(rest[3:], "\n## "); j >= 0 {
		rest = rest[:j+3]
	}
	return rest
}

// /ct's subcommands in the mod are exactly the ones docs/commands.md lists.
func TestModCtVerbsMatchTheDocs(t *testing.T) {
	src := modSource(t)
	m := regexp.MustCompile(`const CT_VERBS = \[([^\]]*)\]`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("chottag-mod.js has no CT_VERBS list")
	}
	var verbs []string
	for _, q := range regexp.MustCompile(`'([a-z]+)'`).FindAllStringSubmatch(m[1], -1) {
		verbs = append(verbs, q[1])
	}
	sort.Strings(verbs)
	var docVerbs []string
	for _, c := range regexp.MustCompile("`/ct ([a-z]+)[ `,]").FindAllStringSubmatch(modSection(t), -1) {
		docVerbs = append(docVerbs, c[1])
	}
	docVerbs = dedupe(docVerbs)
	if strings.Join(verbs, ",") != strings.Join(docVerbs, ",") {
		t.Errorf("mod verbs %v, docs/commands.md lists %v", verbs, docVerbs)
	}
	if !strings.Contains(src, "name: 'ct'") {
		t.Error("the mod must register /ct (/chottag is the skill's)")
	}
	// Every verb the mod runs is a real chottag command.
	usage := readFile(t, "internal", "cli", "cli.go")
	for _, v := range []string{"status", "next", "tag", "pool"} {
		if !strings.Contains(usage, "\n  "+v+" ") {
			t.Errorf("/ct %s runs a chottag command that is not in the usage text", v)
		}
	}
}

func dedupe(in []string) []string {
	sort.Strings(in)
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// The guard's text is the one the spec gives, in the code and verbatim in the docs.
func TestModGuardTextIsVerbatimInTheDocs(t *testing.T) {
	const login = "`/login` here would change Claude Code's own login (your Home account), not a chottag account. To add or repair a chottag account: `chottag login <name>` in a terminal. To change Home anyway: start a session with `CHOTTAG_BYPASS=1 claude`, or run `claude auth login` in a terminal."
	logout := strings.Replace(login, "`/login` here would change", "`/logout` here would log out of", 1)

	src := modSource(t)
	start := strings.Index(src, "export const GUARD_LOGIN =")
	end := strings.Index(src, "export const GUARD_LOGOUT")
	if start < 0 || end < start {
		t.Fatal("chottag-mod.js has no GUARD_LOGIN")
	}
	var got strings.Builder
	for _, q := range regexp.MustCompile(`(?s)'([^'\n]*)'|"([^"\n]*)"`).FindAllStringSubmatch(src[start:end], -1) {
		got.WriteString(q[1] + q[2])
	}
	if got.String() != login {
		t.Errorf("GUARD_LOGIN in the mod is\n%s\nwant\n%s", got.String(), login)
	}
	if !strings.Contains(src, "GUARD_LOGIN.replace('`/login` here would change', '`/logout` here would log out of')") {
		t.Error("GUARD_LOGOUT must be the login text with \"log out of\" in place of \"change\"")
	}
	sec := modSection(t)
	for _, want := range []string{login, logout} {
		if !strings.Contains(sec, want) {
			t.Errorf("docs/commands.md's mod section does not carry the guard text verbatim:\n%s", want)
		}
	}
}

func TestModIsNamedByThePluginManifestAndTheSkill(t *testing.T) {
	var m manifest
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", ".claude-plugin", "plugin.json"), &m)
	if !strings.Contains(m.Description, "mod") {
		t.Errorf("plugin.json's description should mention the mod: %q", m.Description)
	}
	skill := readFile(t, "plugin", "skills", "chottag", "SKILL.md")
	for _, want := range []string{"/ct", "`/login`", "2.1.287"} {
		if !strings.Contains(skill, want) {
			t.Errorf("SKILL.md does not mention %s", want)
		}
	}
}

// The mod's version constant is what it compares chottag's version with, so it
// must be the plugin's own version: `scripts/release bump` sets both.
func TestModVersionEqualsPluginJSONVersion(t *testing.T) {
	var p struct {
		Version string `json:"version"`
	}
	decodeOne(t, filepath.Join(repoRoot(t), "plugin", ".claude-plugin", "plugin.json"), &p)
	m := regexp.MustCompile(`(?m)^export const MOD_VERSION = '([^']+)'$`).FindStringSubmatch(modSource(t))
	if m == nil {
		t.Fatal("chottag-mod.js has no `export const MOD_VERSION = 'X.Y.Z'` line")
	}
	if m[1] != p.Version {
		t.Errorf("MOD_VERSION = %q, plugin.json version = %q: scripts/release bump sets both", m[1], p.Version)
	}
	// scripts/release is private: a public snapshot has no release script.
	rel := filepath.Join(repoRoot(t), "scripts", "release")
	if _, err := os.Stat(rel); os.IsNotExist(err) {
		return
	}
	if !strings.Contains(readFile(t, "scripts", "release"), "MOD_VERSION") {
		t.Error("scripts/release bump does not set MOD_VERSION")
	}
}
