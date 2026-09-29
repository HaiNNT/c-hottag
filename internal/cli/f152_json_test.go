package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// F152: setup and logout --force report what their inner steps did.

func TestReporterNestedOKRecordsItsResult(t *testing.T) {
	var out bytes.Buffer
	r := newReporter(true, &out, io.Discard)
	n := r.Nested()
	n.OK(reportSample{"B", "A"})
	if got, ok := n.result.(reportSample); !ok || got != (reportSample{"B", "A"}) {
		t.Fatalf("nested result = %#v, want the fields OK was given", n.result)
	}
	if out.Len() != 0 {
		t.Fatalf("a nested OK wrote %q", out.String())
	}
	if r.result != nil {
		t.Fatalf("a top-level reporter recorded %#v; only a nested one records", r.result)
	}
}

func TestReporterRecordWarnPrintsNothing(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		var out, errb bytes.Buffer
		r := newReporter(jsonMode, &out, &errb)
		r.RecordWarn(warnAdoptFailed, "adopt failed: state.json is corrupt")
		r.OK(nil)
		if errb.Len() != 0 {
			t.Errorf("json=%v: stderr = %q, want nothing (the nested command printed it already)", jsonMode, errb.String())
		}
		if !jsonMode {
			if out.Len() != 0 {
				t.Errorf("text mode stdout = %q, want nothing", out.String())
			}
			continue
		}
		ws, _ := decodeReportDoc(t, out.String())["warnings"].([]any)
		if len(ws) != 1 || ws[0].(map[string]any)["code"] != "adopt_failed" || ws[0].(map[string]any)["message"] != "adopt failed: state.json is corrupt" {
			t.Errorf("warnings = %v, want the one adopt_failed", ws)
		}
	}
}

func TestSetupJSONCarriesAdoptsResult(t *testing.T) {
	home, _ := setupJSONEnv(t, "/bin/zsh")
	if err := os.MkdirAll(filepath.Join(home, "accounts", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"a@example.com"}`)
	code, out, errb := runChottag(t, "setup", "--claude", fake, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	var doc struct {
		Warnings []warning    `json:"warnings"`
		Adopt    *adoptResult `json:"adopt"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	want := adoptResult{
		Adopted: []adoptEntry{{Dir: filepath.Join(home, "accounts", "A"), Name: "A"}},
		Updated: []adoptEntry{}, Skipped: []adoptSkip{},
	}
	if doc.Adopt == nil || len(doc.Adopt.Adopted) != 1 || doc.Adopt.Adopted[0] != want.Adopted[0] ||
		doc.Adopt.Updated == nil || len(doc.Adopt.Updated) != 0 || doc.Adopt.Skipped == nil || len(doc.Adopt.Skipped) != 0 {
		t.Fatalf("adopt = %+v, want %+v (arrays present, never null)", doc.Adopt, want)
	}
	if len(doc.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for a clean adopt", doc.Warnings)
	}
}

// adopt's own failure under an ok setup: the stderr text as before, once,
// and under --json an adopt_failed warning and no adopt object.
func TestSetupReportsAnAdoptFailureAsAWarning(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		home, _ := setupJSONEnv(t, "/bin/zsh")
		if err := os.WriteFile(filepath.Join(home, "state.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"setup"}
		if jsonMode {
			args = append(args, "--json")
		}
		code, out, errb := runChottag(t, args...)
		if code != 0 {
			t.Fatalf("json=%v: exit = %d, want 0 (an adopt failure is not a setup failure); stderr=%q", jsonMode, code, errb)
		}
		if n := strings.Count(errb, "state.json is corrupt"); n != 1 {
			t.Errorf("json=%v: stderr = %q, want adopt's error exactly once", jsonMode, errb)
		}
		if !jsonMode {
			if strings.Contains(out+errb, "adopt failed") {
				t.Errorf("text mode gained a line: stdout=%q stderr=%q", out, errb)
			}
			continue
		}
		doc := decodeOneDocument(t, out)
		if _, ok := doc["adopt"]; ok {
			t.Errorf("doc = %v: a failed adopt has no result", doc)
		}
		ws, _ := doc["warnings"].([]any)
		if len(ws) != 1 || ws[0].(map[string]any)["code"] != "adopt_failed" ||
			!strings.Contains(ws[0].(map[string]any)["message"].(string), "state.json is corrupt") {
			t.Errorf("warnings = %v, want one adopt_failed naming adopt's error", ws)
		}
	}
}

// seedRoles registers A, B and C with the given serving and remote, and
// stubs the revoke and the credential delete.
func seedRoles(t *testing.T, serving, remote string, noRotate ...string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		for _, n := range []string{"A", "B", "C"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: n + "@example.com", Dir: dir}); err != nil {
				return err
			}
		}
		for i := range st.Accounts {
			for _, n := range noRotate {
				if st.Accounts[i].Name == n {
					st.Accounts[i].NoRotate = true
				}
			}
		}
		st.Serving, st.Remote = serving, remote
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stubAuthExec(t, func(string, string, string) error { return nil })
	stubCredsDelete(t, nil)
}

func TestLogoutForceReportsWhereTheRolesMoved(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		serving, remote      string
		noRotate             []string
		args                 []string
		wantServing, wantRem string
		wantLine             string
	}{
		{"both roles", "A", "A", nil, []string{"logout", "--yes", "--force", "A"}, "B", "B", "moved serving to B, remote to B\n"},
		{"remote only goes to serving", "B", "A", nil, []string{"logout", "--yes", "--force", "A"}, "", "B", "moved remote to B\n"},
		{"serving only", "A", "C", nil, []string{"logout", "--yes", "--force", "A"}, "B", "", "moved serving to B\n"},
		{"serving cleared, remote falls back", "A", "A", []string{"B", "C"}, []string{"logout", "--yes", "--force", "A"}, "", "B", "moved remote to B\n"},
		{"no role held", "A", "A", nil, []string{"logout", "--yes", "--force", "C"}, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedRoles(t, tc.serving, tc.remote, tc.noRotate...)
			code, out, errb := runChottag(t, tc.args...)
			if code != exit.OK {
				t.Fatalf("exit = %d, stderr=%q", code, errb)
			}
			account := tc.args[len(tc.args)-1]
			if want := tc.wantLine + "logged out: " + account + "\n"; out != want {
				t.Errorf("stdout = %q, want %q", out, want)
			}

			seedRoles(t, tc.serving, tc.remote, tc.noRotate...)
			code, out, errb = runChottag(t, append(tc.args, "--json")...)
			if code != exit.OK {
				t.Fatalf("--json exit = %d, stderr=%q", code, errb)
			}
			doc := decodeOneDocument(t, out)
			for key, want := range map[string]string{"movedServing": tc.wantServing, "movedRemote": tc.wantRem} {
				got, present := doc[key]
				if want == "" && present {
					t.Errorf("%s = %v, want it absent", key, got)
				}
				if want != "" && got != want {
					t.Errorf("%s = %v, want %s", key, got, want)
				}
			}
		})
	}
}

// Out-of-tree --force only deregisters, and still says where the roles went.
func TestLogoutForceOutOfTreeReportsTheMove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	outside := t.TempDir()
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: outside}); err != nil {
			return err
		}
		inside := filepath.Join(home, "accounts", "B")
		if err := os.MkdirAll(inside, 0o700); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: inside})
	}); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runChottag(t, "logout", "--yes", "--force", "A", "--json")
	if code != exit.OK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	if doc["removed"] != false || doc["movedServing"] != "B" || doc["movedRemote"] != "B" {
		t.Errorf("doc = %v, want removed false and both roles moved to B", doc)
	}
}
