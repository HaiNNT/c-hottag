package cli

import (
	"path/filepath"
	"testing"
)

// TestHomeAbsolutisesARelativeChottagHome (F5): a relative CHOTTAG_HOME must
// come back absolute, since two processes (e.g. the daemon and a later CLI
// invocation) practically never share a current directory, and a relative
// path would otherwise resolve to two different trees.
func TestHomeAbsolutisesARelativeChottagHome(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CHOTTAG_HOME", "relative-home")

	got, err := home()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("home() = %q, want an absolute path", got)
	}
	if want := filepath.Join(dir, "relative-home"); got != want {
		t.Errorf("home() = %q, want %q", got, want)
	}
}

// TestArgv0SelectsTheShim pins the argv[0] dispatch that lets one binary
// serve both roles (§4.3): invoked as `claude`, chottag becomes the shim;
// invoked as `chottag` (by any path), it stays the CLI. It calls
// isShimInvocation directly rather than Run itself: this package's TestMain
// (invariants_test.go) does now guard Run's shim branch with a panicking
// execFn/spawnFn pair, same as internal/shim's own TestMain, but a test that
// only cares about the argv0 decision itself has no reason to also drive a
// stubbed daemon health probe just to reach it.
func TestArgv0SelectsTheShim(t *testing.T) {
	cases := []struct {
		name     string
		argv0    string
		wantShim bool
	}{
		{"invoked as claude", "/home/u/.chottag/bin/claude", true},
		{"invoked as chottag", "/usr/local/bin/chottag", false},
		{"invoked by full path with args", "/home/u/.chottag/bin/claude", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isShimInvocation(c.argv0); got != c.wantShim {
				t.Errorf("isShimInvocation(%q) = %v, want %v", c.argv0, got, c.wantShim)
			}
		})
	}
}
