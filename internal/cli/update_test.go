package cli

// Tests for `chottag update` (spec §2.3). Every fake here answers in place
// of updateGH/updateChild/updateProbe (TestMain installs panicking
// defaults for all three): no test in this file ever runs the real gh or
// claude, downloads anything from the network, or binds a real port.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

// withVersion sets the package's Version (normally stamped at build time
// with -ldflags) for the duration of a test, restoring it through
// t.Cleanup.
func withVersion(t *testing.T, v string) {
	t.Helper()
	orig := Version
	Version = v
	t.Cleanup(func() { Version = orig })
}

// updateHome makes a fresh CHOTTAG_HOME (t.Setenv) with a state.json
// naming a closed test port, and returns it.
func updateHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("CHOTTAG_HOME", h)
	writeStateWithPort(t, h, closedPort(t))
	return h
}

// tarEntry describes one entry buildTarGz writes.
type tarEntry struct {
	name     string
	content  []byte
	typeflag byte // defaults to tar.TypeReg
	linkname string
}

// buildTarGz builds a .tar.gz holding entries, in memory (archive/tar +
// compress/gzip, never a real fixture file: rulings, task-3 brief).
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		typ := e.typeflag
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: typ}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		} else if typ == tar.TypeSymlink {
			hdr.Linkname = e.linkname
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// releaseAssetName is the asset name `update` asks gh for (spec §2.3).
func releaseAssetName(ver string) string {
	return fmt.Sprintf("chottag_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
}

// releaseFixture is one release's downloadable content.
type releaseFixture struct {
	asset     []byte
	checksums []byte
}

// goodRelease builds a fixture holding a valid chottag binary "content" and
// a checksums.txt line naming it correctly, for release tag with version
// ver.
func goodRelease(t *testing.T, ver string, content []byte) releaseFixture {
	t.Helper()
	tgz := buildTarGz(t, []tarEntry{{name: "chottag", content: content}})
	sum := sha256Hex(tgz)
	asset := releaseAssetName(ver)
	checksums := []byte(sum + "  " + asset + "\n")
	return releaseFixture{asset: tgz, checksums: checksums}
}

// childCall is one recorded updateChild invocation.
type childCall struct {
	bin  string
	args []string
	env  []string
}

// fakeGH returns an updateGH-shaped func recording every call, and a
// pointer to the calls. authErr answers "auth status"; tag/tagErr answer
// "release view"; releases maps a release tag (e.g. "v0.3.1", matching the
// literal tag `release download ... -- TAG` is invoked with) to what
// "release download" writes into --dir. It deliberately answers nothing
// for "repo view" or "attestation verify" (fix round 1 item 4): a test
// whose release version crosses attestedSince must say so explicitly, by
// wrapping this func in attestGH (update_attest_test.go), rather than get
// a free "public and verified" answer it never asked for.
func fakeGH(t *testing.T, authErr error, tag string, tagErr error, releases map[string]releaseFixture) (func(context.Context, ...string) ([]byte, error), *[][]string) {
	t.Helper()
	calls := &[][]string{}
	fn := func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		switch {
		case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
			return nil, authErr
		case len(args) >= 2 && args[0] == "release" && args[1] == "view":
			if tagErr != nil {
				return nil, tagErr
			}
			return []byte(tag + "\n"), nil
		case len(args) >= 2 && args[0] == "release" && args[1] == "download":
			dir := ""
			for i, a := range args {
				if a == "--dir" && i+1 < len(args) {
					dir = args[i+1]
				}
			}
			relTag := args[len(args)-1]
			fx, ok := releases[relTag]
			if !ok {
				return nil, fmt.Errorf("fake gh: no release fixture for %s", relTag)
			}
			ver := strings.TrimPrefix(relTag, "v")
			if err := os.WriteFile(filepath.Join(dir, releaseAssetName(ver)), fx.asset, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), fx.checksums, 0o644); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}
		t.Fatalf("fake gh: unexpected args %v", args)
		return nil, nil
	}
	return fn, calls
}

// fakeChild returns an updateChild-shaped func recording every call. When
// linkOnSetup is true, a "setup" call symlinks CHOTTAG_HOME/bin/chottag to
// bin, the way the real `chottag setup` relinks it. restartErr is what a
// "daemon restart" call returns.
func fakeChild(t *testing.T, home string, linkOnSetup bool, restartErr error) (func(context.Context, string, ...string) error, *[]childCall) {
	t.Helper()
	calls := &[]childCall{}
	fn := func(_ context.Context, bin string, args ...string) error {
		*calls = append(*calls, childCall{bin: bin, args: append([]string(nil), args...), env: os.Environ()})
		switch {
		case len(args) == 1 && args[0] == "setup":
			if linkOnSetup {
				binDir := filepath.Join(home, "bin")
				if err := os.MkdirAll(binDir, 0o700); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(binDir, "chottag")
				os.Remove(link)
				if err := os.Symlink(bin, link); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		case len(args) == 2 && args[0] == "daemon" && args[1] == "restart":
			return restartErr
		}
		return nil
	}
	return fn, calls
}

// bareUpdateGH wraps base (a fakeGH) for a test that is not itself about
// attestation but reaches the download stage on a bare (non --version)
// update whose release predates attestedSince: fix round 2 item 1 makes
// that call `gh repo view` even then (a stale "latest" is suspicious
// whether or not Version itself parses — the R82 "never goes down" guard
// that used to make this unreachable can't be relied on once Version
// doesn't parse, e.g. a "dev" build). A private answer takes the
// note-and-continue path, so the test's own point is unaffected. Any
// `attestation` call Fatals the test (fix round 3 item 1): a release
// below the cut-over must never run attestation at all, and every test
// using this wraps exactly such a release.
func bareUpdateGH(t *testing.T, base func(context.Context, ...string) ([]byte, error)) func(context.Context, ...string) ([]byte, error) {
	t.Helper()
	gh, _ := attestGH(t, base, "true", "", nil, true, nil)
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "attestation" {
			t.Fatalf("bareUpdateGH: unexpected attestation call %v: a release below the cut-over must never run attestation", args)
		}
		return gh(ctx, args...)
	}
}

// stubUpdateSeams swaps updateGH, updateChild and updateProbe for the
// duration of the calling test, restoring TestMain's panicking defaults
// through t.Cleanup (F130).
func stubUpdateSeams(t *testing.T, gh func(context.Context, ...string) ([]byte, error), child func(context.Context, string, ...string) error, probe func(int) (bool, string)) {
	t.Helper()
	origGH, origChild, origProbe, origFetch := updateGH, updateChild, updateProbe, updateFetch
	updateGH, updateChild, updateProbe = gh, child, probe
	stubInstalled(t, "") // a test that cares overrides this after
	if gh != nil {
		// `update --check` asks updateFetch, not gh (R124). Answer it from the
		// same fake "release view" tag, so a test written against gh keeps its
		// meaning; a test about the fetch itself passes a nil gh and stubs
		// updateFetch with stubFetch.
		updateFetch = func(ctx context.Context, _ *url.URL, _ string) (updatecheck.Release, error) {
			out, err := gh(ctx, "release", "view")
			if err != nil {
				return updatecheck.Release{}, err
			}
			tag := strings.TrimSpace(string(out))
			return updatecheck.Release{Tag: tag, Version: strings.TrimPrefix(tag, "v"), PublishedAt: time.Now().Add(-72 * time.Hour)}, nil
		}
	}
	t.Cleanup(func() {
		updateGH, updateChild, updateProbe, updateFetch = origGH, origChild, origProbe, origFetch
	})
}

// stubInstalled makes installedVersion report v, or an unknown version for "".
func stubInstalled(t *testing.T, v string) {
	t.Helper()
	orig := installedVersion
	installedVersion = func(string) (string, error) {
		if v == "" {
			return "", errors.New("the installed version is unknown")
		}
		return v, nil
	}
	t.Cleanup(func() { installedVersion = orig })
}

func neverRunningProbe(int) (bool, string) { return false, "" }

func hasDownloadCall(calls [][]string) bool {
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "release" && c[1] == "download" {
			return true
		}
	}
	return false
}

// TestTopLevelUpdateHelpListsRepoFlag is part 0 final review fix 4:
// cli.go's top-level usage line for "update" must list every flag
// updateUsage itself does, so `chottag help` (and `chottag update --help`,
// which is exactly commandUsage("update")) never omits --repo.
func TestTopLevelUpdateHelpListsRepoFlag(t *testing.T) {
	if !strings.Contains(commandUsage("update"), "[--repo OWNER/NAME]") {
		t.Errorf("commandUsage(\"update\") = %q, want it to list [--repo OWNER/NAME]", commandUsage("update"))
	}
}

// --- --check --------------------------------------------------------------

