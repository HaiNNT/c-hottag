package proxy_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// refusingRefreshChooser swaps every serving request onto account "D" and
// always fails Refresh, forcing the safety net straight to an original-login
// resend (Drift) the first time the swapped token is refused.
type refusingRefreshChooser struct{}

func (refusingRefreshChooser) Choose(context.Context, router.Decision, string) (string, string, bool, bool) {
	return "D", "tok-stale-D", false, true
}

func (refusingRefreshChooser) Record(router.Kind, []string, string) {}

func (refusingRefreshChooser) Refresh(context.Context, string) (string, bool) { return "", false }

func TestOnUsageSeesHeadersOfASwappedResponse(t *testing.T) {
	var mu sync.Mutex
	type seen struct {
		account string
		status  int
		limit   string
	}
	var got []seen

	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
		w.WriteHeader(200)
	}), proxytest.Options{
		Choose: servingSwap{},
		OnUsage: func(account string, status int, hdr http.Header) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, seen{account, status, hdr.Get("Anthropic-Ratelimit-Unified-5h-Utilization")})
		},
	})

	do(t, h, "POST", "https://api.anthropic.com/v1/messages")

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("OnUsage called %d times, want 1", len(got))
	}
	if got[0].account != "B" || got[0].status != 200 || got[0].limit != "0.42" {
		t.Fatalf("got %+v, want account B status 200 utilization 0.42", got[0])
	}
}

// TestOnUsageIsNotCalledForAnUnswappedRequest guards the "headers only for a
// swapped response" rule: usage belongs to an account, and an unswapped
// request (no Choose configured, so account stays "") has none.
func TestOnUsageIsNotCalledForAnUnswappedRequest(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}), proxytest.Options{
		OnUsage: func(string, int, http.Header) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})
	do(t, h, "GET", "https://api.anthropic.com/api/claude_cli/bootstrap")
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("OnUsage called %d times for an unswapped request; usage belongs to an account, and there is none", calls)
	}
}

// TestOnUsageNotCalledOnDriftedResponse guards F26: when the safety net gives
// up on the swapped account (refresh failed, retry still refused) it resends
// the request on the CLIENT'S OWN login and returns that response. account
// still names the swapped-to account, but the headers this response carries
// are Home's, not that account's — recording them here would let M4 rotate
// an account on a number it never produced.
func TestOnUsageNotCalledOnDriftedResponse(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+homeTok {
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.99")
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	h := proxytest.Start(t, up, proxytest.Options{
		Choose: refusingRefreshChooser{},
		OnUsage: func(string, int, http.Header) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})
	do(t, h, "POST", "https://api.anthropic.com/v1/messages")
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("OnUsage called %d times on a drifted response; want 0 — its quota headers are Home's, not the swapped account's", calls)
	}
}

// TestOnUsageIsNotCalledForAnUntouchedRouteWithChooserConfigured is the
// negative test that actually exercises the `account != ""` guard: under
// `proxy run` Choose is never nil, unlike
// TestOnUsageIsNotCalledForAnUnswappedRequest's trace-only Options{}. A
// guard mutated to `s.cfg.Choose != nil` would fire OnUsage here even though
// nothing was swapped; this test catches exactly that.
func TestOnUsageIsNotCalledForAnUntouchedRouteWithChooserConfigured(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}), proxytest.Options{
		Choose: servingSwap{},
		OnUsage: func(string, int, http.Header) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})
	do(t, h, "GET", "https://api.anthropic.com/v1/environments/env_01ABCDEFGH12345678/work/poll")
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("OnUsage called %d times for an Untouched-class request with a chooser configured; want 0", calls)
	}
}

// TestOnUsagePanicDoesNotHarmTheResponse guards notifyUsage's recover: a
// panicking callback must not turn a response the client was about to
// receive into a dropped connection.
func TestOnUsagePanicDoesNotHarmTheResponse(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}), proxytest.Options{
		Choose:  servingSwap{},
		OnUsage: func(string, int, http.Header) { panic("boom") },
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatalf("client saw %v, want no error: a panicking OnUsage must not harm the response", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("got status=%d body=%q, want 200 {\"ok\":true}", resp.StatusCode, body)
	}
}

// TestOnUsageCannotMutateTheClientsResponse guards notifyUsage's h.Clone():
// the callback is otherwise handed the live response header map and could
// rewrite what the client actually receives.
func TestOnUsageCannotMutateTheClientsResponse(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}), proxytest.Options{
		Choose:  servingSwap{},
		OnUsage: func(_ string, _ int, hdr http.Header) { hdr.Set("Content-Type", "text/plain") },
	})
	resp := do(t, h, "POST", "https://api.anthropic.com/v1/messages")
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("client saw Content-Type %q, want application/json: OnUsage must not be able to mutate the live response", ct)
	}
}

