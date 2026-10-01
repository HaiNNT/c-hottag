// Package cli implements chottag's command line.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/shim"
)

// Version is set at build time with -ldflags "-X github.com/HaiNNT/c-hottag/internal/cli.Version=...".
var Version = "dev"

const usage = `usage: chottag <command>

commands:
  version                       print version
  setup [--label NAME]          install the shim: tree, CA, symlinks, PATH; then adopt
  uninstall [--purge]           remove the shim and restore the shell rc; --purge also deletes every login
  adopt [--claude PATH]         register the account slots that already hold a login
  login NAME [--claude PATH]    log a slot in (browser) and register it
  logout NAME [--force] [--yes] revoke a slot's login and remove it
  tag [NAME] [--force]          set the serving account (no NAME: next in rotation)
  next [--force]                serving account -> next in rotation
  remote [NAME]                 set or show the account that owns claude.ai objects
  own KIND ID [ACCOUNT]         re-attribute one claude.ai object, or print its owner
  rotate NAME [on|off]          include or exclude NAME from next rotation
  rename OLD NEW                rename an account (display name only; the slot and its login stay)
  notify [on|off]               show or set desktop notifications (macOS; on by default)
  auto [VERB]                   show auto-switch (on by default); VERB: on, off, mode M, set KEY VALUE, reset
  plan NAME TIER [--units N]    set NAME's plan tier for auto-switch: pro, max5x, max20x or team
  status [--json] (alias ls)    print each account's usage and limit state, and the live sessions
  statusline [--json] [--cmux]  one line for Claude Code's status line: whether this session goes through chottag, and its account
  doctor [--fix]                check the install; --fix repairs what it safely can
  proxy run [flags]             run the routing proxy in the foreground
  daemon run [--claude PATH]    run the daemon in the foreground (the shim starts it on demand)
  daemon start                  start the daemon in the background unless it is running
  daemon stop [--force]         stop the daemon (the next claude launch starts it again)
  daemon restart [--force]      stop the daemon, then start it again
  daemon logs [-n N] [-f]       print the last N lines of daemon.log; -f follows it
  update [--check] [--version V] [--repo OWNER/NAME] [--restart|--no-restart]  install the latest release (from the repo you installed from); --check only reports
  update [--auto-check on|off] [--auto-install on|off] [--auto-restart on|off]  turn the daily update check, automatic install, and restart when idle, on or off
  trace [on [--for DUR]|off]    show or switch the daemon's trace mode (default 1h, at most 24h)
  trace mark TEXT...            add a marker to the trace log
  trace summarize [--all]       print the route table since the last trace on (--all: whole log)
  trace run [flags]             developer tool: a separate observe-only proxy
  trace env [--listen ADDR]     developer tool: export lines for a trace run session
  --json (global)               print one JSON document (most commands)
`

// helpAliases maps a command spelling to the name usage files its lines
// under, so commandUsage and the --help guard answer the same for every
// spelling of a command (F164): ls -> status, and rc/remote-control ->
// remote (the same aliases dispatch's switch accepts).
var helpAliases = map[string]string{
	"ls":             "status",
	"rc":             "remote",
	"remote-control": "remote",
}

// usageLines returns usage's own lines (each still carrying its leading
// two-space indent) whose first field is cmd, resolving cmd through
// helpAliases first. nil means cmd names no line in usage: an unknown
// command, or the pseudo-commands help/-h/--help, which are not listed
// there and already print the whole thing unconditionally (dispatch's own
// "help" case).
func usageLines(cmd string) []string {
	if target, ok := helpAliases[cmd]; ok {
		cmd = target
	}
	var lines []string
	for _, line := range strings.Split(usage, "\n") {
		trimmed := strings.TrimPrefix(line, "  ")
		if trimmed == line {
			continue // not a two-space-indented command line
		}
		if fields := strings.Fields(trimmed); len(fields) > 0 && fields[0] == cmd {
			lines = append(lines, line)
		}
	}
	return lines
}

