package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

func errResp(status int, ctype, body string) *http.Response {
	h := http.Header{}
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestErrTypeOf(t *testing.T) {
	big := `{"type":"error","error":{"type":"api_error","message":"` + strings.Repeat("x", maxErrTypeBytes) + `"}}`
	cases := []struct {
		name string
		resp *http.Response
		want string
	}{
		{"known", errResp(401, "application/json", `{"type":"error","error":{"type":"authentication_error","message":"secret words"}}`), "authentication_error"},
		{"known with charset", errResp(404, "application/json; charset=utf-8", `{"error":{"type":"not_found_error"}}`), "not_found_error"},
		{"unknown value", errResp(403, "application/json", `{"error":{"type":"made up\nvalue"}}`), "other"},
		{"no type", errResp(401, "application/json", `{"error":{"message":"m"}}`), ""},
		{"not json body", errResp(401, "application/json", `<html>`), ""},
		{"not json content type", errResp(401, "text/html", `{"error":{"type":"api_error"}}`), ""},
		{"success status", errResp(200, "application/json", `{"error":{"type":"api_error"}}`), ""},
		{"body past the bound is unparseable", errResp(500, "application/json", big), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := ""
			if c.resp.Body != nil {
				// remember the body to compare after replay
				b, _ := io.ReadAll(c.resp.Body)
				orig = string(b)
				c.resp.Body = io.NopCloser(strings.NewReader(orig))
			}
			if got := errTypeOf(c.resp); got != c.want {
				t.Fatalf("errTypeOf = %q, want %q", got, c.want)
			}
			b, _ := io.ReadAll(c.resp.Body)
			if string(b) != orig {
				t.Fatalf("the body was changed: %d bytes, want %d", len(b), len(orig))
			}
		})
	}
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestErrTypeOfDecodesGzipForParsingAndReplaysTheOriginalBytes(t *testing.T) {
	comp := gz(t, `{"type":"error","error":{"type":"authentication_error","message":"m"}}`)
	resp := &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}},
		Body: io.NopCloser(bytes.NewReader(comp))}
	if got := errTypeOf(resp); got != "authentication_error" {
		t.Fatalf("errTypeOf = %q", got)
	}
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(b, comp) || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("the compressed bytes or headers changed")
	}
}

func TestErrTypeOfBadOrTruncatedGzipOrOtherEncodingGivesNothingAndReplays(t *testing.T) {
	comp := gz(t, `{"error":{"type":"api_error"}}`)
	for name, c := range map[string]struct {
		enc  string
		body []byte
	}{
		"truncated": {"gzip", comp[:len(comp)/2]},
		"not gzip":  {"gzip", []byte("plain")},
		"br":        {"br", comp},
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {c.enc}},
				Body: io.NopCloser(bytes.NewReader(c.body))}
			if got := errTypeOf(resp); got != "" {
				t.Fatalf("errTypeOf = %q, want empty", got)
			}
			if b, _ := io.ReadAll(resp.Body); !bytes.Equal(b, c.body) {
				t.Fatal("the body was changed")
			}
		})
	}
}
