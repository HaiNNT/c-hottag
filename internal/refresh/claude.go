// Package refresh renews an account slot's login by running the real claude
// binary inside that slot, letting Claude Code perform its own OAuth refresh.
// chottag never reads or writes a refresh token itself.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultArgs is the cheapest command measured to force an authenticated
// call (and therefore a refresh of an expired token) without spending model
// quota: `claude auth status` is local-only and never refreshes.
var DefaultArgs = []string{"mcp", "list"}

const defaultTimeout = 90 * time.Second

// defaultWaitDelay is how long execRun keeps waiting, after the child's own
// process has already exited or been killed, for grandchildren that
// inherited the discard pipes to release them.
const defaultWaitDelay = 5 * time.Second

// proxyEnv names the variables removed from the child's environment: the
// child must talk to Anthropic directly, not through chottag (which would
// recurse, and which never intercepts the OAuth host anyway).
var proxyEnv = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"NODE_EXTRA_CA_CERTS", "CLAUDE_CONFIG_DIR",
}

// authEnv names the variables that would override the slot's own login, or
// make the child behave as a nested session instead of renewing that login
// (R149): the daemon may be started from a shell that has them set, and a
// refresh that ran on them would renew nothing in the slot. CLAUDE_CONFIG_DIR
// is not here: ChildEnv sets it to the slot.
var authEnv = []string{
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_SESSION_KIND", "CLAUDE_CODE_ENTRYPOINT", "CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION",
}

type Claude struct {
	// Bin is the real claude binary (never chottag's shim).
	Bin string
	// Resolve, when set, finds the binary at the start of every Refresh and
	// replaces Bin: a long-running daemon must follow a claude that moved or
	// was installed after it started.
	Resolve func() (string, error)
	// Args defaults to DefaultArgs.
	Args []string
	// Timeout defaults to 90s.
	Timeout time.Duration
	// WaitDelay bounds how long the default Run keeps waiting, once the
	// child process itself has exited or been killed, for a grandchild
	// (e.g. an MCP server subprocess) that inherited the discard pipes to
	// release them. Defaults to 5s. Exposed mainly so tests don't have to
	// sleep for the real default.
	WaitDelay time.Duration
	// Run defaults to running the binary; tests replace it.
	Run func(ctx context.Context, bin string, args, env []string) error
}

// Refresh runs the command in slotDir. The child's output is discarded: it
// can contain account details, and chottag logs neither.
func (c Claude) Refresh(ctx context.Context, slotDir string) error {
	args := c.Args
	if len(args) == 0 {
		args = DefaultArgs
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	waitDelay := c.WaitDelay
	if waitDelay == 0 {
		waitDelay = defaultWaitDelay
	}
	bin := c.Bin
	if c.Resolve != nil {
		var err error
		if bin, err = c.Resolve(); err != nil {
			return fmt.Errorf("refresh %s: %v", slotDir, err)
		}
	}
	run := c.Run
	if run == nil {
		run = execRun(waitDelay, slotDir)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := run(ctx, bin, args, ChildEnv(slotDir)); err != nil {
		// %v, not %w, and that is load-bearing: it keeps this timeout's own
		// context.DeadlineExceeded (from the WithTimeout above) from
		// satisfying errors.Is on whatever tokens.doRefresh receives, so a
		// refresh that timed out here is classified as a genuine failure
		// and backs off — a login that keeps hanging past `timeout` should
		// be treated as broken, not retried immediately forever the way
		// tokens.doRefresh treats a *caller*-cancelled attempt. A future
		// %v -> %w tidy-up would silently flip that: this refresher would
		// stop ever backing off on a persistent hang.
		return fmt.Errorf("refresh %s: %s %s: %v", slotDir, bin, strings.Join(args, " "), err)
	}
	return nil
}

// ChildEnv is the environment a process reading or renewing the login in
// slotDir must run with: the parent's environment, minus the proxy and CA
// variables (so the child talks to Anthropic directly, never back through
// chottag) and minus the auth and session variables that would override the
// slot's login (authEnv), plus CLAUDE_CONFIG_DIR pointed at the slot and CHOTTAG_BYPASS=1, so a child
// that still reaches chottag's claude shim execs the real binary with no
// proxy (issue #2, R145).
func ChildEnv(slotDir string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !contains(proxyEnv, name) && !contains(authEnv, name) && name != "CHOTTAG_BYPASS" {
			env = append(env, kv)
		}
	}
	return append(env, "CLAUDE_CONFIG_DIR="+slotDir, "CHOTTAG_BYPASS=1")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// execRun returns a Run function that bounds cmd.Wait with waitDelay and
// runs the child in dir (L5): the child inherits whatever directory the
// daemon happens to be running from otherwise, and a refresh has no
// business reading or writing anywhere but the slot it is renewing. If the
// killed child leaves grandchildren holding the discard pipes open (e.g. a
// shell script's background/foreground subprocess), Wait would otherwise
// block until they exit on their own. WaitDelay forces the pipes closed
// shortly after so a context-triggered kill's timeout is actually enforced.
func execRun(waitDelay time.Duration, dir string) func(ctx context.Context, bin string, args, env []string) error {
	return func(ctx context.Context, bin string, args, env []string) error {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.Stdin = nil
		cmd.WaitDelay = waitDelay
		err := cmd.Run()
		// WaitDelay also fires on an ordinary (non-canceled) exit when a
		// grandchild — not the child itself — is still holding the
		// discard pipes open past waitDelay (e.g. a slow-to-release
		// MCP server subprocess spawned by `claude mcp list`). In that
		// case the parent already exited 0: the refresh completed, and
		// only the pipe wait was cut short, so treat it as success
		// rather than a spurious failure.
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return nil
		}
		return err
	}
}
