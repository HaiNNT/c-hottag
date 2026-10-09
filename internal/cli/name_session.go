package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/HaiNNT/c-hottag/internal/sessname"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// nameSessionNow is the clock for the `<folder> HH:MM` fallback name. A
// package-level var so tests fix the time.
var nameSessionNow = time.Now

// nameSessionModelTimeout bounds the model-title child (R172).
const nameSessionModelTimeout = 4 * time.Second

// nameSessionModelRun runs the real Claude Code for a model title and returns
// its stdout. A package-level var so tests never run a real Claude Code; its
// TestMain default panics.
var nameSessionModelRun = defaultNameSessionModelRun

func defaultNameSessionModelRun(ctx context.Context, bin string, args, env []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	// A grandchild holding the stdout pipe must not outlive the budget: kill
	// the whole process group on cancel, and force the pipe closed shortly
	// after (chottag targets darwin and linux only).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// modelTitle asks the real Claude Code (past the shim, so no journal entry)
// for a short title; "" on any failure. It never logs the prompt or reply.
func modelTitle(home, prompt string) string {
	bin, err := realClaudeBin("", home)
	if err != nil {
		return ""
	}
	args := []string{"-p", "--model", "haiku", "--output-format", "json",
		"--no-session-persistence", "--settings", `{"disableAllHooks":true}`,
		"--tools", "", "--strict-mcp-config"}
	env := append(os.Environ(), "CHOTTAG_NAME_SESSIONS=0")
	ctx, cancel := context.WithTimeout(context.Background(), nameSessionModelTimeout)
	defer cancel()
	out, err := nameSessionModelRun(ctx, bin, args, env, sessname.ModelStdin(prompt))
	if err != nil {
		return ""
	}
	return sessname.ParseModelReply(out)
}

const nameSessionUsage = "usage: chottag name-session"

// maxHookInput bounds what name-session reads from stdin.
const maxHookInput = 1 << 20

// claudeConfigDir is where Claude Code keeps its projects: CLAUDE_CONFIG_DIR,
// else ~/.claude. "" when neither can be known.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(u, ".claude")
}

// namesModeFor is the session-naming mode for this run: the state's, unless
// CHOTTAG_NAME_SESSIONS overrides it (0 is off, model is model, any other
// value is ignored). A missing or unreadable state means on.
func namesModeFor(home string) string {
	switch os.Getenv("CHOTTAG_NAME_SESSIONS") {
	case "0":
		return store.NamesOff
	case "model":
		return store.NamesModel
	}
	st, err := store.Store{Dir: home}.Load()
	if err != nil {
		return store.NamesOn
	}
	return st.NamesMode()
}

// runNameSession is the Claude Code UserPromptSubmit command hook (R171;
// a SessionStart input prints nothing, F280). It always exits 0 and prints only the hook's JSON,
// or nothing. It never logs or stores a title.
func runNameSession(args []string, stdin io.Reader, r *reporter) int {
	if r.JSON() {
		return refuseJSON(r, "name-session")
	}
	if len(args) != 0 {
		return r.Usage(nameSessionUsage)
	}
	h, err := home()
	if err != nil {
		return r.OK(nil)
	}
	mode := namesModeFor(h)
	if mode == store.NamesOff {
		return r.OK(nil)
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil || len(b) > maxHookInput {
		return r.OK(nil)
	}
	var in sessname.Input
	if json.Unmarshal(b, &in) != nil {
		return r.OK(nil)
	}
	env := sessname.Env{Home: h, ConfigDir: claudeConfigDir(), Now: nameSessionNow}
	if mode == store.NamesModel {
		env.Model = func(prompt string) string { return modelTitle(h, prompt) }
	}
	name, ok := sessname.Decide(in, env)
	if !ok {
		return r.OK(nil)
	}
	var out struct {
		H struct {
			Event string `json:"hookEventName"`
			Title string `json:"sessionTitle"`
		} `json:"hookSpecificOutput"`
	}
	out.H.Event, out.H.Title = in.Event, name
	line, err := json.Marshal(out)
	if err != nil {
		return r.OK(nil)
	}
	r.Stdout().Write(append(line, '\n'))
	return r.OK(nil)
}
