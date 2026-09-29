package installsh

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReleaseInstallRunsTheRealSetup(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	if !strings.Contains(r.stdout, "chottag "+relVersion+"\n") {
		t.Errorf("stdout lacks `chottag version`'s line:\n%s", r)
	}
	for _, want := range []string{
		"gh auth status",
		"gh release view --repo " + repo + " --json tagName --jq .tagName",
		"gh release download --repo " + repo + " --pattern " + assetName(relVersion) + " --pattern checksums.txt --dir ",
		"shasum -a 256 ",
	} {
		if !s.called(want) {
			t.Errorf("no call starting %q in:\n%s", want, strings.Join(s.calls(), "\n"))
		}
	}
	// Never ~/.claude*: setup's rc block is the only thing in HOME.
	if got := entries(t, s.home); !reflect.DeepEqual(got, []string{".zshrc"}) {
		t.Errorf("HOME holds %v, want only .zshrc", got)
	}
	if got := entries(t, s.tmp); len(got) != 0 {
		t.Errorf("TMPDIR holds %v after a successful run", got)
	}
	if fi, err := os.Stat(s.chHome); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o700 {
		t.Errorf("the chottag home's mode = %v, want 0700 like setup's own MkdirAll", fi.Mode())
	}
}

func TestDefaultHomeIsDotChottagUnderHOME(t *testing.T) {
	s := newSandbox(t)
	s.chHome = "" // CHOTTAG_HOME unset
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	if r := s.install(); r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(filepath.Join(s.home, ".chottag"), relVersion)
	if got := entries(t, s.home); !reflect.DeepEqual(got, []string{".chottag", ".zshrc"}) {
		t.Errorf("HOME holds %v, want [.chottag .zshrc]", got)
	}
}

func TestVersionFlagInstallsThatReleaseWithoutAskingForTheLatest(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false) // FAKE_GH_LATEST stays unset: `release view` would fail

	if r := s.install("--version", relTag); r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	if s.called("gh release view") {
		t.Error("--version still asked gh for the latest release")
	}
}

// TestRepoFlagNamesEveryGhCall pins part 0 T2: --repo OWNER/NAME makes
// every gh call name that repo instead of install.sh's default REPO.
func TestRepoFlagNamesEveryGhCall(t *testing.T) {
	s := newSandbox(t)
	other := "alice/c-hottag-fork"
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	r := s.install("--repo", other)
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	for _, want := range []string{
		"gh release view --repo " + other,
		"gh release download --repo " + other,
	} {
		if !s.called(want) {
			t.Errorf("no call starting %q in:\n%s", want, strings.Join(s.calls(), "\n"))
		}
	}
	if s.called("--repo " + repo) {
		t.Error("a call still named the default repo")
	}
}

// TestNoRepoFlagUsesTheDefaultRepo pins the other half: with no --repo,
// every gh call still names install.sh's default REPO.
func TestNoRepoFlagUsesTheDefaultRepo(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	for _, want := range []string{
		"gh release view --repo " + repo,
		"gh release download --repo " + repo,
	} {
		if !s.called(want) {
			t.Errorf("no call starting %q in:\n%s", want, strings.Join(s.calls(), "\n"))
		}
	}
}

// TestRepoWithVersionDownloadsFromThatRepo pins --repo combined with
// --version: the download call names the given repo, not the default.
func TestRepoWithVersionDownloadsFromThatRepo(t *testing.T) {
	s := newSandbox(t)
	other := "alice/c-hottag-fork"
	s.release(relTag, false) // FAKE_GH_LATEST stays unset: --version must not ask gh for the latest

	r := s.install("--repo", other, "--version", relTag)
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	if !s.called("gh release download --repo " + other) {
		t.Errorf("no call downloading from %q in:\n%s", other, strings.Join(s.calls(), "\n"))
	}
}

