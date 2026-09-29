package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installAtVersion moves ti onto the M3 layout (N4): the running chottag is
// <home>/versions/<ver>/chottag and bin/ links point at it. TempDirs is set
// to an unrelated dir, as in production, where the refusal is on and
// ~/.chottag is under no temp dir.
func installAtVersion(t *testing.T, ti *testInstall, ver string) string {
	t.Helper()
	dir := filepath.Join(ti.home, "versions", ver)
	must(t, os.MkdirAll(dir, 0o700))
	exe := writeExecutable(t, dir, "chottag")
	must(t, testProvisionBin(exe, filepath.Join(ti.home, "bin")))
	ti.env.Executable = exe
	ti.env.TempDirs = []string{t.TempDir()}
	return exe
}

func TestBinIsOKForAnInstallUnderVersions(t *testing.T) {
	ti := newTestInstall(t)
	exe := installAtVersion(t, ti, "0.3.0")
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "bin")
	if r.Status != StatusOK || !strings.Contains(r.Detail, exe) {
		t.Fatalf("bin = %+v, want ok naming %s", r, exe)
	}
}

func TestBinRepairsAMissingLinkForAnInstallUnderVersions(t *testing.T) {
	ti := newTestInstall(t)
	installAtVersion(t, ti, "0.3.0")
	breakThenRepair(t, ti, InstallChecks(), "bin", func() {
		must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	}, "home:bin/claude")
}

// After an upgrade, bin/ points at 0.4.0. Doctor run from the old 0.3.0
// binary must report, never repoint the links back (I3).
func TestBinRunFromAnOlderVersionIsReportOnly(t *testing.T) {
	ti := newTestInstall(t)
	installAtVersion(t, ti, "0.4.0")
	oldDir := filepath.Join(ti.home, "versions", "0.3.0")
	must(t, os.MkdirAll(oldDir, 0o700))
	ti.env.Executable = writeExecutable(t, oldDir, "chottag")
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "bin")
	if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag setup") {
		t.Fatalf("bin --fix = %+v, want a report-only problem with the setup hint", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}
