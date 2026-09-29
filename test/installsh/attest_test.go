package installsh

// Tests for install.sh's build-attestation check (Task 7, Rulings 18-19):
// a release at or after ATTESTED_SINCE, from a public repo, must pass `gh
// attestation verify`, failing closed on any error. The fake gh's default
// FAKE_GH_PRIVATE is "true" (harness_test.go), so every pre-existing test
// in this package (release v1.2.3) takes the private, checksum-only path.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttestationVerifiedOnAPublicRepo(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_GH_PRIVATE"] = "false"

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	calls := s.calls()
	want := []string{
		"gh repo view " + repo + " --json isPrivate,nameWithOwner --jq ",
		"gh attestation verify --help",
		"gh attestation verify ",
	}
	for _, w := range want {
		if !s.called(w) {
			t.Errorf("no call starting %q in:\n%s", w, strings.Join(calls, "\n"))
		}
	}
	verified := false
	for _, c := range calls {
		if strings.HasPrefix(c, "gh attestation verify ") && !strings.Contains(c, "--help") &&
			strings.Contains(c, assetName(relVersion)) && strings.Contains(c, "--repo "+repo) {
			verified = true
		}
	}
	if !verified {
		t.Errorf("attestation verify was not called with the downloaded asset and --repo %s:\n%s", repo, strings.Join(calls, "\n"))
	}
	if !strings.Contains(r.stdout, "verified the build attestation") {
		t.Errorf("stdout lacks the verified-attestation line:\n%s", r)
	}
}

func TestAttestationFailureInstallsNothing(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_GH_PRIVATE"] = "false"
	s.env["FAKE_GH_ATTEST"] = "fail"

	r := s.install()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r)
	}
	if !strings.Contains(r.stderr, assetName(relVersion)) || !strings.Contains(r.stderr, "nothing was installed") {
		t.Errorf("stderr = %q, want it to name the asset and say nothing was installed", r.stderr)
	}
	s.assertNothingInstalled()
}

func TestAttestationUnsupportedGhFailsClosed(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_GH_PRIVATE"] = "false"
	s.env["FAKE_GH_ATTEST"] = "unsupported"

	r := s.install()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r)
	}
	if !strings.Contains(r.stderr, "upgrade gh") {
		t.Errorf("stderr = %q, want it to say to upgrade gh", r.stderr)
	}
	s.assertNothingInstalled()
}

func TestAttestationPrivateRepoIsCheckedByChecksumOnly(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	// FAKE_GH_PRIVATE left unset: the fake gh's default is "true".

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, relVersion)
	found := false
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "note:") && strings.Contains(line, "is private") {
			found = true
		}
	}
	if !found {
		t.Errorf("stdout lacks a note: line naming the repo as private:\n%s", r)
	}
	if s.called("gh attestation") {
		t.Errorf("a private repo must never call gh attestation:\n%s", strings.Join(s.calls(), "\n"))
	}
}

func TestAttestationRepoViewFailureFailsClosed(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_GH_REPO_FAIL"] = "1"

	r := s.install()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r)
	}
	s.assertNothingInstalled()
}

func TestAttestationNotAskedBeforeTheCutOver(t *testing.T) {
	s := newSandbox(t)
	s.release("v0.3.9", false)

	r := s.install("--version", "v0.3.9")
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, "0.3.9")
	if s.called("gh repo view") {
		t.Errorf("a pre-cut-over release must never call gh repo view:\n%s", strings.Join(s.calls(), "\n"))
	}
	if !strings.Contains(r.stdout, "predates attestations") {
		t.Errorf("stdout = %q, want a note that the release predates attestations", r.stdout)
	}
}

// TestNonSemverLatestTagNeverInstalledUnverified is fix round 1 item 1
// (HIGH): the previous base_at_least treated any unparseable version as
// already at the cut-over, so a "latest" tag that isn't a real version at
// all (a moved branch tip, a nightly channel) skipped verification
// entirely. base_before now fails closed instead, and check_version_tag
// rejects such a tag outright.
func TestNonSemverLatestTagNeverInstalledUnverified(t *testing.T) {
	for _, tag := range []string{"nightly", "V9.0.0"} {
		t.Run(tag, func(t *testing.T) {
			s := newSandbox(t)
			s.env["FAKE_GH_LATEST"] = tag
			s.env["FAKE_GH_PRIVATE"] = "false"

			r := s.install()
			if r.code == 0 {
				t.Fatalf("exit 0, want a failure: a non-version latest tag must never install unverified\n%s", r)
			}
			s.assertNothingInstalled()
			if s.called("gh attestation") || s.called("gh repo view") {
				t.Errorf("a non-version tag must never reach the attestation logic:\n%s", strings.Join(s.calls(), "\n"))
			}
		})
	}
}