// TestInstallJSONAfterReleaseInstall pins the install.json record a
// release install writes: exact fields, mode 0600, an RFC3339
// installedAt, and no leftover temp file (the atomic-write guarantee).
func TestInstallJSONAfterReleaseInstall(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	if r := s.install(); r.code != 0 {
		t.Fatal(r)
	}
	rec := s.readInstallRecord()
	if rec.Repo != repo || rec.Version != relVersion || rec.Source != "release" {
		t.Errorf("install.json = %+v, want repo %q version %q source \"release\"", rec, repo, relVersion)
	}
}

// TestInstallJSONNamesTheGivenRepo pins that a non-default --repo is what
// lands in install.json, not install.sh's REPO default.
func TestInstallJSONNamesTheGivenRepo(t *testing.T) {
	s := newSandbox(t)
	other := "alice/c-hottag-fork"
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	if r := s.install("--repo", other); r.code != 0 {
		t.Fatal(r)
	}
	rec := s.readInstallRecord()
	if rec.Repo != other {
		t.Errorf("install.json repo = %q, want %q", rec.Repo, other)
	}
}

// TestCloneBuildWritesSourceBuildAndGivenRepo pins a clone build's
// install.json: source "build", and --repo recorded as given even though
// a clone build never talks to that repo (spec §2.2).
func TestCloneBuildWritesSourceBuildAndGivenRepo(t *testing.T) {
	s := newSandbox(t)
	dir := s.clone()
	s.env["FAKE_GH_AUTH"] = "no"
	other := "alice/c-hottag-fork"

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"), "--repo", other)
	if r.code != 0 {
		t.Fatal(r)
	}
	rec := s.readInstallRecord()
	if rec.Repo != other || rec.Source != "build" {
		t.Errorf("install.json = %+v, want repo %q source \"build\"", rec, other)
	}
}

// TestChecksumMismatchWritesNoInstallJSON pins that a failed install
// (checksum mismatch) writes no install.json, on top of
// TestChecksumMismatchInstallsNothing's broader assertNothingInstalled.
func TestChecksumMismatchWritesNoInstallJSON(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, true)
	s.env["FAKE_GH_LATEST"] = relTag

	r := s.install()
	if r.code != 1 || !strings.Contains(r.stderr, "checksum mismatch") {
		t.Fatalf("want exit 1 naming a checksum mismatch\n%s", r)
	}
	if _, err := os.Lstat(s.installRecordPath()); !os.IsNotExist(err) {
		t.Errorf("install.json exists after a failed install (err %v)", err)
	}
	s.assertNothingInstalled()
}

func TestUpgradeKeepsTheOldVersionAndRepointsTheLinks(t *testing.T) {
	s := newSandbox(t)
	s.release("v1.2.3", false)
	if r := s.install("--version", "v1.2.3"); r.code != 0 {
		t.Fatal(r)
	}
	s.release("v1.2.4", false)
	if r := s.install("--version", "v1.2.4"); r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, "1.2.4") // links moved, one rc block
	if _, err := os.Stat(filepath.Join(s.chHome, "versions", "1.2.3", "chottag")); err != nil {
		t.Errorf("the old version was removed: %v (N4 keeps it)", err)
	}
	// The same version again: still one rc block, links unchanged.
	if r := s.install("--version", "v1.2.4"); r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, "1.2.4")
	if got := entries(t, filepath.Join(s.chHome, "versions", "1.2.4")); !reflect.DeepEqual(got, []string{"chottag"}) {
		t.Errorf("versions/1.2.4 holds %v, want only chottag (no chottag.new left behind)", got)
	}
}

func TestChecksumMismatchInstallsNothing(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, true)
	s.env["FAKE_GH_LATEST"] = relTag
	dir := s.clone() // run from a clone, so a Go build is possible: a mismatch must still never fall back to it

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"))
	if r.code != 1 || !strings.Contains(r.stderr, "checksum mismatch") {
		t.Fatalf("want exit 1 naming a checksum mismatch\n%s", r)
	}
	s.assertNothingInstalled()
	if s.called("go ") {
		t.Error("a checksum mismatch fell back to a Go build")
	}
}