func TestUpdateCheckReportsAnAvailableUpdate(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, childCalls := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if hasDownloadCall(*ghCalls) {
		t.Errorf("gh calls = %v, want no download call", *ghCalls)
	}
	if len(*childCalls) != 0 {
		t.Errorf("child calls = %v, want none", *childCalls)
	}
}

func TestUpdateCheckJSONFields(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check"}, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	if doc["current"] != "0.3.0" || doc["latest"] != "0.3.1" || doc["updateAvailable"] != true {
		t.Errorf("doc = %v, want current 0.3.0, latest 0.3.1, updateAvailable true", doc)
	}
}

func TestUpdateCheckDevVersionAlwaysHasAnUpdateAvailable(t *testing.T) {
	withVersion(t, "dev")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check"}, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["updateAvailable"] != true {
		t.Errorf("doc = %v, want updateAvailable true for a dev build", doc)
	}
}

// --- already up to date ----------------------------------------------------

func TestUpdateAlreadyUpToDate(t *testing.T) {
	withVersion(t, "0.3.1")
	h := updateHome(t)
	if err := os.MkdirAll(filepath.Join(h, "versions", "0.3.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "versions", "0.3.1", "chottag"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("stdout = %q, want it to say already up to date", out.String())
	}
	if len(*childCalls) != 0 {
		t.Errorf("child calls = %v, want none", *childCalls)
	}
	if hasDownloadCall(*ghCalls) {
		t.Errorf("gh calls = %v, want no download call", *ghCalls)
	}
}

// TestUpdateBareUpdateNeverDowngrades is the R82 ruling (fix round 1 item
// 13): a bare `chottag update` (no --version) never installs a release
// that is not strictly newer than this binary — even if gh's own "latest"
// somehow resolves to something older (a yanked release, a mirror lag,
// anything). It reports up to date, installs nothing, and
// updateAvailable is false.
func TestUpdateBareUpdateNeverDowngrades(t *testing.T) {
	withVersion(t, "0.3.5")
	h := updateHome(t)
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil) // "latest" is OLDER than 0.3.5
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["updateAvailable"] != false || doc["installed"] != false {
		t.Errorf("doc = %v, want updateAvailable false and installed false", doc)
	}
	if hasDownloadCall(*ghCalls) {
		t.Errorf("gh calls = %v, want no download call: never downgrade without --version", *ghCalls)
	}
	if len(*childCalls) != 0 {
		t.Errorf("child calls = %v, want none", *childCalls)
	}
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1")); err == nil {
		t.Error("versions/0.3.1 exists, want nothing installed")
	}
}

// TestUpdateDevVersionAlwaysInstalls: a "dev" build never parses as a
// semver, so it is always treated as older than any real release (fix
// round 1 item 13).
func TestUpdateDevVersionAlwaysInstalls(t *testing.T) {
	withVersion(t, "dev")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1", "chottag")); err != nil {
		t.Errorf("versions/0.3.1/chottag: %v", err)
	}
	found := false
	for _, c := range *childCalls {
		if len(c.args) == 1 && c.args[0] == "setup" {
			found = true
		}
	}
	if !found {
		t.Errorf("child calls = %v, want a setup call", *childCalls)
	}
}

// --- install ---------------------------------------------------------------

func TestUpdateInstallsTheLatestRelease(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("#!/bin/sh\necho fake-chottag\n"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	bin := filepath.Join(h, "versions", "0.3.1", "chottag")
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("versions/0.3.1/chottag: %v", err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 0755", fi.Mode().Perm())
	}
	found := false
	for _, c := range *childCalls {
		if len(c.args) == 1 && c.args[0] == "setup" {
			found = true
			if c.bin != bin {
				t.Errorf("setup called with bin %q, want %q", c.bin, bin)
			}
		}
	}
	if !found {
		t.Errorf("child calls = %v, want a setup call", *childCalls)
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok {
		t.Fatalf("readInstallRecord: ok=%v err=%v", ok, err)
	}
	if rec.Repo != defaultRepo || rec.Version != "0.3.1" || rec.Source != "release" {
		t.Errorf("install.json = %+v, want repo %s version 0.3.1 source release", rec, defaultRepo)
	}
	if !strings.Contains(out.String(), "installed chottag 0.3.1 from release v0.3.1 ("+defaultRepo+")") {
		t.Errorf("stdout = %q, want the installed line", out.String())
	}
}

func TestUpdateVersionFlagInstallsAnOlderRelease(t *testing.T) {
	withVersion(t, "0.3.1")
	h := updateHome(t)
	fx := goodRelease(t, "0.2.9", []byte("old-binary"))
	gh, ghCalls := fakeGH(t, nil, "", nil, map[string]releaseFixture{"v0.2.9": fx})
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--version", "v0.2.9"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(h, "versions", "0.2.9", "chottag")); err != nil {
		t.Errorf("versions/0.2.9/chottag: %v", err)
	}
	// release view must never be called: --version names the tag outright.
	for _, c := range *ghCalls {
		if len(c) >= 2 && c[0] == "release" && c[1] == "view" {
			t.Errorf("gh calls = %v, want no release view call with --version", *ghCalls)
		}
	}
}

// TestUpdateDownloadArgvIsExact is fix round 1 item 14: the exact argv
// `release download --repo R --pattern <asset> --pattern checksums.txt
// --dir D -- vX`, not merely "some call that includes these".
func TestUpdateDownloadArgvIsExact(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, ghCalls := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var dl []string
	for _, c := range *ghCalls {
		if len(c) >= 2 && c[0] == "release" && c[1] == "download" {
			dl = c
		}
	}
	if dl == nil {
		t.Fatal("no release download call recorded")
	}
	asset := releaseAssetName("0.3.1")
	// The --dir value (a fresh os.MkdirTemp path) is the only field this
	// test cannot predict; lift it straight from the recorded call so the
	// rest of the argv is compared exactly.
	dirIdx := -1
	for i, a := range dl {
		if a == "--dir" {
			dirIdx = i + 1
		}
	}
	if dirIdx < 0 || dirIdx >= len(dl) {
		t.Fatalf("download argv = %v, has no --dir value", dl)
	}
	want := []string{"release", "download", "--repo", defaultRepo, "--pattern", asset, "--pattern", "checksums.txt", "--dir", dl[dirIdx], "--", "v0.3.1"}
	if !reflect.DeepEqual(dl, want) {
		t.Errorf("download argv = %v, want %v", dl, want)
	}
}

// --- checksum and tar rejection ---------------------------------------------

// assertUpdateFailedJSON runs runUpdate in JSON mode and asserts exit 1 and
// error.code == "update_failed" (fix round 1 item 3).
func assertUpdateFailedJSON(t *testing.T, args []string) map[string]any {
	t.Helper()
	var out, errb bytes.Buffer
	code := runUpdate(args, newReporter(true, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	errObj, _ := doc["error"].(map[string]any)
	if errObj == nil || errObj["code"] != string(codeUpdateFailed) {
		t.Errorf("doc = %v, want error.code %q", doc, codeUpdateFailed)
	}
	return doc
}

func TestUpdateChecksumMismatchInstallsNothing(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	fx.checksums = []byte(strings.Repeat("0", 64) + "  " + releaseAssetName("0.3.1") + "\n")
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	assertUpdateFailedJSON(t, nil)
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1")); err == nil {
		t.Error("versions/0.3.1 exists, want nothing installed after a checksum mismatch")
	}
	if len(*childCalls) != 0 {
		t.Errorf("child calls = %v, want none", *childCalls)
	}
}

func TestUpdateRejectsATraversalEntry(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	tgz := buildTarGz(t, []tarEntry{{name: "../chottag", content: []byte("evil")}})
	fx := releaseFixture{asset: tgz, checksums: []byte(sha256Hex(tgz) + "  " + releaseAssetName("0.3.1") + "\n")}
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	assertUpdateFailedJSON(t, nil)
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1")); err == nil {
		t.Error("versions/0.3.1 exists, want nothing placed for a ../chottag entry")
	}
}

func TestUpdateRejectsAnAbsolutePathEntry(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	tgz := buildTarGz(t, []tarEntry{{name: "/abs/chottag", content: []byte("evil")}})
	fx := releaseFixture{asset: tgz, checksums: []byte(sha256Hex(tgz) + "  " + releaseAssetName("0.3.1") + "\n")}
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	assertUpdateFailedJSON(t, nil)
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1")); err == nil {
		t.Error("versions/0.3.1 exists, want nothing placed for a /abs/chottag entry")
	}
}

func TestUpdateRejectsASymlinkEntry(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	tgz := buildTarGz(t, []tarEntry{{name: "chottag", typeflag: tar.TypeSymlink, linkname: "/bin/sh"}})
	fx := releaseFixture{asset: tgz, checksums: []byte(sha256Hex(tgz) + "  " + releaseAssetName("0.3.1") + "\n")}
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	assertUpdateFailedJSON(t, nil)
	if _, err := os.Stat(filepath.Join(h, "versions", "0.3.1")); err == nil {
		t.Error("versions/0.3.1 exists, want nothing placed for a symlink entry")
	}
}

