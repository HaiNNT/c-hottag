package tracelog_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func TestFingerprintLimit(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		header        http.Header
		body          string
		wantNames     []string // sorted; nil means "must be empty"
		wantHeaders   map[string]string
		wantErrType   string
		wantResetAt   string
		mustNotAppear []string // substrings that must not appear anywhere in the recorded fields
	}{
		{
			name:   "429 with rate-limit headers and error.type",
			status: 429,
			header: http.Header{
				"Retry-After":                  {"37"},
				"Anthropic-Ratelimit-Requests": {"0"},
				"Anthropic-Ratelimit-Reset":    {"2026-09-23T16:00:00Z"},
				"X-Should-Retry":               {"true"},
				"Content-Type":                 {"application/json"},
			},
			body:      `{"error":{"type":"rate_limit_error","message":"You've hit your weekly limit"},"request_id":"req_1","type":"error"}`,
			wantNames: []string{"Anthropic-Ratelimit-Requests", "Anthropic-Ratelimit-Reset", "Content-Type", "Retry-After", "X-Should-Retry"},
			wantHeaders: map[string]string{
				"Retry-After":                  "37",
				"Anthropic-Ratelimit-Requests": "0",
				"Anthropic-Ratelimit-Reset":    "2026-09-23T16:00:00Z",
				"X-Should-Retry":               "true",
			},
			wantErrType:   "rate_limit_error",
			wantResetAt:   "2026-09-23T16:00:00Z",
			mustNotAppear: []string{"You've hit your weekly limit"},
		},
		{
			name:   "429 with reset only inside error.message",
			status: 429,
			header: http.Header{
				"Content-Type": {"application/json"},
			},
			body:          `{"error":{"type":"rate_limit_error","message":"resets at 2026-09-23T16:00:00Z for your account"}}`,
			wantNames:     []string{"Content-Type"},
			wantHeaders:   nil,
			wantErrType:   "rate_limit_error",
			wantResetAt:   "2026-09-23T16:00:00Z",
			mustNotAppear: []string{"resets at", "for your account"},
		},
		{
			name:          "429 with epoch reset inside error.message",
			status:        429,
			header:        http.Header{"Content-Type": {"application/json"}},
			body:          `{"error":{"type":"rate_limit_error","message":"try again after 1790000000"}}`,
			wantNames:     []string{"Content-Type"},
			wantHeaders:   nil,
			wantErrType:   "rate_limit_error",
			wantResetAt:   "1790000000",
			mustNotAppear: []string{"try again after"},
		},
		{
			name:   "200 must capture nothing",
			status: 200,
			header: http.Header{
				"Retry-After":  {"37"},
				"Content-Type": {"application/json"},
			},
			body:        `{"error":{"type":"rate_limit_error","message":"2026-09-23T16:00:00Z"}}`,
			wantNames:   nil,
			wantHeaders: nil,
			wantErrType: "",
			wantResetAt: "",
		},
		{
			name:   "credential headers never appear, not even as names",
			status: 500,
			header: http.Header{
				"Authorization": {"Bearer sk-ant-oat01-SECRET"},
				"Set-Cookie":    {"session=abc123"},
				"Content-Type":  {"application/json"},
			},
			body:          `{"error":{"type":"internal_server_error","message":"no timestamp here"}}`,
			wantNames:     []string{"Content-Type"},
			wantHeaders:   nil,
			wantErrType:   "internal_server_error",
			wantResetAt:   "",
			mustNotAppear: []string{"Authorization", "Set-Cookie", "sk-ant-oat01-SECRET", "session=abc123", "Cookie"},
		},
		{
			name:          "prose with no timestamp records no timestamp and no prose",
			status:        503,
			header:        http.Header{"Content-Type": {"application/json"}},
			body:          `{"error":{"type":"overloaded_error","message":"the server is temporarily overloaded, please retry"}}`,
			wantNames:     []string{"Content-Type"},
			wantHeaders:   nil,
			wantErrType:   "overloaded_error",
			wantResetAt:   "",
			mustNotAppear: []string{"temporarily overloaded", "please retry"},
		},
		{
			// A server-chosen error.type is not truncated when it fails the
			// enum-like check, it is rejected outright: a truncated 64-byte
			// slice of 100,000 'A's is still prose, so nothing short of
			// rejection satisfies the "never persist prose" rule.
			name:          "error.type that is 100,000 bytes of prose is rejected outright, not truncated",
			status:        400,
			header:        http.Header{"Content-Type": {"application/json"}},
			body:          `{"error":{"type":"` + strings.Repeat("A", 100000) + ` LEAKED PROSE","message":"no timestamp here"}}`,
			wantNames:     []string{"Content-Type"},
			wantHeaders:   nil,
			wantErrType:   "",
			wantResetAt:   "",
			mustNotAppear: []string{"LEAKED PROSE", "AAAA"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			names, limitHeaders, errType, resetAt := tracelog.FingerprintLimit(c.status, c.header, []byte(c.body))

			gotNames := append([]string(nil), names...)
			sort.Strings(gotNames)
			wantNames := append([]string(nil), c.wantNames...)
			sort.Strings(wantNames)
			if !reflect.DeepEqual(gotNames, wantNames) {
				t.Fatalf("names = %v, want %v", gotNames, wantNames)
			}
			if !reflect.DeepEqual(limitHeaders, c.wantHeaders) {
				t.Fatalf("limitHeaders = %v, want %v", limitHeaders, c.wantHeaders)
			}
			if errType != c.wantErrType {
				t.Fatalf("errType = %q, want %q", errType, c.wantErrType)
			}
			if resetAt != c.wantResetAt {
				t.Fatalf("resetAt = %q, want %q", resetAt, c.wantResetAt)
			}

			// Never allow the body/message prose, or a credential value or
			// name, to leak into any recorded field.
			all := errType + " " + resetAt
			for _, n := range names {
				all += " " + n
			}
			for _, v := range limitHeaders {
				all += " " + v
			}
			for _, bad := range c.mustNotAppear {
				if strings.Contains(all, bad) {
					t.Fatalf("recorded fields %q must not contain %q", all, bad)
				}
			}
		})
	}
}