func TestChecksumsWithoutTheAssetInstallsNothing(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	other := strings.Repeat("2", 64) + "  chottag_" + relVersion + "_plan9_mips.tar.gz\n"
	if err := os.WriteFile(filepath.Join(s.assets, relTag, "checksums.txt"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}

	r := s.install()
	if r.code != 1 || !strings.Contains(r.stderr, "has no line for "+assetName(relVersion)) {
		t.Fatalf("want exit 1 naming the missing checksum line\n%s", r)
	}
	s.assertNothingInstalled()
}

// TestSha256sumBranchVerifiesTheDownload pins the sha256_of fallback (item
// 7): when shasum is not on PATH, install.sh must reach for sha256sum
// instead of failing, and still catch a checksum mismatch through it.
func TestSha256sumBranchVerifiesTheDownload(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.dropFake("shasum")
	s.addFake("sha256sum", fakeSha256sum)

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	if !s.called("sha256sum ") {
		t.Error("install.sh never called sha256sum")
	}
}

// TestSha256sumBranchCatchesAMismatch: the fallback tool must be able to
// fail the install too, not just succeed silently.
func TestSha256sumBranchCatchesAMismatch(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, true) // checksums.txt lists the wrong sum
	s.env["FAKE_GH_LATEST"] = relTag
	s.dropFake("shasum")
	s.addFake("sha256sum", fakeSha256sum)

	r := s.install()
	if r.code != 1 || !strings.Contains(r.stderr, "checksum mismatch") {
		t.Fatalf("want exit 1 naming a checksum mismatch\n%s", r)
	}
	s.assertNothingInstalled()
}

// TestTarballWithoutChottagInstallsNothing pins item 7: a release tarball
// that holds no chottag member (a corrupted upload, a mismatched archive)
// must fail the install rather than leave a broken or missing binary
// silently in place.
func TestTarballWithoutChottagInstallsNothing(t *testing.T) {
	s := newSandbox(t)
	s.releaseWithoutBinary(relTag)
	s.env["FAKE_GH_LATEST"] = relTag

	r := s.install()
	// GNU and BSD tar both fail extracting a named member that is not in
	// the archive, so this hits try_release's "could not unpack" die, not
	// the later "holds no chottag binary" check — either message says
	// nothing was installed, which is the guarantee this test actually
	// pins.
	if r.code != 1 || !strings.Contains(r.stderr, "nothing was installed") {
		t.Fatalf("want exit 1 saying nothing was installed\n%s", r)
	}
	s.assertNothingInstalled()
}

func TestNoChecksumToolInstallsNothing(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.dropFake("shasum") // and sha256sum is never on PATH

	r := s.install()
	if r.code != 1 || !strings.Contains(r.stderr, "neither shasum nor sha256sum") {
		t.Fatalf("want exit 1 naming the missing checksum tool\n%s", r)
	}
	s.assertNothingInstalled()
}

func TestUnsupportedPlatformInstallsNothing(t *testing.T) {
	for _, c := range []struct {
		name, key, value, want string
	}{
		{"os", "FAKE_UNAME_S", "FreeBSD", "unsupported OS: FreeBSD"},
		{"cpu", "FAKE_UNAME_M", "i686", "unsupported CPU: i686"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSandbox(t)
			s.release(relTag, false)
			s.env["FAKE_GH_LATEST"] = relTag
			s.env[c.key] = c.value

			r := s.install()
			if r.code != 1 || !strings.Contains(r.stderr, c.want) {
				t.Fatalf("want exit 1 with %q\n%s", c.want, r)
			}
			s.assertNothingInstalled()
			if s.called("gh ") {
				t.Error("gh was called for an unsupported platform")
			}
		})
	}
}

// TestUnameArchAliasesMapToTheSameAsset pins install.sh's uname alias
// table (item 7): x86_64 and amd64 must both resolve to the amd64 asset,
// aarch64 and arm64 to arm64 — regardless which spelling this uname
// reports. It runs the host's own architecture under uname's OTHER
// spelling, so the release published for the host's real (runtime.GOARCH)
// asset is still the one requested and found.
func TestUnameArchAliasesMapToTheSameAsset(t *testing.T) {
	_, hostArch := hostUname(t)
	alt := map[string]string{"x86_64": "amd64", "amd64": "x86_64", "arm64": "aarch64", "aarch64": "arm64"}[hostArch]
	if alt == "" {
		t.Fatalf("no known alias for host uname -m %q", hostArch)
	}

	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_UNAME_M"] = alt

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
}

