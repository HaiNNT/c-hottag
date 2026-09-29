package proxy_test

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

// The goldens are the bytes the proxy writes TODAY (main at 649d63b) for
// three kinds of traffic, with the three per-run values removed by
// normalizedLines: the start time "t", the duration "ms" and a stream's
// random pairing "id". They are run against the unchanged tree first (M2c
// T1 Step 2), so "proxy.jsonl is unchanged" is checked against today, not
// against the new code's own output. If a literal here does not match
// today's bytes, fix the LITERAL to today's line and say so in the report:
// the literal's only job is to record today.
const (
	goldenJSON            = `{"kind":"req","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","auth":"none","status":200,"reqType":"application/json","respType":"application/json"}`
	goldenJSONShapes      = `{"kind":"req","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","auth":"none","status":200,"reqType":"application/json","respType":"application/json","reqShape":{"model":"string","n":"number"},"respShape":{"ok":"bool"}}`
	goldenHead            = `{"kind":"head","id":"ID","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","auth":"none","status":200,"reqType":"application/json","respType":"text/event-stream"}`
	goldenStreamReq       = `{"kind":"req","id":"ID","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","auth":"none","status":200,"reqType":"application/json","respType":"text/event-stream"}`
	goldenStreamReqShapes = `{"kind":"req","id":"ID","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","auth":"none","status":200,"reqType":"application/json","respType":"text/event-stream","reqShape":{}}`
	goldenTunnel          = `{"kind":"tunnel","form":"blind","host":"echo.example.org:443","status":200}`
)

// goldenPlain is proxy.jsonl for driveGoldenTraffic; goldenShaped is the
// same traffic with shapes (`trace run --shapes`, and the daemon's
// trace.jsonl). A head record never carries shapes.
var (
	goldenPlain  = []string{goldenJSON, goldenHead, goldenStreamReq, goldenTunnel}
	goldenShaped = []string{goldenJSONShapes, goldenHead, goldenStreamReqShapes, goldenTunnel}

	normT  = regexp.MustCompile(`"t":"[^"]*",`)
	normMS = regexp.MustCompile(`,"ms":[0-9]+`)
	normID = regexp.MustCompile(`"id":"[0-9a-f]{8}"`)
)

// normalizedLines reads a JSONL file and removes the per-run values. A
// missing or empty file is no lines.
func normalizedLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		line = normT.ReplaceAllString(line, "")
		line = normMS.ReplaceAllString(line, "")
		line = normID.ReplaceAllString(line, `"id":"ID"`)
		out = append(out, line)
	}
	return out
}

func assertLines(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s =\n%s\nwant\n%s", label, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// goldenUpstream answers `{}` with a one-event stream and anything else
// with a small JSON object.
func goldenUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "{}" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: RESP-BODY-SECRET\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
}

// driveGoldenTraffic sends a JSON request, a stream and a blind tunnel,
// waiting for each one's record (wait) before the next, so the file order
// is fixed: req, head, req, tunnel.
func driveGoldenTraffic(t *testing.T, h *proxytest.Harness, wait func(kind string, n int)) {
	t.Helper()
	post := func(body string) {
		req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	post(`{"model":"hello","n":1}`)
	wait("req", 1)
	post(`{}`)
	wait("req", 2)
	c, br, status := connect(t, h, "echo.example.org:443")
	if status != http.StatusOK {
		t.Fatalf("CONNECT status %d", status)
	}
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	c.Close()
	wait("tunnel", 1)
}

// TestProxyJSONLGoldenWithTraceOff is today's `proxy run` configuration:
// the one log, no shapes.
func TestProxyJSONLGoldenWithTraceOff(t *testing.T) {
	h := proxytest.Start(t, goldenUpstream(), proxytest.Options{})
	driveGoldenTraffic(t, h, func(k string, n int) { h.Records(t, k, n) })
	assertLines(t, "proxy.jsonl", normalizedLines(t, h.LogPath), goldenPlain)
}

// TestTraceRunShapesGoldenIsUnchanged is `trace run --shapes` (spec §6:
// "a golden test"): one log, with shapes.
func TestTraceRunShapesGoldenIsUnchanged(t *testing.T) {
	h := proxytest.Start(t, goldenUpstream(), proxytest.Options{Shapes: true})
	driveGoldenTraffic(t, h, func(k string, n int) { h.Records(t, k, n) })
	assertLines(t, "trace run --shapes log", normalizedLines(t, h.LogPath), goldenShaped)
}
