package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// guardChooser is a PoolGuard: it refuses every serving request, or swaps it
// to account when refusal is empty, with the pool boundary on.
type guardChooser struct {
	refusal string
	guarded bool
	refresh atomic.Int32
}

func (c *guardChooser) Choose(ctx context.Context, d router.Decision, id string) (string, string, bool, bool) {
	a, t, o, ok, _ := c.ChooseGuarded(ctx, d, id)
	return a, t, o, ok
}

func (c *guardChooser) ChooseGuarded(_ context.Context, d router.Decision, _ string) (string, string, bool, bool, string) {
	if d.Class == router.Untouched {
		return "", "", false, false, ""
	}
	if c.refusal != "" {
		return "", "", false, false, c.refusal
	}
	return "W", "tok-W", false, true, ""
}
func (c *guardChooser) Guarded() bool                        { return c.guarded }
func (c *guardChooser) Record(router.Kind, []string, string) {}
func (c *guardChooser) Refresh(context.Context, string) (string, bool) {
	c.refresh.Add(1)
	return "tok-fresh", true
}

var _ proxy.PoolGuard = (*guardChooser)(nil)

func postMessages(t *testing.T, h *proxytest.Harness) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// A refused request is answered locally: 503, an Anthropic-shaped error
// naming the pool, and nothing reaches upstream (so never Home's login).
func TestPoolGuardRefusalIsALocal503AndNeverGoesUpstream(t *testing.T) {
	var upstream atomic.Int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstream.Add(1) })
	msg := `chottag: no account in pool "work" can serve this request (run: chottag pool)`
	h := proxytest.Start(t, up, proxytest.Options{Choose: &guardChooser{refusal: msg, guarded: true}})
	resp := postMessages(t, h)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body.Type != "error" || body.Error.Type != "api_error" || body.Error.Message != msg {
		t.Fatalf("body = %s", raw)
	}
	if upstream.Load() != 0 {
		t.Fatal("a refused request reached upstream")
	}
}

// With the boundary on (more than one pool) the safety net never resends on
// the client's own login: the swapped account's refusal goes back.
func TestPoolGuardNeverResendsOnHomesLogin(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: &guardChooser{guarded: true}})
	resp := postMessages(t, h)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want the refusal passed back", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range seen {
		if strings.Contains(a, "sk-ant-oat01-home") {
			t.Fatal("a request went out on Home's login")
		}
	}
	if len(seen) != 2 { // the swapped attempt and the refreshed retry
		t.Fatalf("upstream saw %d attempts, want 2", len(seen))
	}
}

// Without the boundary (one pool) today's resend stays.
func TestPoolGuardOffKeepsTheHomeLoginResend(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), "tok-") {
			w.WriteHeader(http.StatusForbidden)
		}
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: &guardChooser{guarded: false}})
	resp := postMessages(t, h)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the Home-login resend's 200", resp.StatusCode)
	}
}
