package leakscan

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLeakScanFindsEachGenericCategory(t *testing.T) {
	cases := []struct{ name, line, category string }{
		{"a macOS home path", "cd " + homePath("zed"), "home-path"},
		{"a Linux home path", "ls /" + "home/zed/x", "home-path"},
		{"a lowercase home path", "cd " + lowerHomePath("zed"), "home-path"},
		{"a JSON-escaped home path", "path " + jsonHomePath("zed"), "home-path"},
		{"a real-looking email", "mail " + email("carol", "corp.test"), "email"},
		{"a session link", "see https://" + sessionURL(), "session-url"},
		{"a mixed-case session link", "see https://" + "Claude.AI/code/" + "session_" + "01ABCDEF", "session-url"},
		{"a bare session id", "id " + bareSessionID(), "session-url"},
		{"a session trailer", trailer(), "session-trailer"},
		{"a lowercase session trailer", trailerCased("claude"), "session-trailer"},
		{"an uppercase session trailer", trailerCased("CLAUDE"), "session-trailer"},
	}
	for _, c := range cases {
		dir := tree(t, map[string]string{"notes/f.txt": "a clean line\n" + c.line + "\n"})
		out, errb, code := script(t, dir, "notes/f.txt\n", "leak-scan")
		if code != 1 {
			t.Errorf("%s: exit %d, want 1 (stderr %q)", c.name, code, errb)
		}
		if want := "notes/f.txt:2: " + c.category + "\n"; out != want {
			t.Errorf("%s: stdout %q, want %q", c.name, out, want)
		}
	}
}

func TestLeakScanAllowsTheDocumentedFixtures(t *testing.T) {
	lines := []string{
		homePath("alice"), "/" + "home/u/.chottag/bin", "/" + "Users/x/.claude", "/" + "home/bob/x",
		lowerHomePath("alice"), jsonHomePath("u"), lowerHomePath("x"),
		email("alice", "example.com"), email("A", "Example.COM"), email("bob", "mail.example.org"),
		"https://user:hunter2" + "@corp-proxy.example:3128", email("12345+alice", "users.noreply.github.com"),
		"git" + "@github.com:HaiNNT/c-hottag.git", "chottag@c-hottag", "chottag@v1.2.3",
	}
	dir := tree(t, map[string]string{"f.txt": strings.Join(lines, "\n") + "\n"})
	out, errb, code := script(t, dir, "f.txt\n", "leak-scan")
	if code != 0 || out != "" {
		t.Errorf("exit %d, stdout %q (stderr %q); want 0 and no hits", code, out, errb)
	}
}

func TestLeakScanNeverPrintsTheMatchedText(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt": "zebracorp\n",
		"f.txt":    "hello ZebraCorp\n" + email("carol", "corp.test") + "\n" + homePath("zed") + "\n",
	})
	out, errb, code := script(t, dir, "f.txt\n", "leak-scan", "-p", "pats.txt")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	all := strings.ToLower(out + errb)
	for _, secret := range []string{"zebracorp", "carol", "corp.test", "zed"} {
		if strings.Contains(all, secret) {
			t.Errorf("output repeats %q:\n%s%s", secret, out, errb)
		}
	}
}

func TestLeakScanPrivatePatternsAreNumberedAndCaseInsensitive(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt": "# a comment\n\nfirst-term\nsecond\\.term\n",
		"f.txt":    "x SECOND.TERM y\nsecondXterm is not a hit\n",
	})
	out, _, code := script(t, dir, "f.txt\n", "leak-scan", "-p", "pats.txt")
	if code != 1 || out != "f.txt:1: private pattern 2\n" {
		t.Errorf("exit %d, stdout %q; want 1 and one hit on pattern 2", code, out)
	}
}

