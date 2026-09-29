package proxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// openTestTraceLog is the daemon's second log for a test: an append-only
// file in a temp dir. Registered before proxytest.Start, so it closes after
// the harness's servers.
func openTestTraceLog(t *testing.T) (*tracelog.Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace-mode.jsonl")
	w, err := tracelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, path
}

func always(v bool) func() bool { return func() bool { return v } }

// TestTracingOffWritesTodaysProxyJSONLAndNoTraceLog is spec §6's first
// proxy bullet: the daemon's configuration (a trace log wired) with
// tracing off writes today's proxy.jsonl bytes and nothing else.
func TestTracingOffWritesTodaysProxyJSONLAndNoTraceLog(t *testing.T) {
	tw, tpath := openTestTraceLog(t)
	h := proxytest.Start(t, goldenUpstream(), proxytest.Options{Tracing: always(false), TraceLog: tw})
	driveGoldenTraffic(t, h, func(k string, n int) { h.Records(t, k, n) })
	assertLines(t, "proxy.jsonl", normalizedLines(t, h.LogPath), goldenPlain)
	assertLines(t, "trace.jsonl", normalizedLines(t, tpath), nil)
}

// TestTracingOnSplitsShapesOutOfProxyJSONL is T5: while tracing, the trace
// log gets the shaped records and proxy.jsonl still gets exactly today's.
func TestTracingOnSplitsShapesOutOfProxyJSONL(t *testing.T) {
	tw, tpath := openTestTraceLog(t)
	h := proxytest.Start(t, goldenUpstream(), proxytest.Options{Tracing: always(true), TraceLog: tw})
	driveGoldenTraffic(t, h, func(k string, n int) { h.Records(t, k, n) })
	assertLines(t, "proxy.jsonl", normalizedLines(t, h.LogPath), goldenPlain)
	assertLines(t, "trace.jsonl", normalizedLines(t, tpath), goldenShaped)
}

// TestTraceLogAloneStillGetsHeads: `proxy run --log ""` (no proxy.jsonl)
// while tracing must still write the stream's head to the trace log.
func TestTraceLogAloneStillGetsHeads(t *testing.T) {
	tw, tpath := openTestTraceLog(t)
	h := proxytest.Start(t, goldenUpstream(), proxytest.Options{NoTrace: true, Tracing: always(true), TraceLog: tw})
	driveGoldenTraffic(t, h, func(k string, n int) { h.RecordsIn(t, tpath, k, n) })
	assertLines(t, "trace.jsonl", normalizedLines(t, tpath), goldenShaped)
	assertLines(t, "proxy.jsonl", normalizedLines(t, h.LogPath), nil)
}

