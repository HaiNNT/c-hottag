package cli

// runUpdate is `chottag update` (spec §2.3): check, install, roll back,
// prune, and the R82 daemon-restart policy. Every external effect (gh, a
// child chottag, the daemon health probe) sits behind a package-level seam
// var, exactly like shim's execFn/spawnFn and this package's
// claudeAuthExec/credsDelete/signalFn (F116/F130): TestMain
// (invariants_test.go) installs a panicking default for each, so a test
// that forgets to stub one fails loudly instead of really running gh, a
// downloaded binary, or probing a real daemon.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/shim"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
	"github.com/HaiNNT/c-hottag/internal/usagepoll"
)

const updateUsage = "usage: chottag update [--check] [--version vX.Y.Z] [--repo OWNER/NAME] [--restart | --no-restart]\n       chottag update [--auto-check on|off] [--auto-install on|off] [--auto-restart on|off]"

// versionTagPattern is install.sh's check_version rule (spec §2.2, §2.3): a
// release tag, with an optional leading v and an optional pre-release or
// build suffix. It validates a tag already in hand regardless of where it
// came from (gh's own `release view`, or --version): install.sh's
// check_version runs after any leading "v" has already been stripped, so
// it never requires one either.
var versionTagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?$`)

// versionFlagPrefixPattern is install.sh's own inline --version check
// instead: `case $want_tag in v[0-9]*)`, stricter than versionTagPattern
// in the one respect that matters for a flag a person types — it must
// start with "v" followed by a digit, never a bare "0.3.0" (fix round 1
// item 9).
var versionFlagPrefixPattern = regexp.MustCompile(`^v[0-9]`)

// checkVersionTag reports whether tag passes versionTagPattern, and returns
// it with a leading "v" stripped.
func checkVersionTag(tag string) (ver string, ok bool) {
	if !versionTagPattern.MatchString(tag) {
		return "", false
	}
	return strings.TrimPrefix(tag, "v"), true
}

// updateResult is `update --json`'s fields (spec §2.3). Daemon is one of
// "not-running", "restarted", "deferred", "restart-failed",
// "not-restarted" (--no-restart: the daemon was left alone), or
// "not-probed" (fix round 1 item 11: a daemonPort/health error after an
// otherwise-successful install, so the daemon was never reached at all).
type updateResult struct {
	Repo            string   `json:"repo"`
	Current         string   `json:"current"`
	Latest          string   `json:"latest"`
	UpdateAvailable bool     `json:"updateAvailable"`
	Installed       bool     `json:"installed"`
	Pruned          []string `json:"pruned,omitempty"`
	Daemon          string   `json:"daemon,omitempty"`
	LiveSessions    int      `json:"liveSessions,omitempty"`
	// SelfRestart, set only when Daemon is "deferred", says whether the
	// running daemon restarts itself onto the new version when idle (R126);
	// false means `chottag daemon restart` finishes the update (R129).
	SelfRestart *bool `json:"selfRestart,omitempty"`
	// Backups lists the files update copied before installing (R123).
	Backups []string `json:"backups,omitempty"`
	// WhatsNew is a short summary of each release newer than the old
	// version, up to the one now available or installed, newest first (R157).
	// Absent when none was found or the releases list could not be fetched.
	WhatsNew []updatecheck.Note `json:"whatsNew,omitempty"`
	// Plugin is the Claude Code plugin's update step, present when a newer
	// release is available (check) or was installed (R156).
	Plugin *pluginStep `json:"plugin,omitempty"`
}

// pluginStep is what to run to bring the Claude Code plugin to the release
// chottag just installed or found. chottag never runs claude itself.
type pluginStep struct {
	Commands []string `json:"commands"`
	Note     string   `json:"note"`
}

// pluginUpdateStep is the one plugin step (R156).
var pluginUpdateStep = pluginStep{
	Commands: []string{
		"claude plugin marketplace update c-hottag",
		"claude plugin update chottag@c-hottag",
		"/reload-plugins",
	},
	Note: "chottag does not run claude. Run the first two in a terminal, or have Claude Code run them after you agree, then /reload-plugins in each open Claude Code session.",
}

// --- seams ------------------------------------------------------------

// updateGH runs `gh args...` and returns its stdout. TestMain installs a
// panicking default; production's own default (below) is installed at
// package init and never touched by production code.
var updateGH = func(ctx context.Context, args ...string) ([]byte, error) {
	// gh is an operator-installed binary on this shell's PATH, never
	// request- or network-derived.
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, ghError(args, err, ee.Stderr)
		}
		return out, err
	}
	return out, nil
}

// updateFetch asks GitHub for the repo's latest release: one HTTPS GET, no
// token, no gh (R124). upstream is the already-resolved upstream proxy (nil
// dials direct): the daemon passes its own, and `chottag update --check`
// resolves one with updateUpstream. TestMain installs a panicking default.
var updateFetch = fetchViaUpstream

// fetchViaUpstream is updateFetch's production body.
func fetchViaUpstream(ctx context.Context, upstream *url.URL, repo string) (updatecheck.Release, error) {
	return fetchLatest(ctx, usagepoll.NewClient(upstream), updatecheck.APIBase, repo)
}

// updateReleases lists the repo's newest releases, with their notes, for the
// "what's new" summary (R157): one HTTPS GET through the same upstream and
// client as updateFetch. TestMain installs a panicking default.
var updateReleases = fetchReleasesViaUpstream

// fetchReleasesViaUpstream is updateReleases's production body.
func fetchReleasesViaUpstream(ctx context.Context, upstream *url.URL, repo string) ([]updatecheck.Release, error) {
	ctx, cancel := context.WithTimeout(ctx, updateFetchTimeout)
	defer cancel()
	return updatecheck.List(ctx, usagepoll.NewClient(upstream), updatecheck.APIBase, repo)
}

// whatsNew is the summary of the releases after since, up to upTo. Any
// failure (no upstream, the fetch) only drops it: the update never fails
// for it.
func whatsNew(ctx context.Context, h, repo, since, upTo string) []updatecheck.Note {
	upstream, err := updateUpstream(h)
	if err != nil {
		return nil
	}
	rels, err := updateReleases(ctx, upstream, repo)
	if err != nil {
		return nil
	}
	return updatecheck.WhatsNew(rels, since, upTo)
}

// textWhatsNew prints the "What's new" block, if there is one.
func textWhatsNew(r *reporter, notes []updatecheck.Note) {
	if len(notes) == 0 {
		return
	}
	r.Text("\nWhat's new:\n")
	for _, n := range notes {
		r.Text("  %s: %s\n", n.Version, n.Summary)
		if n.URL != "" {
			r.Text("    %s\n", n.URL)
		}
	}
}

// textPluginStep prints the plugin step.
func textPluginStep(r *reporter) {
	r.Text("\nUpdate the Claude Code plugin too (chottag does not run claude):\n")
	for _, c := range pluginUpdateStep.Commands[:2] {
		r.Text("  %s\n", c)
	}
	r.Text("then run %s in each open Claude Code session\n", pluginUpdateStep.Commands[2])
}

// updateExtras fills res's whatsNew and plugin from the release now
// available (check) or installed, and prints them, for a newer release.
func updateExtras(ctx context.Context, h, repo, ver string, newer bool, res *updateResult, r *reporter) {
	if !newer || os.Getenv(autoUpdateEnv) == "1" {
		return
	}
	res.WhatsNew = whatsNew(ctx, h, repo, Version, ver)
	textWhatsNew(r, res.WhatsNew)
	step := pluginUpdateStep
	res.Plugin = &step
	textPluginStep(r)
}

// updateUpstream is the upstream proxy a CLI run of the check uses, resolved
// as `daemon start` does: the shell's HTTPS_PROXY, unless it is chottag's
// own address.
func updateUpstream(h string) (*url.URL, error) {
	port, err := daemonPort(h)
	if err != nil {
		port = 0 // no known port: nothing can be chottag's own
	}
	u, err := parseUpstreamProxy(shim.SpawnUpstream(os.Environ(), port))
	if err != nil {
		return nil, fmt.Errorf("HTTPS_PROXY: %w", err)
	}
	return u, nil
}

// fetchLatest is updateFetch's request, with a 10 s bound, on c.
func fetchLatest(ctx context.Context, c *http.Client, apiBase, repo string) (updatecheck.Release, error) {
	ctx, cancel := context.WithTimeout(ctx, updateFetchTimeout)
	defer cancel()
	return updatecheck.Latest(ctx, c, apiBase, repo)
}

// updateFetchTimeout bounds one update check.
const updateFetchTimeout = 10 * time.Second

// updateLockName is the file, under <home>/run, an install holds so a
// person's update and the daemon's never overlap.
const updateLockName = "update.lock"

// updateRepo is the repo `update` works against for home h: install.json's
// when it is valid, else the default.
func updateRepo(h string) string { return resolveUpdateRepo(h, nil) }

func resolveUpdateRepo(h string, r *reporter) string {
	rec, ok, err := readInstallRecord(h)
	switch {
	case err != nil:
		if r != nil {
			r.Warn(warnInstallRecord, "chottag: install.json is unreadable; using the default repo: "+err.Error())
		}
	case ok && rec.Repo != "":
		if validRepo(rec.Repo) {
			return rec.Repo
		}
		// Fix round 1 item 4: an invalid repo already sitting in
		// install.json (hand-edited, or from a version of install.sh
		// that validated less strictly) must never be used, or written
		// back — the repo stays defaultRepo, and nothing overwrites
		// it with rec.Repo again.
		if r != nil {
			r.Warn(warnInstallRecord, fmt.Sprintf("chottag: invalid repo %q in install.json; using the default %s", rec.Repo, defaultRepo))
		}
	}
	return defaultRepo
}

// releaseNewer reports whether candidate is a strictly newer release than
// current, for `update`'s install path. It is updatecheck.Newer plus one
// rule: a current version that does not parse (this binary's own unstamped
// "dev") is older than any release, so a dev build can update to one. A
// candidate that does not parse is never newer.
func releaseNewer(current, candidate string) bool {
	if !updatecheck.Parses(candidate) {
		return false
	}
	if !updatecheck.Parses(current) {
		return true
	}
	return updatecheck.Newer(candidate, current)
}

// ghError is the error updateGH's production default returns for a
// failed `gh args...`: err (from exec.Cmd.Output(), e.g. "exit status 1")
// with whatever sanitizeGHStderr keeps of stderr appended, so nothing raw
// ever reaches a caller's message or a --json error document. Pulled out
// of updateGH's closure so it is directly testable without spawning any
// process: TestGHErrorSanitizesStderr calls it with a stderr holding a
// secret and fails immediately if that secret survives — it would notice
// at once if a future edit dropped the sanitizeGHStderr call here, rather
// than only failing if some caller happens to exercise it with the right
// input (fix round 2 item M4).
func ghError(args []string, err error, stderr []byte) error {
	msg := fmt.Sprintf("gh %s: %v", strings.Join(args, " "), err)
	if sanitized := sanitizeGHStderr(string(stderr)); sanitized != "" {
		msg += ": " + sanitized
	}
	return errors.New(msg)
}

// urlLikePattern finds an http(s) URL substring in gh's stderr, so
// sanitizeGHStderr can strip whatever userinfo, query string or fragment
// it carries — exactly where a signed download URL's credential would
// leak (fix round 1 item 8) — before it ever reaches this process's own
// stderr or a --json error document. Case-insensitive (fix round 2 item
// M3): gh's own stderr is not guaranteed to spell "https" in lowercase.
var urlLikePattern = regexp.MustCompile(`(?i)https?://\S+`)

