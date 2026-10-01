package proxy_test

import (
	"bufio"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

const idleQuiet = 5 * time.Minute

// A new server counts its construction as the last request start: a daemon
// that was just started cannot know it has been quiet for 5 minutes.
func TestAFreshServerIsNotIdleForItsFirstQuietWindow(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(200, nil), proxytest.Options{})
	now := time.Now()
	if h.Server.Idle(now, idleQuiet) {
		t.Fatal("a proxy that has just started must not be idle")
	}
	if !h.Server.Idle(now.Add(idleQuiet+time.Second), idleQuiet) {
		t.Fatal("a proxy that has served nothing for the quiet window must be idle")
	}
}

func TestIdleNeedsTheQuietWindowAfterTheLastRequestStarted(t *testing.T) {
	h := proxytest.Start(t, sseUpstream(200, nil), proxytest.Options{})
	postStream(t, h, `{}`)
	now := time.Now()
	if h.Server.Idle(now, idleQuiet) {
		t.Fatal("idle right after a request")
	}
	if h.Server.Idle(now.Add(4*time.Minute), idleQuiet) {
		t.Fatal("idle 4 minutes after a request, want busy")
	}
	// The client's tunnel is still open here with no request in it: that
	// must not block.
	waitIdleAt(t, h, now.Add(6*time.Minute))
}

func TestIdleNeverWhileAStreamIsOpen(t *testing.T) {
	up, release := gatedStream(t, "text/event-stream")
	defer release()
	h := proxytest.Start(t, up, proxytest.Options{})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if h.Server.Idle(time.Now().Add(time.Hour), idleQuiet) {
		t.Fatal("idle while a streaming response is still open")
	}
	release()
	io.ReadAll(br)
	waitIdleAt(t, h, time.Now().Add(time.Hour))
}

// waitIdleAt waits for the proxy to read as idle at at. The handler's return
// races the client's read of the last response byte, so the in-flight count
// falls an instant after the client is done. The deadline is a hang guard.
func waitIdleAt(t *testing.T, h *proxytest.Harness, at time.Time) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !h.Server.Idle(at, idleQuiet) {
		if time.Now().After(deadline) {
			t.Fatal("still busy after the request ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
