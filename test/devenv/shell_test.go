package devenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellKeepsDevBinFirst (R170): `scripts/dev-env shell` starts an
// interactive shell that re-reads the user's rc file, which puts prod's bin
// dir first on PATH again. The shell must still end up with the dev bin dir
// first and nothing under $HOME/.chottag on PATH. Everything runs in a temp
// HOME; the real HOME is never read or written.
func TestShellKeepsDevBinFirst(t *testing.T) {
	for _, sh := range []string{"zsh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			shPath, err := exec.LookPath(sh)
			if err != nil {
				t.Skipf("%s not installed", sh)
			}
			if _, err := exec.LookPath("git"); err != nil {
				t.Skip("git not installed")
			}
			// Piped stdin makes a plain shell non-interactive, so it would not
			// read its rc file; a wrapper named like the shell forces -i.
			// (dev-env already passes -i to bash, with its own rc file.)
			wrap := filepath.Join(t.TempDir(), sh)
			script := "#!/bin/sh\n[ $# -gt 0 ] || set -- -i\nexec " + shPath + " \"$@\"\n"
			if err := os.WriteFile(wrap, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			root := filepath.Join(t.TempDir(), "dev")
			devBin := filepath.Join(root, "home", ".chottag", "bin")
			if err := os.MkdirAll(devBin, 0o755); err != nil {
				t.Fatal(err)
			}
			prodBin := filepath.Join(home, ".chottag", "bin")
			if err := os.MkdirAll(prodBin, 0o755); err != nil {
				t.Fatal(err)
			}
			rc := "export PATH=\"$HOME/.chottag/bin:$PATH\"\n"
			for _, f := range []string{".zshrc", ".bashrc"} {
				if err := os.WriteFile(filepath.Join(home, f), []byte(rc), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			devenv, err := filepath.Abs(filepath.Join("..", "..", "scripts", "dev-env"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(devenv, "shell")
			cmd.Env = []string{
				"HOME=" + home,
				"SHELL=" + wrap,
				"CHOTTAG_DEV_ROOT=" + root,
				"PATH=" + prodBin + ":/usr/bin:/bin",
			}
			cmd.Stdin = strings.NewReader("echo DEVPATH=$PATH\nexit\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("dev-env shell: %v\n%s", err, out)
			}
			var path string
			for _, l := range strings.Split(string(out), "\n") {
				if i := strings.LastIndex(l, "DEVPATH="); i >= 0 && !strings.Contains(l, "echo") {
					path = l[i+len("DEVPATH="):]
				}
			}
			if path == "" {
				t.Fatalf("no PATH line in output:\n%s", out)
			}
			entries := strings.Split(path, ":")
			if resolved, _ := filepath.EvalSymlinks(entries[0]); entries[0] != devBin && resolved != devBin {
				t.Errorf("first PATH entry = %q, want %q", entries[0], devBin)
			}
			for _, e := range entries {
				if strings.HasPrefix(e, filepath.Join(home, ".chottag")) {
					t.Errorf("PATH keeps prod entry %q: %s", e, path)
				}
			}
			if _, err := os.Stat(filepath.Join(home, ".zshenv")); err == nil {
				t.Errorf("a file appeared in the temp HOME")
			}
		})
	}
}
