package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// acctJSONExclude takes name out of rotation.
func acctJSONExclude(t *testing.T, s store.Store, name string) {
	t.Helper()
	if _, err := s.Update(func(st *store.State) error {
		for i := range st.Accounts {
			if st.Accounts[i].Name == name {
				st.Accounts[i].NoRotate = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// acctJSONSlot creates <home>/accounts/<name> and a fake claude whose
// `auth status --json` reports a login, for adopt.
func acctJSONSlot(t *testing.T, name, email string) (home, fake string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "accounts", name), 0o700); err != nil {
		t.Fatal(err)
	}
	return home, writeFakeClaude(t, `{"loggedIn":true,"email":"`+email+`"}`)
}

func acctJSONWant(t *testing.T, doc map[string]any, key string, want any) {
	t.Helper()
	if doc[key] != want {
		t.Errorf("%s = %v, want %v", key, doc[key], want)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "tag NAME switches", command: "tag",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"tag", "B"} },
			check: func(t *testing.T, doc map[string]any) {
				acctJSONWant(t, doc, "serving", "B")
				acctJSONWant(t, doc, "previous", "A")
			},
		},
		jsonCase{
			name: "tag an unknown account", command: "tag",
			setup:    func(t *testing.T) []string { seedTwoAccounts(t); return []string{"tag", "nope"} },
			wantExit: exit.Error, wantCode: codeUnknownAccount,
		},
		jsonCase{
			name: "next switches", command: "next",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"next"} },
			check: func(t *testing.T, doc map[string]any) {
				acctJSONWant(t, doc, "serving", "B")
				if sk, ok := doc["skipped"].([]any); !ok || len(sk) != 0 {
					t.Errorf("skipped = %v, want []", doc["skipped"])
				}
			},
		},
		jsonCase{
			name: "next with no candidate", command: "next",
			setup: func(t *testing.T) []string {
				_, s := seedTwoAccounts(t)
				acctJSONExclude(t, s, "B")
				return []string{"next"}
			},
			wantExit: exit.UserAction, wantCode: codeNoCandidate,
			check: func(t *testing.T, doc map[string]any) {
				sk, _ := docError(t, doc)["skipped"].([]any)
				if len(sk) != 1 {
					t.Fatalf("error.skipped = %v, want one entry", docError(t, doc)["skipped"])
				}
				if e := sk[0].(map[string]any); e["name"] != "B" || e["reason"] != "out_of_rotation" {
					t.Errorf("skipped[0] = %v, want B out_of_rotation", e)
				}
			},
		},
		jsonCase{
			name: "remote shows the remote account", command: "remote",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"remote"} },
			check: func(t *testing.T, doc map[string]any) {
				acctJSONWant(t, doc, "remote", "A")
				acctJSONWant(t, doc, "changed", false)
			},
		},
		jsonCase{
			name: "remote sets the remote account", command: "remote",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"remote", "B"} },
			check: func(t *testing.T, doc map[string]any) {
				acctJSONWant(t, doc, "remote", "B")
				acctJSONWant(t, doc, "changed", true)
			},
		},
		jsonCase{
			name: "remote an unknown account", command: "remote",
			setup:    func(t *testing.T) []string { seedTwoAccounts(t); return []string{"remote", "nope"} },
			wantExit: exit.Error, wantCode: codeUnknownAccount,
		},
		jsonCase{
			name: "rc is remote", command: "rc",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"rc", "B"} },
		},
		jsonCase{
			name: "remote-control is remote", command: "remote-control",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"remote-control"} },
		},
		jsonCase{
			name: "adopt registers a slot with a login", command: "adopt",
			setup: func(t *testing.T) []string {
				_, fake := acctJSONSlot(t, "A", "a@example.com")
				return []string{"adopt", "--claude", fake}
			},
			check: func(t *testing.T, doc map[string]any) {
				ad, _ := doc["adopted"].([]any)
				if len(ad) != 1 || ad[0].(map[string]any)["name"] != "A" {
					t.Errorf("adopted = %v, want [A]", doc["adopted"])
				}
				for _, k := range []string{"updated", "skipped"} {
					if a, ok := doc[k].([]any); !ok || len(a) != 0 {
						t.Errorf("%s = %v, want []", k, doc[k])
					}
				}
			},
		},
		jsonCase{
			name: "adopt with a bad flag", command: "adopt",
			setup:    func(t *testing.T) []string { acctJSONSlot(t, "A", "a@example.com"); return []string{"adopt", "--nope"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}

// Review Focus 3: `tag B --force` switches to B, with --force parsed as the
// flag rather than read as a second name, in every order and with --json
// anywhere.
func TestTagNameThenForceSwitchesToTheName(t *testing.T) {
	for _, args := range [][]string{
		{"tag", "B", "--force"},
		{"tag", "--force", "B"},
		{"--json", "tag", "B", "--force"},
		{"tag", "B", "--force", "--json"},
		{"tag", "--json", "B", "--force"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, s := seedTwoAccounts(t)
			code, out, errb := runChottag(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stdout=%q stderr=%q", code, out, errb)
			}
			st, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.Serving != "B" {
				t.Errorf("serving = %q, want B", st.Serving)
			}
		})
	}
}

// Before M1d-d a flag after NAME was never parsed at all: `tag B --bogus`
// exited 0. Now it is exit 2, and nothing switches; a second NAME is exit
// 2 as well.
func TestTagParsesFlagsAfterTheName(t *testing.T) {
	for _, args := range [][]string{{"tag", "B", "--bogus"}, {"tag", "B", "A"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, s := seedTwoAccounts(t)
			if code, _, errb := runChottag(t, args...); code != exit.Usage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Usage, errb)
			}
			st, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.Serving != "A" {
				t.Errorf("serving = %q, want A untouched", st.Serving)
			}
		})
	}
}

