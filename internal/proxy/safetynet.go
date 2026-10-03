package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/HaiNNT/c-hottag/internal/router"
)

// maxReplayBody is the largest request body the safety net buffers so it can
// be sent again (its own refresh-and-retry and original-login fallback).
// Bigger requests are sent once, as they are — except a request armed for
// the wall retry, which uses the larger maxWallRetryBody cap instead (see
// safetyNet.maxBody).
const maxReplayBody = 4 << 20

// maxWallRetryBody is the cap for a request armed for the wall retry (M4
// spec §4a, S14): a Claude Code turn carries its whole context, so the
// ordinary 4 MiB replay cap would leave long sessions unretried.
const maxWallRetryBody = 32 << 20

// safetyNet wraps the upstream transport for one swapped request. Claude Code
// treats 401/403/404 as terminal for its SSE transport, so it must never see
// one that chottag's swap caused: refresh and retry once, then send the
// request exactly as the client wrote it and return that response.
type safetyNet struct {
	base           http.RoundTripper
	chooser        Chooser
	account        string
	original       string // the client's own Authorization header
	originalAPIKey string // the client's own X-Api-Key header, if any
	drift          *atomic.Uint64
	onDrift        func() // marks the trace record
	// serving is true for a plain serving route (class serving, no object):
	// a request there that is refused twice is the account's login being
	// refused, not a route-table mismatch, so it is not counted as route
	// drift (R158). onRefused marks the trace record with the first refused
	// status; onServingRefusal reports the account and that status.
	serving          bool
	onRefused        func(status int)
	onServingRefusal func(account string, status int, resent bool)
	// ownerAnswer is F241/R96: true for a request to MCPProxyHost whose
	// account was chosen from the owner map. A connector id's own owner
	// account answering 401/403/404 is that owner's genuine protocol
	// answer (a stale MCP session id, an auth handshake), not a routing
	// error, so RoundTrip returns it to the client exactly as received —
	// no refresh-and-retry, no original-login resend, no drift count.
	ownerAnswer bool
	// onOwnerAnswer marks the trace record when ownerAnswer passed a
	// refusal straight through, mirroring onUnreplayableRefusal below.
	onOwnerAnswer func()
	// onUnreplayableRefusal marks the trace record when a body too large to
	// buffer for replay was ALSO refused: neither retry nor resend could
	// run, so Claude Code saw a chottag-caused refusal (see maxReplayBody,
	// maxWallRetryBody for a request armed for the wall retry, and
	// bufferBody). Called with req.ContentLength: the declared size, or
	// -1 when unknown (chunked).
	onUnreplayableRefusal func(contentLength int64)

	// noOriginal: more than one pool exists (M8), or the request is remote
	// class or owner-routed (R147), so the client's own login,
	// which may be another pool's or none, is never used: a refused request
	// returns the swapped account's refusal instead of the resend below.
	noOriginal bool

	// maxBody caps bufferBody: maxReplayBody, or maxWallRetryBody when the
	// wall retry is armed. 0 means maxReplayBody.
	maxBody int64
	// wallRetry is proxy.Config.WallRetry when this request is armed for
	// it (serving class, no object owner), else nil. decision and bodyID
	// are what Choose is asked again with, and onRetarget tells forward
	// which account the response now belongs to.
	wallRetry  func(ctx context.Context, account string, h http.Header) (bool, func(string, int))
	decision   router.Decision
	bodyID     string
	onRetarget func(account string)

	// wallDone and wallTo are set by retryAtWall when it actually resends
	// the request on a new account, and consumed exactly once by RoundTrip
	// once the whole chain — including the safety net's own
	// refresh-and-retry and original-login fallback on that resend — has
	// reached its final response. wallTo is cleared to "" if that chain
	// ends in the original-login fallback, since the response then did not
	// come from the resent account.
	wallDone func(to string, status int)
	wallTo   string
}

func refused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound
}

func (s *safetyNet) RoundTrip(req *http.Request) (*http.Response, error) {
	limit := s.maxBody
	if limit == 0 {
		limit = maxReplayBody
	}
	body, replayable := bufferBody(req, limit)
	resp, err := s.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests && s.wallRetry != nil && replayable {
		resp, err = s.retryAtWall(req, body, resp)
	}
	if err != nil || !refused(resp.StatusCode) {
		s.finishWall(false, resp, err)
		return resp, err
	}
	firstRefused := resp.StatusCode
	if s.ownerAnswer {
		// The connector's own owner account refused this call: its answer,
		// not a chottag routing error (F241/R96). Return it as is — no
		// retry, no resend, no drift.
		if s.onOwnerAnswer != nil {
			s.onOwnerAnswer()
		}
		s.finishWall(false, resp, err)
		return resp, err
	}
	if !replayable {
		// The body was too large to buffer for a possible retry/resend, and
		// the swapped attempt was refused: this is the one case neither the
		// refresh-and-retry nor the original-login resend below can run,
		// so the client sees a chottag-caused refusal. Marked distinctly
		// (never as Drift — no resend happened here) so we learn from the
		// trace log how often this actually fires in practice.
		if s.onUnreplayableRefusal != nil {
			s.onUnreplayableRefusal(req.ContentLength)
		}
		s.finishWall(false, resp, err)
		return resp, err
	}

	if tok, ok := s.chooser.Refresh(req.Context(), s.account); ok {
		retry := clone(req, body)
		retry.Header.Set("Authorization", "Bearer "+tok)
		drain(resp)
		resp, err = s.base.RoundTrip(retry)
		switch {
		case err != nil && req.Context().Err() != nil:
			// The client is gone (canceled or timed out): a resend would
			// be a pointless extra upstream call, so report the error.
			s.finishWall(false, resp, err)
			return resp, err
		case err == nil && !refused(resp.StatusCode):
			s.finishWall(false, resp, err)
			return resp, err
		}
		// Otherwise fall through to the original-login resend below: the
		// retry was refused again, or it failed at the transport level
		// (e.g. a reset connection) while the client is still waiting —
		// exactly the case the safety net exists for, so it must not be
		// treated as a synthesized failure.
	}

	if s.noOriginal {
		// The swapped account was refused twice and Home's login may not
		// be used: nothing is resent, but it is still a refused swap, which
		// is what the route-drift count (and `chottag doctor`) is about.
		s.countRefusal(firstRefused, false)
		s.finishWall(false, resp, err)
		return resp, err
	}

	// Still refused: the route table is wrong about this route, or the
	// account genuinely cannot see this object. Give Claude Code exactly
	// what it would have got without chottag.
	original := clone(req, body)
	original.Header.Set("Authorization", s.original)
	if s.originalAPIKey != "" {
		original.Header.Set("X-Api-Key", s.originalAPIKey)
	}
	drain(resp)
	s.countRefusal(firstRefused, true)
	resp, err = s.base.RoundTrip(original)
	// The response is the original-login fallback's, not the resent
	// account's: wall retry's done, if pending, must report "" (see
	// finishWall).
	s.finishWall(true, resp, err)
	return resp, err
}

