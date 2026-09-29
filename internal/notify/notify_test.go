package notify

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewPicksOsascriptOnlyOnDarwin(t *testing.T) {
	if _, ok := New("darwin").(Osascript); !ok {
		t.Errorf("New(darwin) = %T, want Osascript", New("darwin"))
	}
	for _, goos := range []string{"linux", "windows", "freebsd", ""} {
		if _, ok := New(goos).(Nop); !ok {
			t.Errorf("New(%q) = %T, want Nop (Linux notify-send is an outline, R53)", goos, New(goos))
		}
	}
	if err := (Nop{}).Send(context.Background(), "t", "b"); err != nil {
		t.Errorf("Nop.Send = %v, want nil", err)
	}
}

// TestOsascriptPassesTextAsArgvOnly is Review Focus 5: an account name
// holding AppleScript is data. The script is three fixed -e lines, and the
// title and body are argv items after them, byte for byte.
func TestOsascriptPassesTextAsArgvOnly(t *testing.T) {
	var gotName string
	var gotArgs []string
	withRun(t, func(_ context.Context, name string, args []string) error {
		gotName, gotArgs = name, append([]string(nil), args...)
		return nil
	})
	title := `chottag: B" & (do shell script "touch /tmp/pwned") & " needs login`
	body := `-x Run: chottag login B'; end run -- \" & quit`
	if err := (Osascript{}).Send(context.Background(), title, body); err != nil {
		t.Fatal(err)
	}
	if gotName != "/usr/bin/osascript" {
		t.Errorf("ran %q, want /usr/bin/osascript: an absolute path, so the daemon's PATH can't substitute another binary", gotName)
	}
	want := []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--",
		title, body,
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args =\n%q\nwant\n%q", gotArgs, want)
	}
}

// TestOsascriptStopsOptionParsingBeforeData is Review Focus 5, the review's
// fix-round-1 finding: osascript stops parsing -e/-l/etc. options at the
// first "--" (or the first non-option word). Without it, a title starting
// with "-" — say "-e", the exact flag osascript's own script uses — would
// be read as another -e option instead of display notification's data,
// letting an account name inject AppleScript into the running script.
func TestOsascriptStopsOptionParsingBeforeData(t *testing.T) {
	var gotArgs []string
	withRun(t, func(_ context.Context, _ string, args []string) error {
		gotArgs = append([]string(nil), args...)
		return nil
	})
	title := `-e display dialog "pwned"`
	body := "b"
	if err := (Osascript{}).Send(context.Background(), title, body); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--",
		title, body,
	}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args =\n%q\nwant\n%q: \"--\" must come before the title so osascript never reads it as an option", gotArgs, want)
	}
}

func TestOsascriptCleansControlCharactersAndBoundsLength(t *testing.T) {
	var gotArgs []string
	withRun(t, func(_ context.Context, _ string, args []string) error {
		gotArgs = append([]string(nil), args...)
		return nil
	})
	if err := (Osascript{}).Send(context.Background(), "a\nb\x00c\td", strings.Repeat("é", 300)); err != nil {
		t.Fatal(err)
	}
	if got := gotArgs[7]; got != "a b c d" {
		t.Errorf("title = %q, want control characters turned into spaces: %q", got, "a b c d")
	}
	body := gotArgs[8]
	if len(body) == 0 || len(body) > 200 || !utf8.ValidString(body) {
		t.Errorf("body is %d bytes (valid UTF-8: %v), want 1..200 bytes cut on a rune boundary", len(body), utf8.ValidString(body))
	}
}

func TestOsascriptReturnsTheRunnerError(t *testing.T) {
	withRun(t, func(context.Context, string, []string) error { return exec.ErrNotFound })
	if err := (Osascript{}).Send(context.Background(), "t", "b"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Send = %v, want exec.ErrNotFound passed back for the counter", err)
	}
}

func TestOsascriptHandsTheContextToTheRunner(t *testing.T) {
	withRun(t, func(ctx context.Context, _ string, _ []string) error { return ctx.Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Osascript{}).Send(ctx, "t", "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send = %v, want the caller's ctx (context.Canceled): the Dispatcher's timeout rides it", err)
	}
}
