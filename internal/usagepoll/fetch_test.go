package usagepoll

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
)

// stubTokens is a TokenSource that never reads a Keychain and never runs
// claude. token is what Token returns (ok false when empty); refreshed is
// what ForceRefresh returns (ok false when empty).
type stubTokens struct {
	mu        sync.Mutex
	token     string
	refreshed string
	dirs      []string
	refreshes int
	state     creds.TokenState // what Token reports as the slot's state
}

func (s *stubTokens) Token(_ context.Context, dir string) (string, creds.Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirs = append(s.dirs, dir)
	return s.token, creds.Status{State: s.state}, s.token != ""
}

func (s *stubTokens) ForceRefresh(_ context.Context, dir string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshes++
	return s.refreshed, s.refreshed != ""
}

// usageServer answers /api/oauth/usage on loopback. handle gets the bearer
// the request carried and writes the reply.
func usageServer(t *testing.T, handle func(w http.ResponseWriter, bearer string)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/oauth/usage" {
			t.Errorf("request = %s %s, want GET /api/oauth/usage", r.Method, r.URL.Path)
		}
		handle(w, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

var fetchNow = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func testFetcher(srv *httptest.Server, tokens TokenSource, scale float64) Fetcher {
	return newFetcher(FetchConfig{
		URL:    srv.URL + "/api/oauth/usage",
		Client: NewClient(nil),
		Tokens: tokens,
		Now:    func() time.Time { return fetchNow },
	}, scale)
}

func TestFetchSendsTheSlotBearerAndReadsTheBody(t *testing.T) {
	srv, hits := usageServer(t, func(w http.ResponseWriter, bearer string) {
		if bearer != "tok-A" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(capturedShape))
	})
	tokens := &stubTokens{token: "tok-A"}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if !out.OK || out.GaveUp || out.Status != "200" {
		t.Fatalf("outcome = %+v, want OK with status 200", out)
	}
	if !near(out.Result.FiveHour.Utilization, 0.42) || !near(out.Result.SevenDay.Utilization, 0.075) {
		t.Fatalf("result = %+v, want 0.42 / 0.075", out.Result)
	}
	if !out.Result.At.Equal(fetchNow) {
		t.Fatalf("Result.At = %v, want the fetch clock's %v", out.Result.At, fetchNow)
	}
	if hits.Load() != 1 || len(tokens.dirs) != 1 || tokens.dirs[0] != "/slots/A" || tokens.refreshes != 0 {
		t.Fatalf("hits = %d, token dirs = %v, refreshes = %d; want 1 request for /slots/A and no refresh", hits.Load(), tokens.dirs, tokens.refreshes)
	}
}

func TestFetchRefreshesOnceOn401ThenRetries(t *testing.T) {
	srv, hits := usageServer(t, func(w http.ResponseWriter, bearer string) {
		if bearer != "tok-new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(capturedShape))
	})
	tokens := &stubTokens{token: "tok-old", refreshed: "tok-new"}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if !out.OK || out.Status != "401>200" {
		t.Fatalf("outcome = %+v, want OK with status 401>200", out)
	}
	if hits.Load() != 2 || tokens.refreshes != 1 {
		t.Fatalf("hits = %d, refreshes = %d; want 2 and 1", hits.Load(), tokens.refreshes)
	}
}

func TestFetchGivesUpAfterASecond401(t *testing.T) {
	srv, hits := usageServer(t, func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusUnauthorized) })
	tokens := &stubTokens{token: "tok-old", refreshed: "tok-new"}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if out.OK || !out.GaveUp || out.Status != "401>401" {
		t.Fatalf("outcome = %+v, want GaveUp with status 401>401", out)
	}
	if hits.Load() != 2 || tokens.refreshes != 1 {
		t.Fatalf("hits = %d, refreshes = %d; want exactly one retry and one refresh", hits.Load(), tokens.refreshes)
	}
}

func TestFetchRefreshSucceedsButBodyIsUnparsable(t *testing.T) {
	srv, hits := usageServer(t, func(w http.ResponseWriter, bearer string) {
		if bearer != "tok-new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`<html>ok</html>`))
	})
	tokens := &stubTokens{token: "tok-old", refreshed: "tok-new"}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if out.OK || out.GaveUp || out.Status != "401>200 unparsed" {
		t.Fatalf("outcome = %+v, want a non-GaveUp failure with status 401>200 unparsed: auth worked", out)
	}
	if hits.Load() != 2 || tokens.refreshes != 1 {
		t.Fatalf("hits = %d, refreshes = %d; want 2 and 1", hits.Load(), tokens.refreshes)
	}
}

