package proxy_test

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxy/proxytest"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
)

const testSID = "0123456789abcdef0123456789abcdef"

// identChooser records the Identity each Choose call saw and swaps to "B".
type identChooser struct {
	mu   sync.Mutex
	ids  []proxy.Identity
	okay []bool
}

func (c *identChooser) Choose(ctx context.Context, _ router.Decision, _ string) (string, string, bool, bool) {
	id, ok := proxy.IdentityFrom(ctx)
	c.mu.Lock()
	c.ids, c.okay = append(c.ids, id), append(c.okay, ok)
	c.mu.Unlock()
	return "B", "sk-ant-oat01-SWAPPED", false, true
}
func (c *identChooser) Record(router.Kind, []string, string)           {}
func (c *identChooser) Refresh(context.Context, string) (string, bool) { return "", false }

func (c *identChooser) seen() ([]proxy.Identity, []bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]proxy.Identity(nil), c.ids...), append([]bool(nil), c.okay...)
}

func sessionURL(t *testing.T, s proxyauth.Secret, addr string) *url.URL {
	t.Helper()
	u, err := url.Parse(s.SessionProxyURL(addr, "default", testSID))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func basicOf(u *url.URL) string {
	pw, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
}

func sendAs(t *testing.T, c *http.Client, method, target string, hdr map[string]string) {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-HOME")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestIdentifiedTunnelGivesEveryRequestTheSameIdentity(t *testing.T) {
	ch := &identChooser{}
	h, s, _ := authHarness(t, http.NotFoundHandler(), ch)
	c := h.ClientVia(sessionURL(t, s, h.ProxyAddr))
	sendAs(t, c, "GET", "https://api.anthropic.com/v1/models", nil)
	sendAs(t, c, "GET", "https://api.anthropic.com/v1/models", map[string]string{"X-Claude-Code-Session-Id": "native-1"})
	ids, oks := ch.seen()
	if len(ids) != 2 || !oks[0] || !oks[1] {
		t.Fatalf("identities %+v ok %v", ids, oks)
	}
	want := proxyauth.Caller{Pool: "default", SID: testSID}
	if ids[0].Caller != want || ids[1].Caller != want {
		t.Fatalf("callers %+v", ids)
	}
	if ids[0].NativeID != "" || ids[1].NativeID != "native-1" {
		t.Errorf("native ids %q %q", ids[0].NativeID, ids[1].NativeID)
	}
	h.Records(t, "req", 2)
	if h.LogContains(t, "native-1") {
		t.Error("the native session id reached the trace")
	}
}

func TestInferenceIsPostV1MessagesOnTheAPIHostOnly(t *testing.T) {
	ch := &identChooser{}
	h, s, _ := authHarness(t, http.NotFoundHandler(), ch)
	c := h.ClientVia(sessionURL(t, s, h.ProxyAddr))
	cases := []struct {
		method, url string
		want        bool
	}{
		{"POST", "https://api.anthropic.com/v1/messages", true},
		{"POST", "https://api.anthropic.com/v1/messages?beta=true", true},
		{"GET", "https://api.anthropic.com/v1/messages", false},
		{"POST", "https://api.anthropic.com/v1/messages/count_tokens", false},
		{"POST", "https://api.anthropic.com/v1/messages/", false},
		{"POST", "https://api.anthropic.com/v1/models", false},
	}
	for _, tc := range cases {
		sendAs(t, c, tc.method, tc.url, nil)
	}
	ids, _ := ch.seen()
	if len(ids) != len(cases) {
		t.Fatalf("Choose saw %d requests, want %d", len(ids), len(cases))
	}
	for i, tc := range cases {
		if ids[i].Inference != tc.want {
			t.Errorf("%s %s: Inference %v, want %v", tc.method, tc.url, ids[i].Inference, tc.want)
		}
	}
}

func TestLegacyCallerHasNoIdentityAndIsRoutedAsBefore(t *testing.T) {
	ch := &identChooser{}
	h, _, _ := authHarness(t, http.NotFoundHandler(), ch)
	sendAs(t, h.Client, "POST", "https://api.anthropic.com/v1/messages", map[string]string{"X-Claude-Code-Session-Id": "native-1"})
	ids, oks := ch.seen()
	if len(ids) != 1 || oks[0] {
		t.Fatalf("legacy caller: identities %+v ok %v, want one call with ok=false", ids, oks)
	}
	for _, r := range h.Records(t, "req", 1) {
		if r.SID != "" || !r.Swapped || r.Account != "B" || r.Class != "serving" {
			t.Errorf("legacy record %+v: want swapped to B, serving, no sid", r)
		}
	}
}

func TestNoCallerAuthMeansNoIdentity(t *testing.T) {
	ch := &identChooser{}
	h := proxytest.Start(t, http.NotFoundHandler(), proxytest.Options{Choose: ch})
	sendAs(t, h.Client, "POST", "https://api.anthropic.com/v1/messages", nil)
	ids, oks := ch.seen()
	if len(ids) != 1 || oks[0] {
		t.Fatalf("auth off: identities %+v ok %v", ids, oks)
	}
}

func TestAbsoluteFormIdentifiedRequestIsTracedWithItsSID(t *testing.T) {
	ch := &identChooser{}
	h, s, _ := authHarness(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), ch)
	su := sessionURL(t, s, h.ProxyAddr)
	if code := rawAbsolute(t, h.ProxyAddr, su, "https://api.anthropic.com/v1/models"); code != 200 {
		t.Fatalf("status %d", code)
	}
	recs := h.Records(t, "req", 1)
	if recs[0].SID != testSID[:8] || recs[0].Form != "absolute" {
		t.Fatalf("absolute form record: %+v", recs[0])
	}
}

