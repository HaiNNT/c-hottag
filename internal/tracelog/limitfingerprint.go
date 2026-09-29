package tracelog

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxLimitHeaderNames caps how many response header names a limit
// fingerprint records, so a pathological or adversarial response can never
// grow a trace record without bound.
const maxLimitHeaderNames = 40

// maxLimitHeaderValueBytes caps the length of a single allowlisted header
// value recorded in a limit fingerprint.
const maxLimitHeaderValueBytes = 200

// maxLimitHeaderEntries caps how many allowlisted header VALUES a limit
// fingerprint records: names is capped separately (maxLimitHeaderNames), but
// an adversarial response can pad itself with hundreds of reset-ish header
// names, each within the allowlist, and inflate limitHeaders without bound
// unless it has its own cap.
const maxLimitHeaderEntries = 20

// credentialHeaderNames must never appear in a limit fingerprint, not even
// as a bare name in the header-names list: chottag's absolute rule is that
// no token or cookie is ever logged, and a credential header's mere
// presence (let alone its value) is exactly the kind of thing that rule
// exists to keep out of a trace file.
var credentialHeaderNames = map[string]bool{
	"authorization":       true,
	"set-cookie":          true,
	"cookie":              true,
	"x-api-key":           true,
	"proxy-authorization": true,
}

// isLimitHeaderName reports whether name's VALUE is safe to record: only
// headers known to carry rate-limit counts or reset timestamps, never a
// header that could carry a credential or arbitrary server-chosen text.
//
// Every rule is anchored (an exact match or a prefix), never a bare
// substring match: "contains ratelimit" or "contains reset" would also
// match headers like X-Password-Reset-Token or X-Custom-Ratelimit-Secret,
// recording a credential-bearing value under an innocuous-looking name.
// Every reset field chottag actually needs (…-5h-Reset, …-7d-Reset,
// …-Unified-Reset) already sits under the anthropic-ratelimit prefix, so a
// bare "contains reset" arm buys nothing and costs a credential leak.
func isLimitHeaderName(name string) bool {
	l := strings.ToLower(name)
	switch {
	case l == "retry-after":
		return true
	case strings.HasPrefix(l, "anthropic-ratelimit"):
		return true
	case strings.HasPrefix(l, "x-should-retry"):
		return true
	case strings.HasPrefix(l, "x-ratelimit"):
		return true
	case strings.HasPrefix(l, "ratelimit-"):
		return true
	}
	return false
}

var (
	// isoTimestampRe matches an ISO-8601 timestamp (date, time, optional
	// fractional seconds and offset). Deliberately strict: only a substring
	// that actually looks like a timestamp is ever extracted, never the
	// prose around it.
	isoTimestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:?\d{2})?`)
	// epochTimestampRe matches a bare 10-digit epoch-seconds integer (spans
	// 2001-09-09 to 2286-11-20). Word-bounded so it can never match part of
	// a longer number or an id, and 10 digits specifically so it can never
	// mistake a Retry-After delta-seconds count (always far smaller) for an
	// absolute epoch timestamp.
	epochTimestampRe = regexp.MustCompile(`\b\d{10}\b`)
	// errorTypeRe matches an enum-like token: Anthropic's error.type values
	// (e.g. "rate_limit_error", "overloaded_error") are always lowercase
	// snake_case, never prose. A value that doesn't match is rejected
	// outright, not truncated — a truncated 64-byte slice of prose is still
	// prose, so only a conforming token is ever recorded.
	errorTypeRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
)

// extractTimestamp returns the first ISO-8601 timestamp or bare
// epoch-seconds integer found in s, or "" if none. It returns ONLY the
// matched substring — callers must never fall back to passing s itself
// through when no match is found, since s is exactly the prose (e.g.
// error.message) that must never be stored.
func extractTimestamp(s string) string {
	if m := isoTimestampRe.FindString(s); m != "" {
		return m
	}
	return epochTimestampRe.FindString(s)
}

// errorEnvelope is Anthropic's error response shape:
// {"error":{"type":...,"message":...}, ...}. Message is decoded only to be
// searched in memory for a timestamp (extractTimestamp) — it is never
// itself returned by FingerprintLimit or stored anywhere.
type errorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// FingerprintLimit extracts a redacted usage-limit classifier fingerprint
// from a response (gate G6, spec §6.2): chottag must eventually tell a
// usage-limit refusal apart from a burst throttle or a server overload, and
// this is the one place that decides what evidence is safe to keep towards
// that classifier. status < 400 (i.e. not a refusal) always returns
// everything zero — a fingerprint is only ever meaningful for a refusal.
//
// Returns:
//   - names: every response header name present, minus the credential
//     exclusion list (Authorization, Set-Cookie, Cookie, X-Api-Key,
//     Proxy-Authorization — never recorded, not even as a bare name),
//     capped at maxLimitHeaderNames.
//   - limitHeaders: values for an allowlist of rate-limit/reset headers only
//     (Retry-After, and any header whose lowercased name starts with
//     anthropic-ratelimit, x-should-retry, x-ratelimit or ratelimit-), each
//     truncated to maxLimitHeaderValueBytes, capped at maxLimitHeaderEntries
//     entries.
//   - errType: the JSON body's error.type, when body is Anthropic's error
//     envelope AND the value matches errorTypeRe (an enum-like
//     `[a-z0-9_]{1,64}` token). A non-conforming value is rejected outright
//     (empty string), never truncated. error.message is NEVER returned or
//     stored — see errorEnvelope.
//   - resetAt: a reset timestamp, matched strictly (see extractTimestamp)
//     out of an allowlisted header value first, else out of error.message;
//     only the matched substring, never the surrounding prose.
func FingerprintLimit(status int, header http.Header, body []byte) (names []string, limitHeaders map[string]string, errType, resetAt string) {
	if status < 400 {
		return nil, nil, "", ""
	}

	for name := range header {
		if credentialHeaderNames[strings.ToLower(name)] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxLimitHeaderNames {
		names = names[:maxLimitHeaderNames]
	}

	var limitNames []string
	for name := range header {
		l := strings.ToLower(name)
		if credentialHeaderNames[l] || !isLimitHeaderName(name) {
			continue
		}
		limitNames = append(limitNames, name)
	}
	sort.Strings(limitNames)
	if len(limitNames) > maxLimitHeaderEntries {
		limitNames = limitNames[:maxLimitHeaderEntries]
	}
	for _, name := range limitNames {
		if limitHeaders == nil {
			limitHeaders = map[string]string{}
		}
		limitHeaders[name] = truncate(strings.Join(header[name], ", "), maxLimitHeaderValueBytes)
	}

	var env errorEnvelope
	if len(body) > 0 && json.Unmarshal(body, &env) == nil && errorTypeRe.MatchString(env.Error.Type) {
		errType = env.Error.Type
	}

	resetAt = resetFromHeaders(limitHeaders)
	if resetAt == "" {
		resetAt = extractTimestamp(env.Error.Message)
	}
	return names, limitHeaders, errType, resetAt
}

// resetFromHeaders looks for a timestamp inside any allowlisted header
// value, in sorted key order so the result is deterministic when more than
// one header happens to carry one.
func resetFromHeaders(limitHeaders map[string]string) string {
	keys := make([]string, 0, len(limitHeaders))
	for k := range limitHeaders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if ts := extractTimestamp(limitHeaders[k]); ts != "" {
			return ts
		}
	}
	return ""
}

// truncate cuts s to at most max bytes, never splitting a multi-byte UTF-8
// rune: it backs the cut point up to the nearest rune boundary at or before
// max, so the result is always valid UTF-8.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