// commandUsage returns cmd's own usage lines (an alias resolves first, so
// commandUsage("ls") returns status's line) under a one-line "usage:
// chottag <command>" header — a command group like "daemon" or "trace"
// gets every one of its verbs' lines, since usage files them all under the
// group's own first field. An unknown cmd falls back to the full usage
// text, the same text dispatch's default case already prints for it (F164).
func commandUsage(cmd string) string {
	lines := usageLines(cmd)
	if lines == nil {
		return usage
	}
	return "usage: chottag " + cmd + "\n\n" + strings.Join(lines, "\n") + "\n"
}

// wantsHelp reports whether args, scanned only up to the first "--", asks
// for help: -h, -help, --help, or -help=…/--help=… in any position. "--"
// stops the scan because everything after it belongs to the command itself
// (trace run -- claude --help forwards --help to the traced claude, not to
// chottag), and a positional argument that merely contains "help" (e.g. an
// account named "help-desk") is never a match (F164).
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "-help" || a == "--help" ||
			strings.HasPrefix(a, "-help=") || strings.HasPrefix(a, "--help=") {
			return true
		}
	}
	return false
}

// isShimInvocation reports whether this process was launched through the
// `claude` symlink rather than as `chottag`. The shim path is selected by
// argv[0] so one binary serves both roles and there is no copy to keep in
// sync (§4.3).
func isShimInvocation(argv0 string) bool {
	return filepath.Base(argv0) == "claude"
}

// versionResult is `version --json`'s field (spec §5.3).
type versionResult struct {
	Chottag string `json:"chottag"`
}

func Run(argv0 string, args []string, stdout, stderr io.Writer) int {
	// An argv0 whose base is `claude` reaches shim.Run. This package's own
	// TestMain (invariants_test.go) installs a panicking execFn/spawnFn pair
	// via shim.SetSeamsForTest before any test runs, so a test that reaches
	// here without stubbing them itself fails loudly instead of really
	// syscall.Exec'ing the user's claude or forking a detached daemon —
	// literally identical to the protection internal/shim's own TestMain
	// gives its package (fix round 3, D7/F-H closed the gap: shim's TestMain
	// used to guard only spawnFn, so "identical" was false as written until
	// it also gained an execFn default). Production code installs nothing
	// here; shim's default execFn/spawnFn (syscallExec/spawnDaemon) are what
	// actually run.
	//
	// The global-flag pre-pass runs AFTER this branch, never before it: the
	// shim forwards its arguments to Claude Code untouched, --json included
	// (spec §5.3).
	if isShimInvocation(argv0) {
		h, err := home()
		if err != nil {
			fmt.Fprintln(stderr, "chottag:", err)
			return exit.Error
		}
		// shim cannot import cli (cli imports shim), so Version is passed
		// explicitly rather than through a package var (fix round 2, N2): a
		// var could silently carry the wrong build's version with nothing
		// to catch it; a parameter fails loud the moment a caller forgets
		// it, and a test can pin the exact value threaded through. Run
		// compares a proof-less daemon's health Version against it to
		// decide legacy vs mismatch (fix round 1 item 5).
		return shim.Run(args, h, os.Environ(), Version, stdout, stderr)
	}
	g, rest, err := splitGlobal(args)
	if err != nil {
		// JSON mode is undecidable here, so this stays text only.
		fmt.Fprintln(stderr, "chottag:", err)
		return exit.Usage
	}
	return runCommand(g, rest, stdout, stderr, dispatch)
}

// runCommand builds the invocation's one reporter, runs d, and holds the
// result to the one-document contract: under --json a command that returned
// without reporting gets the internal fallback and exit 1 (spec §5.3). d is
// a parameter only so a test can hand it a command that reports nothing.
func runCommand(g globalFlags, args []string, stdout, stderr io.Writer, d func([]string, *reporter) int) int {
	r := newReporter(g.json, stdout, stderr)
	return r.finish(d(args, r))
}

