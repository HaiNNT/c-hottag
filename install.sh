#!/bin/sh
# install.sh: install chottag (c-hottag) for the current user.
#
#   ./install.sh [--version vX.Y.Z] [--repo OWNER/NAME]   from a clone
#   gh api -H 'Accept: application/vnd.github.raw' \
#       repos/HaiNNT/c-hottag/contents/install.sh | sh
#   (pipe into `sh -s -- --version vX.Y.Z --repo OWNER/NAME` to pick a
#   release or a fork)
#
# The binary comes from, first that works:
#   (a) a GitHub release, through an authenticated gh, checked against the
#       release's checksums.txt (a mismatch aborts) and, for a public repo
#       from v0.4.0 on, its build attestation (`gh attestation verify`);
#   (b) `go build` of the clone this script sits in (not with --version).
# It lands in ${CHOTTAG_HOME:-~/.chottag}/versions/<version>/chottag, and
# then `chottag setup` links bin/chottag and bin/claude to it and adds the
# PATH block to your shell rc. Old versions are kept.
#
# Never uses sudo. Never touches ~/.claude or ~/.claude.json.
# POSIX sh. Everything runs inside main, so a truncated download runs nothing.

set -eu

REPO=HaiNNT/c-hottag
MODULE=github.com/HaiNNT/c-hottag
# ATTESTED_SINCE must equal internal/cli/update.go's attestedSince
# (TestAttestedSinceMatchesInstallSh ties them).
ATTESTED_SINCE=0.4.0

say() { printf '%s\n' "$*"; }
die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}
usage_error() {
	printf 'install.sh: %s\nusage: install.sh [--version vX.Y.Z] [--repo OWNER/NAME]\n' "$*" >&2
	exit 2
}
have() { command -v "$1" >/dev/null 2>&1; }

# base_before VER SINCE: true only when both VER's and SINCE's bases
# (MAJOR.MINOR.PATCH, any -pre or +build suffix ignored) are all-digit
# dot-separated triples AND VER's is strictly less than SINCE's.
# Fails CLOSED on anything else - an unparseable base (a "nightly" or
# "V9.0.0" tag), or awk missing or erroring - by returning false, i.e.
# "not before", which is what makes the caller verify (fix round 1 item
# 1: the previous base_at_least treated any unparseable version as
# already AT the cut-over, which skipped verification entirely for
# exactly the tags most worth checking).
base_before() {
	awk -v a="$1" -v b="$2" '
	function is_base(v) { return v ~ /^[0-9]+\.[0-9]+\.[0-9]+$/ }
	BEGIN {
		split(a, ax, /[-+]/); abase = ax[1]
		split(b, bx, /[-+]/); bbase = bx[1]
		if (!is_base(abase) || !is_base(bbase)) exit 1
		split(abase, p, "."); split(bbase, q, ".")
		for (i = 1; i <= 3; i++) {
			if (p[i] + 0 < q[i] + 0) exit 0
			if (p[i] + 0 > q[i] + 0) exit 1
		}
		exit 1
	}'
}

# is_digits VALUE: true iff VALUE is one or more decimal digits.
is_digits() {
	case $1 in
	'' | *[!0-9]*) return 1 ;;
	esac
}

