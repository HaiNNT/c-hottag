// Package leakscan tests scripts/leak-scan and scripts/manifest-select, and
// the repo's own public-manifest.txt. Every fixture that has to look like a
// leak is assembled at run time from pieces (homePath, email, sessionURL,
// trailer), so this package's own source never trips the scan it tests:
// CI scans test/ too.
package leakscan

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// script runs scripts/<name> under /bin/sh in dir, with stdin, and returns
// stdout, stderr and the exit status. HOME and TMPDIR are fresh temp dirs:
// a script under test never sees the real HOME.
func script(t *testing.T, dir, stdin, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{filepath.Join(root(t), "scripts", name)}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %s: %v", name, err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// waitForGlob blocks until a path matching glob exists, polling at a fine
// grain rather than sleeping a fixed guessed duration (M5): the caller
// waits for an event the script under test itself produces (a marker file
// created right after it installs its signal traps), not for a wall-clock
// value. deadline is a hang guard only, never a correctness bound.
func waitForGlob(t *testing.T, glob string, deadline time.Duration) {
	t.Helper()
	give := time.Now().Add(deadline)
	for {
		if matches, err := filepath.Glob(glob); err == nil && len(matches) > 0 {
			return
		}
		if time.Now().After(give) {
			t.Fatalf("timed out after %s waiting for %s", deadline, glob)
		}
		time.Sleep(time.Millisecond)
	}
}

// tree writes files (slash path -> content) under a fresh directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The pieces below are joined at run time; see the package comment.
func homePath(name string) string       { return "/" + "Users/" + name + "/.chottag" }
func lowerHomePath(name string) string  { return "/" + "users/" + name + "/.chottag" }
func jsonHomePath(name string) string   { return "\\/" + "Users" + "\\/" + name }
func email(local, domain string) string { return local + "@" + domain }
func sessionURL() string                { return "claude.ai/code/" + "session_" + "01ABCDEF" }
func trailer() string                   { return "Claude-" + "Session: https://example.com/x" }
func trailerCased(prefix string) string { return prefix + "-" + "Session: https://example.com/x" }
func bareSessionID() string             { return "session_01" + "ABCDEFGHIJ0123456789" }
