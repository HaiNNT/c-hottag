package proxy

import (
	"net/url"
	"testing"
)

// TestProxyHostPortDefaultsThePort exists because every other test in this
// package builds its UpstreamProxy URL from a real net.Listen address, which
// always carries an explicit port — the port-defaulting branches below have
// otherwise never run. (Binding the privileged ports 80/443 to prove this
// end-to-end via a real dial isn't available in a sandboxed test run; see
// proxytest.proxyDialAddr's matching unit test, which must keep agreeing
// with this one on every case here.)
func TestProxyHostPortDefaultsThePort(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"http, no port, defaults to 80", "http://proxy.example.org", "proxy.example.org:80"},
		{"https, no port, defaults to 443", "https://proxy.example.org", "proxy.example.org:443"},
		{"http, explicit port, kept as-is", "http://proxy.example.org:8080", "proxy.example.org:8080"},
		{"https, explicit port, kept as-is", "https://proxy.example.org:8443", "proxy.example.org:8443"},
		{"IP host, no port, defaults to 80", "http://127.0.0.1", "127.0.0.1:80"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := proxyHostPort(u); got != c.want {
				t.Errorf("proxyHostPort(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}
