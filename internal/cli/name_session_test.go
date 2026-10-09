package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

type modelCall struct {
	bin   string
	args  []string
	env   []string
	stdin string
	dl    bool
	left  time.Duration
}

// stubModel installs a fake child run returning out/err and records calls.
func stubModel(t *testing.T, out string, runErr error) *[]modelCall {
	t.Helper()
	bindir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bindir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir)
	var calls []modelCall
	old := nameSessionModelRun
	nameSessionModelRun = func(ctx context.Context, bin string, args, env []string, stdin string) ([]byte, error) {
		d, dl := ctx.Deadline()
		calls = append(calls, modelCall{bin, args, env, stdin, dl, time.Until(d)})
		return []byte(out), runErr
	}
	t.Cleanup(func() { nameSessionModelRun = old })
	return &calls
}

func (e nsEnv) hookPrompt(t *testing.T, prompt string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": nsID,
		"transcript_path": e.tr, "cwd": e.repo, "prompt": prompt})
	var out, errb bytes.Buffer
	runNameSession(nil, bytes.NewReader(in), newReporter(false, &out, &errb))
	return out.String()
}

func TestNameSessionModelTitle(t *testing.T) {
	good := `{"type":"result","subtype":"success","is_error":false,"result":"Add upload client retry"}`
	cases := []struct {
		name, mode, head, reply string
		runErr                  error
		want                    string
		calls                   int
	}{
		{"ok", "model", "ref: refs/heads/main\n", good, nil, "Acme · Add upload client retry", 1},
		{"opt-in off", "", "ref: refs/heads/main\n", good, nil, "Acme 09:07", 0},
		{"branch session", "model", "ref: refs/heads/feat/x\n", good, nil, "x", 0},
		{"too long", "model", "ref: refs/heads/main\n",
			`{"subtype":"success","is_error":false,"result":"` + strings.Repeat("a", 61) + `"}`, nil, "Acme 09:07", 1},
		{"too many words", "model", "ref: refs/heads/main\n",
			`{"subtype":"success","is_error":false,"result":"a b c d e f g h i"}`, nil, "Acme 09:07", 1},
		{"two lines", "model", "ref: refs/heads/main\n",
			`{"subtype":"success","is_error":false,"result":"Fix date\nin billing"}`, nil, "Acme 09:07", 1},
		{"empty", "model", "ref: refs/heads/main\n",
			`{"subtype":"success","is_error":false,"result":"  "}`, nil, "Acme 09:07", 1},
		{"is_error true", "model", "ref: refs/heads/main\n",
			`{"subtype":"success","is_error":true,"result":"Fix date test"}`, nil, "Acme 09:07", 1},
		{"api error text", "model", "ref: refs/heads/main\n",
			`{"subtype":"success","is_error":false,"result":"API Error: failed"}`, nil, "Acme 09:07", 1},
		{"not success", "model", "ref: refs/heads/main\n",
			`{"subtype":"error_max_turns","is_error":false,"result":"Fix date test"}`, nil, "Acme 09:07", 1},
		{"not json", "model", "ref: refs/heads/main\n", "plain", nil, "Acme 09:07", 1},
		{"timeout", "model", "ref: refs/heads/main\n", "", context.DeadlineExceeded, "Acme 09:07", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newNSEnv(t, c.head)
			t.Setenv("CHOTTAG_NAME_SESSIONS", c.mode)
			calls := stubModel(t, c.reply, c.runErr)
			out := e.hookPrompt(t, "Add a retry to the upload client")
			if !strings.Contains(out, `"sessionTitle":"`+c.want+`"`) {
				t.Errorf("out %q, want %q", out, c.want)
			}
			if len(*calls) != c.calls {
				t.Errorf("calls %d, want %d", len(*calls), c.calls)
			}
		})
	}
}

