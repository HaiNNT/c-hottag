//go:build !chottag_fakeusage

package cli

import (
	"io"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

// applyFakeUtil is the release build's twin of the live-check knob in
// fakeutil_on.go (M4 spec §8, S12). It reads nothing and returns a no-op:
// a release binary holds neither the knob's code nor its environment
// variable's name (cmd/chottag's TestReleaseBinaryHasNoFakeLimitHook).
func applyFakeUtil(func(string) string, func() (store.State, error), *statusSink, io.Writer, time.Time) func(time.Time) {
	return func(time.Time) {}
}
