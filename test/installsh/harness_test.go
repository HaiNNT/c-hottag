// Package installsh runs install.sh under /bin/sh against fakes. PATH holds
// only fake uname, gh and shasum (plus go and git for the clone cases, and
// sha256sum for the tests that drop shasum to exercise install.sh's
// fallback) and symlinks to a fixed list of real tools, so the script can
// reach no real gh, go, network or ~. HOME, CHOTTAG_HOME and TMPDIR are
// temp dirs.
//
// The fake release's tarball holds a REAL chottag, built once per test
// binary with -X …cli.Version=1.2.3 (what GoReleaser stamps for tag
// v1.2.3). So the `chottag setup` the script runs is the product's own
// setup, and the bin links and rc block these tests assert were made by
// the real code (M3 plan ruling 1). Setup on an empty temp home creates the
// tree and a CA in pure Go, writes the rc block for SHELL=/bin/zsh into the
// temp HOME, and adopts nothing: no Keychain, no claude, no network.
package installsh

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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	relVersion = "1.2.3"
	relTag     = "v" + relVersion
	repo       = "HaiNNT/c-hottag"
	// hangGuard bounds one install.sh run. It is a hang guard only: no
	// assertion depends on how long a run takes (F185).
	hangGuard = 120 * time.Second
)

// realTools are the only real programs on the script's PATH: what
// install.sh and the fakes call. gh, go, git, uname, shasum and sha256sum
// are never among them.
var realTools = []string{"awk", "cat", "chmod", "cp", "cut", "date", "dirname", "grep", "gzip", "head", "mkdir", "mktemp", "mv", "rm", "tar"}

var (
	buildOnce  sync.Once
	buildErr   error
	buildDir   string
	chottagBin string // the real chottag, built by prepare
	scriptBody []byte // install.sh as committed
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func moduleRoot() (string, error) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", fmt.Errorf("no go.mod at %s: %v", root, err)
	}
	return root, nil
}

// withEnv returns base with every KEY in set replaced by set's value.
func withEnv(base []string, set ...string) []string {
	drop := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		drop[k] = true
	}
	var out []string
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// touchSources reads every .go file under root (skipping dot dirs such as
// .git and .worktrees, vendor and testdata). go test cannot see the go
// build subprocess's reads; reading them here, during a test, puts them in
// this test binary's cached read set, so an edit anywhere busts a cached
// pass (the same reason as cmd/chottag/buildcheck_test.go).
func touchSources(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (strings.HasPrefix(n, ".") || n == "vendor" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			_, err = os.ReadFile(path)
		}
		return err
	})
}

// prepare builds the real chottag once per test binary and reads
// install.sh. It runs inside a test, not TestMain, so its file reads count
// toward the test cache.
func prepare(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		if scriptBody, err = os.ReadFile(filepath.Join(root, "install.sh")); err != nil {
			buildErr = err
			return
		}
		if err = touchSources(root); err != nil {
			buildErr = err
			return
		}
		if buildDir, err = os.MkdirTemp("", "installsh-build-"); err != nil {
			buildErr = err
			return
		}
		chottagBin = filepath.Join(buildDir, "chottag")
		goBin, err := exec.LookPath("go")
		if err != nil {
			buildErr = fmt.Errorf("the go tool is not on PATH: %v", err)
			return
		}
		cmd := exec.Command(goBin, "build", "-trimpath",
			"-ldflags", "-X github.com/HaiNNT/c-hottag/internal/cli.Version="+relVersion,
			"-o", chottagBin, "./cmd/chottag")
		cmd.Dir = root
		// GOFLAGS cleared: a developer's -tags=chottag_fakeusage must not
		// turn this into a "+fakeusage" build.
		cmd.Env = withEnv(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

// hostUname is what uname -s / -m print on the host, so the asset the
// script asks for is the one holding a binary this host can run.
func hostUname(t *testing.T) (s, m string) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		s = "Darwin"
	case "linux":
		s = "Linux"
	default:
		t.Skipf("install.sh targets darwin and linux, not %s", runtime.GOOS)
	}
	switch runtime.GOARCH {
	case "amd64":
		m = "x86_64"
	case "arm64":
		m = "arm64"
		if runtime.GOOS == "linux" {
			m = "aarch64"
		}
	default:
		t.Skipf("install.sh targets amd64 and arm64, not %s", runtime.GOARCH)
	}
	return s, m
}

