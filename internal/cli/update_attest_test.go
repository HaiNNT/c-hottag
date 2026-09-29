package cli

// Tests for the build-attestation cut-over (spec Task 7, Rulings 18-19):
// `chottag update` verifies `gh attestation verify` for a release at or
// above attestedSince from a public repo, and fails closed on any error,
// a renamed repo, or an unexpected answer. Every gh call goes through
// attestGH, itself layered on fakeGH (update_test.go): no test here runs
// the real gh.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

// attestGH wraps fakeGH (update_test.go): "repo view" answers private and
// (unless renamedTo overrides it) the repo it was asked about, so no
// rename is reported by default; "attestation verify --help" fails
// unless supported; and "attestation verify <file>" returns verifyErr.
// Every call is recorded, including the ones this func passes through to
// base — so *calls is the FULL chronological gh call log, not just the
// attestation-related ones (TestAttestationRunsAfterDownloadAndBeforeExtraction
// relies on that).
func attestGH(t *testing.T, base func(context.Context, ...string) ([]byte, error), private, renamedTo string, repoErr error, supported bool, verifyErr error) (func(context.Context, ...string) ([]byte, error), *[][]string) {
	calls := &[][]string{}
	return func(ctx context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string(nil), args...))
		switch {
		case len(args) >= 3 && args[0] == "repo" && args[1] == "view":
			name := renamedTo
			if name == "" {
				name = args[2]
			}
			return []byte(private + "\t" + name + "\n"), repoErr
		case len(args) >= 3 && args[0] == "attestation" && args[2] == "--help":
			if !supported {
				return nil, errors.New(`unknown command "attestation" for "gh"`)
			}
			return nil, nil
		case len(args) >= 2 && args[0] == "attestation" && args[1] == "verify":
			return nil, verifyErr
		}
		return base(ctx, args...)
	}, calls
}

func TestUpdateAttestationCutOver(t *testing.T) {
	cases := []struct {
		name, tag, private, renamedTo string
		repoErr                       error
		supported                     bool
		verifyErr                     error
		args                          []string // defaults to {} when nil; runUpdate's own flags never include --json (newReporter's jsonMode carries that)
		wantExit                      int
		wantInstalled                 bool
		wantCalls                     []string // the attestation-related verbs, in order
		wantWarn                      string   // "" means: assert neither attestation_skipped nor pre_attestation appears
		wantErrSubstr                 string
	}{
		// Fix round 2 item 1: an explicit rollback (--version) makes no
		// repo-view call, since a release this old was never attested
		// regardless of the repo's visibility. A BARE update landing on
		// a pre-cutover latest is a different story now (see the
		// TestUpdateDevBuild... tests below) — R82's "never goes down"
		// can't protect against it once Version fails to parse.
		{name: "explicit rollback before the cut-over: no calls", tag: "v0.3.1", private: "false", supported: true, args: []string{"--version", "v0.3.1"}, wantExit: exit.OK, wantInstalled: true, wantWarn: "pre_attestation"},
		{name: "public, verified", tag: "v0.4.0", private: "false", supported: true, wantExit: exit.OK, wantInstalled: true, wantCalls: []string{"repo view", "attestation --help", "attestation verify"}},
		{name: "pre-release of the cut-over counts", tag: "v0.4.0-rc1", private: "false", supported: true, wantExit: exit.OK, wantInstalled: true, wantCalls: []string{"repo view", "attestation --help", "attestation verify"}},
		{name: "public, verify fails", tag: "v0.4.1", private: "false", supported: true, verifyErr: errors.New("no attestations found"), wantExit: exit.Error, wantInstalled: false, wantCalls: []string{"repo view", "attestation --help", "attestation verify"}},
		{name: "public, gh too old", tag: "v0.4.1", private: "false", supported: false, wantExit: exit.Error, wantInstalled: false, wantCalls: []string{"repo view", "attestation --help"}, wantErrSubstr: "no attestation command"},
		{name: "private: skipped with a warning", tag: "v0.4.1", private: "true", supported: true, wantExit: exit.OK, wantInstalled: true, wantCalls: []string{"repo view"}, wantWarn: "attestation_skipped"},
		{name: "repo view fails", tag: "v0.4.1", private: "", repoErr: errors.New("HTTP 502"), supported: true, wantExit: exit.Error, wantInstalled: false, wantCalls: []string{"repo view"}},
		{name: "repo view says nonsense", tag: "v0.4.1", private: "maybe", supported: true, wantExit: exit.Error, wantInstalled: false, wantCalls: []string{"repo view"}},
		{name: "repo renamed: fails closed", tag: "v0.4.1", private: "false", renamedTo: "New/Owner", supported: true, wantExit: exit.Error, wantInstalled: false, wantCalls: []string{"repo view"}, wantErrSubstr: "moved to New/Owner"},
		{name: "--check never verifies", tag: "v0.4.1", private: "false", supported: true, args: []string{"--check"}, wantExit: exit.OK, wantInstalled: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withVersion(t, "0.3.0")
			h := updateHome(t)
			ver := strings.TrimPrefix(c.tag, "v")
			base, _ := fakeGH(t, nil, c.tag, nil, map[string]releaseFixture{c.tag: goodRelease(t, ver, []byte("#!/bin/sh\n"))})
			gh, calls := attestGH(t, base, c.private, c.renamedTo, c.repoErr, c.supported, c.verifyErr)
			child, _ := fakeChild(t, h, true, nil)
			stubUpdateSeams(t, gh, child, neverRunningProbe)
			args := c.args
			var out, errb bytes.Buffer
			code := runUpdate(args, newReporter(true, &out, &errb))
			if code != c.wantExit {
				t.Fatalf("exit %d, want %d; out=%s", code, c.wantExit, out.String())
			}
			_, err := os.Stat(filepath.Join(h, "versions", ver, "chottag"))
			if (err == nil) != c.wantInstalled {
				t.Errorf("installed = %v, want %v", err == nil, c.wantInstalled)
			}
			var got []string
			for _, a := range *calls {
				switch {
				case a[0] == "repo":
					got = append(got, "repo view")
				case a[0] == "attestation" && slices.Contains(a, "--help"):
					got = append(got, "attestation --help")
				case a[0] == "attestation":
					got = append(got, "attestation verify")
					if !slices.Contains(a, "--repo") || !strings.HasSuffix(a[2], releaseAssetName(ver)) {
						t.Errorf("verify args = %v, want the downloaded asset and --repo", a)
					}
				}
			}
			if !slices.Equal(got, c.wantCalls) {
				t.Errorf("attestation calls = %v, want %v", got, c.wantCalls)
			}
			// Fix round 1 item 5a: exactly the wanted warning code
			// appears, and never the other one — attestation_skipped
			// in particular must never fire "for free".
			for _, code := range []string{"attestation_skipped", "pre_attestation"} {
				gotCode := strings.Contains(out.String(), code)
				wantCode := c.wantWarn == code
				if gotCode != wantCode {
					t.Errorf("warning %s present = %v, want %v\n%s", code, gotCode, wantCode, out.String())
				}
			}
			if c.wantErrSubstr != "" && !strings.Contains(out.String(), c.wantErrSubstr) {
				t.Errorf("want %q in %s", c.wantErrSubstr, out.String())
			}
		})
	}
}

