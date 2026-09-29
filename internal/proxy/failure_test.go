package proxy_test

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

// TestMidStreamTruncationClosesTheClientConnection pins measured behaviour:
// an upstream that dies partway through a body gives the client
// io.ErrUnexpectedEOF, and the client's connection is gone — never a clean
// end of body. This is spec §6's "any upstream error/close closes the
// client side immediately", which the proxy already satisfies.
func TestMidStreamTruncationClosesTheClientConnection(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		w.Write([]byte("0123456789"))
		// Return without writing the declared 100 bytes. net/http flushes
		// what we wrote, notices the short write, and closes the connection
		// without keep-alive — the truncation F40 measured on a raw socket.
		// Do NOT Hijack here: Hijack discards net/http's buffered body, so
		// the upstream would send headers and no body at all, which is a
		// different scenario and the one case ReverseProxy races on.
	}), proxytest.Options{})

	resp, err := h.Client.Get("https://api.anthropic.com/v1/mcp_servers")
	if err != nil {
		t.Fatalf("expected headers to arrive: %v", err)
	}
	defer resp.Body.Close()
	_, readErr := io.ReadAll(resp.Body)
	if !errors.Is(readErr, io.ErrUnexpectedEOF) {
		t.Errorf("body read error = %v, want io.ErrUnexpectedEOF — a silent clean EOF would hide a truncated response from Claude Code", readErr)
	}
}

// TestUnreachableUpstreamReturns502 pins measured behaviour on the MITM
// path: a refused dial produces a 502 with an empty body, and the client's
// connection stays usable. See the ruling in the plan for why this is not
// changed to a connection close.
func TestUnreachableUpstreamReturns502(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		proxytest.Options{DownHosts: []string{"api.anthropic.com"}})

	resp, err := h.Client.Get("https://api.anthropic.com/v1/mcp_servers")
	if err != nil {
		t.Fatalf("expected a response, got %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 0 {
		t.Errorf("body = %q, want empty — chottag must not put its own text in a response Claude Code will parse", body)
	}
}
