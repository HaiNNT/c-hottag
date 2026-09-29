package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// fakeusageMarkers are strings that exist only in the tagged files: the
// environment variables' names (CHOTTAG_FAKE_LIMIT also covers ..._TTL),
// the log line, the version suffix, the hooks' own function names and
// their source files' names (which the binary's line table records for any
// code compiled from them). The M4 fake-util knob (fakeutil_on.go) is
// listed alongside the fake-limit hook (M4 spec §8).
var fakeusageMarkers = []string{
	"CHOTTAG_FAKE_LIMIT",
	"chottag_fakeusage build",
	"+fakeusage",
	"simulateLimits",
	"fakeusage_on.go",
	"CHOTTAG_FAKE_UTIL",
	"fakeUtilOverride",
	"fakeutil_on.go",
}

func buildChottag(t *testing.T, tags ...string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "chottag")
	args := []string{"build", "-o", out}
	if len(tags) > 0 {
		args = append(args, "-tags", tags[0])
	}
	goTool(t, []string{"CGO_ENABLED=0"}, append(args, "./cmd/chottag")...)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestReleaseBinaryHasNoFakeLimitHook is spec §10.1's proof that a release
// build holds neither the fake-limit hook's code nor its variable's name.
// It scans the built binary itself, not the file list: that catches the
// hook reaching a release build by any route (a helper moved into an
// untagged file, a string in another package). The tagged build is the
// control: every marker must be found there, or the scan proves nothing.
func TestReleaseBinaryHasNoFakeLimitHook(t *testing.T) {
	t.Parallel()
	touchSources(t, moduleRoot(t))
	release := buildChottag(t)
	tagged := buildChottag(t, "chottag_fakeusage")
	for _, m := range fakeusageMarkers {
		if bytes.Contains(release, []byte(m)) {
			t.Errorf("the release binary contains %q: the fake-limit hook leaked out of its build tag", m)
		}
		if !bytes.Contains(tagged, []byte(m)) {
			t.Errorf("the tagged binary lacks %q: this scan cannot see the hook, so its clean result means nothing", m)
		}
	}
}

// TestFakeusageTaggedSuitePasses runs internal/cli's tests in the tagged
// build, so the gate's plain `go test ./...` exercises the hook too
// (spec §10.1). It also vets the tagged files, which `go vet ./...` skips.
// -race always: the gate runs with it, and the hook writes shared state;
// -shuffle=on, as the gate runs it. The gate's last line runs the same
// tagged suite again, on purpose (ci.yml's header says why).
func TestFakeusageTaggedSuitePasses(t *testing.T) {
	t.Parallel()
	touchSources(t, moduleRoot(t))
	goTool(t, nil, "vet", "-tags", "chottag_fakeusage", "./internal/cli/")
	goTool(t, nil, "test", "-tags", "chottag_fakeusage", "-race", "-count=1", "-shuffle=on", "./internal/cli/")
}