// TestReleaseDownloadFailureFallsBackToBuildWithANote pins item 6 and the
// rest of item 7: when no --version was given, gh sees a release but its
// download fails (here: never published), install.sh silently used to
// fall back to a Go build with no sign anything went wrong. It must now
// print `note: release not used: <reason>`, with gh's own captured stderr
// (item 3) inside that reason.
func TestReleaseDownloadFailureFallsBackToBuildWithANote(t *testing.T) {
	s := newSandbox(t)
	dir := s.clone()
	s.env["FAKE_GH_LATEST"] = relTag // gh sees a release...
	// ...but its assets were never published (no s.release call), so
	// `gh release download` fails and try_build must run instead.

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"))
	if r.code != 0 {
		t.Fatal(r)
	}
	const ver = "0.3.0-2-gabc1234" // FAKE_GIT_DESCRIBE without its "v"
	s.assertInstalled(s.chHome, ver)
	if !strings.Contains(r.stdout, "note: release not used: could not download") {
		t.Errorf("stdout lacks the fallback note:\n%s", r)
	}
	if !strings.Contains(r.stdout, "release not found") {
		t.Errorf("stdout lacks gh's own captured stderr reason:\n%s", r)
	}
}

func TestNoGhAndNotACloneSaysWhatIsMissing(t *testing.T) {
	s := newSandbox(t)
	s.dropFake("gh")

	r := s.install()
	if r.code != 1 {
		t.Fatal(r)
	}
	for _, want := range []string{"gh is not installed", "not running from a clone", "Go"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q\n%s", want, r)
		}
	}
	s.assertNothingInstalled()
}

func TestGhNotLoggedInBuildsFromTheClone(t *testing.T) {
	s := newSandbox(t)
	dir := s.clone()
	s.env["FAKE_GH_AUTH"] = "no"

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"))
	if r.code != 0 {
		t.Fatal(r)
	}
	const ver = "0.3.0-2-gabc1234" // FAKE_GIT_DESCRIBE without its "v"
	s.assertInstalled(s.chHome, ver)

	var args []string
	pwd, env := "", ""
	for _, c := range s.calls() {
		switch {
		case strings.HasPrefix(c, "go.arg "):
			args = append(args, strings.TrimPrefix(c, "go.arg "))
		case strings.HasPrefix(c, "go.pwd "):
			pwd = strings.TrimPrefix(c, "go.pwd ")
		case strings.HasPrefix(c, "go.env "):
			env = strings.TrimPrefix(c, "go.env ")
		}
	}
	if len(args) != 7 {
		t.Fatalf("go args = %q, want 7", args)
	}
	out := args[5]
	args[5] = "OUT"
	want := []string{"build", "-trimpath", "-ldflags", "-X github.com/HaiNNT/c-hottag/internal/cli.Version=" + ver, "-o", "OUT", "./cmd/chottag"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("go args = %q, want %q", args, want)
	}
	// The script's temp dir is gone by now, so compare strings: mktemp
	// makes it as "$TMPDIR/chottag-install.XXXXXX".
	if !strings.HasPrefix(out, s.tmp+string(os.PathSeparator)+"chottag-install.") {
		t.Errorf("go build -o %s is not under TMPDIR: the build goes to a temp dir, then is moved", out)
	}
	if resolve(t, pwd) != resolve(t, dir) {
		t.Errorf("go ran in %s, want the clone %s", pwd, dir)
	}
	if env != "CGO_ENABLED=0 GOFLAGS=" {
		t.Errorf("go env = %q, want CGO_ENABLED=0 and an empty GOFLAGS", env)
	}
	if s.called("gh release download") {
		t.Error("a logged-out gh still tried to download")
	}
}