// maxGHStderrLen caps the sanitized line's length, the same way
// install.sh's own gh_reason helper caps its "note: release not used"
// text at 200 chars.
const maxGHStderrLen = 200

// redactURL reduces raw (an http(s) URL substring found in gh's stderr)
// to scheme://host/path: userinfo, the query string and the fragment are
// dropped entirely (exactly where a signed URL's credential lives), but
// the path is kept, unlike internal/redact's WithoutUserinfo (built for a
// different need — comparing which upstream proxy two sides use — which
// drops the path too and would turn a URL a person might want to
// recognize, e.g. one naming the actual release asset, into just a bare
// host). raw that fails to parse as a URL with a host — e.g. the "URL"
// match is `\S+`-greedy and happened to swallow something odd — becomes a
// fixed placeholder rather than passing it through unredacted.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// truncateUTF8 returns s, cut to at most maxBytes bytes on a UTF-8 rune
// boundary: a plain byte slice (s[:maxBytes]) can split a multi-byte rune
// in half and produce invalid UTF-8 (fix round 2 item M3).
//
// strings.ToValidUTF8 runs first (fix round 3 item N2) so any invalid
// UTF-8 already present in s — not from truncation, just raw bytes gh
// happened to write — is gone before the cut is even considered: the
// previous "back off while !utf8.ValidString" loop had no way to tell
// that case apart from a cut mid-rune, and would keep backing off past
// perfectly good text all the way to "" if the invalid bytes sat early
// in s. Once s is entirely valid UTF-8, cutting at maxBytes can only
// ever land inside the LAST rune it would otherwise include — for that,
// utf8.RuneStart bounds the back-off to at most 3 bytes (the most a
// UTF-8 rune ever adds beyond its first byte).
func truncateUTF8(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for i := 0; i < 3 && end > 0 && !utf8.RuneStart(s[end]); i++ {
		end--
	}
	return s[:end]
}

// sanitizeGHStderr reduces gh's captured stderr to one line safe to embed
// in an error message: its last non-empty line (gh's own multi-line
// output tends to end with the specific part, everything before it is
// usually generic preamble), with any URL redacted through redactURL,
// capped in length on a rune boundary.
func sanitizeGHStderr(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			last = l
			break
		}
	}
	last = urlLikePattern.ReplaceAllStringFunc(last, redactURL)
	return truncateUTF8(last, maxGHStderrLen)
}

// execUpdateChild is updateChild's production default: it runs bin
// args..., the same trust boundary install.sh's own `"$dest/chottag"
// setup` and daemon restart lines are (a binary chottag itself just
// verified and placed under CHOTTAG_HOME, never request- or
// network-derived), with stdin closed and stdout/stderr going to this
// process's own stderr.
//
// It sets no cmd.Env, so exec.Cmd's own zero value applies: the child
// inherits this process's environment exactly as os.Environ() would
// report it, unchanged — in particular a proxied shell's HTTPS_PROXY
// reaches `chottag daemon restart` untouched (spec §2.3, Review Focus 2).
// TestUpdateChildInheritsTheProcessEnvironmentUnchanged (update_test.go)
// calls this function directly (never through the mutable updateChild
// var, which TestMain permanently overwrites with a panicking default for
// the whole test binary) to pin exactly that. The other half of this
// guarantee — that the daemon, once restarted, never adopts its own
// address as its upstream — is internal/shim/shim_test.go's
// TestSpawnedUpstreamExcludesOurOwnAddress.
func execUpdateChild(ctx context.Context, bin string, args ...string) error {
	// nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// updateChild runs bin args... as a child, stdin closed, stdout/stderr to
// this process's own stderr (execUpdateChild above).
var updateChild = execUpdateChild

// updateProbe wraps shim.ProbeHealth.
var updateProbe = func(port int) (running bool, version string) {
	ok, h := shim.ProbeHealth(port)
	return ok, h.Version
}

// --- gh helpers ---------------------------------------------------------

// ghAuthCheck runs `gh auth status`, failing with the same messages
// install.sh's try_release uses when gh is missing or logged out.
func ghAuthCheck(ctx context.Context) error {
	if _, err := updateGH(ctx, "auth", "status"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("gh is not installed")
		}
		return errors.New("gh is not logged in (run: gh auth login)")
	}
	return nil
}

