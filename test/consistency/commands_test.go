package consistency

import (
	"go/ast"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/doccheck"
)

// commandHeading is a command section's title in docs/commands.md:
// ### `chottag daemon logs`
var commandHeading = regexp.MustCompile("^`chottag ([a-z][a-z -]*[a-z])`$")

// commandSections maps each level-3 "`chottag PATH`" section of
// docs/commands.md to that section. A path documented twice fails.
func commandSections(t *testing.T) map[string]doccheck.Section {
	t.Helper()
	out := map[string]doccheck.Section{}
	for _, s := range doccheck.Sections(read(t, "docs/commands.md")) {
		m := commandHeading.FindStringSubmatch(s.Title)
		if s.Level != 3 || m == nil {
			continue
		}
		if _, dup := out[m[1]]; dup {
			t.Errorf("docs/commands.md documents `chottag %s` twice", m[1])
		}
		out[m[1]] = s
	}
	return out
}

// commandsSection is the level-2 section of docs/commands.md titled title;
// callers read its Lead, the text before its first subheading.
func commandsSection(t *testing.T, title string) doccheck.Section {
	t.Helper()
	s, ok := doccheck.Find(read(t, "docs/commands.md"), 2, title)
	if !ok {
		t.Fatalf("docs/commands.md has no ## %s", title)
	}
	return s
}

// sectionFlags is the set of flags a command section's table rows document
// (a first cell starting with "-", its first word), without dashes.
func sectionFlags(body string) map[string]bool {
	out := map[string]bool{}
	for _, c := range firstCells(body) {
		if strings.HasPrefix(c, "-") {
			w := strings.Fields(c)[0]
			out[strings.TrimLeft(w, "-")] = true
		}
	}
	return out
}

// TestCommandsDocHasASectionPerCommand (P3-R1): every command path the usage
// text or a flag set knows has its own section, and no section names one
// that doesn't exist.
func TestCommandsDocHasASectionPerCommand(t *testing.T) {
	known := knownPaths(t)
	got := commandSections(t)
	for _, p := range sortedKeys(known) {
		if _, ok := got[p]; !ok {
			t.Errorf("docs/commands.md has no ### `chottag %s` section", p)
		}
	}
	for p := range got {
		if !known[p] {
			t.Errorf("docs/commands.md documents `chottag %s`, which is not a chottag command", p)
		}
	}
}

// TestCommandsDocListsExactlyEachCommandsFlags (P3-R1): a section's flag
// table lists exactly the flags its command's flag set defines, plus those
// of the command it delegates to; --json and --help are documented once,
// under "Global flags and help".
func TestCommandsDocListsExactlyEachCommandsFlags(t *testing.T) {
	sets := cliFlagSets(t)
	for path, s := range commandSections(t) {
		want := flagsFor(sets, path)
		got := sectionFlags(s.Body)
		for _, f := range sortedKeys(want) {
			if !got[f] {
				t.Errorf("`chottag %s` section lacks a row for its flag --%s", path, f)
			}
		}
		for _, f := range sortedKeys(got) {
			if !want[f] {
				t.Errorf("`chottag %s` section documents --%s, which it does not take", path, f)
			}
		}
	}
}

// TestCommandsDocNamesEveryAlias: each alias dispatch accepts is named in
// its target command's section.
func TestCommandsDocNamesEveryAlias(t *testing.T) {
	secs := commandSections(t)
	for alias, target := range cliAliases(t) {
		if s, ok := secs[target]; !ok || !strings.Contains(s.Body, "`chottag "+alias+"`") {
			t.Errorf("the `chottag %s` section does not name its alias `chottag %s`", target, alias)
		}
	}
}

// TestCommandsDocGlobalFlagsAndHelp: --json, -h/--help and `chottag help`
// are documented once, in their own section.
func TestCommandsDocGlobalFlagsAndHelp(t *testing.T) {
	s := commandsSection(t, "Global flags and help")
	for _, want := range []string{"--json", "-h", "--help"} {
		row := false
		for _, l := range strings.Split(s.Lead, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "|") && strings.Contains(l, "`"+want+"`") {
				row = true
			}
		}
		if !row {
			t.Errorf("## Global flags and help has no table row for `%s`", want)
		}
	}
	if !strings.Contains(s.Lead, "`chottag help`") {
		t.Error("## Global flags and help does not mention `chottag help`")
	}
}

// TestCommandsDocListsEveryExitCode (P3-R1): the exit-code table lists
// exactly the codes internal/exit declares.
func TestCommandsDocListsEveryExitCode(t *testing.T) {
	want := map[string]bool{}
	for _, v := range intConsts(t, "internal/exit") {
		want[strconv.Itoa(v)] = true
	}
	if len(want) == 0 {
		t.Fatal("internal/exit declares no codes")
	}
	got := map[string]bool{}
	for _, c := range firstCells(commandsSection(t, "Exit codes").Lead) {
		got[c] = true
	}
	for _, c := range sortedKeys(want) {
		if !got[c] {
			t.Errorf("## Exit codes lacks exit %s", c)
		}
	}
	for _, c := range sortedKeys(got) {
		if !want[c] {
			t.Errorf("## Exit codes lists exit %s, which internal/exit does not declare", c)
		}
	}
}

// doctorIDs are the check ids internal/doctor declares: every `ID: "…"`
// in a Check literal, with "token:" + name written "token:NAME".
func doctorIDs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	eachFile(t, "internal/doctor", func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "ID" {
				return true
			}
			switch v := kv.Value.(type) {
			case *ast.BasicLit:
				s, _ := strconv.Unquote(v.Value)
				out[s] = true
			case *ast.BinaryExpr:
				if lit, ok := v.X.(*ast.BasicLit); ok {
					s, _ := strconv.Unquote(lit.Value)
					out[s+"NAME"] = true
				}
			}
			return true
		})
	})
	return out
}

// TestCommandsDocListsEveryDoctorCheck: the doctor section's check table
// lists exactly the checks internal/doctor runs.
func TestCommandsDocListsEveryDoctorCheck(t *testing.T) {
	s, ok := commandSections(t)["doctor"]
	if !ok {
		t.Fatal("no `chottag doctor` section")
	}
	want := doctorIDs(t)
	got := map[string]bool{}
	for _, c := range firstCells(s.Body) {
		if !strings.HasPrefix(c, "-") {
			got[c] = true
		}
	}
	for _, id := range sortedKeys(want) {
		if !got[id] {
			t.Errorf("the doctor section lacks a row for check %q", id)
		}
	}
	for _, id := range sortedKeys(got) {
		if !want[id] {
			t.Errorf("the doctor section lists check %q, which internal/doctor does not run", id)
		}
	}
}

// TestCommandsDocOwnListsEveryKind: `chottag own` names every object kind
// the router knows.
func TestCommandsDocOwnListsEveryKind(t *testing.T) {
	s, ok := commandSections(t)["own"]
	if !ok {
		t.Fatal("no `chottag own` section")
	}
	kinds := stringConsts(t, "internal/router", "Kind")
	if len(kinds) == 0 {
		t.Fatal("internal/router declares no Kind constants")
	}
	for _, k := range kinds {
		if !strings.Contains(s.Body, "`"+k+"`") {
			t.Errorf("the own section does not name kind `%s`", k)
		}
	}
}
