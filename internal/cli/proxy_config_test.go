package cli

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// TestParseUpstreamProxyRejectsNonHTTPScheme, unlike a full runProxy
// invocation, asserts the rejection directly: mutating the scheme check
// fails this test with an assertion, not a hang on the real default port
// (F6) — see TestProxyRunRejectsNonHTTPUpstreamProxy's comment in
// proxy_test.go for why the CLI-level test alone was not enough.
func TestParseUpstreamProxyRejectsNonHTTPScheme(t *testing.T) {
	_, err := parseUpstreamProxy("socks5://127.0.0.1:1080")
	if err == nil || !strings.Contains(err.Error(), "only http:// is supported") {
		t.Fatalf("err = %v, want a rejection naming http:// only", err)
	}
}

// TestParseUpstreamProxyRejectsNoHost mirrors the test above for the
// second half of the validation.
func TestParseUpstreamProxyRejectsNoHost(t *testing.T) {
	_, err := parseUpstreamProxy("http://")
	if err == nil || !strings.Contains(err.Error(), "no host") {
		t.Fatalf("err = %v, want a rejection naming the missing host", err)
	}
}

// TestParseUpstreamProxyAcceptsHTTP pins the positive case: a well-formed
// http:// value must parse through unchanged, not get caught by either
// rejection.
func TestParseUpstreamProxyAcceptsHTTP(t *testing.T) {
	u, err := parseUpstreamProxy("http://127.0.0.1:3128")
	if err != nil || u == nil || u.Host != "127.0.0.1:3128" {
		t.Fatalf("u, err = %v, %v; want a parsed http upstream", u, err)
	}
}

// TestParseUpstreamProxyEmptyIsNil pins the unset-flag case: nil, nil —
// not an error, and not a non-nil zero-value URL that downstream code
// might mistake for "route through the zero-value host".
func TestParseUpstreamProxyEmptyIsNil(t *testing.T) {
	u, err := parseUpstreamProxy("")
	if err != nil || u != nil {
		t.Fatalf("u, err = %v, %v; want nil, nil for an unset flag", u, err)
	}
}

// TestNewProxyConfigWiresUpstreamProxy pins the other half of F2's
// three-mutation matrix: `UpstreamProxy: upstreamURL` silently shrinking to
// `UpstreamProxy: nil` inside newProxyConfig compiles and would otherwise
// leave the whole suite green — no other test constructs a proxy.Config
// and inspects its UpstreamProxy field.
func TestNewProxyConfigWiresUpstreamProxy(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1:3128")
	if err != nil {
		t.Fatal(err)
	}
	cfg := newProxyConfig(nil, proxyauth.Secret{}, nil, nil, nil, nil, nil, u)
	if cfg.UpstreamProxy != u {
		t.Fatalf("UpstreamProxy = %v, want %v", cfg.UpstreamProxy, u)
	}
}

// TestWireProxyConfigRoutesErrorsToTheirOwnThrottle pins fix round 2, item
// 3: wireProxyConfig (proxy.go) is runProxyWithSignal's only call that pairs
// usageErrorThrottle and logErrorThrottle with proxy.Config's OnUsageError
// and OnLogError — two adjacent, same-typed func(error) arguments the
// reviewer transposed at that call site with the whole tree staying green
// (no other test builds a *proxy.Config from production's real throttles
// and inspects which callback prints which message). This calls
// wireProxyConfig itself — the actual production wiring, not a hand-rolled
// duplicate of it — then invokes each of the resulting two callbacks and
// asserts each one's message names the right subsystem. Mutation: swapping
// the two throttle arguments inside wireProxyConfig must fail this test.
func TestWireProxyConfigRoutesErrorsToTheirOwnThrottle(t *testing.T) {
	var stderr bytes.Buffer
	cfg := wireProxyConfig(&stderr, nil, proxyauth.Secret{}, nil, nil, nil, nil)

	stderr.Reset()
	cfg.OnUsageError(errors.New("boom"))
	if got := stderr.String(); !strings.Contains(got, "usage callback failed") || strings.Contains(got, "trace log write failed") {
		t.Errorf("OnUsageError wrote %q, want it to name the usage callback, not the trace log", got)
	}

	stderr.Reset()
	cfg.OnLogError(errors.New("boom"))
	if got := stderr.String(); !strings.Contains(got, "trace log write failed") || strings.Contains(got, "usage callback failed") {
		t.Errorf("OnLogError wrote %q, want it to name the trace log, not the usage callback", got)
	}
}