// latestTag returns repo's latest release tag through `gh release view`.
func latestTag(ctx context.Context, repo string) (string, error) {
	out, err := updateGH(ctx, "release", "view", "--repo", repo, "--json", "tagName", "--jq", ".tagName")
	if err != nil {
		return "", fmt.Errorf("gh sees no release of %s: %w", repo, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hexSha256Pattern is a bare, lowercase sha256 hex digest: checksums.txt's
// first field, strictly (fix round 1 item 7).
var hexSha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checksumFor returns asset's expected sha256 from checksumsPath (a
// downloaded checksums.txt), matching install.sh's own awk line: a
// candidate line's second field is either the asset's name or "*"+name
// (the "binary mode" marker sha256sum/shasum prepend it with). Strict
// (fix round 1 item 7): a candidate line must have exactly two
// whitespace-separated fields; its first field must look like a sha256
// hex digest, or this errors rather than trust a malformed hash; and two
// lines both naming asset is an error, never "whichever comes first".
func checksumFor(checksumsPath, asset string) (string, error) {
	b, err := os.ReadFile(checksumsPath)
	if err != nil {
		return "", err
	}
	found := ""
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] != asset && fields[1] != "*"+asset {
			continue
		}
		if !hexSha256Pattern.MatchString(fields[0]) {
			return "", fmt.Errorf("checksums.txt's line for %s does not hold a sha256 hex digest", asset)
		}
		if found != "" {
			return "", fmt.Errorf("checksums.txt has two lines for %s", asset)
		}
		found = fields[0]
	}
	if found == "" {
		return "", fmt.Errorf("checksums.txt has no line for %s", asset)
	}
	return found, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// maxExtractedSize caps the chottag entry extractChottag will copy: a
// download this size is already absurd for a CLI binary, and refusing
// upfront — rather than trusting a possibly-lying tar header for an
// effectively unbounded copy — is cheap insurance (fix round 1 item 6). A
// var, not a const, so TestExtractChottagRejectsAnOversizedEntry can lower
// it for the duration of one test rather than building a real 256 MiB tar
// entry.
var maxExtractedSize int64 = 256 << 20 // 256 MiB

// extractRename is os.Rename, seamed only so a test can capture the exact
// arguments a rename used — never anything more dangerous, and never
// given a panicking TestMain default: it defaults straight to the real
// os.Rename, and every call site here only ever renames a path this same
// function just wrote under destDir. TestExtractChottagRenamesOnlyWithinDestDir
// swaps it to assert the source is exactly destDir/chottag.new (fix round
// 2 item B1).
var extractRename = os.Rename

// extractChottag walks tgzPath and places the one entry it accepts — a
// regular file whose cleaned name is exactly "chottag", no larger than
// maxExtractedSize — at destBin. Anything else named chottag (a symlink,
// hardlink or directory), or a chottag reached only through a path like
// "../chottag" or "/abs/chottag" (whose cleaned name is never exactly
// "chottag"), is simply never accepted: if nothing in the archive
// matches, this returns an error naming the asset, exactly as it would for
// an asset that never held a chottag binary at all — there is no special
// case to get wrong for a hostile entry, because the acceptance rule
// alone already excludes it.
//
// The accepted entry is copied into destDir/chottag.new FIRST (spec
// §2.3 step 5's own naming), through io.CopyN bounded to hdr.Size so a
// lying header cannot make the copy exceed either the size check below or
// the entry's own declared length, and only then renamed to destBin —
// both steps inside destDir, so the rename is never cross-filesystem even
// when tgzPath sits in a scratch dir (os.MkdirTemp("", ...), normally
// $TMPDIR) on a different filesystem than CHOTTAG_HOME. Fix round 2 item
// B1: the previous version buffered the copy in that scratch dir instead
// and renamed from there into destDir — exactly the shape of a
// cross-device rename, which fails with EXDEV on any Linux box whose
// /tmp is a tmpfs (a common default), so every single update failed
// there, and left an empty versions/<ver>/ directory behind besides
// (os.MkdirAll had already run).
//
// destDir is created if it does not exist yet; on any failure after that
// point, chottag.new is removed, and destDir itself is removed too, but
// ONLY if this call is the one that created it — this function must never
// delete a directory it did not create.
func extractChottag(tgzPath, destDir, destBin string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	destDirExisted := true
	if _, err := os.Stat(destDir); errors.Is(err, os.ErrNotExist) {
		destDirExisted = false
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	tmpPath := filepath.Join(destDir, "chottag.new")
	placed := false
	defer func() {
		if placed {
			return
		}
		if destDirExisted {
			os.Remove(tmpPath)
		} else {
			os.RemoveAll(destDir)
		}
	}()

	tmpBin, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			tmpBin.Close()
			return err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Clean(hdr.Name) != "chottag" {
			continue
		}
		if hdr.Size > maxExtractedSize {
			tmpBin.Close()
			return fmt.Errorf("chottag entry is %d bytes, over the %d byte limit", hdr.Size, maxExtractedSize)
		}
		if _, err := io.CopyN(tmpBin, tr, hdr.Size); err != nil {
			tmpBin.Close()
			return err
		}
		found = true
		break
	}
	if closeErr := tmpBin.Close(); closeErr != nil {
		return closeErr
	}
	if !found {
		return fmt.Errorf("holds no chottag binary")
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := extractRename(tmpPath, destBin); err != nil {
		return err
	}
	placed = true
	return nil
}

// --- semver comparison ---------------------------------------------------

// parseBaseSemver parses base (already stripped of any "-" suffix and
// "+" build metadata) as MAJOR.MINOR.PATCH.
func parseBaseSemver(base string) (major, minor, patch int, ok bool) {
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	ints := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, false
		}
		ints[i] = n
	}
	return ints[0], ints[1], ints[2], true
}

// attestedSince is the first release built by the attesting workflow
// (part 1). A release at or after it, from a public repo, must pass
// gh attestation verify; install.sh's ATTESTED_SINCE is the same value.
// "Exists" is decided by version and visibility, never by asking whether
// an attestation happens to be there: a replaced asset has none, and
// must fail, not skip (Ruling 18).
const attestedSince = "0.4.0"

// baseBefore compares MAJOR.MINOR.PATCH only, so 0.4.0-rc1 is not
// before 0.4.0: it came from the same workflow. An unparseable version
// is never "before", which fails closed toward verifying.
func baseBefore(ver, cut string) bool {
	a1, b1, c1, ok1 := parseBaseSemver(baseOf(ver))
	a2, b2, c2, ok2 := parseBaseSemver(cut)
	if !ok1 || !ok2 {
		return false
	}
	if a1 != a2 {
		return a1 < a2
	}
	if b1 != b2 {
		return b1 < b2
	}
	return c1 < c2
}

// baseOf strips a -pre-release or +build suffix.
func baseOf(v string) string {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return v[:i]
	}
	return v
}

// repoViewJQ asks `gh repo view` for both fields this package needs in one
// call, as a single tab-separated line: whether repo is private, and its
// current canonical nameWithOwner (fix round 1 item 2 — a renamed repo
// must be caught wherever isPrivate is asked, in both attestationRequired
// and the pre-cutover "refuse a stale latest" path a future caller may
// add). install.sh's repo_view asks the exact same fields the exact same
// way, so a mistaken repo shows up identically in either tool.
const repoViewJQ = "[.isPrivate,.nameWithOwner]|@tsv"

