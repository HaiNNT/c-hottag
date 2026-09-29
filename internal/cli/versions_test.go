package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M3 N4: install.sh puts each binary at $CHOTTAG_HOME/versions/<ver>/chottag
// and runs ITS setup, so bin/chottag and bin/claude point into the home.
// These pin what setup, uninstall and --purge do with that layout.

func writeInstalledVersion(t *testing.T, home, ver string) string {
	t.Helper()
	dir := filepath.Join(home, "versions", ver)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "chottag")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// assertBinLinksResolveTo checks both links resolve to exe, and that
// bin/chottag's own target is exactly want (no chain, no loop).
func assertBinLinksResolveTo(t *testing.T, binDir, exe, want string) {
	t.Helper()
	target, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"chottag", "claude"} {
		got, err := filepath.EvalSymlinks(filepath.Join(binDir, name))
		if err != nil {
			t.Fatalf("bin/%s does not resolve: %v", name, err)
		}
		if got != target {
			t.Errorf("bin/%s resolves to %s, want %s", name, got, target)
		}
	}
	if got, err := os.Readlink(filepath.Join(binDir, "chottag")); err != nil || got != want {
		t.Errorf("bin/chottag -> %q (err %v), want %q", got, err, want)
	}
}

func TestProvisionBinSymlinksRepointsOnUpgrade(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	v1 := writeInstalledVersion(t, home, "0.3.0")
	v2 := writeInstalledVersion(t, home, "0.4.0")

	for _, step := range []struct{ exe, name string }{{v1, "install 0.3.0"}, {v1, "re-run 0.3.0"}, {v2, "upgrade to 0.4.0"}} {
		if err := provisionBinSymlinks(step.exe, binDir); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		assertBinLinksResolveTo(t, binDir, step.exe, step.exe)
	}
	if _, err := os.Stat(v1); err != nil {
		t.Errorf("the old version's binary is gone: %v (N4 keeps it)", err)
	}
}

// On darwin os.Executable reports the path the process was started by
// (F190), so `chottag setup` typed in a shell reports $CHOTTAG_HOME/bin/
// chottag, the link itself; Linux reports the resolved file. From the link,
// neither link may become a loop, and both must still resolve to the
// installed version.
func TestProvisionBinSymlinksFromTheLinkItself(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	v1 := writeInstalledVersion(t, home, "0.3.0")
	if err := provisionBinSymlinks(v1, binDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(binDir, "claude")); err != nil {
		t.Fatal(err)
	}

	if err := provisionBinSymlinks(filepath.Join(binDir, "chottag"), binDir); err != nil {
		t.Fatal(err)
	}
	assertBinLinksResolveTo(t, binDir, v1, v1)
}

func setupAtVersion(t *testing.T) (home, v string) {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/zsh")
	if code := runSetup(nil, newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("setup = %d", code)
	}
	return home, writeInstalledVersion(t, home, "0.3.0")
}

// Plain uninstall "removes exactly" the rc block, the two links and
// bundle.pem (parent §5); the installed binaries stay (M3 plan ruling 9).
func TestUninstallKeepsInstalledVersions(t *testing.T) {
	home, v := setupAtVersion(t)
	var out bytes.Buffer
	if code := runUninstall(nil, strings.NewReader(""), newReporter(true, &out, io.Discard)); code != 0 {
		t.Fatalf("uninstall = %d: %s", code, out.String())
	}
	var doc struct {
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	for _, p := range doc.Removed {
		if strings.HasPrefix(p, filepath.Join(home, "versions")) {
			t.Errorf("uninstall reports removing %s", p)
		}
	}
	if _, err := os.Stat(v); err != nil {
		t.Errorf("plain uninstall removed the installed binary: %v", err)
	}
}

// N4: --purge removes versions/ too. It does, because it is inside
// $CHOTTAG_HOME and --purge is RemoveAll of the home (F189).
func TestPurgeRemovesInstalledVersions(t *testing.T) {
	interactiveStdin(t)
	home, _ := setupAtVersion(t)
	if code := runUninstall([]string{"--purge"}, strings.NewReader("purge\n"), newReporter(false, io.Discard, io.Discard)); code != 0 {
		t.Fatalf("uninstall --purge = %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "versions")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--purge left versions/ (err %v)", err)
	}
}