// TestFingerprintLimitCredentialExclusionIsLoadBearing documents the
// mutation this test suite must catch: if the credential-header exclusion
// in FingerprintLimit's name loop were ever removed, this test fails,
// because Authorization would then appear in names. (Verified by hand:
// temporarily removing the `if credentialHeaderNames[...] { continue }`
// line in the names loop makes this test fail with "Authorization" present.)
func TestFingerprintLimitCredentialExclusionIsLoadBearing(t *testing.T) {
	header := http.Header{
		"Authorization":       {"Bearer sk-ant-oat01-SECRET"},
		"Proxy-Authorization": {"Basic xyz"},
		"Cookie":              {"a=b"},
		"X-Api-Key":           {"sk-ant-api-SECRET"},
	}
	names, limitHeaders, _, _ := tracelog.FingerprintLimit(429, header, nil)
	if len(names) != 0 {
		t.Fatalf("names = %v, want none (all inputs are credential headers)", names)
	}
	if len(limitHeaders) != 0 {
		t.Fatalf("limitHeaders = %v, want none", limitHeaders)
	}
}

// TestFingerprintLimitHeaderAllowlistIsAnchored is a mutation-style test for
// the header-value allowlist: it must match by an anchored rule (exact name
// or prefix), never by bare substring containment. A "contains reset" or
// "contains ratelimit" arm would record these headers' values too —
// including two that carry a credential-shaped secret under an
// innocuous-looking reset/ratelimit name.
func TestFingerprintLimitHeaderAllowlistIsAnchored(t *testing.T) {
	header := http.Header{
		"X-Password-Reset-Token":               {"SECRET-RESET-TOKEN"},
		"X-Session-Reset-Secret":               {"hunter2"},
		"X-Custom-Ratelimit-Secret":            {"also-secret"},
		"Anthropic-Ratelimit-Unified-7d-Reset": {"2026-09-30T00:00:00Z"},
		"Retry-After":                          {"12"},
	}
	_, limitHeaders, _, _ := tracelog.FingerprintLimit(429, header, nil)

	for _, n := range []string{"X-Password-Reset-Token", "X-Session-Reset-Secret", "X-Custom-Ratelimit-Secret"} {
		if v, ok := limitHeaders[n]; ok {
			t.Fatalf("limitHeaders[%q] = %q, want absent (not allowlisted, matches only by substring)", n, v)
		}
	}
	want := map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": "2026-09-30T00:00:00Z",
		"Retry-After":                          "12",
	}
	for n, wantV := range want {
		if got := limitHeaders[n]; got != wantV {
			t.Fatalf("limitHeaders[%q] = %q, want %q", n, got, wantV)
		}
	}

	var all string
	for _, v := range limitHeaders {
		all += v + " "
	}
	for _, bad := range []string{"SECRET-RESET-TOKEN", "hunter2", "also-secret"} {
		if strings.Contains(all, bad) {
			t.Fatalf("limitHeaders leaked %q: %v", bad, limitHeaders)
		}
	}
}

