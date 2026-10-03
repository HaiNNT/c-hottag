package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

const maxShapeBytes = 1 << 20

func (s *Server) forward(w http.ResponseWriter, r *http.Request, form string) {
	defer s.requestStarted()()
	start := time.Now()
	// traced is asked ONCE, here. Every shape decision and record below
	// uses it, so a switch flipped mid-request changes only the next
	// request (M2c spec §3).
	traced := s.tracing()
	host := router.HostOnly(r.URL.Host)
	d := router.Route(router.Request{
		Method: r.Method, Host: host, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		AbsoluteForm: form == "absolute",
	})
	path, ids := tracelog.TemplatePath(r.URL.Path)
	rec := tracelog.Record{
		SID: sidOf(r.Context()),
		T:   start.UTC(), Kind: "req", Form: form, Method: r.Method, Host: host, Path: path, PathIDs: ids,
		QueryKeys: tracelog.QueryKeys(r.URL.RawQuery), Class: string(d.Class),
		Auth:    tracelog.AuthKind(r.Header.Get("Authorization"), r.Header.Get("X-Api-Key") != ""),
		ReqType: r.Header.Get("Content-Type"), Upgrade: r.Header.Get("Upgrade"),
	}
	originalAuth := r.Header.Get("Authorization")
	originalAPIKey := r.Header.Get("X-Api-Key")
	if traced {
		s.noteClaudeVersion(r.Header.Get("User-Agent"))
	}

	// An artifact republish names its object only in the request body.
	var bodyIDs []string
	if s.cfg.Choose != nil && d.RequestField != "" && isJSON(rec.ReqType) {
		if body, ok := readSmallBody(r); ok {
			bodyIDs = owners.ExtractIDs(body, d.RequestField)
		}
	}
	bodyID := ""
	if len(bodyIDs) > 0 {
		bodyID = bodyIDs[0]
	}

	// Never swap onto a cleartext connection: an absolute-form http://
	// request classed Serving would otherwise send the swapped token in
	// the clear.
	account := ""
	// ownerAnswer is F241/R96: a connector id's own owner-mapped account
	// answering 401/403/404 is that owner's genuine protocol answer (a
	// stale MCP session id, an auth handshake), not a routing error — but
	// only on MCPProxyHost, where the owner map's only object is a
	// connector (mcpProxyRules). An owner-mapped object on api.anthropic.com
	// (e.g. an artifact) keeps the safety net's refresh and drift count (R147:
	// but never a resend on Home's login, see neverHome).
	ownerAnswer := false
	// neverHome (R147): a remote-class or owner-routed request is never
	// resent on the client's own login, whatever the pool count.
	neverHome := false
	refusal := ""
	if s.cfg.Choose != nil && d.Class != router.Untouched && r.URL.Scheme == "https" && rec.Auth == "oauth-access" {
		acct, tok, owner, ok, why := s.choose(r.Context(), d, bodyID)
		refusal = why
		if ok {
			r.Header.Set("Authorization", "Bearer "+tok)
			r.Header.Del("X-Api-Key")
			rec.Swapped, rec.Account, account = true, acct, acct
			ownerAnswer = owner && host == router.MCPProxyHost
			neverHome = owner || d.Class == router.Remote
		}
	}

	// Owner recording, shaping and limit-fingerprinting all need the
	// decoded JSON response; without a client Accept-Encoding the transport
	// negotiates gzip itself and hands us a decoded body. Left alone, a
	// real client's own Accept-Encoding: gzip passes straight through and
	// the response comes back compressed, so json.Unmarshal in
	// tracelog.FingerprintLimit fails silently and every fingerprint field
	// stays empty.
	recordOwners := account != "" && d.Records != ""
	if recordOwners || traced || s.cfg.LimitFingerprint {
		r.Header.Del("Accept-Encoding")
	}
	if traced {
		// Only buffer bodies we can actually shape, so a chunked non-JSON
		// upload (e.g. a large binary) is never read into memory here.
		if isJSON(rec.ReqType) {
			if body, ok := readSmallBody(r); ok {
				rec.ReqShape = tracelog.Shape(body)
			}
		}
	}

	var captured *capture
	// finalise logs rec exactly once, even if ReverseProxy panics with
	// http.ErrAbortHandler (it does this when writing the response back to
	// the client fails mid-body, e.g. the client hung up on an SSE stream).
	// Any other panic is also logged before being re-raised unchanged.
	defer func() {
		// captured is only ever set on the traced path now: a recording
		// route's response is fully consumed and recorded from inside
		// ModifyResponse (F14), which also sets rec.RespShape when traced,
		// so there is nothing left to do with it here.
		if captured != nil && traced {
			rec.RespShape = tracelog.Shape(captured.buf.Bytes())
		}
		rec.Millis = time.Since(start).Milliseconds()
		p := recover()
		if p != nil {
			if p == http.ErrAbortHandler && rec.Err == "" {
				rec.Err = "aborted mid-body"
			}
		}
		s.emit(rec, traced)
		if p != nil {
			panic(p)
		}
	}()

	if refusal != "" {
		// The pool boundary: nothing in the session's pool can serve this,
		// and the client's own login must not (M8). Answered here.
		rec.Status, rec.Err = http.StatusServiceUnavailable, "pool refused"
		writePoolRefusal(w, refusal)
		return
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = r.URL.Scheme
			pr.Out.URL.Host = r.URL.Host
			pr.Out.Host = upstreamHostHeader(r)
		},
		Transport: s.transportFor(account, originalAuth, originalAPIKey, d, bodyID, ownerAnswer, neverHome, &rec, func(to string) {
			// The wall retry resent the request on another account (M4
			// spec §4a): the response, its usage headers included, is that
			// account's. Same goroutine as ModifyResponse, so no lock.
			account, rec.Account = to, to
		}),
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			rec.Status = resp.StatusCode
			rec.RespType = resp.Header.Get("Content-Type")
			success := rec.Status >= 200 && rec.Status < 300
			respIsJSON := isJSON(rec.RespType)

			// Usage belongs to an account, so only a swapped response has
			// any — and a DRIFTED response was answered on the client's own
			// login, so its quota headers are not this account's at all.
			// Same hazard the owner-recording block below guards against
			// (F26): `account` says who chottag meant to be, only !Drift
			// says who actually answered.
			if s.cfg.OnUsage != nil && account != "" && !rec.Drift {
				s.notifyUsage(account, resp.StatusCode, resp.Header)
			}

			// A limit fingerprint is observation only (never affects what
			// reaches the client) and only ever meaningful for a refusal,
			// so read the body just for status >= 400 JSON; FingerprintLimit
			// itself is the one place that decides what of that is safe to
			// keep (see its doc for the allowlist and exclusions).
			if s.cfg.LimitFingerprint {
				var fpBody []byte
				if respIsJSON && rec.Status >= 400 {
					fpBody = bufferResponseBody(resp)
				}
				rec.RespHeaderNames, rec.RespLimitHeaders, rec.RespErrorType, rec.RespResetAt =
					tracelog.FingerprintLimit(rec.Status, resp.Header, fpBody)
			}

			// Ids already known from the client's own request body can be
			// recorded now, before any response byte reaches the client: the
			// client chose them, so recording them doesn't need the response
			// body at all.
			//
			// !rec.Drift matters as much as success here: the safety net may
			// have already abandoned the swapped account and resent the
			// ORIGINAL request on the client's own login, returning a
			// genuine 2xx from an account that never actually made this
			// call. ModifyResponse runs on whatever RoundTrip returns, so
			// that response is indistinguishable from a real one by status
			// code alone — rec.Drift is set (safetynet.go's onDrift) before
			// the final RoundTrip returns, so it is always current here.
			// Recording through a drift would attribute the object to an
			// account that refused it — and every later request for that
			// object would then be swapped there, refused, force-refreshed
			// and drifted again, corrupting owners.json persistently.
			if recordOwners && success && !rec.Drift && len(bodyIDs) > 0 {
				s.cfg.Choose.Record(d.Records, bodyIDs, account)
			}

			switch {
			case recordOwners && success && !rec.Drift && respIsJSON:
				// Response-derived ids need the response body itself (e.g. a
				// session create only learns its id from the response). Read
				// it here, fully, and record before returning — i.e. before
				// ReverseProxy copies a single byte back to the client — so a
				// client that acts immediately on the response can never
				// outrace the owner map (F14). Recording routes are small
				// JSON API calls (session create, artifact publish,
				// environment bridge, connector list); router.go never sets
				// Records on a route that also streams (SSE lives at
				// /v1/messages and the worker/events routes, all Untouched or
				// Serving), so buffering here cannot stall a live stream.
				buf := bufferResponseBody(resp)
				if traced {
					rec.RespShape = tracelog.Shape(buf)
				}
				// No len(ids) > 0 guard here, unlike the request-body site
				// above: a response-derived extraction yielding nothing on a
				// recording route is a shape mismatch worth counting
				// (owners.Map.ZeroIDExtractions, F19/F27) — the create
				// response's field moved or its JSON shape changed, and
				// without this call that failure looks identical to a quiet
				// session. A request-body extraction has no analogous
				// "should always be there" expectation, so it keeps its
				// guard and is never counted.
				s.cfg.Choose.Record(d.Records, owners.ExtractIDs(buf, d.RecordField), account)
			case traced && respIsJSON:
				captured = &capture{rc: resp.Body}
				resp.Body = captured
			}

			// A stream's final record is only written when the stream ends
			// (the deferred finaliser above), so a stream that runs for
			// minutes, or dies with the daemon, would leave no line while it
			// is live. Write a header-time "head" record now (spec §4.2,
			// M1c6a). Last in ModifyResponse, so every header-derived field
			// (status, drift, fingerprint) is already set.
			//
			// Ordering needs no lock: ReverseProxy calls ModifyResponse
			// synchronously on this goroutine, before it copies the body,
			// and the finaliser runs on the same goroutine after ServeHTTP
			// returns, so this head is always written before its req.
			// tracelog.Writer serialises whole lines, so another request's
			// records may fall between the two but never inside either.
			// A failure before headers (dial error, timeout) never reaches
			// ModifyResponse: ID stays empty, and the req is written alone.
			// The cost is a mutex and one write(2) (no fsync), plus a
			// rotation about once per 8 MB, before a stream's headers reach
			// the client: small, and paid by streams only.
			//
			// s.logging(traced) short-circuits first: with no log to write
			// to, every intercepted response would otherwise still pay for
			// a mime parse and a crypto/rand read whose result is never
			// written.
			if s.logging(traced) && isEventStream(rec.RespType) {
				rec.ID = tracelog.NewID()
				s.emit(tracelog.HeadOf(rec), traced)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				rec.Status = 0
				rec.Err = "client canceled"
				return
			}
			rec.Status = http.StatusBadGateway
			rec.Err = err.Error()
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// notifyUsage hands the headers to the usage hook and absorbs anything the
// callback does wrong. Observation must never turn a response the client was
// about to receive into a dropped connection: a panic here would unwind into
// the per-connection MITM server, which closes the whole TLS connection —
// killing the in-flight request and every keep-alive request queued behind it.
//
// h is cloned because the callback is otherwise handed the live response
// header map and could rewrite what the client receives.
//
// Deliberately synchronous: ordering and the caller-serialises contract of
// status.File depend on it. Slowness is the consumer's problem to solve (an
// in-memory update under its own lock, flushed to disk on a timer), not
// something a goroutine here could fix without breaking that contract.
//
// A panic is reported through OnUsageError, not swallowed silently: without
// this, a broken OnUsage consumer would silently and permanently stop
// recording usage, with nothing in the daemon log ever saying so.
// OnUsageError falling back to OnLogError when unset keeps that report
// reaching an older caller that only ever wired OnLogError — see Config's
// doc comment — but a caller with both wired must never have this land as a
// trace-log failure, which is a different subsystem entirely.
func (s *Server) notifyUsage(account string, status int, h http.Header) {
	defer func() {
		if recover() == nil {
			return
		}
		report := s.cfg.OnUsageError
		if report == nil {
			report = s.cfg.OnLogError
		}
		if report != nil {
			// Fixed string, never the panic value: the callback was handed
			// response headers, so a value derived from them must not reach
			// a log. internal/cli/trace.go interpolates err.Error() verbatim
			// into stderr.
			report(errors.New("OnUsage callback panicked"))
		}
	}()
	s.cfg.OnUsage(account, status, h.Clone())
}

func isJSON(ct string) bool { return strings.Contains(ct, "json") }

// isEventStream reports whether ct is text/event-stream, ignoring case and
// parameters (e.g. "; charset=utf-8").
//
// The prefix check runs first, and cheaply rules out every non-stream
// content type (json, empty, etc.) without paying for mime.ParseMediaType,
// which this hot path (every intercepted response) would otherwise call
// unconditionally. mime.ParseMediaType is still the authority on the
// answer: it lower-cases the media type itself, so the case-insensitive
// prefix check below cannot let a mismatched case slip past it.
func isEventStream(ct string) bool {
	if len(ct) < len("text/event-stream") || !strings.EqualFold(ct[:len("text/event-stream")], "text/event-stream") {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "text/event-stream"
}

// upstreamHostHeader is the Host header sent upstream: the client's original
// Host (with its default :443 stripped), never the dial target's hostport.
// The MITM path sets r.URL.Host to the CONNECT hostport (e.g.
// "api.anthropic.com:443"), which must still be dialed via r.URL.Host, but
// must never leak its port into the Host header the upstream sees.
func upstreamHostHeader(r *http.Request) string {
	h := r.Host
	if h == "" {
		h = r.URL.Host
	}
	if host, port, err := net.SplitHostPort(h); err == nil && port == "443" {
		return host
	}
	return h
}

// readSmallBody buffers a body of at most maxShapeBytes and restores r.Body so
// the request is forwarded unchanged. ok=false if the body is larger or absent.
func readSmallBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength > maxShapeBytes {
		return nil, false
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxShapeBytes+1))
	rest := r.Body
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), rest), rest}
	if err != nil || len(buf) > maxShapeBytes {
		return nil, false
	}
	return buf, true
}

