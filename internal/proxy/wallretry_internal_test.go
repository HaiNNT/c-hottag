package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/router"
)

// These drive safetyNet.RoundTrip directly with a scripted transport, as
// safetynet_internal_test.go does: the wall retry (M4 spec §4a) depends
// only on the safety net's own logic.

// wallChooser answers Choose with a fixed account and counts the calls;
// Refresh answers from refreshTok.
type wallChooser struct {
	acct, tok  string
	ok         bool
	chooses    atomic.Int32
	refreshTok string
	refreshed  []string
}

func (c *wallChooser) Choose(context.Context, router.Decision, string) (string, string, bool, bool) {
	c.chooses.Add(1)
	return c.acct, c.tok, false, c.ok
}
func (c *wallChooser) Record(router.Kind, []string, string) {}
func (c *wallChooser) Refresh(_ context.Context, account string) (string, bool) {
	c.refreshed = append(c.refreshed, account)
	return c.refreshTok, c.refreshTok != ""
}

// countingBody records whether anything read it and whether it was closed.
type countingBody struct {
	reads  atomic.Int32
	closed atomic.Bool
}

func (b *countingBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (b *countingBody) Close() error             { b.closed.Store(true); return nil }

// labelledTransport returns one scripted response per call and records the
// Authorization header's token (never a real one: these are test labels).
type labelledTransport struct {
	resps []*http.Response
	errs  []error
	auths []string
}

func (tr *labelledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	i := len(tr.auths)
	tr.auths = append(tr.auths, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	var err error
	if i < len(tr.errs) {
		err = tr.errs[i]
	}
	return tr.resps[i], err
}

func limit429(body io.ReadCloser) *http.Response {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: body}
}

func okResp() *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}
}

// hookRecorder is a WallRetry hook that answers retry and records its
// calls and its done callback.
type hookRecorder struct {
	retry   bool
	calls   int
	account string
	sawHdr  string
	doneTo  string
	doneSt  int
	doneN   int
}

func (h *hookRecorder) hook(_ context.Context, account string, hdr http.Header) (bool, func(string, int)) {
	h.calls++
	h.account, h.sawHdr = account, hdr.Get("Anthropic-Ratelimit-Unified-Status")
	return h.retry, func(to string, status int) { h.doneN++; h.doneTo, h.doneSt = to, status }
}

func armedNet(tr http.RoundTripper, ch Chooser, h *hookRecorder, retargeted *string) *safetyNet {
	var drift atomic.Uint64
	return &safetyNet{
		base: tr, chooser: ch, account: "A", original: "Bearer original-token", drift: &drift,
		maxBody: maxWallRetryBody, wallRetry: h.hook,
		decision:   router.Decision{Class: router.Serving},
		onRetarget: func(to string) { *retargeted = to },
	}
}

func wallRequest(t *testing.T, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-A")
	return req
}

func TestWallRetryBodyCapIs32MiB(t *testing.T) {
	if maxWallRetryBody != 32<<20 {
		t.Fatalf("maxWallRetryBody = %d, want 32 MiB (spec §4a)", maxWallRetryBody)
	}
}

// TestWallRetryResendsOnceOnTheNewAccount: a limited 429 on A, the hook
// switches, Choose names B: the client gets B's 200, the upstream saw
// exactly two requests, the 429's body was closed unread, and the response
// now belongs to B.
func TestWallRetryResendsOnceOnTheNewAccount(t *testing.T) {
	first := &countingBody{}
	tr := &labelledTransport{resps: []*http.Response{limit429(first), okResp()}}
	ch := &wallChooser{acct: "B", tok: "tok-B", ok: true}
	h := &hookRecorder{retry: true}
	var retargeted string
	resp, err := armedNet(tr, ch, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader(`{"model":"x"}`)))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("RoundTrip = %v, %v; want B's 200", resp, err)
	}
	if strings.Join(tr.auths, ",") != "tok-A,tok-B" {
		t.Fatalf("upstream saw %v, want exactly A then B", tr.auths)
	}
	if h.calls != 1 || h.account != "A" || h.sawHdr != "rejected" {
		t.Fatalf("hook = %+v, want one call for A with the 429's headers", h)
	}
	if h.doneN != 1 || h.doneTo != "B" || h.doneSt != 200 {
		t.Fatalf("done = %d calls (%q, %d), want one (B, 200)", h.doneN, h.doneTo, h.doneSt)
	}
	if retargeted != "B" {
		t.Fatalf("onRetarget = %q, want B", retargeted)
	}
	if first.reads.Load() != 0 || !first.closed.Load() {
		t.Fatalf("the 429 body: %d reads, closed %v; want closed unread (§4a item 6)", first.reads.Load(), first.closed.Load())
	}
}

