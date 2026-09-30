// Package export tests scripts/export-public against throwaway git
// repositories under t.TempDir(): a fake private repo with tags, and a fake
// public clone whose origin is a local bare repo that must never gain a ref.
// It never runs the export against this repository, never uses the network,
// and never sees the real HOME. Leak-shaped fixtures are assembled at run
// time, so this file never trips the scan CI runs over test/.
package export

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const publicAuthor = "c-hottag <c-hottag@users.noreply.github.com>"

func repoRoot(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type env struct {
	t    *testing.T
	home string
	priv string // the fake private repo: the export runs here
	pub  string // the fake public clone
	bare string // pub's origin
}

func (e *env) vars() []string {
	return []string{
		"HOME=" + e.home, "TMPDIR=" + e.home, "PATH=" + os.Getenv("PATH"), "LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(e.home, "gitconfig"),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com",
	}
}

// git runs git in dir and returns its trimmed stdout; any failure is fatal.
func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.vars()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		e.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// write puts files (slash path -> content) into the private repo's work tree.
func (e *env) write(files map[string]string) {
	e.t.Helper()
	for rel, body := range files {
		p := filepath.Join(e.priv, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			e.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			e.t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) commitAndTag(tag string) {
	e.t.Helper()
	e.git(e.priv, "add", "-A")
	e.git(e.priv, "commit", "-q", "--allow-empty", "-m", "fixture "+tag)
	e.git(e.priv, "tag", tag)
}

// fixtureGate's third line pins I1(b): the gate copy is a git work tree.
const fixtureGate = "# fixture\n\n- The gate, exactly:\n```sh\ntest -f README.md\ntest -x run.sh\n[ \"$(git rev-parse --is-inside-work-tree)\" = true ]\n```\n"

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	e := &env{t: t, home: t.TempDir(), priv: filepath.Join(base, "priv"), pub: filepath.Join(base, "pub"), bare: filepath.Join(base, "bare.git")}
	if err := os.WriteFile(filepath.Join(e.home, "gitconfig"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.git(base, "init", "-q", "-b", "main", e.priv)
	e.write(map[string]string{
		"README.md":                       "c-hottag fixture\n",
		"run.sh":                          "#!/bin/sh\necho ok\n",
		"internal/a.go":                   "package a\n",
		"secret.txt":                      "no manifest line selects this\n",
		"docs/release-notes/v1.md":        "notes\n",
		"docs/internal/notes.md":          "private notes\n",
		"docs/internal/leak-patterns.txt": "# fixture patterns\nzebracorp\n",
		"public-manifest.txt":             "README.md\nCLAUDE.md\nrun.sh\ninternal/\ndocs/\npublic-manifest.txt\n!docs/internal/\n",
		"CLAUDE.md":                       fixtureGate,
	})
	e.commitAndTag("v1.2.3")
	e.git(base, "init", "-q", "--bare", e.bare)
	e.git(base, "init", "-q", "-b", "main", e.pub)
	e.git(e.pub, "commit", "-q", "--allow-empty", "-m", "root")
	e.git(e.pub, "remote", "add", "origin", e.bare)
	return e
}

// export runs the real scripts/export-public from inside the fake private
// repo and returns stdout, stderr and the exit status.
func (e *env) export(args ...string) (string, string, int) {
	e.t.Helper()
	return e.exportScript(filepath.Join(repoRoot(e.t), "scripts", "export-public"), args...)
}

// exportScript is export, but runs the export-public copy at script (with
// whatever manifest-select and leak-scan sit beside it).
func (e *env) exportScript(script string, args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{script}, args...)...)
	cmd.Dir = e.priv
	cmd.Env = e.vars()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			e.t.Fatalf("export-public: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// exportWithPath runs the real scripts/export-public with extraBin
// prepended to PATH (ahead of the real tools), so a fake `git` there
// intercepts every git call the script makes. It never sleeps to bound
// the wait: a 60s hang guard only, event-driven via cmd.Wait().
func (e *env) exportWithPath(extraBin string, args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(repoRoot(e.t), "scripts", "export-public")}, args...)...)
	cmd.Dir = e.priv
	env := e.vars()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + extraBin + string(os.PathListSeparator) + os.Getenv("PATH")
		}
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("export-public: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				e.t.Fatalf("export-public: %v", err)
			}
			code = ee.ExitCode()
		}
		return out.String(), errb.String(), code
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		e.t.Fatal("export-public: 60s hang guard tripped")
		return "", "", 0
	}
}

