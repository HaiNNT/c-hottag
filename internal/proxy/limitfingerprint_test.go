package proxy_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

// TestLimitFingerprintDecodesGzipBody guards against a real client's own
// Accept-Encoding: gzip passing straight through to upstream when
// --limit-fingerprint is the only flag on (no --shapes, no swapping): left
// alone, the response comes back gzip-compressed, json.Unmarshal in
// tracelog.FingerprintLimit fails silently, and every fingerprint field
// stays empty.
func TestLimitFingerprintDecodesGzipBody(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		io.WriteString(gz, `{"error":{"type":"rate_limit_error","message":"You've hit your weekly limit"}}`)
		gz.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(buf.Bytes())
	}), proxytest.Options{LimitFingerprint: true})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	r := h.Records(t, "req", 1)[0]
	if r.RespErrorType != "rate_limit_error" {
		t.Fatalf("RespErrorType = %q, want %q (gzip body was never decoded before fingerprinting)", r.RespErrorType, "rate_limit_error")
	}
}

// TestLimitFingerprintNeverTruncatesResponseToClient guards against the
// exact shape of two bugs this repo has already hit: a large refusal body
// (here 3 MB) must reach the client byte-identically even though
// --limit-fingerprint reads part of it for the fingerprint.
func TestLimitFingerprintNeverTruncatesResponseToClient(t *testing.T) {
	padding := bytes.Repeat([]byte("A"), 3*1024*1024)
	body := append([]byte(`{"error":{"type":"rate_limit_error","message":"x"},"padding":"`), append(padding, []byte(`"}`)...)...)

	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(body)
	}), proxytest.Options{LimitFingerprint: true})

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("client received %d bytes, want %d byte-identical bytes", len(got), len(body))
	}
}