// TestAttestationRunsAfterDownloadAndBeforeExtraction is fix round 1 item
// 5b: the gh calls happen in exactly this order — release download, then
// (crossing the cut-over) repo view, attestation --help, attestation
// verify — and only then would extraction place the binary. The "verify
// fails" case above already pins the second half (nothing is installed
// when verify fails); this pins the gh-call ordering itself.
func TestAttestationRunsAfterDownloadAndBeforeExtraction(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	tag, ver := "v0.4.1", "0.4.1"
	base, _ := fakeGH(t, nil, tag, nil, map[string]releaseFixture{tag: goodRelease(t, ver, []byte("#!/bin/sh\n"))})
	gh, calls := attestGH(t, base, "false", "", nil, true, nil)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit %d; stderr=%s", code, errb.String())
	}

	var verbs []string
	for _, a := range *calls {
		switch {
		case len(a) >= 2 && a[0] == "release" && a[1] == "download":
			verbs = append(verbs, "download")
		case len(a) >= 2 && a[0] == "repo" && a[1] == "view":
			verbs = append(verbs, "repo view")
		case len(a) >= 3 && a[0] == "attestation" && a[2] == "--help":
			verbs = append(verbs, "attestation --help")
		case len(a) >= 2 && a[0] == "attestation" && a[1] == "verify":
			verbs = append(verbs, "attestation verify")
		}
	}
	want := []string{"download", "repo view", "attestation --help", "attestation verify"}
	if !slices.Equal(verbs, want) {
		t.Errorf("gh call order = %v, want %v", verbs, want)
	}
}