// The resend carries the same body as the original request.
func TestWallRetryResendsTheSameBody(t *testing.T) {
	var bodies []string
	tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			return limit429(io.NopCloser(strings.NewReader(""))), nil
		}
		return okResp(), nil
	})
	var retargeted string
	h := &hookRecorder{retry: true}
	if _, err := armedNet(tr, &wallChooser{acct: "B", tok: "tok-B", ok: true}, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader(`{"turn":42}`))); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != `{"turn":42}` || bodies[1] != bodies[0] {
		t.Fatalf("bodies = %q, want the same body twice", bodies)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestWallRetryPassesASecondLimitThroughWithNoThirdRequest (Review Focus
// 3): the resend is also limited; it is returned as is, and the hook is
// never asked again.
func TestWallRetryPassesASecondLimitThroughWithNoThirdRequest(t *testing.T) {
	second := limit429(io.NopCloser(strings.NewReader("")))
	tr := &labelledTransport{resps: []*http.Response{limit429(&countingBody{}), second}}
	h := &hookRecorder{retry: true}
	var retargeted string
	resp, err := armedNet(tr, &wallChooser{acct: "B", tok: "tok-B", ok: true}, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp != second {
		t.Fatalf("RoundTrip = %v, %v; want the resend's own 429", resp, err)
	}
	if len(tr.auths) != 2 || h.calls != 1 || h.doneSt != 429 {
		t.Fatalf("upstream %v, hook calls %d, done status %d; want 2 requests, 1 call, 429", tr.auths, h.calls, h.doneSt)
	}
}

// TestWallRetryWithNoTargetPassesTheOriginal429Through: every account is
// limited, so serving did not move and Choose names A again. The client
// gets the original 429, body unread and still open.
func TestWallRetryWithNoTargetPassesTheOriginal429Through(t *testing.T) {
	body := &countingBody{}
	orig := limit429(body)
	tr := &labelledTransport{resps: []*http.Response{orig}}
	h := &hookRecorder{retry: true}
	var retargeted string
	resp, err := armedNet(tr, &wallChooser{acct: "a", tok: "tok-A", ok: true}, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp != orig {
		t.Fatalf("RoundTrip = %v, %v; want the original 429 itself", resp, err)
	}
	if len(tr.auths) != 1 || body.reads.Load() != 0 || body.closed.Load() || retargeted != "" {
		t.Fatalf("upstream %v, reads %d, closed %v, retargeted %q; want one request and an untouched body", tr.auths, body.reads.Load(), body.closed.Load(), retargeted)
	}
	if h.doneN != 1 || h.doneTo != "" || h.doneSt != 0 {
		t.Fatalf("done = %d (%q, %d), want one call with no resend", h.doneN, h.doneTo, h.doneSt)
	}
}

// A hook that declines (not a limit, or auto-switch off) leaves everything
// as today: Choose is not asked again.
func TestWallRetryDeclinedPassesThrough(t *testing.T) {
	orig := limit429(&countingBody{})
	tr := &labelledTransport{resps: []*http.Response{orig}}
	ch := &wallChooser{acct: "B", tok: "tok-B", ok: true}
	var retargeted string
	resp, _ := armedNet(tr, ch, &hookRecorder{retry: false}, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if resp != orig || ch.chooses.Load() != 0 || len(tr.auths) != 1 {
		t.Fatalf("resp %v, chooses %d, upstream %v; want the 429 untouched", resp, ch.chooses.Load(), tr.auths)
	}
}

// TestWallRetryNeverResendsABodyOver32MiB (Review Focus 3): a declared
// body one byte over the cap is never buffered, so the hook is never
// asked and the 429 passes through.
func TestWallRetryNeverResendsABodyOver32MiB(t *testing.T) {
	orig := limit429(&countingBody{})
	tr := &labelledTransport{resps: []*http.Response{orig}}
	h := &hookRecorder{retry: true}
	req := wallRequest(t, io.LimitReader(zeros{}, maxWallRetryBody+1))
	req.ContentLength = maxWallRetryBody + 1 // 32 MiB + 1: over the cap
	var retargeted string
	resp, _ := armedNet(tr, &wallChooser{acct: "B", tok: "tok-B", ok: true}, h, &retargeted).RoundTrip(req)
	if resp != orig || h.calls != 0 || len(tr.auths) != 1 {
		t.Fatalf("resp %v, hook calls %d, upstream %v; want the 429 through and no hook", resp, h.calls, tr.auths)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A body whose read fails is not replayable either.
func TestWallRetryNeverResendsABodyWhoseReadFailed(t *testing.T) {
	orig := limit429(&countingBody{})
	tr := &labelledTransport{resps: []*http.Response{orig}}
	h := &hookRecorder{retry: true}
	var retargeted string
	resp, _ := armedNet(tr, &wallChooser{acct: "B", tok: "tok-B", ok: true}, h, &retargeted).RoundTrip(wallRequest(t, failingReader{}))
	if resp != orig || h.calls != 0 {
		t.Fatalf("resp %v, hook calls %d; want the 429 through and no hook", resp, h.calls)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

// TestWallRetryNeverResendsA401 (Review Focus 3): a 401 keeps the safety
// net's own path (F17) and never reaches the hook.
func TestWallRetryNeverResendsA401(t *testing.T) {
	tr := &labelledTransport{resps: []*http.Response{
		{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))},
		okResp(),
	}}
	h := &hookRecorder{retry: true}
	var retargeted string
	resp, err := armedNet(tr, &wallChooser{}, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp.StatusCode != 200 || h.calls != 0 {
		t.Fatalf("resp %v, err %v, hook calls %d; want the safety net's resend and no hook", resp, err, h.calls)
	}
	if strings.Join(tr.auths, ",") != "tok-A,original-token" {
		t.Fatalf("upstream saw %v, want the swapped attempt then the original login", tr.auths)
	}
}

// A resend that is itself refused (401 on B) still gets the safety net:
// refresh B, retry once (ruling 2).
func TestWallRetryResendThatIsRefusedStillGetsTheSafetyNet(t *testing.T) {
	tr := &labelledTransport{resps: []*http.Response{
		limit429(&countingBody{}),
		{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))},
		okResp(),
	}}
	ch := &wallChooser{acct: "B", tok: "tok-B", ok: true, refreshTok: "tok-B2"}
	var retargeted string
	resp, err := armedNet(tr, ch, &hookRecorder{retry: true}, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("RoundTrip = %v, %v", resp, err)
	}
	if strings.Join(tr.auths, ",") != "tok-A,tok-B,tok-B2" || strings.Join(ch.refreshed, ",") != "B" {
		t.Fatalf("upstream %v, refreshed %v; want A, B, then B refreshed", tr.auths, ch.refreshed)
	}
}

// TestWallRetryResendRefusedThenRefreshSucceedsReportsB (review round 1,
// item 1): B's resend is refused, the safety net's own refresh-and-retry on
// B succeeds — the response IS B's, so done must report (B, 200), not fire
// early with the resend's own 401.
func TestWallRetryResendRefusedThenRefreshSucceedsReportsB(t *testing.T) {
	tr := &labelledTransport{resps: []*http.Response{
		limit429(&countingBody{}),
		{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))},
		okResp(),
	}}
	ch := &wallChooser{acct: "B", tok: "tok-B", ok: true, refreshTok: "tok-B2"}
	h := &hookRecorder{retry: true}
	var retargeted string
	resp, err := armedNet(tr, ch, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("RoundTrip = %v, %v", resp, err)
	}
	if h.doneN != 1 || h.doneTo != "B" || h.doneSt != 200 {
		t.Fatalf("done = %d (%q, %d), want exactly one (B, 200): the refresh-and-retry on B answered", h.doneN, h.doneTo, h.doneSt)
	}
}

// TestWallRetryResendRefusedTwiceFallsBackToOriginalLoginReportsEmpty
// (review round 1, item 1): B's resend is refused, the refresh-and-retry on
// B is refused again too, so the safety net falls all the way back to the
// client's own original login. That response is NOT B's, so done must
// report ("", status), and the fallback still counts as drift.
func TestWallRetryResendRefusedTwiceFallsBackToOriginalLoginReportsEmpty(t *testing.T) {
	tr := &labelledTransport{resps: []*http.Response{
		limit429(&countingBody{}),
		{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))},
		okResp(),
	}}
	ch := &wallChooser{acct: "B", tok: "tok-B", ok: true, refreshTok: "tok-B2"}
	h := &hookRecorder{retry: true}
	var retargeted string
	sn := armedNet(tr, ch, h, &retargeted)
	resp, err := sn.RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("RoundTrip = %v, %v", resp, err)
	}
	if strings.Join(tr.auths, ",") != "tok-A,tok-B,tok-B2,original-token" {
		t.Fatalf("upstream saw %v, want A, B, B2, then the client's own login", tr.auths)
	}
	if h.doneN != 1 || h.doneTo != "" || h.doneSt != 200 {
		t.Fatalf("done = %d (%q, %d), want exactly one (\"\", 200): the response came from the original-login fallback, not B", h.doneN, h.doneTo, h.doneSt)
	}
	if sn.drift.Load() != 1 {
		t.Fatalf("drift = %d, want 1", sn.drift.Load())
	}
}

// A WallRetry hook that panics is treated as retry=false with no done
// call (review round 1, item 2): the 429 passes through and the connection
// is not dropped, mirroring how notifyUsage absorbs a panicking OnUsage.
func TestWallRetryPanickingHookIsADecline(t *testing.T) {
	orig := limit429(&countingBody{})
	tr := &labelledTransport{resps: []*http.Response{orig}}
	var drift atomic.Uint64
	sn := &safetyNet{
		base: tr, chooser: &wallChooser{acct: "B", tok: "tok-B", ok: true}, account: "A",
		original: "Bearer original-token", drift: &drift,
		maxBody: maxWallRetryBody,
		wallRetry: func(context.Context, string, http.Header) (bool, func(string, int)) {
			panic("boom")
		},
		decision: router.Decision{Class: router.Serving},
	}
	resp, err := sn.RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err != nil || resp != orig {
		t.Fatalf("RoundTrip = %v, %v; want the 429 through untouched despite the panic", resp, err)
	}
	if len(tr.auths) != 1 {
		t.Fatalf("upstream saw %v, want exactly one request (no resend)", tr.auths)
	}
}

// lengthCheckTransport records how many times it was called and the exact
// byte length of each request body it read.
type lengthCheckTransport struct {
	calls  int
	gotLen int64
}

func (tr *lengthCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.calls++
	b, _ := io.ReadAll(req.Body)
	tr.gotLen = int64(len(b))
	return limit429(io.NopCloser(strings.NewReader(""))), nil
}

// TestWallRetryNeverResendsAChunkedBodyOverCap (review round 1, item 4): a
// body with no declared length (chunked, ContentLength -1) that turns out
// to be one byte over the cap is not replayable either — it must still
// reach upstream byte-for-byte intact, and the hook is never asked.
func TestWallRetryNeverResendsAChunkedBodyOverCap(t *testing.T) {
	tr := &lengthCheckTransport{}
	h := &hookRecorder{retry: true}
	req := wallRequest(t, io.LimitReader(zeros{}, maxWallRetryBody+1))
	req.ContentLength = -1 // chunked: declared size unknown
	var retargeted string
	resp, err := armedNet(tr, &wallChooser{acct: "B", tok: "tok-B", ok: true}, h, &retargeted).RoundTrip(req)
	if err != nil || resp.StatusCode != 429 {
		t.Fatalf("RoundTrip = %v, %v; want the 429 through untouched", resp, err)
	}
	if tr.calls != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1 (no resend)", tr.calls)
	}
	if tr.gotLen != maxWallRetryBody+1 {
		t.Fatalf("upstream body length = %d, want %d (byte-for-byte)", tr.gotLen, maxWallRetryBody+1)
	}
	if h.calls != 0 {
		t.Fatalf("hook calls = %d, want 0: the body is not replayable", h.calls)
	}
}