// TestLeakScanTrimsWhitespaceAndCRFromPrivatePatterns pins I3: a stray
// space or a CRLF patterns file must never silently disable a pattern.
func TestLeakScanTrimsWhitespaceAndCRFromPrivatePatterns(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt": "zebracorp \r\n  zulucorp\r\n",
		"f.txt":    "hello zebracorp\nhello zulucorp\n",
	})
	out, _, code := script(t, dir, "f.txt\n", "leak-scan", "-p", "pats.txt")
	want := "f.txt:1: private pattern 1\nf.txt:2: private pattern 2\n"
	if code != 1 || out != want {
		t.Errorf("exit %d, stdout %q; want %q", code, out, want)
	}
}

// TestLeakScanRejectsAnUnsupportedBackslashEscape pins M6: a backslash
// letter escape (\d, \w, \s, \b) has no meaning in POSIX ERE and is
// rejected at load time, named only by the pattern's number.
func TestLeakScanRejectsAnUnsupportedBackslashEscape(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt": "clean-one\nzebra\\dcorp\n",
		"f.txt":    "clean\n",
	})
	out, errb, code := script(t, dir, "f.txt\n", "leak-scan", "-p", "pats.txt")
	if code != 2 || out != "" {
		t.Errorf("exit %d, stdout %q; want 2 and no output", code, out)
	}
	if !strings.Contains(errb, "pattern 2") {
		t.Errorf("stderr %q: want it to name pattern 2", errb)
	}
	if strings.Contains(errb, "zebra") || strings.Contains(errb, "corp") {
		t.Errorf("stderr %q: must never repeat a pattern's text", errb)
	}
}

// TestLeakScanChecksPathsAgainstPrivatePatterns pins M3: a path that
// matches a private pattern is reported by its stdin line number, never
// by the path itself, because the path repeats the private term.
func TestLeakScanChecksPathsAgainstPrivatePatterns(t *testing.T) {
	dir := tree(t, map[string]string{"pats.txt": "zebracorp\n", "docs/zebracorp-notes.md": "clean\n"})
	out, _, code := script(t, dir, "docs/zebracorp-notes.md\n", "leak-scan", "-p", "pats.txt")
	if code != 1 || out != "stdin line 1:0: private pattern 1 (in the path)\n" {
		t.Errorf("exit %d, stdout %q; want the stdin line number, never the path", code, out)
	}
}

// TestLeakScanMasksEveryHitForAPathThatMatchesAPrivatePattern extends M3:
// once a path matches a private pattern, every other hit for that same
// path (content hits too) must also use the stdin line label, since the
// real path would repeat the private term.
func TestLeakScanMasksEveryHitForAPathThatMatchesAPrivatePattern(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt":                "zebracorp\n",
		"docs/zebracorp-notes.md": email("carol", "corp.test") + "\n",
	})
	out, _, code := script(t, dir, "docs/zebracorp-notes.md\n", "leak-scan", "-p", "pats.txt")
	want := "stdin line 1:0: private pattern 1 (in the path)\nstdin line 1:1: email\n"
	if code != 1 || out != want {
		t.Errorf("exit %d, stdout %q; want %q", code, out, want)
	}
}

// TestLeakScanMasksContentHitsWhenOnlyAGenericPathMatches pins the NEW-2
// residual: masking must not be conditional on a private-pattern path
// match. A path that matches only a GENERIC category (no -p needed)
// must mask a content hit in that same file too, since the path is the
// matched text there either way.
func TestLeakScanMasksContentHitsWhenOnlyAGenericPathMatches(t *testing.T) {
	p := "notes/" + email("carol", "corp.test") + ".md"
	dir := tree(t, map[string]string{p: "this has " + email("carol", "corp.test") + " in it\n"})
	out, _, code := script(t, dir, p+"\n", "leak-scan")
	want := "stdin line 1:0: email (in the path)\nstdin line 1:1: email\n"
	if code != 1 || out != want {
		t.Errorf("exit %d, stdout %q; want %q", code, out, want)
	}
}

