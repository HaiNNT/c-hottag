// Package notify sends chottag's desktop notifications (M2 spec §4).
//
// It has three parts, each testable alone:
//   - A Notifier delivers one notice: Osascript on darwin, Nop elsewhere
//     (Linux notify-send is an outline, R53).
//   - A Dispatcher takes notices off the caller's goroutine. Enqueue never
//     blocks. A full queue, a failed send and a timed-out send are each
//     counted once, and nothing is retried.
//   - Events decides when a notice fires (D11): once per episode, with the
//     dedup state in memory for one daemon generation.
//
// A notice carries an account NAME, a reset time and a command. It never
// carries an email, an org, a token or a path: this package imports only
// the standard library, so nothing can hand it one
// (TestNotifyImportsOnlyTheStandardLibrary).
package notify

import (
	"context"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Notifier delivers one notice. Send must return once ctx is done.
type Notifier interface {
	Send(ctx context.Context, title, body string) error
}

// Nop is the notifier on every platform but darwin.
type Nop struct{}

// Send does nothing.
func (Nop) Send(context.Context, string, string) error { return nil }

// New returns the notifier for goos: Osascript on darwin, Nop elsewhere.
func New(goos string) Notifier {
	if goos == "darwin" {
		return Osascript{}
	}
	return Nop{}
}

// OsascriptPath is absolute, so nothing on the daemon's PATH can stand in
// for it.
const OsascriptPath = "/usr/bin/osascript"

// osascriptScript is the whole AppleScript, and it never changes. The title
// and body reach it only as argv items 1 and 2, which AppleScript treats as
// data: an account name holding a quote or `do shell script` is displayed,
// never run. The trailing "--" ends osascript's own option parsing, so a
// title that starts with "-" (even "-e", the flag this script itself uses)
// is never read as another option: it is argv data, unconditionally.
var osascriptScript = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
	"--",
}

// Osascript posts a notice through macOS Notification Center.
type Osascript struct{}

// Send runs osascript once. The Dispatcher bounds it with a timeout, and
// exec.CommandContext kills the process when ctx ends.
func (Osascript) Send(ctx context.Context, title, body string) error {
	args := append(append([]string(nil), osascriptScript...), clean(title), clean(body))
	return runFn(ctx, OsascriptPath, args)
}

// runFn runs a command to completion. A test replaces it through
// SetRunForTest, and each test binary's TestMain arms a panicking default.
var runFn = runCommand

func runCommand(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	// No stdin, stdout or stderr are set, so there is nothing to copy and
	// Wait returns as soon as a killed process exits. WaitDelay bounds it
	// anyway.
	cmd.WaitDelay = time.Second
	return cmd.Run()
}

// maxText bounds each of the title and the body, in bytes.
const maxText = 200

// clean turns control characters into spaces and cuts s to maxText bytes
// on a rune boundary. It is display hygiene, not the injection defence:
// argv is that.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= maxText {
		return s
	}
	cut := maxText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
