//go:build !chottag_fakeusage

package cli

import (
	"io"
	"time"

	"github.com/HaiNNT/c-hottag/internal/store"
)

// applyFakeLimits is the release build's twin of the checklist-only hook in
// fakeusage_on.go (spec §10.1, R52). It does nothing and reads nothing: a
// release binary holds neither the hook's code nor its environment
// variable's name (cmd/chottag's TestReleaseBinaryHasNoFakeLimitHook).
func applyFakeLimits(func(string) string, func() (store.State, error), *statusSink, io.Writer, time.Time) {
}