// TestUpdateDevBuildRefusesStalePublicLatest is fix round 2 item 1: R82's
// "a bare `chottag update` never goes down" relies on Version parsing,
// which a "dev" build's unstamped default never does, so a bare update
// can land on a release below the cut-over after all. On a public repo
// that must refuse, not install unverified — mirroring install.sh's own
// refusal of a stale public latest.
func TestUpdateDevBuildRefusesStalePublicLatest(t *testing.T) {
	withVersion(t, "dev")
	h := updateHome(t)
	tag, ver := "v0.3.9", "0.3.9"
	base, _ := fakeGH(t, nil, tag, nil, map[string]releaseFixture{tag: goodRelease(t, ver, []byte("#!/bin/sh\n"))})
	gh, calls := attestGH(t, base, "false", "", nil, true, nil)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit %d, want %d; stderr=%s", code, exit.Error, errb.String())
	}
	if _, err := os.Stat(filepath.Join(h, "versions", ver, "chottag")); err == nil {
		t.Error("installed, want nothing installed")
	}
	found := false
	for _, a := range *calls {
		if len(a) >= 2 && a[0] == "repo" && a[1] == "view" {
			found = true
		}
	}
	if !found {
		t.Errorf("no gh repo view call in %v", *calls)
	}
	if !strings.Contains(errb.String(), "predates attestations") {
		t.Errorf("stderr = %q, want it to say the release predates attestations", errb.String())
	}
}

// TestUpdateDevBuildStaleLatestOnPrivateRepoWarnsAndInstalls is the other
// half of fix round 2 item 1: a private repo (which never gets
// attestations either way) gets the checksum-only note instead of a
// refusal, and the install proceeds.
func TestUpdateDevBuildStaleLatestOnPrivateRepoWarnsAndInstalls(t *testing.T) {
	withVersion(t, "dev")
	h := updateHome(t)
	tag, ver := "v0.3.9", "0.3.9"
	base, _ := fakeGH(t, nil, tag, nil, map[string]releaseFixture{tag: goodRelease(t, ver, []byte("#!/bin/sh\n"))})
	gh, calls := attestGH(t, base, "true", "", nil, true, nil)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.OK {
		t.Fatalf("exit %d; stderr=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(h, "versions", ver, "chottag")); err != nil {
		t.Errorf("not installed: %v", err)
	}
	found := false
	for _, a := range *calls {
		if len(a) >= 2 && a[0] == "repo" && a[1] == "view" {
			found = true
		}
	}
	if !found {
		t.Errorf("no gh repo view call in %v", *calls)
	}
	for _, a := range *calls {
		if len(a) >= 1 && a[0] == "attestation" {
			t.Errorf("attestation call %v: a release below the cut-over must never run attestation", a)
		}
	}
	if !strings.Contains(errb.String(), "predates attestations") {
		t.Errorf("stderr = %q, want the pre-attestation note", errb.String())
	}
}

// TestUpdateBareUpdateRefusesStalePublicLatestOnAReleaseBuild is fix
// round 3 item 1: the stale-latest refusal (fix round 2 item 1) is not a
// "dev build" special case — an ordinary release build (a normally
// parsing Version) refuses a bare update's public, pre-cutover latest
// exactly the same way, because R82's "never goes down" guard says
// nothing about whether the LATEST release itself predates attestedSince.
func TestUpdateBareUpdateRefusesStalePublicLatestOnAReleaseBuild(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	tag, ver := "v0.3.1", "0.3.1"
	base, _ := fakeGH(t, nil, tag, nil, map[string]releaseFixture{tag: goodRelease(t, ver, []byte("#!/bin/sh\n"))})
	gh, calls := attestGH(t, base, "false", "", nil, true, nil)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)

	var out, errb bytes.Buffer
	code := runUpdate(nil, newReporter(false, &out, &errb))
	if code != exit.Error {
		t.Fatalf("exit %d, want %d; stderr=%s", code, exit.Error, errb.String())
	}
	if _, err := os.Stat(filepath.Join(h, "versions", ver, "chottag")); err == nil {
		t.Error("installed, want nothing installed")
	}
	found := false
	for _, a := range *calls {
		if len(a) >= 2 && a[0] == "repo" && a[1] == "view" {
			found = true
		}
	}
	if !found {
		t.Errorf("no gh repo view call in %v", *calls)
	}
}

// TestCheckVersionTagRejectsMalformedBases is fix round 2 item 2: Go's
// versionTagPattern must agree with install.sh's check_version_tag (a
// case-pattern mirror) on every edge case that mirror's own rewrite was
// meant to get right, including the two a naive `set -- $vbase` word
// split could get wrong by globbing against a file in the cwd.
func TestCheckVersionTagRejectsMalformedBases(t *testing.T) {
	for _, tag := range []string{"v1.2.[3]", "v1.2.?", "v1.2.3.", "1.2.3.", "v1.2.3-"} {
		if _, ok := checkVersionTag(tag); ok {
			t.Errorf("checkVersionTag(%q) = ok, want rejected", tag)
		}
	}
}

func TestAttestedSinceMatchesInstallSh(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^ATTESTED_SINCE=(\S+)$`).FindSubmatch(b)
	if m == nil || string(m[1]) != attestedSince {
		t.Fatalf("install.sh ATTESTED_SINCE = %q, want %q", m, attestedSince)
	}
}