// repoView runs the gh repo view call above and returns repo's privacy and
// its current nameWithOwner. A renamed repo (nameWithOwner does not match
// repo, case-insensitively) fails closed with the new name, so a caller
// never silently asks about, or attests against, the wrong repo (fix
// round 1 item 2). An error or a malformed answer also fails closed.
func repoView(ctx context.Context, repo string) (private bool, err error) {
	out, err := updateGH(ctx, "repo", "view", repo, "--json", "isPrivate,nameWithOwner", "--jq", repoViewJQ)
	if err != nil {
		return false, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), "\t")
	if len(fields) != 2 {
		return false, fmt.Errorf("gh repo view %s gave an unexpected answer", repo)
	}
	privateStr, nameWithOwner := fields[0], fields[1]
	if !strings.EqualFold(nameWithOwner, repo) {
		return false, fmt.Errorf("%s moved to %s; run: chottag update --repo %s", repo, nameWithOwner, nameWithOwner)
	}
	switch privateStr {
	case "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("gh repo view %s gave an unexpected isPrivate answer", repo)
}

// attestationRequired reports whether ver (from repo) must pass
// verifyAttestation: at or after attestedSince, and repo is public. An
// error or an unexpected `gh repo view` answer fails closed (returns an
// error, never false): Ruling 18.
func attestationRequired(ctx context.Context, repo, ver string) (bool, error) {
	if baseBefore(ver, attestedSince) {
		return false, nil
	}
	private, err := repoView(ctx, repo)
	if err != nil {
		return false, fmt.Errorf("%s's release attestation cannot be checked: %w", repo, err)
	}
	return !private, nil
}

// verifyAttestation checks assetPath's build provenance attestation
// against repo (Ruling 19: no --signer-workflow pin). A gh too old to
// support `gh attestation verify` fails closed with a message naming the
// upgrade, rather than silently skipping, and includes gh's own (already
// sanitized, via updateGH's own ghError) reason (fix round 1 item 6).
func verifyAttestation(ctx context.Context, repo, assetPath string) error {
	if _, err := updateGH(ctx, "attestation", "verify", "--help"); err != nil {
		return fmt.Errorf("gh has no attestation command (or it failed): %v; upgrade gh (2.49 or later) and run chottag update again", err)
	}
	if _, err := updateGH(ctx, "attestation", "verify", assetPath, "--repo", repo); err != nil {
		return fmt.Errorf("the build attestation of %s does not verify against %s: %w", filepath.Base(assetPath), repo, err)
	}
	return nil
}

// describeSuffixPattern matches install.sh's own `git describe --tags
// --always --dirty` output shape (install.sh ~143-146), with the tag's
// "v" and MAJOR.MINOR.PATCH already stripped: "-<N>-g<hex>", optionally
// followed by "-dirty" when the working tree had uncommitted changes at
// build time.
var describeSuffixPattern = regexp.MustCompile(`^([0-9]+)-g[0-9a-f]+(-dirty)?$`)

// bareDirtySuffix is the OTHER shape install.sh ~143-146 can produce: `git
// describe --dirty` run exactly AT a tag, with no commits since, still
// appends "-dirty" on its own with no "-<N>-g<hex>" in front (fix round 3
// item N1). It is a describe suffix, not a pre-release: a dirty build at
// v0.3.0 is still built from v0.3.0's own commit, exactly like an N=0
// describe would be, and must rank AFTER the bare "0.3.0" release the
// same way any other describe suffix does — never before it as a
// generic "-suffix" pre-release would.
const bareDirtySuffix = "dirty"

// suffixKind ranks a version's suffix relative to its own bare base (fix
// round 2 item I1's ruling): a pre-release suffix (anything after a "-"
// that is not a git-describe suffix) sorts BEFORE the bare base, as
// ordinary semver precedence dictates; the bare base itself; and a
// describe suffix — necessarily built from a commit AFTER whichever tag
// git found, so it is never actually AT that tag's release — sorts AFTER
// it. Declared in that order so a plain int comparison of two kinds
// already gives the right ranking.
type suffixKind int

const (
	kindPreRelease suffixKind = iota
	kindBare
	kindDescribe
)

// versionPrecedence is one version string's full ordering, once its
// MAJOR.MINOR.PATCH base and suffix (if any) have been classified.
type versionPrecedence struct {
	major, minor, patch int
	kind                suffixKind
	describeN           int      // valid only when kind == kindDescribe
	preIdents           []string // valid only when kind == kindPreRelease: semver's own dot-separated identifiers
}

// parseVersionPrecedence parses s into its full ordering. checkVersionTag
// and cli.Version both already leave any leading "v" stripped, but a "v"
// is stripped here too, defensively (fix round 3 item N3) — this
// function's own doc comment is the only contract a future caller sees,
// and it should hold even for a caller that hands it a raw tag. ok is
// false when even the MAJOR.MINOR.PATCH base fails to parse —
// releaseNewer treats that as always the oldest possible value, the same
// rule "dev" (this binary's own unstamped default) has always needed.
//
// A describe suffix built from a commit AFTER a pre-release tag (e.g.
// "0.3.0-rc1-5-gabc1234") is deliberately out of scope: `gh release
// view`'s "latest" never resolves to a pre-release tag in the first
// place (GitHub excludes releases marked prerelease from it), so
// describeSuffixPattern is only ever matched against a suffix that
// followed a genuine release tag.
func parseVersionPrecedence(s string) (versionPrecedence, bool) {
	s = strings.TrimPrefix(s, "v")
	// "+build" metadata is ignored for precedence entirely (semver §10):
	// dropped before anything else here even looks at it.
	core, _, _ := strings.Cut(s, "+")
	base, suffix, hasSuffix := strings.Cut(core, "-")
	major, minor, patch, ok := parseBaseSemver(base)
	if !ok {
		return versionPrecedence{}, false
	}
	if !hasSuffix {
		return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindBare}, true
	}
	// Fix round 3 item N1: `git describe --dirty` run exactly AT a tag
	// (install.sh ~143-146) appends only "-dirty", with no "-<N>-g<hex>"
	// in front — describeSuffixPattern alone would miss this and let it
	// fall through to kindPreRelease, ranking a dirty clone build of
	// v0.3.0 BEFORE the v0.3.0 release itself, which a bare `chottag
	// update` would then "helpfully" replace it with. Treated as
	// kindDescribe with N=0, exactly the ordering an actual "-0-g<hex>"
	// would have gotten had git produced one.
	if suffix == bareDirtySuffix {
		return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindDescribe, describeN: 0}, true
	}
	if m := describeSuffixPattern.FindStringSubmatch(suffix); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return versionPrecedence{}, false // an int this large would be unprecedented; fail closed
		}
		return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindDescribe, describeN: n}, true
	}
	return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindPreRelease, preIdents: strings.Split(suffix, ".")}, true
}

// numericIdentPattern is semver §11's own definition of a numeric
// identifier: digits only. Fix round 3 item N3: strconv.Atoi alone would
// also accept a leading sign ("-5") as numeric, which semver does not —
// a pre-release identifier is either all-digits or it is alphanumeric,
// nothing in between, and it must never silently become "text" just
// because it overflows an int (handled in compareIdentifier below).
var numericIdentPattern = regexp.MustCompile(`^[0-9]+$`)

// compareIdentifier compares one pair of dot-separated pre-release
// identifiers per semver §11: numeric identifiers compare numerically
// (compareNumericIdent, sound even when one overflows an int);
// alphanumeric identifiers compare lexically (ASCII); and a numeric
// identifier always has LOWER precedence than an alphanumeric one at the
// same position.
func compareIdentifier(a, b string) int {
	aNum, bNum := numericIdentPattern.MatchString(a), numericIdentPattern.MatchString(b)
	switch {
	case aNum && bNum:
		return compareNumericIdent(a, b)
	case aNum && !bNum:
		return -1
	case !aNum && bNum:
		return 1
	default:
		if a == b {
			return 0
		}
		if a < b {
			return -1
		}
		return 1
	}
}