func assetName(ver string) string {
	return fmt.Sprintf("chottag_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
}

const fakeUname = `#!/bin/sh
echo "uname $*" >> "$FAKE_LOG"
case "$1" in
-m) echo "$FAKE_UNAME_M" ;;
*) echo "$FAKE_UNAME_S" ;;
esac
`

// fakeGh serves releases from $FAKE_GH_ASSETS/<tag>/. `release view`
// prints $FAKE_GH_LATEST (unset: no release). `release download` expects
// its tag after a literal "--" (install.sh's own -- guard, item 2), copies
// each --pattern's matches into --dir, and fails, like gh, when a pattern
// matches nothing or the tag has no assets directory. `repo view` prints
// a tab-separated isPrivate and nameWithOwner (default: the requested
// repo unchanged, i.e. no rename; FAKE_GH_REPO_NAME overrides the second
// field, for the renamed-repo tests, fix round 1 item 2) - or, when
// FAKE_GH_REPO_ANSWER is set, that verbatim line instead, for the
// malformed-answer tests (fix round 2 item 3). Every failure prints a
// line on stderr, so install.sh's captured-reason tests (item 3) have
// something to capture.
const fakeGh = `#!/bin/sh
echo "gh $*" >> "$FAKE_LOG"
case "$1 $2" in
"auth status")
	[ "$FAKE_GH_AUTH" = ok ] && exit 0
	echo "You are not logged into any GitHub hosts." >&2
	exit 1 ;;
"release view")
	[ -n "${FAKE_GH_LATEST:-}" ] || { echo "release not found" >&2; exit 1; }
	echo "$FAKE_GH_LATEST"
	exit 0 ;;
"release download")
	tag=
	seen_dashdash=
	for a in "$@"; do
		if [ -n "$seen_dashdash" ]; then
			tag=$a
			break
		fi
		[ "$a" = -- ] && seen_dashdash=1
	done
	[ -n "$tag" ] || { echo "fake gh: release download got no tag after --" >&2; exit 64; }
	[ -d "$FAKE_GH_ASSETS/$tag" ] || { echo "release not found" >&2; exit 1; }
	dir=.
	prev=
	for a in "$@"; do
		[ "$prev" = --dir ] && dir=$a
		prev=$a
	done
	mkdir -p "$dir"
	prev=
	for a in "$@"; do
		if [ "$prev" = --pattern ]; then
			found=
			for f in "$FAKE_GH_ASSETS/$tag"/*; do
				case ${f##*/} in
				$a) cp "$f" "$dir/"; found=1 ;;
				esac
			done
			[ -n "$found" ] || { echo "no assets match the file pattern" >&2; exit 1; }
		fi
		prev=$a
	done
	exit 0 ;;
"repo view")
	[ -n "${FAKE_GH_REPO_FAIL:-}" ] && { echo "HTTP 502" >&2; exit 1; }
	if [ -n "${FAKE_GH_REPO_ANSWER:-}" ]; then
		printf '%s\n' "$FAKE_GH_REPO_ANSWER"
	else
		printf '%s\t%s\n' "${FAKE_GH_PRIVATE:-true}" "${FAKE_GH_REPO_NAME:-$3}"
	fi
	exit 0 ;;
"attestation verify")
	case " $* " in
	*" --help "*) [ "${FAKE_GH_ATTEST:-ok}" = unsupported ] && { echo 'unknown command "attestation" for "gh"' >&2; exit 1; }; exit 0 ;;
	esac
	[ "${FAKE_GH_ATTEST:-ok}" = ok ] && exit 0
	echo "no attestations found" >&2
	exit 1 ;;
esac
echo "fake gh: unexpected: $*" >&2
exit 64
`

// fakeShasum prints $FAKE_SHA256 for the file: the test sets it to the
// tarball's real SHA-256, so a wrong checksums.txt line is what makes a
// mismatch.
const fakeShasum = `#!/bin/sh
echo "shasum $*" >> "$FAKE_LOG"
if [ "$1" != -a ] || [ "$2" != 256 ]; then echo "fake shasum: want -a 256, got $*" >&2; exit 2; fi
printf '%s  %s\n' "$FAKE_SHA256" "$3"
`

// fakeSha256sum prints $FAKE_SHA256 for the file, in sha256sum's own
// output shape (no -a 256 flag): the sha256_of fallback branch (no shasum
// on PATH) goes through this instead.
const fakeSha256sum = `#!/bin/sh
echo "sha256sum $*" >> "$FAKE_LOG"
printf '%s  %s\n' "$FAKE_SHA256" "$1"
`

// fakeGo logs each argument, its working dir and its build env, then
// "builds" by copying the real chottag to its -o path.
const fakeGo = `#!/bin/sh
echo "go $*" >> "$FAKE_LOG"
for a in "$@"; do echo "go.arg $a" >> "$FAKE_LOG"; done
echo "go.pwd $(pwd)" >> "$FAKE_LOG"
echo "go.env CGO_ENABLED=${CGO_ENABLED-unset} GOFLAGS=${GOFLAGS-unset}" >> "$FAKE_LOG"
out=
prev=
for a in "$@"; do
	[ "$prev" = -o ] && out=$a
	prev=$a
done
[ -n "$out" ] || { echo "fake go: no -o" >&2; exit 2; }
cp "$FAKE_GO_BINARY" "$out"
`

const fakeGit = `#!/bin/sh
echo "git $*" >> "$FAKE_LOG"
case " $* " in
*" describe "*) echo "$FAKE_GIT_DESCRIBE" ;;
*) exit 1 ;;
esac
`

// sandbox is one install.sh run's world.
type sandbox struct {
	t      *testing.T
	root   string
	home   string // $HOME
	chHome string // $CHOTTAG_HOME; "" leaves it unset
	tmp    string // $TMPDIR
	path   string // the whole of $PATH
	assets string // the fake gh's releases
	log    string // one line per fake call
	shell  string // the interpreter; /bin/sh unless a test says otherwise
	argv0  string // the interpreter's argv[0]; "" means shell's path
	env    map[string]string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	prepare(t)
	us, um := hostUname(t)
	root := t.TempDir()
	s := &sandbox{
		t: t, root: root,
		home:   filepath.Join(root, "home"),
		chHome: filepath.Join(root, "chottag-home"),
		tmp:    filepath.Join(root, "tmp"),
		path:   filepath.Join(root, "path"),
		assets: filepath.Join(root, "assets"),
		log:    filepath.Join(root, "calls.log"),
		shell:  "/bin/sh",
	}
	for _, d := range []string{s.home, s.tmp, s.path, s.assets} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range realTools {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("real tool %s is not on the test's PATH: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(s.path, tool)); err != nil {
			t.Fatal(err)
		}
	}
	s.addFake("uname", fakeUname)
	s.addFake("gh", fakeGh)
	s.addFake("shasum", fakeShasum)
	s.env = map[string]string{
		"FAKE_UNAME_S":   us,
		"FAKE_UNAME_M":   um,
		"FAKE_GH_AUTH":   "ok",
		"FAKE_GH_ASSETS": s.assets,
		"FAKE_LOG":       s.log,
	}
	return s
}

func (s *sandbox) addFake(name, body string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.path, name), []byte(body), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) dropFake(name string) {
	s.t.Helper()
	if err := os.Remove(filepath.Join(s.path, name)); err != nil {
		s.t.Fatal(err)
	}
}

