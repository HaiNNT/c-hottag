package proxy_test

import (
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
)

func TestClaudeCLIVersion(t *testing.T) {
	// "claude-cli/2.1.282 (" is 20 bytes. Padding it with 236 x's makes
	// 256 bytes, the longest value parsed (maxUserAgentLen); one more x is
	// 257, one byte over, and is refused whatever it says.
	at256 := "claude-cli/2.1.282 (" + strings.Repeat("x", 256-20)
	cases := []struct {
		name, ua, want string
		ok             bool
	}{
		{"real", "claude-cli/2.1.282 (external, cli)", "2.1.282", true},
		{"paren right after", "claude-cli/2.1.282(external)", "2.1.282", true},
		{"wide parts", "claude-cli/10.20.300 (x)", "10.20.300", true},
		{"256 bytes", at256, "2.1.282", true},
		{"257 bytes", at256 + "x", "", false},
		{"10 KB", "claude-cli/2.1.282 (" + strings.Repeat("x", 10<<10) + ")", "", false},
		{"two parts", "claude-cli/2.1", "", false},
		{"no separator", "claude-cli/2.1.282", "", false},
		{"suffix", "claude-cli/2.1.282-beta (x)", "", false},
		{"foreign product", "evil/2.1.282 (external, cli)", "", false},
		{"leading space", " claude-cli/2.1.282 (x)", "", false},
		{"case", "Claude-CLI/2.1.282 (x)", "", false},
		{"prefix", "xclaude-cli/2.1.282 (x)", "", false},
		{"7-digit part", "claude-cli/1234567.1.1 (x)", "", false},
		{"digit flood", "claude-cli/" + strings.Repeat("9", 200) + ".1.1 (x)", "", false},
		{"empty", "", "", false},
	}
	for _, c := range cases {
		got, ok := proxy.ClaudeCLIVersion(c.ua)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: ClaudeCLIVersion = (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}
