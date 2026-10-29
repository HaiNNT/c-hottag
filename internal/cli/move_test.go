package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

const (
	moveSidA = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	moveSidB = "a1a1b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	moveSidC = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	movePort = 47851
)

// moveHome is a sandbox with accounts A, B and C under spread, registry
// entries for the given sids, and no daemon (the probe says down).
func moveHome(t *testing.T, sids ...string) (string, store.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	t.Setenv("HTTPS_PROXY", "")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		st.Port = movePort
		for _, n := range []string{"A", "B", "C"} {
			if err := st.Add(store.Account{Name: n, Dir: filepath.Join(home, "accounts", n)}); err != nil {
				return err
			}
		}
		st.SetPolicy(store.PolicySpread)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, sid := range sids {
		moveEntry(t, home, fmt.Sprint(i+1), session.Session{Port: movePort, SID: sid, Pool: "default", Started: time.Now()})
	}
	restore := SetStatusProbeForTest(func(int) (bool, string) { return false, "" })
	t.Cleanup(restore)
	// No ancestor walk reaches a real ps.
	t.Cleanup(SetParentPIDForTest(func(int) (int, error) { return 0, fmt.Errorf("no ancestor") }))
	return home, s
}

func moveEntry(t *testing.T, home, name string, s session.Session) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.PID = os.Getpid()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "run", name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func moveCache(t *testing.T, home string, f status.File) {
	t.Helper()
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

func moveInside(t *testing.T, sid string) {
	t.Helper()
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag.default.%s:secret@127.0.0.1:%d", sid, movePort))
}

func moveDoc(t *testing.T, args ...string) (int, map[string]any, string) {
	t.Helper()
	code, out, errs := runChottag(t, append([]string{"--json"}, args...)...)
	return code, decodeOneDocument(t, out), errs
}

func TestMoveFromInsideSession(t *testing.T) {
	_, s := moveHome(t, moveSidA)
	moveInside(t, moveSidA)
	code, doc, errs := moveDoc(t, "move", "B")
	if code != exit.OK {
		t.Fatalf("exit %d %v %q", code, doc, errs)
	}
	if doc["session"] != moveSidA || doc["account"] != "B" || doc["from"] != "" {
		t.Fatalf("doc = %v", doc)
	}
	st, _ := s.Load()
	m, ok := st.Moves[moveSidA]
	if !ok || m.Account != "B" || m.Dir == "" || m.At.IsZero() {
		t.Fatalf("moves = %+v", st.Moves)
	}
}

func TestMoveTextLine(t *testing.T) {
	home, _ := moveHome(t, moveSidA)
	moveCache(t, home, status.File{Sessions: []sessions.Activity{{SID: moveSidA, Account: "A", LastSeen: time.Now()}}})
	moveInside(t, moveSidA)
	code, out, _ := runChottag(t, "move", "B")
	want := "moved session a1a1a1a1 to B (from A); it switches at its next request, and its prompt cache starts cold on B\n"
	if code != exit.OK || out != want {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestMoveFromPrefersPlacedOverLastUsed(t *testing.T) {
	home, _ := moveHome(t, moveSidA)
	moveCache(t, home, status.File{Sessions: []sessions.Activity{{SID: moveSidA, Account: "A", Placed: "C", LastSeen: time.Now()}}})
	moveInside(t, moveSidA)
	_, doc, _ := moveDoc(t, "move", "B")
	if doc["from"] != "C" {
		t.Fatalf("doc = %v", doc)
	}
}

func TestMoveBySessionPrefix(t *testing.T) {
	_, s := moveHome(t, moveSidA, moveSidC)
	code, doc, errs := moveDoc(t, "move", "B", "--session", "c3c3")
	if code != exit.OK || doc["session"] != moveSidC {
		t.Fatalf("exit %d %v %q", code, doc, errs)
	}
	if st, _ := s.Load(); st.Moves[moveSidC].Account != "B" || len(st.Moves) != 1 {
		t.Fatalf("moves = %+v", st.Moves)
	}
}

func TestMoveSessionAmbiguous(t *testing.T) {
	moveHome(t, moveSidA, moveSidB)
	code, doc, _ := moveDoc(t, "move", "B", "--session", "a1a1")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "session_ambiguous")
}

func TestMoveSessionTooShort(t *testing.T) {
	moveHome(t, moveSidA)
	code, doc, _ := moveDoc(t, "move", "B", "--session", "a1a")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "usage")
}

func TestMoveNoSession(t *testing.T) {
	moveHome(t, moveSidA)
	code, doc, _ := moveDoc(t, "move", "B")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "no_session")
	code, doc, _ = moveDoc(t, "move", "B", "--session", "ffff")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "no_session")
}

func TestMoveOtherHomesPort(t *testing.T) {
	moveHome(t)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag.default.%s:secret@127.0.0.1:%d", moveSidA, movePort+1))
	code, doc, _ := moveDoc(t, "move", "B")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "no_session")
}