func TestCloneWithoutGoSaysGoIsMissing(t *testing.T) {
	s := newSandbox(t)
	dir := s.clone()
	s.dropFake("go")
	s.env["FAKE_GH_AUTH"] = "no"

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"))
	if r.code != 1 || !strings.Contains(r.stderr, "go is not installed") || !strings.Contains(r.stderr, "gh is not logged in") {
		t.Fatalf("want exit 1 naming both missing pieces\n%s", r)
	}
	s.assertNothingInstalled()
}

func TestVersionFlagNeverFallsBackToABuild(t *testing.T) {
	s := newSandbox(t)
	dir := s.clone() // gh is logged in, but v9.9.9 has no release

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"), "--version", "v9.9.9")
	if r.code != 1 || !strings.Contains(r.stderr, "needs that release") {
		t.Fatalf("want exit 1 saying --version needs a release\n%s", r)
	}
	if s.called("go ") {
		t.Error("--version fell back to building the clone's checkout")
	}
	s.assertNothingInstalled()
}

func TestBadArgumentsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--bogus"},
		{"--version"},
		{"--version", "latest"},
		{"--version", "v1/../../x"},
		{"--version=v1 2"},
		// Fix round item 1: an empty --version= used to fall through and
		// silently run a full release-or-clone-build install instead of
		// erroring.
		{"--version="},
		// Fix round item 2: a value that could be mistaken for a flag.
		{"--version", "-x"},
		// Part 0 T2: --repo validation.
		{"--repo", "bad"},
		{"--repo", "a/b/c"},
		{"--repo", "a/b;x"},
		{"--repo"},
		{"--repo="},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			s := newSandbox(t)
			r := s.install(args...)
			if r.code != 2 || !strings.Contains(r.stderr, "usage: install.sh") {
				t.Fatalf("want exit 2 with the usage line\n%s", r)
			}
			if s.called("gh ") || s.called("uname ") {
				t.Error("a bad argument got past parsing")
			}
			s.assertNothingInstalled()
		})
	}
}

func TestPipedScriptInstallsFromARelease(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag

	r := s.pipe(s.root)
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	if !strings.Contains(r.stdout, "chottag "+relVersion+"\n") {
		t.Errorf("the piped script did not run to its end:\n%s", r)
	}
}

func TestPipedScriptNeverTreatsTheWorkingDirAsAClone(t *testing.T) {
	s := newSandbox(t)
	dir := s.clone()
	s.env["FAKE_GH_AUTH"] = "no"

	r := s.pipe(dir) // cwd is a clone, but $0 is the shell
	if r.code != 1 || !strings.Contains(r.stderr, "not running from a clone") {
		t.Fatalf("want exit 1: a piped script is never a clone\n%s", r)
	}
	if s.called("go ") {
		t.Error("the piped script built the working directory")
	}
	s.assertNothingInstalled()
}

func TestRunsUnderDash(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash not installed (on Linux CI /bin/sh is dash, so every test here runs under it)")
	}
	s := newSandbox(t)
	s.shell = dash
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	if r := s.install(); r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
}

func TestScriptIsAnExecutablePOSIXScript(t *testing.T) {
	prepare(t)
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "install.sh")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("install.sh mode = %v; the README runs ./install.sh", fi.Mode())
	}
	if !strings.HasPrefix(string(scriptBody), "#!/bin/sh\n") || !strings.Contains(string(scriptBody), "\nset -eu\n") {
		t.Error("install.sh must start with #!/bin/sh and run with set -eu")
	}
	if !strings.HasSuffix(string(scriptBody), "\nmain \"$@\"\n") {
		t.Error(`install.sh must end with main "$@", so a truncated download runs nothing`)
	}
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Log("shellcheck not installed; skipping the lint half (M3 spec §3: where a checker is available)")
		return
	}
	if out, err := exec.Command(sc, "-s", "sh", p).CombinedOutput(); err != nil {
		t.Errorf("shellcheck -s sh install.sh: %v\n%s", err, out)
	}
}