// release publishes tag on the fake gh: chottag_<ver>_<os>_<arch>.tar.gz
// (the real chottag and a README.md at the archive root, GoReleaser's
// layout) and checksums.txt. It sets FAKE_SHA256 to the tarball's real
// SHA-256; wrongSum writes 64 zeros into checksums.txt instead.
func (s *sandbox) release(tag string, wrongSum bool) {
	s.t.Helper()
	bin, err := os.ReadFile(chottagBin)
	if err != nil {
		s.t.Fatal(err)
	}
	s.publishTarball(tag, wrongSum, tarFile{"chottag", 0o755, bin}, tarFile{"README.md", 0o644, []byte("# c-hottag\n")})
}

// releaseWithoutBinary publishes tag the same way release does, except the
// tarball holds no chottag member at all — GoReleaser never ships this,
// but a corrupted upload or a mismatched archive could (item 7).
func (s *sandbox) releaseWithoutBinary(tag string) {
	s.t.Helper()
	s.publishTarball(tag, false, tarFile{"README.md", 0o644, []byte("# c-hottag\n")})
}

type tarFile struct {
	name string
	mode int64
	data []byte
}

// publishTarball builds a chottag_<ver>_<os>_<arch>.tar.gz from files and a
// matching checksums.txt (or a wrong one, for wrongSum), and sets
// FAKE_SHA256 to the tarball's real SHA-256.
func (s *sandbox) publishTarball(tag string, wrongSum bool, files ...tarFile) {
	s.t.Helper()
	ver := strings.TrimPrefix(tag, "v")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			s.t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			s.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		s.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		s.t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	realSum := hex.EncodeToString(sum[:])
	listed := realSum
	if wrongSum {
		listed = strings.Repeat("0", 64)
	}
	dir := filepath.Join(s.assets, tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, assetName(ver)), buf.Bytes(), 0o644); err != nil {
		s.t.Fatal(err)
	}
	sums := fmt.Sprintf("%s  %s\n%s  chottag_%s_plan9_mips.tar.gz\n", listed, assetName(ver), strings.Repeat("1", 64), ver)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(sums), 0o644); err != nil {
		s.t.Fatal(err)
	}
	s.env["FAKE_SHA256"] = realSum
}

