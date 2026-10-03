package tracelog

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns 8 lowercase hex characters from crypto/rand. It exists only
// to pair a streamed response's "head" record with its final "req" record
// (spec §4.2, M1c6a): it is never derived from the request, a token or the
// time, so it carries no information of its own. crypto/rand.Read cannot
// fail on a supported platform (since Go 1.24 it crashes the program
// instead), so there is no error to handle.
func NewID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// HeadOf returns the header-time "head" record for r (spec §4.2, M1c6a).
//
// It is an allowlist, not r with a few fields cleared: a field added to
// Record later is left out of the head until someone decides it is known at
// header time and adds it here (TestHeadOfClassifiesEveryRecordField fails
// until they decide). Left out on purpose:
//   - Millis and Err: not known until the stream ends.
//   - ReqShape and RespShape: body-derived (`--shapes`), and the response
//     body is unread at header time.
//   - Upgrade, Unreplayable, UnreplayableBytes, OwnerRefused and Mark: not
//     in the spec's list; they stay on the final record only.
//
// The fingerprint fields are copied: FingerprintLimit derives them from
// headers (and a JSON error body, which a text/event-stream response never
// has), and they are empty unless `trace run --limit-fingerprint` is on.
func HeadOf(r Record) Record {
	return Record{
		T: r.T, Kind: "head", ID: r.ID, SID: r.SID,
		Form: r.Form, Method: r.Method, Host: r.Host, Path: r.Path,
		PathIDs: r.PathIDs, QueryKeys: r.QueryKeys,
		Class: r.Class, Auth: r.Auth,
		Swapped: r.Swapped, Account: r.Account, Drift: r.Drift, Refused: r.Refused,
		Status: r.Status, ReqType: r.ReqType, RespType: r.RespType,
		RespHeaderNames: r.RespHeaderNames, RespLimitHeaders: r.RespLimitHeaders,
		RespErrorType: r.RespErrorType, RespResetAt: r.RespResetAt,
	}
}
