package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// maxErrTypeBytes is the most of an error response's body read to find its
// error type (R177).
const maxErrTypeBytes = 64 << 10

// errTypes are the error.type values of Anthropic's API errors that are
// recorded as they are; any other value is recorded as "other", so a body
// can never put text of its own into a trace record or a log line.
var errTypes = map[string]bool{
	"invalid_request_error": true, "authentication_error": true, "permission_error": true,
	"not_found_error": true, "request_too_large": true, "rate_limit_error": true,
	"api_error": true, "overloaded_error": true,
}

// errTypeOf returns the allowlisted error.type of a JSON error response
// (status 400 or more), "other" for a type that is not on the list and ""
// for a missing or unparseable one. It reads at most maxErrTypeBytes and
// puts the bytes back (a gzip body is decoded from a bounded copy for parsing
// only; any other content encoding gives ""), so the client still receives the exact same body.
// Neither the message nor the body is kept.
func errTypeOf(resp *http.Response) string {
	if resp == nil || resp.Body == nil || resp.StatusCode < 400 || !isJSON(resp.Header.Get("Content-Type")) {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrTypeBytes))
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), rest), rest}
	parse := buf
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		// Only for parsing: the client still gets the original bytes.
		zr, err := gzip.NewReader(bytes.NewReader(buf))
		if err != nil {
			return ""
		}
		parse, _ = io.ReadAll(io.LimitReader(zr, maxErrTypeBytes))
	default:
		return ""
	}
	var v struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(parse, &v) != nil || v.Error.Type == "" {
		return ""
	}
	if errTypes[v.Error.Type] {
		return v.Error.Type
	}
	return "other"
}