// compareNumericIdent compares two identifiers already known to match
// numericIdentPattern (digits only, so never negative and never with a
// leading sign to confuse strconv.Atoi). When both fit in an int, plain
// numeric comparison is exact. When either overflows, it is compared as
// "a longer numeric" instead (fix round 3 item N3's ruling): a longer
// all-digit string is always the larger number precisely because
// numericIdentPattern rules out leading zeros' one loophole — an
// all-digit string only satisfies ^[0-9]+$ with the digits it visibly
// has, so length alone orders any two such strings correctly regardless
// of how large either is, with a lexical comparison only needed to break
// a tie between two equal-length digit strings.
func compareNumericIdent(a, b string) int {
	if ai, aErr := strconv.Atoi(a); aErr == nil {
		if bi, bErr := strconv.Atoi(b); bErr == nil {
			switch {
			case ai < bi:
				return -1
			case ai > bi:
				return 1
			default:
				return 0
			}
		}
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// comparePrerelease implements semver §11's precedence rule for two
// pre-release identifier lists, compared identifier by identifier
// (compareIdentifier); when every compared identifier is equal, the
// shorter list has lower precedence.
func comparePrerelease(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdentifier(a[i], b[i]); c != 0 {
			return c
		}
	}
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return 0
}

// comparePrecedence returns -1, 0 or 1 as a is older than, equal to, or
// newer than b, by fix round 2 item I1's full ruling.
func comparePrecedence(a, b versionPrecedence) int {
	if a.major != b.major {
		if a.major < b.major {
			return -1
		}
		return 1
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return -1
		}
		return 1
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return -1
		}
		return 1
	}
	if a.kind != b.kind {
		if a.kind < b.kind {
			return -1
		}
		return 1
	}
	switch a.kind {
	case kindPreRelease:
		return comparePrerelease(a.preIdents, b.preIdents)
	case kindDescribe:
		if a.describeN != b.describeN {
			if a.describeN < b.describeN {
				return -1
			}
			return 1
		}
	}
	return 0
}

// --- prune --------------------------------------------------------------

type versionDir struct {
	name      string
	prec      versionPrecedence
	parseable bool
}

// sortVersionDirsNewestFirst orders dirs newest first, by the same full
// version order `update` itself uses for releaseNewer (parseVersionPrecedence
// / comparePrecedence) — never just a MAJOR.MINOR.PATCH comparison, which
// would treat every "0.3.0-N-g<hex>" describe build as merely equal to the
// bare 0.3.0 release it was cut from, and to every OTHER describe build on
// that same base, regardless of N (part 0 final review item 2). A dir whose
// name does not parse at all sorts oldest of all (spec §2.3 Prune).
func sortVersionDirsNewestFirst(dirs []versionDir) {
	sort.SliceStable(dirs, func(i, j int) bool {
		a, b := dirs[i], dirs[j]
		if a.parseable != b.parseable {
			return a.parseable
		}
		if !a.parseable {
			return false
		}
		return comparePrecedence(a.prec, b.prec) > 0
	})
}

// linkedVersionDir returns the version directory name binDir/chottag
// resolves into, or "" when it cannot be resolved (no such link yet). It
// does not itself insist the result lives under versionsDir: an unrelated
// or foreign resolution just yields a name pruneOldVersions's dirs slice
// never contains, so keep[name] is never consulted for it — harmless,
// and simpler than comparing paths through a symlink (e.g. macOS's
// /tmp -> /private/tmp), which a literal string comparison against
// versionsDir would get wrong for a perfectly ordinary case.
func linkedVersionDir(binDir string) string {
	resolved, err := filepath.EvalSymlinks(filepath.Join(binDir, "chottag"))
	if err != nil {
		return ""
	}
	return filepath.Base(filepath.Dir(resolved))
}

// pruneCandidates returns the versions/* directory names pruneOldVersions
// would remove for protect, WITHOUT removing anything — the same
// keep-newest-three-plus-linked-plus-protect rule (fix round 1 item 10),
// split out so runUpdate's daemon-version-unknown guard (fix round 2 item
// M1) can ask "would this have removed anything at all?" without
// actually touching disk (fix round 3 item N4): a "kept all versions"
// warning is only informative when skipping prune actually changed the
// outcome, never when there were three or fewer versions to begin with,
// which nothing would ever have pruned regardless of protect.
func pruneCandidates(h string, protect string) []string {
	versionsDir := filepath.Join(h, "versions")
	ents, err := os.ReadDir(versionsDir)
	if err != nil {
		return nil
	}
	var dirs []versionDir
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(versionsDir, e.Name(), "chottag")); err != nil {
			continue
		}
		prec, ok := parseVersionPrecedence(e.Name())
		dirs = append(dirs, versionDir{name: e.Name(), prec: prec, parseable: ok})
	}
	sortVersionDirsNewestFirst(dirs)
	keep := map[string]bool{}
	for i, d := range dirs {
		if i < 3 {
			keep[d.name] = true
		}
	}
	if protect != "" {
		keep[protect] = true
	}
	if linked := linkedVersionDir(filepath.Join(h, "bin")); linked != "" {
		keep[linked] = true
	}
	var candidates []string
	for _, d := range dirs {
		if !keep[d.name] {
			candidates = append(candidates, d.name)
		}
	}
	return candidates
}

// pruneOldVersions removes every dir pruneCandidates names for protect,
// via os.RemoveAll — guarded so the path removed is always a direct
// child of versionsDir. A removal failure is returned in failed rather
// than as an error: prune failing is a warning, never a reason to fail
// the whole update (spec §2.3).
func pruneOldVersions(h string, protect string) (pruned []string, failed []string) {
	versionsDir := filepath.Join(h, "versions")
	for _, name := range pruneCandidates(h, protect) {
		dir := filepath.Join(versionsDir, name)
		if filepath.Dir(dir) != versionsDir {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			failed = append(failed, name)
			continue
		}
		pruned = append(pruned, name)
	}
	return pruned, failed
}

// installSourceGuess reports the install.json "source" value this binary
// would get if nothing had been recorded yet: "release" when a
// versions/<Version>/chottag directory already exists (i.e. this binary is
// itself running from a release install already placed there), and "build"
// otherwise (a local go build, or a chottag home from before install.sh
// started writing install.json).
func installSourceGuess(h string) string {
	if _, err := os.Stat(filepath.Join(h, "versions", Version, "chottag")); err == nil {
		return "release"
	}
	return "build"
}

// writeRepoBack updates h's install.json so its repo is repo, on every
// non-"--check" exit-0 path once --repo was explicitly given (part 0 final
// review item 1): a person who ran `chottag update --repo X` expects the
// NEXT bare `chottag update` to keep using X, even when this particular run
// found nothing to install. Every other field an existing record already
// held (version, source, installedAt) is kept exactly as it was; when there
// is no existing record (or it is unreadable — already warned about
// earlier in runUpdate), a fresh one is written with this binary's own
// Version, installSourceGuess's source, and installedAt now. A write
// failure is an install_record warning, never a reason to fail an update
// that has already succeeded.
func writeRepoBack(h string, repo string, r *reporter) {
	rec, ok, err := readInstallRecord(h)
	if err != nil || !ok {
		rec = installRecord{Version: Version, Source: installSourceGuess(h), InstalledAt: time.Now().UTC()}
	}
	rec.Repo = repo
	if err := writeInstallRecord(h, rec); err != nil {
		r.Warn(warnInstallRecord, "chottag: could not write install.json: "+err.Error())
	}
}

// --- runUpdate ------------------------------------------------------------

