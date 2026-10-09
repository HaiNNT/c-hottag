package cmuxctl

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const topJSON = `{"windows":[{"workspaces":[
 {"id":"WS-1","title":"Acme api","panes":[{"surfaces":[
   {"id":"S-IDLE","ref":"surface:1","title":"alice@Mac:/tmp","processes":[
     {"name":"zsh (kiro-cli-t","children":[{"name":"zsh","children":[]}]}]},
   {"id":"S-EMPTY","ref":"surface:2","title":"Terminal","processes":[]},
   {"id":"S-VIM","ref":"surface:3","title":"vim","processes":[
     {"name":"zsh","children":[{"name":"vim","children":[]}]}]},
   {"id":"S-CLAUDE","ref":"surface:4","title":"claude","processes":[
     {"name":"2.1.291","children":[{"name":"zsh","children":[]}]}]},
   {"id":"S-GIT","ref":"surface:6","title":"t","processes":[
     {"name":"zsh","children":[{"name":"gitstatusd-darwin-arm64","children":[]}]}]}
 ]}]},
 {"id":"WS-2","title":"Acme web","panes":[{"surfaces":[
   {"id":"S-BASH","ref":"surface:5","title":"t","processes":[{"name":"-bash","children":[]}]}]}]}
]}]}`

type rec struct {
	bin  string
	args []string
	env  []string
	n    int
}

func fake(out string, err error) (Client, *rec) {
	r := &rec{}
	return Client{Bin: "/bin/cmux", Run: func(bin string, args []string, env []string) ([]byte, error) {
		r.bin, r.args, r.env = bin, args, env
		r.n++
		return []byte(out), err
	}}, r
}

func panicClient() Client {
	return Client{Bin: "x", Run: func(string, []string, []string) ([]byte, error) {
		panic("runner must not be called")
	}}
}

func hasQuiet(env []string) bool {
	for _, e := range env {
		if e == "CMUX_QUIET=1" {
			return true
		}
	}
	return false
}

func TestSnapshotIdle(t *testing.T) {
	c, r := fake(topJSON, nil)
	s, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"S-IDLE": true, "S-EMPTY": true, "S-BASH": true, "S-VIM": false, "S-CLAUDE": false, "S-GIT": true} {
		got, ok := s.Surfaces[id]
		if !ok || got.Idle != want {
			t.Errorf("%s: ok=%v idle=%v want %v", id, ok, got.Idle, want)
		}
	}
	if s.Surfaces["S-IDLE"].Workspace != "WS-1" || s.Surfaces["S-IDLE"].Ref != "surface:1" {
		t.Errorf("surface: %+v", s.Surfaces["S-IDLE"])
	}
	if s.Workspaces["WS-2"] != "Acme web" {
		t.Errorf("workspaces: %v", s.Workspaces)
	}
	want := []string{"--json", "--id-format", "both", "top", "--all", "--processes"}
	if !reflect.DeepEqual(r.args, want) || !hasQuiet(r.env) || r.bin != "/bin/cmux" {
		t.Errorf("argv %v env %v", r.args, r.env)
	}
}

func TestSnapshotBadJSON(t *testing.T) {
	c, _ := fake("not json", nil)
	if _, err := c.Snapshot(); err == nil {
		t.Fatal("want error")
	}
}

func TestSendArgs(t *testing.T) {
	c, r := fake("OK\n", nil)
	if err := c.Send("WS-1", "S-IDLE", "cd '/x' && claude --resume N"); err != nil {
		t.Fatal(err)
	}
	want := []string{"send", "--workspace", "WS-1", "--surface", "S-IDLE", "cd '/x' && claude --resume N\n"}
	if !reflect.DeepEqual(r.args, want) || !hasQuiet(r.env) {
		t.Errorf("argv %q", r.args)
	}
}

func TestSendRefusesEmptyIDs(t *testing.T) {
	c := panicClient()
	if c.Send("", "S", "x") == nil || c.Send("W", "", "x") == nil {
		t.Fatal("want errors")
	}
}

func TestNewTabParsesUUID(t *testing.T) {
	out := "OK surface:28 (E983EC5D-2601-4C14-894B-05EDB2CC3ADD) pane:4 (E6A3AA01-E99B-46A6-8B4C-19C63B958DE5) workspace:4 (E72B0882-62C2-44E3-9566-AAFA22CE3974)\n"
	c, r := fake(out, nil)
	id, err := c.NewTab("WS-1")
	if err != nil || id != "E983EC5D-2601-4C14-894B-05EDB2CC3ADD" {
		t.Fatalf("%q %v", id, err)
	}
	want := []string{"--id-format", "both", "new-surface", "--type", "terminal", "--workspace", "WS-1", "--focus", "false"}
	if !reflect.DeepEqual(r.args, want) {
		t.Errorf("argv %q", r.args)
	}
	c, _ = fake("something odd\n", nil)
	if _, err := c.NewTab("WS-1"); err == nil || !strings.Contains(err.Error(), "something odd") {
		t.Errorf("err %v", err)
	}
	if _, err := panicClient().NewTab(""); err == nil {
		t.Error("want refusal")
	}
}

func TestNewWorkspaceParsesRef(t *testing.T) {
	c, r := fake("OK workspace:12\n", nil)
	ref, err := c.NewWorkspace("repo", "/Users/alice/repo", "cd '/Users/alice/repo' && claude --resume N")
	if err != nil || ref != "workspace:12" {
		t.Fatalf("%q %v", ref, err)
	}
	want := []string{"new-workspace", "--name", "repo", "--cwd", "/Users/alice/repo", "--command", "cd '/Users/alice/repo' && claude --resume N", "--focus", "false"}
	if !reflect.DeepEqual(r.args, want) {
		t.Errorf("argv %q", r.args)
	}
	p := panicClient()
	for _, a := range [][3]string{{"", "c", "x"}, {"n", "", "x"}, {"n", "c", ""}} {
		if _, err := p.NewWorkspace(a[0], a[1], a[2]); err == nil {
			t.Errorf("%v: want refusal", a)
		}
	}
}

func TestRunErrorIncludesOutput(t *testing.T) {
	boom := errors.New("exit 1")
	c, _ := fake("  bad thing \n", boom)
	err := c.Send("W", "S", "x")
	if err == nil || !errors.Is(err, boom) || !strings.Contains(err.Error(), "bad thing") {
		t.Errorf("err %v", err)
	}
}

func TestFind(t *testing.T) {
	look := func(string) (string, error) { return "/usr/bin/cmux", nil }
	noLook := func(string) (string, error) { return "", errors.New("no") }
	all := func(string) bool { return true }
	none := func(string) bool { return false }
	env := []string{"A=b", "CMUX_BUNDLED_CLI_PATH=/env/cmux"}
	if got := Find(env, look, all); got != "/env/cmux" {
		t.Errorf("env: %q", got)
	}
	if got := Find(env, look, none); got != "/usr/bin/cmux" {
		t.Errorf("look: %q", got)
	}
	only := func(p string) bool { return p == AppCLI }
	if got := Find(nil, noLook, only); got != AppCLI {
		t.Errorf("app: %q", got)
	}
	if got := Find(nil, noLook, none); got != "" {
		t.Errorf("none: %q", got)
	}
}