// TestFingerprintLimitHeaderEntryCap documents that limitHeaders, like
// names, cannot grow without bound: an adversarial response padded with
// hundreds of allowlisted-but-distinct header names must still yield at
// most maxLimitHeaderEntries (20) recorded values.
func TestFingerprintLimitHeaderEntryCap(t *testing.T) {
	header := http.Header{}
	for i := 0; i < 500; i++ {
		header.Set(fmt.Sprintf("Anthropic-Ratelimit-Field-%03d-Reset", i), "v")
	}
	_, limitHeaders, _, _ := tracelog.FingerprintLimit(429, header, nil)
	if len(limitHeaders) != 20 {
		t.Fatalf("len(limitHeaders) = %d, want 20", len(limitHeaders))
	}
}

// TestFingerprintLimitTimestampFractionIsBounded documents that the ISO
// timestamp regex's fractional-seconds group cannot consume an unbounded
// run of digits: a crafted message with thousands of fractional digits must
// still yield a small, bounded resetAt, not a multi-kilobyte one.
func TestFingerprintLimitTimestampFractionIsBounded(t *testing.T) {
	msg := "resets at 2026-09-23T16:00:00." + strings.Repeat("9", 5000) + "Z for your account"
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"type": "rate_limit_error", "message": msg},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, resetAt := tracelog.FingerprintLimit(429, http.Header{"Content-Type": {"application/json"}}, body)
	if len(resetAt) > 40 {
		t.Fatalf("resetAt length = %d, want bounded (<=40); got %q", len(resetAt), resetAt)
	}
	if !strings.HasPrefix(resetAt, "2026-09-23T16:00:00.999999999") {
		t.Fatalf("resetAt = %q, want prefix %q", resetAt, "2026-09-23T16:00:00.999999999")
	}
}

// TestFingerprintLimitHeaderValueTruncationIsRuneSafe documents that
// truncate never splits a multi-byte UTF-8 rune. "世" is 3 bytes in UTF-8;
// 100 repeats is 300 bytes, and the 200-byte cap (not a multiple of 3)
// lands mid-rune unless truncate backs up to a rune boundary.
func TestFingerprintLimitHeaderValueTruncationIsRuneSafe(t *testing.T) {
	value := strings.Repeat("世", 100)
	header := http.Header{"Retry-After": {value}}
	_, limitHeaders, _, _ := tracelog.FingerprintLimit(429, header, nil)
	got := limitHeaders["Retry-After"]
	if !utf8.ValidString(got) {
		t.Fatalf("truncated value is not valid UTF-8: %q", got)
	}
	if len(got) > 200 {
		t.Fatalf("truncated value length = %d, want <= 200", len(got))
	}
}