// runUpdate is `chottag update [--check] [--version vX.Y.Z] [--repo
// OWNER/NAME] [--restart]` (spec §2.3).
func runUpdate(args []string, r *reporter) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(r.Stderr())
	check := fs.Bool("check", false, "report whether a newer release exists; install nothing")
	versionFlag := fs.String("version", "", "install this release tag instead of the latest, e.g. to roll back")
	repoFlag := fs.String("repo", "", "use this repo instead of install.json's or the default")
	restart := fs.Bool("restart", false, "restart the daemon even if a claude session is running")
	noRestart := fs.Bool("no-restart", false, "install, but leave the daemon running")
	autoCheck := fs.String("auto-check", "", "turn the daemon's update check on or off")
	autoInstall := fs.String("auto-install", "", "turn the daemon's automatic install of a new release on or off")
	autoRestart := fs.String("auto-restart", "", "turn the daemon's restart onto an installed update, when idle, on or off")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return r.FlagError(err)
	}
	if len(positional) != 0 {
		return r.Usage(updateUsage)
	}
	if *restart && *noRestart {
		msg := "--restart and --no-restart cannot be combined"
		fmt.Fprintf(r.Stderr(), "chottag: %s\n%s\n", msg, updateUsage)
		return r.FailNoText(exit.Usage, codeUsage, msg, nil)
	}
	setSwitches := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auto-check" || f.Name == "auto-install" || f.Name == "auto-restart" {
			setSwitches = true
		}
	})
	if setSwitches {
		return runUpdateSwitches(r, *autoCheck, *autoInstall, *autoRestart, *check, *versionFlag != "", *restart, *noRestart, fs)
	}
	if *repoFlag != "" && !validRepo(*repoFlag) {
		msg := fmt.Sprintf("--repo must look like OWNER/NAME, got %q", *repoFlag)
		fmt.Fprintf(r.Stderr(), "chottag: %s\n%s\n", msg, updateUsage)
		return r.FailNoText(exit.Usage, codeUsage, msg, nil)
	}
	usingExplicitVersion := *versionFlag != ""
	if usingExplicitVersion {
		_, fullOK := checkVersionTag(*versionFlag)
		if !versionFlagPrefixPattern.MatchString(*versionFlag) || !fullOK {
			msg := fmt.Sprintf("--version must look like vX.Y.Z, got %q", *versionFlag)
			fmt.Fprintf(r.Stderr(), "chottag: %s\n%s\n", msg, updateUsage)
			return r.FailNoText(exit.Usage, codeUsage, msg, nil)
		}
	}

	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}

	// A release before 0.8.0 does not read pools: refuse the rollback, before
	// any download, while extra pools exist (M8 spec §2). An unreadable
	// state.json does not block a rollback.
	if usingExplicitVersion && !*check {
		if ver, _ := checkVersionTag(*versionFlag); rollbackLosesPools(ver) {
			if st, err := (store.Store{Dir: h}).Load(); err == nil && len(st.Pools) > 0 {
				return r.Fail(exit.Usage, codePoolsBlockRollback, fmt.Sprintf(
					"%s predates pools, and this install has pools beyond default (%s): remove them first with `chottag pool rm`, then roll back",
					*versionFlag, strings.Join(st.PoolNames()[1:], ", ")), nil)
			}
		}
	}

	repo := resolveUpdateRepo(h, r)
	if *repoFlag != "" {
		repo = *repoFlag
	}

	ctx := context.Background()
	// --check asks GitHub's API directly (updateFetch) and needs no gh;
	// installing still does, for the download and the attestation.
	if !*check {
		if err := ghAuthCheck(ctx); err != nil {
			return r.Fail(exit.Error, codeUpdateFailed, err.Error(), nil)
		}
	}

	tag := *versionFlag
	if tag == "" && *check {
		upstream, err := updateUpstream(h)
		if err != nil {
			return r.Fail(exit.Error, codeUpdateFailed, err.Error(), nil)
		}
		rel, err := updateFetch(ctx, upstream, repo)
		// Only the install's own repo goes into the cache the daemon shares:
		// `--repo X` must not leave another repo's release in it.
		if repo == updateRepo(h) {
			recordUpdateCheck(h, rel, err, r)
			if err == nil {
				noticeUpdateFromCLI(h, rel, r)
			}
		}
		if err != nil {
			return r.Fail(exit.Error, codeUpdateFailed, err.Error(), nil)
		}
		tag = rel.Tag
	} else if tag == "" {
		tag, err = latestTag(ctx, repo)
		if err != nil {
			return r.Fail(exit.Error, codeUpdateFailed, err.Error(), nil)
		}
	}
	ver, ok := checkVersionTag(tag)
	if !ok {
		return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("release tag %q from %s does not look like a version", tag, repo), nil)
	}
	newer := releaseNewer(Version, ver)

	if *check {
		if newer {
			r.Text("update available: %s -> %s (%s)\n", Version, ver, repo)
		} else {
			r.Text("chottag %s is up to date (%s)\n", Version, repo)
		}
		res := updateResult{Repo: repo, Current: Version, Latest: ver, UpdateAvailable: newer}
		updateExtras(ctx, h, repo, ver, newer, &res, r)
		return r.OK(res)
	}

	versionsDir := filepath.Join(h, "versions")
	destDir := filepath.Join(versionsDir, ver)
	destBin := filepath.Join(destDir, "chottag")

	switch {
	case ver == Version:
		if _, err := os.Stat(destBin); err == nil {
			if *repoFlag != "" {
				writeRepoBack(h, repo, r)
			}
			r.Text("chottag %s is already up to date\n", Version)
			return r.OK(updateResult{Repo: repo, Current: Version, Latest: ver, UpdateAvailable: false})
		}
	case !usingExplicitVersion && !newer:
		// R82 ruling (fix round 1 item 13, refined by fix round 2 item
		// I1's suffix-aware ordering): a bare `chottag update` never goes
		// down. ver == Version (plain string equality) was just excluded
		// above; !newer here means ver's PRECEDENCE is not strictly
		// greater than Version's — it may be strictly older, or merely a
		// differently-spelled equal (e.g. "0.3.0-5-gabc1234" vs
		// "0.3.0-5-gabc1234-dirty" are distinct strings of equal rank) —
		// either way there is nothing to install without --version
		// forcing it.
		if *repoFlag != "" {
			writeRepoBack(h, repo, r)
		}
		r.Text("chottag %s is up to date (%s)\n", Version, repo)
		return r.OK(updateResult{Repo: repo, Current: Version, Latest: ver, UpdateAvailable: false})
	}

	// The whole install, from the download through setup, holds the update
	// lock, so a person's update and the daemon's never overlap (R124).
	if err := os.MkdirAll(filepath.Join(h, "run"), 0o700); err != nil {
		return r.FailErr(err)
	}
	unlock, locked, err := fsutil.TryLock(filepath.Join(h, "run", updateLockName))
	if err != nil {
		return r.FailErr(err)
	}
	if !locked {
		return r.Fail(exit.Error, codeUpdateInProgress, "another chottag update is running", nil)
	}
	defer unlock()

	// Download and verify, into a temp dir removed on return: nothing under
	// CHOTTAG_HOME is touched until the checksum has matched (spec §2.3
	// "Install" steps 1-3).
	tmp, err := os.MkdirTemp("", "chottag-update-")
	if err != nil {
		return r.FailErr(err)
	}
	defer os.RemoveAll(tmp)

	asset := fmt.Sprintf("chottag_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	if _, err := updateGH(ctx, "release", "download", "--repo", repo, "--pattern", asset, "--pattern", "checksums.txt", "--dir", tmp, "--", tag); err != nil {
		return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("could not download %s and checksums.txt from release %s: %s; nothing was installed", asset, tag, err), nil)
	}
	assetPath := filepath.Join(tmp, asset)
	want, err := checksumFor(filepath.Join(tmp, "checksums.txt"), asset)
	if err != nil {
		return r.Fail(exit.Error, codeUpdateFailed, err.Error()+"; nothing was installed", nil)
	}
	got, err := sha256File(assetPath)
	if err != nil {
		return r.Fail(exit.Error, codeUpdateFailed, "could not checksum "+assetPath+": "+err.Error(), nil)
	}
	if got != want {
		return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("checksum mismatch for %s (checksums.txt says %s, the download is %s); nothing was installed", asset, want, got), nil)
	}

	if baseBefore(ver, attestedSince) {
		if usingExplicitVersion {
			// An explicit rollback (fix round 1 item 3): a release this
			// old was never attested regardless of the repo's
			// visibility, so there is nothing to ask gh about.
			r.Warn(warnPreAttestation, fmt.Sprintf("chottag: %s predates attestations (v%s); verified by checksum only", tag, attestedSince))
		} else {
			// R82's "a bare `chottag update` never goes down" only
			// blocks a release OLDER than Version — it says nothing
			// about the LATEST release still predating attestedSince,
			// which is routine while Version itself predates it too, and
			// stays possible even once past it if Version fails to parse
			// at all: a "dev" build (this binary's own unstamped
			// default) never does, so releaseNewer("dev", x) is always
			// true (fix round 2 item 1). Mirror install.sh's own
			// refusal either way: fail closed on a public repo,
			// note-and-continue on a private one (which never gets
			// attestations regardless).
			private, err := repoView(ctx, repo)
			if err != nil {
				return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("the latest release of %s, %s, predates attestations (v%s), and %s; nothing was installed", repo, tag, attestedSince, err), nil)
			}
			if !private {
				return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("the latest release of %s, %s, predates attestations (v%s); nothing was installed. Pass --version %s to install it explicitly.", repo, tag, attestedSince, tag), nil)
			}
			r.Warn(warnPreAttestation, fmt.Sprintf("chottag: %s predates attestations (v%s); verified by checksum only", tag, attestedSince))
		}
	} else if need, err := attestationRequired(ctx, repo, ver); err != nil {
		return r.Fail(exit.Error, codeUpdateFailed, err.Error()+"; nothing was installed", nil)
	} else if need {
		if err := verifyAttestation(ctx, repo, assetPath); err != nil {
			return r.Fail(exit.Error, codeUpdateFailed, err.Error()+"; nothing was installed", nil)
		}
		r.Text("verified the build attestation of %s (%s)\n", asset, repo)
	} else {
		r.Warn(warnAttestationSkipped, fmt.Sprintf("chottag: %s is private: GitHub attests public repositories only, so %s is verified by checksum only", repo, tag))
	}

	// R123: state.json is backed up before anything is extracted or the new
	// binary's setup (and any migration it runs) can touch it, but after the
	// download is verified. A failed backup stops here.
	var backups []string
	stateBackup, err := backupFile(h, filepath.Join(h, "state.json"), "state.json")
	var pe *backupPruneError
	if errors.As(err, &pe) {
		r.Warn(warnPruneFailed, "chottag: "+pe.Error())
		err = nil
	}
	if err != nil {
		return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("could not back up state.json: %s; nothing was installed", err), nil)
	}
	if stateBackup != "" {
		backups = append(backups, stateBackup)
		r.Text("backed up state.json to %s\n", stateBackup)
	}

	if err := extractChottag(assetPath, destDir, destBin); err != nil {
		return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf("%s in %s; nothing was installed", err, asset), nil)
	}

	if err := updateChild(ctx, destBin, "setup"); err != nil {
		// Fix round 1 item 2: don't claim the OLD bin/chottag link is
		// intact — setup relinks it before doing anything else that could
		// fail, so a failure here may already have pointed it at the new,
		// verified binary.
		return r.Fail(exit.Error, codeUpdateFailed, fmt.Sprintf(
			"%s setup failed: %s; the new binary is in place at %s, but setup may already have relinked bin/chottag to it before failing — run %s setup again to finish",
			destBin, err, destBin, destBin), nil)
	}

	if err := writeInstallRecord(h, installRecord{Repo: repo, Version: ver, Source: "release", InstalledAt: time.Now().UTC()}); err != nil {
		r.Warn(warnInstallRecord, "chottag: could not write install.json: "+err.Error())
	}

	r.Text("installed chottag %s from release %s (%s)\n", ver, tag, repo)

	// Probe once (fix round 1 item 10): the result both protects the
	// running daemon's own version from prune below and decides the
	// daemon block that follows, so the daemon is never probed twice.
	port, portErr := daemonPort(h)
	running := false
	daemonVer := ""
	if portErr == nil {
		running, daemonVer = updateProbe(port)
	}
	// daemonVersionUnknown covers two cases prune must treat alike (fix
	// round 2 item M1, the same conservative shape as item 12's unknown
	// live-session count): daemonPort itself failed, so it is not even
	// known WHETHER a daemon is running, let alone which version — or it
	// answered "running" but with a version string that does not parse,
	// which is just as unusable as not having one. Either way, protect
	// stays "" and is never trusted as a keep name; prune itself is
	// skipped below rather than risk removing the version an
	// unidentifiable running daemon still needs.
	daemonVersionUnknown := portErr != nil
	protect := ""
	if running {
		if v, ok := checkVersionTag(daemonVer); ok {
			protect = v
		} else {
			daemonVersionUnknown = true
		}
	}

	var pruned, failedPrune []string
	if daemonVersionUnknown {
		// Fix round 3 item N4: only worth a warning when skipping prune
		// actually changed the outcome — pruneCandidates(h, "") is the
		// worst case (protect unknown, so nothing is protected beyond the
		// newest three and the linked one), and if even THAT would remove
		// nothing, no real protect value could have made a difference
		// either: protect only ever keeps one more name, never fewer.
		if len(pruneCandidates(h, "")) != 0 {
			r.Warn(warnPruneFailed, "chottag: kept all versions: the daemon's version is unknown")
		}
	} else {
		pruned, failedPrune = pruneOldVersions(h, protect)
		if len(failedPrune) != 0 {
			r.Warn(warnPruneFailed, "chottag: could not remove old version(s): "+strings.Join(failedPrune, ", "))
		}
	}
	r.Text("pruned %d old version(s)\n", len(pruned))

	res := updateResult{Repo: repo, Current: Version, Latest: ver, UpdateAvailable: newer, Installed: true, Pruned: pruned, Backups: backups}

	// done ends a successful install: the daemon's outcome is already
	// printed, then what's new and the plugin step (R156, R157).
	done := func(res updateResult) int {
		updateExtras(ctx, h, repo, ver, newer, &res, r)
		return r.OK(res)
	}

	if *noRestart {
		// --no-restart (R124): the daemon is left alone on every path, a
		// running one or none, with sessions or without.
		res.Daemon = "not-restarted"
		r.Text("daemon not restarted (--no-restart)\n")
		return done(res)
	}
	if portErr != nil {
		// Fix round 1 item 11: the install itself already succeeded, so
		// this is a warning, not a failure — nothing here undoes the
		// install, it only leaves the daemon's fate to the operator.
		res.Daemon = "not-probed"
		r.Warn(warnRestartFailed, "chottag: could not tell whether the daemon needs restarting: "+portErr.Error()+"; run: chottag daemon restart if it is running")
		return done(res)
	}
	if !running {
		res.Daemon = "not-running"
		r.Text("daemon not running\n")
		return done(res)
	}

	sessions, sessErr := liveSessions(h)
	sessionsUnknown := sessErr != nil
	if sessErr == nil {
		res.LiveSessions = len(sessions)
	}
	live := sessionsUnknown || len(sessions) > 0

	if !live || *restart {
		if err := updateChild(ctx, destBin, "daemon", "restart"); err != nil {
			// Fix (part 0 final review item 3): don't tell the person to
			// rerun the very command that just failed — the child's own
			// stderr (already on this process's own stderr, via
			// execUpdateChild) already names the actual supervisor command
			// to use. The new binary is in place either way; it starts on
			// its own the next time the daemon starts.
			res.Daemon = "restart-failed"
			r.Warn(warnRestartFailed, "chottag: daemon restart failed (see the message above); the new version starts with the next daemon start")
		} else {
			res.Daemon = "restarted"
			r.Text("daemon restarted\n")
		}
		return done(res)
	}

	// Fix round 1 item 12: one line, the way `daemon restart`'s own
	// live-sessions notice does (TextWarn: printed on stdout in text mode,
	// recorded as a warning in both) — not a separate Text and Warn line
	// as before. An unreadable session registry says so plainly rather
	// than claiming "0 session(s)".
	res.Daemon = "deferred"
	sessionsText := fmt.Sprintf("%d session(s)", res.LiveSessions)
	if sessionsUnknown {
		sessionsText = "session count unknown"
	}
	// R129: say plainly whether the restart happens on its own, so a person
	// or an agent knows to finish the update with `chottag daemon restart`.
	st, stErr := (store.Store{Dir: h}).Load()
	restartOn := stErr == nil && st.RestartOn()
	self := restartOn && daemonRestartsItselfOnto(daemonVer, ver)
	res.SelfRestart = &self
	const finish = "run chottag daemon restart once when convenient"
	var how string
	switch {
	case self:
		// R126: the daemon restarts itself once idle.
		how = "the daemon restarts itself when idle, or run: chottag daemon restart"
	case updatecheck.Parses(daemonVer) && updatecheck.Newer(restartLoopSince, daemonVer):
		how = fmt.Sprintf("the running daemon (%s) can't restart itself: %s", daemonVer, finish)
	case updatecheck.Parses(daemonVer) && !updatecheck.Newer(ver, daemonVer):
		how = fmt.Sprintf("the running daemon (%s) is newer than %s and won't restart onto it: %s", daemonVer, ver, finish)
	case !restartOn && updatecheck.Parses(daemonVer):
		how = fmt.Sprintf("auto-restart is off, so the daemon keeps running %s: %s", daemonVer, finish)
	default:
		how = "the running daemon can't restart itself: " + finish
	}
	r.TextWarn(warnUpdateDeferred, fmt.Sprintf("chottag: daemon restart deferred: %s running; %s", sessionsText, how))
	return done(res)
}