// TestOnUsageFiresOnceOnANonSuccessSwappedResponse: 429 is the case usage
// tracking exists for, and (unlike 401/403/404) never triggers the safety
// net, so it is a clean check that the hook fires on a refusal too.
func TestOnUsageFiresOnceOnANonSuccessSwappedResponse(t *testing.T) {
	var mu sync.Mutex
	var got []int
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.WriteHeader(429)
	}), proxytest.Options{
		Choose: servingSwap{},
		OnUsage: func(_ string, status int, _ http.Header) {
			mu.Lock()
			got = append(got, status)
			mu.Unlock()
		},
	})
	do(t, h, "POST", "https://api.anthropic.com/v1/messages")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != 429 {
		t.Fatalf("got %v, want exactly one call with status 429", got)
	}
}

// TestOnUsageFiresOnceForAStreamAndTheClientStillReceivesEveryChunk
// substantiates the headers-only design claim: the hook fires once, from the
// headers alone, before the body finishes streaming, and the client still
// receives every chunk of it.
func TestOnUsageFiresOnceForAStreamAndTheClientStillReceivesEveryChunk(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.5")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "data: two\n\n")
	}), proxytest.Options{
		Choose: servingSwap{},
		OnUsage: func(string, int, http.Header) {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	first, err := r.ReadString('\n')
	if err != nil || first != "data: one\n" {
		t.Fatalf("first chunk = %q, err %v", first, err)
	}

	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("OnUsage called %d times before the stream finished, want exactly 1", calls)
	}
	mu.Unlock()

	close(release)
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading rest of stream: %v", err)
	}
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("client did not receive the second chunk, got %q", rest)
	}
}

// TestOnUsagePanicIsReportedThroughOnLogError guards against a broken OnUsage
// consumer silently and permanently stopping usage recording: notifyUsage
// must still report the panic (via the existing OnLogError hook, with a
// fixed string — never the recovered value, which could carry data derived
// from the response headers the callback was handed) while continuing to
// absorb it, so the client still gets its response.
func TestOnUsagePanicIsReportedThroughOnLogError(t *testing.T) {
	var logMu sync.Mutex
	var logErrs []error
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}), proxytest.Options{
		Choose:  servingSwap{},
		OnUsage: func(string, int, http.Header) { panic("boom") },
		OnLogError: func(err error) {
			logMu.Lock()
			logErrs = append(logErrs, err)
			logMu.Unlock()
		},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatalf("client saw %v, want no error: a panicking OnUsage must not harm the response", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
		t.Fatalf("got status=%d body=%q, want 200 {\"ok\":true}", resp.StatusCode, body)
	}

	logMu.Lock()
	defer logMu.Unlock()
	if len(logErrs) != 1 {
		t.Fatalf("OnLogError called %d times, want exactly 1", len(logErrs))
	}
	if strings.Contains(logErrs[0].Error(), "boom") {
		t.Fatalf("logged error %q must not carry the recovered panic value", logErrs[0])
	}
}

// TestPanickingUsageHookReportsThroughOnUsageError guards notifyUsage's
// routing: a broken OnUsage consumer silently and permanently stops usage
// recording, and reporting that through OnLogError (as it used to) sends
// anyone reading the daemon log to the wrong subsystem — the trace log is
// fine, the usage hook is not. When OnUsageError is set it must receive the
// report instead, and OnLogError must stay silent.
func TestPanickingUsageHookReportsThroughOnUsageError(t *testing.T) {
	var mu sync.Mutex
	var usageErr, logErr error
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}), proxytest.Options{
		Choose:  servingSwap{},
		OnUsage: func(string, int, http.Header) { panic("boom") },
		OnUsageError: func(err error) {
			mu.Lock()
			usageErr = err
			mu.Unlock()
		},
		OnLogError: func(err error) {
			mu.Lock()
			logErr = err
			mu.Unlock()
		},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatalf("client saw %v, want no error: a panicking OnUsage must not harm the response", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if usageErr == nil {
		t.Fatal("a panicking OnUsage did not reach OnUsageError")
	}
	if usageErr.Error() != "OnUsage callback panicked" {
		// Fixed string only: the callback was handed response headers, so a
		// value derived from them (e.g. the recovered panic value) must
		// never reach this text, which internal/cli/trace.go writes
		// verbatim to stderr.
		t.Errorf("reported error = %q, want exactly %q", usageErr, "OnUsage callback panicked")
	}
	if logErr != nil {
		t.Errorf("a usage-callback panic was reported as a trace-log failure: %v", logErr)
	}
}

// TestPanickingUsageHookFallsBackToOnLogError guards back-compat: a caller
// that only wired OnLogError (the pre-OnUsageError shape) must keep being
// told about a panicking OnUsage, rather than losing the report entirely.
func TestPanickingUsageHookFallsBackToOnLogError(t *testing.T) {
	var mu sync.Mutex
	var logErr error
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}), proxytest.Options{
		Choose:  servingSwap{},
		OnUsage: func(string, int, http.Header) { panic("boom") },
		OnLogError: func(err error) {
			mu.Lock()
			logErr = err
			mu.Unlock()
		},
	})
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatalf("client saw %v, want no error: a panicking OnUsage must not harm the response", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if logErr == nil {
		t.Fatal("with OnUsageError unset, the panic must still reach OnLogError")
	}
}