// shQuote single-quotes s for embedding in a POSIX sh script literal.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gitWrapper writes a fake `git` into a fresh bin directory. The first
// time every one of need appears somewhere among its own arguments (in
// any position; export-public's git calls carry a variable-length
// -C/-c prefix), it runs action (a sh snippet) before ever touching the
// real git; every other call, and every later call once triggered, goes
// straight to the real git. Matching every token in need, not just one,
// matters: for example a bare "refs/tags/v1.2.3" also appears in an
// earlier, unrelated (and negated) existence check, so a tag-creation
// test needs both "update-ref" and the ref name present at once to hit
// the right call. action runs as a child of the script under test, so
// "$PPID" in it is that script's own process -- e.g. `kill -TERM
// "$PPID"` signals the running export-public, not this wrapper.
func gitWrapper(t *testing.T, dir, action string, need ...string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	bin := filepath.Join(dir, "wrapper-bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "wrapper-triggered")
	var flags, checks, cond strings.Builder
	for i, tok := range need {
		fmt.Fprintf(&flags, "f%d=0\n", i)
		fmt.Fprintf(&checks, "    case \"$a\" in\n    %s) f%d=1 ;;\n    esac\n", shQuote(tok), i)
		if i > 0 {
			cond.WriteString(" && ")
		}
		fmt.Fprintf(&cond, `[ "$f%d" -eq 1 ]`, i)
	}
	// wt captures export-public's own "--work-tree=$tmp/out" value, and
	// lastarg its final positional argument (e.g. the file `hash-object`
	// is about to read), on every call regardless of trigger, so an
	// action can reach into the script's own scratch directory (whose
	// path is otherwise invisible to the test) via "$wt"/"$lastarg"
	// without needing to know it in advance.
	script := "#!/bin/sh\n" +
		"real=" + shQuote(real) + "\n" +
		"marker=" + shQuote(marker) + "\n" +
		"wt=\n" +
		"lastarg=\n" +
		"for a in \"$@\"; do\n" +
		"  case \"$a\" in\n" +
		"  --work-tree=*) wt=${a#--work-tree=} ;;\n" +
		"  esac\n" +
		"  lastarg=$a\n" +
		"done\n" +
		"if [ ! -e \"$marker\" ]; then\n" +
		flags.String() +
		"  for a in \"$@\"; do\n" +
		checks.String() +
		"  done\n" +
		"  if " + cond.String() + "; then\n" +
		"    : >\"$marker\"\n" +
		action + "\n" +
		"  fi\n" +
		"fi\n" +
		"exec \"$real\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// pubState is everything a failed export must leave exactly as it was.
func (e *env) pubState() string {
	return e.git(e.pub, "rev-parse", "HEAD") + "|" + e.git(e.pub, "tag", "-l") + "|" +
		e.git(e.pub, "status", "--porcelain", "--untracked-files=all")
}

func (e *env) mustExport(tag string) {
	e.t.Helper()
	if out, errb, code := e.export(tag, e.pub); code != 0 {
		e.t.Fatalf("export %s: exit %d\n%s%s", tag, code, out, errb)
	}
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func TestExportCommitsOnlyTheManifestSelectedPaths(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	got := lines(e.git(e.pub, "ls-files"))
	want := []string{"CLAUDE.md", "README.md", "docs/release-notes/v1.md", "internal/a.go", "public-manifest.txt", "run.sh"}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("public clone holds %q, want %q (deny by default; ! beats an include)", got, want)
	}
}

func TestExportMakesOneCommitWithNoTrailersAndTagsIt(t *testing.T) {
	e := newEnv(t)
	hooks := filepath.Join(e.pub, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\necho 'Claude-" + "Session: https://example.com/x' >> \"$1\"\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.mustExport("v1.2.3")
	if n := e.git(e.pub, "rev-list", "--count", "HEAD"); n != "2" {
		t.Errorf("public clone has %s commits, want the root plus exactly one", n)
	}
	if who := e.git(e.pub, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); who != publicAuthor+"|"+publicAuthor {
		t.Errorf("author|committer = %q, want %q for both", who, publicAuthor)
	}
	raw := e.git(e.pub, "cat-file", "commit", "HEAD")
	if _, msg, _ := strings.Cut(raw, "\n\n"); msg != "c-hottag v1.2.3" {
		t.Errorf("commit message = %q, want exactly %q (no trailers, no hook edits)", msg, "c-hottag v1.2.3")
	}
	if tr := e.git(e.pub, "log", "-1", "--format=%(trailers)"); tr != "" {
		t.Errorf("commit carries trailers %q", tr)
	}
	if e.git(e.pub, "rev-parse", "v1.2.3^{commit}") != e.git(e.pub, "rev-parse", "HEAD") {
		t.Error("tag v1.2.3 does not point at the export commit")
	}
}

func TestExportKeepsExecutableBits(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	if s := e.git(e.pub, "ls-files", "-s", "run.sh"); !strings.HasPrefix(s, "100755 ") {
		t.Errorf("run.sh is %q in the public clone, want mode 100755", s)
	}
}

func TestExportFailsOnALeakAndCommitsNothing(t *testing.T) {
	cases := []struct {
		name, file, body, want string
	}{
		{"a private pattern", "README.md", "hello ZebraCorp\n", "README.md:1: private pattern 1"},
		{"a generic home path", "internal/b.go", "package a\n// " + "/" + "Users/zed/x\n", "internal/b.go:2: home-path"},
	}
	for _, c := range cases {
		e := newEnv(t)
		e.write(map[string]string{c.file: c.body})
		e.commitAndTag("v1.2.4")
		before := e.pubState()
		out, errb, code := e.export("v1.2.4", e.pub)
		if code != 1 || !strings.Contains(errb, c.want) || !strings.Contains(errb, "nothing was committed") {
			t.Errorf("%s: exit %d, stderr %q; want 1 naming %q", c.name, code, errb, c.want)
		}
		if lower := strings.ToLower(out + errb); strings.Contains(lower, "zebracorp") || strings.Contains(lower, "zed") {
			t.Errorf("%s: output repeats the leaked text:\n%s%s", c.name, out, errb)
		}
		if after := e.pubState(); after != before {
			t.Errorf("%s: public clone changed: %q -> %q", c.name, before, after)
		}
	}
}

func TestExportFailsClosedWithoutUsablePatterns(t *testing.T) {
	e := newEnv(t)
	before := e.pubState()
	if err := os.Remove(filepath.Join(e.priv, "docs", "internal", "leak-patterns.txt")); err != nil {
		t.Fatal(err)
	}
	if _, errb, code := e.export("v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "no leak patterns file") {
		t.Errorf("missing patterns: exit %d, stderr %q; want 1", code, errb)
	}
	empty := filepath.Join(e.home, "empty-patterns.txt")
	if err := os.WriteFile(empty, []byte("# nothing yet\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errb, code := e.export("-p", empty, "v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "holds no patterns") {
		t.Errorf("empty patterns: exit %d, stderr %q; want 1", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

func TestExportFailsClosedWhenTheGateFails(t *testing.T) {
	cases := []struct{ name, claude, want string }{
		{"a failing gate line", "- The gate:\n```sh\ntest -f README.md\nfalse\n```\n", "the gate failed"},
		{"no gate block", "# no gate here\n", "no gate block"},
	}
	for _, c := range cases {
		e := newEnv(t)
		e.write(map[string]string{"CLAUDE.md": c.claude})
		e.commitAndTag("v1.2.4")
		before := e.pubState()
		if _, errb, code := e.export("v1.2.4", e.pub); code != 1 || !strings.Contains(errb, c.want) {
			t.Errorf("%s: exit %d, stderr %q; want 1 and %q", c.name, code, errb, c.want)
		}
		if after := e.pubState(); after != before {
			t.Errorf("%s: public clone changed: %q -> %q", c.name, before, after)
		}
	}
}

func TestExportNeedsAnExistingVersionTag(t *testing.T) {
	e := newEnv(t)
	before := e.pubState()
	if _, errb, code := e.export("v9.9.9", e.pub); code != 1 || !strings.Contains(errb, "no tag v9.9.9") {
		t.Errorf("missing tag: exit %d, stderr %q; want 1", code, errb)
	}
	for _, bad := range []string{"main", "1.2.3", "v1.2", "v1.2.3;x"} {
		if _, _, code := e.export(bad, e.pub); code != 2 {
			t.Errorf("tag %q: exit %d, want 2 (usage)", bad, code)
		}
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

func TestExportRefusesAnUnsuitableClone(t *testing.T) {
	t.Run("untracked file", func(t *testing.T) {
		e := newEnv(t)
		junk := filepath.Join(e.pub, "junk.txt")
		if err := os.WriteFile(junk, []byte("mine\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := e.pubState()
		if _, errb, code := e.export("v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "untracked or ignored") {
			t.Errorf("exit %d, stderr %q; want 1", code, errb)
		}
		if _, err := os.Stat(junk); err != nil || e.pubState() != before {
			t.Error("a refused export touched the clone")
		}
	})
	t.Run("ignored file", func(t *testing.T) {
		e := newEnv(t)
		for name, body := range map[string]string{".git/info/exclude": "local.txt\n", "local.txt": "mine\n"} {
			if err := os.WriteFile(filepath.Join(e.pub, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, errb, code := e.export("v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "untracked or ignored") {
			t.Errorf("exit %d, stderr %q; want 1: emptying the work tree would delete an ignored file", code, errb)
		}
		if _, err := os.Stat(filepath.Join(e.pub, "local.txt")); err != nil {
			t.Error("a refused export deleted an ignored file")
		}
	})
	t.Run("tag already there", func(t *testing.T) {
		e := newEnv(t)
		e.git(e.pub, "tag", "v1.2.3")
		if _, errb, code := e.export("v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "already has tag") {
			t.Errorf("exit %d, stderr %q; want 1", code, errb)
		}
	})
	t.Run("the private repo itself", func(t *testing.T) {
		e := newEnv(t)
		if _, errb, code := e.export("v1.2.3", e.priv); code != 1 || !strings.Contains(errb, "is this repository") {
			t.Errorf("exit %d, stderr %q; want 1", code, errb)
		}
	})
	t.Run("detached HEAD", func(t *testing.T) {
		e := newEnv(t)
		e.git(e.pub, "checkout", "-q", "--detach")
		if _, errb, code := e.export("v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "not on a branch") {
			t.Errorf("exit %d, stderr %q; want 1", code, errb)
		}
	})
}

func TestExportDeletesWhatTheNewSnapshotDrops(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	e.git(e.priv, "rm", "-q", "internal/a.go")
	e.write(map[string]string{"internal/b.go": "package a\n"})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	files := strings.Join(lines(e.git(e.pub, "ls-files", "internal")), ",")
	if files != "internal/b.go" {
		t.Errorf("internal/ in the public clone = %q, want only internal/b.go", files)
	}
	if n := e.git(e.pub, "rev-list", "--count", "HEAD"); n != "3" {
		t.Errorf("%s commits, want root + one per export", n)
	}
}

func TestExportUsesTheTagsManifestNotTheWorkingCopys(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{"public-manifest.txt": "README.md\nCLAUDE.md\nrun.sh\ninternal/\nsecret.txt\npublic-manifest.txt\n"})
	e.mustExport("v1.2.3")
	if got := e.git(e.pub, "ls-files", "secret.txt"); got != "" {
		t.Error("secret.txt was exported: the manifest must come from the tag, not the working copy")
	}
}

func TestExportRefusesASnapshotThatChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	e.git(e.priv, "tag", "v1.2.4", "v1.2.3")
	before := e.pubState()
	if _, errb, code := e.export("v1.2.4", e.pub); code != 1 || !strings.Contains(errb, "already matches") {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

func TestExportNeverTouchesARemote(t *testing.T) {
	e := newEnv(t)
	remotes := e.git(e.pub, "config", "--get-regexp", `^remote\.`)
	e.mustExport("v1.2.3")
	if got := e.git(e.bare, "for-each-ref"); got != "" {
		t.Errorf("the public clone's origin gained refs:\n%s", got)
	}
	if got := e.git(e.pub, "config", "--get-regexp", `^remote\.`); got != remotes {
		t.Errorf("remote config changed: %q -> %q", remotes, got)
	}
	// And statically: no network or remote-changing command in any script
	// the export runs (comment lines aside).
	// A git subcommand stands alone as a word ("$clone" is a variable).
	gitNet := regexp.MustCompile(`\bgit\b.*(^|[\s;&|(])(push|fetch|pull|clone|ls-remote|remote)([\s;&|)]|$)`)
	tool := regexp.MustCompile(`(^|[\s;&|(])(curl|wget|gh|ssh|scp|nc)([\s;&|)]|$)`)
	for _, name := range []string{"export-public", "leak-scan", "manifest-select"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			if gitNet.MatchString(l) || tool.MatchString(l) {
				t.Errorf("scripts/%s:%d runs a network or remote command: %s", name, i+1, l)
			}
		}
	}
}

// --- Fix round 1 (review findings C1, I1-I6, M1) ---

// TestExportRefusesATagWithANewlineInAPath pins C1: a path holding a
// literal newline byte survives `ls-tree -z` as one record, but the
// newline->newline-delimited conversion manifest-select and leak-scan
// need would otherwise split it into a bogus extra "line" (here, a lone
// "docs/"), letting the whole docs/internal/ subtree -- the private
// patterns file included -- reach the clone unscanned via
// deny-by-default's own allow line for docs/.
func TestExportRefusesATagWithANewlineInAPath(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{"docs/\nx": "must never be exported\n"})
	e.commitAndTag("v1.2.4")
	before := e.pubState()
	_, errb, code := e.export("v1.2.4", e.pub)
	if code != 1 || !strings.Contains(errb, "newline") {
		t.Errorf("exit %d, stderr %q; want 1 naming the embedded newline", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q (docs/internal/ must never reach it)", before, after)
	}
}

// TestExportHandlesQuotedAndNonASCIIPaths (I6) kills the mutant that
// reverts the controller's -z override back to plain `ls-tree`: a path
// holding a `"` or a non-ASCII byte is exactly what that mode C-quotes,
// which manifest-select would then refuse (exit 2) instead of selecting.
func TestExportHandlesQuotedAndNonASCIIPaths(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{
		`internal/has"quote.go`: "package a\n",
		"internal/héllo.go":     "package a\n",
	})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	// -z: plain `ls-files` C-quotes both the `"` and the non-ASCII byte,
	// which would make this assertion pass even if export-public had
	// mangled them.
	raw := e.git(e.pub, "ls-files", "-z", "internal")
	var got []string
	for _, p := range strings.Split(raw, "\x00") {
		if p != "" {
			got = append(got, p)
		}
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{`internal/has"quote.go`, "internal/héllo.go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("public clone's internal/ = %q, want it to include %q", got, want)
		}
	}
}

// TestExportRejectsAnEmptyPatternsFlag pins M1: "-p ”" must be a usage
// error (exit 2), the same as leak-scan treats it, not a silent
// fall-back to the default patterns file.
func TestExportRejectsAnEmptyPatternsFlag(t *testing.T) {
	e := newEnv(t)
	before := e.pubState()
	if _, errb, code := e.export("-p", "", "v1.2.3", e.pub); code != 2 {
		t.Errorf("-p '': exit %d, stderr %q; want 2 (usage)", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportRefusesAStaleIndexLock pins half of I3: a leftover
// .git/index.lock (a killed or crashed git process) must be caught
// before anything is built, with the clone left exactly as it was.
func TestExportRefusesAStaleIndexLock(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.pub, ".git", "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := e.pubState()
	if _, errb, code := e.export("v1.2.3", e.pub); code != 1 || !strings.Contains(errb, "index.lock") {
		t.Errorf("exit %d, stderr %q; want 1 naming index.lock", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportIntoAnUnbornClone pins the success half of I3: a brand-new
// public repo with no commit yet (the runbook's first-ever export) is a
// valid, not a refused, target.
func TestExportIntoAnUnbornClone(t *testing.T) {
	e := newEnv(t)
	unborn := filepath.Join(filepath.Dir(e.pub), "unborn")
	e.git(filepath.Dir(e.pub), "init", "-q", "-b", "main", unborn)
	out, errb, code := e.export("v1.2.3", unborn)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if n := e.git(unborn, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("unborn clone has %s commit(s) after export, want exactly 1 (no parent)", n)
	}
	if e.git(unborn, "rev-parse", "v1.2.3^{commit}") != e.git(unborn, "rev-parse", "HEAD") {
		t.Error("tag v1.2.3 does not point at the export commit")
	}
	got := lines(e.git(unborn, "ls-files"))
	want := []string{"CLAUDE.md", "README.md", "docs/release-notes/v1.md", "internal/a.go", "public-manifest.txt", "run.sh"}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("unborn clone holds %q, want %q", got, want)
	}
}

// TestExportRollsBackWhenTagCreationFails pins the other half of I3: if
// creating the lightweight tag fails after the branch ref has already
// moved, the branch ref must be rolled back too, not left pointing at an
// untagged commit.
func TestExportRollsBackWhenTagCreationFails(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, "exit 9", "update-ref", "refs/tags/v1.2.3")
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "could not create tag") {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q (the branch ref move must have been rolled back)", before, after)
	}
}

// TestExportRollsBackOnASignalDuringStep5 pins I5: a TERM arriving after
// the branch ref and the tag already point at the new commit, but before
// the final `reset --hard`, must still leave the clone exactly as it
// was -- not a tagged, unreset clone with a stale work tree. No sleeps:
// the fake git signals the real export-public's own PID and only then
// lets the real `reset --hard` run, so the shell's pending trap fires
// deterministically at its next safe point.
func TestExportRollsBackOnASignalDuringStep5(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, `      kill -TERM "$PPID" 2>/dev/null || :`, "reset")
	before := e.pubState()
	beforeFiles := e.git(e.pub, "status", "--porcelain", "--untracked-files=all")
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 {
		t.Errorf("exit %d, stderr %q; want 1 (a signal must fail closed, never exit 0 or 12x)", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
	if afterFiles := e.git(e.pub, "status", "--porcelain", "--untracked-files=all"); afterFiles != beforeFiles {
		t.Errorf("public clone's work tree changed: %q -> %q", beforeFiles, afterFiles)
	}
}

// TestExportFailsClosedWhenGitLsTreeFails (I6) kills the mutant that
// removes the exit check after `git ls-tree`: a failing ls-tree must
// never be read as an empty, harmless path list.
func TestExportFailsClosedWhenGitLsTreeFails(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, "exit 9", "ls-tree")
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || errb == "" {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportNeverSignsDespiteTheOwnersGlobalConfig pins I4: the owner's
// global commit.gpgSign / tag.gpgSign / tag.forceSignAnnotated must never
// reach the anonymous public commit or turn the lightweight tag into a
// signed, owner-attributed annotated one.
func TestExportNeverSignsDespiteTheOwnersGlobalConfig(t *testing.T) {
	e := newEnv(t)
	fakeGPG := filepath.Join(e.home, "fake-gpg")
	if err := os.WriteFile(fakeGPG, []byte("#!/bin/sh\necho 'gpg must never run' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[commit]\n\tgpgSign = true\n[tag]\n\tgpgSign = true\n\tforceSignAnnotated = true\n[gpg]\n\tprogram = " + fakeGPG + "\n"
	if err := os.WriteFile(filepath.Join(e.home, "gitconfig"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mustExport("v1.2.3")
	raw := e.git(e.pub, "cat-file", "commit", "HEAD")
	if strings.Contains(raw, "gpgsig") {
		t.Errorf("commit carries a gpgsig despite the owner's global signing config:\n%s", raw)
	}
	if kind := e.git(e.pub, "cat-file", "-t", "refs/tags/v1.2.3"); kind != "commit" {
		t.Errorf("refs/tags/v1.2.3 is a %s object, want commit (a lightweight tag: never annotated, never signed)", kind)
	}
}

// --- Fix round 2 (re-review findings N1, N2 (structural), N3, N5, N6 (structural), I1/I2/M3/M4 test gaps) ---

// TestExportIsolatesAGateEditFromWhatGetsCommitted pins fix N3(a): the
// gate now runs in a throwaway copy ($tmp/gate), never the copy that is
// staged and committed ($tmp/out), so a gate line that edits a tracked
// file cannot change what reaches the clone -- the export still
// succeeds, and the public commit's README.md is exactly the scanned,
// unedited bytes, not the gate's mutated ones. (The controller's brief
// for this finding expected the fix to make such an edit fail the
// export; this implementation instead makes the edit structurally
// unable to reach the clone in the first place, which is the stronger
// guarantee -- see TestExportRefusesWhenTheStagedTreeDriftsFromTheTag
// for a test that does exercise the belt-and-braces invariant N3(b)
// added alongside it, with exit 1 and an unchanged clone.)
func TestExportIsolatesAGateEditFromWhatGetsCommitted(t *testing.T) {
	e := newEnv(t)
	claude := "# fixture\n\n- The gate, exactly:\n```sh\n" +
		"printf 'mutated\\n' >> README.md\n" +
		"test -f README.md\n" +
		"```\n"
	e.write(map[string]string{"CLAUDE.md": claude})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	got := e.git(e.pub, "show", "HEAD:README.md")
	if got != "c-hottag fixture" {
		t.Errorf("public README.md = %q, want the original fixture content unmutated by the gate", got)
	}
}

// TestExportRefusesWhenTheStagedTreeDriftsFromTheTag pins the
// belt-and-braces invariant R1(4): if the bytes about to be staged ever
// differ from the ones the tag actually recorded for that path -- from
// any cause -- the export must fail closed and leave the clone
// untouched. A `git` wrapper mutates one of export-public's own
// id-named raw-blob files (via the last argument to the first
// `hash-object` call, whose path is otherwise private to the running
// script) after the scan and immediately before it is hashed, simulating
// exactly that kind of drift. Since R-N2 that first call writes the blob
// into the gate's own git repo, so the drift is caught there, before the
// gate even runs. The injected text ("not-a-secret") is
// deliberately not a leak-scan hit, so only the invariant -- not the
// scan -- can be what catches this.
func TestExportRefusesWhenTheStagedTreeDriftsFromTheTag(t *testing.T) {
	e := newEnv(t)
	action := `      if [ -n "$lastarg" ] && [ -f "$lastarg" ]; then printf 'not-a-secret\n' >>"$lastarg"; fi`
	bin := gitWrapper(t, e.home, action, "hash-object")
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "does not byte-for-byte match") {
		t.Errorf("exit %d, stderr %q; want 1 naming the byte mismatch", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportRefusesIfTheCloneChangedDuringTheGate pins N5: the clone is
// checked clean once, up front, but the gate can run for minutes; a
// second look right before anything in the clone actually moves must
// catch a change that happened in between, and must not let the
// resulting rollback path (never entered here, since nothing was ever
// touched) delete what it found.
func TestExportRefusesIfTheCloneChangedDuringTheGate(t *testing.T) {
	e := newEnv(t)
	junk := filepath.Join(e.pub, "surprise.txt")
	action := "      printf 'surprise\\n' >" + shQuote(junk) + "\n"
	bin := gitWrapper(t, e.home, action, "commit-tree")
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "changed since it was checked") {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if _, err := os.Stat(junk); err != nil {
		t.Error("the recheck's own failure must not have deleted the surprise file it found")
	}
}

// TestExportRollsBackOnASignalDuringTheBranchRefUpdate and
// TestExportRollsBackOnASignalDuringTheTagRefUpdate pin N1: a signal
// arriving while one of the two update-ref calls is actually running is
// only handled once that call returns -- by which point it may already
// have moved the ref -- so the flag guarding a rollback must already be
// set beforehand, not after.
func TestExportRollsBackOnASignalDuringTheBranchRefUpdate(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, `      kill -TERM "$PPID" 2>/dev/null || :`, "update-ref", "refs/heads/main")
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q (the branch ref move must have been rolled back)", before, after)
	}
}

func TestExportRollsBackOnASignalDuringTheTagRefUpdate(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, `      kill -TERM "$PPID" 2>/dev/null || :`, "update-ref", "refs/tags/v1.2.3")
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q (both the branch ref and the tag must have been rolled back)", before, after)
	}
}

// TestExportIgnoresExportIgnoreAttribute pins R1: materialisation reads
// raw blobs via `git cat-file blob` (`git ls-tree` plus `git cat-file`,
// never `git archive` or a checkout), so a tag's own .gitattributes
// export-ignore entry -- an archive-only exclusion -- has no effect. A
// manifest-selected path marked export-ignore is still exported; it
// would previously (I1's original scenario, when materialisation went
// through `git archive`) have been silently dropped by `git archive`
// itself, before the fixed round-1/round-2 exit-status checks even had
// anything to check.
func TestExportIgnoresExportIgnoreAttribute(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{".gitattributes": "internal/a.go export-ignore\n"})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	if got := e.git(e.pub, "ls-files", "internal/a.go"); got == "" {
		t.Error("internal/a.go (export-ignore) was not committed, despite raw-blob materialisation bypassing git archive")
	}
}

// TestExportCommitsAFileTheExportedTreesGitignoreWouldSkip pins I2 (a
// mutant the re-review found surviving): a file force-tracked despite
// matching the exported tree's own .gitignore must still be staged with
// `add -A -f`, not silently dropped the way plain `add -A` would.
func TestExportCommitsAFileTheExportedTreesGitignoreWouldSkip(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{".gitignore": "internal/skip.go\n"})
	e.commitAndTag("v1.2.4")
	e.write(map[string]string{"internal/skip.go": "package a\n"})
	e.git(e.priv, "add", "-f", "internal/skip.go")
	e.git(e.priv, "commit", "-q", "-m", "fixture v1.2.5")
	e.git(e.priv, "tag", "v1.2.5")
	e.mustExport("v1.2.5")
	if got := e.git(e.pub, "ls-files", "internal/skip.go"); got == "" {
		t.Error("a file force-tracked despite matching the exported tree's own .gitignore was not committed")
	}
}

// TestExportRunsNoHookOnTheClone pins M3 (a mutant the re-review found
// surviving): every clone-targeted git call goes through the same
// core.hooksPath=/dev/null helper, not just the old `git commit`.
func TestExportRunsNoHookOnTheClone(t *testing.T) {
	e := newEnv(t)
	hooks := filepath.Join(e.pub, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(e.pub, "hook-ran")
	for _, name := range []string{"reference-transaction", "post-index-change", "post-checkout", "post-commit"} {
		body := "#!/bin/sh\necho " + name + " >>" + shQuote(marker) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.mustExport("v1.2.3")
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("a clone hook ran during the export:\n%s", b)
	}
}

// TestExportCommitsInUTC pins M4 (a mutant the re-review found
// surviving): the commit's author and committer dates carry a +0000
// offset, regardless of the machine's local time zone.
func TestExportCommitsInUTC(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	who := e.git(e.pub, "log", "-1", "--date=format:%z", "--format=%ad|%cd")
	if who != "+0000|+0000" {
		t.Errorf("author|committer date offsets = %q, want +0000|+0000", who)
	}
}

// TestExportScansTheTagSuffixAndMessage pins M4 (a mutant the re-review
// found surviving): a pre-release suffix carrying a private term must
// fail the commit-message leak scan, naming the message file and line,
// never the term itself.
func TestExportScansTheTagSuffixAndMessage(t *testing.T) {
	e := newEnv(t)
	tag := "v1.2.3-zebracorp"
	e.git(e.priv, "tag", tag)
	before := e.pubState()
	_, errb, code := e.export(tag, e.pub)
	if code != 1 || !strings.Contains(errb, "msg:1:") {
		t.Errorf("exit %d, stderr %q; want 1 naming msg:1:", code, errb)
	}
	if strings.Contains(strings.ToLower(errb), "zebracorp") {
		t.Errorf("output repeats the leaked term:\n%s", errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportRejectsAMultiLineTag pins N6: `printf '%s\n' "$tag" | grep
// -Eq '^…$'` matches if any LINE of a multi-line tag matches, not just
// the whole string; the newline must be rejected outright.
func TestExportRejectsAMultiLineTag(t *testing.T) {
	e := newEnv(t)
	before := e.pubState()
	if _, errb, code := e.export("v1.2.3\nrm -rf /", e.pub); code != 2 || !strings.Contains(errb, "newline") {
		t.Errorf("exit %d, stderr %q; want 2 naming the newline", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// --- Fix round 3 (re-review 2 findings R1-R6) ---

// utf16le encodes an ASCII/BMP string as raw (no-BOM) UTF-16LE bytes, so
// a fixture can be written to disk exactly the way a working-tree-encoding
// clean filter expects to read it.
func utf16le(s string) string {
	b := make([]byte, 0, len(s)*2)
	for _, r := range s {
		b = append(b, byte(r), byte(r>>8))
	}
	return string(b)
}

// TestExportBypassesWorkingTreeEncodingAttribute pins R1: the reviewer's
// own reproduction. A nested .gitattributes marks internal/n.txt
// working-tree-encoding=UTF-16LE; the fixture's on-disk bytes are
// genuinely UTF-16LE, so `git add`'s own clean filter -- run once, by
// this setup, to build the tag -- stores a clean UTF-8 blob holding the
// private term. Materialising from that raw blob (never `git archive`
// or a checkout, which would apply the attribute and re-encode it,
// hiding the term from a byte-oriented scanner) means export-public's
// own scan sees the term exactly as committed.
func TestExportBypassesWorkingTreeEncodingAttribute(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{
		"internal/.gitattributes": "*.txt working-tree-encoding=UTF-16LE\n",
		"internal/n.txt":          utf16le("hello ZebraCorp\n"),
	})
	e.commitAndTag("v1.2.4")
	before := e.pubState()
	_, errb, code := e.export("v1.2.4", e.pub)
	if code != 1 || !strings.Contains(errb, "private pattern 1") {
		t.Errorf("exit %d, stderr %q; want 1 naming private pattern 1", code, errb)
	}
	if strings.Contains(strings.ToLower(errb), "zebracorp") {
		t.Errorf("output repeats the leaked term:\n%s", errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportBypassesExportSubst pins R1: an export-subst file's
// committed blob must equal the tag's own blob exactly, with the
// literal, unsubstituted "$Format:...$" placeholder -- `git archive` is
// the only thing that would ever expand it, and materialisation never
// calls it.
func TestExportBypassesExportSubst(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{
		".gitattributes":       "internal/version.txt export-subst\n",
		"internal/version.txt": "commit: $Format:%H$\n",
	})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	tagOid := e.git(e.priv, "rev-parse", "v1.2.4:internal/version.txt")
	pubOid := e.git(e.pub, "rev-parse", "HEAD:internal/version.txt")
	if pubOid != tagOid {
		t.Errorf("public internal/version.txt blob = %s, want the tag's own blob %s (export-subst must never substitute)", pubOid, tagOid)
	}
	if got := e.git(e.pub, "show", "HEAD:internal/version.txt"); got != "commit: $Format:%H$" {
		t.Errorf("public internal/version.txt = %q, want the literal, unsubstituted placeholder", got)
	}
}

// TestExportRefusesARawUTF16File pins R1(6): a selected file whose
// stored blob genuinely starts with a UTF-16 BOM -- no .gitattributes
// involved -- fails closed, naming the path, because the text-oriented
// scanner cannot read it.
func TestExportRefusesARawUTF16File(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{"internal/u16.txt": "\xff\xfeh\x00i\x00"})
	e.commitAndTag("v1.2.4")
	before := e.pubState()
	_, errb, code := e.export("v1.2.4", e.pub)
	if code != 1 || !strings.Contains(errb, "internal/u16.txt") {
		t.Errorf("exit %d, stderr %q; want 1 naming internal/u16.txt", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportExportsRealImages pins R1(6)'s allowlist after F231 G2: a
// real, valid PNG, GIF and JPEG (each holding NUL bytes), made by the Go
// standard library's own encoders, pass the full format walk and export
// byte-for-byte.
func TestExportExportsRealImages(t *testing.T) {
	e := newEnv(t)
	imgs := realImages(t)
	files := map[string]string{}
	for ext, b := range imgs {
		if !bytes.Contains(b, []byte{0}) {
			t.Fatalf("the %s fixture holds no NUL byte, so it would never reach the allowlist", ext)
		}
		files["internal/pic."+ext] = string(b)
	}
	files["internal/pic.JPEG"] = string(imgs["jpg"])
	e.write(files)
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	for rel := range files {
		tagOid := e.git(e.priv, "rev-parse", "v1.2.4:"+rel)
		if pubOid := e.git(e.pub, "rev-parse", "HEAD:"+rel); pubOid != tagOid {
			t.Errorf("public %s blob = %s, want the tag's own blob %s", rel, pubOid, tagOid)
		}
	}
}

// realImages is one tiny, valid image per verified format ("png", "gif",
// "jpg"), from the standard library's encoders.
func realImages(t *testing.T) map[string][]byte {
	t.Helper()
	pal := image.NewPaletted(image.Rect(0, 0, 3, 2), color.Palette{color.Black, color.White})
	pal.SetColorIndex(1, 1, 1)
	var p, g, j bytes.Buffer
	if err := png.Encode(&p, pal); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&g, pal, nil); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, pal, nil); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{"png": p.Bytes(), "gif": g.Bytes(), "jpg": j.Bytes()}
}

// pngChunk is one PNG chunk: length, type, data and CRC.
func pngChunk(typ string, data []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.WriteString(typ)
	b.Write(data)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	return b.Bytes()
}

// withPNGChunk inserts chunk just before a valid PNG's final IEND chunk
// (always its last 12 bytes).
func withPNGChunk(pngBytes, chunk []byte) []byte {
	cut := len(pngBytes) - 12
	out := append([]byte{}, pngBytes[:cut]...)
	out = append(out, chunk...)
	return append(out, pngBytes[cut:]...)
}

// TestExportRefusesABinaryThatIsNotItsFormat pins F231 G2: an allowlisted
// extension and the right leading bytes no longer vouch for a file. Every
// file on the binary allowlist must pass its format's full walk (PNG:
// every chunk's length, type and CRC, IHDR first, IEND last, nothing after
// it; GIF: every block up to the trailer, nothing after it; JPEG: every
// segment and the entropy-coded data up to EOI, nothing after it), or it
// is refused, naming the path. A format dropped from the allowlist (PDF,
// ICO, WOFF) is refused too.
//
// Review round 1, M2: every body here but the first is benign, UTF-16
// text neither scan flags, so the walk alone must refuse it; ignoring its
// verdict fails each of those cases. The first case is F231 G2's own
// reproduction, a PNG header and a UTF-16 body carrying a home path and
// the private term.
func TestExportRefusesABinaryThatIsNotItsFormat(t *testing.T) {
	leak := []byte(utf16le("hello " + "/" + "Users/zed/x ZebraCorp\n"))
	benign := []byte(utf16le("hello world, nothing to see\n"))
	imgs := realImages(t)
	badCRC := append([]byte{}, imgs["png"]...)
	badCRC[8+8+13] ^= 0xff // the IHDR chunk's CRC
	cases := []struct {
		name, file string
		body       []byte
	}{
		{"PNG header, UTF-16 body carrying a leak", "internal/x.png", append([]byte("\x89PNG\r\n\x1a\n"), leak...)},
		{"PNG header, UTF-16 body", "internal/x.png", append([]byte("\x89PNG\r\n\x1a\n"), benign...)},
		{"GIF header, UTF-16 body", "internal/x.gif", append([]byte("GIF89a"), benign...)},
		{"JPEG header, UTF-16 body", "internal/x.jpg", append([]byte("\xff\xd8\xff\xe0"), benign...)},
		{"PNG with a wrong CRC", "internal/x.png", badCRC},
		{"PNG without IEND", "internal/x.png", imgs["png"][:len(imgs["png"])-12]},
		{"PNG with bytes after IEND", "internal/x.png", append(append([]byte{}, imgs["png"]...), benign...)},
		{"PNG whose first chunk is not IHDR", "internal/x.png", append([]byte("\x89PNG\r\n\x1a\n"), append(pngChunk("IDAT", benign), pngChunk("IEND", nil)...)...)},
		{"GIF with bytes after the trailer", "internal/x.gif", append(append([]byte{}, imgs["gif"]...), benign...)},
		{"GIF truncated before the trailer", "internal/x.gif", imgs["gif"][:len(imgs["gif"])-1]},
		{"JPEG with bytes after EOI", "internal/x.jpg", append(append([]byte{}, imgs["jpg"]...), benign...)},
		{"JPEG truncated before EOI", "internal/x.jpg", imgs["jpg"][:len(imgs["jpg"])-2]},
		{"a PDF (no longer allowlisted)", "internal/x.pdf", append([]byte("%PDF-1.4\n"), benign...)},
		{"an ICO (no longer allowlisted)", "internal/x.ico", append([]byte("\x00\x00\x01\x00"), benign...)},
		{"a WOFF (no longer allowlisted)", "internal/x.woff", append([]byte("wOFF"), benign...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.write(map[string]string{c.file: string(c.body)})
			e.commitAndTag("v1.2.4")
			e.refused("v1.2.4", c.file)
			if _, errb, _ := e.export("--check", "v1.2.4"); !strings.Contains(errb, c.file) || strings.Contains(errb, "zed") {
				t.Errorf("--check: stderr %q; want it to name %s and never the text", errb, c.file)
			}
		})
	}
}

// TestExportRefusesAnOversizedImage pins review round 1, M5: the format
// walk costs about an awk cell per byte, so an allowlisted binary over
// 5 MiB is refused up front, naming its path and size.
func TestExportRefusesAnOversizedImage(t *testing.T) {
	e := newEnv(t)
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 5<<20)...)
	e.write(map[string]string{"internal/big.png": string(body)})
	e.commitAndTag("v1.2.4")
	e.refused("v1.2.4", fmt.Sprintf("internal/big.png is %d bytes", len(body)))
}

// TestExportScansUTF16TextInsideAValidImage pins G2's second half: a
// structurally valid image can still carry UTF-16 text in a part that is
// not compressed (a PNG text or private chunk, a JPEG comment segment),
// where the raw-byte scan cannot read it. Every allowlisted binary is
// also scanned with its NUL bytes dropped, which turns ASCII-range
// UTF-16/32 text back into plain text, so the hit is found and reported
// by path and line, never by its text.
func TestExportScansUTF16TextInsideAValidImage(t *testing.T) {
	imgs := realImages(t)
	home := []byte(utf16le("/" + "Users/zed/x\n"))
	term := []byte(utf16le("hello ZebraCorp\n"))
	com := func(data []byte) []byte {
		seg := []byte{0xff, 0xfe, byte((len(data) + 2) >> 8), byte(len(data) + 2)}
		out := append([]byte{}, imgs["jpg"][:2]...)
		out = append(out, seg...)
		out = append(out, data...)
		return append(out, imgs["jpg"][2:]...)
	}
	cases := []struct {
		name, file string
		body       []byte
		want       string
	}{
		{"PNG tEXt chunk, a home path", "internal/pic.png", withPNGChunk(imgs["png"], pngChunk("tEXt", append([]byte("Comment\x00"), home...))), "home-path"},
		{"PNG private chunk, a private term", "internal/pic.png", withPNGChunk(imgs["png"], pngChunk("prVt", term)), "private pattern 1"},
		{"JPEG comment segment, a private term", "internal/pic.jpg", com(term), "private pattern 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.write(map[string]string{c.file: string(c.body)})
			e.commitAndTag("v1.2.4")
			e.refused("v1.2.4", c.file+":")
			_, errb, code := e.export("v1.2.4", e.pub)
			if code != 1 || !strings.Contains(errb, c.want) {
				t.Errorf("exit %d, stderr %q; want 1 naming %q", code, errb, c.want)
			}
			if strings.Contains(errb, "zed") {
				t.Errorf("output repeats the leaked text:\n%s", errb)
			}
		})
	}
}

// TestExportToleratesCRLFInCLAUDEMD pins R2: a CR left on the fence
// lines themselves (not just a command line) must not stop the gate
// block from being recognised.
func TestExportToleratesCRLFInCLAUDEMD(t *testing.T) {
	e := newEnv(t)
	claude := "# fixture\r\n\r\n- The gate, exactly:\r\n```sh\r\ntest -f README.md\r\ntest -x run.sh\r\n```\r\n"
	e.write(map[string]string{"CLAUDE.md": claude})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
}

// TestExportIgnoresASignalAfterSuccess pins R4: once the commit and tag
// already exist, a later signal (here, during the final success
// message's own `rev-parse --short`) must not report exit 1 -- that
// would contradict "exit 1 means nothing was committed".
func TestExportIgnoresASignalAfterSuccess(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, `      kill -TERM "$PPID" 2>/dev/null || :`, "rev-parse", "--short")
	out, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 0 {
		t.Errorf("exit %d, stderr %q; want 0 (the export had already succeeded)", code, errb)
	}
	if !strings.Contains(out, "tagged v1.2.3") {
		t.Errorf("stdout %q; want the success message", out)
	}
	if e.git(e.pub, "rev-parse", "v1.2.3^{commit}") != e.git(e.pub, "rev-parse", "HEAD") {
		t.Error("tag v1.2.3 does not point at the export commit")
	}
}

// TestExportDoesNotTouchTheWorkTreeBeforeItStarted pins R5's "touched
// always true" mutant: a failure before the final `reset --hard` (here,
// the tag update-ref failing) must never run `reset --hard`/`clean -fdx`
// -- which would otherwise delete a file the export itself never
// touched, dropped into the clone at the same point.
func TestExportDoesNotTouchTheWorkTreeBeforeItStarted(t *testing.T) {
	e := newEnv(t)
	junk := filepath.Join(e.pub, "unrelated.txt")
	action := "      printf 'unrelated\\n' >" + shQuote(junk) + "\n      exit 9"
	bin := gitWrapper(t, e.home, action, "update-ref", "refs/tags/v1.2.3")
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if _, err := os.Stat(junk); err != nil {
		t.Error("a failure before the final reset --hard must not clean/reset a file it never touched")
	}
}

// TestExportHandlesGlobSpecialCharactersInPaths pins R5's
// "GIT_LITERAL_PATHSPECS removed" mutant: a selected path holding a
// literal glob-special character must still be matched literally by the
// xargs/ls-tree query that drives materialisation and the invariant.
func TestExportHandlesGlobSpecialCharactersInPaths(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{
		"internal/a*b.go":  "package a\n",
		"internal/[ab].go": "package a\n",
	})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	got := e.git(e.pub, "ls-files", "internal")
	for _, want := range []string{"internal/a*b.go", "internal/[ab].go"} {
		if !strings.Contains(got, want) {
			t.Errorf("public clone's internal/ = %q, want it to include %q", got, want)
		}
	}
}

// --- Fix round 4 (re-review 3 findings X1-X6, controller redesign) ---

// gitIn is git, with stdin fed from in.
func (e *env) gitIn(dir, in string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = e.vars()
	cmd.Stdin = strings.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		e.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

// entry is one tree entry for plumbTag. body is the blob's bytes (a
// symlink's link text for 120000), or a commit id for 160000.
type entry struct{ mode, path, body string }

// plumbTag adds entries to the private repo's index with plumbing only
// (hash-object --stdin, update-index -z --index-info), then commits and
// tags. Nothing touches the work tree, so it builds trees this host's
// filesystem could never hold: case-colliding paths on a case-insensitive
// disk, a symlink next to its own case alias, a submodule.
func (e *env) plumbTag(tag string, entries ...entry) {
	e.t.Helper()
	var info strings.Builder
	for _, en := range entries {
		oid := en.body
		if en.mode != "160000" {
			oid = e.gitIn(e.priv, en.body, "hash-object", "-w", "--stdin")
		}
		fmt.Fprintf(&info, "%s %s\t%s\x00", en.mode, oid, en.path)
	}
	e.gitIn(e.priv, info.String(), "update-index", "-z", "--index-info")
	e.git(e.priv, "commit", "-q", "-m", "fixture "+tag)
	e.git(e.priv, "tag", tag)
}

// objects is the public clone's whole object store: count-objects -v
// plus every file (loose object, pack, index) under .git/objects.
func (e *env) objects() string {
	e.t.Helper()
	root := filepath.Join(e.pub, ".git", "objects")
	var names []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			names = append(names, rel)
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	sort.Strings(names)
	return e.git(e.pub, "count-objects", "-v") + "\n" + strings.Join(names, "\n")
}

// refused runs the export and asserts the X1/X2 contract for a refusal:
// exit 1, stderr holding want, no private term in any output, the clone's
// refs, tags, index and work tree unchanged, and not one new object in
// its object store.
func (e *env) refused(tag, want string) {
	e.t.Helper()
	before, beforeObjs := e.pubState(), e.objects()
	out, errb, code := e.export(tag, e.pub)
	if code != 1 || !strings.Contains(errb, want) {
		e.t.Errorf("exit %d, stderr %q; want 1 naming %q", code, errb, want)
	}
	if strings.Contains(strings.ToLower(out+errb), "zebracorp") {
		e.t.Errorf("output repeats the private term:\n%s%s", out, errb)
	}
	if after := e.pubState(); after != before {
		e.t.Errorf("public clone changed: %q -> %q", before, after)
	}
	if afterObjs := e.objects(); afterObjs != beforeObjs {
		e.t.Errorf("a refused export wrote objects into the public clone:\nbefore:\n%s\nafter:\n%s", beforeObjs, afterObjs)
	}
}

// TestExportRefusesACaseCollision pins X1(a): internal/Notes.txt (the
// private term) and internal/notes.txt (clean) are one file on a
// case-insensitive disk, so materialising both at their paths let the
// clean one overwrite the other before the scan, and the term shipped
// with exit 0. Any case-fold collision among selected paths is refused
// up front, whatever the filesystem.
func TestExportRefusesACaseCollision(t *testing.T) {
	e := newEnv(t)
	e.plumbTag("v1.2.4",
		entry{"100644", "internal/Notes.txt", "hello ZebraCorp\n"},
		entry{"100644", "internal/notes.txt", "clean\n"})
	e.refused("v1.2.4", "differ only in letter case")
}

// TestExportRefusesADirectoryPrefixCaseCollision pins X1 for directory
// prefixes: two directories, or a file and a directory, that differ only
// in case are the same place on a case-insensitive disk.
func TestExportRefusesADirectoryPrefixCaseCollision(t *testing.T) {
	cases := []struct {
		name    string
		entries []entry
	}{
		{"two directories", []entry{{"100644", "docs/Release-notes/v2.md", "notes\n"}}},
		{"a file and a directory", []entry{
			{"100644", "internal/x", "file\n"},
			{"100644", "internal/X/y.go", "package y\n"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.plumbTag("v1.2.4", c.entries...)
			e.refused("v1.2.4", "differ only in letter case")
		})
	}
}

// TestExportNeverWritesOutsideItsTempDir pins X1(b): a symlink whose case
// alias is also a selected path turned a later write (a file into the
// symlink's target, or a directory under it) into a write anywhere on
// disk, before any check ran. Nothing outside the export's own temp dir
// may change, and the export is refused.
func TestExportNeverWritesOutsideItsTempDir(t *testing.T) {
	t.Run("file alias", func(t *testing.T) {
		e := newEnv(t)
		victim := filepath.Join(t.TempDir(), "victim.txt")
		if err := os.WriteFile(victim, []byte("untouched\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.plumbTag("v1.2.4",
			entry{"120000", "internal/A.txt", victim},
			entry{"100644", "internal/a.txt", "overwritten\n"})
		e.refused("v1.2.4", "")
		if b, err := os.ReadFile(victim); err != nil || string(b) != "untouched\n" {
			t.Errorf("the file outside the temp dir changed: %q, %v", b, err)
		}
	})
	t.Run("directory alias", func(t *testing.T) {
		e := newEnv(t)
		victim := t.TempDir()
		e.plumbTag("v1.2.4",
			entry{"120000", "internal/L", victim},
			entry{"100644", "internal/l/pwned.txt", "pwned\n"})
		e.refused("v1.2.4", "")
		if got, err := os.ReadDir(victim); err != nil || len(got) != 0 {
			t.Errorf("the directory outside the temp dir gained %v (%v)", got, err)
		}
	})
}

// TestExportRefusesASubmodule pins the controller ruling: a selected
// gitlink (mode 160000) has no content to scan, so it is refused.
func TestExportRefusesASubmodule(t *testing.T) {
	e := newEnv(t)
	head := e.git(e.priv, "rev-parse", "HEAD")
	e.plumbTag("v1.2.4", entry{"160000", "internal/sub", head})
	e.refused("v1.2.4", "submodule")
}

// TestExportRefusesAnEscapingSymlink pins the controller ruling: a
// selected symlink whose target is absolute, or leaves the tree root via
// "..", is refused -- including a ".." after a name, which could step
// back out through another in-tree symlink.
func TestExportRefusesAnEscapingSymlink(t *testing.T) {
	cases := []struct{ name, target string }{
		{"absolute", "/" + "etc/passwd"},
		{"parent of the root", "../../outside"},
		{"dot-dot after a name", "a.go/../../README.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.plumbTag("v1.2.4", entry{"120000", "internal/esc", c.target})
			e.refused("v1.2.4", "symlink")
		})
	}
}

// TestExportKeepsAnInTreeSymlink: a symlink that stays inside the tree is
// exported as a symlink (mode 120000, the tag's own blob), including one
// that climbs no higher than the root.
func TestExportKeepsAnInTreeSymlink(t *testing.T) {
	e := newEnv(t)
	e.plumbTag("v1.2.4",
		entry{"120000", "internal/link", "a.go"},
		entry{"120000", "internal/up", "../README.md"})
	e.mustExport("v1.2.4")
	for _, p := range []string{"internal/link", "internal/up"} {
		if s := e.git(e.pub, "ls-tree", "HEAD", p); !strings.HasPrefix(s, "120000 blob "+e.git(e.priv, "rev-parse", "v1.2.4:"+p)) {
			t.Errorf("%s in the public commit is %q, want the tag's own 120000 blob", p, s)
		}
		if fi, err := os.Lstat(filepath.Join(e.pub, filepath.FromSlash(p))); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s in the public clone's work tree is not a symlink (%v)", p, err)
		}
	}
}

// TestExportLeavesNoObjectsInTheCloneOnRefusal pins X2: blobs were
// written into the clone's object store (hash-object -w) before the leak
// scan, so a refused export left private content at rest there. Nothing
// is written into the clone until the scans and the gate have passed.
func TestExportLeavesNoObjectsInTheCloneOnRefusal(t *testing.T) {
	cases := []struct {
		name, file, body, want string
	}{
		{"a content leak", "internal/leak.txt", "hello ZebraCorp\n", "private pattern 1"},
		{"a failing gate", "CLAUDE.md", "- The gate:\n```sh\nfalse\n```\n", "the gate failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.write(map[string]string{c.file: c.body})
			e.commitAndTag("v1.2.4")
			e.refused("v1.2.4", c.want)
		})
	}
}

// TestExportRefusesAPrivateTermInAPathName pins the redesign's path-name
// scan: content is scanned under oid names, so the selected path list is
// scanned on its own, as content, and a hit there never prints the path.
func TestExportRefusesAPrivateTermInAPathName(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{"internal/zebracorp-notes.go": "package a\n"})
	e.commitAndTag("v1.2.4")
	e.refused("v1.2.4", "private pattern 1")
}

// TestExportRunsNoFilterDriverOnTheClone pins X5 and ruling 8: the final
// reset --hard must run no filter driver from the clone's config and must
// not re-encode, whatever attributes the exported tree carries. The work
// tree ends up holding exactly the committed bytes.
func TestExportRunsNoFilterDriverOnTheClone(t *testing.T) {
	e := newEnv(t)
	marker := filepath.Join(e.home, "filter-ran")
	drv := filepath.Join(e.home, "drv.sh")
	if err := os.WriteFile(drv, []byte("#!/bin/sh\necho ran >>"+shQuote(marker)+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"filter.x.smudge", drv}, {"filter.x.clean", drv}, {"filter.x.required", "true"}} {
		e.git(e.pub, "config", kv[0], kv[1])
	}
	e.write(map[string]string{
		"internal/.gitattributes": "*.go filter=x\n*.txt working-tree-encoding=UTF-16LE\n",
		"internal/n.txt":          utf16le("hello\n"),
	})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("a filter driver from the clone's config ran during the export:\n%s", b)
	}
	for p, want := range map[string]string{"internal/a.go": "package a\n", "internal/n.txt": "hello\n"} {
		if b, err := os.ReadFile(filepath.Join(e.pub, filepath.FromSlash(p))); err != nil || string(b) != want {
			t.Errorf("work tree %s = %q (%v), want the raw committed bytes %q", p, b, err, want)
		}
		if got, tag := e.git(e.pub, "rev-parse", "HEAD:"+p), e.git(e.priv, "rev-parse", "v1.2.4:"+p); got != tag {
			t.Errorf("committed %s = %s, want the tag's blob %s", p, got, tag)
		}
	}
}

// TestExportRefusesACloneWithInfoAttributes: $GIT_DIR/info/attributes
// could name a filter or an encoding for the final work-tree sync, and
// no option turns it off, so a clone that has one is refused up front.
func TestExportRefusesACloneWithInfoAttributes(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.pub, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.pub, ".git", "info", "attributes"), []byte("* filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.refused("v1.2.3", "info/attributes")
}

// TestExportHandlesASymlinkWhoseTargetLooksLikeAnOption pins X3: ln got
// no "--", so link text "-v" was parsed as an option and a stray symlink
// appeared in the private repo's own work tree.
func TestExportHandlesASymlinkWhoseTargetLooksLikeAnOption(t *testing.T) {
	e := newEnv(t)
	e.plumbTag("v1.2.4", entry{"120000", "internal/lnk", "-v"})
	e.mustExport("v1.2.4")
	if _, err := os.Lstat(filepath.Join(e.priv, "lnk")); err == nil {
		t.Error("a stray lnk appeared in the private repo's work tree")
	}
	if s := e.git(e.pub, "ls-tree", "HEAD", "internal/lnk"); !strings.HasPrefix(s, "120000 blob "+e.git(e.priv, "rev-parse", "v1.2.4:internal/lnk")) {
		t.Errorf("internal/lnk in the public commit is %q, want the tag's own 120000 blob", s)
	}
}

// TestExportRefusesContentTheScannerCannotRead pins X4: an allowlisted
// extension no longer vouches for a file by its name alone (its leading
// bytes must match), and a compressed file is refused whatever its name,
// because the scanner cannot see inside it.
func TestExportRefusesContentTheScannerCannotRead(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte("hello ZebraCorp\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, file, body, want string }{
		{"UTF-16 named .PNG", "internal/x.PNG", "\xff\xfe" + utf16le("hello ZebraCorp\n"), "internal/x.PNG"},
		{"gzip named .gz", "internal/t.gz", gz.String(), "compressed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.write(map[string]string{c.file: c.body})
			e.commitAndTag("v1.2.4")
			e.refused("v1.2.4", c.want)
		})
	}
}

// TestExportRefusesASelectionItCannotMatchToTheTree pins X6: every path
// manifest-select prints must be one of the tag's entries; a selected path
// with no entry would otherwise be dropped silently, with exit 0. A
// manifest-select that invents a path stands in for any such mismatch.
func TestExportRefusesASelectionItCannotMatchToTheTree(t *testing.T) {
	e := newEnv(t)
	bin := t.TempDir()
	for _, name := range []string{"export-public", "leak-scan"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := "#!/bin/sh\n" + shQuote(filepath.Join(repoRoot(t), "scripts", "manifest-select")) + " \"$@\" || exit\nprintf 'internal/ghost.go\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "manifest-select"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	before := e.pubState()
	_, errb, code := e.exportScript(filepath.Join(bin, "export-public"), "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "does not match") {
		t.Errorf("exit %d, stderr %q; want 1 naming the mismatch", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// TestExportRefusesAUnicodeNormalisationCollision covers what the ASCII
// case-fold check cannot: "é" precomposed and decomposed are one name on
// a normalisation-insensitive disk (APFS). Where this host's temp disk
// merges them, the gate's materialisation must refuse rather than write
// one over the other; where it keeps them apart, both export.
func TestExportRefusesAUnicodeNormalisationCollision(t *testing.T) {
	nfc, nfd := "internal/é.go", "internal/é.go"
	probe := t.TempDir()
	if err := os.WriteFile(filepath.Join(probe, "é"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, statErr := os.Stat(filepath.Join(probe, "é"))
	merges := statErr == nil
	e := newEnv(t)
	e.plumbTag("v1.2.4", entry{"100644", nfc, "package a\n"}, entry{"100644", nfd, "package b\n"})
	if merges {
		e.refused("v1.2.4", "same file")
		return
	}
	e.mustExport("v1.2.4")
}

// TestExportRollbackRunsNoFilterDriver pins ruling 8 for the rollback's
// own reset --hard: a signal during the final reset rolls the work tree
// back to the previous export, and that reset rewrites a filtered file
// too, so it must also run with no attributes.
func TestExportRollbackRunsNoFilterDriver(t *testing.T) {
	e := newEnv(t)
	marker := filepath.Join(e.home, "filter-ran")
	drv := filepath.Join(e.home, "drv.sh")
	if err := os.WriteFile(drv, []byte("#!/bin/sh\necho ran >>"+shQuote(marker)+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"filter.x.smudge", drv}, {"filter.x.clean", drv}, {"filter.x.required", "true"}} {
		e.git(e.pub, "config", kv[0], kv[1])
	}
	e.write(map[string]string{"internal/.gitattributes": "*.go filter=x\n"})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	e.write(map[string]string{"internal/a.go": "package a // v1.2.5\n"})
	e.commitAndTag("v1.2.5")
	bin := gitWrapper(t, e.home, `      kill -TERM "$PPID" 2>/dev/null || :`, "reset")
	_, errb, code := e.exportWithPath(bin, "v1.2.5", e.pub)
	if code != 1 {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("a filter driver from the clone's config ran during the export or its rollback:\n%s", b)
	}
	if b, err := os.ReadFile(filepath.Join(e.pub, "internal", "a.go")); err != nil || string(b) != "package a\n" {
		t.Errorf("work tree internal/a.go = %q (%v), want v1.2.4's bytes back", b, err)
	}
}

// --- M2: scripts/export-public --check REV ---

// TestExportCheckPassesOnACleanRevision pins M2: --check runs the same
// selection, refusal, path-name and blob scans (steps 1-5) as a real
// export, with no clone and no gate, and exits 0 on a revision that would
// export cleanly.
func TestExportCheckPassesOnACleanRevision(t *testing.T) {
	e := newEnv(t)
	out, errb, code := e.export("--check", "HEAD")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, errb)
	}
	if !strings.Contains(out, "check HEAD") {
		t.Errorf("stdout %q; want it to report the checked revision", out)
	}
}

// TestExportCheckAcceptsAnyRevisionNotOnlyAVersionTag pins M2: --check is
// meant to run on HEAD in CI (and on the private gate before a commit),
// which is never a vX.Y.Z tag; a real export's TAG format restriction
// does not apply to it.
func TestExportCheckAcceptsAnyRevisionNotOnlyAVersionTag(t *testing.T) {
	e := newEnv(t)
	if _, errb, code := e.export("--check", "main"); code != 0 {
		t.Errorf("--check main: exit %d, stderr %q, want 0", code, errb)
	}
}

// TestExportCheckFailsOnALeakWithoutACloneArgumentAtAll pins M2: the same
// leak scan a real export runs, with nothing ever written anywhere (there
// is no clone to touch in the first place).
func TestExportCheckFailsOnALeakWithoutACloneArgumentAtAll(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{"README.md": "hello ZebraCorp\n"})
	e.commitAndTag("v1.2.4")
	out, errb, code := e.export("--check", "v1.2.4")
	if code != 1 || !strings.Contains(errb, "private pattern 1") {
		t.Errorf("exit %d, stderr %q; want 1 naming private pattern 1", code, errb)
	}
	if strings.Contains(strings.ToLower(out+errb), "zebracorp") {
		t.Errorf("output repeats the leaked text:\n%s%s", out, errb)
	}
}

// TestExportCheckRefusesTheSameUnsafeTreeAnExportWould pins M2: the
// refusal logic (case-fold collisions, submodules, escaping symlinks,
// unreadable content) is shared with a real export, so a private
// pre-commit check on HEAD catches it too.
func TestExportCheckRefusesTheSameUnsafeTreeAnExportWould(t *testing.T) {
	e := newEnv(t)
	e.plumbTag("v1.2.4",
		entry{"100644", "internal/Notes.txt", "hello ZebraCorp\n"},
		entry{"100644", "internal/notes.txt", "clean\n"})
	_, errb, code := e.export("--check", "v1.2.4")
	if code != 1 || !strings.Contains(errb, "differ only in letter case") {
		t.Errorf("exit %d, stderr %q; want 1", code, errb)
	}
}

// TestExportCheckRunsNoGateAndNeedsNoClone pins M2: --check never
// materialises the gate copy and never needs (or touches) a public clone;
// a CLAUDE.md whose gate would fail is irrelevant to it.
func TestExportCheckRunsNoGateAndNeedsNoClone(t *testing.T) {
	e := newEnv(t)
	e.write(map[string]string{"CLAUDE.md": "- The gate:\n```sh\nfalse\n```\n"})
	e.commitAndTag("v1.2.4")
	if _, errb, code := e.export("--check", "v1.2.4"); code != 0 {
		t.Errorf("exit %d, stderr %q; want 0 (the gate must not run under --check)", code, errb)
	}
}

// TestExportCheckUsageErrors pins the usage contract shared with a real
// export: a malformed invocation is exit 2, not a silent no-op.
func TestExportCheckUsageErrors(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"--check"},
		{"--check", "HEAD", "extra"},
	} {
		if _, errb, code := e.export(args...); code != 2 {
			t.Errorf("export-public %v: exit %d, stderr %q, want 2", args, code, errb)
		}
	}
}

// TestExportCheckFailsClosedOnAnUnknownRevision pins M2's own version of
// TestExportNeedsAnExistingVersionTag: an unresolvable REV is exit 1, not
// a silent "nothing selected" success.
func TestExportCheckFailsClosedOnAnUnknownRevision(t *testing.T) {
	e := newEnv(t)
	if _, errb, code := e.export("--check", "does-not-exist"); code != 1 || !strings.Contains(errb, "no revision does-not-exist") {
		t.Errorf("exit %d, stderr %q; want 1 naming the revision", code, errb)
	}
}

// --- Re-review R-M2: scripts/export-public --check-index ---

// TestExportCheckIndexCatchesAStagedLeakBeforeItIsCommitted pins R-M2:
// --check HEAD checks the last commit, so a leak staged for the next
// commit passes it; --check-index checks the tree of the current index
// (git write-tree), so a pre-commit or gate step refuses the change
// itself, before it is ever committed. A leak only in the work tree (not
// staged) is not part of the index, and is not what --check-index checks.
func TestExportCheckIndexCatchesAStagedLeakBeforeItIsCommitted(t *testing.T) {
	e := newEnv(t)
	if out, errb, code := e.export("--check-index"); code != 0 || !strings.Contains(out, "check the index") {
		t.Fatalf("clean index: exit %d, stdout %q, stderr %q; want 0 reporting the index", code, out, errb)
	}
	e.write(map[string]string{"README.md": "hello ZebraCorp\n"})
	if _, errb, code := e.export("--check-index"); code != 0 {
		t.Errorf("unstaged leak: exit %d, stderr %q; want 0 (the work tree is not the index)", code, errb)
	}
	e.git(e.priv, "add", "README.md")
	if _, errb, code := e.export("--check", "HEAD"); code != 0 {
		t.Errorf("--check HEAD with a staged leak: exit %d, stderr %q; want 0 (HEAD itself is clean)", code, errb)
	}
	out, errb, code := e.export("--check-index")
	if code != 1 || !strings.Contains(errb, "README.md:1: private pattern 1") || !strings.Contains(errb, "nothing was checked") {
		t.Errorf("staged leak: exit %d, stderr %q; want 1 naming README.md:1", code, errb)
	}
	if strings.Contains(strings.ToLower(out+errb), "zebracorp") {
		t.Errorf("output repeats the leaked text:\n%s%s", out, errb)
	}
	if n := e.git(e.priv, "rev-list", "--count", "HEAD"); n != "1" {
		t.Errorf("private repo has %s commits, want 1: --check-index must never commit", n)
	}
}

// TestExportCheckIndexUsageErrors pins --check-index's usage contract: it
// takes no argument, and it is not combined with --check REV or a clone.
func TestExportCheckIndexUsageErrors(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{
		{"--check-index", "HEAD"},
		{"--check-index", "--check", "HEAD"},
		{"--check", "HEAD", "--check-index"},
		{"--check-index", "v1.2.3", e.pub},
	} {
		if _, errb, code := e.export(args...); code != 2 {
			t.Errorf("export-public %v: exit %d, stderr %q, want 2", args, code, errb)
		}
	}
}

// --- Re-review R-N2: the gate's git repo holds a real commit ---

// TestExportGateRunsInARealOneCommitRepo pins R-N2: the gate's git repo
// used to hold an index but no objects and no commit, so a gate test
// calling `git rev-parse HEAD`, `log`, `diff` or `show` failed only during
// an export. The gate now runs in a one-commit repo whose HEAD tree is the
// exported tree itself, with a fixed author and date and no signature,
// whatever the owner's own git config says.
func TestExportGateRunsInARealOneCommitRepo(t *testing.T) {
	e := newEnv(t)
	seen := filepath.Join(e.home, "gate-head")
	claude := "# fixture\n\n- The gate, exactly:\n```sh\n" +
		"git rev-parse --verify HEAD\n" +
		"git log -1 --format=%an\n" +
		"git show --stat HEAD\n" +
		"git diff --exit-code HEAD\n" +
		"test -z \"$(git status --porcelain)\"\n" +
		"git log -1 --format='%T|%an <%ae>|%ad|%cn <%ce>|%cd|%G?' --date=raw >" + shQuote(seen) + "\n" +
		"```\n"
	e.write(map[string]string{"CLAUDE.md": claude})
	e.commitAndTag("v1.2.4")
	// The owner signs every commit by default; the gate's commit must not.
	if err := os.WriteFile(filepath.Join(e.home, "gitconfig"), []byte("[commit]\n\tgpgSign = true\n[gpg]\n\tprogram = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mustExport("v1.2.4")
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the gate never recorded its HEAD: %v", err)
	}
	got := strings.TrimSpace(string(b))
	want := e.git(e.pub, "rev-parse", "HEAD^{tree}") + "|" + publicAuthor + "|946684800 +0000|" + publicAuthor + "|946684800 +0000|N"
	if got != want {
		t.Errorf("gate HEAD = %q, want %q (the exported tree, the fixed identity and date, unsigned)", got, want)
	}
}

// --- F231 G1: the gate cannot change what the scans passed ---

// gateEnv is an env whose tag v1.2.4 carries a CLAUDE.md gate made of
// lines, each run by `sh -c` in the gate copy, whose parent directory is
// export-public's own temp directory.
func gateEnv(t *testing.T, gateLines ...string) *env {
	t.Helper()
	e := newEnv(t)
	claude := "# fixture\n\n- The gate, exactly:\n```sh\n" + strings.Join(gateLines, "\n") + "\n```\n"
	e.write(map[string]string{"CLAUDE.md": claude})
	e.commitAndTag("v1.2.4")
	return e
}

// TestExportFailsWhenTheGateRewritesAScannedFile pins F231 G1: code in
// the gate runs as the owner, so it can make the export's temp files
// writable again and rewrite the id-named copy of a blob the scan
// already passed. The export must fail, naming the path, and commit
// nothing.
func TestExportFailsWhenTheGateRewritesAScannedFile(t *testing.T) {
	e := gateEnv(t,
		"chmod -R u+w ..",
		"printf 'hello Zebra%s\\n' Corp >>../blobs/$(printf 'c-hottag fixture\\n' | git hash-object --stdin)",
	)
	e.refused("v1.2.4", "README.md changed during the gate")
}

// TestExportFailsWhenTheGateAddsAFile pins F231 G1's real exploit: a gate
// step that writes a new id-named blob and adds its id and a record to
// the export's own working lists used to have that unscanned file
// committed, because every later check read those same lists. The
// export must fail and commit nothing.
func TestExportFailsWhenTheGateAddsAFile(t *testing.T) {
	e := gateEnv(t,
		"chmod -R u+w ..",
		"printf 'hello Zebra%s\\n' Corp >../x && o=$(git hash-object --stdin <../x) && mv ../x ../blobs/$o && printf '%s\\n' $o >>../oids && printf '100644 blob %s\\tinternal/extra.txt\\n' $o >>../sel",
	)
	e.refused("v1.2.4", "during the gate")
	if strings.Contains(e.git(e.pub, "ls-tree", "-r", "--name-only", "HEAD"), "extra.txt") {
		t.Error("the unscanned file reached the clone")
	}
}

// TestExportFailsWhenTheGateAddsAStrayBlob pins G1's "no file added": a
// file dropped into the scanned-blob directory, even one no list names,
// fails the export.
func TestExportFailsWhenTheGateAddsAStrayBlob(t *testing.T) {
	e := gateEnv(t,
		"chmod -R u+w ..",
		"printf 'stray\\n' >../blobs/stray",
	)
	e.refused("v1.2.4", "stray")
}

// TestExportMakesTheScannedFilesReadOnlyDuringTheGate pins G1's first
// line of defence: an honest gate step that writes into the export's
// temp files (not its own copy of the tree) is refused by the file
// system, so the gate line itself fails.
func TestExportMakesTheScannedFilesReadOnlyDuringTheGate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	e := gateEnv(t,
		"test -f README.md",
		"! printf x 2>/dev/null >>../blobs/$(printf 'c-hottag fixture\\n' | git hash-object --stdin)",
		"! touch ../new 2>/dev/null",
		"! printf x 2>/dev/null >>../sel",
	)
	e.mustExport("v1.2.4")
}

// TestExportFailsWhenTheGateTruncatesItsOwnCommandList pins G1 for the
// gate's command list: a gate line that empties the list file must not
// stop the lines after it from running (they are held in memory), so a
// failing later line still fails the export.
func TestExportFailsWhenTheGateTruncatesItsOwnCommandList(t *testing.T) {
	e := gateEnv(t,
		"chmod -R u+w .. && : >../gatecmds",
		"false",
	)
	e.refused("v1.2.4", "the gate failed")
}

// TestExportCommitsOnlyTheTreeItPinnedBeforeTheGate pins G1's boundary:
// a process the gate left running can still change files after every
// post-gate file check has passed. Here it adds an unscanned blob to the
// clone, an entry for it to the scratch index, and a matching record to
// the post-gate copy of the selection, right before the clone's
// write-tree -- so every file-based check agrees with it. Only the tree
// id pinned in the script's memory before the gate can catch that, and
// must.
func TestExportCommitsOnlyTheTreeItPinnedBeforeTheGate(t *testing.T) {
	e := newEnv(t)
	pub, err := filepath.EvalSymlinks(e.pub)
	if err != nil {
		t.Fatal(err)
	}
	action := `      o=$(printf 'hello Zebra%s\n' Corp | "$real" -C ` + shQuote(pub) + ` hash-object -w --stdin)
      "$real" -C ` + shQuote(pub) + ` update-index --add --cacheinfo "100644,$o,internal/extra.txt"
      s="$(dirname "$GIT_INDEX_FILE")/sel"
      chmod u+w "$s"
      printf '100644 blob %s\tinternal/extra.txt\n' "$o" >>"$s"`
	bin := gitWrapper(t, e.home, action, "write-tree", pub)
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "not the tree that was scanned and gated") {
		t.Errorf("exit %d, stderr %q; want 1 naming the tree mismatch", code, errb)
	}
	if _, err := os.Stat(filepath.Join(e.home, "wrapper-triggered")); err != nil {
		t.Error("the wrapper never ran: the clone's write-tree call changed shape")
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// --- Review round 1: the gate runs as the owner, so the clone is pinned
// across it and re-checked after it ---

// note writes value into $HOME/name, where a gate line can read it: the
// gate runs with the export's own environment, whose HOME is e.home.
func (e *env) note(name, value string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.home, name), []byte(value), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// noExportCommit fails t if any commit in the clone is by the export's
// own identity, or if tag names an export commit.
func (e *env) noExportCommit() {
	e.t.Helper()
	if who := e.git(e.pub, "log", "--all", "--format=%an <%ae>"); strings.Contains(who, publicAuthor) {
		e.t.Errorf("the clone holds an export commit:\n%s", who)
	}
}

// TestExportRefusesWhenTheGateMovesTheClonesRefs pins review round 1, C1:
// the clone's HEAD used to be read only after the gate, so a gate that
// found the clone could commit an unscanned file (and then `git rm` it
// again, to look tidy), and the export committed on top of it with exit
// 0: its own diff looked clean, while history carried the unscanned blob.
// The branch, its commit and the tag's absence are pinned before the
// gate and must be unchanged after it.
func TestExportRefusesWhenTheGateMovesTheClonesRefs(t *testing.T) {
	commit := `c=$(cat "$HOME/pubpath") && printf 'unscanned\n' >"$c/u.txt" && git -C "$c" add u.txt && git -C "$c" commit -q -m gate-commit`
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"a direct commit", []string{commit}, "HEAD moved during the gate"},
		{"a commit, then a tidy git rm", []string{commit, `c=$(cat "$HOME/pubpath") && git -C "$c" rm -q u.txt && git -C "$c" commit -q -m gate-tidy`}, "HEAD moved during the gate"},
		{"a branch switch", []string{`c=$(cat "$HOME/pubpath") && git -C "$c" checkout -q -b other`}, "no longer on refs/heads/main"},
		{"the tag made early", []string{`c=$(cat "$HOME/pubpath") && git -C "$c" tag v1.2.4`}, "tag v1.2.4 appeared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := gateEnv(t, c.lines...)
			e.note("pubpath", e.pub)
			_, errb, code := e.export("v1.2.4", e.pub)
			if code != 1 || !strings.Contains(errb, c.want) {
				t.Errorf("exit %d, stderr %q; want 1 naming %q", code, errb, c.want)
			}
			e.noExportCommit()
		})
	}
}

// TestExportRefusesAnObjectPlantedInTheClone pins review round 1, I1:
// `hash-object -w` only freshens an object that already exists, and git
// does not re-hash on read. A gate that planted a loose object holding
// other bytes at a scanned blob's path got those bytes committed and
// checked out. git fsck, run before any ref moves, refuses the hash-path
// mismatch.
func TestExportRefusesAnObjectPlantedInTheClone(t *testing.T) {
	e := gateEnv(t, `c=$(cat "$HOME/pubpath") && o=$(printf 'c-hottag fixture\n' | git hash-object --stdin) && b=$(printf 'unscanned\n' | git -C "$c" hash-object -w --stdin) && mkdir -p "$c/.git/objects/$(printf %s "$o" | cut -c1-2)" && cp "$c/.git/objects/$(printf %s "$b" | cut -c1-2)/$(printf %s "$b" | cut -c3-)" "$c/.git/objects/$(printf %s "$o" | cut -c1-2)/$(printf %s "$o" | cut -c3-)"`)
	e.note("pubpath", e.pub)
	before := e.git(e.pub, "rev-parse", "HEAD")
	_, errb, code := e.export("v1.2.4", e.pub)
	if code != 1 || !strings.Contains(errb, "git fsck") {
		t.Errorf("exit %d, stderr %q; want 1 naming git fsck", code, errb)
	}
	if after := e.git(e.pub, "rev-parse", "HEAD"); after != before {
		t.Errorf("the clone's HEAD moved: %s -> %s", before, after)
	}
	if tags := e.git(e.pub, "tag", "-l"); tags != "" {
		t.Errorf("the clone has tags %q", tags)
	}
}

// fsmonitorLine is a gate line that writes an fsmonitor hook which, if git
// ever runs it, leaves $HOME/fsm-ran.
const fsmonitorLine = `printf '#!/bin/sh\ntouch "$HOME/fsm-ran"\n' >"$HOME/fsm.sh" && chmod +x "$HOME/fsm.sh"`

// TestExportRefusesAGateChangeToTheClonesConfig pins review round 1, I1:
// the clone's .git/config is hashed before the gate and must be the same
// after it (here the gate sets core.fsmonitor there, which a later
// `status` would run).
func TestExportRefusesAGateChangeToTheClonesConfig(t *testing.T) {
	e := gateEnv(t, fsmonitorLine, `c=$(cat "$HOME/pubpath") && git -C "$c" config core.fsmonitor "$HOME/fsm.sh"`)
	e.note("pubpath", e.pub)
	_, errb, code := e.export("v1.2.4", e.pub)
	if _, err := os.Stat(filepath.Join(e.home, "fsm-ran")); err == nil {
		t.Error("the gate's fsmonitor hook ran")
	}
	if code != 1 || !strings.Contains(errb, "config changed during the gate") {
		t.Errorf("exit %d, stderr %q; want 1 naming the config change", code, errb)
	}
	e.noExportCommit()
}

// TestExportNeverRunsAnFsmonitorHook pins review round 1, I1: every git
// call on the clone runs with core.fsmonitor=false, so even an fsmonitor
// hook set where no check can see it (here the owner's global config,
// which the gate can write too) never runs.
func TestExportNeverRunsAnFsmonitorHook(t *testing.T) {
	e := gateEnv(t, fsmonitorLine, `printf '[core]\n\tfsmonitor = %s\n' "$HOME/fsm.sh" >>"$GIT_CONFIG_GLOBAL"`)
	_, errb, code := e.export("v1.2.4", e.pub)
	if _, err := os.Stat(filepath.Join(e.home, "fsm-ran")); err == nil {
		t.Error("an fsmonitor hook ran during the export")
	}
	if code != 0 {
		t.Errorf("exit %d, stderr %q; want 0", code, errb)
	}
}

// TestExportRefusesInfoAttributesWrittenByTheGate pins review round 1, I1:
// $GIT_DIR/info/attributes, refused up front, is checked again after the
// gate, before the work-tree sync it would apply to.
func TestExportRefusesInfoAttributesWrittenByTheGate(t *testing.T) {
	e := gateEnv(t, `c=$(cat "$HOME/pubpath") && mkdir -p "$c/.git/info" && printf '* text\n' >"$c/.git/info/attributes"`)
	e.note("pubpath", e.pub)
	_, errb, code := e.export("v1.2.4", e.pub)
	if code != 1 || !strings.Contains(errb, "info/attributes") {
		t.Errorf("exit %d, stderr %q; want 1 naming info/attributes", code, errb)
	}
	e.noExportCommit()
}

// TestExportIgnoresCodeTheGateAppendsToTheScript pins review round 1, M1:
// sh reads a script as it runs it, so code the gate appended to the
// export-public file used to run after main returned. The script ends
// with `main "$@"; exit` on one line.
func TestExportIgnoresCodeTheGateAppendsToTheScript(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"export-public", "leak-scan", "manifest-select"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, name), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := gateEnv(t, `s=$(cat "$HOME/scriptpath") && printf '\ntouch "$HOME/appended-ran"\n' >>"$s"`)
	e.note("scriptpath", filepath.Join(bin, "export-public"))
	if _, errb, code := e.exportScript(filepath.Join(bin, "export-public"), "v1.2.4", e.pub); code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, errb)
	}
	if _, err := os.Stat(filepath.Join(e.home, "appended-ran")); err == nil {
		t.Error("code the gate appended to the script ran after main returned")
	}
}

// TestExportGateRepoIgnoresTheOwnersInitTemplate pins review round 1, M3:
// the gate repo is made with `git init --template=`, so nothing from the
// owner's init.templateDir (hooks, info/) lands in it.
func TestExportGateRepoIgnoresTheOwnersInitTemplate(t *testing.T) {
	e := gateEnv(t, "test ! -e .git/from-template")
	tpl := filepath.Join(e.home, "template")
	if err := os.MkdirAll(tpl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tpl, "from-template"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e.note("gitconfig", "[init]\n\ttemplateDir = "+tpl+"\n")
	e.mustExport("v1.2.4")
}

// --- Review round 2: every git-dir file that changes what git reads or
// runs in the clone, and every ref, is pinned across the gate ---

// smudgeLine is a gate line that writes a smudge driver which, if git ever
// runs it, leaves $HOME/smudge-ran.
const smudgeLine = `printf '#!/bin/sh\ntouch "$HOME/smudge-ran"\ncat\n' >"$HOME/smudge.sh" && chmod +x "$HOME/smudge.sh"`

// TestExportRefusesGitDirMetadataWrittenByTheGate pins review round 2, N1,
// N2 and N4. `.git/commondir` is honoured even in a main work tree, so a
// gate that copied .git elsewhere and pointed commondir at it swapped the
// config, info/ and refs every later check read; a smudge driver named
// there then ran inside the export's own final reset --hard, after fsck
// and both ref moves (and could move the tag). info/grafts, shallow and
// objects/info/alternates change which objects history (and a push)
// reaches. Each is digested before the gate and must be unchanged after
// it, checked before any other git call on the clone.
func TestExportRefusesGitDirMetadataWrittenByTheGate(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"commondir, with a smudge driver there", []string{
			smudgeLine,
			`c=$(cat "$HOME/pubpath") && cp -R "$c/.git" "$HOME/common" && mkdir -p "$HOME/common/info" && printf '* filter=evil\n' >"$HOME/common/info/attributes" && git config -f "$HOME/common/config" filter.evil.smudge "$HOME/smudge.sh" && git config -f "$HOME/common/config" filter.evil.required true && printf '%s\n' "$HOME/common" >"$c/.git/commondir"`,
		}, "commondir changed during the gate"},
		{"info/grafts", []string{`c=$(cat "$HOME/pubpath") && mkdir -p "$c/.git/info" && git -C "$c" rev-parse HEAD >"$c/.git/info/grafts"`}, "info/grafts changed during the gate"},
		{"shallow", []string{`c=$(cat "$HOME/pubpath") && git -C "$c" rev-parse HEAD >"$c/.git/shallow"`}, "shallow changed during the gate"},
		{"objects/info/alternates", []string{`git init -q --bare "$HOME/alt.git" && c=$(cat "$HOME/pubpath") && mkdir -p "$c/.git/objects/info" && printf '%s\n' "$HOME/alt.git/objects" >"$c/.git/objects/info/alternates"`}, "alternates changed during the gate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := gateEnv(t, c.lines...)
			e.note("pubpath", e.pub)
			_, errb, code := e.export("v1.2.4", e.pub)
			if _, err := os.Stat(filepath.Join(e.home, "smudge-ran")); err == nil {
				t.Error("a smudge driver the gate planted ran during the export")
			}
			if code != 1 || !strings.Contains(errb, c.want) {
				t.Errorf("exit %d, stderr %q; want 1 naming %q", code, errb, c.want)
			}
		})
	}
}

// TestExportRefusesACloneWithCommondir pins review round 2, N1 up front: a
// clone whose .git holds a commondir file (or whose common dir is not its
// git dir) is refused before anything runs.
func TestExportRefusesACloneWithCommondir(t *testing.T) {
	e := newEnv(t)
	common := filepath.Join(e.home, "common")
	e.git(e.home, "clone", "-q", "--bare", e.pub, common)
	if err := os.WriteFile(filepath.Join(e.pub, ".git", "commondir"), []byte(common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errb, code := e.export("v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "commondir") {
		t.Errorf("exit %d, stderr %q; want 1 naming commondir", code, errb)
	}
}

// TestExportRefusesARefAddedByTheGate pins review round 2, N2: the clone's
// full for-each-ref output is pinned across the gate, so an extra tag or a
// refs/replace/ entry (which reset --hard and show would follow) refuses
// the export. Every clone call also runs with GIT_NO_REPLACE_OBJECTS=1.
func TestExportRefusesARefAddedByTheGate(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"an extra tag", `c=$(cat "$HOME/pubpath") && git -C "$c" tag other`},
		{"a replace ref", `c=$(cat "$HOME/pubpath") && r=$(git -C "$c" commit-tree -m replaced "$(git -C "$c" rev-parse 'HEAD^{tree}')") && git -C "$c" replace HEAD "$r"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := gateEnv(t, c.line)
			e.note("pubpath", e.pub)
			_, errb, code := e.export("v1.2.4", e.pub)
			if code != 1 || !strings.Contains(errb, "refs changed during the gate") {
				t.Errorf("exit %d, stderr %q; want 1 naming the ref change", code, errb)
			}
			e.noExportCommit()
		})
	}
}

// TestExportRefusesATagMovedDuringTheFinalReset pins review round 2's
// last check: after the final reset --hard (the one step that can run a
// driver after both ref moves), the tag and HEAD must both still be the
// export commit. A git wrapper moves the tag just as that reset starts.
func TestExportRefusesATagMovedDuringTheFinalReset(t *testing.T) {
	e := newEnv(t)
	pub, err := filepath.EvalSymlinks(e.pub)
	if err != nil {
		t.Fatal(err)
	}
	action := `      "$real" -C ` + shQuote(pub) + ` update-ref refs/tags/v1.2.3 "$("$real" -C ` + shQuote(pub) + ` rev-parse HEAD^)"`
	bin := gitWrapper(t, e.home, action, "reset", "--hard")
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "not the export commit after the work-tree sync") {
		t.Errorf("exit %d, stderr %q; want 1 naming the moved tag", code, errb)
	}
}

// --- Review round 3: git-dir discovery is pinned ---

// TestExportRefusesACloneWhoseGitDirIsNotAPlainDirectory pins review
// round 3, item 1: the clone's git dir must be exactly CLONE/.git, a real
// directory -- never a gitfile (`clone --separate-git-dir`, a worktree)
// or a symlink, either of which a gate could repoint.
func TestExportRefusesACloneWhoseGitDirIsNotAPlainDirectory(t *testing.T) {
	cases := []struct {
		name string
		make func(e *env, moved string)
	}{
		{"a gitfile", func(e *env, moved string) {
			if err := os.WriteFile(filepath.Join(e.pub, ".git"), []byte("gitdir: "+moved+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink", func(e *env, moved string) {
			if err := os.Symlink(moved, filepath.Join(e.pub, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			moved := filepath.Join(e.home, "moved.git")
			if err := os.Rename(filepath.Join(e.pub, ".git"), moved); err != nil {
				t.Fatal(err)
			}
			c.make(e, moved)
			if got := e.git(e.pub, "rev-parse", "--is-inside-work-tree"); got != "true" {
				t.Fatalf("fixture: the clone is not a work tree (%q)", got)
			}
			_, errb, code := e.export("v1.2.3", e.pub)
			if code != 1 || !strings.Contains(errb, "is not a plain directory") {
				t.Errorf("exit %d, stderr %q; want 1 naming the git dir", code, errb)
			}
		})
	}
}

// TestExportPinsGitDirDiscoveryAcrossTheGate pins review round 3, item 2:
// every clone call runs with GIT_DIR and GIT_WORK_TREE pinned, and plain
// discovery from the clone must still resolve to CLONE/.git after the
// gate. The first case is the re-review's shape: the gate copies .git to
// the clone's parent directory (core.worktree pointing back, a smudge
// driver there) and moves .git/HEAD aside, so discovery falls through to
// the parent. The second turns .git into a gitfile pointing at such a
// copy. Both are refused, and the smudge never runs.
func TestExportPinsGitDirDiscoveryAcrossTheGate(t *testing.T) {
	evil := `git config -f "$g/config" filter.evil.smudge "$HOME/smudge.sh" && git config -f "$g/config" filter.evil.required true && mkdir -p "$g/info" && printf '* filter=evil\n' >"$g/info/attributes"`
	cases := []struct {
		name string
		line string
	}{
		{"HEAD moved aside, a repo in the parent", `c=$(cat "$HOME/pubpath") && g=$(dirname "$c")/.git && cp -R "$c/.git" "$g" && git config -f "$g/config" core.worktree "$c" && ` + evil + ` && mv "$c/.git/HEAD" "$c/.git/HEAD.moved"`},
		{".git turned into a gitfile", `c=$(cat "$HOME/pubpath") && g="$HOME/Y" && cp -R "$c/.git" "$g" && ` + evil + ` && rm -rf "$c/.git" && printf 'gitdir: %s\n' "$g" >"$c/.git"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := gateEnv(t, smudgeLine, c.line)
			e.note("pubpath", e.pub)
			_, errb, code := e.export("v1.2.4", e.pub)
			if _, err := os.Stat(filepath.Join(e.home, "smudge-ran")); err == nil {
				t.Error("a smudge driver the gate planted ran during the export")
			}
			if code != 1 || !strings.Contains(errb, "during the gate") {
				t.Errorf("exit %d, stderr %q; want 1, refused after the gate", code, errb)
			}
		})
	}
}

// TestExportDiesWhenGitStatusFails pins review round 3, item 5: a failing
// `git status` in the clone must refuse the export, not read as a clean
// work tree (its output is empty either way). A git wrapper makes the
// first status call fail with no output.
func TestExportDiesWhenGitStatusFails(t *testing.T) {
	e := newEnv(t)
	bin := gitWrapper(t, e.home, "      exit 1", "status")
	before := e.pubState()
	_, errb, code := e.exportWithPath(bin, "v1.2.3", e.pub)
	if code != 1 || !strings.Contains(errb, "git status failed") {
		t.Errorf("exit %d, stderr %q; want 1 naming the failed status", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("public clone changed: %q -> %q", before, after)
	}
}

// syncManifest also selects .github/ and plugin/, so a --sync test can change
// a workflow or a plugin doc.
const syncManifest = "README.md\nCLAUDE.md\nrun.sh\ninternal/\ndocs/\n.github/\nplugin/\n.claude-plugin/\npublic-manifest.txt\n!docs/internal/\n"

// syncEnv is an env whose v1.2.3 export is done and whose fixture also holds
// a workflow and a plugin doc (committed and exported as v1.2.4).
func syncEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.write(map[string]string{
		"public-manifest.txt":      syncManifest,
		".github/workflows/ci.yml": "name: ci\n",
		"plugin/skills/x/SKILL.md": "skill\n",
		".claude-plugin/README.md": "marketplace\n",
		"docs/guide.md":            "guide\n",
		"docs/extra.md":            "extra\n",
	})
	e.commitAndTag("v1.2.4")
	e.mustExport("v1.2.4")
	return e
}

func (e *env) commitAll(msg string) {
	e.t.Helper()
	e.git(e.priv, "add", "-A")
	e.git(e.priv, "commit", "-q", "-m", msg)
}

func TestSyncCommitsADocsOnlyChangeWithoutATag(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	head := e.git(e.pub, "rev-parse", "HEAD")
	tags := e.git(e.pub, "tag", "-l")
	e.write(map[string]string{"docs/release-notes/v1.md": "new notes\n", "docs/new.md": "new\n"})
	e.commitAll("docs")
	out, errb, code := e.export("--sync", "HEAD", e.pub)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	short := e.git(e.pub, "rev-parse", "--short", "HEAD")
	if want := "(docs sync after v1.2.3)."; !strings.Contains(out, want) || !strings.Contains(out, "as "+short+" ") {
		t.Errorf("stdout %q lacks the final line with %q and short id %s", out, want, short)
	}
	if n := e.git(e.pub, "rev-list", "--count", head+"..HEAD"); n != "1" {
		t.Errorf("%s new commits, want exactly 1", n)
	}
	if e.git(e.pub, "rev-parse", "HEAD^") != head {
		t.Error("the sync commit is not on top of the clone's old HEAD")
	}
	if got := e.git(e.pub, "tag", "-l"); got != tags {
		t.Errorf("tags %q -> %q, want none created", tags, got)
	}
	raw := e.git(e.pub, "cat-file", "commit", "HEAD")
	if _, msg, _ := strings.Cut(raw, "\n\n"); msg != "c-hottag docs update after v1.2.3" {
		t.Errorf("commit message = %q", msg)
	}
	if tr := e.git(e.pub, "log", "-1", "--format=%(trailers)"); tr != "" {
		t.Errorf("trailers %q", tr)
	}
	if who := e.git(e.pub, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); who != publicAuthor+"|"+publicAuthor {
		t.Errorf("author|committer = %q", who)
	}
	if got := e.git(e.pub, "show", "HEAD:docs/new.md"); got != "new" {
		t.Errorf("docs/new.md = %q", got)
	}
	if st := e.git(e.pub, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Errorf("work tree not clean: %q", st)
	}
}

func TestSyncRefusesACodeChange(t *testing.T) {
	cases := []struct{ path, body string }{
		{"internal/a.go", "package a\n// changed\n"},
		{"run.sh", "#!/bin/sh\necho changed\n"},
		{".github/workflows/ci.yml", "name: changed\n"},
		{"plugin/skills/x/SKILL.md", "changed skill\n"},
		{".claude-plugin/README.md", "changed marketplace\n"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			e := syncEnv(t)
			e.write(map[string]string{c.path: c.body, "docs/guide.md": "changed guide\n"})
			e.commitAll("mixed")
			before := e.pubState()
			refs := e.git(e.pub, "for-each-ref")
			_, errb, code := e.export("--sync", "HEAD", e.pub)
			if code == 0 || !strings.Contains(errb, c.path+" changed") {
				t.Errorf("exit %d, stderr %q; want a refusal naming %s", code, errb, c.path)
			}
			if after := e.pubState(); after != before {
				t.Errorf("clone changed: %q -> %q", before, after)
			}
			if got := e.git(e.pub, "for-each-ref"); got != refs {
				t.Errorf("refs changed: %q -> %q", refs, got)
			}
		})
	}
}

func TestSyncNamesTheFirstOffendingPath(t *testing.T) {
	e := syncEnv(t)
	e.write(map[string]string{"run.sh": "#!/bin/sh\necho changed\n", "internal/a.go": "package a\n// changed\n"})
	e.commitAll("code")
	_, errb, code := e.export("--sync", "HEAD", e.pub)
	if code == 0 || !strings.Contains(errb, "internal/a.go changed") {
		t.Errorf("exit %d, stderr %q; want the first sorted offender internal/a.go", code, errb)
	}
}

func TestSyncCountsDeletionsAndRenames(t *testing.T) {
	e := syncEnv(t)
	e.git(e.priv, "rm", "-q", "docs/extra.md")
	e.git(e.priv, "mv", "docs/guide.md", "docs/manual.md")
	e.commitAll("delete and rename docs")
	if out, errb, code := e.export("--sync", "HEAD", e.pub); code != 0 {
		t.Fatalf("docs delete/rename refused: exit %d\n%s%s", code, out, errb)
	}
	if got := e.git(e.pub, "ls-files", "docs"); strings.Contains(got, "extra.md") || strings.Contains(got, "guide.md") || !strings.Contains(got, "docs/manual.md") {
		t.Errorf("docs in the clone = %q", got)
	}

	e2 := syncEnv(t)
	e2.git(e2.priv, "rm", "-q", "internal/a.go")
	e2.commitAll("delete code")
	before := e2.pubState()
	_, errb, code := e2.export("--sync", "HEAD", e2.pub)
	if code == 0 || !strings.Contains(errb, "internal/a.go changed") {
		t.Errorf("deleting a non-docs file: exit %d, stderr %q; want a refusal", code, errb)
	}
	if after := e2.pubState(); after != before {
		t.Errorf("clone changed: %q -> %q", before, after)
	}
}

func TestSyncRefusesAnUnbornOrUntaggedClone(t *testing.T) {
	// Unborn: a fresh clone with no commit.
	e := newEnv(t)
	unborn := filepath.Join(filepath.Dir(e.pub), "unborn")
	e.git(filepath.Dir(e.pub), "init", "-q", "-b", "main", unborn)
	if _, errb, code := e.export("--sync", "HEAD", unborn); code == 0 || !strings.Contains(errb, "no commit yet") {
		t.Errorf("unborn: exit %d, stderr %q", code, errb)
	}
	if st := e.git(unborn, "status", "--porcelain", "--ignored") + e.git(unborn, "for-each-ref"); st != "" {
		t.Errorf("unborn clone changed: %q", st)
	}
	// Untagged: e.pub has one commit and no v* tag.
	before := e.pubState()
	if _, errb, code := e.export("--sync", "HEAD", e.pub); code == 0 || !strings.Contains(errb, "no v* tag") {
		t.Errorf("untagged: exit %d, stderr %q", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("clone changed: %q -> %q", before, after)
	}
}

func TestSyncRefusesNoChange(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	before := e.pubState()
	_, errb, code := e.export("--sync", "HEAD", e.pub)
	if code == 0 || !strings.Contains(errb, "nothing to sync") {
		t.Errorf("exit %d, stderr %q; want nothing to sync", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("clone changed: %q -> %q", before, after)
	}
}

func TestSyncRejectsBadUsage(t *testing.T) {
	e := newEnv(t)
	for _, args := range [][]string{{"--sync", "HEAD"}, {"--sync", "--check", "HEAD"}, {"--check", "--sync", "HEAD"}} {
		if _, _, code := e.export(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestSyncRefusesAnOddlyNamedBaseTag(t *testing.T) {
	e := newEnv(t)
	e.mustExport("v1.2.3")
	e.git(e.pub, "commit", "-q", "--allow-empty", "-m", "later")
	e.git(e.pub, "tag", "vnext")
	e.write(map[string]string{"docs/new.md": "new\n"})
	e.commitAll("docs")
	before := e.pubState()
	_, errb, code := e.export("--sync", "HEAD", e.pub)
	if code == 0 || !strings.Contains(errb, "does not look like") {
		t.Errorf("exit %d, stderr %q", code, errb)
	}
	if after := e.pubState(); after != before {
		t.Errorf("clone changed: %q -> %q", before, after)
	}
}
