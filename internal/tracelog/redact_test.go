package tracelog_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func TestAuthKind(t *testing.T) {
	cases := []struct {
		auth   string
		apiKey bool
		want   string
	}{
		{"", false, "none"},
		{"", true, "x-api-key"},
		{"Bearer sk-ant-oat01-abc", false, "oauth-access"},
		{"Bearer sk-ant-ort01-abc", false, "oauth-refresh"},
		{"Bearer sk-ant-api03-abc", false, "api-key"},
		{"Bearer eyJhbGciOi.x.y", false, "jwt"},
		{"Bearer something", false, "bearer-other"},
		{"Basic dXNlcjpwYXNz", false, "basic"},
		{"sk-ant-oat01-RAWSECRET", false, "secret"},
		{"weird", false, "other"},
	}
	for _, c := range cases {
		if got := tracelog.AuthKind(c.auth, c.apiKey); got != c.want {
			t.Errorf("AuthKind(%q,%v) = %q, want %q", c.auth, c.apiKey, got, c.want)
		}
	}
}

func TestIsIDAndIsSecret(t *testing.T) {
	ids := []string{"cse_01HABCDEF12345678", "123456", "3f2b8c1e-1234-4abc-9def-0123456789ab"}
	for _, s := range ids {
		if !tracelog.IsID(s) {
			t.Errorf("IsID(%q) = false", s)
		}
	}
	notIDs := []string{"v1", "messages", "bridge", "environments", "sk-ant-oat01-0123456789abcdef", "eyJhbGciOiJIUzI1NiJ9abcdef1234"}
	for _, s := range notIDs {
		if tracelog.IsID(s) {
			t.Errorf("IsID(%q) = true", s)
		}
	}
	if !tracelog.IsSecret("sk-ant-oat01-x") || !tracelog.IsSecret("eyJabc") || tracelog.IsSecret("hello") {
		t.Error("IsSecret wrong")
	}
}

func TestTemplatePath(t *testing.T) {
	tmpl, ids := tracelog.TemplatePath("/v1/code/sessions/cse_01HABCDEF12345678/bridge")
	if tmpl != "/v1/code/sessions/{id}/bridge" {
		t.Fatalf("tmpl = %q", tmpl)
	}
	if !reflect.DeepEqual(ids, []string{tracelog.HashID("cse_01HABCDEF12345678")}) {
		t.Fatalf("ids = %v", ids)
	}
	if tmpl, ids := tracelog.TemplatePath("/v1/messages"); tmpl != "/v1/messages" || ids != nil {
		t.Fatalf("got %q %v", tmpl, ids)
	}
	if tmpl, _ := tracelog.TemplatePath("/x/sk-ant-oat01-SECRETSECRET/y"); tmpl != "/x/{secret}/y" {
		t.Fatalf("secret segment not redacted: %q", tmpl)
	}
}

func TestQueryKeys(t *testing.T) {
	got := tracelog.QueryKeys("beta=true&limit=20&cursor=cur_01ABCDEFGHIJ123456&token=sk-ant-oat01-x")
	want := []string{"beta=true", "cursor", "limit=20", "token"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestQueryKeysProbe(t *testing.T) {
	// A bare "sk-ant-oat01-KEYSECRET" key (no "=", so url.ParseQuery gives it
	// an empty value) must itself be redacted: it is the key, not a value,
	// but it is still a raw secret and must never reach the log verbatim.
	got := tracelog.QueryKeys("sk-ant-oat01-KEYSECRET&email=alice@example.com&code=Ab3xY")
	joined := strings.Join(got, " ")
	for _, leak := range []string{"sk-ant-oat01-KEYSECRET", "alice@example.com", "Ab3xY"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("secret/email/code value leaked in QueryKeys output: %v", got)
		}
	}
	for _, want := range []string{"{secret}", "email", "code"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("QueryKeys output %v missing %q", got, want)
		}
	}
}

func TestQueryKeysNonAllowlisted(t *testing.T) {
	// "cursor" is short and non-id-looking but not in the {beta, limit}
	// allowlist, so it must be logged bare even though it would previously
	// have been inlined as cursor=abc.
	got := tracelog.QueryKeys("cursor=abc")
	want := []string{"cursor"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHashIDKeepsTypePrefix(t *testing.T) {
	a, b := tracelog.HashID("cse_01HABCDEF12345678"), tracelog.HashID("session_01HABCDEF12345678")
	if !strings.HasPrefix(a, "cse_") || !strings.HasPrefix(b, "session_") {
		t.Fatalf("prefixes not kept: %s %s", a, b)
	}
	pa, ha := tracelog.SplitIDLabel(a)
	pb, hb := tracelog.SplitIDLabel(b)
	if pa != "cse_" || pb != "session_" || ha != hb || len(ha) != 8 {
		t.Fatalf("split %q/%q %q/%q", pa, ha, pb, hb)
	}
	if strings.Contains(a, "01HABCDEF") {
		t.Fatalf("id body leaked: %s", a)
	}
	u := tracelog.HashID("123e4567-e89b-12d3-a456-426614174000")
	if p, h := tracelog.SplitIDLabel(u); p != "" || h != u || len(u) != 8 {
		t.Fatalf("uuid label %q split %q/%q", u, p, h)
	}
	if p, _ := tracelog.SplitIDLabel(tracelog.HashID("Env_UPPER_1234567890")); p != "" {
		t.Fatalf("non-lower-case prefix kept: %q", p)
	}
}
