package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPRPolicyChecksTheSenderOnReopen (P4-R4, and the reopen fallback from
// the whole-branch final review): a structural check of pr-policy.yml's
// run block (does it branch on ACTION, does the author fallback sit
// between the sender check and the close) cannot tell the difference
// between a real either-writer-keeps-it-open rule and a broken one: a
// review found that narrowing the author arm's case pattern, turning
// either exit 0 into a no-op, or dropping the author call's exit 1 on API
// failure would all still pass a text-only check. So this file runs the
// step's actual `run:` block under /bin/sh, exactly as the pattern
// test/installsh uses for install.sh: only a fake `gh` on PATH (never the
// real one, per the HARD SAFETY RULE), and no network, no real repository,
// no real HOME.

// prPolicyFakeGh is that fake gh. It understands only the two calls the
// workflow makes. A permission lookup answers from a FAKE_PERM_<user> env
// var set by the test ("FAIL", or unset, simulates the API call itself
// failing, the way an outage or a renamed account would); a close call is
// only logged, never executed against anything. Every invocation is
// appended to $FAKE_LOG so the test can see what actually ran.
const prPolicyFakeGh = `#!/bin/sh
set -eu
echo "$*" >>"$FAKE_LOG"
case "$1" in
api)
	path=$2
	user=${path##*/collaborators/}
	user=${user%/permission}
	eval "perm=\${FAKE_PERM_$user:-}"
	if [ -z "$perm" ] || [ "$perm" = FAIL ]; then
		echo "fake gh: no usable permission for $user" >&2
		exit 1
	fi
	printf '%s\n' "$perm"
	;;
pr)
	# "pr close $PR --repo $REPO --comment $msg": nothing to do but log it
	# (above), so the test can tell a close happened without this fake
	# reaching a real pull request.
	;;
*)
	echo "fake gh: unsupported invocation: $*" >&2
	exit 1
	;;
esac
`

// runPRPolicyStep runs the "close unless invited" step's run: block (read
// fresh from the committed .github/workflows/pr-policy.yml, or from
// override when a mutation-check test supplies a scratch copy's text)
// under /bin/sh, with only the fake gh above on PATH. action, author and
// sender simulate the pull_request_target event's ACTION/AUTHOR/SENDER
// env lines; authorPerm and senderPerm answer the permission checks the
// script makes for each ("" leaves no FAKE_PERM_ set, so that call fails
// like an API error would; "FAIL" does the same thing more explicitly). It
// returns the run block's own exit code, whether it called `gh pr close`,
// and the fake gh's call log plus the run's combined output, for a
// failure message.
func runPRPolicyStep(t *testing.T, override, action, author, sender, authorPerm, senderPerm string) (exit int, closeCalled bool, detail string) {
	t.Helper()
	body := override
	if body == "" {
		body = runBlock(t, read(t, prPolicy), "close unless invited")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(prPolicyFakeGh), 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls.log")
	env := []string{
		"PATH=" + dir,
		"GH_TOKEN=fake-token",
		"REPO=HaiNNT/c-hottag",
		"PR=42",
		"ACTION=" + action,
		"AUTHOR=" + author,
		"SENDER=" + sender,
		"GITHUB_SERVER_URL=https://example-git.test",
		"FAKE_LOG=" + logPath,
	}
	if authorPerm != "" {
		env = append(env, "FAKE_PERM_"+author+"="+authorPerm)
	}
	if senderPerm != "" {
		env = append(env, "FAKE_PERM_"+sender+"="+senderPerm)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Dir = dir
	cmd.Env = env
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()
	code := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("run block: %v\n%s", runErr, out.String())
	}
	logBytes, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	log := string(logBytes)
	detail = "--- fake gh log ---\n" + log + "--- run output ---\n" + out.String()
	return code, strings.Contains(log, "pr close"), detail
}

func TestPRPolicyRunBlockDecidesFromLiveAccessChecks(t *testing.T) {
	cases := []struct {
		name                   string
		action                 string
		author, sender         string
		authorPerm, senderPerm string
		wantExit               int
		wantClose              bool
	}{
		{
			name:   "reopen: non-writer sender, writer author stays open",
			action: "reopened", author: "author1", sender: "sender1",
			authorPerm: "write", senderPerm: "read",
			wantExit: 0, wantClose: false,
		},
		{
			name:   "reopen: the author's own permission lookup fails, stays open by exiting the job",
			action: "reopened", author: "author1", sender: "sender1",
			authorPerm: "FAIL", senderPerm: "read",
			wantExit: 1, wantClose: false,
		},
		{
			name:   "reopen: neither sender nor author can write, closes again",
			action: "reopened", author: "author1", sender: "sender1",
			authorPerm: "read", senderPerm: "read",
			wantExit: 0, wantClose: true,
		},
		{
			name:   "opened by a non-writer closes",
			action: "opened", author: "author1", sender: "sender1",
			authorPerm: "read", senderPerm: "",
			wantExit: 0, wantClose: true,
		},
		{
			name:   "opened by a writer stays open",
			action: "opened", author: "author1", sender: "sender1",
			authorPerm: "write", senderPerm: "",
			wantExit: 0, wantClose: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exit, closeCalled, detail := runPRPolicyStep(t, "", c.action, c.author, c.sender, c.authorPerm, c.senderPerm)
			if exit != c.wantExit {
				t.Errorf("exit = %d, want %d\n%s", exit, c.wantExit, detail)
			}
			if closeCalled != c.wantClose {
				t.Errorf("gh pr close called = %v, want %v\n%s", closeCalled, c.wantClose, detail)
			}
		})
	}
}
