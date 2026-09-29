package tracelog_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func TestShapeKeepsStructureDropsValues(t *testing.T) {
	body := []byte(`{"environment_id":"env_01ABCDEFGH12345678","token":"sk-ant-oat01-XYZ-SECRET","jwt":"eyJhbGciOi.SECRET","n":3,"ok":true,"none":null,"name":"my session","list":[{"a":1},{"a":2}]}`)
	b, _ := json.Marshal(tracelog.Shape(body))
	s := string(b)
	for _, leak := range []string{"SECRET", "my session", "env_01ABCDEFGH12345678"} {
		if strings.Contains(s, leak) {
			t.Fatalf("shape leaks %q: %s", leak, s)
		}
	}
	for _, want := range []string{
		`"environment_id":"id:` + tracelog.HashID("env_01ABCDEFGH12345678") + `"`,
		`"token":"secret"`, `"jwt":"secret"`, `"n":"number"`, `"ok":"bool"`, `"none":"null"`,
		`"name":"string"`, `"list":[{"a":"number"}]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("shape %s missing %s", s, want)
		}
	}
}

func TestShapeNonJSON(t *testing.T) {
	if tracelog.Shape([]byte("data: hi\n\n")) != nil {
		t.Fatal("non-JSON should give nil")
	}
}

func TestShapeKeepsFeatureFlagAndSchemaKeys(t *testing.T) {
	keys := []string{
		"tengu_velvet_mallet_sonnet_4_5",
		"claude_code_skills_dashboard_enabled_cli",
		"multisession_poll_interval_ms_at_capacity",
		"ccr_auto_create_pr_on_push",
		"tengu-model-error-overrides",
		"current_interval_seconds",
		"commentPathUnreadableEnabled", // all letters, no digits: kept by the digit floor (F186)
	}
	for _, k := range keys {
		body, err := json.Marshal(map[string]any{k: "v"})
		if err != nil {
			t.Fatalf("marshal %q: %v", k, err)
		}
		b, _ := json.Marshal(tracelog.Shape(body))
		s := string(b)
		wantKey := `"` + k + `":`
		if !strings.Contains(s, wantKey) {
			t.Errorf("key %q was collapsed, shape = %s", k, s)
		}
	}
}

func TestShapeCollapsesIDLikeKeys(t *testing.T) {
	keys := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"HaiNNT/c-hottag",
		"ios/com.anthropic.claude",
		"io.modelcontextprotocol/clientInfo",
		"alice@example.com",
		"0123456789abcdef",
		"1000000",
		"01HXYZABCD1234EFGH5678",
		"mcpsrv_01HXYZABCD12345678",
	}
	for _, k := range keys {
		body, err := json.Marshal(map[string]any{k: "v"})
		if err != nil {
			t.Fatalf("marshal %q: %v", k, err)
		}
		b, _ := json.Marshal(tracelog.Shape(body))
		s := string(b)
		if strings.Contains(s, k) {
			t.Errorf("key %q leaked, shape = %s", k, s)
		}
		if !strings.Contains(s, `"{key}":`) {
			t.Errorf("key %q did not collapse to {key}, shape = %s", k, s)
		}
	}
}

func TestShapeCollapsesIDLikeKeyNestedTwoLevelsDeep(t *testing.T) {
	body := []byte(`{"outer":{"11111111-1111-4111-8111-111111111111":{"n":1}}}`)
	b, _ := json.Marshal(tracelog.Shape(body))
	s := string(b)
	if strings.Contains(s, "11111111-1111-4111-8111-111111111111") {
		t.Fatalf("nested id key leaked: %s", s)
	}
	want := `{"outer":{"{key}":{"n":"number"}}}`
	if s != want {
		t.Fatalf("shape = %s, want %s", s, want)
	}
}

func TestShapeSecretKeyStillGivesSecretNotKey(t *testing.T) {
	body := []byte(`{"sk-ant-oat01-XYZSECRETSECRET":"v"}`)
	b, _ := json.Marshal(tracelog.Shape(body))
	s := string(b)
	want := `{"{secret}":"string"}`
	if s != want {
		t.Fatalf("shape = %s, want %s", s, want)
	}
}
