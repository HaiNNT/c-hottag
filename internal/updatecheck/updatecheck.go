// Package updatecheck fetches the latest GitHub release and compares
// versions. It makes one HTTP GET with the caller's client and never
// installs anything.
package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// APIBase is the GitHub REST API root.
const APIBase = "https://api.github.com"

// maxBody caps how much of a response is read.
const maxBody = 1 << 20

// Release is the part of a GitHub release the update check needs.
type Release struct {
	Tag         string // as published, e.g. "v0.6.0"
	Version     string // Tag without a leading "v"
	PublishedAt time.Time
	Draft       bool
	Prerelease  bool
}

// Latest fetches {apiBase}/repos/{repo}/releases/latest. A non-200 status,
// a body over 1 MiB, a body that is not the expected JSON and a release
// with no tag are all errors.
func Latest(ctx context.Context, c *http.Client, apiBase, repo string) (Release, error) {
	url := apiBase + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, fmt.Errorf("updatecheck: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("updatecheck: fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("updatecheck: latest release: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return Release{}, fmt.Errorf("updatecheck: read latest release: %w", err)
	}
	if len(body) > maxBody {
		return Release{}, fmt.Errorf("updatecheck: latest release response exceeds 1 MiB")
	}
	var raw struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Release{}, fmt.Errorf("updatecheck: decode latest release: %w", err)
	}
	if raw.TagName == "" {
		return Release{}, fmt.Errorf("updatecheck: latest release has no tag_name")
	}
	return Release{
		Tag:         raw.TagName,
		Version:     strings.TrimPrefix(raw.TagName, "v"),
		PublishedAt: raw.PublishedAt,
		Draft:       raw.Draft,
		Prerelease:  raw.Prerelease,
	}, nil
}

// Parses reports whether v is a release version (MAJOR.MINOR.PATCH with an
// optional suffix). A dev build's "dev" is not.
func Parses(v string) bool {
	_, ok := parseVersionPrecedence(v)
	return ok
}

// IsPrerelease reports whether v carries a semver pre-release suffix
// (0.6.0-rc.1). A describe suffix (0.6.0-3-gabc, a build after 0.6.0) and
// a version that does not parse are not.
func IsPrerelease(v string) bool {
	p, ok := parseVersionPrecedence(v)
	return ok && p.kind == kindPreRelease
}

// Newer reports whether candidate is strictly newer than current. It is
// false when either does not parse, so a dev build never has a newer
// release.
func Newer(candidate, current string) bool {
	pc, ok := parseVersionPrecedence(candidate)
	if !ok {
		return false
	}
	pr, ok := parseVersionPrecedence(current)
	if !ok {
		return false
	}
	return comparePrecedence(pr, pc) < 0
}

// SameMajor reports whether a and b share a major version. It is false when
// either does not parse.
func SameMajor(a, b string) bool {
	pa, ok := parseVersionPrecedence(a)
	if !ok {
		return false
	}
	pb, ok := parseVersionPrecedence(b)
	if !ok {
		return false
	}
	return pa.major == pb.major
}

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

// describeSuffixPattern matches a git-describe suffix, "-<N>-g<hex>",
// optionally followed by "-dirty", with the leading "v" and the base
// already stripped.
var describeSuffixPattern = regexp.MustCompile(`^([0-9]+)-g[0-9a-f]+(-dirty)?$`)

// bareDirtySuffix is `git describe --dirty` run exactly at a tag. It is a
// describe suffix (N=0), not a pre-release: a dirty build at v0.3.0 ranks
// after the bare 0.3.0 release.
const bareDirtySuffix = "dirty"

// suffixKind ranks a version's suffix relative to its bare base: a
// pre-release sorts before the base, the bare base itself, and a describe
// suffix after it. Declared in that order so an int comparison is the
// ranking.
type suffixKind int

const (
	kindPreRelease suffixKind = iota
	kindBare
	kindDescribe
)

type versionPrecedence struct {
	major, minor, patch int
	kind                suffixKind
	describeN           int      // valid only when kind == kindDescribe
	preIdents           []string // valid only when kind == kindPreRelease
}

// parseVersionPrecedence parses s into its full ordering. A leading "v" is
// stripped and "+build" metadata ignored (semver section 10). ok is false
// when the MAJOR.MINOR.PATCH base fails to parse.
func parseVersionPrecedence(s string) (versionPrecedence, bool) {
	s = strings.TrimPrefix(s, "v")
	core, _, _ := strings.Cut(s, "+")
	base, suffix, hasSuffix := strings.Cut(core, "-")
	major, minor, patch, ok := parseBaseSemver(base)
	if !ok {
		return versionPrecedence{}, false
	}
	if !hasSuffix {
		return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindBare}, true
	}
	if suffix == bareDirtySuffix {
		return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindDescribe, describeN: 0}, true
	}
	if m := describeSuffixPattern.FindStringSubmatch(suffix); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return versionPrecedence{}, false // fail closed on an unprecedented int
		}
		return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindDescribe, describeN: n}, true
	}
	return versionPrecedence{major: major, minor: minor, patch: patch, kind: kindPreRelease, preIdents: strings.Split(suffix, ".")}, true
}

// numericIdentPattern is semver section 11's numeric identifier: digits
// only (strconv.Atoi alone would accept a sign).
var numericIdentPattern = regexp.MustCompile(`^[0-9]+$`)

// compareIdentifier compares one pair of pre-release identifiers: numeric
// ones numerically, alphanumeric ones lexically, and a numeric one below an
// alphanumeric one.
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

// compareNumericIdent compares two all-digit identifiers, exact even when
// one overflows an int: a longer all-digit string is the larger number,
// and equal lengths compare lexically.
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

// comparePrerelease compares two identifier lists identifier by identifier;
// when all compared are equal, the shorter list is lower.
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
// newer than b.
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
