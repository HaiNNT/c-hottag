package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// TestSlotSwapperCachesFailedRead guards against re-running the (possibly
// slow, macOS Keychain-backed) read on every single swapped request while a
// slot is unusable: a failed read must be cached for 30s like a successful
// one, and the failure warned about only once per failure window, not once
// per request.
func TestSlotSwapperCachesFailedRead(t *testing.T) {
	calls := 0
	readErr := errors.New("keychain locked")
	var warnBuf bytes.Buffer
	s := &slotSwapper{
		dirs: map[router.Class]string{router.Serving: "/some/dir"},
		read: func(string) (creds.Token, error) {
			calls++
			return creds.Token{}, readErr
		},
		warn:  &warnBuf,
		cache: map[router.Class]cachedToken{},
	}

	for i := 0; i < 3; i++ {
		if _, ok := s.token(router.Serving); ok {
			t.Fatal("expected swap to be skipped on read failure")
		}
	}

	if calls != 1 {
		t.Fatalf("read called %d times, want 1 (failure should be cached for 30s)", calls)
	}
	if n := strings.Count(warnBuf.String(), "slot unusable"); n != 1 {
		t.Fatalf("warned %d times, want 1 per failure window: %q", n, warnBuf.String())
	}
}

// TestParseIntercept checks that --intercept suffixes are normalized
// (trimmed, lower-cased) and that empty entries (e.g. a trailing comma) are
// dropped, so router.SuffixMatcher never compares against " Foo.Com " or "".
func TestParseIntercept(t *testing.T) {
	got := parseIntercept(" Anthropic.com, ,CLAUDE.AI , claude.com ,")
	want := []string{"anthropic.com", "claude.ai", "claude.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestTraceRunConfigTracingFollowsShapes pins traceRunConfig's wiring
// (trace.go): `trace run --shapes` must make cfg.Tracing() report true and
// plain `trace run` false, and TraceLog must stay nil — the one log
// (Log) carries the shapes itself when Tracing is true, exactly as
// Config.Shapes did before M2c split it into Config.Tracing/TraceLog.
// Mutation: changing the literal's `shapesOn` to `shapesOn && false` must
// fail this test.
func TestTraceRunConfigTracingFollowsShapes(t *testing.T) {
	for _, shapesOn := range []bool{true, false} {
		cfg := traceRunConfig(nil, proxyauth.Secret{}, nil, nil, shapesOn, false, nil)
		if cfg.TraceLog != nil {
			t.Fatalf("shapesOn=%v: TraceLog = %v, want nil", shapesOn, cfg.TraceLog)
		}
		if cfg.Tracing == nil {
			t.Fatalf("shapesOn=%v: Tracing is nil", shapesOn)
		}
		if got := cfg.Tracing(); got != shapesOn {
			t.Fatalf("shapesOn=%v: cfg.Tracing() = %v, want %v", shapesOn, got, shapesOn)
		}
	}
}

// TestSwapPreflightRejectsExpiredToken checks that an expired slot token
// fails the --swap preflight with a clear message (and never prints the
// "swap CLASS -> DIR" success line), instead of silently running with a
// slot that will pass through requests unswapped from the first use.
func TestSwapPreflightRejectsExpiredToken(t *testing.T) {
	var buf bytes.Buffer
	read := func(dir string) (creds.Token, error) {
		return creds.Token{AccessToken: "tok", ExpiresAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
	}
	swaps := swapFlags{router.Serving: "/some/dir"}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if swapPreflight(swaps, read, &buf, now) {
		t.Fatal("expected preflight to fail for an expired token")
	}
	out := buf.String()
	if !strings.Contains(out, "token expired at") || !strings.Contains(out, "CLAUDE_CONFIG_DIR=/some/dir claude auth status") {
		t.Fatalf("message: %q", out)
	}
	if strings.Contains(out, "swap serving -> /some/dir:") {
		t.Fatalf("must not print a success line for an expired token: %q", out)
	}
}