// TestExtractChottagLeavesNoDebrisOnATruncatedGzip is fix round 1 item 5's
// own test: a genuinely corrupt (truncated) gzip stream must leave neither
// the scratch temp file nor destDir behind.
func TestExtractChottagLeavesNoDebrisOnATruncatedGzip(t *testing.T) {
	// Large, incompressible content: tar pads short entries and trailer
	// blocks to 512-byte boundaries, so a small truncation of a small
	// payload can land entirely in that padding and never actually corrupt
	// the entry's own compressed bytes. A few KB of pseudo-random content,
	// cut in half, unambiguously lands inside the compressed entry data
	// itself.
	content := make([]byte, 8192)
	for i := range content {
		content[i] = byte(i*2654435761 + 17)
	}
	full := buildTarGz(t, []tarEntry{{name: "chottag", content: content}})
	truncated := full[:len(full)/2]
	scratch := t.TempDir()
	tgzPath := filepath.Join(scratch, "asset.tar.gz")
	if err := os.WriteFile(tgzPath, truncated, 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(t.TempDir(), "versions", "0.3.1")
	destBin := filepath.Join(destDir, "chottag")

	if err := extractChottag(tgzPath, destDir, destBin); err == nil {
		t.Fatal("extractChottag(truncated gzip) = nil error, want one")
	}
	if _, err := os.Stat(destDir); err == nil {
		t.Error("destDir exists after a truncated gzip, want nothing created (this call would have created it)")
	}
}

// TestExtractChottagRejectsAnOversizedEntry is fix round 1 item 6. It
// lowers maxExtractedSize for the duration of the test rather than
// building a real 256 MiB archive.
func TestExtractChottagRejectsAnOversizedEntry(t *testing.T) {
	origMax := maxExtractedSize
	maxExtractedSize = 100
	t.Cleanup(func() { maxExtractedSize = origMax })

	scratch := t.TempDir()
	tgz := buildTarGz(t, []tarEntry{{name: "chottag", content: bytes.Repeat([]byte("x"), 200)}})
	tgzPath := filepath.Join(scratch, "asset.tar.gz")
	if err := os.WriteFile(tgzPath, tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(t.TempDir(), "versions", "0.3.1")
	destBin := filepath.Join(destDir, "chottag")

	if err := extractChottag(tgzPath, destDir, destBin); err == nil {
		t.Fatal("extractChottag(oversized entry) = nil error, want one")
	}
	if _, err := os.Stat(destDir); err == nil {
		t.Error("destDir exists after an oversized entry, want nothing created")
	}
}

// TestExtractChottagRenamesOnlyWithinDestDir is fix round 2 item B1: the
// final placement must be a rename FROM inside destDir (destDir/chottag.new)
// TO destBin, never from wherever the downloaded .tar.gz happens to sit —
// that shape is exactly a cross-device rename (EXDEV) whenever the
// download's own scratch dir (os.MkdirTemp("", ...), normally $TMPDIR)
// and CHOTTAG_HOME live on different filesystems, which is the ordinary
// case on a Linux box with a tmpfs /tmp. tgzPath and destDir are given as
// two entirely separate t.TempDir() trees here (mirroring that split),
// and extractRename is swapped to capture the exact arguments the rename
// actually used.
func TestExtractChottagRenamesOnlyWithinDestDir(t *testing.T) {
	orig := extractRename
	var gotSrc, gotDst string
	extractRename = func(src, dst string) error {
		gotSrc, gotDst = src, dst
		return orig(src, dst)
	}
	t.Cleanup(func() { extractRename = orig })

	scratchRoot := t.TempDir() // stands in for the download's own scratch dir
	tgz := buildTarGz(t, []tarEntry{{name: "chottag", content: []byte("payload")}})
	tgzPath := filepath.Join(scratchRoot, "asset.tar.gz")
	if err := os.WriteFile(tgzPath, tgz, 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := filepath.Join(t.TempDir(), "versions", "0.3.1") // an unrelated tree
	destBin := filepath.Join(destDir, "chottag")

	if err := extractChottag(tgzPath, destDir, destBin); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(gotSrc) != destDir {
		t.Errorf("rename source = %q, want it inside destDir %q", gotSrc, destDir)
	}
	if filepath.Base(gotSrc) != "chottag.new" {
		t.Errorf("rename source = %q, want destDir/chottag.new (spec §2.3 step 5's own naming)", gotSrc)
	}
	if gotDst != destBin {
		t.Errorf("rename destination = %q, want %q", gotDst, destBin)
	}
	if _, err := os.Stat(destBin); err != nil {
		t.Errorf("destBin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "chottag.new")); err == nil {
		t.Error("chottag.new still exists after a successful rename")
	}
}

// TestExtractChottagCleansUpOnlyWhatItCreated is fix round 2 item B1's own
// cleanup rule: on a failure after destDir existed already, only the
// chottag.new debris is removed, never the directory itself; on a failure
// where THIS call created destDir, the whole directory goes.
func TestExtractChottagCleansUpOnlyWhatItCreated(t *testing.T) {
	t.Run("pre-existing destDir is never removed", func(t *testing.T) {
		tgz := buildTarGz(t, []tarEntry{{name: "../chottag", content: []byte("evil")}}) // never matches: nothing found
		tgzPath := filepath.Join(t.TempDir(), "asset.tar.gz")
		if err := os.WriteFile(tgzPath, tgz, 0o644); err != nil {
			t.Fatal(err)
		}
		destDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(destDir, "keep-me"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		destBin := filepath.Join(destDir, "chottag")

		if err := extractChottag(tgzPath, destDir, destBin); err == nil {
			t.Fatal("extractChottag(no matching entry) = nil error, want one")
		}
		if _, err := os.Stat(destDir); err != nil {
			t.Errorf("pre-existing destDir was removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(destDir, "keep-me")); err != nil {
			t.Errorf("pre-existing destDir's own content was removed: %v", err)
		}
		if _, err := os.Stat(filepath.Join(destDir, "chottag.new")); err == nil {
			t.Error("chottag.new debris left behind")
		}
	})

	t.Run("a freshly-created destDir is removed whole", func(t *testing.T) {
		tgz := buildTarGz(t, []tarEntry{{name: "../chottag", content: []byte("evil")}})
		tgzPath := filepath.Join(t.TempDir(), "asset.tar.gz")
		if err := os.WriteFile(tgzPath, tgz, 0o644); err != nil {
			t.Fatal(err)
		}
		destDir := filepath.Join(t.TempDir(), "versions", "0.3.1") // does not exist yet
		destBin := filepath.Join(destDir, "chottag")

		if err := extractChottag(tgzPath, destDir, destBin); err == nil {
			t.Fatal("extractChottag(no matching entry) = nil error, want one")
		}
		if _, err := os.Stat(destDir); err == nil {
			t.Error("destDir this call created should have been removed on failure")
		}
	})
}

// --- checksums.txt strictness (fix round 1 item 7) --------------------------

func TestChecksumForNoLineForTheAsset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)+"  some_other_file.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checksumFor(path, "chottag_0.3.1_linux_amd64.tar.gz"); err == nil {
		t.Error("checksumFor with no matching line = nil error, want one")
	}
}

func TestChecksumForDuplicateLineIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checksums.txt")
	asset := "chottag_0.3.1_linux_amd64.tar.gz"
	content := strings.Repeat("a", 64) + "  " + asset + "\n" + strings.Repeat("b", 64) + "  " + asset + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checksumFor(path, asset); err == nil {
		t.Error("checksumFor with two lines for the same asset = nil error, want one")
	}
}

func TestChecksumForRejectsAMalformedHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checksums.txt")
	asset := "chottag_0.3.1_linux_amd64.tar.gz"
	if err := os.WriteFile(path, []byte("not-a-hash  "+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checksumFor(path, asset); err == nil {
		t.Error("checksumFor with a non-hex, non-64-char hash = nil error, want one")
	}
}

func TestChecksumForIgnoresAWrongFieldCountLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checksums.txt")
	asset := "chottag_0.3.1_linux_amd64.tar.gz"
	// Three fields: not a valid two-field line, even though the asset
	// name appears among them (fix round 1 item 7: exactly two fields).
	content := strings.Repeat("a", 64) + "  " + asset + "  extra\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := checksumFor(path, asset); err == nil {
		t.Error("checksumFor with a 3-field line = nil error, want it ignored and reported as no matching line")
	}
}

func TestChecksumForAcceptsAValidLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checksums.txt")
	asset := "chottag_0.3.1_linux_amd64.tar.gz"
	hash := strings.Repeat("a", 64)
	if err := os.WriteFile(path, []byte(hash+"  "+asset+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := checksumFor(path, asset)
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Errorf("checksumFor = %q, want %q", got, hash)
	}
}

// --- version ordering (fix round 2 item I1) ---------------------------------

// TestSemverNewer is the controller's fix round 2 item I1 table: MAJOR.
// MINOR.PATCH compared numerically first; on an equal base, a
// git-describe suffix (-<N>-g<hex>, install.sh's own clone-build shape,
// optionally -dirty) ranks AFTER its base, any other "-suffix" is a
// pre-release ranking BEFORE its base, "+build" metadata is ignored, and
// "dev" (or anything unparseable) is older than everything.
func TestSemverNewer(t *testing.T) {
	cases := []struct {
		name      string
		a, b      string
		wantNewer bool // releaseNewer(a, b)
	}{
		{"equal", "0.3.0", "0.3.0", false},
		{"plain newer patch", "0.3.0", "0.3.1", true},
		{"plain older patch", "0.3.1", "0.3.0", false},
		{"rc < release", "0.3.0-rc1", "0.3.0", true},
		{"release < rc of next", "0.3.0", "0.3.0-rc1", false},
		{"release < describe", "0.3.0", "0.3.0-5-gabc1234", true},
		{"describe < release", "0.3.0-5-gabc1234", "0.3.0", false},
		{"describe N ordering", "0.3.0-3-gabc1234", "0.3.0-5-gdef5678", true},
		{"describe N ordering reversed", "0.3.0-5-gdef5678", "0.3.0-3-gabc1234", false},
		{"-dirty does not outrank a clean describe of the same N", "0.3.0-5-gabc1234", "0.3.0-5-gabc1234-dirty", false},
		{"a clean describe does not outrank its own -dirty of the same N", "0.3.0-5-gabc1234-dirty", "0.3.0-5-gabc1234", false},
		{"+build metadata is ignored", "0.3.0+build1", "0.3.0+build2", false},
		{"+build metadata ignored even on a describe suffix", "0.3.0-5-gabc1234+meta1", "0.3.0-5-gabc1234+meta2", false},
		{"dev is older than a release", "dev", "0.3.0", true},
		{"a release is not older than dev", "0.3.0", "dev", false},
		{"dev vs dev", "dev", "dev", false},
		{"pre-release identifier: shorter list ranks lower", "0.3.0-alpha", "0.3.0-alpha.1", true},
		{"pre-release identifier: lexical", "0.3.0-alpha", "0.3.0-beta", true},
		{"pre-release identifier: numeric ranks below alphanumeric", "0.3.0-alpha.1", "0.3.0-alpha.beta", true},
		{"pre-release identifier: numeric compares numerically, not lexically", "0.3.0-alpha.2", "0.3.0-alpha.10", true},
		// Fix round 3 item N1: `git describe --dirty` run exactly AT a tag
		// (install.sh ~143-146) appends a bare "-dirty" with no "-<N>-g<hex>"
		// in front - still a describe suffix (N=0), never a pre-release.
		{"a bare -dirty tag build outranks its own clean release", "0.3.0", "0.3.0-dirty", true},
		{"a clean release does not outrank its own dirty tag build", "0.3.0-dirty", "0.3.0", false},
		{"a real describe outranks an rc of the same base", "0.3.0-rc1", "0.3.0-5-gabc1234", true},
		{"a describe with commits outranks a bare dirty-at-tag build", "0.3.0-dirty", "0.3.0-5-gabc1234", true},
		{"a bare dirty-at-tag build does not outrank a describe with commits", "0.3.0-5-gabc1234", "0.3.0-dirty", false},
		// Fix round 3 item N3: a leading "v" is stripped defensively, and a
		// pre-release identifier that overflows an int must still compare
		// as a (longer) number, never fall back to lexical/text comparison.
		{"a leading v is stripped defensively", "v0.3.0", "v0.3.1", true},
		{"one side spelled with v, the other without", "v0.3.0", "0.3.1", true},
		{"overflowing numeric identifier still compares as a longer number", "0.3.0-5", "0.3.0-99999999999999999999", true},
		{"overflowing numeric identifier reversed", "0.3.0-99999999999999999999", "0.3.0-5", false},
		{"two overflowing numeric identifiers of equal length compare lexically", "0.3.0-10000000000000000001", "0.3.0-10000000000000000002", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := releaseNewer(c.a, c.b); got != c.wantNewer {
				t.Errorf("releaseNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.wantNewer)
			}
		})
	}
}

// --- gh stderr sanitizing (fix round 1 item 8, fix round 2 item M3) ---------

// TestSanitizeGHStderrStripsSignedURLQueryAndUserinfo pins that a signed
// download URL's query string (where the actual credential lives) and any
// userinfo never survive into an error message, that the path IS kept
// (M3: unlike internal/redact's WithoutUserinfo, which drops it), and that
// only the LAST non-empty line of a multi-line stderr is kept.
func TestSanitizeGHStderrStripsSignedURLQueryAndUserinfo(t *testing.T) {
	raw := "generic preamble gh always prints\n" +
		"failed to fetch https://user:hunter2@objects.example.com/foo/bar" +
		"?X-Amz-Signature=SECRETVALUE&token=abc123\n"
	got := sanitizeGHStderr(raw)
	for _, secret := range []string{"SECRETVALUE", "hunter2", "token=abc123", "preamble"} {
		if strings.Contains(got, secret) {
			t.Errorf("sanitizeGHStderr(%q) = %q, must not contain %q", raw, got, secret)
		}
	}
	if !strings.Contains(got, "objects.example.com") {
		t.Errorf("sanitizeGHStderr(%q) = %q, want the host kept", raw, got)
	}
	if !strings.Contains(got, "/foo/bar") {
		t.Errorf("sanitizeGHStderr(%q) = %q, want the path kept", raw, got)
	}
}

// TestSanitizeGHStderrIsCaseInsensitive is M3: gh's own stderr is not
// guaranteed to spell "https" in lowercase.
func TestSanitizeGHStderrIsCaseInsensitive(t *testing.T) {
	raw := "failed: HTTPS://user:hunter2@Example.COM/path?token=SECRET"
	got := sanitizeGHStderr(raw)
	for _, secret := range []string{"hunter2", "SECRET"} {
		if strings.Contains(got, secret) {
			t.Errorf("sanitizeGHStderr(%q) = %q, must not contain %q", raw, got, secret)
		}
	}
	if !strings.Contains(strings.ToLower(got), "example.com/path") {
		t.Errorf("sanitizeGHStderr(%q) = %q, want the host and path kept", raw, got)
	}
}

// TestSanitizeGHStderrKeepsWrappingPunctuation is M3: a URL with no
// userinfo, query or fragment at all — e.g. one merely wrapped in
// parentheses in a human sentence — must survive completely unchanged,
// punctuation included.
func TestSanitizeGHStderrKeepsWrappingPunctuation(t *testing.T) {
	raw := "see (https://api.github.com/repos/o/r/releases/tags/v0.3.1)"
	got := sanitizeGHStderr(raw)
	if !strings.Contains(got, "(https://api.github.com/repos/o/r/releases/tags/v0.3.1)") {
		t.Errorf("sanitizeGHStderr(%q) = %q, want the parenthesized URL to survive whole", raw, got)
	}
}

func TestSanitizeGHStderrCapsLength(t *testing.T) {
	got := sanitizeGHStderr(strings.Repeat("x", 1000))
	if len(got) > maxGHStderrLen {
		t.Errorf("sanitizeGHStderr length = %d, want at most %d", len(got), maxGHStderrLen)
	}
}

// TestSanitizeGHStderrTruncatesOnARuneBoundary is M3: a naive byte-slice
// truncation can split a multi-byte UTF-8 rune in half.
func TestSanitizeGHStderrTruncatesOnARuneBoundary(t *testing.T) {
	// "é" is 2 bytes (U+00E9, 0xC3 0xA9); repeating it so the cap (200
	// bytes) lands mid-rune unless truncateUTF8 backs up.
	got := sanitizeGHStderr(strings.Repeat("é", 150))
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeGHStderr result is not valid UTF-8: %q", got)
	}
	if len(got) > maxGHStderrLen {
		t.Errorf("length = %d, want at most %d", len(got), maxGHStderrLen)
	}
}