// TestLeakScanMasksAReadFailureWhenTheGenericPathMatches is the other
// half of the NEW-2 residual: a "cannot read" message must use the same
// stdin-line label, not the path, once the path matches a generic
// category.
func TestLeakScanMasksAReadFailureWhenTheGenericPathMatches(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't block reads")
	}
	p := "notes/" + email("carol", "corp.test") + ".x"
	dir := tree(t, map[string]string{p: "clean\n"})
	full := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(full, 0o600)
	out, errb, code := script(t, dir, p+"\n", "leak-scan")
	if code != 2 {
		t.Fatalf("exit %d (stdout %q, stderr %q), want 2", code, out, errb)
	}
	if !strings.Contains(errb, "cannot read stdin line 1") {
		t.Errorf("stderr %q: want the read failure labelled by stdin line, not the path", errb)
	}
	if strings.Contains(errb, "corp.test") {
		t.Errorf("stderr %q: must not repeat the path", errb)
	}
}

func TestLeakScanReadsBinaryFiles(t *testing.T) {
	dir := tree(t, map[string]string{"x.pyc": "\x00\x01bin" + homePath("zed") + "\x00tail\n"})
	out, _, code := script(t, dir, "x.pyc\n", "leak-scan")
	if code != 1 || out != "x.pyc:1: home-path\n" {
		t.Errorf("exit %d, stdout %q; a home path inside a binary file must be exactly one hit on line 1", code, out)
	}
}

// TestLeakScanScansASymlinksLinkText pins M2 (ruling): a symlink is
// scanned by its link text, not by following it.
func TestLeakScanScansASymlinksLinkText(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(homePath("zed"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	out, errb, code := script(t, dir, "link\n", "leak-scan")
	if code != 1 || out != "link:1: home-path\n" {
		t.Errorf("exit %d, stdout %q (stderr %q); want the link's own text scanned for a hit", code, out, errb)
	}
}

// TestLeakScanNeverFollowsASymlink is the other half of M2: a symlink
// whose link text is clean, but whose target holds a leak, must never be
// followed. The link text ("elsewhere/secret.txt") is fully controlled,
// so this test doesn't depend on where the OS puts a temp directory.
func TestLeakScanNeverFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "elsewhere"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "elsewhere", "secret.txt"), []byte(email("carol", "corp.test")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere/secret.txt", filepath.Join(dir, "clean-name")); err != nil {
		t.Fatal(err)
	}
	out, errb, code := script(t, dir, "clean-name\n", "leak-scan")
	if code != 0 || out != "" {
		t.Errorf("exit %d, stdout %q (stderr %q): the symlink's target must never be read, only its link text", code, out, errb)
	}
}

// TestLeakScanAppliesGenericCategoriesToThePathToo pins M4. NEW-2: the
// path is the matched text for a path-based generic hit too, so it is
// reported by its stdin line number, exactly like a private-pattern
// path hit, never by the path itself -- with or without -p.
func TestLeakScanAppliesGenericCategoriesToThePathToo(t *testing.T) {
	homeInPath := "fx" + homePath("zed") + "/f"
	emailInPath := "notes/" + email("carol", "corp.test") + ".md"
	dir := tree(t, map[string]string{homeInPath: "clean\n", emailInPath: "clean\n"})
	in := homeInPath + "\n" + emailInPath + "\n"
	out, errb, code := script(t, dir, in, "leak-scan")
	want := "stdin line 1:0: home-path (in the path)\nstdin line 2:0: email (in the path)\n"
	if code != 1 || out != want {
		t.Errorf("exit %d, stdout %q (stderr %q); want %q", code, out, errb, want)
	}
}

// TestLeakScanHonorsDashC pins the M8 mutation "-C ignored": scanning
// must happen relative to -C, not the current directory.
func TestLeakScanHonorsDashC(t *testing.T) {
	dir := tree(t, map[string]string{"sub/leak.txt": homePath("zed") + "\n"})
	out, errb, code := script(t, dir, "leak.txt\n", "leak-scan", "-C", "sub")
	if code != 1 || out != "leak.txt:1: home-path\n" {
		t.Errorf("exit %d, stdout %q (stderr %q); -C sub must scan sub/leak.txt", code, out, errb)
	}
}

