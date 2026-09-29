// Package bunsmoke checks that Bun — the runtime inside the native claude
// binary — sends HTTPS through HTTPS_PROXY and trusts NODE_EXTRA_CA_CERTS.
package bunsmoke

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

const script = `const r = await fetch("https://api.anthropic.com/v1/messages", {method: "POST", headers: {authorization: "Bearer sk-ant-oat01-HOME-SECRET"}, body: "{}"}); console.log(r.status, await r.text());`

// requireBunEnv set to 1 (CI sets it) makes a missing bun a failure
// instead of a skip, so CI cannot pass this package by skipping it. Left
// unset, a machine without Bun still skips, as before.
const requireBunEnv = "CHOTTAG_REQUIRE_BUN"

// bunVerdict decides what a test does: "run" when bun is on PATH, else
// "fail" when require is "1", else "skip".
func bunVerdict(found bool, require string) string {
	switch {
	case found:
		return "run"
	case require == "1":
		return "fail"
	default:
		return "skip"
	}
}

// needBun runs the test only where bun is on PATH (bunVerdict).
func needBun(t *testing.T) {
	t.Helper()
	_, err := exec.LookPath("bun")
	switch bunVerdict(err == nil, os.Getenv(requireBunEnv)) {
	case "fail":
		t.Fatalf("bun is not on PATH, and %s=1 requires it", requireBunEnv)
	case "skip":
		t.Skip("bun not installed")
	}
}

func TestBunVerdict(t *testing.T) {
	for _, c := range []struct {
		found   bool
		require string
		want    string
	}{
		{true, "", "run"},
		{true, "1", "run"},
		{false, "", "skip"},
		{false, "0", "skip"},
		{false, "1", "fail"},
	} {
		if got := bunVerdict(c.found, c.require); got != c.want {
			t.Errorf("bunVerdict(%t, %q) = %q, want %q", c.found, c.require, got, c.want)
		}
	}
}

// runBunDeadline bounds a bun child well under `go test`'s own default
// timeout (10m, see cmd/chottag's goTool), so a bun that wedges fails this
// test with its own output instead of being reaped anonymously once the
// package itself times out.
const runBunDeadline = 30 * time.Second

func runBun(t *testing.T, env ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runBunDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "-e", script)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("bun did not finish within the %s hang guard:\n%s", runBunDeadline, out)
	}
	return string(out), err
}

func TestBunHonoursProxyAndExtraCA(t *testing.T) {
	needBun(t)
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }), proxytest.Options{})
	proxyEnv := "HTTPS_PROXY=" + h.ProxyURL.String()

	out, err := runBun(t, proxyEnv, "NODE_EXTRA_CA_CERTS="+h.CAFile)
	if err != nil || !strings.Contains(out, "200 ok") {
		t.Fatalf("with CA: err %v out %q", err, out)
	}
	if r := h.Records(t, "req", 1)[0]; r.Form != "mitm" || r.Path != "/v1/messages" {
		t.Fatalf("record %+v", r)
	}

	if out, err := runBun(t, proxyEnv); err == nil && strings.Contains(out, "200 ok") {
		t.Fatalf("without NODE_EXTRA_CA_CERTS bun must reject the proxy CA; got %q", out)
	}
}

// F225 pinned: Bun sends Proxy-Authorization from HTTPS_PROXY's
// userinfo on CONNECT, and trusts a name-constrained CA (T2). A Bun
// change that stops either shows up here, not as a 407 in a live session.
func TestBunSendsProxyAuthorizationFromUserinfo(t *testing.T) {
	needBun(t)
	s, err := proxyauth.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := proxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }), proxytest.Options{ProxyAuth: s})
	out, err := runBun(t, "HTTPS_PROXY="+h.ProxyURL.String(), "NODE_EXTRA_CA_CERTS="+h.CAFile)
	if err != nil || !strings.Contains(out, "200 ok") {
		t.Fatalf("with userinfo: err %v out %q", err, out)
	}
	out, err = runBun(t, "HTTPS_PROXY="+h.PlainProxyURL.String(), "NODE_EXTRA_CA_CERTS="+h.CAFile)
	if err != nil {
		t.Fatalf("without userinfo: bun failed to run at all: %v out %q", err, out)
	}
	// fetch() surfaces the CONNECT's own 407 response as if it were the
	// answer to the request: the script's console.log(r.status, ...) print
	// is what actually reaches out, not a rejected promise.
	if !strings.HasPrefix(out, "407 ") {
		t.Fatalf("without userinfo the CONNECT must get 407; got %q", out)
	}
	recs := h.Records(t, "tunnel", 1)
	found := false
	for _, r := range recs {
		if r.Status == http.StatusProxyAuthRequired {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no tunnel record with Status 407 among %+v", recs)
	}
}
