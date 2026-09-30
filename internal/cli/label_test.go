package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/selector"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestLabelTitle(t *testing.T) {
	for _, c := range []struct{ title, label, want string }{
		{"chottag: B needs login", "dev", "chottag · dev: B needs login"},
		{"chottag: all accounts limited", "dev", "chottag · dev: all accounts limited"},
		{"chottag: B needs login", "", "chottag: B needs login"},
		{"something else", "dev", "something else"},
		{"chottag:", "dev", "chottag:"},
		{"", "dev", ""},
	} {
		if got := labelTitle(c.title, c.label); got != c.want {
			t.Errorf("labelTitle(%q, %q) = %q, want %q", c.title, c.label, got, c.want)
		}
	}
}

func TestDaemonNotifyLabelsTitlesFromState(t *testing.T) {
	n := newRecordingNotifier()
	base := notifyTestState("A", "B")
	dn := newDaemonNotify(func() (store.State, error) {
		st, err := base()
		st.Label = "dev"
		return st, err
	}, n)
	defer dn.Close()
	notifyEventHook(dn, func(selector.Event) {})(needsLoginEvent("B"))
	got := drainNotices(t, dn, n)
	if len(got) != 1 || got[0].title != "chottag · dev: B needs login" {
		t.Fatalf("notices = %+v, want the labelled title", got)
	}
}

func setupLabelHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/zsh")
	return home
}

func TestSetupLabelFlag(t *testing.T) {
	home := setupLabelHome(t)
	label := func() string {
		st, err := (store.Store{Dir: home}).Load()
		if err != nil {
			t.Fatal(err)
		}
		return st.Label
	}
	run := func(args ...string) int {
		return runSetup(args, newReporter(false, io.Discard, io.Discard))
	}

	if c := run("--label", "dev"); c != 0 || label() != "dev" {
		t.Fatalf("--label dev: exit %d, label %q", c, label())
	}
	if c := run(); c != 0 || label() != "dev" {
		t.Fatalf("bare setup: exit %d, label %q; want it kept", c, label())
	}
	if c := run("--label=my-box-2"); c != 0 || label() != "my-box-2" {
		t.Fatalf("--label=my-box-2: exit %d, label %q", c, label())
	}
	for _, bad := range []string{"Dev", "has space", "under_score", strings.Repeat("a", 17), "é"} {
		if c := run("--label", bad); c != 2 || label() != "my-box-2" {
			t.Errorf("--label %q: exit %d, label %q; want exit 2 and state untouched", bad, c, label())
		}
	}
	if c := run("--label"); c != 2 {
		t.Errorf("--label with no value: exit %d, want 2", c)
	}
	if c := run("--label", ""); c != 0 || label() != "" {
		t.Fatalf(`--label "": exit %d, label %q; want cleared`, c, label())
	}
}

func TestSetupInvalidLabelTouchesNothing(t *testing.T) {
	home := setupLabelHome(t)
	var errb bytes.Buffer
	if c := runSetup([]string{"--label", "BAD"}, newReporter(false, io.Discard, &errb)); c != 2 {
		t.Fatalf("exit %d, want 2", c)
	}
	if !strings.Contains(errb.String(), "a-z") {
		t.Errorf("stderr %q does not name the rule", errb.String())
	}
	if _, err := os.Stat(home + "/ca"); err == nil {
		t.Error("setup created the tree despite an invalid label")
	}
}

func TestRenderStatusLabel(t *testing.T) {
	now := time.Unix(1789870000, 0)
	f := status.File{Serving: "A", Remote: "B"}
	var plain, labelled bytes.Buffer
	renderStatus(&plain, f, now)
	f.Label = "dev"
	renderStatus(&labelled, f, now)
	if !strings.HasPrefix(plain.String(), "serving: A   remote: B\n\n") {
		t.Errorf("unlabelled first line changed: %q", plain.String())
	}
	if want := strings.Replace(plain.String(), "remote: B\n", "remote: B   (dev)\n", 1); labelled.String() != want {
		t.Errorf("labelled = %q, want %q", labelled.String(), want)
	}
}

func TestParseSetupArgsRebuildsAdoptArgs(t *testing.T) {
	r := newReporter(false, io.Discard, io.Discard)
	got, label, set, code, err := parseSetupArgs([]string{"--name", "b=y", "--label", "dev", "--claude", "/x/claude", "--name=a=x"}, r)
	if err != nil || code != 0 || !set || label != "dev" {
		t.Fatalf("got label %q set %v code %d err %v", label, set, code, err)
	}
	if want := "--claude /x/claude --name a=x --name b=y"; strings.Join(got, " ") != want {
		t.Errorf("adopt args = %q, want %q", strings.Join(got, " "), want)
	}
	if _, _, _, code, err := parseSetupArgs([]string{"stray"}, r); err == nil || code != 2 {
		t.Errorf("stray positional: code %d err %v, want exit 2", code, err)
	}
}