// TestTruncateUTF8KeepsValidTextEvenWhenTheRestIsInvalidUTF8 is fix round
// 3 item N2: the previous "back off while !utf8.ValidString" loop could
// not tell invalid UTF-8 already present in s apart from a cut that
// merely landed mid-rune, and would keep backing off — past perfectly
// good text — all the way to "" whenever the invalid bytes sat early in
// s, rather than only at the cut point itself.
func TestTruncateUTF8KeepsValidTextEvenWhenTheRestIsInvalidUTF8(t *testing.T) {
	s := string([]byte{0xff}) + strings.Repeat("a", 300)
	got := truncateUTF8(s, 200)
	if got == "" {
		t.Fatal("truncateUTF8 returned \"\", want the valid \"a\"s kept")
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncateUTF8 result is not valid UTF-8: %q", got)
	}
	if len(got) > 200 {
		t.Errorf("length = %d, want at most 200", len(got))
	}
}

// TestTruncateUTF8CutsAtARuneBoundaryNotJustAByteCount pins the
// mid-rune-cut half of truncateUTF8's contract directly (not only
// through sanitizeGHStderr, whose 200-byte cap and gh-stderr framing
// could mask a boundary bug at other lengths).
func TestTruncateUTF8CutsAtARuneBoundaryNotJustAByteCount(t *testing.T) {
	s := strings.Repeat("é", 150) // 300 bytes, each rune 2 bytes (0xC3 0xA9)
	got := truncateUTF8(s, 51)    // an odd cap: byte 51 lands mid-rune
	if !utf8.ValidString(got) {
		t.Errorf("truncateUTF8(%d bytes of \"é\", 51) = %q, not valid UTF-8", len(s), got)
	}
	if len(got) > 51 {
		t.Errorf("length = %d, want at most 51", len(got))
	}
}