// bufferResponseBody reads up to maxShapeBytes of resp.Body for inspection,
// then restores resp.Body to deliver the identical full stream downstream —
// mirrors readSmallBody: a route that records owners must never truncate,
// delay past its cap, or otherwise change what the client actually receives
// merely because its body was inspected.
func bufferResponseBody(resp *http.Response) []byte {
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, maxShapeBytes))
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buf), rest), rest}
	return buf
}

// capture tees up to maxShapeBytes of a response body for shaping.
type capture struct {
	rc  io.ReadCloser
	buf bytes.Buffer
}

func (c *capture) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if c.buf.Len() < maxShapeBytes {
		c.buf.Write(p[:n])
	}
	return n, err
}

func (c *capture) Close() error { return c.rc.Close() }

// choose is Chooser.Choose, through ChooseGuarded when the chooser enforces
// the pool boundary.
func (s *Server) choose(ctx context.Context, d router.Decision, bodyID string) (account, token string, owner, ok bool, refusal string) {
	if g, isG := s.cfg.Choose.(PoolGuard); isG {
		return g.ChooseGuarded(ctx, d, bodyID)
	}
	account, token, owner, ok = s.cfg.Choose.Choose(ctx, d, bodyID)
	return account, token, owner, ok, ""
}

// writePoolRefusal answers 503 with an Anthropic-shaped error: Claude Code
// retries a 5xx with backoff, so a racing `pool leave` recovers on its own.
func writePoolRefusal(w http.ResponseWriter, msg string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": msg},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(body)
}