// countRefusal records a swapped request that was refused twice. The trace
// record always learns it (drift, and the first refused status). Only an
// object or remote route, a route the table does not list, or a 404 counts as
// route drift; a 401 or 403 on a classified serving route is reported as the
// account's login being refused instead (R158). resent
// says the request was then sent again on the client's own login.
func (s *safetyNet) countRefusal(status int, resent bool) {
	if s.onDrift != nil {
		s.onDrift()
	}
	if s.onRefused != nil {
		s.onRefused(status)
	}
	if s.serving && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if s.onServingRefusal != nil {
			s.onServingRefusal(s.account, status, resent)
		}
		return
	}
	s.drift.Add(1)
}

// finishWall calls s.wallDone exactly once, if a wall-retry resend is
// pending one, with the account the final response belongs to (or "" when
// viaOriginalLogin, since the chain ended in the client's own credential,
// not the resent account's) and that response's status (0 on a transport
// error). A no-op when no resend is pending: the early decline paths in
// retryAtWall call done themselves, and an unarmed request never sets
// wallDone at all.
func (s *safetyNet) finishWall(viaOriginalLogin bool, resp *http.Response, err error) {
	if s.wallDone == nil {
		return
	}
	done := s.wallDone
	to := s.wallTo
	s.wallDone, s.wallTo = nil, ""
	if viaOriginalLogin {
		to = ""
	}
	status := 0
	if err == nil {
		status = resp.StatusCode
	}
	done(to, status)
}

// retryAtWall is the wall retry (M4 spec §4a): the daemon's hook decides
// and may switch serving; if Choose then names another account, the
// request is resent there exactly once. The 429's body is closed unread
// when it is replaced, and passed through untouched when it is not.
//
// When the resend happens, done is NOT called here: a refusal on it can
// still send the resend through the safety net's own refresh-and-retry and
// original-login fallback (RoundTrip's own logic, now running with
// s.account set to the resent account), so the final response — and thus
// what done should report — is not known until RoundTrip returns. done and
// the resent account are stashed on the safetyNet (wallDone, wallTo) and
// consumed exactly once by finishWall.
func (s *safetyNet) retryAtWall(req *http.Request, body []byte, first *http.Response) (*http.Response, error) {
	retry, done := s.callWallRetry(req, first)
	if !retry {
		return first, nil
	}
	acct, tok, _, ok := s.chooser.Choose(req.Context(), s.decision, s.bodyID)
	if !ok || strings.EqualFold(acct, s.account) {
		// No new target: the 429 stands as is, and this is already the
		// final outcome, so done fires right here.
		if done != nil {
			done("", 0)
		}
		return first, nil
	}
	first.Body.Close()
	resend := clone(req, body)
	resend.Header.Set("Authorization", "Bearer "+tok)
	resend.Header.Del("X-Api-Key")
	s.account = acct
	if s.onRetarget != nil {
		s.onRetarget(acct)
	}
	s.wallDone, s.wallTo = done, acct
	return s.base.RoundTrip(resend)
}

// callWallRetry calls the WallRetry hook and absorbs a panic as a decline
// (retry=false, done=nil), mirroring how notifyUsage absorbs a panicking
// OnUsage (forward.go): a panic here must never turn a response the client
// was about to receive into a dropped connection.
func (s *safetyNet) callWallRetry(req *http.Request, first *http.Response) (retry bool, done func(string, int)) {
	defer func() {
		if recover() != nil {
			retry, done = false, nil
		}
	}()
	return s.wallRetry(req.Context(), s.account, first.Header.Clone())
}

// bufferBody reads the request body, up to limit bytes, so it can be
// replayed. replayable is false for a body too large to hold or one that cannot be re-read — in
// which case req.Body is restored to yield the full original byte stream
// (the bytes already read, followed by the untouched remainder) so the
// first, non-retried, attempt upstream still sees exactly what the client
// sent. A partially-consumed body must never be replayed and must never be
// truncated either: mirrors readSmallBody in forward.go.
func bufferBody(req *http.Request, limit int64) ([]byte, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true
	}
	if req.ContentLength > limit {
		return nil, false
	}
	buf, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	rest := req.Body
	req.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), rest), rest}
	if err != nil || int64(len(buf)) > limit {
		return nil, false
	}
	req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

func clone(req *http.Request, body []byte) *http.Request {
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	return out
}

func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}