func TestSanitizeGHStderrEmptyIsEmpty(t *testing.T) {
	if got := sanitizeGHStderr("   \n\n  "); got != "" {
		t.Errorf("sanitizeGHStderr(blank) = %q, want empty", got)
	}
}

// TestGHErrorSanitizesStderr is fix round 2 item M4: ghError, the
// standalone function updateGH's production default actually calls, is
// tested directly — no process to spawn — so a future edit that drops or
// bypasses the sanitizer here is caught immediately, not only if some
// caller happens to exercise it with the right input.
func TestGHErrorSanitizesStderr(t *testing.T) {
	args := []string{"release", "download", "--repo", "o/r"}
	stderr := []byte("preamble\nfailed: https://user:hunter2@example.com/x?token=SECRET123\n")
	err := ghError(args, errors.New("exit status 1"), stderr)
	if err == nil {
		t.Fatal("ghError(...) = nil, want an error")
	}
	msg := err.Error()
	for _, secret := range []string{"hunter2", "SECRET123", "preamble"} {
		if strings.Contains(msg, secret) {
			t.Errorf("ghError message = %q, must not contain %q", msg, secret)
		}
	}
	if !strings.Contains(msg, "example.com") {
		t.Errorf("ghError message = %q, want the host kept", msg)
	}
}

// --- setup failure (fix round 1 item 2) --------------------------------------

// TestUpdateSetupFailureLeavesInstallJSONAndOldVersionsUntouched: a failed
// `<new> setup` must fail the whole update (exit 1, code update_failed),
// change nothing already on disk (install.json, old versions), never
// restart the daemon, and its message must not claim the OLD bin/chottag
// link is still intact — setup relinks it before doing anything else that
// could fail.
func TestUpdateSetupFailureLeavesInstallJSONAndOldVersionsUntouched(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	origRec := installRecord{Repo: defaultRepo, Version: "0.3.0", Source: "release", InstalledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := writeInstallRecord(h, origRec); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(h, "versions", "0.3.0")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "chottag"), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}

	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	setupErr := errors.New("boom")
	childCalls := &[]childCall{}
	child := func(_ context.Context, bin string, args ...string) error {
		*childCalls = append(*childCalls, childCall{bin: bin, args: append([]string(nil), args...)})
		if len(args) == 1 && args[0] == "setup" {
			return setupErr
		}
		return nil
	}
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1; stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out.String())
	}
	errObj, _ := doc["error"].(map[string]any)
	if errObj == nil || errObj["code"] != string(codeUpdateFailed) {
		t.Errorf("doc = %v, want error.code %q", doc, codeUpdateFailed)
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "may already have relinked") {
		t.Errorf("error.message = %q, want it to warn that bin/chottag may already be relinked, not claim the old link is intact", msg)
	}
	dest := filepath.Join(h, "versions", "0.3.1", "chottag")
	if !strings.Contains(msg, dest+" setup") {
		t.Errorf("error.message = %q, want it to name %q", msg, dest+" setup")
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok || rec != origRec {
		t.Errorf("install.json = %+v (ok=%v err=%v), want it unchanged at %+v", rec, ok, err, origRec)
	}
	if _, err := os.Stat(filepath.Join(oldDir, "chottag")); err != nil {
		t.Errorf("old versions/0.3.0/chottag should be untouched: %v", err)
	}
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" {
			t.Errorf("child calls = %v, want no daemon restart call after a setup failure", *childCalls)
		}
	}
}

// --- gh missing / logged out ------------------------------------------------

func TestUpdateGHMissing(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, exec.ErrNotFound, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "gh is not installed") {
		t.Errorf("stderr = %q, want the install.sh-equivalent message", errb.String())
	}
}

func TestUpdateGHLoggedOut(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, errors.New("not logged in"), "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "gh is not logged in (run: gh auth login)") {
		t.Errorf("stderr = %q, want the install.sh-equivalent message", errb.String())
	}
}

// --- repo precedence ---------------------------------------------------------

