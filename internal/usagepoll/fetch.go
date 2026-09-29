package usagepoll

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Timeout bounds one poll request, from dial to the last body byte (spec
// §6.4).
const Timeout = 10 * time.Second

// NewClient builds the poll's own client. upstream is the daemon's
// already-resolved --upstream-proxy (parseUpstreamProxy's result, which
// CHOTTAG_UPSTREAM_PROXY feeds): nil dials direct.
//
// Proxy is never http.ProxyFromEnvironment. The daemon's environment can
// hold an HTTPS_PROXY that points at the daemon's own listener, and a poll
// must never go through chottag itself (spec §6.4).
//
// Keep-alives are off: polls are minutes to hours apart, and a pooled
// connection that outlived a sleep would fail the first poll after wake.
// Redirects are not followed: a 3xx is a failed poll, and the bearer never
// goes anywhere but the configured URL.
func NewClient(upstream *url.URL) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: Timeout,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   true,
	}
	if upstream != nil {
		tr.Proxy = http.ProxyURL(upstream)
	}
	return &http.Client{
		Timeout:       Timeout,
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// FetchConfig configures NewFetcher. URL, Client and Tokens are required.
type FetchConfig struct {
	URL    string
	Client *http.Client
	Tokens TokenSource
	// Now stamps Result.At. Default: the wall clock.
	Now func() time.Time
}

// NewFetcher returns the production poll: a Bearer GET with the slot's own
// token, one ForceRefresh and one retry on a 401, and parse at
// UtilizationScale.
func NewFetcher(c FetchConfig) Fetcher { return newFetcher(c, UtilizationScale) }

func newFetcher(c FetchConfig, scale float64) Fetcher {
	if c.URL == "" || c.Client == nil || c.Tokens == nil {
		panic("usagepoll: FetchConfig needs URL, Client and Tokens")
	}
	now := c.Now
	if now == nil {
		now = wallNow
	}
	return func(ctx context.Context, dir string) Outcome {
		// Token refreshes through the product's own path when the token is
		// stale (in the background: this call then reports !ok, and the
		// poll is skipped). A slot that needs login never gets a poll.
		tok, _, ok := c.Tokens.Token(ctx, dir)
		if !ok {
			return Outcome{Status: "no-token", NoToken: true}
		}
		code, body, err := get(ctx, c.Client, c.URL, tok)
		retried := false
		if err == nil && code == http.StatusUnauthorized {
			if tok, ok = c.Tokens.ForceRefresh(ctx, dir); !ok {
				return Outcome{Status: "401 refresh-failed", GaveUp: true}
			}
			retried = true
			code, body, err = get(ctx, c.Client, c.URL, tok)
		}
		prefix := ""
		if retried {
			prefix = "401>"
		}
		if err != nil {
			return Outcome{Status: prefix + errStatus(err), GaveUp: retried}
		}
		if code != http.StatusOK {
			return Outcome{Status: prefix + strconv.Itoa(code), GaveUp: retried}
		}
		res, err := parse(body, scale)
		if err != nil {
			st := prefix + "200 unparsed"
			if errors.Is(err, errUnpinned) {
				st = prefix + "200 unpinned"
			}
			// An unparsable or unpinned body after a successful refresh is
			// not a giving-up: auth worked, so a reset poll keeps its
			// backoff instead of waiting for the next event.
			return Outcome{Status: st}
		}
		res.At = now()
		return Outcome{OK: true, Result: res, Status: prefix + "200"}
	}
}

// get sends one poll request and reads at most maxBody of the answer.
func get(ctx context.Context, c *http.Client, endpoint, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// errStatus names a transport failure for the log without its text: a
// *url.Error's message repeats the URL, and nothing here needs more than
// the class.
func errStatus(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "error"
}
