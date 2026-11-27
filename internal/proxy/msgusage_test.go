package proxy_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/msgusage"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
)

const msgBody = `{"id":"msg_01","model":"claude-opus-5-5","usage":{"input_tokens":2,"output_tokens":84,"cache_read_input_tokens":28263}}`

type msgCall struct {
	account string
	u       msgusage.Usage
}

type msgRec struct {
	mu    sync.Mutex
	calls []msgCall
}

func (m *msgRec) hook(account string, u msgusage.Usage, _ time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, msgCall{account, u})
}

func (m *msgRec) get() []msgCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]msgCall(nil), m.calls...)
}

func msgUpstream(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, msgBody)
	})
}

func postBody(t *testing.T, h *proxytest.Harness, url string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+homeTok)
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestOnMessageUsageSwapped(t *testing.T) {
	var m msgRec
	h := proxytest.Start(t, msgUpstream(200), proxytest.Options{Choose: servingSwap{}, OnMessageUsage: m.hook})
	code, body := postBody(t, h, "https://api.anthropic.com/v1/messages")
	if code != 200 || body != msgBody {
		t.Fatalf("client got %d %q", code, body)
	}
	got := m.get()
	want := msgusage.Usage{ID: "msg_01", Model: "claude-opus-5-5", In: 2, Out: 84, CR: 28263}
	if len(got) != 1 || got[0].account != "B" || got[0].u != want {
		t.Fatalf("got %+v", got)
	}
}

func TestOnMessageUsageDriftIsUnknown(t *testing.T) {
	var m msgRec
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+homeTok {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, msgBody)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: refusingRefreshChooser{}, OnMessageUsage: m.hook})
	code, body := postBody(t, h, "https://api.anthropic.com/v1/messages")
	if code != 200 || body != msgBody {
		t.Fatalf("client got %d %q", code, body)
	}
	got := m.get()
	if len(got) != 1 || got[0].account != "" || got[0].u.ID != "msg_01" {
		t.Fatalf("got %+v", got)
	}
}

func TestOnMessageUsageNot2xx(t *testing.T) {
	var m msgRec
	h := proxytest.Start(t, msgUpstream(429), proxytest.Options{Choose: servingSwap{}, OnMessageUsage: m.hook})
	postBody(t, h, "https://api.anthropic.com/v1/messages")
	if got := m.get(); len(got) != 0 {
		t.Fatalf("called on a 429: %+v", got)
	}
}

func TestOnMessageUsageOtherPath(t *testing.T) {
	var m msgRec
	h := proxytest.Start(t, msgUpstream(200), proxytest.Options{Choose: servingSwap{}, OnMessageUsage: m.hook})
	postBody(t, h, "https://api.anthropic.com/v1/messages/count_tokens")
	if got := m.get(); len(got) != 0 {
		t.Fatalf("called on count_tokens: %+v", got)
	}
}

func TestOnMessageUsagePanicAbsorbed(t *testing.T) {
	reported := make(chan error, 4)
	h := proxytest.Start(t, msgUpstream(200), proxytest.Options{
		Choose:         servingSwap{},
		OnMessageUsage: func(string, msgusage.Usage, time.Time) { panic("boom") },
		OnUsageError:   func(err error) { reported <- err },
	})
	code, body := postBody(t, h, "https://api.anthropic.com/v1/messages")
	if code != 200 || body != msgBody {
		t.Fatalf("client got %d %q", code, body)
	}
	select {
	case err := <-reported:
		if !strings.Contains(err.Error(), "OnMessageUsage") {
			t.Fatalf("error %v", err)
		}
	default:
		t.Fatal("panic not reported")
	}
}

// A client that asks for gzip still gets its usage recorded: the proxy drops
// Accept-Encoding so the tee reads plain JSON.
func TestOnMessageUsageWithAClientAcceptEncoding(t *testing.T) {
	var m msgRec
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			io.WriteString(zw, msgBody)
			zw.Close()
			return
		}
		io.WriteString(w, msgBody)
	})
	h := proxytest.Start(t, up, proxytest.Options{Choose: servingSwap{}, OnMessageUsage: m.hook})
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+homeTok)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := h.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	got := m.get()
	if len(got) != 1 || got[0].u.Out != 84 {
		t.Fatalf("got %+v", got)
	}
}
