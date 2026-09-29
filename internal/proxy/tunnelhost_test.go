package proxy_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/router"
)

// countingChooser counts every Choose call and always swaps to account "B".
// Task 6 reuses this name and shape.
type countingChooser struct {
	n atomic.Int32
}

func (c *countingChooser) Choose(context.Context, router.Decision, string) (string, string, bool, bool) {
	c.n.Add(1)
	return "B", "sk-ant-oat01-SWAPPED", false, true
}

func (c *countingChooser) Record(router.Kind, []string, string) {}

func (c *countingChooser) Refresh(context.Context, string) (string, bool) { return "", false }

func TestTunnelRejectsAMismatchedHostWith421(t *testing.T) {
	var hits atomic.Int32
	ch := &countingChooser{}
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }), proxytest.Options{Choose: ch})
	for _, host := range []string{"evil.example", "api.anthropic.com:8443"} {
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
		req.Host = host // CONNECT still goes to api.anthropic.com:443 (URL.Host)
		req.Header.Set("Authorization", "Bearer sk-ant-oat01-HOME")
		resp, err := h.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Errorf("Host %q: status %d, want 421", host, resp.StatusCode)
		}
	}
	if ch.n.Load() != 0 || hits.Load() != 0 {
		t.Fatalf("Choose called %d times, upstream hit %d times; want 0 and 0", ch.n.Load(), hits.Load())
	}
	if h.LogContains(t, "evil.example") {
		t.Error("the client's Host value reached the log")
	}
}

func TestTunnelSendsTheConnectHostUpstream(t *testing.T) {
	var gotHost string
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotHost = r.Host }), proxytest.Options{})
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", nil)
	req.Host = "API.Anthropic.com:443"
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || gotHost != "api.anthropic.com" {
		t.Fatalf("status %d, upstream Host %q; want 200 and the CONNECT host without :443", resp.StatusCode, gotHost)
	}
}