// next's only flag is --force, in any order, in any spelling the flag
// package accepts, and with --json anywhere; a NAME, even after --, is
// still exit 2. (M1d-c's inline check accepted only the exact token
// "--force", so --force=true is the row that tells the FlagSet apart.)
func TestNextFlagOrder(t *testing.T) {
	limitB := func(t *testing.T, home string) {
		now := time.Now()
		writeStatusFile(t, home, status.File{Accounts: []status.Account{
			{Name: "B", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: now}},
		}})
	}
	for _, args := range [][]string{
		{"next", "--force"},
		{"--json", "next", "--force"},
		{"next", "--force", "--json"},
		{"next", "--json", "--force"},
		{"next", "--force=true"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home, s := seedTwoAccounts(t)
			limitB(t, home)
			if code, out, errb := runChottag(t, args...); code != 0 {
				t.Fatalf("exit = %d, want 0 (forced past B's limit); stdout=%q stderr=%q", code, out, errb)
			}
			if st, _ := s.Load(); st.Serving != "B" {
				t.Errorf("serving = %q, want B", st.Serving)
			}
		})
	}
	for _, args := range [][]string{{"next", "B"}, {"next", "--force", "B"}, {"next", "--", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, s := seedTwoAccounts(t)
			code, _, errb := runChottag(t, args...)
			if code != exit.Usage || !strings.Contains(errb, "next takes no arguments") {
				t.Fatalf("exit = %d stderr=%q, want 2 and \"next takes no arguments\"", code, errb)
			}
			if st, _ := s.Load(); st.Serving != "A" {
				t.Errorf("serving = %q, want A untouched", st.Serving)
			}
		})
	}
}

// remote takes no flags: a stray one is exit 2, never an account name; --
// ends that rule.
func TestRemoteRejectsAStrayFlag(t *testing.T) {
	for _, args := range [][]string{{"remote", "--force"}, {"remote", "B", "--force"}, {"remote", "-x"}, {"remote", "A", "B"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, s := seedTwoAccounts(t)
			if code, _, errb := runChottag(t, args...); code != exit.Usage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Usage, errb)
			}
			if st, _ := s.Load(); st.Remote != "A" {
				t.Errorf("remote = %q, want A untouched", st.Remote)
			}
		})
	}
	_, s := seedTwoAccounts(t)
	if code, _, errb := runChottag(t, "remote", "--", "B"); code != 0 {
		t.Fatalf("remote -- B = %d, stderr=%q; want 0", code, errb)
	}
	if st, _ := s.Load(); st.Remote != "B" {
		t.Errorf("remote = %q, want B", st.Remote)
	}
}

// Fix round 4, item 5: `remote --json`'s remote field must follow the same
// omitempty convention `status --json` already uses for its own empty
// remote/serving fields — an unset remote is absent from the document, not
// present as an empty string.
func TestRemoteJSONOmitsEmptyRemoteField(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir()) // no accounts registered at all
	code, out, errb := runChottag(t, "remote", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	if v, ok := doc["remote"]; ok {
		t.Errorf("doc = %v, want no \"remote\" key for an unset remote, got %v", doc, v)
	}
}