func TestFetchGivesUpWhenTheRefreshFails(t *testing.T) {
	srv, hits := usageServer(t, func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusUnauthorized) })
	tokens := &stubTokens{token: "tok-old"}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if out.OK || !out.GaveUp || hits.Load() != 1 || out.Status != "401 refresh-failed" {
		t.Fatalf("outcome = %+v, hits = %d; want GaveUp after one request with status \"401 refresh-failed\"", out, hits.Load())
	}
}

// TestFetchTimesOutOnASlowServer uses a client with a short Timeout (not
// the production 10s) so the test is fast and deterministic: the server
// never answers, so the client's own deadline fires the request's context.
func TestFetchTimesOutOnASlowServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	f := newFetcher(FetchConfig{
		URL:    srv.URL + "/api/oauth/usage",
		Client: &http.Client{Timeout: 50 * time.Millisecond, Transport: NewClient(nil).Transport},
		Tokens: &stubTokens{token: "tok-A"},
	}, 100)
	out := f(context.Background(), "/slots/A")
	if out.OK || out.GaveUp || out.NoToken || out.Status != "timeout" {
		t.Fatalf("outcome = %+v, want a plain \"timeout\" failure with NoToken false", out)
	}
}

func TestFetchWithoutATokenSendsNothing(t *testing.T) {
	srv, hits := usageServer(t, func(w http.ResponseWriter, _ string) { w.Write([]byte(capturedShape)) })
	out := testFetcher(srv, &stubTokens{}, 100)(context.Background(), "/slots/A")
	if out.OK || out.GaveUp || !out.NoToken || out.Status != "no-token" || hits.Load() != 0 {
		t.Fatalf("outcome = %+v, hits = %d; want no-token (NoToken true) and no request", out, hits.Load())
	}
}

