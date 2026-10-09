package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/HaiNNT/c-hottag/internal/sessname"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// nameSessionNow is the clock for the `<folder> HH:MM` fallback name. A
// package-level var so tests fix the time.
var nameSessionNow = time.Now

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
// CHOTTAG_NAME_SESSIONS overrides it (0 is off, any other value is ignored,
// so the removed "model" reads as the state's mode, on by default). A missing or unreadable state means on.
func namesModeFor(home string) string {
	switch os.Getenv("CHOTTAG_NAME_SESSIONS") {
	case "0":
		return store.NamesOff
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