// refuseJSON is the answer of a command that streams or runs forever
// (spec §5.3): exit 2 before doing any work, so a caller that asked for JSON
// never receives text on stdout.
func refuseJSON(r *reporter, command string) int {
	return r.Fail(exit.Usage, codeJSONUnsupported, command+" does not support --json", nil)
}

// dispatch runs the command args[0] names. Its case labels are the
// dispatch list that Task 8's completeness test reads.
func dispatch(args []string, r *reporter) int {
	if len(args) == 0 {
		fmt.Fprint(r.Stderr(), usage)
		return r.FailNoText(exit.Usage, codeUsage, "no command given", nil)
	}
	// A --help anywhere in args[1:], up to the first "--", answers with
	// that command's usage and runs nothing else — before the switch below
	// runs a byte of the command itself (F164: `setup --help` used to
	// install the shim and edit the shell rc, only then fail inside the
	// inner adopt's own flag parsing). usageLines(args[0]) is nil for an
	// unknown command, so `chottag nosuch --help` still falls through to
	// the unknown-command error below exactly as `chottag nosuch` does.
	if usageLines(args[0]) != nil && wantsHelp(args[1:]) {
		if r.JSON() {
			return refuseJSON(r, "help")
		}
		fmt.Fprint(r.Stdout(), commandUsage(args[0]))
		return exit.OK
	}
	switch args[0] {
	case "version":
		r.Text("chottag %s\n", Version)
		return r.OK(versionResult{Chottag: Version})
	case "trace":
		// runTrace refuses --json itself, for run, env, mark and summarize
		// only (M2c): on, off and bare trace answer with one document.
		return runTrace(args[1:], r)
	case "setup":
		return runSetup(args[1:], r)
	case "uninstall":
		return runUninstall(args[1:], os.Stdin, r)
	case "adopt":
		return runAdopt(args[1:], r)
	case "login":
		return runLogin(args[1:], os.Stdin, r)
	case "logout":
		return runLogout(args[1:], os.Stdin, r)
	case "tag":
		return runTag(args[1:], r)
	case "next":
		return runNextCmd(args[1:], r)
	case "remote", "rc", "remote-control":
		return runRemote(args[1:], r)
	case "rotate":
		return runRotate(args[1:], r)
	case "rename":
		return runRename(args[1:], r)
	case "notify":
		return runNotify(args[1:], r)
	case "auto":
		return runAuto(args[1:], r)
	case "plan":
		return runPlan(args[1:], r)
	case "status", "ls":
		h, err := home()
		if err != nil {
			return r.FailErr(err)
		}
		return runStatus(h, args[1:], r)
	case "statusline":
		return runStatusline(args[1:], r)
	case "own":
		h, err := home()
		if err != nil {
			return r.FailErr(err)
		}
		return runOwn(h, args[1:], r)
	case "doctor":
		return runDoctor(args[1:], r)
	case "proxy":
		if r.JSON() {
			return refuseJSON(r, "proxy")
		}
		return runProxy(args[1:], r)
	case "daemon":
		return runDaemonCmd(args[1:], r)
	case "update":
		return runUpdate(args[1:], r)
	case "help", "-h", "--help":
		if r.JSON() {
			return refuseJSON(r, "help")
		}
		fmt.Fprint(r.Stdout(), usage)
		return exit.OK
	default:
		fmt.Fprintf(r.Stderr(), "chottag: unknown command %q\n%s", args[0], usage)
		return r.FailNoText(exit.Usage, codeUsage, fmt.Sprintf("unknown command %q", args[0]), nil)
	}
}

// home is $CHOTTAG_HOME or ~/.chottag, always absolute: a relative
// CHOTTAG_HOME would otherwise resolve differently depending on the
// process's current directory at the moment each command happens to read
// it — the daemon and a later CLI invocation practically never share one —
// silently pointing them at two different trees (F5).
func home() (string, error) {
	if h := os.Getenv("CHOTTAG_HOME"); h != "" {
		return filepath.Abs(h)
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".chottag"), nil
}
