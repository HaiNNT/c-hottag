package notify

import "context"

// SetRunForTest swaps the command runner Osascript uses and returns a func
// that restores the previous one.
//
// It is in a non-test file so that another package's test binary can arm a
// failing default too: internal/cli's TestMain does, because the daemon
// builds a real Osascript on darwin. An export_test.go helper would compile
// only into this package's own test binary (F103, the same reason as
// shim.SetSeamsForTest). Production never calls it.
func SetRunForTest(fn func(ctx context.Context, name string, args []string) error) (restore func()) {
	orig := runFn
	runFn = fn
	return func() { runFn = orig }
}
