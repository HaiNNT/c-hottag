package proxy_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

// gatedStream is an SSE upstream that sends one event, flushes, then holds
// the stream open until release is called. release is idempotent. The
// caller must `defer release()`: a deferred call runs before any t.Cleanup,
// and the harness's cleanup closes the upstream httptest.Server, whose
// Close blocks until every handler returns. A t.Cleanup(release) registered
// here, before proxytest.Start, would run AFTER that Close (cleanups are
// LIFO) and hang a failing test until the go test timeout.
func gatedStream(t *testing.T, contentType string) (http.Handler, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-gate
		io.WriteString(w, "data: two\n\n")
	}), release
}

// sseUpstream answers every request with a complete two-event stream.
func sseUpstream(status int, extra http.Header) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		for k, vs := range extra {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		io.WriteString(w, "data: RESP-BODY-SECRET\n\n")
		io.WriteString(w, "data: two\n\n")
	})
}

// postStream sends a JSON POST to /v1/messages with the home bearer and
// reads the whole response.
func postStream(t *testing.T, h *proxytest.Harness, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+homeTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp
}

// rawLines returns every line of the trace log decoded into a generic map,
// so a test sees exactly the keys on disk, not a re-marshalled Record.
func rawLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("bad trace line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func kindCount(lines []map[string]any, kind string) int {
	n := 0
	for _, m := range lines {
		if m["kind"] == kind {
			n++
		}
	}
	return n
}

var hexID = regexp.MustCompile(`^[0-9a-f]{8}$`)

func TestStreamHeadIsWrittenWhileTheStreamIsOpen(t *testing.T) {
	// Mixed case on purpose: detection must be case-insensitive.
	up, release := gatedStream(t, "Text/Event-Stream; charset=utf-8")
	defer release()
	h := proxytest.Start(t, up, proxytest.Options{})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if l, err := br.ReadString('\n'); err != nil || l != "data: one\n" {
		t.Fatalf("first event %q err %v", l, err)
	}

	head := h.Records(t, "head", 1)[0]
	if n := kindCount(rawLines(t, h.LogPath), "req"); n != 0 {
		t.Fatalf("%d req records while the stream is still open, want 0", n)
	}
	if !hexID.MatchString(head.ID) || head.Status != 200 || head.Path != "/v1/messages" {
		t.Fatalf("head %+v", head)
	}

	release()
	io.ReadAll(br)
	final := h.Records(t, "req", 1)[0]
	if final.ID != head.ID {
		t.Fatalf("req id %q, head id %q: want the same", final.ID, head.ID)
	}
	lines := rawLines(t, h.LogPath)
	if len(lines) != 2 || lines[0]["kind"] != "head" || lines[1]["kind"] != "req" {
		t.Fatalf("want exactly [head, req], got %v", lines)
	}
}

func TestStreamHeadIsASubsetOfItsReq(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(200, nil), proxytest.Options{Choose: servingSwap{}, Shapes: true})
	postStream(t, h, `{"model":"m"}`)
	h.Records(t, "req", 1)
	var head, final map[string]any
	for _, m := range rawLines(t, h.LogPath) {
		switch m["kind"] {
		case "head":
			head = m
		case "req":
			final = m
		}
	}
	if head == nil || final == nil {
		t.Fatalf("want a head and a req, got head=%v req=%v", head, final)
	}
	for k, v := range head {
		if k == "kind" {
			continue
		}
		if !reflect.DeepEqual(final[k], v) {
			t.Errorf("head %s = %v, req %s = %v", k, v, k, final[k])
		}
	}
	for _, k := range []string{"ms", "err", "reqShape", "respShape"} {
		if _, ok := head[k]; ok {
			t.Errorf("head carries %q", k)
		}
	}
	// Non-vacuous: the req really has the body-derived field the head must
	// not, and the head really carries the swap.
	if _, ok := final["reqShape"]; !ok {
		t.Fatalf("req has no reqShape; the exclusion check above proved nothing: %v", final)
	}
	if head["account"] != "B" || head["swapped"] != true {
		t.Fatalf("head lacks the swap: %v", head)
	}
}

func TestStreamHeadCarriesTheLimitFingerprint(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}), proxytest.Options{LimitFingerprint: true})
	postStream(t, h, `{}`)
	final := h.Records(t, "req", 1)[0]
	head := h.Records(t, "head", 1)[0]
	if head.Status != 429 || len(head.RespHeaderNames) == 0 || head.RespLimitHeaders["Retry-After"] != "30" {
		t.Fatalf("head fingerprint %+v", head)
	}
	if !reflect.DeepEqual(head.RespHeaderNames, final.RespHeaderNames) || !reflect.DeepEqual(head.RespLimitHeaders, final.RespLimitHeaders) {
		t.Fatalf("head %+v\nreq %+v", head, final)
	}
}

func TestNonStreamResponseWritesOneRecordWithNoID(t *testing.T) {
	seen := make(chan string, 1)
	h := proxytest.Start(t, authEcho(seen), proxytest.Options{})
	do(t, h, "GET", "https://api.anthropic.com/v1/models")
	h.Records(t, "req", 1)
	lines := rawLines(t, h.LogPath)
	if len(lines) != 1 {
		t.Fatalf("want one record, got %v", lines)
	}
	if _, ok := lines[0]["id"]; ok {
		t.Fatalf("non-stream record carries an id: %v", lines[0])
	}
}

func TestTraceOffWritesNoStreamRecords(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(200, nil), proxytest.Options{NoTrace: true})
	if resp := postStream(t, h, `{}`); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	st, err := os.Stat(h.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 0 {
		b, _ := os.ReadFile(h.LogPath)
		t.Fatalf("trace off, but the log has %q", b)
	}
}

func TestFailureBeforeHeadersWritesOnlyTheReq(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(200, nil), proxytest.Options{DownHosts: []string{"api.anthropic.com"}})
	if resp := postStream(t, h, `{}`); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	final := h.Records(t, "req", 1)[0]
	if final.Err == "" || final.ID != "" {
		t.Fatalf("req %+v, want an err and no id", final)
	}
	if n := kindCount(rawLines(t, h.LogPath), "head"); n != 0 {
		t.Fatalf("%d head records for a request that never got headers", n)
	}
}

// TestStreamAbortedMidBodyKeepsThePair: the upstream dies after the first
// event, so the head is written and the req follows with the same id and the
// finaliser's "aborted mid-body" error (the path TestAbortedMidBodyIsTraced
// pins for the req alone).
func TestStreamAbortedMidBodyKeepsThePair(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}), proxytest.Options{})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	final := h.Records(t, "req", 1)[0]
	head := h.Records(t, "head", 1)[0]
	if !strings.Contains(final.Err, "aborted") || final.ID == "" || final.ID != head.ID || head.Err != "" {
		t.Fatalf("head %+v\nreq %+v", head, final)
	}
}

func TestStreamRecordsCarryNoBearerOrBody(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(200, nil), proxytest.Options{Choose: servingSwap{}, Shapes: true, LimitFingerprint: true})
	postStream(t, h, `{"prompt":"REQ-BODY-SECRET"}`)
	h.Records(t, "req", 1)
	h.Records(t, "head", 1)
	for _, secret := range []string{"HOME-SECRET", "OTHER-SECRET", "REQ-BODY-SECRET", "RESP-BODY-SECRET"} {
		if h.LogContains(t, secret) {
			t.Errorf("%s leaked into the trace log", secret)
		}
	}
}