func TestNameSessionModelChildShape(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/main\n")
	t.Setenv("CHOTTAG_NAME_SESSIONS", "model")
	calls := stubModel(t, "", errors.New("x"))
	e.hookPrompt(t, strings.Repeat("é", 1500))
	if len(*calls) != 1 {
		t.Fatalf("calls %d", len(*calls))
	}
	c := (*calls)[0]
	if filepath.Base(c.bin) != "claude" || !c.dl {
		t.Errorf("bin %q deadline %v", c.bin, c.dl)
	}
	if c.left <= 0 || c.left > nameSessionModelTimeout {
		t.Errorf("deadline in %v, want within %v", c.left, nameSessionModelTimeout)
	}
	want := []string{"-p", "--model", "haiku", "--output-format", "json", "--no-session-persistence", "--settings", `{"disableAllHooks":true}`, "--tools", "", "--strict-mcp-config"}
	if strings.Join(c.args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args %q", c.args)
	}
	if slices.Contains(c.args, "--mcp-config") {
		t.Errorf("args load an MCP config: %q", c.args)
	}
	if !slices.Contains(c.env, "CHOTTAG_NAME_SESSIONS=0") {
		t.Errorf("env lacks opt-out")
	}
	head := "Give a 3 to 6 word title for this task. Reply with the title only.\n\n"
	if !strings.HasPrefix(c.stdin, head) || c.stdin[len(head):] != strings.Repeat("é", 1000) {
		t.Errorf("stdin shape: %d bytes", len(c.stdin))
	}
}

func TestNameSessionModelBaseSurvivesUpgrade(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/main\n")
	t.Setenv("CHOTTAG_NAME_SESSIONS", "model")
	stubModel(t, `{"subtype":"success","is_error":false,"result":"Fix flaky billing test"}`, nil)
	if out := e.hookPrompt(t, "Fix the flaky date test in billing"); !strings.Contains(out, "Acme · Fix flaky billing test") {
		t.Fatalf("first %q", out)
	}
	writeTranscript(t, e.tr, `{"type":"ai-title","aiTitle":"Billing date test"}`+"\n")
	calls := stubModel(t, "", errors.New("must not run"))
	in, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": nsID,
		"transcript_path": e.tr, "cwd": e.repo, "session_title": "Acme · Fix flaky billing test", "prompt": "next"})
	var out, errb bytes.Buffer
	runNameSession(nil, bytes.NewReader(in), newReporter(false, &out, &errb))
	if !strings.Contains(out.String(), "Acme · Billing date test") || len(*calls) != 0 {
		t.Errorf("upgrade %q calls %d", out.String(), len(*calls))
	}
}

func TestNameSessionModelNotAskedAgain(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/main\n")
	t.Setenv("CHOTTAG_NAME_SESSIONS", "model")
	calls := stubModel(t, "", errors.New("x"))
	// First prompt fails: the fallback name is saved.
	e.hookPrompt(t, "Add a retry to the upload client")
	// A later prompt with saved state and no generated title: no call.
	in, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": nsID,
		"transcript_path": e.tr, "cwd": e.repo, "session_title": "Acme 09:07", "prompt": "Fix the flaky date test in billing"})
	var out, errb bytes.Buffer
	runNameSession(nil, bytes.NewReader(in), newReporter(false, &out, &errb))
	if len(*calls) != 1 {
		t.Errorf("calls %d, want 1 (the first prompt only)", len(*calls))
	}
}

func TestNameSessionModelSkipsUserNamed(t *testing.T) {
	e := newNSEnv(t, "ref: refs/heads/main\n")
	t.Setenv("CHOTTAG_NAME_SESSIONS", "model")
	calls := stubModel(t, "", errors.New("x"))
	in, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": nsID,
		"transcript_path": e.tr, "cwd": e.repo, "session_title": "my own", "prompt": "Add a retry to the upload client"})
	var out, errb bytes.Buffer
	runNameSession(nil, bytes.NewReader(in), newReporter(false, &out, &errb))
	if len(*calls) != 0 || out.Len() != 0 {
		t.Errorf("calls %d out %q", len(*calls), out.String())
	}
}

func TestNameSessionModelDefaultRunnerBoundedByGrandchild(t *testing.T) {
	// The fake claude starts a background child that holds stdout and
	// outlives it; the run must still return soon after the context ends.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nsleep 30 &\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The default runner is the production one; TestMain replaced it.
	run := defaultNameSessionModelRun
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := run(ctx, bin, nil, os.Environ(), "x"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("want an error from the killed child")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not return after its context ended")
	}
}
