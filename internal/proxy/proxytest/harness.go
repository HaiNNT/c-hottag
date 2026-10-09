// Package proxytest runs a chottag proxy in front of a fake Anthropic upstream.
package proxytest

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/ca"
	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

type Options struct {
	Choose proxy.Chooser
	// Shapes is `trace run --shapes`: every request traced, into the one
	// log. Ignored when Tracing is set.
	Shapes bool
	// Tracing, when set, is Config.Tracing as-is (M2c).
	Tracing func() bool
	// TraceLog is Config.TraceLog (M2c): the daemon's second log. nil is
	// `trace run`'s single log.
	TraceLog *tracelog.Writer
	// OnClaudeVersion is Config.OnClaudeVersion (M2c).
	OnClaudeVersion  func(version string)
	LimitFingerprint bool
	DownHosts        []string
	OnLogError       func(error)
	OnUsage          func(account string, status int, h http.Header)
	OnUsageError     func(error)
	// OnServingRefusal is Config.OnServingRefusal (R158).
	OnServingRefusal func(account string, status int, resent bool, method, path string)
	// OnUnknownOwner, OnOwnerFound and Now are Config's (M12/R168).
	OnUnknownOwner func(account, kind, idHash string, status int)
	OnOwnerFound   func(kind, idHash, found, tried string, status int)
	Now            func() time.Time
	// WallRetry is Config.WallRetry (M4 §4a).
	WallRetry     func(ctx context.Context, account string, h http.Header) (bool, func(string, int))
	UpstreamProxy *url.URL
	Version       string
	// ProxyAuth is Config.ProxyAuth (part 1 T6): zero means no caller auth,
	// as before this task.
	ProxyAuth proxyauth.Secret

	// NoTrace starts the proxy with trace logging off (Config.Log nil), as
	// `proxy run` would with no trace log. The harness still creates
	// LogPath, empty, so a test can assert nothing was written to it.
	NoTrace bool
}

type Harness struct {
	ProxyURL  *url.URL
	ProxyAddr string
	CAFile    string
	LogPath   string
	Client    *http.Client
	Server    *proxy.Server

	// PlainProxyURL is the proxy's URL with no userinfo, whatever
	// Options.ProxyAuth was: a caller that wants the unauthenticated shape
	// (e.g. to check it gets 407) starts from this, never from stripping
	// ProxyURL's userinfo itself.
	PlainProxyURL *url.URL

	// UpstreamAddr is the real listen address of the fake Anthropic mock
	// standing in for api.anthropic.com. Normally invisible to a test: the
	// harness's own DialContext substitutes it in by hostname. A test that
	// chains chottag through an upstream-proxy test double needs it
	// explicitly, because once a real CONNECT tunnel is in play that
	// substitution no longer has a hostname to key off — the tunnel's far
	// end must be redirected here at the TCP level instead.
	UpstreamAddr string

	logWriter *tracelog.Writer
	proxyCA   *ca.Authority
}

func Start(t *testing.T, upstream http.Handler, opts Options) *Harness {
	t.Helper()
	dir := t.TempDir()

	upCA, err := ca.LoadOrCreate(filepath.Join(dir, "upstream-ca"))
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewUnstartedServer(upstream)
	up.TLS = anySNI(upCA)
	// Quiet: TestSwapRequiresHTTPS deliberately sends plaintext to this
	// TLS-only listener, which would otherwise log a handshake error line.
	up.Config.ErrorLog = log.New(io.Discard, "", 0)
	up.StartTLS()
	t.Cleanup(up.Close)
	upAddr := up.Listener.Addr().String()

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	proxyCA, err := ca.LoadOrCreate(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(dir, "ca", "ca.pem")
	logPath := filepath.Join(dir, "trace.jsonl")
	lw, err := tracelog.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lw.Close() })

	var cfgLog *tracelog.Writer
	if !opts.NoTrace {
		cfgLog = lw
	}

	down := map[string]bool{}
	for _, h := range opts.DownHosts {
		down[h] = true
	}
	tracing := opts.Tracing
	if tracing == nil && opts.Shapes {
		tracing = func() bool { return true }
	}
	srv := proxy.New(proxy.Config{
		CA:               proxyCA,
		Intercept:        proxy.SuffixMatcher(proxy.DefaultTraceSuffixes),
		Log:              cfgLog,
		Tracing:          tracing,
		TraceLog:         opts.TraceLog,
		OnClaudeVersion:  opts.OnClaudeVersion,
		LimitFingerprint: opts.LimitFingerprint,
		Choose:           opts.Choose,
		OnLogError:       opts.OnLogError,
		OnUsage:          opts.OnUsage,
		OnUsageError:     opts.OnUsageError,
		WallRetry:        opts.WallRetry,
		OnServingRefusal: opts.OnServingRefusal,
		OnUnknownOwner:   opts.OnUnknownOwner,
		OnOwnerFound:     opts.OnOwnerFound,
		Now:              opts.Now,
		UpstreamProxy:    opts.UpstreamProxy,
		Version:          opts.Version,
		ProxyAuth:        opts.ProxyAuth,
		UpstreamRootCAs:  upCA.Pool(),
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host := router.HostOnly(addr)
			switch {
			case down[host]:
				return nil, errors.New("proxytest: host down")
			case host == "echo.example.org":
				return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
			case opts.UpstreamProxy != nil && addr == proxyDialAddr(opts.UpstreamProxy):
				// The dial target really is the (real, locally-listening) test
				// upstream proxy itself, not a stand-in for the fake Anthropic
				// upstream — chase it for real. Compared against the same
				// port-defaulting proxy.proxyHostPort applies (not against
				// opts.UpstreamProxy.Host verbatim), so a portless proxy URL
				// still matches what dialThroughProxy actually dials, instead
				// of silently falling through to the fake-upstream default.
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			default:
				return (&net.Dialer{}).DialContext(ctx, network, upAddr)
			}
		},
	})
	ps := httptest.NewServer(srv)
	t.Cleanup(ps.Close)
	pu, _ := url.Parse(ps.URL)

	authedURL := pu
	if !opts.ProxyAuth.IsZero() {
		u, err := url.Parse(opts.ProxyAuth.ProxyURL(pu.Host))
		if err != nil {
			t.Fatal(err)
		}
		authedURL = u
	}

	h := &Harness{
		ProxyURL:      authedURL,
		PlainProxyURL: pu,
		ProxyAddr:     pu.Host,
		UpstreamAddr:  upAddr,
		CAFile:        caFile,
		LogPath:       logPath,
		Server:        srv,
		logWriter:     lw,
		proxyCA:       proxyCA,
	}
	h.Client = h.ClientVia(authedURL)
	return h
}

