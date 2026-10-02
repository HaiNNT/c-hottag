package shim

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxyauth"
)

// authBypassHome is a home whose state.json is corrupt: any path that reads
// it fails, so a run that succeeds provably never read it.
func authBypassHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func runAuthBypass(t *testing.T, home string, args, env []string) (int, execCall, string) {
	t.Helper()
	var got execCall
	defer swapExec(home, &got)()
	var errb strings.Builder
	code := Run(args, home, env, ownVersion, io.Discard, &errb)
	return code, got, errb.String()
}

func TestAuthSubcommandsBypassChottag(t *testing.T) {
	for _, args := range [][]string{
		{"auth", "login"},
		{"auth", "logout"},
		{"auth", "status", "--json"},
		{"setup-token"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := authBypassHome(t)
			real := fakeClaude(t)
			code, got, errs := runAuthBypass(t, home, args, []string{"PATH=" + filepath.Dir(real)})
			if code != 0 {
				t.Fatalf("Run = %d (%s), want 0", code, errs)
			}
			if got.bin != real {
				t.Errorf("exec'd %q, want %q", got.bin, real)
			}
			if !slices.Equal(got.args, args) {
				t.Errorf("args = %v, want %v unchanged", got.args, args)
			}
			if len(got.live) != 0 {
				t.Errorf("session registered: %v", got.live)
			}
			if _, err := os.Stat(filepath.Join(home, "ca")); err == nil {
				t.Error("ca was created under home")
			}
			b, _ := os.ReadFile(filepath.Join(home, "state.json"))
			if string(b) != "{not valid json" {
				t.Errorf("state.json was rewritten: %q", b)
			}
		})
	}
}

func TestOtherInvocationsTakeTheNormalPath(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"auth flow"},
		{"fix the auth bug"},
		{"-p", "auth"},
		{"--print", "setup-token"},
		{"mcp", "list"},
		{"Auth", "login"},
		{"AUTH"},
		{"auths"},
		{"--", "auth"},
		{"hello", "auth", "login"},
		{"--model", "auth"},
		{"--resume", "auth"},
		{"--name", "auth"},
		{"-c", "auth"},
		{"--debug", "auth", "login"},
		{"--verbose", "setup-token"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home := authBypassHome(t)
			real := fakeClaude(t)
			code, got, errs := runAuthBypass(t, home, args, []string{"PATH=" + filepath.Dir(real)})
			// The corrupt state.json makes the normal path fail before exec.
			if code == 0 || got.bin != "" {
				t.Fatalf("Run = %d, exec %q: want the normal path (fails on state.json)", code, got.bin)
			}
			if !strings.Contains(errs, "chottag:") {
				t.Errorf("stderr %q", errs)
			}
		})
	}
}

func TestAuthBypassDropsOnlyChottagsOwnProxy(t *testing.T) {
	home := authBypassHome(t)
	real := fakeClaude(t)
	secret := secretOf(t, home)
	own := secret.SessionProxyURL("127.0.0.1:47850", "default", proxyauth.NewSID())
	legacy := secret.ProxyURL("127.0.0.1:47850")
	ownCA := filepath.Join(home, "ca", "ca.pem")
	ownBundle := filepath.Join(home, "ca", "bundle.pem")
	base := []string{"PATH=" + filepath.Dir(real), "HOME=/Users/alice", "OTHER=1"}

	cases := []struct {
		name      string
		env       []string
		wantProxy string // "" = dropped
		wantLower string // https_proxy, "" = dropped
		wantCA    string // NODE_EXTRA_CA_CERTS is never touched
	}{
		{"session proxy dropped, CA kept", append(slices.Clone(base), "HTTPS_PROXY="+own, "https_proxy="+own, "NODE_EXTRA_CA_CERTS="+ownCA), "", "", ownCA},
		{"legacy credential dropped", append(slices.Clone(base), "HTTPS_PROXY="+legacy), "", "", ""},
		{"own bundle kept", append(slices.Clone(base), "NODE_EXTRA_CA_CERTS="+ownBundle), "", "", ownBundle},
		{"own bundle with a foreign proxy, both kept", append(slices.Clone(base), "HTTPS_PROXY=http://proxy.example.com:3128", "NODE_EXTRA_CA_CERTS="+ownBundle), "http://proxy.example.com:3128", "", ownBundle},
		{"foreign proxy and CA kept", append(slices.Clone(base), "HTTPS_PROXY=http://proxy.example.com:3128", "https_proxy=http://proxy.example.com:3128", "NODE_EXTRA_CA_CERTS=/etc/corp/ca.pem"), "http://proxy.example.com:3128", "http://proxy.example.com:3128", "/etc/corp/ca.pem"},
		{"foreign loopback proxy kept", append(slices.Clone(base), "HTTPS_PROXY=http://127.0.0.1:3128"), "http://127.0.0.1:3128", "", ""},
		{"credential-less non-default loopback kept", append(slices.Clone(base), "HTTPS_PROXY=http://127.0.0.1:47850"), "http://127.0.0.1:47850", "", ""},
		{"default-port loopback dropped", append(slices.Clone(base), "HTTPS_PROXY=http://127.0.0.1:47821"), "", "", ""},
		{"localhost credential dropped", append(slices.Clone(base), "HTTPS_PROXY=http://chottag:x@localhost:47850"), "", "", ""},
		{"ipv6 credential dropped", append(slices.Clone(base), "HTTPS_PROXY=http://chottag.default.0123456789abcdef0123456789abcdef:x@[::1]:47850"), "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, got, errs := runAuthBypass(t, home, []string{"auth", "login"}, c.env)
			if code != 0 {
				t.Fatalf("Run = %d (%s)", code, errs)
			}
			if v := envGet(got.env, "HTTPS_PROXY"); v != c.wantProxy {
				t.Errorf("HTTPS_PROXY = %q, want %q", v, c.wantProxy)
			}
			if v := envGet(got.env, "NODE_EXTRA_CA_CERTS"); v != c.wantCA {
				t.Errorf("NODE_EXTRA_CA_CERTS = %q, want %q", v, c.wantCA)
			}
			if v := envGet(got.env, "https_proxy"); v != c.wantLower {
				t.Errorf("https_proxy = %q, want %q", v, c.wantLower)
			}
			if envGet(got.env, "OTHER") != "1" || envGet(got.env, "HOME") != "/Users/alice" {
				t.Errorf("unrelated env lost: %v", got.env)
			}
		})
	}
}

// CHOTTAG_BYPASS=1 keeps its exact behaviour: environment untouched.
func TestExplicitBypassKeepsTheEnvironmentUnchanged(t *testing.T) {
	home := authBypassHome(t)
	real := fakeClaude(t)
	own := secretOf(t, home).ProxyURL("127.0.0.1:47850")
	env := []string{"CHOTTAG_BYPASS=1", "PATH=" + filepath.Dir(real), "HTTPS_PROXY=" + own, "NODE_EXTRA_CA_CERTS=" + filepath.Join(home, "ca", "ca.pem")}
	code, got, _ := runAuthBypass(t, home, []string{"auth", "login"}, env)
	if code != 0 || !slices.Equal(got.env, env) {
		t.Errorf("code %d env %v, want unchanged %v", code, got.env, env)
	}
}
