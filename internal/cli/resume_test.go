package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"os"

	"github.com/HaiNNT/c-hottag/internal/cmuxctl"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/journal"
)

func init() {
	registerJSONCases(
		jsonCase{
			name: "sessions on an empty home", command: "sessions",
			setup: func(t *testing.T) []string { cliJSONHome(t); return []string{"sessions"} },
			check: func(t *testing.T, doc map[string]any) {
				if a, ok := doc["sessions"].([]any); !ok || len(a) != 0 {
					t.Errorf("sessions = %v, want []", doc["sessions"])
				}
			},
		},
		jsonCase{
			name: "sessions with a stray argument", command: "sessions",
			setup:    func(t *testing.T) []string { cliJSONHome(t); return []string{"sessions", "extra"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
		jsonCase{
			name: "resume with nothing to resume", command: "resume",
			setup: func(t *testing.T) []string { cliJSONHome(t); return []string{"resume"} },
			check: func(t *testing.T, doc map[string]any) {
				if a, ok := doc["sessions"].([]any); !ok || len(a) != 0 {
					t.Errorf("sessions = %v, want []", doc["sessions"])
				}
			},
		},
		jsonCase{
			name: "resume with a stray argument", command: "resume",
			setup:    func(t *testing.T) []string { cliJSONHome(t); return []string{"resume", "extra"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}

const resumeTop = `{"windows":[{"workspaces":[
 {"id":"WS-1","title":"Acme api","panes":[{"surfaces":[
   {"id":"S-IDLE","ref":"surface:1","title":"alice@Mac:/tmp","processes":[{"name":"zsh","children":[]}]},
   {"id":"S-VIM","ref":"surface:3","title":"vim","processes":[{"name":"zsh","children":[{"name":"vim","children":[]}]}]}
 ]}]}
]}]}`

const newSurfaceOut = "OK surface:9 (AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA) pane:1 (BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB) workspace:1 (CCCCCCCC-CCCC-CCCC-CCCC-CCCCCCCCCCCC)\n"

// fakeCmux stubs cmuxFind and cmuxRun and records every argv.
type fakeCmux struct {
	calls    [][]string
	sendFail string // a surface id whose send errors
	topFail  bool   // cmux top errors
}

func (f *fakeCmux) sub(args []string) string {
	for _, a := range args {
		switch a {
		case "top", "new-surface", "new-workspace", "send":
			return a
		}
	}
	return ""
}

func (f *fakeCmux) sends() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if f.sub(c) == "send" {
			out = append(out, c)
		}
	}
	return out
}

func installCmux(t *testing.T) *fakeCmux {
	t.Helper()
	f := &fakeCmux{}
	oldRun, oldFind := cmuxRun, cmuxFind
	cmuxFind = func() string { return "/fake/cmux" }
	cmuxRun = func(bin string, args []string, env []string) ([]byte, error) {
		f.calls = append(f.calls, append([]string(nil), args...))
		switch f.sub(args) {
		case "top":
			if f.topFail {
				return []byte("cmux is unwell"), errors.New("exit status 1")
			}
			return []byte(resumeTop), nil
		case "new-surface":
			return []byte(newSurfaceOut), nil
		case "new-workspace":
			return []byte("OK workspace:7\n"), nil
		case "send":
			for i, a := range args {
				if a == "--surface" && args[i+1] == f.sendFail {
					return []byte("boom"), errors.New("exit status 1")
				}
			}
			return []byte("OK\n"), nil
		}
		t.Errorf("unexpected cmux argv %q", args)
		return nil, errors.New("unexpected")
	}
	t.Cleanup(func() { cmuxRun, cmuxFind = oldRun, oldFind })
	return f
}

func resumeHome(t *testing.T, alive map[int]bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	// Its own transcripts: the unrecorded scan must not see another test's.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubAlive(t, alive)
	return home
}

// testNative makes a valid native id from a short label.
func testNative(name string) string {
	h := hex.EncodeToString([]byte(name))
	return "00000000-0000-0000-0000-" + strings.Repeat("0", 12-len(h)) + h
}

var lostAt = time.Now().Add(-time.Hour).Truncate(time.Second)

// putLost writes an ended-and-lost entry with a native id, ended off seconds
// before lostAt (batch order is newest end first).
func putLost(t *testing.T, home string, pid int, off int, ws, surf, dir string) journal.Entry {
	t.Helper()
	e := journal.Entry{PID: pid, PPID: pid - 1, Started: lostAt.Add(-time.Hour + time.Duration(pid)*time.Second),
		SID: "sid", Pool: "default", Dir: dir, CmuxWorkspace: ws, CmuxSurface: surf,
		Native: testNative(dir[strings.LastIndex(dir, "/")+1:]), Ended: lostAt.Add(-time.Duration(off) * time.Second), Outcome: journal.OutcomeLost}
	if err := openJ(t, home).Put(e); err != nil {
		t.Fatal(err)
	}
	// Resume only relaunches a conversation that has a transcript.
	writeTranscript(t, filepath.Join(claudeConfigDir(), "projects", "-Users-alice", e.ResumeID()+".jsonl"), "{}\n")
	return e
}

func resumeDoc(t *testing.T, out string) resumeResult {
	t.Helper()
	decodeOneDocument(t, out)
	var res resumeResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func placements(res resumeResult) []string {
	var p []string
	for _, r := range res.Sessions {
		p = append(p, r.Placement)
	}
	return p
}

func TestResumePlacesEachKind(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	a := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	putLost(t, home, 12, 10, "WS-1", "S-VIM", "/Users/alice/b")
	putLost(t, home, 13, 20, "WS-GONE", "S-GONE", "/Users/alice/c")
	putLost(t, home, 14, 30, "WS-GONE", "S-GONE2", "/Users/alice/d")

	code, out, errs := runChottag(t, "resume", "--json")
	if code != exit.OK {
		t.Fatalf("resume = %d, stderr %q, out %q", code, errs, out)
	}
	if got, want := placements(resumeDoc(t, out)), []string{"tab", "new-tab", "new-workspace", "new-tab"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("placements %v, want %v", got, want)
	}
	sends := fc.sends()
	wantA := []string{"send", "--workspace", "WS-1", "--surface", "S-IDLE", journal.Command(a) + "\n"}
	if len(sends) != 3 || !reflect.DeepEqual(sends[0], wantA) {
		t.Fatalf("sends %q, want first %q", sends, wantA)
	}
	last := sends[2]
	if last[2] != "workspace:7" || last[4] != "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA" {
		t.Errorf("D's send %q: want workspace:7 and the new tab", last)
	}
	var sawNewWS bool
	for _, c := range fc.calls {
		for i, a := range c {
			if (a == "--workspace" || a == "--surface") && (i+1 >= len(c) || c[i+1] == "") {
				t.Errorf("argv %q has an empty %s", c, a)
			}
		}
		if fc.sub(c) == "new-workspace" {
			sawNewWS = true
			if c[2] != "c" || c[4] != "/Users/alice/c" {
				t.Errorf("new-workspace argv %q", c)
			}
		}
	}
	if !sawNewWS {
		t.Error("no new-workspace call")
	}
}

func TestResumeSameTabOnce(t *testing.T) {
	home := resumeHome(t, nil)
	installCmux(t)
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	putLost(t, home, 12, 5, "WS-1", "S-IDLE", "/Users/alice/b")
	_, out, _ := runChottag(t, "resume", "--json")
	if got, want := placements(resumeDoc(t, out)), []string{"tab", "new-tab"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("placements %v, want %v", got, want)
	}
}

func TestResumeTwiceDoesNothing(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	if code, _, errs := runChottag(t, "resume"); code != exit.OK {
		t.Fatalf("first resume = %d %q", code, errs)
	}
	n := len(fc.sends())
	code, out, _ := runChottag(t, "resume")
	if code != exit.OK || !strings.Contains(out, "chottag: no lost sessions to resume") {
		t.Fatalf("second resume = %d %q", code, out)
	}
	if len(fc.sends()) != n {
		t.Errorf("the second resume sent again: %q", fc.sends())
	}
}

func TestResumeSkipsRunningNative(t *testing.T) {
	home := resumeHome(t, map[int]bool{99: true})
	installCmux(t)
	e := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	run := journal.Entry{PID: 99, PPID: 98, Started: lostAt, Dir: "/Users/alice/a", ResumeOf: e.Native}
	if err := openJ(t, home).Put(run); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runChottag(t, "resume")
	if code != exit.OK || !strings.Contains(out, "no lost sessions to resume") {
		t.Fatalf("resume = %d %q", code, out)
	}
}

func TestResumeWithoutDaemonSweeps(t *testing.T) {
	home := resumeHome(t, nil) // every pid dead
	installCmux(t)
	open := journal.Entry{PID: 11, PPID: 1, Started: lostAt, Dir: "/Users/alice/a", Native: testNative("n1"), CmuxWorkspace: "WS-1", CmuxSurface: "S-IDLE"}
	if err := openJ(t, home).Put(open); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(claudeConfigDir(), "projects", "-Users-alice", open.Native+".jsonl"), "{}\n")
	code, out, errs := runChottag(t, "resume", "--json")
	if code != exit.OK {
		t.Fatalf("resume = %d %q %q", code, out, errs)
	}
	if got := placements(resumeDoc(t, out)); !reflect.DeepEqual(got, []string{"tab"}) {
		t.Fatalf("placements %v", got)
	}
}

func TestResumePrintMarksNothing(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	a := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	code, out, _ := runChottag(t, "resume", "--print")
	if code != exit.OK || strings.TrimSpace(out) != journal.Command(a) {
		t.Fatalf("resume --print = %d %q", code, out)
	}
	if len(fc.sends()) != 0 {
		t.Errorf("--print sent: %q", fc.sends())
	}
	code, out, _ = runChottag(t, "sessions")
	if code != exit.OK || !strings.Contains(out, "/Users/alice/a") {
		t.Fatalf("the batch is gone after --print: %d %q", code, out)
	}
}

func TestResumeNoCmux(t *testing.T) {
	home := resumeHome(t, nil)
	oldFind := cmuxFind
	cmuxFind = func() string { return "" }
	t.Cleanup(func() { cmuxFind = oldFind })
	a := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	code, out, errs := runChottag(t, "resume", "--json")
	if code != exit.OK {
		t.Fatalf("resume = %d %q %q", code, out, errs)
	}
	res := resumeDoc(t, out)
	if got := placements(res); !reflect.DeepEqual(got, []string{"print"}) || res.Sessions[0].Command != journal.Command(a) {
		t.Fatalf("rows %+v", res.Sessions)
	}
	if _, out, _ := runChottag(t, "resume"); !strings.Contains(out, "no lost sessions to resume") {
		t.Errorf("a no-cmux print was not marked: %q", out)
	}
}

func TestResumeFailedRowNotMarked(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	fc.sendFail = "S-IDLE"
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	code, out, _ := runChottag(t, "resume", "--json")
	doc := decodeOneDocument(t, out)
	e, _ := doc["error"].(map[string]any)
	if code != exit.Error || e["code"] != "resume_failed" || e["message"] != "1 of 1 sessions could not be resumed" {
		t.Fatalf("resume = %d %v", code, doc)
	}
	rows, _ := e["sessions"].([]any)
	if len(rows) != 1 || !strings.Contains(rows[0].(map[string]any)["error"].(string), "cmux send") {
		t.Errorf("rows %v", rows)
	}
	if _, out, _ := runChottag(t, "sessions"); !strings.Contains(out, "/Users/alice/a") {
		t.Errorf("the failed entry left the batch: %q", out)
	}
}

func TestResumePick(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	putLost(t, home, 12, 5, "WS-1", "S-VIM", "/Users/alice/b")
	pick := func(in string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runResume([]string{"--pick"}, strings.NewReader(in), newReporter(false, &out, &errb))
		return code, out.String(), errb.String()
	}

	if code, out, errs := pick("9\n"); code != exit.Usage || len(fc.sends()) != 0 {
		t.Fatalf("pick 9 = %d %q %q sends %q", code, out, errs, fc.sends())
	}
	if code, out, _ := pick("\n"); code != exit.OK || !strings.Contains(out, "nothing chosen") || len(fc.sends()) != 0 {
		t.Fatalf("empty pick = %d %q", code, out)
	}
	code, out, errs := pick("2\n")
	if code != exit.OK || !strings.Contains(out, "1  /Users/alice/a  (default)  ended") || !strings.Contains(out, `tab "vim"`) {
		t.Fatalf("pick 2 = %d %q %q", code, out, errs)
	}
	if len(fc.sends()) != 1 || !strings.Contains(fc.sends()[0][5], "/Users/alice/b") {
		t.Fatalf("sends %q", fc.sends())
	}
	if _, out, _ := runChottag(t, "sessions"); !strings.Contains(out, "/Users/alice/a") || strings.Contains(out, "/Users/alice/b") {
		t.Errorf("the unpicked entry should remain alone in the batch: %q", out)
	}

	var o, e bytes.Buffer
	if code := runResume([]string{"--pick"}, strings.NewReader("1\n"), newReporter(true, &o, &e)); code != exit.Usage {
		t.Errorf("--pick --json = %d, want usage", code)
	}
	if doc := decodeOneDocument(t, o.String()); doc["error"].(map[string]any)["code"] != "usage" {
		t.Errorf("doc %v", doc)
	}
}

func TestResumeJSONShape(t *testing.T) {
	home := resumeHome(t, nil)
	installCmux(t)
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	putLost(t, home, 12, 5, "WS-1", "S-VIM", "/Users/alice/b")
	putLost(t, home, 13, 10, "WS-GONE", "S-GONE", "/Users/alice/c")
	_, out, _ := runChottag(t, "resume", "--json")
	res := resumeDoc(t, out)
	if got, want := placements(res), []string{"tab", "new-tab", "new-workspace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("placements %v", got)
	}
	r0 := res.Sessions[0]
	if r0.Dir != "/Users/alice/a" || r0.Native != testNative("a") || !r0.Ended.Equal(lostAt) || r0.Command == "" {
		t.Errorf("row %+v", r0)
	}
}

func TestSessionsDefaultAndAll(t *testing.T) {
	home := resumeHome(t, map[int]bool{50: true})
	a := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	j := openJ(t, home)
	for _, e := range []journal.Entry{
		{PID: 21, PPID: 20, Started: lostAt.Add(-3 * time.Hour), Dir: "/Users/alice/x", Native: "nx", Ended: lostAt.Add(-3 * time.Hour), Outcome: journal.OutcomeExited},
		{PID: 22, PPID: 20, Started: lostAt.Add(-2 * time.Hour), Dir: "/Users/alice/r", Native: "nr", Ended: lostAt.Add(-2 * time.Hour), Outcome: journal.OutcomeLost, Resumed: lostAt.Add(-time.Hour)},
		{PID: 50, PPID: 49, Started: lostAt.Add(time.Minute), Dir: "/Users/alice/run"},
	} {
		if err := j.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := runChottag(t, "sessions", "--json")
	if code != exit.OK {
		t.Fatalf("sessions = %d", code)
	}
	decodeOneDocument(t, out)
	var res sessionsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Command != journal.Command(a) || res.Sessions[0].Outcome != "lost" || res.Sessions[0].Ended == nil {
		t.Fatalf("default rows %+v", res.Sessions)
	}
	code, text, _ := runChottag(t, "sessions")
	if code != exit.OK || !strings.Contains(text, "DIR") || !strings.Contains(text, "COMMAND") || !strings.Contains(text, journal.Command(a)) {
		t.Fatalf("text %q", text)
	}

	_, out, _ = runChottag(t, "sessions", "--all", "--json")
	res = sessionsResult{}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, r := range res.Sessions {
		dirs = append(dirs, r.Dir)
	}
	if want := []string{"/Users/alice/run", "/Users/alice/a", "/Users/alice/r", "/Users/alice/x"}; !reflect.DeepEqual(dirs, want) {
		t.Fatalf("--all dirs %v, want %v", dirs, want)
	}
	_, text, _ = runChottag(t, "sessions", "--all")
	for _, s := range []string{"running", "lost", "resumed", "exited"} {
		if !strings.Contains(text, s) {
			t.Errorf("--all text lacks %q:\n%s", s, text)
		}
	}
	if code, out, _ := runChottag(t, "sessions", "extra"); code != exit.Usage {
		t.Errorf("positional = %d %q", code, out)
	}
}

func TestSessionsEmpty(t *testing.T) {
	resumeHome(t, nil)
	code, out, _ := runChottag(t, "sessions")
	if code != exit.OK || out != "no lost sessions\n" {
		t.Fatalf("sessions = %d %q", code, out)
	}
	_, out, _ = runChottag(t, "sessions", "--json")
	var res sessionsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Sessions == nil || len(res.Sessions) != 0 {
		t.Fatalf("empty JSON %q (%v)", out, err)
	}
}

func TestStatusLostHint(t *testing.T) {
	home, _ := seedTwoAccounts(t)
	stubAlive(t, nil)
	code, out, errs := runChottag(t, "status")
	if code != exit.OK || strings.Contains(out, "were lost") {
		t.Fatalf("status without a batch = %d %q %q", code, out, errs)
	}
	_, jout, _ := runChottag(t, "status", "--json")
	if strings.Contains(jout, "lostSessions") {
		t.Errorf("lostSessions present with none:\n%s", jout)
	}
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	putLost(t, home, 12, 5, "WS-1", "S-VIM", "/Users/alice/b")
	_, out, _ = runChottag(t, "status")
	if !strings.Contains(out, "2 sessions were lost at "+lostAt.Local().Format("15:04")+": chottag resume") {
		t.Errorf("status lacks the hint:\n%s", out)
	}
	_, jout, _ = runChottag(t, "status", "--json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(jout), &doc); err != nil || doc["lostSessions"] != float64(2) {
		t.Errorf("lostSessions = %v (%v)", doc["lostSessions"], err)
	}
}

func TestResumeNeverSendsAnUnsafeNative(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	bad := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	// Planted straight into the file: the ingestion check is bypassed.
	bad.Native = "x; touch /tmp/p"
	if err := openJ(t, home).Update(bad.Key(), func(e *journal.Entry) bool { e.Native = bad.Native; return true }); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := runChottag(t, "resume"); !strings.Contains(out, "no lost sessions") {
		t.Errorf("an unsafe entry reached the batch: %q", out)
	}
	rows := placeAll([]journal.Entry{bad}, &cmuxctl.Client{Bin: "x", Run: cmuxRun}, cmuxctl.Snapshot{}, "", false, true, func(journal.Entry) { t.Error("marked") })
	if len(rows) != 1 || rows[0].Placement != "failed" || rows[0].Error != "unsafe session id" || rows[0].Command != "" {
		t.Fatalf("rows %+v", rows)
	}
	for _, c := range fc.calls {
		for _, a := range c {
			if strings.Contains(a, "touch") {
				t.Errorf("unsafe id reached cmux argv %q", c)
			}
		}
	}
}

func TestSessionsAllShowsNoCommandForARunningNative(t *testing.T) {
	home := resumeHome(t, map[int]bool{99: true})
	e := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	if err := openJ(t, home).Put(journal.Entry{PID: 99, PPID: 98, Started: lostAt, Dir: "/Users/alice/a", ResumeOf: e.Native}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runChottag(t, "sessions", "--all", "--json")
	var res sessionsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Sessions {
		if r.Command != "" {
			t.Errorf("row %+v has a command", r)
		}
	}
}

func TestResumeCmuxErrorPrintsAndMarksNothing(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	fc.topFail = true
	a := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	code, out, errs := runChottag(t, "resume")
	if code != exit.OK || strings.TrimSpace(out) != journal.Command(a) || !strings.Contains(errs, "cmux top") {
		t.Fatalf("resume = %d %q %q", code, out, errs)
	}
	code, out, _ = runChottag(t, "resume")
	if code != exit.OK || strings.TrimSpace(out) != journal.Command(a) {
		t.Fatalf("the batch was lost after a cmux error: %d %q", code, out)
	}
}

func putLostAgo(t *testing.T, home string, pid int, ago time.Duration, dir string) {
	t.Helper()
	e := journal.Entry{PID: pid, PPID: pid - 1, Started: time.Now().Add(-ago - time.Hour), SID: "sid", Pool: "default", Dir: dir,
		Native: testNative(dir[strings.LastIndex(dir, "/")+1:]), Ended: time.Now().Add(-ago), Outcome: journal.OutcomeLost}
	if err := openJ(t, home).Put(e); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyRecentLossesAreInTheDefaultBatchAndHint(t *testing.T) {
	home, _ := seedTwoAccounts(t)
	stubAlive(t, nil)
	putLostAgo(t, home, 11, 25*time.Hour, "/Users/alice/old")
	_, out, _ := runChottag(t, "sessions")
	if strings.Contains(out, "/Users/alice/old") {
		t.Errorf("a 25h-old loss is in the default batch:\n%s", out)
	}
	_, out, _ = runChottag(t, "sessions", "--all")
	if !strings.Contains(out, "/Users/alice/old") {
		t.Errorf("--all lacks the old loss:\n%s", out)
	}
	_, out, _ = runChottag(t, "status")
	if strings.Contains(out, "were lost") || strings.Contains(out, "was lost") {
		t.Errorf("status hints at a 25h-old loss:\n%s", out)
	}
	if code, out, _ := runChottag(t, "resume", "--print"); code != exit.OK || !strings.Contains(out, "no lost sessions") {
		t.Errorf("resume --print = %d %q", code, out)
	}
	putLostAgo(t, home, 13, 23*time.Hour, "/Users/alice/fresh")
	_, out, _ = runChottag(t, "sessions")
	if !strings.Contains(out, "/Users/alice/fresh") {
		t.Errorf("a 23h-old loss is not in the batch:\n%s", out)
	}
	_, jout, _ := runChottag(t, "status", "--json")
	if !strings.Contains(jout, `"lostSessions": 1`) && !strings.Contains(jout, `"lostSessions":1`) {
		t.Errorf("status --json lacks lostSessions 1:\n%s", jout)
	}
}

func TestResumeUsesTheCallersOwnBusyTab(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	t.Setenv("CMUX_SURFACE_ID", "S-VIM") // busy, but it is where chottag itself runs
	putLost(t, home, 12, 0, "WS-1", "S-VIM", "/Users/alice/b")
	code, out, errs := runChottag(t, "resume", "--json")
	if code != exit.OK {
		t.Fatalf("resume = %d %q %q", code, out, errs)
	}
	if got := placements(resumeDoc(t, out)); !reflect.DeepEqual(got, []string{"tab"}) {
		t.Fatalf("placements %v, want [tab]", got)
	}
	if s := fc.sends(); len(s) != 1 || s[0][4] != "S-VIM" {
		t.Fatalf("sends %q", s)
	}
}

func TestResumeRefusesWhileAnotherRunHoldsTheLock(t *testing.T) {
	home := resumeHome(t, nil)
	installCmux(t)
	putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	unlock, ok, err := fsutil.TryLock(filepath.Join(home, "sessions", "resume.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v %v", ok, err)
	}
	code, out, _ := runChottag(t, "resume", "--json")
	doc := decodeOneDocument(t, out)
	e, _ := doc["error"].(map[string]any)
	if code != exit.Error || e["code"] != "resume_busy" || !strings.Contains(e["message"].(string), "another chottag resume is running") {
		t.Fatalf("resume = %d %v", code, doc)
	}
	_ = unlock()
	if code, out, _ := runChottag(t, "resume", "--json"); code != exit.OK {
		t.Fatalf("after unlock: %d %q", code, out)
	}
}

func TestSessionsTitleColumnAndJSON(t *testing.T) {
	home := resumeHome(t, nil)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	custom := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/custom")
	ai := putLost(t, home, 12, 1, "WS-1", "S-IDLE", "/Users/alice/ai")
	none := putLost(t, home, 13, 2, "WS-1", "S-IDLE", "/Users/alice/none")
	put := func(e journal.Entry, body string) {
		writeTranscript(t, filepath.Join(cfg, "projects", "-Users-alice", e.ResumeID()+".jsonl"), body)
	}
	put(custom, `{"type":"ai-title","aiTitle":"Generated"}`+"\n"+`{"type":"custom-title","customTitle":"Acme release"}`+"\n")
	put(ai, `{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n")
	_ = none

	_, out, _ := runChottag(t, "sessions", "--json")
	decodeOneDocument(t, out)
	var res sessionsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res.Sessions {
		got[r.Dir] = r.Title
	}
	want := map[string]string{"/Users/alice/custom": "Acme release", "/Users/alice/ai": "Fix Acme login", "/Users/alice/none": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("titles %v, want %v", got, want)
	}
	if strings.Contains(out, `"title":""`) {
		t.Errorf("empty title not omitted: %s", out)
	}
	_, text, _ := runChottag(t, "sessions")
	if !strings.Contains(text, "TITLE") || !strings.Contains(text, "Acme release") || !strings.Contains(text, "Fix Acme login") {
		t.Errorf("text lacks the title column:\n%s", text)
	}
}

func TestResumePickShowsTitleAndJSONHasIt(t *testing.T) {
	home := resumeHome(t, nil)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	installCmux(t)
	e := putLost(t, home, 11, 0, "", "", "/Users/alice/a")
	writeTranscript(t, filepath.Join(cfg, "projects", "-Users-alice", e.ResumeID()+".jsonl"), `{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n")
	_, out, _ := runChottag(t, "resume", "--print", "--json")
	if res := resumeDoc(t, out); len(res.Sessions) != 1 || res.Sessions[0].Title != "Fix Acme login" {
		t.Errorf("json %s", out)
	}
	var sb, errb bytes.Buffer
	r := newReporter(false, &sb, &errb)
	pickSessions([]journal.Entry{e}, cmuxctl.Snapshot{}, strings.NewReader("\n"), r)
	if !strings.Contains(sb.String(), "Fix Acme login") {
		t.Errorf("pick lacks the title:\n%s", sb.String())
	}
}

func TestResumeNamesEachSessionAndItsRemoteControl(t *testing.T) {
	home := resumeHome(t, nil)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	f := installCmux(t)
	rc := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/rc")
	plain := putLost(t, home, 12, 1, "WS-1", "S-GONE", "/Users/alice/plain")
	bare := putLost(t, home, 13, 2, "", "", "/Users/alice/bare")
	put := func(e journal.Entry, body string) {
		writeTranscript(t, filepath.Join(cfg, "projects", "-Users-alice", e.ResumeID()+".jsonl"), body)
	}
	put(rc, `{"type":"custom-title","customTitle":"Acme release · rc 10:00"}`+"\n"+`{"type":"bridge-session","sessionId":"x"}`+"\n")
	put(plain, `{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n")
	// bare has the bridge record but no title: no name to give.
	put(bare, `{"type":"bridge-session","sessionId":"x"}`+"\n")

	code, out, errs := runChottag(t, "resume")
	if code != exit.OK {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	for _, want := range []string{
		`resumed "Acme release · rc 10:00" (/Users/alice/rc) in its tab`,
		`resumed "Fix Acme login" (/Users/alice/plain) in a new tab`,
		"cd '/Users/alice/bare' && claude --resume " + bare.ResumeID() + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	sends := f.sends()
	if len(sends) != 2 {
		t.Fatalf("sends %q", sends)
	}
	wantRC := journal.CommandNamed(rc, "Acme release · rc 10:00") + "\n"
	if got := sends[0][len(sends[0])-1]; got != wantRC {
		t.Errorf("rc session sent %q, want %q", got, wantRC)
	}
	if got := sends[1][len(sends[1])-1]; got != journal.Command(plain)+"\n" {
		t.Errorf("a session without Remote Control gets no flag: %q", got)
	}
}

func TestSessionsCommandNamesRemoteControl(t *testing.T) {
	home := resumeHome(t, nil)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	installCmux(t)
	e := putLost(t, home, 11, 0, "", "", "/Users/alice/rc")
	writeTranscript(t, filepath.Join(cfg, "projects", "-Users-alice", e.ResumeID()+".jsonl"),
		`{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n"+`{"type":"bridge-session","sessionId":"x"}`+"\n")
	_, out, _ := runChottag(t, "sessions", "--json")
	var res sessionsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Command != journal.CommandNamed(e, "Fix Acme login") {
		t.Errorf("sessions command %s", out)
	}
	_, printed, _ := runChottag(t, "resume", "--print")
	if strings.TrimSpace(printed) != journal.CommandNamed(e, "Fix Acme login") {
		t.Errorf("--print %q", printed)
	}
}

func TestResumeSkipsAConversationWithoutATranscript(t *testing.T) {
	home := resumeHome(t, nil)
	fc := installCmux(t)
	gone := putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/gone")
	putLost(t, home, 12, 1, "WS-1", "S-VIM", "/Users/alice/kept")
	if err := os.Remove(filepath.Join(claudeConfigDir(), "projects", "-Users-alice", gone.ResumeID()+".jsonl")); err != nil {
		t.Fatal(err)
	}

	code, text, errs := runChottag(t, "resume", "--print")
	if code != exit.OK {
		t.Fatalf("resume --print = %d %q %q", code, text, errs)
	}
	if !strings.Contains(text, "skipped /Users/alice/gone: no conversation to resume\n") {
		t.Errorf("no skipped line:\n%s", text)
	}
	if strings.Contains(text, gone.ResumeID()) {
		t.Errorf("the missing conversation's command was printed:\n%s", text)
	}
	// --print marks nothing, so it is skipped again; a real run marks it,
	// so it never comes back after that.
	code, text, _ = runChottag(t, "resume", "--print")
	if code != exit.OK || !strings.Contains(text, "skipped /Users/alice/gone") {
		t.Errorf("--print marked the skipped entry: %d %q", code, text)
	}
	runChottag(t, "resume")
	code, text, _ = runChottag(t, "resume", "--print")
	if code != exit.OK || strings.Contains(text, "gone") {
		t.Errorf("after a real run = %d %q", code, text)
	}

	before := len(fc.sends())
	home2 := resumeHome(t, nil)
	putLost(t, home2, 11, 0, "WS-1", "S-IDLE", "/Users/alice/gone2")
	e2 := putLost(t, home2, 12, 1, "", "", "/Users/alice/gone3")
	_ = os.Remove(filepath.Join(claudeConfigDir(), "projects", "-Users-alice", e2.ResumeID()+".jsonl"))
	code, out, _ := runChottag(t, "resume", "--json")
	if code != exit.OK {
		t.Fatalf("a skipped row failed the command: %d %q", code, out)
	}
	res := resumeDoc(t, out)
	if got, want := placements(res), []string{"tab", "skipped"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("placements %v, want %v", got, want)
	}
	if r := res.Sessions[1]; r.Error != "no conversation to resume" || r.Command != "" {
		t.Errorf("skipped row %+v", r)
	}
	if n := len(fc.sends()) - before; n != 1 {
		t.Errorf("sends %d, want 1", n)
	}
}

func TestSessionsMarksAMissingConversation(t *testing.T) {
	home := resumeHome(t, nil)
	gone := putLost(t, home, 11, 0, "", "", "/Users/alice/gone")
	putLost(t, home, 12, 1, "", "", "/Users/alice/kept")
	if err := os.Remove(filepath.Join(claudeConfigDir(), "projects", "-Users-alice", gone.ResumeID()+".jsonl")); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runChottag(t, "sessions", "--json")
	var raw struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw.Sessions) != 2 {
		t.Fatalf("sessions %q (%v)", out, err)
	}
	if m := raw.Sessions[0]; m["missing"] != true || m["command"] != nil || m["outcome"] != "lost" {
		t.Errorf("missing row %v", m)
	}
	if m := raw.Sessions[1]; m["missing"] != nil || m["command"] == nil {
		t.Errorf("kept row %v", m)
	}
	_, text, _ := runChottag(t, "sessions")
	if !strings.Contains(text, "lost") || strings.Contains(text, gone.ResumeID()) {
		t.Errorf("text %q", text)
	}
}

// unrecordedSetup makes a lost batch of one and the transcripts around it.
func unrecordedSetup(t *testing.T) (home string, in string, lost journal.Entry) {
	t.Helper()
	home = resumeHome(t, nil)
	installCmux(t)
	lost = putLost(t, home, 11, 0, "WS-1", "S-IDLE", "/Users/alice/a")
	proj := filepath.Join(claudeConfigDir(), "projects", "-Users-alice")
	at := func(name string, off time.Duration, body string) string {
		id := testNative(name)
		p := filepath.Join(proj, id+".jsonl")
		writeTranscript(t, p, body)
		if err := os.Chtimes(p, lostAt.Add(off), lostAt.Add(off)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	cwd := func(d string) string { return `{"type":"user","cwd":"` + d + `"}` + "\n" }
	in = at("unrec", -10*time.Minute, `{"type":"ai-title","aiTitle":"Fix Acme login"}`+"\n"+cwd("/Users/alice/it's"))
	at("old", -3*time.Hour, cwd("/Users/alice/old"))
	at("late", 5*time.Minute, cwd("/Users/alice/late"))
	at("nocwd", -time.Minute, `{"type":"user"}`+"\n")
	at("relcwd", -time.Minute, cwd("rel/dir"))
	// A journaled conversation (by ResumeOf) is not unrecorded.
	of := at("ofres", -time.Minute, cwd("/Users/alice/of"))
	if err := openJ(t, home).Put(journal.Entry{PID: 60, PPID: 59, Started: lostAt.Add(-time.Hour), Dir: "/Users/alice/of",
		Native: testNative("other"), ResumeOf: of, Ended: lostAt.Add(-time.Hour), Outcome: journal.OutcomeExited}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Join(proj, "not-a-uuid.jsonl"), cwd("/Users/alice/x"))
	writeTranscript(t, filepath.Join(proj, testNative("sub"), "sub.jsonl"), cwd("/Users/alice/sub"))
	return home, in, lost
}

func TestSessionsListsUnrecordedConversations(t *testing.T) {
	_, id, _ := unrecordedSetup(t)
	_, out, _ := runChottag(t, "sessions", "--json")
	var res sessionsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Unrecorded) != 1 {
		t.Fatalf("unrecorded %+v", res.Unrecorded)
	}
	u := res.Unrecorded[0]
	if u.Dir != "/Users/alice/it's" || u.Native != id || u.Title != "Fix Acme login" || !u.Modified.Equal(lostAt.Add(-10*time.Minute)) ||
		u.Command != `cd '/Users/alice/it'\''s' && claude --resume `+id {
		t.Errorf("row %+v", u)
	}
	_, text, _ := runChottag(t, "sessions")
	if !strings.Contains(text, "unrecorded") || !strings.Contains(text, u.Command) {
		t.Errorf("text lacks the unrecorded row:\n%s", text)
	}
}

func TestSessionsUnrecordedIsEmptyWithoutABatch(t *testing.T) {
	resumeHome(t, nil)
	_, out, _ := runChottag(t, "sessions", "--json")
	if !strings.Contains(out, `"unrecorded": []`) && !strings.Contains(out, `"unrecorded":[]`) {
		t.Errorf("no empty unrecorded array: %s", out)
	}
}

func TestResumeHintsAtUnrecordedConversations(t *testing.T) {
	_, id, _ := unrecordedSetup(t)
	fc := installCmux(t)
	code, text, _ := runChottag(t, "resume")
	if code != exit.OK {
		t.Fatalf("resume = %d %q", code, text)
	}
	const hint = "1 more conversation was active before the loss but not launched through chottag: chottag sessions lists its command\n"
	if !strings.HasSuffix(text, hint) {
		t.Errorf("hint missing:\n%s", text)
	}
	if strings.Contains(text, id) || len(fc.sends()) != 1 {
		t.Errorf("an unrecorded conversation was placed: %q", fc.sends())
	}
}

func TestResumeJSONCountsUnrecorded(t *testing.T) {
	unrecordedSetup(t)
	_, out, _ := runChottag(t, "resume", "--json", "--print")
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil || raw["unrecorded"] != float64(1) {
		t.Errorf("unrecorded = %v (%v) in %s", raw["unrecorded"], err, out)
	}
}

func TestUnrecordedCapsItsCandidates(t *testing.T) {
	home := resumeHome(t, nil)
	lost := putLost(t, home, 11, 0, "", "", "/Users/alice/a")
	for i := 0; i < 60; i++ {
		id := testNative(fmt.Sprintf("c%02d", i))
		p := filepath.Join(claudeConfigDir(), "projects", "-p", id+".jsonl")
		writeTranscript(t, p, `{"cwd":"/Users/alice/p"}`+"\n")
		mt := lostAt.Add(-time.Duration(i) * time.Second)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	got := unrecordedConversations(claudeConfigDir(), []journal.Entry{lost}, []journal.Entry{lost})
	if len(got) != 50 || got[0].Native != testNative("c00") {
		t.Errorf("got %d rows, first %v", len(got), got[0].Native)
	}
}
