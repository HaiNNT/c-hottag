package proxy

import "testing"

func TestSameTunnelHostTable(t *testing.T) {
	cases := []struct {
		host, connect string
		want          bool
	}{
		{"api.anthropic.com", "api.anthropic.com:443", true},
		{"api.anthropic.com:443", "api.anthropic.com:443", true},
		{"API.Anthropic.com", "api.anthropic.com:443", true},
		{"api.anthropic.com:8443", "api.anthropic.com:8443", true},
		{"api.anthropic.com", "api.anthropic.com:8443", false},
		{"api.anthropic.com:8443", "api.anthropic.com:443", false},
		{"evil.example", "api.anthropic.com:443", false},
		{"", "api.anthropic.com:443", false},
		{"api.anthropic.com.", "api.anthropic.com:443", false},
		{"[::1]", "[::1]:443", true},
		{"[::1]:443", "[::1]:443", true},
		{"api.anthropic.com:", "api.anthropic.com:443", false},
		{"api.anthropic.com", "api.anthropic.com", false}, // a CONNECT target without a port is malformed
	}
	for _, c := range cases {
		if got := sameTunnelHost(c.host, c.connect); got != c.want {
			t.Errorf("sameTunnelHost(%q, %q) = %v, want %v", c.host, c.connect, got, c.want)
		}
	}
}
