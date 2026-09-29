package cli

import (
	"io"
	"os"
)

// isInteractive reports whether a prompt may be shown on stdin (spec §5.3:
// "No prompts without a TTY"). logout's [y/N] and uninstall --purge's typed
// word prompt only when it is true and --json is off; otherwise they refuse
// with exit 3. A package variable so tests can decide: internal/cli's
// TestMain installs a non-interactive default, and a prompt test opts in
// with interactiveStdin(t).
var isInteractive = stdinIsTerminal

// stdinIsTerminal is true only for a character device that is not
// /dev/null. /dev/null is a character device too, and a `< /dev/null`
// caller (a cron job, a launchd agent) must get confirmation_required, not
// a prompt that reads EOF and aborts with exit 1.
func stdinIsTerminal(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

// SetInteractiveForTest swaps isInteractive for fn and returns a func that
// restores whatever was installed at the moment of the call, never
// production's stdinIsTerminal by name: that keeps TestMain's default
// armed across every stub-and-restore cycle (F130). Same contract as
// SetAuthExecForTest and SetSignalForTest, and in a non-test file for the
// same reason (F103).
func SetInteractiveForTest(fn func(stdin io.Reader) bool) (restore func()) {
	orig := isInteractive
	isInteractive = fn
	return func() { isInteractive = orig }
}
