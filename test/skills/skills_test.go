// Package skills statically checks .claude/skills/upstream-check (R94):
// SKILL.md's frontmatter, that every script it mentions exists and is
// executable, that each script has the sh -eu shape scripts/dev-env's own
// scripts use, and that none of them calls a bare `chottag` at command
// position other than through scripts/dev-env or the read-only
// `doctor --json` / `status --json` forms. It never spawns a process
// (F232 forbids a test starting go or sh without its own allowlist entry):
// every check here reads file contents and file mode only.
package skills

import (
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

const skillName = "upstream-check"

func skillPath(t *testing.T, parts ...string) string {
	t.Helper()
	return filepath.Join(append([]string{repoRoot(t), ".claude", "skills", skillName}, parts...)...)
}

// scriptFiles is every script this skill ships, under its own scripts/.
var scriptFiles = []string{"detect", "diff-routes", "trace"}

// frontmatter parses SKILL.md's single-line "key: value" frontmatter
// (test/plugin's own skill() helper parses the product skill the same way).
func frontmatter(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		t.Fatalf("%s does not start with a --- frontmatter line", path)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		t.Fatalf("%s frontmatter has no closing ---", path)
	}
	fields := map[string]string{}
	for _, l := range lines[1:end] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			t.Fatalf("%s frontmatter line %q is not key: value", path, l)
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return fields
}

func TestSkillFrontmatterName(t *testing.T) {
	f := frontmatter(t, skillPath(t, "SKILL.md"))
	if f["name"] != skillName {
		t.Errorf("name = %q, want %q", f["name"], skillName)
	}
	if f["description"] == "" {
		t.Error("description is empty")
	}
}

var scriptMentionRe = regexp.MustCompile(`scripts/[A-Za-z0-9_-]+`)

// TestSkillMDScriptsExistAndAreExecutable checks every "scripts/<name>"
// SKILL.md mentions: either this skill's own scripts/<name>, or the
// repo-root scripts/<name> (scripts/dev-env), whichever exists.
func TestSkillMDScriptsExistAndAreExecutable(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(skillPath(t, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range scriptMentionRe.FindAllString(string(b), -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		name := strings.TrimPrefix(m, "scripts/")
		candidates := []string{
			filepath.Join(root, ".claude", "skills", skillName, "scripts", name),
			filepath.Join(root, "scripts", name),
		}
		found := false
		for _, c := range candidates {
			fi, err := os.Stat(c)
			if err != nil || fi.IsDir() {
				continue
			}
			found = true
			if fi.Mode()&0o111 == 0 {
				t.Errorf("%s is not executable (mode %s)", c, fi.Mode())
			}
			break
		}
		if !found {
			t.Errorf("SKILL.md mentions %q, but neither %s nor %s exists", m, candidates[0], candidates[1])
		}
	}
}

// TestScriptsHaveTheShShape checks every script under this skill's own
// scripts/ starts with #!/bin/sh, contains "set -eu", and is executable
// (dev-env's own house style, spelled out in the brief).
func TestScriptsHaveTheShShape(t *testing.T) {
	for _, name := range scriptFiles {
		path := skillPath(t, "scripts", name)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %s)", path, fi.Mode())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		first, _, _ := strings.Cut(text, "\n")
		if first != "#!/bin/sh" {
			t.Errorf("%s's first line is %q, want %q", path, first, "#!/bin/sh")
		}
		if !strings.Contains(text, "set -eu") {
			t.Errorf("%s has no \"set -eu\"", path)
		}
	}
}

// chottagCmdRe matches a "chottag" invocation at command position: line
// start, or right after a ;/&/| segment boundary (with optional
// whitespace), a bare "chottag" or a path ending in "/chottag" (never a
// variable like $prod_chottag or a substring like "chottag-dev": both need
// a literal, unbroken "chottag" right at that position), followed by
// whitespace and its first one or two argument words.
var chottagCmdRe = regexp.MustCompile(`(?:^|[;&|]\s*)(?:\S*/)?chottag\s+(\S+(?:\s+\S+)?)`)

// TestNoScriptCallsChottagDirectly is R91/R94's rule for this skill's own
// scripts: every chottag invocation goes through scripts/dev-env (which
// manages its own env) or is one of the two read-only forms the guard
// allows against the prod install. A simple line scan (F232).
func TestNoScriptCallsChottagDirectly(t *testing.T) {
	for _, name := range scriptFiles {
		path := skillPath(t, "scripts", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "#") {
				continue // a comment, not a command
			}
			if strings.Contains(line, "scripts/dev-env") {
				continue // dev-env manages its own env
			}
			for _, m := range chottagCmdRe.FindAllStringSubmatch(line, -1) {
				words := strings.Fields(m[1])
				first, second := "", ""
				if len(words) > 0 {
					first = words[0]
				}
				if len(words) > 1 {
					second = words[1]
				}
				if (first == "doctor" || first == "status") && second == "--json" {
					continue
				}
				t.Errorf("%s:%d: bare chottag invocation %q is neither read-only (doctor|status --json) nor through scripts/dev-env", path, i+1, strings.TrimSpace(line))
			}
		}
	}
}