func TestUpdateRepoPrecedence(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	if err := writeInstallRecord(h, installRecord{Repo: "Someone/from-install-json", Version: "0.3.0", Source: "release", InstalledAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check"}, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["repo"] != "Someone/from-install-json" {
		t.Errorf("repo = %v, want the install.json repo", doc["repo"])
	}
	for _, c := range *ghCalls {
		if len(c) >= 3 && c[0] == "release" && c[1] == "view" && c[2] == "--repo" {
			if len(c) > 3 && c[3] != "Someone/from-install-json" {
				t.Errorf("release view --repo = %q, want the install.json repo", c[3])
			}
		}
	}
}

func TestUpdateRepoFlagOverridesInstallJSONAndIsWrittenBack(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	if err := writeInstallRecord(h, installRecord{Repo: "Someone/from-install-json", Version: "0.3.0", Source: "release", InstalledAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--repo", "Given/OnTheCommandLine"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok {
		t.Fatalf("readInstallRecord: ok=%v err=%v", ok, err)
	}
	if rec.Repo != "Given/OnTheCommandLine" {
		t.Errorf("install.json repo = %q, want the --repo value written back", rec.Repo)
	}
}

// TestUpdateRepoFlagWrittenBackWhenAlreadyUpToDate is part 0 final review
// fix 1: a person who reruns `chottag update --repo X` after already being
// up to date (the ver == Version, destBin-already-exists early return)
// still expects the NEXT bare `chottag update` to keep using X, so
// install.json's repo is written back even though nothing was installed —
// but every other field an existing record already held (version, source,
// installedAt) is kept exactly as it was.
func TestUpdateRepoFlagWrittenBackWhenAlreadyUpToDate(t *testing.T) {
	withVersion(t, "0.3.1")
	h := updateHome(t)
	if err := os.MkdirAll(filepath.Join(h, "versions", "0.3.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "versions", "0.3.1", "chottag"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	origRec := installRecord{Repo: "Someone/old", Version: "0.3.1", Source: "release", InstalledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := writeInstallRecord(h, origRec); err != nil {
		t.Fatal(err)
	}
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--repo", "Given/Repo"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("stdout = %q, want it to say already up to date", out.String())
	}
	if len(*childCalls) != 0 {
		t.Errorf("child calls = %v, want none", *childCalls)
	}
	if hasDownloadCall(*ghCalls) {
		t.Errorf("gh calls = %v, want no download call", *ghCalls)
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok {
		t.Fatalf("readInstallRecord: ok=%v err=%v", ok, err)
	}
	if rec.Repo != "Given/Repo" {
		t.Errorf("install.json repo = %q, want the --repo value written back", rec.Repo)
	}
	if rec.Version != origRec.Version || rec.Source != origRec.Source || !rec.InstalledAt.Equal(origRec.InstalledAt) {
		t.Errorf("install.json = %+v, want version/source/installedAt unchanged from %+v", rec, origRec)
	}
}

// TestUpdateRepoFlagWrittenBackWhenBareUpdateWouldNotDowngrade is the
// other "up to date" early return (R82's never-downgrade guard). No
// install.json exists yet here, so the write-back must create one: this
// binary's own Version, source "build" (no versions/<Version>/chottag
// exists in this test's CHOTTAG_HOME), and installedAt around now.
func TestUpdateRepoFlagWrittenBackWhenBareUpdateWouldNotDowngrade(t *testing.T) {
	withVersion(t, "0.3.5")
	h := updateHome(t)
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil) // "latest" is OLDER than 0.3.5
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	before := time.Now().UTC()
	var out, errb bytes.Buffer
	code := runUpdate([]string{"--repo", "Given/Down"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "is up to date") {
		t.Errorf("stdout = %q, want it to say up to date", out.String())
	}
	if hasDownloadCall(*ghCalls) {
		t.Errorf("gh calls = %v, want no download call: never downgrade without --version", *ghCalls)
	}
	if len(*childCalls) != 0 {
		t.Errorf("child calls = %v, want none", *childCalls)
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok {
		t.Fatalf("readInstallRecord: ok=%v err=%v, want a fresh record written", ok, err)
	}
	if rec.Repo != "Given/Down" {
		t.Errorf("install.json repo = %q, want the --repo value", rec.Repo)
	}
	if rec.Version != "0.3.5" {
		t.Errorf("install.json version = %q, want this binary's own Version 0.3.5", rec.Version)
	}
	if rec.Source != "build" {
		t.Errorf("install.json source = %q, want build (no versions/0.3.5/chottag exists)", rec.Source)
	}
	if rec.InstalledAt.Before(before) || rec.InstalledAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("install.json installedAt = %v, want it around now (%v)", rec.InstalledAt, before)
	}
}

// TestUpdateCheckWithRepoFlagDoesNotWriteInstallJSON: --check must never
// write anything, even with --repo given.
func TestUpdateCheckWithRepoFlagDoesNotWriteInstallJSON(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	orig := installRecord{Repo: "Someone/old", Version: "0.3.0", Source: "build", InstalledAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := writeInstallRecord(h, orig); err != nil {
		t.Fatal(err)
	}
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check", "--repo", "Given/Repo"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok || rec != orig {
		t.Errorf("install.json = %+v (ok=%v err=%v), want it unchanged at %+v", rec, ok, err, orig)
	}
}

func TestUpdateCorruptInstallJSONWarnsAndUsesDefault(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	if err := os.WriteFile(filepath.Join(h, installRecordFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--check"}, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["repo"] != defaultRepo {
		t.Errorf("repo = %v, want the default %s", doc["repo"], defaultRepo)
	}
	ws, _ := doc["warnings"].([]any)
	found := false
	for _, w := range ws {
		m, _ := w.(map[string]any)
		if m["code"] == string(warnInstallRecord) {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want an install_record warning", ws)
	}
}

// TestUpdateInvalidRepoInInstallJSONWarnsAndUsesDefault is fix round 1
// item 4: an invalid repo already sitting in install.json (hand-edited, or
// written by an older, less strict install.sh) must never be used, and a
// successful install must correct it, never write the bad value back.
func TestUpdateInvalidRepoInInstallJSONWarnsAndUsesDefault(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	if err := writeInstallRecord(h, installRecord{Repo: "not a valid repo", Version: "0.3.0", Source: "release", InstalledAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, ghCalls := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "invalid repo") {
		t.Errorf("stderr = %q, want an install_record warning about the invalid repo", errb.String())
	}
	for _, c := range *ghCalls {
		if len(c) >= 3 && c[0] == "release" && c[1] == "download" && c[2] == "--repo" {
			// argv is release download --repo R ...; R is c[3].
			if c[3] != defaultRepo {
				t.Errorf("release download --repo = %q, want the default %s", c[3], defaultRepo)
			}
		}
	}
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok {
		t.Fatalf("readInstallRecord: ok=%v err=%v", ok, err)
	}
	if rec.Repo != defaultRepo {
		t.Errorf("install.json repo = %q, want the corrected default %s written back, not the invalid value", rec.Repo, defaultRepo)
	}
}

func TestUpdateBadRepoFlagIsUsageError(t *testing.T) {
	updateHome(t)
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--repo", "bad"}, newReporter(false, &out, &errb))
	if code != exit.Usage {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
	if len(*ghCalls) != 0 {
		t.Errorf("gh calls = %v, want none: a bad --repo must be rejected before any gh call", *ghCalls)
	}
}

// TestUpdateVersionFlagWithoutVPrefixIsUsageError is fix round 1 item 9:
// install.sh's own --version check requires "v" followed by a digit; a
// bare "0.3.0" is a usage error, not a silently-accepted value.
func TestUpdateVersionFlagWithoutVPrefixIsUsageError(t *testing.T) {
	updateHome(t)
	gh, ghCalls := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--version", "0.3.0"}, newReporter(false, &out, &errb))
	if code != exit.Usage {
		t.Fatalf("exit = %d, want 2; stderr=%q", code, errb.String())
	}
	if len(*ghCalls) != 0 {
		t.Errorf("gh calls = %v, want none: a bad --version must be rejected before any gh call", *ghCalls)
	}
}

// --- prune -------------------------------------------------------------------

func TestUpdatePrunesOldVersionsKeepingTheLinkedOne(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	for _, v := range []string{"0.2.0", "0.2.5", "0.2.9", "0.3.0"} {
		dir := filepath.Join(h, "versions", v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binDir := filepath.Join(h, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h, "versions", "0.2.0", "chottag"), filepath.Join(binDir, "chottag")); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	// This fake does NOT relink bin/chottag on setup, so prune reads
	// whatever the pre-seeded link above resolves to (0.2.0), exactly the
	// scenario task-3-brief.md's prune case describes.
	child, _ := fakeChild(t, h, false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	for _, v := range []string{"0.3.1", "0.3.0", "0.2.9", "0.2.0"} {
		if _, err := os.Stat(filepath.Join(h, "versions", v, "chottag")); err != nil {
			t.Errorf("versions/%s/chottag should survive prune: %v", v, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h, "versions", "0.2.5")); err == nil {
		t.Error("versions/0.2.5 should have been pruned")
	}
	if !strings.Contains(out.String(), "pruned 1 old version(s)") {
		t.Errorf("stdout = %q, want \"pruned 1 old version(s)\"", out.String())
	}
}

func TestPruneNeverRemovesADirWithoutAChottagFile(t *testing.T) {
	h := t.TempDir()
	versionsDir := filepath.Join(h, "versions")
	for _, v := range []string{"0.1.0", "0.2.0", "0.3.0", "0.4.0"} {
		if err := os.MkdirAll(filepath.Join(versionsDir, v), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(versionsDir, v, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(versionsDir, "not-a-version"), 0o700); err != nil {
		t.Fatal(err)
	}
	pruned, failed := pruneOldVersions(h, "")
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	if len(pruned) != 1 || pruned[0] != "0.1.0" {
		t.Errorf("pruned = %v, want only 0.1.0", pruned)
	}
	if _, err := os.Stat(filepath.Join(versionsDir, "not-a-version")); err != nil {
		t.Errorf("not-a-version (no chottag file) should never be removed: %v", err)
	}
}

// TestPruneOrdersByTheFullVersionPrecedence is part 0 final review fix 2:
// prune must keep the newest three by the same suffix-aware full version
// order `update` itself uses (parseVersionPrecedence/comparePrecedence),
// not a coarse MAJOR.MINOR.PATCH-only comparison that treats every
// 0.3.0-N-g<hex> describe build as merely "equal to" the bare 0.3.0
// release: a describe build is always newer than the tag it was cut from,
// and a later describe build (higher N) is newer than an earlier one.
func TestPruneOrdersByTheFullVersionPrecedence(t *testing.T) {
	h := t.TempDir()
	versionsDir := filepath.Join(h, "versions")
	names := []string{"0.3.1", "0.3.0", "0.3.0-5-gaaa", "0.3.0-9-gbbb", "0.3.0-10-gccc"}
	for _, v := range names {
		dir := filepath.Join(versionsDir, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pruned, failed := pruneOldVersions(h, "")
	if len(failed) != 0 {
		t.Fatalf("failed = %v, want none", failed)
	}
	wantPruned := map[string]bool{"0.3.0-5-gaaa": true, "0.3.0": true}
	if len(pruned) != len(wantPruned) {
		t.Fatalf("pruned = %v, want exactly %v", pruned, wantPruned)
	}
	for _, p := range pruned {
		if !wantPruned[p] {
			t.Errorf("pruned %q unexpectedly", p)
		}
	}
	for _, keep := range []string{"0.3.1", "0.3.0-10-gccc", "0.3.0-9-gbbb"} {
		if _, err := os.Stat(filepath.Join(versionsDir, keep, "chottag")); err != nil {
			t.Errorf("versions/%s should survive prune (top three by full version order): %v", keep, err)
		}
	}
}

// TestUpdatePruneFailureWarnsAndKeepsOldVersionsExitZero is fix round 1
// item 1: a prune failure is a warning, never a reason to undo or refuse
// to report a successful install.
func TestUpdatePruneFailureWarnsAndKeepsOldVersionsExitZero(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	versionsDir := filepath.Join(h, "versions")
	oldVersions := []string{"0.1.0", "0.2.0", "0.2.5", "0.2.9", "0.3.0"}
	for _, v := range oldVersions {
		dir := filepath.Join(versionsDir, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Pre-create the new version's own dir so extraction's MkdirAll (a
	// no-op on an already-existing dir) never needs write permission on
	// versionsDir itself, which is about to be locked below.
	if err := os.MkdirAll(filepath.Join(versionsDir, "0.3.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	// Lock versionsDir (so os.RemoveAll cannot unlink a directory ENTRY
	// from it) and every pre-existing subdir (so it cannot unlink even the
	// chottag FILE from within one): the old dirs must come through
	// completely untouched, not merely emptied.
	if err := os.Chmod(versionsDir, 0o500); err != nil {
		t.Fatal(err)
	}
	for _, v := range oldVersions {
		if err := os.Chmod(filepath.Join(versionsDir, v), 0o500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, v := range oldVersions {
			os.Chmod(filepath.Join(versionsDir, v), 0o700)
		}
		os.Chmod(versionsDir, 0o700)
	})

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "could not remove old version") {
		t.Errorf("stderr = %q, want a prune_failed warning", errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["installed"] != true {
		t.Errorf("installed = %v, want true: the install itself succeeded even though prune failed", doc["installed"])
	}
	for _, v := range oldVersions {
		if _, err := os.Stat(filepath.Join(versionsDir, v, "chottag")); err != nil {
			t.Errorf("versions/%s/chottag should survive a failed prune untouched: %v", v, err)
		}
	}
}

// TestUpdatePruneProtectsTheRunningDaemonsVersion is fix round 1 item 10:
// the daemon's own probed version must survive prune even once it has
// fallen out of the top three and bin/chottag no longer points at it —
// exactly what three deferred updates in a row produce, since setup
// relinks bin/chottag unconditionally on every install but the running
// daemon itself is never actually replaced while sessions stay live.
func TestUpdatePruneProtectsTheRunningDaemonsVersion(t *testing.T) {
	withVersion(t, "0.1.0")
	h := updateHome(t)
	seedLiveSessions(t, h, 1) // every update below defers, so the "running"
	// daemon (per the stubbed probe) never actually changes.
	origDir := filepath.Join(h, "versions", "0.1.0")
	if err := os.MkdirAll(origDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origDir, "chottag"), []byte("0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	probe := func(int) (bool, string) { return true, "0.1.0" }
	for _, ver := range []string{"0.2.0", "0.3.0", "0.4.0"} {
		fx := goodRelease(t, ver, []byte("payload-"+ver))
		base, _ := fakeGH(t, nil, "v"+ver, nil, map[string]releaseFixture{"v" + ver: fx})
		// A private answer works for all three versions here: 0.2.0 and
		// 0.3.0 predate attestedSince (fix round 2 item 1's stale-latest
		// check would otherwise refuse them on a public repo), and 0.4.0
		// itself just gets the private skip. This test is about prune,
		// not attestation.
		gh := bareUpdateGH(t, base)
		child, _ := fakeChild(t, h, true, nil) // setup DOES relink bin/chottag each time
		stubUpdateSeams(t, gh, child, probe)

		var out, errb bytes.Buffer
		code := runUpdate(nil, newReporter(false, &out, &errb))
		if code != exit.OK {
			t.Fatalf("update to %s: exit = %d, want 0; stderr=%q", ver, code, errb.String())
		}
		if !strings.Contains(out.String(), "deferred") {
			t.Fatalf("update to %s: stdout = %q, want a deferred restart (live session present)", ver, out.String())
		}
	}

	versionsDir := filepath.Join(h, "versions")
	for _, v := range []string{"0.4.0", "0.3.0", "0.2.0", "0.1.0"} {
		if _, err := os.Stat(filepath.Join(versionsDir, v, "chottag")); err != nil {
			t.Errorf("versions/%s should survive (top 3, or the still-running daemon's own version): %v", v, err)
		}
	}
}

// TestUpdatePruneSkipsEntirelyWhenDaemonVersionIsUnknownButRunning is fix
// round 2 item M1: a running daemon whose health version is empty or
// unparseable gives prune nothing sound to protect, so prune is skipped
// entirely (never "protect nothing and prune anyway") — the same
// conservative shape as item 12's unknown-live-session-count handling.
func TestUpdatePruneSkipsEntirelyWhenDaemonVersionIsUnknownButRunning(t *testing.T) {
	withVersion(t, "0.1.0")
	h := updateHome(t)
	versionsDir := filepath.Join(h, "versions")
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.5", "0.2.9"} {
		dir := filepath.Join(versionsDir, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx := goodRelease(t, "0.3.0", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.0", nil, map[string]releaseFixture{"v0.3.0": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	// Running, but the health document's version is empty: unusable, not
	// merely "not running".
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "" })

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "kept all versions: the daemon's version is unknown") {
		t.Errorf("stderr = %q, want the kept-all-versions warning", errb.String())
	}
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.5", "0.2.9", "0.3.0"} {
		if _, err := os.Stat(filepath.Join(versionsDir, v, "chottag")); err != nil {
			t.Errorf("versions/%s should survive: prune must be skipped entirely, not just protect nothing: %v", v, err)
		}
	}
	if !strings.Contains(out.String(), "pruned 0 old version(s)") {
		t.Errorf("stdout = %q, want \"pruned 0 old version(s)\"", out.String())
	}
}

// TestUpdatePruneSkipsEntirelyWhenDaemonPortFails is fix round 2 item M1's
// other case: daemonPort failing means it is not even known whether a
// daemon is running at all, let alone which version — conservatively
// treated the same as "running with an unknown version".
func TestUpdatePruneSkipsEntirelyWhenDaemonPortFails(t *testing.T) {
	withVersion(t, "0.1.0")
	h := updateHome(t)
	versionsDir := filepath.Join(h, "versions")
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.5", "0.2.9"} {
		dir := filepath.Join(versionsDir, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.0", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.0", nil, map[string]releaseFixture{"v0.3.0": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) {
		t.Fatal("updateProbe reached despite daemonPort failing")
		return false, ""
	})

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "kept all versions: the daemon's version is unknown") {
		t.Errorf("stderr = %q, want the kept-all-versions warning", errb.String())
	}
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.5", "0.2.9", "0.3.0"} {
		if _, err := os.Stat(filepath.Join(versionsDir, v, "chottag")); err != nil {
			t.Errorf("versions/%s should survive: %v", v, err)
		}
	}
}

// TestUpdatePruneSkipsWithNoWarningWhenNothingWouldHaveBeenPruned is fix
// round 3 item N4: with three or fewer versions total, skipping prune
// changes nothing at all — pruneCandidates(h, "") is already empty — so
// the "kept all versions" warning would be pure noise, unlike the two
// tests above (5 versions, where skipping genuinely protects two that a
// real prune would otherwise remove).
func TestUpdatePruneSkipsWithNoWarningWhenNothingWouldHaveBeenPruned(t *testing.T) {
	withVersion(t, "0.1.0")
	h := updateHome(t)
	versionsDir := filepath.Join(h, "versions")
	for _, v := range []string{"0.1.0", "0.2.0"} {
		dir := filepath.Join(versionsDir, v)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx := goodRelease(t, "0.3.0", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.0", nil, map[string]releaseFixture{"v0.3.0": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	// Running, but the health document's version is empty: same
	// unknown-version case as the tests above, just with only 3 versions
	// total (the 2 seeded plus this install) once it lands.
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "" })

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "kept all versions") {
		t.Errorf("stderr = %q, want no kept-all-versions warning: nothing would have been pruned anyway", errb.String())
	}
	for _, v := range []string{"0.1.0", "0.2.0", "0.3.0"} {
		if _, err := os.Stat(filepath.Join(versionsDir, v, "chottag")); err != nil {
			t.Errorf("versions/%s should survive: %v", v, err)
		}
	}
	if !strings.Contains(out.String(), "pruned 0 old version(s)") {
		t.Errorf("stdout = %q, want \"pruned 0 old version(s)\"", out.String())
	}
}

// --- daemon outcomes ---------------------------------------------------------

func TestUpdateDaemonNotRunning(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["daemon"] != "not-running" {
		t.Errorf("daemon = %v, want not-running", doc["daemon"])
	}
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" {
			t.Errorf("child calls = %v, want no daemon restart call", *childCalls)
		}
	}
}

func TestUpdateDaemonRunningNoLiveSessionsRestarts(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "0.3.0" })

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "daemon restarted") {
		t.Errorf("stdout = %q, want \"daemon restarted\"", out.String())
	}
	found := false
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" && c.args[1] == "restart" {
			found = true
		}
	}
	if !found {
		t.Errorf("child calls = %v, want a daemon restart call", *childCalls)
	}
}

// seedLiveSessions writes n live session records for home's registry, each
// naming this test process's own pid: alive() only checks that the pid
// exists, not that each entry's filename matches it, so several files
// naming the current (certainly alive) pid read back as that many live
// sessions (task-3-brief.md's own prescribed trick).
func seedLiveSessions(t *testing.T, home string, n int) {
	t.Helper()
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	for i := 0; i < n; i++ {
		// Registry.Add keys its file by pid, so a second call would
		// overwrite the first; write directly, matching the registry's own
		// file format, so n distinct files exist.
		b, err := json.Marshal(session.Session{PID: pid, Port: 47821 + i})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "run", fmt.Sprintf("%d.json", 900000+i)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = reg
}

func TestUpdateDaemonRunningWithLiveSessionsDefers(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	seedLiveSessions(t, h, 2)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "0.3.0" })

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["daemon"] != "deferred" {
		t.Errorf("daemon = %v, want deferred", doc["daemon"])
	}
	if doc["liveSessions"] != float64(2) {
		t.Errorf("liveSessions = %v, want 2", doc["liveSessions"])
	}
	ws, _ := doc["warnings"].([]any)
	found := false
	for _, w := range ws {
		m, _ := w.(map[string]any)
		if m["code"] == string(warnUpdateDeferred) {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want an update_deferred warning", ws)
	}
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" {
			t.Errorf("child calls = %v, want no daemon restart call", *childCalls)
		}
	}
}

func TestUpdateRestartFlagRestartsDespiteLiveSessions(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	seedLiveSessions(t, h, 1)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "0.3.0" })

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--restart"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "daemon restarted") {
		t.Errorf("stdout = %q, want \"daemon restarted\"", out.String())
	}
	found := false
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" && c.args[1] == "restart" {
			found = true
		}
	}
	if !found {
		t.Errorf("child calls = %v, want a daemon restart call despite live sessions", *childCalls)
	}
}

func TestUpdateRestartChildErrorWarnsAndExitsZero(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, errors.New("boom"))
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "0.3.0" })

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["daemon"] != "restart-failed" {
		t.Errorf("daemon = %v, want restart-failed", doc["daemon"])
	}
	ws, _ := doc["warnings"].([]any)
	found := false
	var msg string
	for _, w := range ws {
		m, _ := w.(map[string]any)
		if m["code"] == string(warnRestartFailed) {
			found = true
			msg, _ = m["message"].(string)
		}
	}
	if !found {
		t.Errorf("warnings = %v, want a restart_failed warning", ws)
	}
	// Part 0 final review fix 3: don't tell the person to rerun the same
	// `daemon restart` command that just failed — the child's own stderr
	// (already on this process's stderr) names the supervisor command
	// instead. Say the new version still starts on its own next time.
	want := "daemon restart failed (see the message above); the new version starts with the next daemon start"
	if !strings.Contains(msg, want) {
		t.Errorf("restart_failed message = %q, want it to contain %q", msg, want)
	}
	if strings.Contains(msg, "run:") {
		t.Errorf("restart_failed message = %q, want no rerun-the-same-command hint", msg)
	}
}