# check_version_tag TAG: true only when TAG matches update.go's own
# versionTagPattern shape (a case-pattern mirror): an optional leading v,
# then MAJOR.MINOR.PATCH as exactly three all-digit fields separated by
# exactly two dots - no leading, trailing or doubled dot, and never a
# fourth field. Any -pre or +build suffix after that is left uninspected
# here - check_version, run on the same string right after, already
# rejects any unsafe character in it - except that the suffix marker
# itself ("-" or "+") must have at least one character after it (fix
# round 3 item 2: "v1.2.3-" is rejected, matching versionTagPattern's own
# `[-+][A-Za-z0-9.-]+` requiring one or more). Built from parameter
# expansion and case only, never `set -- $x` (fix round 2 item 2): that
# word-splits AND globs its unquoted argument, so a base holding a
# bracket expression (e.g. a mistyped "1.2.[3]") could match a file in
# the current directory instead of simply failing to parse. A tag
# failing this check - a moved branch's tip, a nightly channel - must
# never reach try_release's attestation logic disguised as a version
# (fix round 1 item 1).
check_version_tag() {
	case $1 in
	*[-+]) return 1 ;; # a "-" or "+" suffix marker with nothing after it
	esac
	vbase=${1%%[-+]*}
	vbase=${vbase#v}
	case $vbase in
	*.*.*) ;;
	*) return 1 ;;
	esac
	major=${vbase%%.*}
	rest=${vbase#*.}
	minor=${rest%%.*}
	patch=${rest#*.}
	case $patch in
	*.*) return 1 ;; # a fourth field, or a trailing dot leaving one empty
	esac
	is_digits "$major" && is_digits "$minor" && is_digits "$patch"
}

# check_version refuses a version string that is unsafe as a directory name,
# or that could be mistaken for a flag by something it is later passed to.
check_version() {
	case $1 in
	'' | -* | .* | *[!A-Za-z0-9._+-]*) die "refusing the version string '$1'" ;;
	esac
}

# check_repo enforces ^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$ on a --repo value: an
# OWNER and a NAME, exactly one slash apart, each restricted to the
# characters GitHub allows there. POSIX sh has no regex tool, so this is
# done with case patterns instead.
check_repo() {
	case $1 in
	*/*) ;;
	*) usage_error "--repo must be OWNER/NAME, got '$1'" ;;
	esac
	case $1 in
	*/*/*) usage_error "--repo must be OWNER/NAME, got '$1'" ;;
	esac
	owner=${1%%/*}
	name=${1#*/}
	case $owner in
	'' | *[!A-Za-z0-9-]*) usage_error "--repo must be OWNER/NAME, got '$1'" ;;
	esac
	case $name in
	'' | *[!A-Za-z0-9._-]*) usage_error "--repo must be OWNER/NAME, got '$1'" ;;
	esac
}

# gh_reason prints up to ~200 chars of FILE (gh's captured stderr), for a
# "release not used" note. gh's stderr for release commands never carries a
# token, so this is safe to show; the cap just keeps incidental noise short.
gh_reason() {
	[ -s "$1" ] || return 0
	head -c 200 "$1"
}

# sha256_of prints FILE's SHA-256 in hex.
sha256_of() {
	if have shasum; then
		shasum -a 256 "$1" </dev/null | cut -d ' ' -f 1
	elif have sha256sum; then
		sha256sum "$1" </dev/null | cut -d ' ' -f 1
	else
		die "neither shasum nor sha256sum is installed, so the download cannot be verified; nothing was installed"
	fi
}

# find_clone sets src to this script's directory when that is a clone of
# the repo. Piped into sh, $0 is the shell, never install.sh, so the
# working directory is never mistaken for a clone.
find_clone() {
	src=""
	case $0 in
	install.sh | */install.sh) ;;
	*) return 0 ;;
	esac
	dir=$(cd "$(dirname "$0")" && pwd) || return 0
	if [ -f "$dir/go.mod" ] && [ -d "$dir/cmd/chottag" ] &&
		[ "$(head -n 1 "$dir/go.mod")" = "module $MODULE" ]; then
		src=$dir
	fi
}

