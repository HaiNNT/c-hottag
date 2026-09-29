package notify

import (
	"context"
	"os"
	"testing"
)

// TestMain arms a panicking command runner before any test runs. A test
// that reaches Osascript.Send without stubbing the runner (withRun) must
// crash loudly, never put a real notification on this Mac (M2b: tests never
// run osascript). internal/cli's TestMain arms the same default for its own
// test binary through the exported SetRunForTest.
func TestMain(m *testing.M) {
	SetRunForTest(func(context.Context, string, []string) error {
		panic("osascript reached from internal/notify's test binary: stub it with withRun(t, ...)")
	})
	os.Exit(m.Run())
}

// withRun installs fn as the command runner for one test and restores the
// panicking default afterwards.
func withRun(t *testing.T, fn func(ctx context.Context, name string, args []string) error) {
	t.Helper()
	t.Cleanup(SetRunForTest(fn))
}