// TestNonSemverLatestTagNeverFallsBackToACloneBuild is fix round 2 item
// 4's second nit: try_release's own comment says a failed check is
// always fatal, never a reason to try the next source - check_version_tag
// failing on a gh-provided tag must die outright, not set why_release and
// quietly let main() succeed with a Go build instead (which would mask
// exactly the situation - a moved "latest" pointer, or a corrupted
// release list - this check exists to catch).
func TestNonSemverLatestTagNeverFallsBackToACloneBuild(t *testing.T) {
	s := newSandbox(t)
	s.env["FAKE_GH_LATEST"] = "nightly"
	dir := s.clone() // go and git are on PATH: a build IS possible here

	r := s.exec(s.root, nil, filepath.Join(dir, "install.sh"))
	if r.code != 1 || !strings.Contains(r.stderr, "does not look like a version") {
		t.Fatalf("want exit 1 naming the bad tag\n%s", r)
	}
	s.assertNothingInstalled()
	if s.called("go ") {
		t.Error("a non-version latest tag fell back to a Go build")
	}
}

// TestAttestationPreReleaseOfTheCutOverStillVerifies pins the rc1 case
// (fix round 1 item 1's test list): a pre-release of the cut-over itself
// came from the same attesting workflow and must still be verified.
func TestAttestationPreReleaseOfTheCutOverStillVerifies(t *testing.T) {
	s := newSandbox(t)
	tag := "v0.4.0-rc1"
	s.release(tag, false)
	s.env["FAKE_GH_LATEST"] = tag
	s.env["FAKE_GH_PRIVATE"] = "false"

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, "0.4.0-rc1")
	if !s.called("gh attestation verify") {
		t.Errorf("a pre-release of the cut-over must still be verified:\n%s", strings.Join(s.calls(), "\n"))
	}
}

// TestAttestationRepoViewUnexpectedAnswerFailsClosed is fix round 1 item
// 1's last test: an unexpected isPrivate answer fails closed.
func TestAttestationRepoViewUnexpectedAnswerFailsClosed(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_GH_PRIVATE"] = "maybe"

	r := s.install()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r)
	}
	if !strings.Contains(r.stderr, "unexpected answer") {
		t.Errorf("stderr = %q, want it to say gh repo view gave an unexpected answer", r.stderr)
	}
	s.assertNothingInstalled()
}

// TestAttestationRepoRenamedFailsClosed is fix round 1 item 2: a repo
// gh reports as renamed must never be silently queried or attested
// against under its old name.
func TestAttestationRepoRenamedFailsClosed(t *testing.T) {
	s := newSandbox(t)
	s.release(relTag, false)
	s.env["FAKE_GH_LATEST"] = relTag
	s.env["FAKE_GH_PRIVATE"] = "false"
	s.env["FAKE_GH_REPO_NAME"] = "New/Owner"

	r := s.install()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r)
	}
	if !strings.Contains(r.stderr, "moved to New/Owner") || !strings.Contains(r.stderr, "--repo New/Owner") {
		t.Errorf("stderr = %q, want it to name the new repo and --repo New/Owner", r.stderr)
	}
	s.assertNothingInstalled()
}

// TestRepoViewMalformedAnswerFailsClosedWithoutClaimingARename is fix
// round 2 item 3: an answer without exactly one tab (none, or more than
// one) fails closed as simply "unexpected" - never as a "moved to" built
// from a field cut out of the wrong place in a malformed line.
func TestRepoViewMalformedAnswerFailsClosedWithoutClaimingARename(t *testing.T) {
	for _, answer := range []string{"false", "false\tHaiNNT/c-hottag\textra"} {
		t.Run(answer, func(t *testing.T) {
			s := newSandbox(t)
			s.release(relTag, false)
			s.env["FAKE_GH_LATEST"] = relTag
			s.env["FAKE_GH_REPO_ANSWER"] = answer

			r := s.install()
			if r.code != 1 {
				t.Fatalf("exit %d, want 1\n%s", r.code, r)
			}
			if !strings.Contains(r.stderr, "unexpected answer") {
				t.Errorf("stderr = %q, want \"unexpected answer\"", r.stderr)
			}
			if strings.Contains(r.stderr, "moved to") {
				t.Errorf("stderr = %q, must not claim a rename from a malformed answer", r.stderr)
			}
			s.assertNothingInstalled()
		})
	}
}

