// Package tracelog writes and reads chottag's redacted JSONL trace records.
// A record never contains a token or a body — only kinds, templates and shapes.
package tracelog

import "time"

type Record struct {
	T         time.Time `json:"t"`
	SID       string    `json:"sid,omitempty"`  // the first 8 characters of a session id, never more (M6)
	Kind      string    `json:"kind"`           // "req" | "head" | "tunnel" | "mark"
	ID        string    `json:"id,omitempty"`   // pairs a stream's "head" and "req" records (NewID); empty on every other record
	Form      string    `json:"form,omitempty"` // "mitm" | "absolute" | "blind"
	Method    string    `json:"method,omitempty"`
	Host      string    `json:"host,omitempty"`
	Path      string    `json:"path,omitempty"` // templated
	PathIDs   []string  `json:"pathIds,omitempty"`
	QueryKeys []string  `json:"queryKeys,omitempty"`
	Class     string    `json:"class,omitempty"`
	Auth      string    `json:"auth,omitempty"`
	Swapped   bool      `json:"swapped,omitempty"`
	Account   string    `json:"account,omitempty"` // account name a swapped request went out as
	Drift     bool      `json:"drift,omitempty"`   // resent on the client's own login after a refused swap
	// Refused is the status of the first refusal (401, 403 or 404) of a swap
	// that stayed refused after the refresh-and-retry, set beside Drift.
	Refused int `json:"refused,omitempty"`
	// Unreplayable marks the one known gap in the safety net (spec §4.5,
	// deferred pending real-world frequency data): a swapped request whose
	// body was too large to buffer for a possible retry/resend, AND whose
	// swapped attempt was refused, so Claude Code saw a chottag-caused
	// refusal with no retry and no resend. Never set together with Drift —
	// a drift means a resend DID happen. UnreplayableBytes carries the
	// request's declared Content-Length (never the body itself), or -1 when
	// the length was unknown (chunked) and only found to exceed the replay
	// cap while streaming.
	Unreplayable      bool  `json:"unreplayable,omitempty"`
	UnreplayableBytes int64 `json:"unreplayableBytes,omitempty"`
	// OwnerRefused marks a request to MCPProxyHost whose owner-mapped
	// account's 401/403/404 was passed straight through, uncounted as
	// drift: that connector id's own owner answered, genuinely (F241/R96),
	// not a chottag routing error. Never set together with Drift — a
	// drift means a resend on the client's own login DID happen, which
	// this path never does.
	OwnerRefused bool   `json:"ownerRefused,omitempty"`
	Status       int    `json:"status,omitempty"`
	Millis       int64  `json:"ms,omitempty"`
	ReqType      string `json:"reqType,omitempty"`
	RespType     string `json:"respType,omitempty"`
	Upgrade      string `json:"upgrade,omitempty"`
	ReqShape     any    `json:"reqShape,omitempty"`
	RespShape    any    `json:"respShape,omitempty"`
	// RespHeaderNames, RespLimitHeaders, RespErrorType and RespResetAt hold
	// `trace run --limit-fingerprint`'s redacted usage-limit fingerprint
	// (gate G6, spec §6.2): only they, never error.message or any other
	// body text, feed the future usage-limit-vs-throttle classifier. Set
	// only for a status >= 400 response, and only by tracelog.FingerprintLimit
	// (internal/tracelog/limitfingerprint.go), which is the one place that
	// decides what is safe to keep. RespHeaderNames is metadata (header
	// names only, credential headers excluded even as bare names).
	// RespLimitHeaders holds values for an allowlist of rate-limit/reset
	// headers only. RespErrorType is the JSON body's error.type token.
	// RespResetAt is, at most, a matched ISO-8601/epoch-seconds substring —
	// never the surrounding prose it was found in.
	RespHeaderNames  []string          `json:"respHeaderNames,omitempty"`
	RespLimitHeaders map[string]string `json:"respLimitHeaders,omitempty"`
	RespErrorType    string            `json:"respErrorType,omitempty"`
	RespResetAt      string            `json:"respResetAt,omitempty"`
	Mark             string            `json:"mark,omitempty"`
	Err              string            `json:"err,omitempty"`
}
