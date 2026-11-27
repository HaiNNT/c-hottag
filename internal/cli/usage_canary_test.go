package cli

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// TestUsageLedgerNeverHoldsResponseText is R203's privacy canary (D1, R-h):
// a proxied streaming /v1/messages reply carrying a distinctive text, for a
// request carrying a distinctive prompt, must leave neither string in any
// file under CHOTTAG_HOME, while the reply's usage numbers do land in the
// ledger.
func TestUsageLedgerNeverHoldsResponseText(t *testing.T) {
	const replyCanary, promptCanary = "REPLY-CANARY-7f3a91", "PROMPT-CANARY-c04b52"
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", t.TempDir())
	addSlotAccount(t, home, "A")

	up, upCA := startFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_canary\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5-5\",\"content\":[],\"usage\":{\"input_tokens\":7,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":4242,\"output_tokens\":1}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\""+replyCanary+"\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":321}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	wireFakeUpstream(t, up, upCA)

	errb := newSyncBuf()
	sig := make(chan os.Signal, 2)
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- runProxyWithSignal([]string{"run", "--listen", "127.0.0.1:0", "--log", ""}, io.Discard, errb, nil, sig)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		sig <- os.Interrupt
		select {
		case <-codeCh:
		case <-time.After(shutdownGrace + 5*time.Second):
			t.Error("daemon did not shut down")
		}
	}
	t.Cleanup(stop)
	select {
	case <-errb.done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy run to start listening")
	}
	addr := listenAddrFromStderr(t, errb.String())

	tlsConn, br := mitmConn(t, home, addr)
	body := `{"model":"claude-opus-5-5","stream":true,"messages":[{"role":"user","content":"` + promptCanary + `"}]}`
	req := "POST /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nAuthorization: Bearer sk-ant-oat01-client-owned\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := io.WriteString(tlsConn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the proxied response: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, read %v", resp.StatusCode, err)
	}
	if !strings.Contains(string(got), replyCanary) {
		t.Fatalf("the client did not receive the reply text: %q", got)
	}

	// The usage callback runs as the stream ends, which can be just after the
	// client has read it all: wait for the event to reach the ledger (the
	// bound is generous; the loop ends the moment it lands), then stop.
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if evs, err := usagelog.Open(home).Events(time.Time{}, time.Time{}); err == nil && len(evs) > 0 {
			break
		}
	}
	stop() // drains the recorder's queue

	evs, err := usagelog.Open(home).Events(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].MID != "msg_canary" || evs[0].Model != "claude-opus-5-5" ||
		evs[0].In != 7 || evs[0].Out != 321 || evs[0].CR != 4242 || evs[0].Src != "live" || evs[0].Acct != "A" {
		t.Fatalf("ledger events = %+v, want the one canary message's usage under A", evs)
	}

	err = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, c := range []string{replyCanary, promptCanary} {
			if strings.Contains(string(b), c) {
				t.Errorf("%s contains %q", p, c)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errb.String(), replyCanary) || strings.Contains(errb.String(), promptCanary) {
		t.Error("the daemon log (stderr) holds a canary")
	}
}
