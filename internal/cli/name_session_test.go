package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/sessname"
)

const nsID = "3f2a1b4c-0d5e-4f60-8a7b-9c8d7e6f5a4b"

func init() {
	registerJSONCases(jsonCase{
		name: "name-session refuses --json", command: "name-session",
		setup:    func(t *testing.T) []string { cliJSONHome(t); return []string{"name-session"} },
		wantExit: exit.Usage, wantCode: codeJSONUnsupported, jsonOnly: true,
	})
}

type nsEnv struct {
	home, cfg, repo, tr string
}

func newNSEnv(t *testing.T, head string) nsEnv {
	t.Helper()
	e := nsEnv{home: t.TempDir(), cfg: t.TempDir(), repo: filepath.Join(t.TempDir(), "Acme")}
	t.Setenv("CHOTTAG_HOME", e.home)
	t.Setenv("CLAUDE_CONFIG_DIR", e.cfg)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CHOTTAG_NAME_SESSIONS", "")
	if err := os.MkdirAll(filepath.Join(e.repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repo, ".git", "HEAD"), []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	e.tr = filepath.Join(e.cfg, "projects", "-p", nsID+".jsonl")
	writeTranscript(t, e.tr, `{"type":"user"}`+"\n")
	old := nameSessionNow
	nameSessionNow = func() time.Time { return time.Date(2026, 10, 8, 9, 7, 0, 0, time.Local) }
	t.Cleanup(func() { nameSessionNow = old })
	return e
}

func writeTranscript(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e nsEnv) hook(t *testing.T, event, source, title string) (int, string) {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"hook_event_name": event, "session_id": nsID,
		"transcript_path": e.tr, "cwd": e.repo, "source": source, "session_title": title})
	var out, errb bytes.Buffer
	code := runNameSession(nil, bytes.NewReader(in), newReporter(false, &out, &errb))
	if errb.Len() != 0 {
		t.Errorf("stderr: %q", errb.String())
	}
	return code, out.String()
}

func TestNameSessionPrintsOnlyTheHookJSON(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/feat/m12-x\n")
	code, out := e.hook(t, "UserPromptSubmit", "", "")
	if code != exit.OK {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		H struct {
			Event string `json:"hookEventName"`
			Title string `json:"sessionTitle"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || !strings.HasSuffix(out, "\n") {
		t.Fatalf("output %q: %v", out, err)
	}
	if got.H.Event != "UserPromptSubmit" || got.H.Title != "m12-x" {
		t.Errorf("%+v", got)
	}
	// Upgrade with the transcript's title.
	writeTranscript(t, e.tr, `{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n")
	_, out = e.hook(t, "UserPromptSubmit", "", "m12-x")
	if !strings.Contains(out, `"hookEventName":"UserPromptSubmit"`) || !strings.Contains(out, "m12-x · Fix Acme login") {
		t.Errorf("upgrade %q", out)
	}
	// The store holds a hash, never the name.
	b, err := os.ReadFile(filepath.Join(e.home, "sessions", "names", nsID))
	if err != nil || strings.Contains(string(b), "Fix Acme") || !strings.Contains(string(b), sessname.HashOf("m12-x · Fix Acme login")) {
		t.Errorf("store %q %v", b, err)
	}
}

func TestNameSessionSilentCases(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/feat/x\n")
	if _, out := e.hook(t, "SessionStart", "startup", ""); out != "" {
		t.Errorf("clear printed %q", out)
	}
	if _, out := e.hook(t, "UserPromptSubmit", "", "my own"); out != "" {
		t.Errorf("user name printed %q", out)
	}
	t.Setenv("CHOTTAG_NAME_SESSIONS", "0")
	if code, out := e.hook(t, "UserPromptSubmit", "", ""); out != "" || code != exit.OK {
		t.Errorf("opt-out printed %q (%d)", out, code)
	}
}

func TestNameSessionBadInputExitsZeroSilently(t *testing.T) {
	newNSEnv(t, "ref: refs/heads/feat/x\n")
	for _, in := range []string{"", "not json", "{}", strings.Repeat("x", 2<<20)} {
		var out, errb bytes.Buffer
		code := runNameSession(nil, strings.NewReader(in), newReporter(false, &out, &errb))
		if code != exit.OK || out.Len() != 0 {
			t.Errorf("input %.10q: exit %d out %q", in, code, out.String())
		}
	}
}

func TestNameSessionBadTranscriptPath(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/feat/x\n")
	outside := filepath.Join(t.TempDir(), "t.jsonl")
	writeTranscript(t, outside, `{"type":"ai-title","aiTitle":"Leak"}`+"\n")
	e.tr = outside
	_, out := e.hook(t, "UserPromptSubmit", "", "")
	if strings.Contains(out, "Leak") || !strings.Contains(out, `"sessionTitle":"x"`) {
		t.Errorf("%q", out)
	}
}

func TestNameSessionFallbackClock(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/main\n")
	if _, out := e.hook(t, "UserPromptSubmit", "", ""); !strings.Contains(out, `"sessionTitle":"Acme 09:07"`) {
		t.Errorf("%q", out)
	}
}

func (e nsEnv) hookPrompt(t *testing.T, prompt string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": nsID,
		"transcript_path": e.tr, "cwd": e.repo, "prompt": prompt})
	var out, errb bytes.Buffer
	runNameSession(nil, bytes.NewReader(in), newReporter(false, &out, &errb))
	return out.String()
}
