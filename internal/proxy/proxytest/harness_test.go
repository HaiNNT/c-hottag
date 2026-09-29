package proxytest

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// TestProxyDialAddrDefaultsThePort mirrors
// proxy.TestProxyHostPortDefaultsThePort case-for-case: proxyDialAddr is a
// deliberate duplicate of the unexported proxy.proxyHostPort (this test
// package cannot reach it directly), kept here only so the harness's own
// DialContext stub recognises the same address dialThroughProxy actually
// dials. If the two ever drift, one of these two tests should start
// disagreeing with what its own case table says — keep the case tables
// identical when changing either.
func TestProxyDialAddrDefaultsThePort(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"http, no port, defaults to 80", "http://proxy.example.org", "proxy.example.org:80"},
		{"https, no port, defaults to 443", "https://proxy.example.org", "proxy.example.org:443"},
		{"http, explicit port, kept as-is", "http://proxy.example.org:8080", "proxy.example.org:8080"},
		{"https, explicit port, kept as-is", "https://proxy.example.org:8443", "proxy.example.org:8443"},
		{"IP host, no port, defaults to 80", "http://127.0.0.1", "127.0.0.1:80"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := proxyDialAddr(u); got != c.want {
				t.Errorf("proxyDialAddr(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestRecordsFailsLoudlyOnAnUnreadableLog guards against Records silently
// discarding its tracelog.ReadAll error: without that, an unreadable log
// presents to every caller as "got 0 records" instead of failing for the
// real reason.
//
// t.Fatalf makes the calling test fail, so exercising it directly (e.g. via
// t.Run) would fail THIS test too, regardless of whether the fatal fired
// for the right reason. Instead this re-execs the test binary as a helper
// process (the standard library's own pattern, e.g. os/exec's
// TestHelperProcess) and inspects its failure.
func TestRecordsFailsLoudlyOnAnUnreadableLog(t *testing.T) {
	if os.Getenv("PROXYTEST_UNREADABLE_LOG_HELPER") == "1" {
		// Short, so a Records that swallowed the read error and kept
		// polling fails here in seconds, not at the minute-long guard.
		recordsHangGuard = 3 * time.Second
		h := Start(t, http.NotFoundHandler(), Options{})
		// Start's own tracelog.Open already succeeded, so removing LogPath
		// here exercises exactly (and only) Records' later ReadAll
		// failure, not a setup problem.
		if err := os.Remove(h.LogPath); err != nil {
			t.Fatal(err)
		}
		h.Records(t, "req", 1)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRecordsFailsLoudlyOnAnUnreadableLog$")
	cmd.Env = append(os.Environ(), "PROXYTEST_UNREADABLE_LOG_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper process succeeded, want it to fail via Records' t.Fatalf; output:\n%s", out)
	}
	if !strings.Contains(string(out), "proxytest: reading") {
		t.Fatalf("helper process failed, but not for the expected reason; output:\n%s", out)
	}
}

// TestRecordsWaitsPastTheOldTwoSecondBound is F155's regression proof: a
// record that lands 2.5s after the call is found. Before the fix, Records
// gave up at a fixed 2s, and under full-suite -race load a proxy that
// noticed a client's cancel late failed TestClientCancelIsNotLoggedAs502.
func TestRecordsWaitsPastTheOldTwoSecondBound(t *testing.T) {
	t.Parallel()
	h := Start(t, http.NotFoundHandler(), Options{})
	go func() {
		time.Sleep(2500 * time.Millisecond)
		if err := h.logWriter.Write(tracelog.Record{Kind: "req", Method: "GET"}); err != nil {
			t.Errorf("writing the late record: %v", err)
		}
	}()
	if got := h.Records(t, "req", 1); len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
}

// TestRecordsGivesUpAtTheHangGuard: a record that never arrives still
// fails the test, with the count and the wait in the message. Run in a
// helper process for the same reason as the test above it.
func TestRecordsGivesUpAtTheHangGuard(t *testing.T) {
	if os.Getenv("PROXYTEST_HANG_GUARD_HELPER") == "1" {
		recordsHangGuard = 300 * time.Millisecond
		h := Start(t, http.NotFoundHandler(), Options{})
		h.Records(t, "req", 1)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecordsGivesUpAtTheHangGuard$")
	cmd.Env = append(os.Environ(), "PROXYTEST_HANG_GUARD_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("helper process succeeded, want Records to fail at its hang guard; output:\n%s", out)
	}
	if !strings.Contains(string(out), `want 1 "req" records, got 0 after`) {
		t.Fatalf("helper process failed, but not at the hang guard; output:\n%s", out)
	}
}
