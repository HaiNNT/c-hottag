package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/daemonlock"
)

func TestInstallChecksAreOKOnAHealthyInstall(t *testing.T) {
	ti := newTestInstall(t)
	for _, r := range mustRun(t, ti.env, InstallChecks(), false) {
		if r.Status != StatusOK {
			t.Errorf("%+v, want ok", r)
		}
	}
}

func TestSetupMissingSkipsEveryOtherCheckAndCreatesNothing(t *testing.T) {
	for _, name := range []string{"absent", "empty"} {
		t.Run(name, func(t *testing.T) {
			ti := newTestInstall(t)
			home := filepath.Join(t.TempDir(), "chottag")
			if name == "empty" {
				must(t, os.Mkdir(home, 0o700))
			}
			ti.env.Home = home
			rows := mustRun(t, ti.env, InstallChecks(), true)
			if r := rows[0]; r.ID != "setup" || r.Status != StatusProblem || r.Hint != "chottag setup" {
				t.Fatalf("setup = %+v, want a problem with hint chottag setup (D3)", r)
			}
			for _, r := range rows[1:] {
				if r.Status != StatusSkipped {
					t.Errorf("%+v, want skipped", r)
				}
			}
			entries, err := os.ReadDir(home)
			if name == "absent" && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("doctor created %s", home)
			}
			if name == "empty" && (err != nil || len(entries) != 0) {
				t.Fatalf("doctor wrote into %s: %v %v", home, entries, err)
			}
		})
	}
}

func TestSetupAcceptsAFreshSetupWithNoStateYet(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "state.json")))
	if r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "setup"); r.Status != StatusOK {
		t.Fatalf("setup = %+v, want ok: bin/ proves setup ran (F169)", r)
	}
}

func TestSetupReportsACorruptStateAsAnInternalError(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.WriteFile(filepath.Join(ti.home, "state.json"), []byte("{"), 0o600))
	if _, err := Run(ti.env, InstallChecks(), false); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want the corrupt state.json as an internal error", err)
	}
}

func TestTreeBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	breakThenRepair(t, ti, InstallChecks(), "tree", func() {
		must(t, os.Remove(filepath.Join(ti.home, "cache")))
	}, "home:cache")
}

func TestCABreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	// I2: doctor's ca fix refuses while a daemon holds the lock, so this
	// break-then-repair test (which needs the fix to actually run) needs
	// no daemon running — TestCARefusesToFixUnderARunningDaemon covers the
	// other half.
	ti.running = false
	dir := filepath.Join(ti.home, "ca")
	breakThenRepair(t, ti, InstallChecks(), "ca", func() {
		must(t, os.Remove(filepath.Join(dir, "ca.pem")))
		must(t, os.Remove(filepath.Join(dir, "ca.key")))
	}, "home:ca/ca.pem", "home:ca/ca.key", "home:ca/ca.lock")
	if _, err := ca.Load(dir); err != nil {
		t.Fatalf("the repaired CA does not load: %v", err)
	}
}

// TestCARefusesToFixUnderARunningDaemon is I2: a daemon that is already
// running keeps signing with its own in-memory CA. Creating a new pair
// out from under it would make every NEW session trust the new CA while
// the running daemon still signs with the old one — a TLS failure doctor
// itself would just have caused. So the fix refuses (report-only, hint
// `chottag daemon restart`: the daemon's own LoadOrCreateLocked creates
// the pair on start) and writes nothing, whether the daemon is healthy or
// merely holds the lock with an unreadable record.
func TestCARefusesToFixUnderARunningDaemon(t *testing.T) {
	for _, tc := range []struct {
		name       string
		running    bool
		inspectErr error
	}{
		{name: "running and healthy", running: true},
		{name: "lock held, record unreadable", inspectErr: daemonlock.ErrUnreadableRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			ti.running, ti.inspectErr = tc.running, tc.inspectErr
			dir := filepath.Join(ti.home, "ca")
			must(t, os.Remove(filepath.Join(dir, "ca.pem")))
			must(t, os.Remove(filepath.Join(dir, "ca.key")))
			before := ti.snapshot()
			r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "ca")
			if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag daemon restart") {
				t.Fatalf("ca = %+v, want a report-only problem hinting chottag daemon restart", r)
			}
			assertChangedOnly(t, before, ti.snapshot())
		})
	}
}