# repo_view sets priv to gh's isPrivate answer ("true" or "false") for
# $repo, in $tag's context (used in its error messages), after checking
# $repo has not been renamed case-insensitively (fix round 1 item 2): a
# moved repo fails closed naming its new name, rather than silently
# asking gh about - or later attesting against - whatever $repo used to
# be. The answer must hold exactly one tab (fix round 2 item 3): anything
# else - zero, or more than one - fails closed as simply "unexpected",
# never as a (possibly misleading) "moved to" built from a field cut out
# of a malformed answer.
repo_view() {
	info=$(gh repo view "$repo" --json isPrivate,nameWithOwner --jq '[.isPrivate,.nameWithOwner]|@tsv' 2>"$tmp/gh-repo.err" </dev/null) ||
		die "could not tell whether $repo is public, so release $tag cannot be checked for its attestation: $(gh_reason "$tmp/gh-repo.err"); nothing was installed"
	tab=$(printf '\t')
	case "$info" in
	*"$tab"*"$tab"*) die "unexpected answer from gh repo view $repo; nothing was installed" ;;
	*"$tab"*) ;;
	*) die "unexpected answer from gh repo view $repo; nothing was installed" ;;
	esac
	priv=$(printf '%s' "$info" | cut -f1)
	owner_now=$(printf '%s' "$info" | cut -f2)
	same=$(awk -v a="$repo" -v b="$owner_now" 'BEGIN { print (tolower(a) == tolower(b)) ? "yes" : "no" }')
	[ "$same" = yes ] || die "$repo moved to $owner_now; rerun with --repo $owner_now; nothing was installed"
}

# try_release sets built, ver and from when a release binary was downloaded
# and verified. When no release can be used it sets why_release and returns
# 0. These functions report through variables, not return codes: set -e is
# off inside a function called as an if/||/&& condition. A failed check is
# always fatal, never a reason to try the next source.
try_release() {
	if ! have gh; then
		why_release="gh is not installed"
		return 0
	fi
	if ! gh auth status >/dev/null 2>&1 </dev/null; then
		why_release="gh is not logged in (run: gh auth login)"
		return 0
	fi
	tag=$want_tag
	if [ -z "$tag" ]; then
		tag=$(gh release view --repo "$repo" --json tagName --jq .tagName 2>"$tmp/gh-view.err" </dev/null) || tag=""
		if [ -z "$tag" ]; then
			reason=$(gh_reason "$tmp/gh-view.err")
			why_release="gh sees no release of $repo${reason:+: $reason}"
			return 0
		fi
	fi
	# A failed check is always fatal here, never a reason to try the next
	# source (this function's own doc comment, fix round 2 item 4): a
	# malformed tag from gh - a moved "latest" pointer, a corrupted
	# release list - must never quietly let main() fall back to building
	# the clone, which would mask exactly that.
	check_version_tag "$tag" || die "release tag $tag from $repo does not look like a version; nothing was installed"
	rel_ver=${tag#v}
	check_version "$rel_ver"
	ver=$rel_ver
	asset="chottag_${ver}_${os}_${arch}.tar.gz"
	# -- guards $tag from ever being mistaken for a flag by gh, whatever it
	# happens to contain; every flag comes before it, per gh's own (and
	# getopt's) convention for --.
	if ! gh release download --repo "$repo" --pattern "$asset" --pattern checksums.txt --dir "$tmp/dl" -- "$tag" >/dev/null 2>"$tmp/gh-dl.err" </dev/null; then
		reason=$(gh_reason "$tmp/gh-dl.err")
		why_release="could not download $asset and checksums.txt from release $tag${reason:+: $reason}"
		ver=""
		return 0
	fi
	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/dl/checksums.txt")
	[ -n "$want" ] || die "checksums.txt of release $tag has no line for $asset; nothing was installed"
	got=$(sha256_of "$tmp/dl/$asset")
	[ "$got" = "$want" ] || die "checksum mismatch for $asset (checksums.txt says $want, the download is $got); nothing was installed"
	if base_before "$ver" "$ATTESTED_SINCE"; then
		if [ -n "$want_tag" ]; then
			# An explicit rollback (fix round 1 item 3): a release this
			# old was never attested regardless of the repo's
			# visibility, so there is nothing to ask gh about.
			say "note: $tag predates attestations (v$ATTESTED_SINCE); verified by checksum only"
		else
			# The LATEST release predating the cut-over, on a PUBLIC
			# repo, is suspicious - a moved "latest" pointer, or a stale
			# release list - and must never install silently unverified
			# (fix round 1 item 3); a private repo (which never gets
			# attestations either way) gets the same checksum-only note
			# an explicit rollback does.
			repo_view
			case $priv in
			false) die "the latest release of $repo, $tag, predates attestations (v$ATTESTED_SINCE); nothing was installed. Pass --version $tag to install it explicitly." ;;
			true) say "note: $tag predates attestations (v$ATTESTED_SINCE); verified by checksum only" ;;
			*) die "gh repo view $repo gave an unexpected answer; nothing was installed" ;;
			esac
		fi
	else
		repo_view
		case $priv in
		false)
			gh attestation verify --help >/dev/null 2>"$tmp/gh-help.err" </dev/null ||
				die "gh has no attestation command (or it failed): $(gh_reason "$tmp/gh-help.err"); upgrade gh (2.49 or later) and rerun; nothing was installed"
			gh attestation verify "$tmp/dl/$asset" --repo "$repo" >/dev/null 2>"$tmp/gh-att.err" </dev/null ||
				die "the build attestation of $asset does not verify against $repo: $(gh_reason "$tmp/gh-att.err"); nothing was installed"
			say "verified the build attestation of $asset ($repo)"
			;;
		true) say "note: $repo is private, so release $tag carries no attestation; verified by checksum only" ;;
		*) die "gh repo view $repo gave an unexpected answer; nothing was installed" ;;
		esac
	fi
	mkdir "$tmp/x"
	tar -xzf "$tmp/dl/$asset" -C "$tmp/x" chottag </dev/null || die "could not unpack chottag from $asset; nothing was installed"
	[ -f "$tmp/x/chottag" ] || die "$asset holds no chottag binary; nothing was installed"
	built=$tmp/x/chottag
	from="release $tag"
	kind=release
}

