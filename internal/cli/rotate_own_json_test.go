package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/owners"
	"github.com/HaiNNT/c-hottag/internal/router"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// rotOwnJSONHome registers A and B and seeds owners.json with artifact
// art1 owned by A, in a fresh CHOTTAG_HOME.
func rotOwnJSONHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	seedOwnState(t, home, "A", "B")
	seedOwners(t, home, router.KindArtifact, "art1", "A")
	return home
}

func rotOwnJSONOwner(t *testing.T, home string) string {
	t.Helper()
	own, err := owners.Open(filepath.Join(home, "owners.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	acct, _ := own.Lookup(router.KindArtifact, "art1")
	return acct
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "rotate off excludes the account", command: "rotate",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"rotate", "B", "off"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["account"] != "B" || doc["rotate"] != false || doc["inRotation"] != float64(1) {
					t.Errorf("doc = %v, want account B, rotate false, inRotation 1", doc)
				}
			},
		},
		jsonCase{
			name: "rotate shows the setting", command: "rotate",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"rotate", "A"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["account"] != "A" || doc["rotate"] != true || doc["inRotation"] != float64(2) {
					t.Errorf("doc = %v, want account A, rotate true, inRotation 2", doc)
				}
			},
		},
		jsonCase{
			name: "rotate an unknown account", command: "rotate",
			setup:    func(t *testing.T) []string { seedTwoAccounts(t); return []string{"rotate", "nope", "off"} },
			wantExit: exit.Error, wantCode: codeUnknownAccount,
		},
		jsonCase{
			name: "rotate the last account out warns", command: "rotate",
			setup: func(t *testing.T) []string {
				_, s := seedTwoAccounts(t)
				if _, err := s.Update(func(st *store.State) error { st.Accounts[1].NoRotate = true; return nil }); err != nil {
					t.Fatal(err)
				}
				return []string{"rotate", "A", "off"}
			},
			check: func(t *testing.T, doc map[string]any) {
				ws, _ := doc["warnings"].([]any)
				if len(ws) != 1 || ws[0].(map[string]any)["code"] != "no_rotation_left" {
					t.Errorf("warnings = %v, want one no_rotation_left", ws)
				}
			},
		},
		jsonCase{
			name: "own reassigns a known id", command: "own",
			setup: func(t *testing.T) []string { rotOwnJSONHome(t); return []string{"own", "artifact", "art1", "B"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["kind"] != "artifact" || doc["id"] != "art1" || doc["account"] != "B" || doc["reassigned"] != true {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "own prints the owner", command: "own",
			setup: func(t *testing.T) []string { rotOwnJSONHome(t); return []string{"own", "artifact", "art1"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["account"] != "A" || doc["reassigned"] != false {
					t.Errorf("doc = %v, want account A, reassigned false", doc)
				}
			},
		},
		jsonCase{
			name: "own an unknown id", command: "own",
			setup:    func(t *testing.T) []string { rotOwnJSONHome(t); return []string{"own", "artifact", "typo", "B"} },
			wantExit: exit.Error, wantCode: codeNotFound,
			check: func(t *testing.T, doc map[string]any) {
				if e := docError(t, doc); e["kind"] != "artifact" || e["id"] != "typo" {
					t.Errorf("error = %v, want kind and id", e)
				}
			},
		},
	)
}

// rotate takes no flags (spec §5.3): a stray one is exit 2 and changes
// nothing, never read as a name or an on|off; after -- the rule ends.
func TestRotateRejectsAStrayFlag(t *testing.T) {
	for _, args := range [][]string{{"rotate", "--force", "B"}, {"rotate", "B", "--force"}, {"rotate", "-x"}, {"rotate", "B", "off", "-"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, s := seedTwoAccounts(t)
			code, _, errb := runChottag(t, args...)
			if code != exit.Usage || (!strings.Contains(errb, "unexpected flag") && !strings.Contains(errb, "usage: chottag rotate")) {
				t.Fatalf("exit = %d stderr=%q, want 2 naming the stray flag or the usage", code, errb)
			}
			st, _ := s.Load()
			if b, _ := st.Find("B"); !b.Rotates() {
				t.Error("B was taken out of rotation despite the usage error")
			}
		})
	}
	_, s := seedTwoAccounts(t)
	if code, _, errb := runChottag(t, "rotate", "--", "B", "off"); code != 0 {
		t.Fatalf("rotate -- B off = %d, stderr=%q; want 0", code, errb)
	}
	st, _ := s.Load()
	if b, _ := st.Find("B"); b.Rotates() {
		t.Error("rotate -- B off did not take B out of rotation")
	}
}

// Before M1d-d `rotate --force B` looked --force up as an account (exit 1);
// it is a usage error (exit 2) naming the flag.
func TestRotateNamesTheStrayFlag(t *testing.T) {
	seedTwoAccounts(t)
	_, _, errb := runChottag(t, "rotate", "--force", "B")
	if !strings.Contains(errb, `unexpected flag "--force"`) {
		t.Fatalf("stderr = %q, want it to name the stray flag", errb)
	}
}

// own takes no flags either: a stray one is exit 2 and reassigns nothing.
func TestOwnRejectsAStrayFlag(t *testing.T) {
	for _, args := range [][]string{{"own", "artifact", "art1", "--force"}, {"own", "--force", "artifact", "art1", "B"}, {"own", "artifact", "art1", "-B"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			home := rotOwnJSONHome(t)
			if code, _, errb := runChottag(t, args...); code != exit.Usage {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, exit.Usage, errb)
			}
			if got := rotOwnJSONOwner(t, home); got != "A" {
				t.Errorf("owner = %q, want A untouched", got)
			}
		})
	}
	home := rotOwnJSONHome(t)
	if code, _, errb := runChottag(t, "own", "--", "artifact", "art1", "B"); code != 0 {
		t.Fatalf("own -- artifact art1 B = %d, stderr=%q; want 0", code, errb)
	}
	if got := rotOwnJSONOwner(t, home); got != "B" {
		t.Errorf("owner = %q, want B", got)
	}
}