// TestCAFailsClosedOnAnUnreadableDaemonLockState is the controller's
// fail-closed ruling on I2's final review: an Inspect error that is
// neither nil nor daemonlock.ErrUnreadableRecord means the lock's state is
// genuinely UNKNOWN, not "no daemon" — daemonHoldsLock must not guess "no
// daemon" and let a caller proceed. Two sites, one test each:
//   - Detect (through doctor.Run, --fix included): the row stays a
//     report-only problem, surfacing the error, and --fix changes nothing,
//     because Detect's own Hint keeps Run's `fixable` gate from ever
//     calling Fix at all.
//   - Fix itself, called directly (bypassing Detect's gate, the way the
//     belt-and-braces comment on caCheck's Fix says it must): it returns
//     the Inspect error unchanged and creates no CA.
func TestCAFailsClosedOnAnUnreadableDaemonLockState(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "ca", "ca.pem")))
	must(t, os.Remove(filepath.Join(ti.home, "ca", "ca.key")))
	genericErr := errors.New("some transient lock-record read failure")
	ti.inspectErr = genericErr
	before := ti.snapshot()

	// Site 1: Detect.
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "ca")
	if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag daemon restart") || !strings.Contains(r.Detail, genericErr.Error()) {
		t.Fatalf("ca = %+v, want a report-only problem surfacing the Inspect error", r)
	}
	assertChangedOnly(t, before, ti.snapshot())

	// Site 2: Fix, direct.
	if err := caCheck().Fix(ti.env); !errors.Is(err, genericErr) {
		t.Fatalf("Fix err = %v, want the Inspect error returned unchanged", err)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestCANeverReplacesHalfAPair(t *testing.T) {
	for _, gone := range []string{"ca.pem", "ca.key"} {
		t.Run(gone, func(t *testing.T) {
			ti := newTestInstall(t)
			must(t, os.Remove(filepath.Join(ti.home, "ca", gone)))
			before := ti.snapshot()
			r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "ca")
			if r.Status != StatusProblem || !strings.Contains(r.Detail, gone+" is missing") || !strings.Contains(r.Hint, "move both ca.pem and ca.key") {
				t.Fatalf("ca = %+v, want a report-only problem (D4)", r)
			}
			assertChangedOnly(t, before, ti.snapshot())
		})
	}
}