// TestLeakScanFailsOnAnUnreadableFile pins I2: an unreadable input file
// fails closed (exit 2), it is never reported as clean.
func TestLeakScanFailsOnAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits don't block reads")
	}
	dir := tree(t, map[string]string{"secret.txt": "clean\n"})
	p := filepath.Join(dir, "secret.txt")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0o600)
	out, _, code := script(t, dir, "secret.txt\n", "leak-scan")
	if code != 2 || out != "" {
		t.Errorf("exit %d, stdout %q; want 2 and no output for an unreadable file", code, out)
	}
}

// TestLeakScanFailsWhenContentCannotBeRead pins the I2 residual: every
// grep that reads file content must check its own exit status, not rely
// on [ -r ] alone. A Unix domain socket is a portable, root-free way to
// reproduce "[ -r ] says readable, but the read itself fails": the
// socket file's permission bits pass a readability check, yet grep (and
// cat) refuse to read a socket as a byte stream and exit with status 2,
// not 1 ("no match").
// newTestSocket creates a unix domain socket at a short-named path (a
// socket's path must fit sockaddr_un.sun_path, ~104 bytes on macOS, and
// t.TempDir() embeds the test name and can run over that) and returns
// its directory. Skips if this environment can't make one.
func newTestSocket(t *testing.T) (dir string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "lss")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "sock")
	if len(sockPath) > 100 {
		t.Skipf("socket path %q is too long for this OS", sockPath)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("could not create a unix socket here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return dir
}

// TestLeakScanFailsWhenContentCannotBeRead pins the I2 residual for
// run_category (the generic scan): a unix socket's permission bits pass
// [ -r ], but grep/cat refuse to read one as a byte stream -- a
// portable, root-free way to reproduce "access() and open() disagree".
// No -p here: this specifically exercises run_category, not
// scan_private_content (see the -p variant below, R2-3).
func TestLeakScanFailsWhenContentCannotBeRead(t *testing.T) {
	dir := newTestSocket(t)
	out, _, code := script(t, dir, "sock\n", "leak-scan")
	if code != 2 || out != "" {
		t.Errorf("exit %d, stdout %q; want 2 and no output when content can't actually be read", code, out)
	}
}

// TestLeakScanFailsWhenPrivateContentCannotBeRead is the -p variant of
// the test above (R2-3: the round 2 report claimed the no--p test alone
// covered scan_private_content's own st>1 check, at leak-scan:227; it
// does not, because run_category's check on the generic scan fails
// first and the private-content grep in scan_private_content is never
// reached). This test passes -p, so scan_private_content's grep is what
// hits the socket.
func TestLeakScanFailsWhenPrivateContentCannotBeRead(t *testing.T) {
	dir := newTestSocket(t)
	if err := os.WriteFile(filepath.Join(dir, "pats.txt"), []byte("zebracorp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code := script(t, dir, "sock\n", "leak-scan", "-p", "pats.txt")
	if code != 2 || out != "" {
		t.Errorf("exit %d, stdout %q; want 2 and no output when private-pattern content can't actually be read", code, out)
	}
}

// TestLeakScanFindsAPrivateTermAfterANULByte pins NEW-1 (blocking): on
// macOS, /bin/sh is bash 3.2, which truncates a shell variable at a NUL
// byte. Numbering a private-pattern hit by re-testing a line's content
// that was read into a variable would then silently drop a hit after a
// NUL -- exactly the binary-file shape this script exists to catch.
// Per-pattern numbering must come straight from grep on the file itself.
func TestLeakScanFindsAPrivateTermAfterANULByte(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt": "zebracorp\n",
		"f.txt":    "\x00\x01zebracorp\x00tail\n",
	})
	out, errb, code := script(t, dir, "f.txt\n", "leak-scan", "-p", "pats.txt")
	if code != 1 {
		t.Fatalf("exit %d (stderr %q), want 1: a private term after a NUL byte must still be a hit", code, errb)
	}
	if !strings.Contains(out, "private pattern 1") {
		t.Errorf("stdout %q: want a private pattern 1 hit", out)
	}
}

// TestLeakScanFindsAPrivateTermInAnInvalidUTF8Filename pins R2-1: under
// GNU grep in a UTF-8 locale, a path holding a byte sequence that is not
// valid UTF-8 makes grep treat the path list as binary; without -a, a
// real match is then reported only as "binary file matches" on stderr,
// and the path is never masked. This needs a custom (not script()) run,
// since script() always sets LC_ALL=C, which does not exercise the
// bug. If this filesystem refuses a non-UTF-8 file name (as macOS's
// APFS does), the test skips with that reason rather than faking a pass.
func TestLeakScanFindsAPrivateTermInAnInvalidUTF8Filename(t *testing.T) {
	dir := t.TempDir()
	// 0xE9 alone (no UTF-8 lead byte) is not valid UTF-8. "zebracorp" is
	// the private term the pattern below looks for.
	name := "caf\xe9-" + "zebracorp" + ".md"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("clean\n"), 0o600); err != nil {
		t.Skipf("this filesystem refuses a non-UTF-8 file name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pats.txt"), []byte("zebracorp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", filepath.Join(root(t), "scripts", "leak-scan"), "-p", "pats.txt")
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir(), "PATH=" + os.Getenv("PATH"),
		// The bug needs an actual UTF-8 locale, unlike every other test
		// here (which uses LC_ALL=C via script()).
		"LC_ALL=en_US.UTF-8", "LANG=en_US.UTF-8",
	}
	cmd.Stdin = strings.NewReader(name + "\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run leak-scan: %v", err)
		}
		code = ee.ExitCode()
	}
	if code != 1 {
		t.Errorf("exit %d (stdout %q, stderr %q), want 1: a private term in a non-UTF-8 file name must still be a hit", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "zebracorp") || strings.Contains(errb.String(), "zebracorp") {
		t.Errorf("output repeats the private term:\n%s%s", out.String(), errb.String())
	}
}