// TestRefuseStaleLatestOnPublicRepo is fix round 1 item 3's install.sh
// half: with no --version, a latest release below ATTESTED_SINCE on a
// public repo is refused outright, rather than installed unverified.
func TestRefuseStaleLatestOnPublicRepo(t *testing.T) {
	s := newSandbox(t)
	s.release("v0.3.9", false)
	s.env["FAKE_GH_LATEST"] = "v0.3.9"
	s.env["FAKE_GH_PRIVATE"] = "false"

	r := s.install()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", r.code, r)
	}
	if !strings.Contains(r.stderr, "predates attestations") {
		t.Errorf("stderr = %q, want it to say the release predates attestations", r.stderr)
	}
	s.assertNothingInstalled()
}

// TestNonSemverLatestTagRejectsGlobLikeBasesEvenWithAMatchingFile is fix
// round 2 item 2: check_version_tag must reject a base holding a glob
// metacharacter on its own terms, never because a file in the current
// directory happens to make it "match" — the previous `set -- $vbase`
// word split would glob-expand an unquoted "[3]" or "?" against the cwd.
// The stderr check (fix round 3 item 3) is what actually tells the old
// code from the new: with the bug, "[3]" globs to "3" inside
// check_version_tag's own (unobservable) positional parameters, but the
// ORIGINAL string still reaches check_version right after — which
// rejects it too, just for being an unsafe character, with "refusing the
// version string" rather than "does not look like a version". Both
// versions die with a non-zero exit and nothing installed; only the
// message proves which check actually caught it.
func TestNonSemverLatestTagRejectsGlobLikeBasesEvenWithAMatchingFile(t *testing.T) {
	for _, tag := range []string{"v1.2.[3]", "v1.2.?"} {
		t.Run(tag, func(t *testing.T) {
			s := newSandbox(t)
			if err := os.WriteFile(filepath.Join(s.root, "3"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			s.env["FAKE_GH_LATEST"] = tag

			r := s.install()
			if r.code == 0 {
				t.Fatalf("exit 0, want a failure: %q must never parse as a version\n%s", tag, r)
			}
			if !strings.Contains(r.stderr, "does not look like a version") {
				t.Errorf("stderr = %q, want \"does not look like a version\" (check_version_tag itself rejected it)", r.stderr)
			}
			if strings.Contains(r.stderr, "refusing the version string") {
				t.Errorf("stderr = %q, must not be check_version's message: check_version_tag should have rejected %q before check_version ever saw it", r.stderr, tag)
			}
			s.assertNothingInstalled()
		})
	}
}

// TestLatestTagWithATrailingOrMissingDotRejected is fix round 2 item 2's
// other two cases: a trailing dot (one dot too many, leaving an empty
// field) is rejected with or without a leading "v".
func TestLatestTagWithATrailingOrMissingDotRejected(t *testing.T) {
	for _, tag := range []string{"v1.2.3.", "1.2.3."} {
		t.Run(tag, func(t *testing.T) {
			s := newSandbox(t)
			s.env["FAKE_GH_LATEST"] = tag

			r := s.install()
			if r.code == 0 {
				t.Fatalf("exit 0, want a failure: %q must never parse as a version\n%s", tag, r)
			}
			s.assertNothingInstalled()
		})
	}
}

// TestLatestTagWithAnEmptySuffixRejected is fix round 3 item 2:
// versionTagPattern's own suffix group requires at least one character
// after the "-" or "+" marker; check_version_tag now agrees.
func TestLatestTagWithAnEmptySuffixRejected(t *testing.T) {
	s := newSandbox(t)
	s.env["FAKE_GH_LATEST"] = "v1.2.3-"

	r := s.install()
	if r.code == 0 {
		t.Fatalf("exit 0, want a failure: \"v1.2.3-\" must never parse as a version\n%s", r)
	}
	if !strings.Contains(r.stderr, "does not look like a version") {
		t.Errorf("stderr = %q, want \"does not look like a version\"", r.stderr)
	}
	s.assertNothingInstalled()
}

// TestStaleLatestOnPrivateRepoGetsChecksumOnlyNote: the same stale-latest
// situation, but a private repo (which never gets attestations either
// way) gets a note instead of a refusal.
func TestStaleLatestOnPrivateRepoGetsChecksumOnlyNote(t *testing.T) {
	s := newSandbox(t)
	s.release("v0.3.9", false)
	s.env["FAKE_GH_LATEST"] = "v0.3.9"
	// FAKE_GH_PRIVATE left unset: the fake gh's default is "true".

	r := s.install()
	if r.code != 0 {
		t.Fatal(r)
	}
	s.assertInstalled(s.chHome, "0.3.9")
	if !strings.Contains(r.stdout, "predates attestations") {
		t.Errorf("stdout = %q, want a note that the release predates attestations", r.stdout)
	}
}
