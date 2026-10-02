package proxy_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// ownerGuardChooser is guardChooser reporting every swap as owner-routed.
type ownerGuardChooser struct{ guardChooser }

func (c *ownerGuardChooser) ChooseGuarded(ctx context.Context, d router.Decision, id string) (string, string, bool, bool, string) {
	a, t, _, ok, why := c.guardChooser.ChooseGuarded(ctx, d, id)
	return a, t, ok, ok, why
}

var _ proxy.PoolGuard = (*ownerGuardChooser)(nil)

// homeResendProbe is an upstream that refuses every swapped bearer and
// accepts Home's, recording what it saw.
func homeResendProbe(seen *[]string, mu *sync.Mutex) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = append(*seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), "tok-") {
			w.WriteHeader(http.StatusForbidden)
		}
	})
}

func send(t *testing.T, h *proxytest.Harness, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-home")
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func assertNoHome(t *testing.T, resp *http.Response, seen []string) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want the upstream refusal passed back", resp.StatusCode)
	}
	for _, a := range seen {
		if strings.Contains(a, "sk-ant-oat01-home") {
			t.Fatal("a remote or owner request was resent on Home's login")
		}
	}
	if len(seen) != 2 { // the swapped attempt and the refreshed retry
		t.Fatalf("upstream saw %d attempts, want 2", len(seen))
	}
}

// R147: with a single pool (Guarded false), a refused remote-class request
// is never resent on Home's login.
func TestSafetyNetNeverResendsARemoteRequestOnHome(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	h := proxytest.Start(t, homeResendProbe(&seen, &mu), proxytest.Options{Choose: &guardChooser{guarded: false}})
	resp := send(t, h, "https://api.anthropic.com/v1/code/sessions")
	defer resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	assertNoHome(t, resp, seen)
}

// An owner-routed request is never resent on Home either, whatever its route.
func TestSafetyNetNeverResendsAnOwnerRequestOnHome(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	h := proxytest.Start(t, homeResendProbe(&seen, &mu), proxytest.Options{Choose: &ownerGuardChooser{guardChooser{guarded: false}}})
	resp := send(t, h, "https://api.anthropic.com/v1/messages")
	defer resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	assertNoHome(t, resp, seen)
}

// The refusal for a remote account is the local Anthropic-shaped 503, with
// nothing sent upstream.
func TestRemoteRefusalIsALocal503(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	msg := "chottag: account A (remote) has no usable login right now; run: chottag login A"
	h := proxytest.Start(t, homeResendProbe(&seen, &mu), proxytest.Options{Choose: &guardChooser{refusal: msg, guarded: false}})
	resp := send(t, h, "https://api.anthropic.com/v1/code/sessions")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("a refused request reached upstream: %v", seen)
	}
}
