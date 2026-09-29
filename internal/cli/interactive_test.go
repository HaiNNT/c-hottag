package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// interactiveStdin makes isInteractive answer true for this test only: the
// prompt tests (logout's [y/N], uninstall --purge's typed word) feed a
// strings.Reader, which is never a terminal. Restores whatever was
// installed before, which is TestMain's non-interactive default (F130).
func interactiveStdin(t *testing.T) {
	t.Helper()
	t.Cleanup(SetInteractiveForTest(func(io.Reader) bool { return true }))
}

// Only a real terminal is interactive. A pipe, a regular file, /dev/null
// (a character device, but never a terminal) and a non-file reader are
// not. A real terminal cannot be produced without a pty, which the stdlib
// lacks, so that case is covered by the ModeCharDevice branch's absence
// of a counter-example rather than a positive row.
func TestStdinIsTerminalRejectsEveryNonTerminal(t *testing.T) {
	if stdinIsTerminal(strings.NewReader("y\n")) {
		t.Error("a strings.Reader counted as a terminal")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	if stdinIsTerminal(pr) {
		t.Error("a pipe counted as a terminal")
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if stdinIsTerminal(f) {
		t.Error("a regular file counted as a terminal")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if stdinIsTerminal(null) {
		t.Error("/dev/null counted as a terminal: it is a character device, so ModeCharDevice alone is not enough")
	}
}

// TestMain's default is non-interactive, and a stub-and-restore cycle must
// put that default back, never the production stdinIsTerminal (F130).
// Compared by function identity, not by calling it: `go test` usually hands
// the test binary /dev/null as stdin, on which production answers false
// too, so a behavioural check could not tell the two apart.
func TestInteractiveSeamDefaultsToNonInteractiveAfterARestore(t *testing.T) {
	isProduction := func() bool {
		return reflect.ValueOf(isInteractive).Pointer() == reflect.ValueOf(stdinIsTerminal).Pointer()
	}
	if isProduction() {
		t.Fatal("isInteractive is production's stdinIsTerminal: TestMain must install a non-interactive default")
	}
	if isInteractive(os.Stdin) {
		t.Fatal("TestMain's default answered true")
	}
	restore := SetInteractiveForTest(func(io.Reader) bool { return true })
	if !isInteractive(nil) {
		t.Fatal("the stub was not installed")
	}
	restore()
	if isProduction() || isInteractive(os.Stdin) {
		t.Fatal("after restore isInteractive is not TestMain's default: restore reinstalled production")
	}
}
