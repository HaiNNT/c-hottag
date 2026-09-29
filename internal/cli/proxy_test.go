package cli_test

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/status"
)

// TestProxyRunRejectsNonLoopbackListen guards against ever binding the
// proxy (which forwards a bearer, and can refresh and resend one) to a
// non-loopback address, which would let any other host on the network
// reach it.
func TestProxyRunRejectsNonLoopbackListen(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	if code, _, errb := run(t, "proxy", "run", "--listen", "0.0.0.0:47821"); code != 2 || !strings.Contains(errb, "loopback") {
		t.Fatalf("non-loopback listen: %d %q", code, errb)
	}
}

// runBounded is like run, but returns via a timeout instead of hanging the
// whole suite (F6): used only where a regression in flag validation could
// let runProxy fall through to actually serving, which blocks forever with
// nothing in this package able to send it a shutdown signal.
// --listen 127.0.0.1:0 is a second, independent guard on the same tests:
// even if this bound were somehow not reached, an ephemeral port can never
// collide with a real one, unlike the fixed default port a stray "proxy
// run" invocation bound in the past.
func runBounded(t *testing.T, timeout time.Duration, args ...string) (int, string, string) {
	t.Helper()
	type result struct {
		code      int
		out, errb string
	}
	done := make(chan result, 1)
	go func() {
		var out, errb bytes.Buffer
		code := cli.Run("chottag", args, &out, &errb)
		done <- result{code, out.String(), errb.String()}
	}()
	select {
	case r := <-done:
		return r.code, r.out, r.errb
	case <-time.After(timeout):
		t.Fatalf("proxy run did not return within %s — validation stopped rejecting and fell through to serving instead", timeout)
		return 0, "", ""
	}
}

// TestProxyRunRejectsNonHTTPUpstreamProxy pins F44 at the runProxy wiring
// level: not parseUpstreamProxy's own rejection (see
// TestParseUpstreamProxyRejectsNonHTTPScheme in proxy_config_test.go,
// which asserts the same rejection directly and is what actually catches a
// mutation to the validation logic with a clean assertion rather than a
// timeout), but that runProxy calls it and acts on its error.
func TestProxyRunRejectsNonHTTPUpstreamProxy(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	if code, _, errb := runBounded(t, 2*time.Second, "proxy", "run", "--listen", "127.0.0.1:0", "--upstream-proxy", "socks5://127.0.0.1:1080"); code != 2 || !strings.Contains(errb, "only http:// is supported") {
		t.Fatalf("socks5 upstream-proxy: %d %q", code, errb)
	}
}

// TestProxyRunRejectsUpstreamProxyWithNoHost mirrors
// TestProxyRunRejectsNonHTTPUpstreamProxy above for the second half of the
// validation.
func TestProxyRunRejectsUpstreamProxyWithNoHost(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	if code, _, errb := runBounded(t, 2*time.Second, "proxy", "run", "--listen", "127.0.0.1:0", "--upstream-proxy", "http://"); code != 2 || !strings.Contains(errb, "no host") {
		t.Fatalf("hostless upstream-proxy: %d %q", code, errb)
	}
}

// TestProxyRunRejectsUpstreamProxyDoesNotEchoThePassword pins that a
// rejected --upstream-proxy value never puts its password on stderr: this
// is the only rule chottag treats as non-negotiable (never log, print or
// persist a secret), and stderr today is only a terminal, but M1c3 makes
// it a file. It asserts absence, never the redacted value's exact text —
// asserting presence of "xxxxx" would pin url.URL.Redacted()'s mask
// literal rather than the actual guarantee (no password on stderr).
func TestProxyRunRejectsUpstreamProxyDoesNotEchoThePassword(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	// https, not http: this is rejected by the scheme check (F44), not the
	// URL parser, so this also exercises the same code path a valid-but-
	// wrong-scheme URL takes — the exact shape of the reviewer's repro.
	code, _, errb := runBounded(t, 2*time.Second, "proxy", "run", "--listen", "127.0.0.1:0", "--upstream-proxy", "https://bob:hunter2@proxy:8080")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if strings.Contains(errb, "hunter2") {
		t.Fatalf("stderr contains the upstream-proxy password: %q", errb)
	}
	if !strings.Contains(errb, "only http:// is supported") {
		t.Fatalf("stderr = %q, want the usual scheme rejection still present", errb)
	}
}

// TestProxyRunRejectsSchemeLessUpstreamProxyDoesNotEchoThePassword pins the
// same guarantee as the test above for a scheme-less value — the shape
// http_proxy env vars and `curl -x` both accept, so this is the likely
// input, not an exotic one. url.Parse reads the part before the first
// colon as the URL's SCHEME here, not a userinfo username, and puts the
// rest in u.Opaque rather than u.User; url.URL.Redacted() only ever masks
// u.User, so it passes u.Opaque straight through and a naive redaction
// leaks the password verbatim (measured, before this fix: stderr =
// `chottag: --upstream-proxy "bob:hunter2@proxy:8080": only http://
// is supported`).
func TestProxyRunRejectsSchemeLessUpstreamProxyDoesNotEchoThePassword(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	code, _, errb := runBounded(t, 2*time.Second, "proxy", "run", "--listen", "127.0.0.1:0", "--upstream-proxy", "bob:hunter2@proxy:8080")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if strings.Contains(errb, "hunter2") {
		t.Fatalf("stderr contains the upstream-proxy password: %q", errb)
	}
}

// TestProxyRunPrintsExportLinesBeforeServing has moved to
// proxy_run_test.go (package cli), where it can call
// runProxyWithSignal directly with an injected signal channel: it starts a
// real daemon and, unbounded, would leak it running for the rest of the
// test binary's life with a real signal handler still armed (N3) — the
// injected channel lets its t.Cleanup stop that daemon with no real OS
// signal involved.

// TestProxyRunSeedsAndFlushesTheStatusCacheEvenWithoutTraffic pins the two
// runProxy-level pieces of contracts 2 and 4 that no white-box statusSink
// test can see: that runProxy itself calls seedRosterAtStartup before
// serving, and defers sink.flush() so that seed actually reaches disk. A
// reviewer found that deleting either line left the whole suite green,
// because every other test either calls the extracted hooks directly or
// never lets runProxy return at all.
//
// Occupying the listen address first makes http.ListenAndServe fail
// immediately and deterministically, so runProxy returns synchronously
// (via its defer) without a real server, a goroutine, or a timeout.
func TestProxyRunSeedsAndFlushesTheStatusCacheEvenWithoutTraffic(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A") // registers D and A: 2 configured accounts, no traffic ever sent

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	code, _, errb := runHome(t, home, "proxy", "run", "--listen", occupied.Addr().String())
	if code != 1 {
		t.Fatalf("proxy run against an already-occupied port = %d %q, want 1", code, errb)
	}

	f, err := status.Load(status.Path(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want both configured accounts seeded and flushed to disk before runProxy returned, even with zero traffic", f.Accounts)
	}
}