// restartLoopSince is the first release whose daemon restarts itself when idle.
const restartLoopSince = "0.6.0"

// daemonRestartsItselfOnto is whether a running daemon of version daemonVer
// will restart itself onto the installed ver: it has the loop (0.6.0 or
// later; a dev build or an unparseable version has none) and ver is newer.
func daemonRestartsItselfOnto(daemonVer, ver string) bool {
	if !updatecheck.Parses(daemonVer) || updatecheck.Newer(restartLoopSince, daemonVer) {
		return false
	}
	return updatecheck.Newer(ver, daemonVer)
}

// recordUpdateCheck writes the outcome of a check into status.json's
// `update`, keeping the daemon's own notified and auto fields. On a failed
// fetch it records the error and keeps the last latest. A cache that cannot
// be written is a warning, never a reason to fail the check.
func recordUpdateCheck(h string, rel updatecheck.Release, fetchErr error, r *reporter) {
	path := status.Path(h)
	f, err := status.Load(path)
	if err != nil {
		r.Warn(warnUpdateCache, "chottag: could not read status.json to record the check: "+err.Error())
		return
	}
	u := status.Update{}
	if f.Update != nil {
		u = *f.Update
	}
	u.CheckedAt = time.Now().UTC()
	if fetchErr != nil {
		u.Error = fetchErr.Error()
	} else {
		u.Error = ""
		u.Latest = rel.Version
		u.PublishedAt = rel.PublishedAt
		inst, _ := installedVersion(h) // unknown: only the running version counts
		u.Available = releaseAvailable(rel, Version, inst)
	}
	f.Update = &u
	b, err := status.Marshal(f)
	if err == nil {
		err = status.WriteBytes(path, b)
	}
	if err != nil {
		r.Warn(warnUpdateCache, "chottag: could not record the check in status.json: "+err.Error())
	}
}

