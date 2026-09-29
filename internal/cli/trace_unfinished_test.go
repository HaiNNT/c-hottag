package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceSummarizeReportsUnfinishedStreams(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	log := `{"t":"2026-09-24T10:00:00Z","kind":"head","id":"0a1b2c3d","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","status":200,"respType":"text/event-stream"}
{"t":"2026-09-24T10:00:01Z","kind":"req","form":"mitm","method":"GET","host":"api.anthropic.com","path":"/v1/models","class":"untouched","status":200}
`
	if err := os.WriteFile(filepath.Join(home, "trace.jsonl"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "trace", "summarize")
	if code != 0 {
		t.Fatalf("summarize: %d %q", code, errs)
	}
	for _, w := range []string{"unfinished streams: 1", "POST api.anthropic.com /v1/messages"} {
		if !strings.Contains(out, w) {
			t.Errorf("summarize output missing %q:\n%s", w, out)
		}
	}
}