// TestLeakScanTabAndTBoundedPrivatePatternsSurviveStripping pins the I3
// residual: BSD sed reads a bracket expression "[ \t]" as the three
// literal characters space, backslash and t -- not space-and-tab -- so
// it neither strips a real leading/trailing TAB, nor leaves alone a
// pattern that legitimately starts or ends with "t" or "\". [:blank:]
// must be used instead.
func TestLeakScanTabAndTBoundedPrivatePatternsSurviveStripping(t *testing.T) {
	dir := tree(t, map[string]string{
		"pats.txt": "\tzebracorp\t\ntacme\nacme\\.net\n",
		"f.txt":    "hello zebracorp\nclean tacme here\nvisit acme.net today\n",
	})
	out, _, code := script(t, dir, "f.txt\n", "leak-scan", "-p", "pats.txt")
	want := "f.txt:1: private pattern 1\nf.txt:2: private pattern 2\nf.txt:3: private pattern 3\n"
	if code != 1 || out != want {
		t.Errorf("exit %d, stdout %q; want %q (a TAB must be stripped like a space, and a pattern's own leading/trailing \"t\" or \"\\\\\" must survive)", code, out, want)
	}
}

// TestLeakScanScansTheLastPathWithoutATrailingNewline pins M1 with a
// dedicated test (the round 1 report's "covered structurally" claim did
// not actually pin the mutation that removes || [ -n "$raw" ]).
func TestLeakScanScansTheLastPathWithoutATrailingNewline(t *testing.T) {
	dir := tree(t, map[string]string{"e2.txt": homePath("zed") + "\n"})
	out, errb, code := script(t, dir, "e2.txt", "leak-scan")
	if code != 1 || out != "e2.txt:1: home-path\n" {
		t.Errorf("exit %d, stdout %q (stderr %q); the last path must be scanned even without a trailing newline", code, out, errb)
	}
}

