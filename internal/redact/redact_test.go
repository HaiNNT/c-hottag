package redact

import (
	"strings"
	"testing"
)

// TestUpstreamProxyMasksUserinfoButKeepsTheHost pins the ordinary case: a
// message built from the redacted value stays useful (the hosts are still
// there for an operator to compare) while the password never appears.
func TestUpstreamProxyMasksUserinfoButKeepsTheHost(t *testing.T) {
	got := UpstreamProxy("http://bob:hunter2@corp-a:8080")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("UpstreamProxy(%q) = %q: leaked the password", "http://bob:hunter2@corp-a:8080", got)
	}
	if strings.Contains(got, "bob") {
		t.Fatalf("UpstreamProxy(%q) = %q: leaked the username", "http://bob:hunter2@corp-a:8080", got)
	}
	if !strings.Contains(got, "corp-a") {
		t.Fatalf("UpstreamProxy(%q) = %q: dropped the host, message is no longer useful", "http://bob:hunter2@corp-a:8080", got)
	}
}

// TestUpstreamProxyHandlesTheSchemelessShape pins the case url.URL.Redacted()
// alone gets wrong: url.Parse reads "bob" as the scheme (not a userinfo
// username) for a scheme-less "user:pass@host:port" — the shape http_proxy
// env vars and `curl -x` accept — and stuffs the password into u.Opaque,
// which Redacted() does not mask.
func TestUpstreamProxyHandlesTheSchemelessShape(t *testing.T) {
	got := UpstreamProxy("bob:hunter2@proxy:8080")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("UpstreamProxy(%q) = %q: leaked the password via u.Opaque", "bob:hunter2@proxy:8080", got)
	}
}

// TestUpstreamProxyMasksAUsernameOnlyCredential pins F139: a bearer-token
// credential has no password at all ("https://sk-TOKEN@proxy:8080", a real
// shape for one), so u.Redacted() alone — it only ever masks u.User's
// Password — passes the whole username through untouched. The fix replaces
// u.User outright whenever it is set, password or not.
func TestUpstreamProxyMasksAUsernameOnlyCredential(t *testing.T) {
	got := UpstreamProxy("https://sk-SECRETTOKEN@proxy:8080")
	if strings.Contains(got, "sk-SECRETTOKEN") {
		t.Fatalf("UpstreamProxy(%q) = %q: leaked the username-only credential", "https://sk-SECRETTOKEN@proxy:8080", got)
	}
	if !strings.Contains(got, "proxy") {
		t.Fatalf("UpstreamProxy(%q) = %q: dropped the host, message is no longer useful", "https://sk-SECRETTOKEN@proxy:8080", got)
	}
}

func TestUpstreamProxyReportsAnEmptyValueAsNoUpstream(t *testing.T) {
	if got := UpstreamProxy(""); got != "(none)" {
		t.Errorf("UpstreamProxy(\"\") = %q, want \"(none)\"", got)
	}
}

func TestUpstreamProxyReportsAnUnparsableValueRatherThanPrintingItRaw(t *testing.T) {
	raw := "http://%zz/"
	got := UpstreamProxy(raw)
	if got == raw {
		t.Errorf("UpstreamProxy(%q) = %q: an unparsable value must not be echoed verbatim", raw, got)
	}
}

// TestWithoutUserinfoDropsThePasswordEntirely pins fix round 3's D1: unlike
// UpstreamProxy, this is not a mask — the userinfo must be gone, not
// replaced with a placeholder, because the value is meant for a comparison
// two sides make, not a message a human reads.
func TestWithoutUserinfoDropsThePasswordEntirely(t *testing.T) {
	got := WithoutUserinfo("http://bob:hunter2@corp-a:8080")
	want := "http://corp-a:8080"
	if got != want {
		t.Errorf("WithoutUserinfo(%q) = %q, want %q", "http://bob:hunter2@corp-a:8080", got, want)
	}
	if strings.Contains(got, "bob") || strings.Contains(got, "hunter2") || strings.Contains(got, "x") {
		t.Errorf("WithoutUserinfo(%q) = %q: userinfo survived, masked or not", "http://bob:hunter2@corp-a:8080", got)
	}
}

func TestWithoutUserinfoLeavesACredentialFreeValueUnchanged(t *testing.T) {
	got := WithoutUserinfo("http://corp-a:8080")
	if got != "http://corp-a:8080" {
		t.Errorf("WithoutUserinfo(%q) = %q, want the input unchanged", "http://corp-a:8080", got)
	}
}

func TestWithoutUserinfoReportsEmptyAndUnparsableAsEmpty(t *testing.T) {
	for _, raw := range []string{"", "bob:hunter2@proxy:8080", "http://%zz/"} {
		if got := WithoutUserinfo(raw); got != "" {
			t.Errorf("WithoutUserinfo(%q) = %q, want \"\": no scheme/host pair to compare on, and falling back to raw would put userinfo right back", raw, got)
		}
	}
}
