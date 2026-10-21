package skills

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScan runs scan-bundle against a fake "Claude Code binary" holding body,
// under a temp HOME. The script only reads the file, never runs it. go keeps
// the real build cache so the routecheck helper builds once.
func runScan(t *testing.T, body string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "9.9.9"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{skillPath(t, "scripts", "scan-bundle")}, args...)...)
	cmd.Dir = repoRoot(t)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"GOCACHE=" + filepath.Join(cache, "go-build"),
		"GOPATH=" + filepath.Join(home, "gopath"),
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
		"CHOTTAG_HOME=" + filepath.Join(home, "chottag"),
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

const fakeBundle = "\x00\x01junk" +
	`"/api/frame/deploy/prepare"` + // a rule names it
	`"/api/claude_cli/bootstrap"` + // default, but routes/*.txt lists it
	`"/api/hello"` + // unclassified, not an object: INFO
	"`/v1/widgets/${envId}/session-pin`" + // template literal, an object: REVIEW
	`"/v1/code/fake-triggers?x=1"` + // query dropped, an object: REVIEW
	`"/api/oauth/organizations/:orgUUID/mcp/connectors/list"` + // :name -> {id}
	`"/api/"` + `"/v1/files/"` + `"/api/schema.json"` + `"/api/has space"` + "\x00"

func TestScanBundleReportsUnclassifiedPaths(t *testing.T) {
	out, stderr, code := runScan(t, fakeBundle, "--version", "9.9.9")
	if code != 1 {
		t.Errorf("exit = %d, want 1 (REVIEW lines); stderr: %s", code, stderr)
	}
	want := []string{
		"INFO\t/api/hello\tunclassified",
		"REVIEW\t/api/oauth/organizations/{id}/mcp/connectors/list\tunclassified; may name a claude.ai object",
		"REVIEW\t/v1/code/fake-triggers\tunclassified; may name a claude.ai object",
		"REVIEW\t/v1/widgets/{id}/session-pin\tunclassified; may name a claude.ai object",
	}
	got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("output:\n%s\nwant:\n%s", out, strings.Join(want, "\n"))
	}
}

func TestScanBundleExitsZeroWithoutReview(t *testing.T) {
	out, stderr, code := runScan(t, `"/api/hello" "/api/frame/track"`, "--version", "9.9.9")
	if code != 0 || out != "INFO\t/api/hello\tunclassified\n" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, stderr)
	}
}

func TestScanBundleUsageErrors(t *testing.T) {
	if _, stderr, code := runScan(t, "x", "--version", "1.2.3"); code != 2 || !strings.Contains(stderr, "no such binary") {
		t.Errorf("missing binary: exit %d, stderr %q", code, stderr)
	}
	if _, _, code := runScan(t, "x", "--bogus"); code != 2 {
		t.Errorf("bad flag: exit %d, want 2", code)
	}
}