func TestLeakScanFailsClosed(t *testing.T) {
	dir := tree(t, map[string]string{"f.txt": "clean\n", "comments.txt": "# only\n\n", "bad.txt": "(\n", "sub/.keep": ""})
	cases := []struct {
		name        string
		stdin       string
		args        []string
		wantErrHas  string
		wantErrMiss string
	}{
		{name: "a missing patterns file", stdin: "f.txt\n", args: []string{"-p", "nope.txt"}},
		{name: "a patterns file with no pattern", stdin: "f.txt\n", args: []string{"-p", "comments.txt"}},
		{name: "an invalid pattern", stdin: "f.txt\n", args: []string{"-p", "bad.txt"}},
		{name: "no paths on stdin", stdin: ""},
		{name: "an unknown flag", stdin: "f.txt\n", args: []string{"--nope"}},
		{name: "every input path missing", stdin: "nope.txt\n"},
		{name: "a wrongly rooted -C", stdin: "f.txt\n", args: []string{"-C", "sub"}},
		{
			// M8: the mutation that removes the absolute-path refusal
			// survives on exit code and stdout alone, because the path
			// then just looks like a missing relative one. Pin the
			// specific stderr message (and NEW-2: it must never repeat
			// the path, only the stdin line number).
			name: "an absolute path", stdin: "/etc/hosts\n",
			wantErrHas: "refusing an absolute path (stdin line 1)", wantErrMiss: "/etc/hosts",
		},
		{
			name: "a path that escapes -C", stdin: "../etc/passwd\n",
			wantErrHas: "refusing a path that escapes -C (stdin line 1)", wantErrMiss: "../etc/passwd",
		},
		{
			// NEW-3: a TAB in a path would otherwise corrupt the
			// TAB-delimited "$tmp/paths" format and defeat masking.
			name: "a path with an embedded tab", stdin: "a\tzebracorp.md\n",
			wantErrHas: "refusing a path with an embedded tab (stdin line 1)", wantErrMiss: "zebracorp",
		},
		{name: "an empty -p", stdin: "f.txt\n", args: []string{"-p", ""}},
	}
	for _, c := range cases {
		out, errb, code := script(t, dir, c.stdin, "leak-scan", c.args...)
		if code != 2 || out != "" {
			t.Errorf("%s: exit %d, stdout %q; want 2 and no output", c.name, code, out)
		}
		if c.wantErrHas != "" && !strings.Contains(errb, c.wantErrHas) {
			t.Errorf("%s: stderr %q, want it to contain %q", c.name, errb, c.wantErrHas)
		}
		if c.wantErrMiss != "" && strings.Contains(errb, c.wantErrMiss) {
			t.Errorf("%s: stderr %q must not repeat %q", c.name, errb, c.wantErrMiss)
		}
	}
}

// TestLeakScanFailsClosedOnASignal pins I1: SIGTERM/SIGHUP/SIGINT mid-scan
// must exit non-zero (2), never 0, and never a false "clean" report. The
// scan is put in a deterministic mid-scan state by leaving stdin open
// after one path, so the script blocks on the next read. Readiness (its
// traps are installed) is an event, not a guessed duration (M5): the
// script itself creates "$tmp/hits" (scripts/leak-scan, right after the
// `trap` lines) before reading any path, so the test polls its own TMPDIR
// for that file instead of sleeping a fixed time; a 60s guard is a hang
// guard only, never a correctness bound.
func TestLeakScanFailsClosedOnASignal(t *testing.T) {
	dir := tree(t, map[string]string{"leak.txt": homePath("zed") + "\n"})
	tmpdir := t.TempDir()
	cmd := exec.Command("/bin/sh", filepath.Join(root(t), "scripts", "leak-scan"))
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + t.TempDir(), "TMPDIR=" + tmpdir, "PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "leak.txt\n"); err != nil {
		t.Fatal(err)
	}
	waitForGlob(t, filepath.Join(tmpdir, "leak-scan.*", "hits"), 60*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case werr := <-done:
		if werr == nil {
			t.Fatalf("a signalled scan exited 0 (stdout %q)", out.String())
		}
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			t.Fatalf("run leak-scan: %v", werr)
		}
		if ee.ExitCode() != 2 {
			t.Errorf("exit %d, want 2 after SIGTERM (stdout %q, stderr %q)", ee.ExitCode(), out.String(), errb.String())
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatal("leak-scan did not exit within 60s of SIGTERM")
	}
	stdin.Close()
}