func TestCAThatDoesNotLoadIsReportOnly(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.WriteFile(filepath.Join(ti.home, "ca", "ca.pem"), []byte("not a certificate\n"), 0o644))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "ca")
	if r.Status != StatusProblem || !strings.Contains(r.Detail, "does not load") || !strings.Contains(r.Hint, "move both") {
		t.Fatalf("ca = %+v, want a report-only problem", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestBinBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	breakThenRepair(t, ti, InstallChecks(), "bin", func() {
		must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	}, "home:bin/claude")
}

// TestBinPointingElsewhereIsAProblem is I3: a bin/chottag that resolves to
// a DIFFERENT, working executable is some other installed chottag's own
// link — doctor must never silently repoint it (a `go run` temp binary
// running doctor would otherwise steal it, leaving it dangling the moment
// that temp binary is gone). It is report-only, with a hint naming the fix
// the user actually wants (run setup from the binary that should own it),
// and --fix changes nothing.
func TestBinPointingElsewhereIsAProblem(t *testing.T) {
	ti := newTestInstall(t)
	link := filepath.Join(ti.home, "bin", "chottag")
	must(t, os.Remove(link))
	must(t, os.Symlink(writeExecutable(t, t.TempDir(), "chottag"), link))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "bin")
	if r.Status != StatusProblem || !strings.Contains(r.Detail, "bin/chottag does not point at "+ti.exe) {
		t.Fatalf("bin = %+v", r)
	}
	if !strings.Contains(r.Hint, "chottag setup") {
		t.Fatalf("bin.Hint = %q, want a hint to run chottag setup from the wanted binary, not %q", r.Hint, FixHint)
	}
	r = rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "bin")
	if r.Status != StatusProblem {
		t.Fatalf("bin --fix = %+v, want it left as a problem, never silently repointed", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestBinRefusesToFixFromATemporaryBinary is I3's other half: chottag
// running doctor from a `go run` temp binary (or anything else under
// TempDir) must never provision bin/ links at ITSELF — they would dangle
// the moment that temp binary is cleaned up. Missing/dangling links stay a
// problem, report-only, rather than silently fixed from a binary that is
// about to disappear.
func TestBinRefusesToFixFromATemporaryBinary(t *testing.T) {
	ti := newTestInstall(t)
	tmp := t.TempDir()
	ti.env.TempDirs = []string{tmp}
	ti.env.Executable = writeExecutable(t, tmp, "chottag")
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "bin")
	if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag setup") {
		t.Fatalf("bin = %+v, want a report-only problem", r)
	}
	r = rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "bin")
	if r.Status != StatusProblem {
		t.Fatalf("bin --fix = %+v, want it left as a problem: doctor must never link bin/ to a temporary binary", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestBinRefusesToFixWhenExecutableIsUnderAnyConfiguredTempDir is F176:
// isUnderTempDir must check every entry in TempDirs, not just the first —
// production wires os.TempDir(), /tmp and /private/tmp, since the old
// single os.TempDir() guard missed a scratch binary the user actually hit
// under /private/tmp on macOS. tempStandIn here plays that second,
// non-os.TempDir() entry's role.
func TestBinRefusesToFixWhenExecutableIsUnderAnyConfiguredTempDir(t *testing.T) {
	ti := newTestInstall(t)
	unrelated, tempStandIn := t.TempDir(), t.TempDir()
	ti.env.TempDirs = []string{unrelated, tempStandIn}
	ti.env.Executable = writeExecutable(t, tempStandIn, "chottag")
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "bin")
	if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag setup") {
		t.Fatalf("bin --fix = %+v, want a report-only problem: the executable sits under the SECOND configured temp dir", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestBinRefusesToFixFromAGoBuildDir is F176: `go run` and `go test` put
// their binaries under a "go-build..." directory that, on some platforms,
// does not sit under any of the configured TempDirs — so the refusal also
// looks for that path element directly, once TempDirs is non-empty (the
// same gate internal/cli's TestMain uses to disable the whole refusal for
// its own go-build-housed test binary).
func TestBinRefusesToFixFromAGoBuildDir(t *testing.T) {
	ti := newTestInstall(t)
	ti.env.TempDirs = []string{t.TempDir()} // opens the gate; unrelated to the go-build dir below
	goBuildDir := filepath.Join(t.TempDir(), "go-build1234567890", "b001", "exe")
	must(t, os.MkdirAll(goBuildDir, 0o700))
	ti.env.Executable = writeExecutable(t, goBuildDir, "chottag")
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "bin")
	if r.Status != StatusProblem || !strings.Contains(r.Hint, "chottag setup") {
		t.Fatalf("bin --fix = %+v, want a report-only problem: the executable is under a go-build temp dir", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestBinAcceptsTheExecutableSittingInBin(t *testing.T) {
	ti := newTestInstall(t)
	binDir := filepath.Join(ti.home, "bin")
	must(t, os.Remove(filepath.Join(binDir, "chottag")))
	must(t, os.Remove(filepath.Join(binDir, "claude")))
	ti.env.Executable = writeExecutable(t, binDir, "chottag")
	must(t, os.Symlink(ti.env.Executable, filepath.Join(binDir, "claude")))
	if r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "bin"); r.Status != StatusOK {
		t.Fatalf("bin = %+v, want ok when bin/chottag is the running binary itself", r)
	}
}

func TestRCBlockBreakThenRepair(t *testing.T) {
	ti := newTestInstall(t)
	breakThenRepair(t, ti, InstallChecks(), "rc-block", func() {
		must(t, os.WriteFile(ti.rc, []byte("# the user's own line\n"), 0o644))
	}, "user:.zshrc")
}

func TestRCBlockMissingRCFileIsFixable(t *testing.T) {
	ti := newTestInstall(t)
	breakThenRepair(t, ti, InstallChecks(), "rc-block", func() { must(t, os.Remove(ti.rc)) }, "user:.zshrc")
}

func TestRCBlockDetailTellsTheUserToOpenANewShell(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.WriteFile(ti.rc, []byte("# mine\n"), 0o644))
	if r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "rc-block"); r.Status != StatusFixed || !strings.Contains(r.Detail, "open a new shell") {
		t.Fatalf("rc-block = %+v, want fixed with the new-shell note", r)
	}
}

func TestRCBlockReadsThroughASymlinkedRC(t *testing.T) {
	ti := newTestInstall(t)
	target := filepath.Join(ti.userHome, "dotfiles", "zshrc")
	must(t, os.MkdirAll(filepath.Dir(target), 0o700))
	must(t, os.Rename(ti.rc, target))
	must(t, os.Symlink(target, ti.rc))
	if r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "rc-block"); r.Status != StatusOK {
		t.Fatalf("rc-block through a symlink = %+v, want ok", r)
	}
	must(t, os.WriteFile(target, []byte("# mine\n"), 0o644))
	before := ti.snapshot()
	if r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "rc-block"); r.Status != StatusProblem {
		t.Fatalf("rc-block = %+v, want a problem once the target lacks the block", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestRCBlockUnknownShellIsInfoWithTheExportLine(t *testing.T) {
	ti := newTestInstall(t)
	ti.env.Shell = "/opt/homebrew/bin/fish"
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "rc-block")
	want := `export PATH="` + ti.env.BinDir() + `:$PATH"`
	if r.Status != StatusInfo || !strings.Contains(r.Detail, want) || !strings.Contains(r.Detail, `"/opt/homebrew/bin/fish"`) {
		t.Fatalf("rc-block = %+v, want info carrying %s", r, want)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestRCBlockUnknownShellCarriesEveryExportLine pins fix round 2's N1:
// when the block holds more than one export line (CHOTTAG_HOME ahead of
// PATH, cli.rcBlock's shape once CHOTTAG_HOME is set), the unrecognised-
// shell hint must carry ALL of them, not just the first — dropping PATH
// silently used to leave a user's hand-typed fix broken.
func TestRCBlockUnknownShellCarriesEveryExportLine(t *testing.T) {
	ti := newTestInstall(t)
	ti.env.Shell = "/opt/homebrew/bin/fish"
	ti.env.RCBlock = func(binDir string) string {
		return "# >>> chottag >>>\nexport CHOTTAG_HOME=\"/x/chottag-home\"\nexport PATH=\"" + binDir + ":$PATH\"\n# <<< chottag <<<\n"
	}
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), true), "rc-block")
	wantHome := `export CHOTTAG_HOME="/x/chottag-home"`
	wantPath := `export PATH="` + ti.env.BinDir() + `:$PATH"`
	if r.Status != StatusInfo || !strings.Contains(r.Detail, wantHome) || !strings.Contains(r.Detail, wantPath) {
		t.Fatalf("rc-block = %+v, want info carrying both %q and %q", r, wantHome, wantPath)
	}
}

func TestRCBlockForAnotherBinDirIsAProblem(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.WriteFile(ti.rc, []byte(testRCBlock("/old/chottag/bin")), 0o644))
	if r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "rc-block"); r.Status != StatusProblem || r.Hint != FixHint {
		t.Fatalf("rc-block = %+v, want a fixable problem", r)
	}
}