func TestIdentifiedTraceRecordsCarryOnlyTheShortSID(t *testing.T) {
	h, s, _ := authHarness(t, http.NotFoundHandler(), &identChooser{})
	su := sessionURL(t, s, h.ProxyAddr)
	pw, _ := su.User.Password()

	// A blind tunnel: its one record, written when it closes, has the sid.
	if code, _ := rawConnect(t, h.ProxyAddr, "echo.example.org:443", basicOf(su)); code != http.StatusOK {
		t.Fatalf("blind CONNECT status %d", code)
	}
	sendAs(t, h.ClientVia(su), "GET", "https://api.anthropic.com/v1/models", nil)

	var blind int
	for _, r := range h.Records(t, "tunnel", 1) {
		if r.Form == "blind" {
			blind++
			if r.SID != testSID[:8] {
				t.Errorf("blind record sid %q, want %q", r.SID, testSID[:8])
			}
		}
	}
	if blind != 1 {
		t.Errorf("blind records: %d", blind)
	}
	for _, r := range h.Records(t, "req", 1) {
		if r.SID != testSID[:8] {
			t.Errorf("req record sid %q", r.SID)
		}
	}
	if !h.LogContains(t, `"sid":"`+testSID[:8]+`"`) {
		t.Error("no sid in the log")
	}
	if h.LogContains(t, pw) || h.LogContains(t, testSID) || h.LogContains(t, su.User.Username()) {
		t.Error("the password or the full sid reached the trace")
	}
}

func TestMalformedSessionCredentialsGet407(t *testing.T) {
	h, s, raw := authHarness(t, http.NotFoundHandler(), &identChooser{})
	good := sessionURL(t, s, h.ProxyAddr)
	goodPW, _ := good.User.Password()
	b64 := func(x string) string { return base64.StdEncoding.EncodeToString([]byte(x)) }
	cases := map[string]string{
		"bad base64":      "Basic %%%",
		"no colon":        "Basic " + b64("chottag.default."+testSID),
		"extra dots":      "Basic " + b64("chottag.default."+testSID+".x:"+goodPW),
		"uppercase sid":   "Basic " + b64("chottag.default."+strings.ToUpper(testSID)+":"+goodPW),
		"31-char sid":     "Basic " + b64("chottag.default."+testSID[:31]+":"+goodPW),
		"33-char sid":     "Basic " + b64("chottag.default."+testSID+"0:"+goodPW),
		"empty pool":      "Basic " + b64("chottag.."+testSID+":"+goodPW),
		"17-char pool":    "Basic " + b64("chottag."+strings.Repeat("a", 17)+"."+testSID+":"+goodPW),
		"wrong password":  "Basic " + b64("chottag.default."+testSID+":"+raw),
		"another session": "Basic " + b64("chottag.default."+strings.Repeat("a", 32)+":"+goodPW),
	}
	for name, hdr := range cases {
		if code, _ := rawConnect(t, h.ProxyAddr, "api.anthropic.com:443", hdr); code != http.StatusProxyAuthRequired {
			t.Errorf("%s: status %d, want 407", name, code)
		}
	}
}

func TestNoCallerAuthAbsoluteFormHasNoSID(t *testing.T) {
	h := proxytest.Start(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), proxytest.Options{Choose: &identChooser{}})
	if code := rawAbsolute(t, h.ProxyAddr, nil, "https://api.anthropic.com/v1/models"); code != 200 {
		t.Fatalf("status %d", code)
	}
	for _, r := range h.Records(t, "req", 1) {
		if r.SID != "" {
			t.Errorf("auth off: record sid %q", r.SID)
		}
	}
}
