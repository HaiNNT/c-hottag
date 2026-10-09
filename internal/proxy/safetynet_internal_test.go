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

// These tests exercise safetyNet.RoundTrip directly, with a scripted fake
// transport, instead of going through the full CONNECT/MITM stack: the
// scenario under test — a transport-level failure on the refreshed retry,
// with and without the client's own context already done — depends only on
// safetyNet's own logic, and pinning it to real TCP/TLS teardown timing
// would make the test slow and non-deterministic for no added coverage.

// fakeChooser is a minimal Chooser: Choose is never exercised here.
type fakeChooser struct {
	refreshTok string
	refreshOK  bool
}

func (c fakeChooser) Choose(context.Context, router.Decision, string) (string, string, bool, bool) {
	return "", "", false, false
}
func (c fakeChooser) Record(router.Kind, []string, string) {}
func (c fakeChooser) Refresh(context.Context, string) (string, bool) {
	return c.refreshTok, c.refreshOK
}

var _ Chooser = fakeChooser{}

// authStepLabel names which login attempt a scripted call carried, never
// the header value itself.
func authStepLabel(auth string) string {
	switch auth {
	case "Bearer swapped-token":
		return "swapped"
	case "Bearer fresh-token":
		return "fresh"
	case "Bearer fresh2-token":
		return "fresh2"
	case "Bearer original-token":
		return "original"
	default:
		return "unexpected"
	}
}

type scriptedStep struct {
	resp *http.Response
	err  error
}

// scriptedTransport hands back one scripted step per call, in order, and
// records only a label for each Authorization header it saw.
type scriptedTransport struct {
	steps []scriptedStep
	i     int
	auths []string
}

func (tr *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.auths = append(tr.auths, authStepLabel(req.Header.Get("Authorization")))
	s := tr.steps[tr.i]
	tr.i++
	return s.resp, s.err
}

func statusResp(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}
}

func newSafetyNetTestRequest(ctx context.Context) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer swapped-token")
	return req
}