func TestPathIsNeverAProblem(t *testing.T) {
	ti := newTestInstall(t)
	claudeDir := filepath.Dir(ti.claude)
	sep := string(os.PathListSeparator)
	for _, tc := range []struct{ path, status, detail string }{
		{ti.env.BinDir() + sep + claudeDir, StatusOK, "comes first"},
		{claudeDir + sep + ti.env.BinDir(), StatusInfo, "finds claude in " + claudeDir},
		{"", StatusInfo, "no claude"},
	} {
		ti.env.PATH = tc.path
		r := rowByID(t, mustRun(t, ti.env, []Check{pathCheck()}, true), "path")
		if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) {
			t.Errorf("PATH %q: %+v, want %s containing %q", tc.path, r, tc.status, tc.detail)
		}
	}
}

// TestPathOnAnUninstalledShimPointsAtSetup is F176's third row (M2b T4).
// With neither bin link and no rc block, "a new shell picks up the rc
// block" is false: there is no block. The path row says what bin and
// rc-block say, with or without --fix, whatever this process's PATH holds.
func TestPathOnAnUninstalledShimPointsAtSetup(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "bin", "chottag")))
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	must(t, os.WriteFile(ti.rc, []byte("# the user's own line\n"), 0o644))
	for _, path := range []string{filepath.Dir(ti.claude), ""} {
		ti.env.PATH = path
		for _, fix := range []bool{false, true} {
			r := rowByID(t, mustRun(t, ti.env, []Check{pathCheck()}, fix), "path")
			if r.Status != StatusInfo || r.Detail != shimNotInstalledDetail || r.Hint != shimNotInstalledHint {
				t.Errorf("PATH %q fix=%v: %+v, want info %q with hint %q", path, fix, r, shimNotInstalledDetail, shimNotInstalledHint)
			}
		}
	}
}

// TestPathOnAPartialInstallKeepsTheNewShellHint: a partial install (one
// bin link gone, the rc block still there) is not "not installed", so the
// row keeps its old detail.
func TestPathOnAPartialInstallKeepsTheNewShellHint(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	ti.env.PATH = filepath.Dir(ti.claude)
	r := rowByID(t, mustRun(t, ti.env, []Check{pathCheck()}, false), "path")
	if r.Status != StatusInfo || !strings.Contains(r.Detail, "a new shell picks up the rc block") || r.Hint != "" {
		t.Fatalf("path = %+v, want the new-shell info row", r)
	}
}