// TestWallRetryFollowsAConcurrentTag: the hook switched serving to B, but
// a `chottag tag C` landed before the resend; Choose names C, and the
// resend goes there (spec §4a item 2: "whatever serving now is").
func TestWallRetryFollowsAConcurrentTag(t *testing.T) {
	tr := &labelledTransport{resps: []*http.Response{limit429(&countingBody{}), okResp()}}
	h := &hookRecorder{retry: true}
	var retargeted string
	if _, err := armedNet(tr, &wallChooser{acct: "C", tok: "tok-C", ok: true}, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}"))); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tr.auths, ",") != "tok-A,tok-C" || h.doneTo != "C" || retargeted != "C" {
		t.Fatalf("upstream %v, done %q, retargeted %q; want the resend on C", tr.auths, h.doneTo, retargeted)
	}
}

// A transport error on the resend is returned, and done hears status 0.
func TestWallRetryResendTransportErrorReportsStatusZero(t *testing.T) {
	tr := &labelledTransport{resps: []*http.Response{limit429(&countingBody{}), nil}, errs: []error{nil, errors.New("reset")}}
	h := &hookRecorder{retry: true}
	var retargeted string
	_, err := armedNet(tr, &wallChooser{acct: "B", tok: "tok-B", ok: true}, h, &retargeted).RoundTrip(wallRequest(t, strings.NewReader("{}")))
	if err == nil || h.doneTo != "B" || h.doneSt != 0 {
		t.Fatalf("err %v, done (%q, %d); want the error and (B, 0)", err, h.doneTo, h.doneSt)
	}
}

