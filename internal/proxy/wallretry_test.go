package proxy_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// servingState is a chooser whose serving account a WallRetry hook can
// move, as the daemon's compare-and-swap does. Every non-untouched class
// is swapped onto the current serving account.
type servingState struct {
	mu      sync.Mutex
	serving string
}

func (s *servingState) Choose(_ context.Context, d router.Decision, _ string) (string, string, bool, bool) {
	if d.Class == router.Untouched {
		return "", "", false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serving, "tok-" + s.serving, false, true
}
func (s *servingState) Record(router.Kind, []string, string)           {}
func (s *servingState) Refresh(context.Context, string) (string, bool) { return "", false }

func (s *servingState) set(name string) {
	s.mu.Lock()
	s.serving = name
	s.mu.Unlock()
}

// wallUpstream answers A with a limited 429 (and a body that must never
// reach a log) and every other account with 200 "ok from <account>". It
// counts requests.
type wallUpstream struct {
	mu    sync.Mutex
	auths []string
}

func (u *wallUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	io.Copy(io.Discard, r.Body)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.auths = append(u.auths, tok)
	u.mu.Unlock()
	if tok == "tok-A" {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"SECRET-429-BODY"}}`)
		return
	}
	w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.10")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok from "+strings.TrimPrefix(tok, "tok-"))
}

func (u *wallUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...)
}

func wallPost(t *testing.T, h *proxytest.Harness, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestWallRetryEndToEnd is spec §9's first wall-retry case through the
// real proxy: A's limited 429 becomes B's 200 for the client, with exactly
// two upstream requests; the usage hook sees B's response as B's; the
// trace record names B; the 429's body appears in no log.
func TestWallRetryEndToEnd(t *testing.T) {
	up := &wallUpstream{}
	st := &servingState{serving: "A"}
	var mu sync.Mutex
	var usage []string
	var done []string
	h := proxytest.Start(t, up, proxytest.Options{
		Choose: st,
		OnUsage: func(account string, status int, _ http.Header) {
			mu.Lock()
			usage = append(usage, account)
			mu.Unlock()
		},
		WallRetry: func(_ context.Context, account string, _ http.Header) (bool, func(string, int)) {
			st.set("B") // the daemon's planner switched
			return true, func(to string, status int) {
				mu.Lock()
				done = append(done, to)
				mu.Unlock()
			}
		},
	})
	code, body := wallPost(t, h, "https://api.anthropic.com/v1/messages")
	if code != 200 || body != "ok from B" {
		t.Fatalf("client got %d %q, want B's 200", code, body)
	}
	if got := up.seen(); strings.Join(got, ",") != "tok-A,tok-B" {
		t.Fatalf("upstream saw %v, want exactly A then B", got)
	}
	r := h.Records(t, "req", 1)[0]
	if r.Account != "B" || r.Status != 200 {
		t.Fatalf("trace record = %+v, want account B, status 200", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(usage, ",") != "B" || strings.Join(done, ",") != "B" {
		t.Fatalf("OnUsage saw %v and done %v; want B's response attributed to B", usage, done)
	}
	if h.LogContains(t, "SECRET-429-BODY") {
		t.Fatal("the refused 429's body reached the trace log")
	}
}

// TestWallRetryNeverResendsALimitInsideA200Stream (Review Focus 3): a limit
// that arrives as an SSE error event inside a 200 has already started
// reaching the client, so the hook is never asked (F13).
func TestWallRetryNeverResendsALimitInsideA200Stream(t *testing.T) {
	var mu sync.Mutex
	requests, hooks := 0, 0
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\"}}\n\n")
	}), proxytest.Options{
		Choose: &servingState{serving: "A"},
		WallRetry: func(context.Context, string, http.Header) (bool, func(string, int)) {
			mu.Lock()
			hooks++
			mu.Unlock()
			return true, nil
		},
	})
	code, body := wallPost(t, h, "https://api.anthropic.com/v1/messages")
	if code != 200 || !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("client got %d %q, want the stream as sent", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 || hooks != 0 {
		t.Fatalf("%d upstream requests, %d hook calls; want 1 and 0", requests, hooks)
	}
}

// TestWallRetryNeverArmsForRemote (Review Focus 3): a remote-class request
// (a Remote Control session create) that comes back 429 passes through.
func TestWallRetryNeverArmsForRemote(t *testing.T) {
	up := &wallUpstream{}
	hooks := 0
	var mu sync.Mutex
	h := proxytest.Start(t, up, proxytest.Options{
		Choose: &servingState{serving: "A"},
		WallRetry: func(context.Context, string, http.Header) (bool, func(string, int)) {
			mu.Lock()
			hooks++
			mu.Unlock()
			return true, nil
		},
	})
	code, _ := wallPost(t, h, "https://api.anthropic.com/v1/code/sessions")
	mu.Lock()
	defer mu.Unlock()
	if code != 429 || hooks != 0 || len(up.seen()) != 1 {
		t.Fatalf("client got %d, %d hook calls, upstream %v; want the 429 through untouched", code, hooks, up.seen())
	}
}