// ClientVia is like h.Client, but through the given proxy URL: a test that
// wants an unauthenticated client (h.PlainProxyURL) or one with a wrong
// credential builds it here instead of reaching into h.Client's transport.
func (h *Harness) ClientVia(proxyURL *url.URL) *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: tlsTrusting(h.proxyCA)},
	}
}

// CloseLog closes the underlying trace writer, so that any subsequent
// Log.Write call inside the proxy begins failing. Used to test
// Config.OnLogError.
func (h *Harness) CloseLog() {
	h.logWriter.Close()
}

// recordsHangGuard bounds Records' wait. It is a hang guard, not a timing
// assertion (F136's pattern, F155): records arrive when the proxy's
// finaliser runs, and under full-suite -race load that can take well past
// the fixed 2s this used to allow. A record that never comes still fails,
// just not before a whole minute has passed. A variable only so the
// harness's own test can shorten it.
var recordsHangGuard = 60 * time.Second

// Records polls the log until it holds at least n records of kind, and
// returns them. It fails the test if the log can't be read, or if the
// records haven't arrived by recordsHangGuard.
func (h *Harness) Records(t *testing.T, kind string, n int) []tracelog.Record {
	t.Helper()
	return h.RecordsIn(t, h.LogPath, kind, n)
}

// RecordsIn is Records for another log: the trace-mode log a test handed
// the proxy through Options.TraceLog (M2c).
func (h *Harness) RecordsIn(t *testing.T, path, kind string, n int) []tracelog.Record {
	t.Helper()
	start := time.Now()
	deadline := start.Add(recordsHangGuard)
	for {
		all, err := tracelog.ReadAll(path)
		if err != nil {
			// Without this, an unreadable log presents to every caller as
			// "got 0 records" — a misleading failure in infrastructure the
			// whole suite leans on.
			t.Fatalf("proxytest: reading %s: %v", path, err)
		}
		var out []tracelog.Record
		for _, r := range all {
			if r.Kind == kind {
				out = append(out, r)
			}
		}
		if len(out) >= n || time.Now().After(deadline) {
			if len(out) < n {
				t.Fatalf("want %d %q records, got %d after %s (%s): %+v", n, kind, len(out), time.Since(start).Round(time.Millisecond), path, all)
			}
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *Harness) LogContains(t *testing.T, s string) bool {
	t.Helper()
	b, err := os.ReadFile(h.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), s)
}

func tlsTrusting(a *ca.Authority) *tls.Config {
	return &tls.Config{RootCAs: a.Pool(), MinVersion: tls.VersionTLS12}
}

// anySNI is the fake upstream's TLS config: a leaf for whatever SNI the
// proxy dials with. There is no empty-SNI fallback: httptest.StartTLS fills
// in Config.Certificates when it's empty, and crypto/tls only ever calls
// GetCertificate for an empty ClientHello.ServerName when Certificates is
// itself empty, so that case can't be reached here. Test-only by
// construction: production presents leaves only through the strict
// ca.ServerTLSConfig.
func anySNI(a *ca.Authority) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return a.Leaf(h.ServerName)
	}}
}

// proxyDialAddr mirrors proxy.proxyHostPort's port defaulting (unexported,
// so duplicated here) — the address the harness's own DialContext stub
// must recognise as "the upstream proxy itself" is the same one
// dialThroughProxy actually dials, not the proxy URL's Host verbatim.
func proxyDialAddr(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}
