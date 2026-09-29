package proxy_test

import (
	"bytes"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

// TestCloseClientTunnelsEndsALiveMITMTunnel pins F54: a MITM tunnel
// established before shutdown must not keep serving afterwards. Before this
// task the second request succeeded with 200 after the tunnel should have
// been torn down.
func TestCloseClientTunnelsEndsALiveMITMTunnel(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	}), proxytest.Options{})

	// First request establishes the MITM tunnel and leaves it keep-alive.
	resp, err := h.Client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if n := h.Server.CloseClientTunnels(); n < 1 {
		t.Fatalf("CloseClientTunnels closed %d tunnels, want >= 1: the live MITM client conn was never tracked", n)
	}

	// A second call must find nothing left to close: closeAll unconditionally
	// drains its set on every call, so this only proves CloseClientTunnels is
	// safe to call twice (no double-count, no panic on an empty set) — NOT
	// that the closed conn was individually deregistered via forget(). That
	// stronger claim would need to observe connSet's internal map, which
	// this external test package cannot reach; a mutation that breaks forget
	// (see conntrack.go) still passes this assertion, because the entry it
	// leaves behind was already closed once and closeOnce's sync.Once makes
	// a second close of the same entry a no-op either way.
	if n := h.Server.CloseClientTunnels(); n != 0 {
		t.Fatalf("a second CloseClientTunnels closed %d more, want 0: calling it again should be a safe no-op", n)
	}
}

// TestClosingClientTunnelEndsTheMITMGoroutine pins the other load-bearing
// half of F54: closing a tracked client conn must actually make mitm's
// srv.Serve(l) return, not just make CloseClientTunnels report a count. That
// depends on mitm's ConnState hook, which calls l.Close() when the tracked
// conn reaches StateClosed — without it, closing the conn ends the tunnel's
// I/O but the oneConnListener never unblocks, and the goroutine that called
// srv.Serve(l) leaks forever. That would later matter for Task 7's shutdown
// path, which is expected to wait on tunnels ending.
//
// There is no test hook inside package proxy to observe this goroutine
// directly, so this looks for it the same way a leak detector would: by its
// frame in a full stack dump, which names the blocked method unambiguously.
// Other tests in this package may leave their own mitm tunnels dangling
// (proxytest's harness cleanup closes the outer server, which is exactly
// what F54 says cannot reach a hijacked MITM tunnel), so this counts frames
// and checks the DELTA this test's own tunnel added, rather than asserting
// on an absolute count that a noisy suite could never guarantee is zero.
func TestClosingClientTunnelEndsTheMITMGoroutine(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), proxytest.Options{})

	before := mitmGoroutineCount()

	resp, err := h.Client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := mitmGoroutineCount(); got <= before {
		t.Fatalf("no new mitm goroutine appeared after establishing a tunnel: before=%d after=%d; test precondition broken, not what this test means to check", before, got)
	}

	if n := h.Server.CloseClientTunnels(); n < 1 {
		t.Fatalf("CloseClientTunnels closed %d tunnels, want >= 1", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := mitmGoroutineCount()
		if got <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a mitm goroutine is still running 2s after its client tunnel was closed (before=%d, now=%d): closing the tracked conn did not make srv.Serve(l) return (the ConnState hook that calls l.Close() on StateClosed is load-bearing)", before, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mitmGoroutineCount counts goroutines currently blocked inside
// (*Server).mitm, by grepping a full stack dump for its frame.
//
// runtime.Stack truncates silently when the dump does not fit buf: it never
// errors, it just returns n == len(buf) with whatever fit. n < len(buf) is
// the ONLY signal that the dump was not truncated. That truncation was real
// and measured, not hypothetical: while this package still leaked a MITM
// goroutine per tunnel (F54), the stack dump peaked at roughly 2.06 MB
// against a fixed 1<<20 (1 MB) buffer, making the count of "mitm(" frames an
// arbitrary function of where the dump got cut off rather than a real count,
// non-monotonic (it could go DOWN between two calls made milliseconds
// apart), and this test flaky. Task 7 fixed F54 by closing tunnels on
// shutdown, which removed that leak, so a fixed 1 MB buffer no longer
// overflows in this suite as it stands today (0 truncation events measured
// across 6 package runs). The grow-and-retry stays anyway, deliberately: it
// is free, and this guarantee should not depend on how much goroutine-leak
// debt other tests in the package happen to be carrying at the time. Do not
// "simplify" this back to a fixed buffer — the debt this was built for can
// return.
func mitmGoroutineCount() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte("github.com/HaiNNT/c-hottag/internal/proxy.(*Server).mitm("))
		}
		if len(buf) >= 1<<26 {
			panic("mitmGoroutineCount: stack dump did not fit even at 1<<26 bytes; a silently truncated count would be worse than a loud failure here")
		}
		buf = make([]byte, len(buf)*2)
	}
}

// TestCloseUpstreamsLeavesClientTunnelsAlone pins the wake/shutdown split: a
// wake must discard upstream sockets the peer forgot during suspend, and must
// NOT kill the user's live client tunnels.
func TestCloseUpstreamsLeavesClientTunnelsAlone(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), proxytest.Options{})

	resp, err := h.Client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	h.Server.CloseUpstreams() // simulate a wake

	if n := h.Server.CloseClientTunnels(); n < 1 {
		t.Fatalf("CloseClientTunnels found %d live client tunnels after CloseUpstreams ran, want >= 1: a wake must not kill live client tunnels", n)
	}
}