// transportFor arms only a serving-class request with no object owner.
func TestTransportForArmsOnlyUnownedServing(t *testing.T) {
	s := New(Config{Choose: &wallChooser{}, WallRetry: (&hookRecorder{}).hook})
	cases := []struct {
		d    router.Decision
		want bool
	}{
		{router.Decision{Class: router.Serving}, true},
		{router.Decision{Class: router.Serving, Object: router.KindSession}, false},
		{router.Decision{Class: router.Remote}, false},
	}
	for _, c := range cases {
		sn, ok := s.transportFor("A", "", "", c.d, "", false, false, false, nil, nil).(*safetyNet)
		if !ok {
			t.Fatalf("%+v: not a safety net", c.d)
		}
		if armed := sn.wallRetry != nil; armed != c.want || (armed && sn.maxBody != maxWallRetryBody) {
			t.Errorf("%+v: armed %v (maxBody %d), want %v", c.d, armed, sn.maxBody, c.want)
		}
	}
	if sn := New(Config{Choose: &wallChooser{}}).transportFor("A", "", "", router.Decision{Class: router.Serving}, "", false, false, false, nil, nil).(*safetyNet); sn.wallRetry != nil || sn.maxBody != 0 {
		t.Fatal("armed without Config.WallRetry")
	}
}