func TestMoveSerialRefused(t *testing.T) {
	_, s := moveHome(t, moveSidA)
	if _, err := s.Update(func(st *store.State) error { st.SetPolicy("serial"); return nil }); err != nil {
		t.Fatal(err)
	}
	moveInside(t, moveSidA)
	code, doc, _ := moveDoc(t, "move", "B")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "move_serial")
	if st, _ := s.Load(); len(st.Moves) != 0 {
		t.Fatalf("wrote moves %+v", st.Moves)
	}
}

func TestMoveUnknownAccount(t *testing.T) {
	moveHome(t, moveSidA)
	moveInside(t, moveSidA)
	_, doc, _ := moveDoc(t, "move", "Zed")
	wantErrCode(t, doc, "unknown_account")
}

func TestMoveNotInPool(t *testing.T) {
	_, s := moveHome(t, moveSidA)
	if _, err := s.Update(func(st *store.State) error {
		if err := st.AddPool("work"); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "W", Dir: "/Users/alice/w", PoolList: []string{"work"}})
	}); err != nil {
		t.Fatal(err)
	}
	moveInside(t, moveSidA)
	code, doc, _ := moveDoc(t, "move", "W")
	if code != exit.Usage {
		t.Fatalf("exit %d", code)
	}
	wantErrCode(t, doc, "not_in_pool")
}

func TestMoveRefusedReasons(t *testing.T) {
	now := time.Now()
	pct := 99.0
	cases := []struct {
		name   string
		setup  func(st *store.State, f *status.File)
		reason string
		detail string // wanted in the message
		forced bool   // --force still refuses?
	}{
		{"out of rotation", func(st *store.State, f *status.File) {
			for i := range st.Accounts {
				if st.Accounts[i].Name == "B" {
					st.Accounts[i].NoRotate = true
				}
			}
		}, "out of rotation", "out of rotation", true},
		{"needs login", func(st *store.State, f *status.File) {
			f.Accounts = append(f.Accounts, status.Account{Name: "B", Token: creds.StateNeedsLogin, TokenAt: now})
		}, "needs login", "needs login", true},
		{"limited", func(st *store.State, f *status.File) {
			f.Accounts = append(f.Accounts, status.Account{Name: "B", Limited: true, LimitedUntil: now.Add(time.Hour)})
		}, "limited", "limited until ", true},
		{"above switch point", func(st *store.State, f *status.File) {
			f.Accounts = append(f.Accounts, status.Account{Name: "B", Usage: &status.Usage{FiveHourPct: &pct, UpdatedAt: now}})
		}, "above switch point", "above switch point (5h 99% \u2265 ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, s := moveHome(t, moveSidA)
			var f status.File
			if _, err := s.Update(func(st *store.State) error { c.setup(st, &f); return nil }); err != nil {
				t.Fatal(err)
			}
			moveCache(t, home, f)
			moveInside(t, moveSidA)
			code, doc, _ := moveDoc(t, "move", "B")
			if code != exit.Usage {
				t.Fatalf("exit %d %v", code, doc)
			}
			wantErrCode(t, doc, "move_refused")
			if got := docError(t, doc)["reason"]; got != c.reason {
				t.Fatalf("reason: %v", docError(t, doc))
			}
			if msg, _ := docError(t, doc)["message"].(string); !strings.Contains(msg, c.detail) {
				t.Fatalf("message %q lacks %q", msg, c.detail)
			}
			if st, _ := s.Load(); len(st.Moves) != 0 {
				t.Fatalf("a refused move was written: %+v", st.Moves)
			}
			code, doc, _ = moveDoc(t, "move", "B", "--force")
			if c.forced {
				if code != exit.Usage {
					t.Fatalf("--force exit %d %v", code, doc)
				}
				wantErrCode(t, doc, "move_refused")
				return
			}
			if code != exit.OK {
				t.Fatalf("--force exit %d %v", code, doc)
			}
			if st, _ := s.Load(); st.Moves[moveSidA].Account != "B" {
				t.Fatalf("moves = %+v", st.Moves)
			}
		})
	}
}

func TestMoveAlreadyThere(t *testing.T) {
	home, s := moveHome(t, moveSidA)
	moveCache(t, home, status.File{Sessions: []sessions.Activity{{SID: moveSidA, Account: "A", Placed: "B", LastSeen: time.Now()}}})
	moveInside(t, moveSidA)
	code, out, _ := runChottag(t, "move", "B")
	if code != exit.OK || !strings.Contains(out, "already on B") {
		t.Fatalf("exit %d out %q", code, out)
	}
	if st, _ := s.Load(); st.Moves[moveSidA].Account != "B" {
		t.Fatalf("moves = %+v", st.Moves)
	}
}