// parseOnOff parses an `on` or `off` flag value.
func parseOnOff(v string) (on bool, ok bool) {
	switch v {
	case "on":
		return true, true
	case "off":
		return false, true
	}
	return false, false
}

// updateSwitches is the `updates` object of `update --auto-check ...`.
type updateSwitches struct {
	Check   bool `json:"check"`
	Auto    bool `json:"auto"`
	Restart bool `json:"restart"`
}

// updateSwitchesOf reads the three switches from st.
func updateSwitchesOf(st store.State) updateSwitches {
	return updateSwitches{Check: st.UpdateCheckOn(), Auto: st.AutoUpdateOn(), Restart: st.RestartOn()}
}

// runUpdateSwitches is `update --auto-check on|off`, `--auto-install on|off`
// and `--auto-restart on|off`: set the switch in state.json, print both, exit. Auto-install
// needs the check, so turning the check off turns auto-install off too, and
// turning auto-install on turns the check on.
func runUpdateSwitches(r *reporter, autoCheck, autoInstall, autoRestart string, check, explicitVersion, restart, noRestart bool, fs *flag.FlagSet) int {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	usageErr := func(msg string) int {
		fmt.Fprintf(r.Stderr(), "chottag: %s\n%s\n", msg, updateUsage)
		return r.FailNoText(exit.Usage, codeUsage, msg, nil)
	}
	if check || explicitVersion || restart || noRestart || given["repo"] {
		return usageErr("--auto-check, --auto-install and --auto-restart set a switch and exit: they cannot be combined with --check, --version, --repo, --restart or --no-restart")
	}
	var checkOn, installOn, haveCheck, haveInstall bool
	if given["auto-check"] {
		var ok bool
		if checkOn, ok = parseOnOff(autoCheck); !ok {
			return usageErr(fmt.Sprintf("--auto-check takes on or off, got %q", autoCheck))
		}
		haveCheck = true
	}
	if given["auto-install"] {
		var ok bool
		if installOn, ok = parseOnOff(autoInstall); !ok {
			return usageErr(fmt.Sprintf("--auto-install takes on or off, got %q", autoInstall))
		}
		haveInstall = true
	}
	var restartOn, haveRestart bool
	if given["auto-restart"] {
		var ok bool
		if restartOn, ok = parseOnOff(autoRestart); !ok {
			return usageErr(fmt.Sprintf("--auto-restart takes on or off, got %q", autoRestart))
		}
		haveRestart = true
	}
	if haveCheck && haveInstall && !checkOn && installOn {
		return usageErr("--auto-check off and --auto-install on contradict each other: auto-install needs the check")
	}
	h, err := home()
	if err != nil {
		return r.FailErr(err)
	}
	st, err := (store.Store{Dir: h}).Update(func(st *store.State) error {
		if haveCheck {
			st.SetUpdateCheck(checkOn)
			if !checkOn {
				st.SetAutoUpdate(false)
			}
		}
		if haveInstall {
			st.SetAutoUpdate(installOn)
		}
		if haveRestart {
			st.SetAutoRestart(restartOn)
		}
		return nil
	})
	if err != nil {
		return r.FailErr(err)
	}
	onWord := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	r.Text("update check: %s; auto-install: %s; auto-restart: %s\n", onWord(st.UpdateCheckOn()), onWord(st.AutoUpdateOn()), onWord(st.RestartOn()))
	return r.OK(struct {
		Updates updateSwitches `json:"updates"`
	}{updateSwitchesOf(st)})
}