# try_build sets built, ver and from after building the clone. When it
# cannot build it sets why_build and returns 0.
try_build() {
	if [ -z "$src" ]; then
		why_build="this script is not running from a clone of $repo"
		return 0
	fi
	if [ -n "$want_tag" ]; then
		why_build="--version $want_tag needs that release; a clone builds only its own checkout"
		return 0
	fi
	if ! have go; then
		why_build="go is not installed"
		return 0
	fi
	desc=""
	if have git; then
		desc=$(git -C "$src" describe --tags --always --dirty 2>/dev/null </dev/null) || desc=""
	fi
	ver=${desc#v}
	[ -n "$ver" ] || ver=dev
	check_version "$ver"
	mkdir "$tmp/build"
	(cd "$src" && GOFLAGS='' CGO_ENABLED=0 go build -trimpath \
		-ldflags "-X $MODULE/internal/cli.Version=$ver" \
		-o "$tmp/build/chottag" ./cmd/chottag </dev/null) ||
		die "go build failed in $src; nothing was installed"
	built=$tmp/build/chottag
	from="a Go build of $src"
	kind=build
}

# write_record writes $chottag_home/install.json atomically, mode 0600.
# Called with "release" or "build": whichever path produced $built. $repo
# and $ver are already validated to JSON-safe characters, so printf needs
# no escaping.
write_record() {
	rec=$chottag_home/install.json
	now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	(umask 077 && printf '{"repo":"%s","version":"%s","source":"%s","installedAt":"%s"}\n' \
		"$repo" "$ver" "$1" "$now" >"$rec.tmp.$$") || die "could not write $rec"
	mv -f "$rec.tmp.$$" "$rec" || {
		rm -f "$rec.tmp.$$"
		die "could not write $rec"
	}
}

main() {
	want_tag=""
	version_given=""
	repo_arg=""
	while [ $# -gt 0 ]; do
		case $1 in
		--version)
			[ $# -ge 2 ] || usage_error "--version needs a value, like v0.3.0"
			want_tag=$2
			version_given=1
			shift 2
			;;
		--version=*)
			want_tag=${1#--version=}
			version_given=1
			shift
			;;
		--repo)
			[ $# -ge 2 ] || usage_error "--repo needs a value, like OWNER/NAME"
			check_repo "$2"
			repo_arg=$2
			shift 2
			;;
		--repo=*)
			repo_arg=${1#--repo=}
			check_repo "$repo_arg"
			shift
			;;
		-h | --help)
			say "usage: install.sh [--version vX.Y.Z] [--repo OWNER/NAME]"
			exit 0
			;;
		*) usage_error "unknown argument: $1" ;;
		esac
	done
	if [ -n "$version_given" ] && [ -z "$want_tag" ]; then
		usage_error "--version needs a value, like v0.3.0"
	fi
	if [ -n "$want_tag" ]; then
		case $want_tag in
		v[0-9]*) ;;
		*) usage_error "--version must be a release tag like v0.3.0, got '$want_tag'" ;;
		esac
		case ${want_tag#v} in
		*[!A-Za-z0-9._+-]*) usage_error "--version must be a release tag like v0.3.0, got '$want_tag'" ;;
		esac
	fi
	repo=${repo_arg:-$REPO}

	[ -n "${HOME:-}" ] || die "HOME is not set"

	kernel=$(uname -s)
	machine=$(uname -m)
	case $kernel in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "unsupported OS: $kernel (chottag is built for macOS and Linux)" ;;
	esac
	case $machine in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported CPU: $machine (chottag is built for amd64 and arm64)" ;;
	esac

	chottag_home=${CHOTTAG_HOME:-$HOME/.chottag}
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/chottag-install.XXXXXX") || die "could not make a temporary directory"
	# The cleanup trap is set right away, before anything else can fail, so
	# $tmp is removed even if the very next line (absolutising it) does not.
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM
	# Absolutised so a later cd (try_build's, in a subshell, so it never
	# actually reaches here — but nothing should ever rely on that not
	# changing) can never turn $tmp into a path relative to somewhere else.
	tmp=$(cd "$tmp" && pwd) || die "could not resolve the temporary directory"

	find_clone
	built="" ver="" from="" kind="" why_release="" why_build=""
	try_release
	if [ -z "$built" ]; then
		try_build
		if [ -n "$built" ] && [ -z "$want_tag" ] && [ -n "$why_release" ]; then
			say "note: release not used: $why_release"
		fi
	fi
	[ -n "$built" ] || die "no way to get chottag: $why_release; and $why_build. Log gh in (gh auth login) to install a release, or run ./install.sh from a clone with Go installed."

	dest=$chottag_home/versions/$ver
	(umask 077 && mkdir -p "$dest") || die "could not create $dest"
	cp "$built" "$dest/chottag.new" || die "could not copy the chottag binary into $dest; nothing was installed"
	chmod 0755 "$dest/chottag.new" || die "could not make $dest/chottag.new executable; nothing was installed"
	if ! mv -f "$dest/chottag.new" "$dest/chottag"; then
		rm -f "$dest/chottag.new"
		die "could not move the chottag binary into place at $dest/chottag; nothing was installed"
	fi
	say "installed chottag $ver from $from into $dest"

	"$dest/chottag" setup </dev/null || die "chottag setup failed; the binary is at $dest/chottag, and running '$dest/chottag setup' again is safe"
	write_record "$kind"
	"$dest/chottag" version </dev/null || say "warning: $dest/chottag version failed after a successful install and setup"

	say ""
	say "Next:"
	say "  1. Open a new shell, so $chottag_home/bin comes first on PATH."
	say "  2. chottag login <name>   (once per account; it opens a browser)"
	say "  3. chottag status"
	say "If a chottag daemon was already running, 'chottag daemon restart' moves it to this version."
}

main "$@"