// adopt takes no positional. Before M1d-d one silently ended flag parsing,
// so `adopt stray --claude X` ignored --claude and ran the default
// `claude`; now it is exit 2 before anything runs. accounts/ is empty, so
// even the old behaviour would exec nothing.
func TestAdoptRejectsAStrayPositional(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runChottag(t, "adopt", "stray", "--claude", filepath.Join(home, "no-such-claude"))
	if code != exit.Usage || !strings.Contains(errb, "usage: chottag adopt") {
		t.Fatalf("exit = %d stderr=%q, want 2 and adopt's usage line", code, errb)
	}
}

// adopt's flags are order-free: --name before or after --claude.
func TestAdoptFlagOrder(t *testing.T) {
	for _, order := range []string{"claude-first", "name-first"} {
		t.Run(order, func(t *testing.T) {
			home, fake := acctJSONSlot(t, "C", "c@example.com")
			args := []string{"adopt", "--claude", fake, "--name", "C=X", "--json"}
			if order == "name-first" {
				args = []string{"--json", "adopt", "--name", "C=X", "--claude", fake}
			}
			code, out, errb := runChottag(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d, stderr=%q", code, errb)
			}
			ad, _ := decodeOneDocument(t, out)["adopted"].([]any)
			if len(ad) != 1 || ad[0].(map[string]any)["name"] != "X" || ad[0].(map[string]any)["dir"] != filepath.Join(home, "accounts", "C") {
				t.Errorf("adopted = %v, want [{dir: <home>/accounts/C, name: X}]", ad)
			}
		})
	}
}

// A limited tag target is a warning, not a refusal: in JSON it lands in
// warnings with its code, and stderr keeps the same line.
func TestTagJSONCarriesTheLimitedWarning(t *testing.T) {
	home, _ := seedTwoAccounts(t)
	now := time.Now()
	writeStatusFile(t, home, status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, Usage: &status.Usage{UpdatedAt: now}},
	}})
	code, out, errb := runChottag(t, "tag", "B", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	ws, _ := decodeOneDocument(t, out)["warnings"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != "limited" || ws[0].(map[string]any)["message"] != "B is limited" {
		t.Errorf("warnings = %v, want one limited warning", ws)
	}
	if errb != "chottag: warning: B is limited\n" {
		t.Errorf("stderr = %q, want the unchanged warning line", errb)
	}
}

// fallbackTwoAccounts seeds A serving and B above its own switch point
// (max5x's 5h point is 93%) but well below its wall — the shape that sends
// `next` through the fallback (item 2, review round 3), with B the only
// other account so there is nothing else to consider.
func fallbackTwoAccounts(t *testing.T) (home string, s store.Store) {
	t.Helper()
	home, s = seedTwoAccounts(t)
	now := time.Now()
	pct := 95.0
	writeStatusFile(t, home, status.File{Accounts: []status.Account{
		{Name: "B", Usage: &status.Usage{UpdatedAt: now, FiveHourPct: &pct, FiveHourResetsAt: now.Add(4 * time.Hour)}},
	}})
	return home, s
}

// TestNextJSONCarriesFallbackTrue is item 3c (review round 4): a survived
// mutant that dropped nextResult.Fallback (or always left it false) still
// passed every test before this one, since none of them decoded the JSON
// field itself.
func TestNextJSONCarriesFallbackTrue(t *testing.T) {
	_, s := fallbackTwoAccounts(t)
	code, out, errb := runChottag(t, "next", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	doc := decodeOneDocument(t, out)
	if doc["serving"] != "B" {
		t.Fatalf("serving = %v, want B (the fallback)", doc["serving"])
	}
	if fb, _ := doc["fallback"].(bool); !fb {
		t.Errorf("fallback = %v, want true", doc["fallback"])
	}
	if st, err := s.Load(); err != nil || st.Serving != "B" {
		t.Errorf("serving in state.json = %q (%v), want B", st.Serving, err)
	}
}

// TestNextHumanOutputPrintsTheFallbackLine is item 3d (review round 4): a
// survived mutant that suppressed the fallback's human-readable line still
// passed every test before this one, since none of them checked stdout's
// text form for it.
func TestNextHumanOutputPrintsTheFallbackLine(t *testing.T) {
	fallbackTwoAccounts(t)
	code, out, errb := runChottag(t, "next")
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if !strings.Contains(out, "B is above its switch point; no account was below its own\n") {
		t.Errorf("stdout = %q, want the fallback line", out)
	}
	if !strings.Contains(out, "serving: B\n") {
		t.Errorf("stdout = %q, want the serving line too", out)
	}
}