// TestTracingIsEvaluatedOncePerRequest flips the switch while the request
// is at the upstream. The request keeps the answer it started with: one
// consistent record (both shapes or neither), and Tracing called once.
func TestTracingIsEvaluatedOncePerRequest(t *testing.T) {
	for _, startOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("starts %v", startOn), func(t *testing.T) {
			var on atomic.Bool
			on.Store(startOn)
			var calls atomic.Int32
			tw, tpath := openTestTraceLog(t)
			h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				on.Store(!startOn) // the switch flips mid-request
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true}`)
			}), proxytest.Options{Tracing: func() bool { calls.Add(1); return on.Load() }, TraceLog: tw})
			req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"hello","n":1}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			h.Records(t, "req", 1)
			if got := calls.Load(); got != 1 {
				t.Errorf("Tracing called %d times for one MITM request, want 1", got)
			}
			assertLines(t, "proxy.jsonl", normalizedLines(t, h.LogPath), []string{goldenJSON})
			var want []string
			if startOn {
				want = []string{goldenJSONShapes}
			}
			assertLines(t, "trace.jsonl", normalizedLines(t, tpath), want)
		})
	}
}

// TestTracingIsEvaluatedOncePerRequestStream is the stream analogue of
// TestTracingIsEvaluatedOncePerRequest: the upstream flips the switch
// before it writes a single response byte, so ModifyResponse's head-time
// emit (forward.go) runs after the flip. Tracing must still be called
// exactly once, and the head and its req must agree: both traced, or
// neither.
func TestTracingIsEvaluatedOncePerRequestStream(t *testing.T) {
	for _, startOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("starts %v", startOn), func(t *testing.T) {
			var on atomic.Bool
			on.Store(startOn)
			var calls atomic.Int32
			tw, tpath := openTestTraceLog(t)
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			defer release()
			h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				on.Store(!startOn) // the switch flips before any response byte is sent
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: one\n\n")
				w.(http.Flusher).Flush()
				<-gate
				io.WriteString(w, "data: two\n\n")
			}), proxytest.Options{Tracing: func() bool { calls.Add(1); return on.Load() }, TraceLog: tw})
			req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
			resp, err := h.Client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(resp.Body)
			if _, err := br.ReadString('\n'); err != nil {
				t.Fatalf("first event: %v", err)
			}
			release()
			io.ReadAll(br)
			resp.Body.Close()
			h.Records(t, "req", 1)
			if got := calls.Load(); got != 1 {
				t.Errorf("Tracing called %d times for one stream request, want 1", got)
			}
			lines := rawLines(t, tpath)
			switch {
			case startOn && (len(lines) != 2 || lines[0]["kind"] != "head" || lines[1]["kind"] != "req"):
				t.Fatalf("trace.jsonl = %v, want exactly [head, req]", lines)
			case !startOn && len(lines) != 0:
				t.Fatalf("trace.jsonl = %v, want neither head nor req", lines)
			}
		})
	}
}

// TestTracingIsEvaluatedOncePerRequestBlindTunnel is the blind-tunnel
// analogue: the switch flips while the tunnel is open, well after blind's
// one s.tracing() read at connect time, and before the tunnel closes and
// writes its record (connect.go). Tracing must be called exactly once, and
// the record must land in the log chosen at connect time.
func TestTracingIsEvaluatedOncePerRequestBlindTunnel(t *testing.T) {
	for _, startOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("starts %v", startOn), func(t *testing.T) {
			var on atomic.Bool
			on.Store(startOn)
			var calls atomic.Int32
			tw, tpath := openTestTraceLog(t)
			h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{Tracing: func() bool { calls.Add(1); return on.Load() }, TraceLog: tw})
			c, br, status := connect(t, h, "echo.example.org:443")
			if status != http.StatusOK {
				t.Fatalf("CONNECT status %d", status)
			}
			on.Store(!startOn) // the switch flips while the tunnel is open
			io.WriteString(c, "ping")
			buf := make([]byte, 4)
			if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
				t.Fatalf("echo got %q err %v", buf, err)
			}
			c.Close()
			h.Records(t, "tunnel", 1)
			if got := calls.Load(); got != 1 {
				t.Errorf("Tracing called %d times for one blind tunnel, want 1", got)
			}
			var want []string
			if startOn {
				want = []string{goldenTunnel}
			}
			assertLines(t, "trace.jsonl", normalizedLines(t, tpath), want)
		})
	}
}

func getWithUA(t *testing.T, h *proxytest.Harness, ua string, n int) {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	req.Header.Set("User-Agent", ua)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	h.Records(t, "req", n)
}

// TestOnClaudeVersionGetsOnlyATracedRequestsVersion: the callback sees the
// captured version of a traced request, and nothing otherwise.
func TestOnClaudeVersionGetsOnlyATracedRequestsVersion(t *testing.T) {
	cases := []struct {
		name    string
		tracing bool
		ua      string
		want    []string
	}{
		{"traced", true, "claude-cli/2.1.282 (external, cli)", []string{"2.1.282"}},
		{"untraced", false, "claude-cli/2.1.282 (external, cli)", nil},
		{"traced foreign", true, "evil/2.1.282 (external, cli)", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mu sync.Mutex
			var got []string
			h := proxytest.Start(t, goldenUpstream(), proxytest.Options{
				Tracing:         always(c.tracing),
				OnClaudeVersion: func(v string) { mu.Lock(); got = append(got, v); mu.Unlock() },
			})
			getWithUA(t, h, c.ua, 1)
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("OnClaudeVersion got %q, want %q", got, c.want)
			}
		})
	}
}

// TestOnClaudeVersionPanicNeverFailsTheRequest: a broken consumer is
// reported as a fixed string, and the client still gets its response.
func TestOnClaudeVersionPanicNeverFailsTheRequest(t *testing.T) {
	var mu sync.Mutex
	var errs []string
	h := proxytest.Start(t, goldenUpstream(), proxytest.Options{
		Tracing:         always(true),
		OnClaudeVersion: func(string) { panic("claude-cli/2.1.282 SECRET-ISH") },
		OnLogError:      func(err error) { mu.Lock(); errs = append(errs, err.Error()); mu.Unlock() },
	})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.282 (external, cli)")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	h.Records(t, "req", 1)
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 1 || errs[0] != "OnClaudeVersion callback panicked" {
		t.Fatalf("OnLogError got %q, want exactly the fixed string", errs)
	}
}