func TestFetchFailuresAreNotResults(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.Write([]byte(capturedShape))
	}))
	defer target.Close()
	for _, tc := range []struct {
		name, status string
		handle       func(w http.ResponseWriter)
	}{
		{"429", "429", func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }},
		{"500", "500", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }},
		{"503", "503", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"redirect", "302", func(w http.ResponseWriter) {
			w.Header().Set("Location", target.URL+"/api/oauth/usage")
			w.WriteHeader(http.StatusFound)
		}},
		{"unparsable 200", "200 unparsed", func(w http.ResponseWriter) { w.Write([]byte(`<html>ok</html>`)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := usageServer(t, func(w http.ResponseWriter, _ string) { tc.handle(w) })
			out := testFetcher(srv, &stubTokens{token: "tok-A"}, 100)(context.Background(), "/slots/A")
			if out.OK || out.GaveUp || out.Status != tc.status {
				t.Fatalf("outcome = %+v, want a failed poll with status %q", out, tc.status)
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("the poll followed a redirect, carrying the bearer to another URL")
	}
}

// TestFetchStopsWhenTheDaemonStops: a cancelled context drops the
// in-flight request. The upstream is released by defer, not t.Cleanup
// (F154): httptest.Server.Close blocks on a held handler. Defers run LIFO,
// so close(release) is declared after srv.Close() and so runs first: a
// fetch that failed to cancel would then fail fast instead of hanging in
// Close waiting on a handler that never releases.
func TestFetchStopsWhenTheDaemonStops(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Outcome, 1)
	go func() { done <- testFetcher(srv, &stubTokens{token: "tok-A"}, 100)(ctx, "/slots/A") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never arrived")
	}
	cancel()
	select {
	case out := <-done:
		if out.OK || out.GaveUp {
			t.Fatalf("outcome = %+v, want a plain failure", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fetch did not return after its context was cancelled")
	}
}

func TestFetchFailsClosedWhileUnpinned(t *testing.T) {
	srv, _ := usageServer(t, func(w http.ResponseWriter, _ string) { w.Write([]byte(capturedShape)) })
	out := testFetcher(srv, &stubTokens{token: "tok-A"}, 0)(context.Background(), "/slots/A")
	if out.OK || out.GaveUp {
		t.Fatalf("outcome = %+v, want a failed poll while the scale is unpinned", out)
	}
	if out.Status != "200 unpinned" {
		t.Fatalf("status = %q, want %q", out.Status, "200 unpinned")
	}
}

// TestFetchBodyIsCappedAtMaxBody pads an otherwise-valid captured body past
// maxBody with a trailing field. A reader that ignored the cap would parse
// it fine; the fetch must read only the first maxBody bytes, truncating
// the JSON mid-field and turning it into a failed, unparsable poll.
func TestFetchBodyIsCappedAtMaxBody(t *testing.T) {
	padding := strings.Repeat("a", 2*maxBody)
	body := strings.TrimSuffix(capturedShape, "}") + `, "padding": "` + padding + `"}`
	if len(body) <= maxBody {
		t.Fatalf("body = %d bytes, want more than maxBody = %d", len(body), maxBody)
	}
	srv, _ := usageServer(t, func(w http.ResponseWriter, _ string) { w.Write([]byte(body)) })
	out := testFetcher(srv, &stubTokens{token: "tok-A"}, 100)(context.Background(), "/slots/A")
	if out.OK || out.Status != "200 unparsed" {
		t.Fatalf("outcome.OK = %v, Status = %q; want a failed poll with status \"200 unparsed\"", out.OK, out.Status)
	}
}

// TestFetchConnectionRefusedStatusIsAGenericError: errStatus must classify
// a transport error, not return err.Error() verbatim. A *url.Error's
// message repeats the request URL (host:port included), which the log
// must never carry.
func TestFetchConnectionRefusedStatusIsAGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := srv.URL + "/api/oauth/usage"
	host := srv.Listener.Addr().String()
	srv.Close() // closed before the request: the dial itself is refused
	f := newFetcher(FetchConfig{
		URL:    endpoint,
		Client: NewClient(nil),
		Tokens: &stubTokens{token: "tok-A"},
	}, 100)
	out := f(context.Background(), "/slots/A")
	if out.OK || out.GaveUp || out.NoToken {
		t.Fatalf("outcome = %+v, want a plain failure", out)
	}
	if out.Status != "error" {
		t.Fatalf("status = %q, want exactly %q", out.Status, "error")
	}
	if strings.Contains(out.Status, host) {
		t.Fatalf("status %q carries the connection's host:port", out.Status)
	}
}

// TestFetchGaveUpAfterATransportErrorOnTheRetry: a 401 followed by a
// successful ForceRefresh, then a retry that fails at the transport level
// (the server hijacks and closes the connection) instead of with another
// HTTP status. That must still give up (detail 9), not be treated as a
// fresh, unretried failure.
func TestFetchGaveUpAfterATransportErrorOnTheRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		conn.Close()
	}))
	defer srv.Close()
	tokens := &stubTokens{token: "tok-old", refreshed: "tok-new"}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if !out.GaveUp {
		t.Fatalf("outcome = %+v, want GaveUp true after a transport error on the retry", out)
	}
	if out.Status != "401>error" && out.Status != "401>timeout" {
		t.Fatalf("status = %q, want 401>error or 401>timeout", out.Status)
	}
	if hits.Load() != 2 || tokens.refreshes != 1 {
		t.Fatalf("hits = %d, refreshes = %d; want exactly one retry and one refresh", hits.Load(), tokens.refreshes)
	}
}

func TestFetchStatusNeverCarriesTheTokenOrBody(t *testing.T) {
	for _, handle := range []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.Write([]byte(capturedShape)) },
		func(w http.ResponseWriter) { w.WriteHeader(500); w.Write([]byte(`{"error":"tok-SECRET body"}`)) },
		func(w http.ResponseWriter) { w.Write([]byte(`tok-SECRET not json`)) },
	} {
		srv, _ := usageServer(t, func(w http.ResponseWriter, _ string) { handle(w) })
		for _, scale := range []float64{0, 100} {
			out := testFetcher(srv, &stubTokens{token: "tok-SECRET"}, scale)(context.Background(), "/slots/A")
			if strings.Contains(out.Status, "SECRET") || strings.Contains(out.Status, "body") || strings.Contains(out.Status, "{") {
				t.Fatalf("status %q carries the token or body text", out.Status)
			}
		}
	}
}

func TestNewClientNeverReadsTheEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	c := NewClient(nil)
	if c.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %v, want 10s", c.Timeout)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("NewClient(nil) set a Proxy func; it must dial direct and never read HTTPS_PROXY")
	}
	if !tr.DisableKeepAlives {
		t.Fatal("keep-alives are on: a pooled connection can outlive a sleep")
	}
	up, _ := url.Parse("http://127.0.0.1:3128")
	tr = NewClient(up).Transport.(*http.Transport)
	req, _ := http.NewRequest(http.MethodGet, DefaultURL, nil)
	if got, err := tr.Proxy(req); err != nil || got.String() != up.String() {
		t.Fatalf("Proxy(req) = %v, %v; want %v", got, err, up)
	}
}

// TestFetchGoesThroughTheUpstreamProxy drives a real request through a
// loopback stand-in for the user's upstream proxy. The target host is
// .invalid, so a request that skipped the proxy could not resolve at all.
func TestFetchGoesThroughTheUpstreamProxy(t *testing.T) {
	var seen atomic.Value
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.String() + " " + r.Header.Get("Authorization"))
		w.Write([]byte(capturedShape))
	}))
	defer proxySrv.Close()
	up, _ := url.Parse(proxySrv.URL)
	f := newFetcher(FetchConfig{
		URL:    "http://usage.invalid/api/oauth/usage",
		Client: NewClient(up),
		Tokens: &stubTokens{token: "tok-A"},
	}, 100)
	out := f(context.Background(), "/slots/A")
	if !out.OK {
		t.Fatalf("outcome = %+v, want OK through the upstream proxy", out)
	}
	if got, _ := seen.Load().(string); got != "http://usage.invalid/api/oauth/usage Bearer tok-A" {
		t.Fatalf("upstream proxy saw %q", got)
	}
}

func TestNewFetcherRefusesAnIncompleteConfig(t *testing.T) {
	for name, c := range map[string]FetchConfig{
		"no URL":    {Client: http.DefaultClient, Tokens: &stubTokens{}},
		"no client": {URL: DefaultURL, Tokens: &stubTokens{}},
		"no tokens": {URL: DefaultURL, Client: http.DefaultClient},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: NewFetcher did not panic", name)
				}
			}()
			NewFetcher(c)
		}()
	}
}

func TestDefaultURLIsTheCapturedEndpoint(t *testing.T) {
	if DefaultURL != "https://api.anthropic.com/api/oauth/usage" {
		t.Fatalf("DefaultURL = %q", DefaultURL)
	}
}

func TestFetchReportsNeedsLoginFromTheCredsStatus(t *testing.T) {
	srv, hits := usageServer(t, func(http.ResponseWriter, string) { t.Error("a request went out for a slot with no login") })
	tokens := &stubTokens{state: creds.StateNeedsLogin}
	out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A")
	if out.OK || !out.NoToken || !out.NeedsLogin || hits.Load() != 0 {
		t.Fatalf("outcome = %+v, hits = %d; want no-token with NeedsLogin and no request", out, hits.Load())
	}
	// A merely stale slot is not needs-login.
	tokens.state = creds.StateStale
	if out := testFetcher(srv, tokens, 100)(context.Background(), "/slots/A"); !out.NoToken || out.NeedsLogin {
		t.Fatalf("outcome = %+v, want no-token without NeedsLogin for a stale slot", out)
	}
}

func TestFetchCarriesARetryAfterOnA429(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{{"120", 2 * time.Minute}, {"", 0}, {"soon", 0}, {"-5", 0}, {"999999999", maxRetryAfter}} {
		srv, _ := usageServer(t, func(w http.ResponseWriter, _ string) {
			if tc.header != "" {
				w.Header().Set("Retry-After", tc.header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		})
		out := testFetcher(srv, &stubTokens{token: "t"}, 100)(context.Background(), "/slots/A")
		if out.OK || out.Status != "429" || out.RetryAfter != tc.want {
			t.Fatalf("Retry-After %q: outcome = %+v, want status 429 and RetryAfter %v", tc.header, out, tc.want)
		}
	}
}