func TestMovePrunesDeadEntries(t *testing.T) {
	_, s := moveHome(t, moveSidA)
	if _, err := s.Update(func(st *store.State) error {
		st.SetMove(moveSidC, store.Move{Account: "A", At: time.Now()}) // not live
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	moveInside(t, moveSidA)
	if code, _, _ := runChottag(t, "move", "B"); code != exit.OK {
		t.Fatalf("exit %d", code)
	}
	st, _ := s.Load()
	if _, ok := st.Moves[moveSidC]; ok || len(st.Moves) != 1 {
		t.Fatalf("moves = %+v", st.Moves)
	}
}

func TestMoveWarnsNoDaemon(t *testing.T) {
	moveHome(t, moveSidA)
	moveInside(t, moveSidA)
	code, out, errs := runChottag(t, "--json", "move", "B")
	if code != exit.OK || !strings.Contains(out, "daemon_not_running") || !strings.Contains(errs, "not running") {
		t.Fatalf("exit %d out %s err %q", code, out, errs)
	}
}

func TestMoveWarnsOldDaemon(t *testing.T) {
	moveHome(t, moveSidA)
	moveInside(t, moveSidA)
	orig := Version
	Version = "0.10.10"
	t.Cleanup(func() { Version = orig })
	cases := []struct {
		ver  string
		want bool
	}{{"0.10.9", true}, {"weird", true}, {"0.10.10", false}, {"0.11.0", false}}
	for _, c := range cases {
		restore := SetStatusProbeForTest(func(int) (bool, string) { return true, c.ver })
		_, out, errs := runChottag(t, "--json", "move", "B")
		restore()
		if got := strings.Contains(out, "daemon_predates_move"); got != c.want {
			t.Fatalf("daemon %s: warned = %v: %s", c.ver, got, out)
		}
		if strings.Contains(out, "daemon_not_running") {
			t.Fatalf("daemon %s is up: %s", c.ver, out)
		}
		if c.want && !strings.Contains(errs, "chottag daemon restart") {
			t.Fatalf("no restart advice: %q", errs)
		}
	}
}

func TestStatusShowsChosen(t *testing.T) {
	home, _ := moveHome(t, moveSidA, moveSidC)
	moveCache(t, home, status.File{Sessions: []sessions.Activity{
		{SID: moveSidA, Pool: "default", Account: "A", Placed: "B", Chosen: true, LastSeen: time.Now()},
		{SID: moveSidC, Pool: "default", Account: "A", LastSeen: time.Now()},
	}})
	_, out, _ := runChottag(t, "--json", "status")
	doc := decodeOneDocument(t, out)
	rows, _ := doc["sessions"].([]any)
	chosen := 0
	for _, r := range rows {
		row := r.(map[string]any)
		if row["sid"] == moveSidA[:8] {
			if row["chosen"] != true {
				t.Fatalf("row = %v", row)
			}
			chosen++
		} else if _, has := row["chosen"]; has {
			t.Fatalf("an unchosen row has chosen: %v", row)
		}
	}
	if chosen != 1 {
		t.Fatalf("rows = %v", rows)
	}
	_, text, _ := runChottag(t, "status")
	if !strings.Contains(text, "B 1 (1 chosen)") {
		t.Fatalf("text lacks the chosen marker:\n%s", text)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "move puts a session on an account", command: "move",
			setup: func(t *testing.T) []string {
				moveHome(t, moveSidA)
				moveInside(t, moveSidA)
				return []string{"move", "B"}
			},
			wantExit: exit.OK,
		},
		jsonCase{
			name: "move outside a session", command: "move",
			setup:    func(t *testing.T) []string { moveHome(t, moveSidA); return []string{"move", "B"} },
			wantExit: exit.Usage, wantCode: codeNoSession,
		},
	)
}

// The message names the window Eligible refused on: a 5h reading whose reset
// has passed is effectively 0%, so the 7d window is the one over its point.
func TestMoveRefusalNamesTheWindowEligibleUsed(t *testing.T) {
	home, _ := moveHome(t, moveSidA)
	now := time.Now()
	p5, p7 := 97.0, 99.0
	moveCache(t, home, status.File{Accounts: []status.Account{{Name: "B", Usage: &status.Usage{
		FiveHourPct: &p5, FiveHourResetsAt: now.Add(-time.Minute),
		SevenDayPct: &p7, SevenDayResetsAt: now.Add(time.Hour), UpdatedAt: now}}}})
	moveInside(t, moveSidA)
	code, doc, _ := moveDoc(t, "move", "B")
	if code != exit.Usage {
		t.Fatalf("exit %d %v", code, doc)
	}
	msg, _ := docError(t, doc)["message"].(string)
	if !strings.Contains(msg, "above switch point (7d 99% \u2265 ") || strings.Contains(msg, "5h") {
		t.Fatalf("message = %q", msg)
	}
}