// TestUpdateDaemonPortErrorIsAWarningNotAFailure is fix round 1 item 11: a
// daemonPort/state error after an otherwise-successful install must not
// undo it — exit 0, installed:true, daemon "not-probed", a warning, and no
// daemon restart attempted (there is no port to probe or restart through).
func TestUpdateDaemonPortErrorIsAWarningNotAFailure(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	// Corrupt state.json after updateHome's own valid write, so daemonPort
	// (store.Store.Load) fails.
	if err := os.WriteFile(filepath.Join(h, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) {
		t.Fatal("updateProbe reached despite daemonPort failing; there is no port to probe")
		return false, ""
	})

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(true, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["installed"] != true {
		t.Errorf("installed = %v, want true", doc["installed"])
	}
	if doc["daemon"] != "not-probed" {
		t.Errorf("daemon = %v, want not-probed", doc["daemon"])
	}
	if len(errb.String()) == 0 {
		t.Error("stderr is empty, want a warning about the daemon not being probed")
	}
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" {
			t.Errorf("child calls = %v, want no daemon restart call", *childCalls)
		}
	}
}

// TestUpdateSessionRegistryUnreadableDefersAndSaysCountUnknown is fix
// round 1 item 12: an unreadable session registry defers (treated as
// live) unless --restart, and the text says "session count unknown", not
// "0 session(s)".
func TestUpdateSessionRegistryUnreadableDefersAndSaysCountUnknown(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	runDir := filepath.Join(h, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runDir, 0o300); err != nil { // -wx: os.ReadDir fails
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(runDir, 0o700) })

	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "0.3.0" })

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "session count unknown") {
		t.Errorf("stdout = %q, want \"session count unknown\", not a claimed count", out.String())
	}
	if strings.Contains(out.String(), "0 session(s)") {
		t.Errorf("stdout = %q, must not claim 0 sessions when the count is actually unknown", out.String())
	}
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" {
			t.Errorf("child calls = %v, want no daemon restart call (deferred)", *childCalls)
		}
	}
}