// clone makes a directory install.sh accepts as a clone (go.mod naming the
// module, cmd/chottag/, the script) and puts fake go and git on PATH.
func (s *sandbox) clone() string {
	s.t.Helper()
	dir := filepath.Join(s.root, "clone")
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "chottag"), 0o700); err != nil {
		s.t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":              "module github.com/HaiNNT/c-hottag\n\ngo 1.27.1\n",
		"cmd/chottag/main.go": "package main\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "install.sh"), scriptBody, 0o755); err != nil {
		s.t.Fatal(err)
	}
	s.addFake("go", fakeGo)
	s.addFake("git", fakeGit)
	s.env["FAKE_GO_BINARY"] = chottagBin
	s.env["FAKE_GIT_DESCRIBE"] = "v0.3.0-2-gabc1234"
	return dir
}

type result struct {
	code           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", r.code, r.stdout, r.stderr)
}

// environ is the script's whole environment. Never os.Environ(): a real
// GH_TOKEN, GOFLAGS or CHOTTAG_HOME must not reach it.
func (s *sandbox) environ() []string {
	env := []string{"HOME=" + s.home, "PATH=" + s.path, "SHELL=/bin/zsh", "TMPDIR=" + s.tmp, "LANG=C"}
	if s.chHome != "" {
		env = append(env, "CHOTTAG_HOME="+s.chHome)
	}
	for k, v := range s.env {
		env = append(env, k+"="+v)
	}
	return env
}

func (s *sandbox) exec(dir string, stdin []byte, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hangGuard)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.shell, args...)
	if s.argv0 != "" {
		cmd.Args[0] = s.argv0
	}
	cmd.Dir = dir
	cmd.Env = s.environ()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		s.t.Fatalf("install.sh still running after the %s hang guard\n%s%s", hangGuard, out.String(), errb.String())
	}
	r := result{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		s.t.Fatal(err)
	}
	return r
}

// install runs a copy of install.sh that is NOT in a clone, from s.root.
func (s *sandbox) install(args ...string) result {
	s.t.Helper()
	dir := filepath.Join(s.root, "src")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	script := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(script, scriptBody, 0o755); err != nil {
		s.t.Fatal(err)
	}
	return s.exec(s.root, nil, append([]string{script}, args...)...)
}

// pipe runs install.sh the way `gh api … | sh -s -- args` does: the script
// on stdin, from dir, and argv[0] "sh", as a shell found on PATH gets. So
// $0 is "sh", whose dirname is ".", the working directory: the case
// find_clone's own `$0` check exists to refuse (TestPipedScriptNeverTreats
// TheWorkingDirAsAClone pins it). With argv[0] "/bin/sh" instead, dirname
// would be /bin — a real script file on disk, not this piped-stdin case —
// which is why find_clone's case also matches "*/install.sh" specifically,
// not any path at all.
func (s *sandbox) pipe(dir string, args ...string) result {
	s.t.Helper()
	s.argv0 = "sh"
	defer func() { s.argv0 = "" }()
	return s.exec(dir, scriptBody, append([]string{"-s", "--"}, args...)...)
}