func TestInstallDetectWritesNothingOnABrokenInstall(t *testing.T) {
	ti := newTestInstall(t)
	// I2: a running daemon makes the CA check report-only instead of
	// fixable; this test is about the OTHER checks' no-side-effects
	// behaviour, so keep ca fixable exactly as before by running no daemon.
	ti.running = false
	must(t, os.Remove(filepath.Join(ti.home, "cache")))
	must(t, os.Remove(filepath.Join(ti.home, "ca", "ca.pem")))
	must(t, os.Remove(filepath.Join(ti.home, "ca", "ca.key")))
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	must(t, os.WriteFile(ti.rc, []byte("# mine\n"), 0o644))
	before := ti.snapshot()
	rows := mustRun(t, ti.env, InstallChecks(), false)
	for _, id := range []string{"tree", "ca", "bin", "rc-block"} {
		if r := rowByID(t, rows, id); r.Status != StatusProblem || r.Hint != FixHint {
			t.Errorf("%+v, want a fixable problem", r)
		}
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestUninstalledShimIsInfoNotAProblem is F176: `chottag uninstall` (no
// --purge) removes both bin/ links and the rc block but leaves
// state.json, so the setup check still reports ok. Neither bin nor
// rc-block may treat that as a repairable problem: doctor never performs
// a first install (D3), and --fix would otherwise recreate the shim
// pointed at whatever binary happens to be running doctor. Both rows are
// info, hinting chottag setup, and --fix (D3) changes nothing.
func TestUninstalledShimIsInfoNotAProblem(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "bin", "chottag")))
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	must(t, os.WriteFile(ti.rc, []byte("# the user's own line\n"), 0o644))
	before := ti.snapshot()

	rows := mustRun(t, ti.env, InstallChecks(), false)
	for _, id := range []string{"bin", "rc-block"} {
		if r := rowByID(t, rows, id); r.Status != StatusInfo || r.Hint != "chottag setup" {
			t.Fatalf("%s = %+v, want info hinting chottag setup (F176)", id, r)
		}
	}
	assertChangedOnly(t, before, ti.snapshot())

	rows = mustRun(t, ti.env, InstallChecks(), true)
	for _, id := range []string{"bin", "rc-block"} {
		if r := rowByID(t, rows, id); r.Status != StatusInfo {
			t.Fatalf("%s --fix = %+v, want it left info: doctor never performs a first install (D3)", id, r)
		}
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestUninstalledShimWithAnUnrecognisedShellIsStillInfo covers rc-block's
// other "no block" shape (row 5's own unrecognised-$SHELL branch): with
// both bin links also gone, it must still read as "not installed", not as
// the unrecognised-shell notice it gives on an otherwise-intact install.
func TestUninstalledShimWithAnUnrecognisedShellIsStillInfo(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "bin", "chottag")))
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	ti.env.Shell = "/opt/homebrew/bin/fish"
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "rc-block")
	if r.Status != StatusInfo || r.Hint != "chottag setup" {
		t.Fatalf("rc-block = %+v, want info hinting chottag setup even with an unrecognised $SHELL", r)
	}
}

// TestPartialUninstallOnlyOneBinLinkMissingStaysFixable is F176: the
// "shim not installed" judgement requires BOTH bin links to be gone. One
// missing link — even with no rc block either — is still a real,
// repairable problem, not "never installed".
func TestPartialUninstallOnlyOneBinLinkMissingStaysFixable(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.Remove(filepath.Join(ti.home, "bin", "claude")))
	must(t, os.WriteFile(ti.rc, []byte("# the user's own line\n"), 0o644)) // no block either
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "bin")
	if r.Status != StatusProblem || r.Hint != FixHint {
		t.Fatalf("bin = %+v, want a fixable problem: bin/chottag still points at the running binary", r)
	}
}

// TestPartialUninstallLinksPresentButBlockMissingStaysFixable is F176's
// other partial shape: both bin links intact, only the rc block gone.
// That is still a real, repairable problem.
func TestPartialUninstallLinksPresentButBlockMissingStaysFixable(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.WriteFile(ti.rc, []byte("# the user's own line\n"), 0o644))
	r := rowByID(t, mustRun(t, ti.env, InstallChecks(), false), "rc-block")
	if r.Status != StatusProblem || r.Hint != FixHint {
		t.Fatalf("rc-block = %+v, want a fixable problem: both bin links are intact", r)
	}
}