// transportFor wraps the shared transport in the safety net when the request
// was swapped; an unswapped request uses the transport directly. A swapped
// serving-class request with no object owner is also armed for the wall
// retry when Config.WallRetry is set (M4 spec §4a: never remote, never an
// owner-routed request).
func (s *Server) transportFor(account, originalAuth, originalAPIKey string, d router.Decision, bodyID string, ownerAnswer, neverHome bool, rec *tracelog.Record, onRetarget func(string)) http.RoundTripper {
	if account == "" || s.cfg.Choose == nil {
		return s.transport
	}
	sn := &safetyNet{
		base: s.transport, chooser: s.cfg.Choose, account: account,
		original: originalAuth, originalAPIKey: originalAPIKey, drift: &s.drift,
		ownerAnswer: ownerAnswer,
		onDrift:     func() { rec.Drift = true },
		serving:     d.Class == router.Serving && d.Object == "" && !d.Fallback,
		onRefused:   func(status int) { rec.Refused = status },
		onServingRefusal: func(acct string, status int, resent bool) {
			if s.cfg.OnServingRefusal != nil {
				s.cfg.OnServingRefusal(acct, status, resent, rec.Method, rec.Path)
			}
		},
		onOwnerAnswer: func() { rec.OwnerRefused = true },
		onUnreplayableRefusal: func(n int64) {
			rec.Unreplayable = true
			rec.UnreplayableBytes = n
		},
	}
	if g, ok := s.cfg.Choose.(PoolGuard); (ok && g.Guarded()) || neverHome {
		sn.noOriginal = true
	}
	if s.cfg.WallRetry != nil && d.Class == router.Serving && d.Object == "" {
		sn.wallRetry, sn.maxBody = s.cfg.WallRetry, maxWallRetryBody
		sn.decision, sn.bodyID, sn.onRetarget = d, bodyID, onRetarget
	}
	return sn
}