func (s *sandbox) calls() []string {
	b, err := os.ReadFile(s.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (s *sandbox) called(prefix string) bool {
	for _, c := range s.calls() {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func resolve(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// assertInstalled checks what install.sh and the real setup made under
// chHome for ver: the versioned binary (0755, the built bytes), both bin
// links resolving to it, the CA, and exactly one rc block in HOME/.zshrc —
// carrying an `export CHOTTAG_HOME=` line ahead of the PATH line whenever
// s.chHome is set (fix round item 9), matching what CHOTTAG_HOME the
// sandbox actually ran the script under.
func (s *sandbox) assertInstalled(chHome, ver string) {
	t := s.t
	t.Helper()
	dest := filepath.Join(chHome, "versions", ver, "chottag")
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("no versioned binary: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
		t.Errorf("%s mode = %v, want a regular file with 0755", dest, fi.Mode())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(chottagBin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is not the released binary", dest)
	}
	for _, name := range []string{"chottag", "claude"} {
		link := filepath.Join(chHome, "bin", name)
		li, err := os.Lstat(link)
		if err != nil || li.Mode()&os.ModeSymlink == 0 {
			t.Errorf("bin/%s is not a symlink (err %v)", name, err)
			continue
		}
		if resolve(t, link) != resolve(t, dest) {
			t.Errorf("bin/%s resolves to %s, want %s", name, resolve(t, link), resolve(t, dest))
		}
	}
	if _, err := os.Stat(filepath.Join(chHome, "ca", "ca.pem")); err != nil {
		t.Errorf("setup made no CA: %v", err)
	}
	rc, err := os.ReadFile(filepath.Join(s.home, ".zshrc"))
	if err != nil {
		t.Fatalf("no rc file: %v", err)
	}
	block := "# >>> chottag >>>\n"
	if s.chHome != "" {
		// CHOTTAG_HOME was set: setup's rc block also exports it (fix
		// round item 9), so a new shell's `chottag` resolves this same
		// home rather than falling back to $HOME/.chottag.
		block += "export CHOTTAG_HOME=\"" + s.chHome + "\"\n"
	}
	block += "export PATH=\"" + filepath.Join(chHome, "bin") + ":$PATH\"\n# <<< chottag <<<\n"
	if n := strings.Count(string(rc), block); n != 1 {
		t.Errorf("rc holds the chottag block %d times, want 1:\n%s", n, rc)
	}
}

// installRecord is install.json's shape (Task 3 in internal/cli reads the
// real one; this test package parses its own copy, so a field rename here
// is caught independently of that code).
type installRecord struct {
	Repo        string `json:"repo"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	InstalledAt string `json:"installedAt"`
}

// installRecordPath is where install.sh writes the record: under chHome
// when CHOTTAG_HOME is set, else under $HOME/.chottag (install.sh's own
// default).
func (s *sandbox) installRecordPath() string {
	if s.chHome != "" {
		return filepath.Join(s.chHome, "install.json")
	}
	return filepath.Join(s.home, ".chottag", "install.json")
}

// readInstallRecord reads and parses install.json, and fails the test if
// it is missing, unparsable, or not mode 0600, or if a temp file from the
// atomic write (install.json.tmp.*) was left behind.
func (s *sandbox) readInstallRecord() installRecord {
	t := s.t
	t.Helper()
	p := s.installRecordPath()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("no install.json: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s mode = %v, want a regular file with 0600", p, fi.Mode())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var rec installRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("install.json does not parse: %v\n%s", err, b)
	}
	if _, err := time.Parse(time.RFC3339, rec.InstalledAt); err != nil {
		t.Errorf("install.json installedAt %q is not RFC3339: %v", rec.InstalledAt, err)
	}
	tmps, err := filepath.Glob(p + ".tmp.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(tmps) != 0 {
		t.Errorf("install.json temp files left behind: %v", tmps)
	}
	return rec
}

// assertNothingInstalled: no chottag home, nothing in HOME, and TMPDIR
// empty (the script's trap removed its temp dir).
func (s *sandbox) assertNothingInstalled() {
	t := s.t
	t.Helper()
	for _, h := range []string{s.chHome, filepath.Join(s.home, ".chottag")} {
		if h == "" {
			continue
		}
		if _, err := os.Lstat(h); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists (err %v); a failed install must create nothing", h, err)
		}
	}
	if got := entries(t, s.home); len(got) != 0 {
		t.Errorf("HOME holds %v; a failed install must write nothing there", got)
	}
	if got := entries(t, s.tmp); len(got) != 0 {
		t.Errorf("TMPDIR holds %v; the script's temp dir was not removed", got)
	}
}