// TestUpdateSessionRegistryUnreadableWithRestartFlagStillRestarts checks
// --restart still overrides an unknown session count, same as it overrides
// a known live one.
func TestUpdateSessionRegistryUnreadableWithRestartFlagStillRestarts(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	runDir := filepath.Join(h, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runDir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(runDir, 0o700) })

	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, "0.3.0" })

	var out, errb bytes.Buffer
	code := runUpdate([]string{"--restart"}, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	found := false
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" && c.args[1] == "restart" {
			found = true
		}
	}
	if !found {
		t.Errorf("child calls = %v, want a daemon restart call despite an unreadable registry, since --restart was given", *childCalls)
	}
}

// TestUpdateChildInheritsTheProcessEnvironmentUnchanged is Review Focus 2:
// a proxied shell (HTTPS_PROXY set to the daemon's own address) must reach
// `chottag daemon restart` unchanged. update's updateChild seam takes no
// env parameter at all — runUpdate's one call site
// (updateChild(ctx, dest, "daemon", "restart")) has no way to override
// it — so what actually decides this is execUpdateChild, updateChild's
// production default (update.go's own doc comment there names this test).
// This calls it directly, bypassing the mutable updateChild var (which
// TestMain permanently overwrites with a panicking default for the whole
// test binary), with a small shell script — never gh, claude or a
// downloaded/built chottag — as the "child", and reads back what
// environment it actually saw.
func TestUpdateChildInheritsTheProcessEnvironmentUnchanged(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:58212")
	outFile := filepath.Join(t.TempDir(), "env.out")
	if err := execUpdateChild(context.Background(), "/bin/sh", "-c", "env > "+outFile); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "HTTPS_PROXY=http://127.0.0.1:58212") {
		t.Errorf("child env = %q, want it to carry this process's HTTPS_PROXY unchanged", b)
	}
}

// --- one-document harness (jsonharness_test.go) -----------------------------

func init() {
	registerJSONCases(
		jsonCase{
			name: "update --check reports an available update", command: "update",
			setup: func(t *testing.T) []string {
				withVersion(t, "0.3.0")
				updateHome(t)
				gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
				child, _ := fakeChild(t, "", false, nil)
				stubUpdateSeams(t, gh, child, neverRunningProbe)
				return []string{"update", "--check"}
			},
			check: func(t *testing.T, doc map[string]any) {
				if doc["repo"] != defaultRepo || doc["current"] != "0.3.0" || doc["latest"] != "0.3.1" || doc["updateAvailable"] != true {
					t.Errorf("doc = %v, want repo %s, current 0.3.0, latest 0.3.1, updateAvailable true", doc, defaultRepo)
				}
			},
		},
		jsonCase{
			name: "update --repo bad is a usage error", command: "update",
			setup: func(t *testing.T) []string {
				updateHome(t)
				gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
				child, _ := fakeChild(t, "", false, nil)
				stubUpdateSeams(t, gh, child, neverRunningProbe)
				return []string{"update", "--repo", "bad"}
			},
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			// Fix round 1 item 14: a full install, asserting installed:true
			// and a non-empty pruned[].
			name: "update installs and reports pruned", command: "update",
			setup: func(t *testing.T) []string {
				withVersion(t, "0.3.0")
				h := updateHome(t)
				for _, v := range []string{"0.1.0", "0.2.0", "0.2.5", "0.2.9"} {
					dir := filepath.Join(h, "versions", v)
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "chottag"), []byte(v), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				fx := goodRelease(t, "0.3.1", []byte("payload"))
				base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
				gh := bareUpdateGH(t, base)
				child, _ := fakeChild(t, h, true, nil)
				stubUpdateSeams(t, gh, child, neverRunningProbe)
				return []string{"update"}
			},
			check: func(t *testing.T, doc map[string]any) {
				if doc["installed"] != true {
					t.Errorf("installed = %v, want true", doc["installed"])
				}
				pruned, _ := doc["pruned"].([]any)
				if len(pruned) == 0 {
					t.Errorf("pruned = %v, want at least one pruned version", doc["pruned"])
				}
			},
		},
	)
}