func wantAuthSteps(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("upstream saw %d attempts %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("attempt %d was %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestSafetyNetFallsBackAfterRetryTransportError covers a transport-level
// failure (e.g. a reset connection) on the refreshed retry, not a refused
// status. Before the fix, `err != nil` alone returned it to the caller as a
// synthesized failure; that skips the resend entirely, which is exactly the
// case the safety net exists for, since the first refusal's response is
// already drained by that point. It must instead fall through to the
// original-login resend.
func TestSafetyNetFallsBackAfterRetryTransportError(t *testing.T) {
	tr := &scriptedTransport{steps: []scriptedStep{
		{resp: statusResp(http.StatusNotFound)}, // initial swapped attempt: refused
		{err: errors.New("connection reset")},   // refreshed retry: transport error, client still here
		{resp: statusResp(http.StatusOK)},       // original-login resend: succeeds
	}}
	var drift atomic.Uint64
	marked := false
	sn := &safetyNet{
		base: tr, chooser: fakeChooser{refreshTok: "fresh-token", refreshOK: true},
		account: "acct", original: "Bearer original-token",
		drift: &drift, onDrift: func() { marked = true },
	}

	resp, err := sn.RoundTrip(newSafetyNetTestRequest(context.Background()))
	if err != nil {
		t.Fatalf("RoundTrip error = %v, want nil (a transport error on the retry must fall back, not surface)", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	wantAuthSteps(t, tr.auths, []string{"swapped", "fresh", "original"})
	if got := drift.Load(); got != 1 {
		t.Fatalf("drift = %d, want 1", got)
	}
	if !marked {
		t.Fatal("onDrift was not called")
	}
}

// TestSafetyNetDoesNotResendAfterClientGone covers the other half of the
// same fix: when the retry fails because the client's own context is
// already done (canceled or timed out), the safety net must report that
// error instead of making a pointless extra upstream call — the client has
// gone away, so there is no one to deliver a resend to.
func TestSafetyNetDoesNotResendAfterClientGone(t *testing.T) {
	tr := &scriptedTransport{steps: []scriptedStep{
		{resp: statusResp(http.StatusNotFound)}, // initial swapped attempt: refused
		{err: errors.New("connection reset")},   // refreshed retry: fails while the client is gone
	}}
	var drift atomic.Uint64
	sn := &safetyNet{
		base: tr, chooser: fakeChooser{refreshTok: "fresh-token", refreshOK: true},
		account: "acct", original: "Bearer original-token", drift: &drift,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone by the time the retry fails
	req := newSafetyNetTestRequest(ctx)

	_, err := sn.RoundTrip(req)
	if err == nil {
		t.Fatal("RoundTrip error = nil, want the retry's error surfaced (client is gone: no resend)")
	}
	wantAuthSteps(t, tr.auths, []string{"swapped", "fresh"})
	if got := drift.Load(); got != 0 {
		t.Fatalf("drift = %d, want 0 (no resend happened)", got)
	}
}

// rejectedChooser answers RefreshRejected from a script and records the
// token each call was told was refused.
type rejectedChooser struct {
	fakeChooser
	script   []string // tokens to return, "" meaning no newer token
	rejected []string
}

func (c *rejectedChooser) RefreshRejected(_ context.Context, _, rejected string) (string, bool) {
	c.rejected = append(c.rejected, rejected)
	if len(c.script) == 0 {
		return "", false
	}
	tok := c.script[0]
	c.script = c.script[1:]
	return tok, tok != ""
}

// R176: the retry's token was refused because a renewal made a newer one in
// the meantime: it is tried too, and the request succeeds without a Home
// resend or a refusal.
func TestSafetyNetRetriesAgainWithANewerRenewedToken(t *testing.T) {
	tr := &scriptedTransport{steps: []scriptedStep{
		{resp: statusResp(http.StatusUnauthorized)},
		{resp: statusResp(http.StatusUnauthorized)},
		{resp: statusResp(http.StatusOK)},
	}}
	var drift atomic.Uint64
	refusals := 0
	ch := &rejectedChooser{script: []string{"fresh-token", "fresh2-token"}}
	sn := &safetyNet{
		base: tr, chooser: ch, account: "acct", original: "Bearer original-token",
		drift: &drift, serving: true,
		onServingRefusal: func(string, int, string, bool) { refusals++ },
	}
	resp, err := sn.RoundTrip(newSafetyNetTestRequest(context.Background()))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("RoundTrip = %v, %v; want 200", resp, err)
	}
	wantAuthSteps(t, tr.auths, []string{"swapped", "fresh", "fresh2"})
	if want := []string{"swapped-token", "fresh-token"}; len(ch.rejected) != 2 || ch.rejected[0] != want[0] || ch.rejected[1] != want[1] {
		t.Fatalf("the chooser was told %v was refused, want %v", ch.rejected, want)
	}
	if refusals != 0 || drift.Load() != 0 {
		t.Fatalf("refusals = %d, drift = %d, want none: a renewal explains the 401", refusals, drift.Load())
	}
}

// R176: the renewed token is refused too and no newer one exists: still the
// account's refusal, resent on the client's own login and reported.
func TestSafetyNetStillResendsWhenTheRenewedTokenIsRefused(t *testing.T) {
	tr := &scriptedTransport{steps: []scriptedStep{
		{resp: statusResp(http.StatusUnauthorized)},
		{resp: statusResp(http.StatusUnauthorized)},
		{resp: statusResp(http.StatusOK)},
	}}
	var drift atomic.Uint64
	var got []bool
	ch := &rejectedChooser{script: []string{"fresh-token"}}
	sn := &safetyNet{
		base: tr, chooser: ch, account: "acct", original: "Bearer original-token",
		drift: &drift, serving: true,
		onServingRefusal: func(_ string, _ int, _ string, resent bool) { got = append(got, resent) },
	}
	resp, err := sn.RoundTrip(newSafetyNetTestRequest(context.Background()))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("RoundTrip = %v, %v; want 200", resp, err)
	}
	wantAuthSteps(t, tr.auths, []string{"swapped", "fresh", "original"})
	if len(got) != 1 || !got[0] {
		t.Fatalf("serving refusals = %v, want one resent", got)
	}
}

// R176: no renewal to wait for (no newer token): the pre-existing path, one
// retry at most, then the resend.
func TestSafetyNetWithNoRenewalResendsAsBefore(t *testing.T) {
	tr := &scriptedTransport{steps: []scriptedStep{
		{resp: statusResp(http.StatusUnauthorized)},
		{resp: statusResp(http.StatusOK)},
	}}
	var drift atomic.Uint64
	ch := &rejectedChooser{}
	sn := &safetyNet{
		base: tr, chooser: ch, account: "acct", original: "Bearer original-token",
		drift: &drift, serving: true,
	}
	if _, err := sn.RoundTrip(newSafetyNetTestRequest(context.Background())); err != nil {
		t.Fatal(err)
	}
	wantAuthSteps(t, tr.auths, []string{"swapped", "original"})
}

// After the wall retry moved the request to another account, a refusal asks
// the chooser about the token that account was sent, not the old one.
func TestSafetyNetRefusalAfterWallRetryNamesTheBearerSent(t *testing.T) {
	tr := &scriptedTransport{steps: []scriptedStep{
		{resp: statusResp(http.StatusUnauthorized)},
	}}
	ch := &rejectedChooser{}
	sn := &safetyNet{base: tr, chooser: ch, account: "acct", original: "Bearer original-token", drift: &atomic.Uint64{},
		sentBearer: "moved-token", noOriginal: true}
	if _, err := sn.RoundTrip(newSafetyNetTestRequest(context.Background())); err != nil {
		t.Fatal(err)
	}
	if len(ch.rejected) != 1 || ch.rejected[0] != "moved-token" {
		t.Fatalf("the chooser was told %v was refused, want [moved-token]", ch.rejected)
	}
}
