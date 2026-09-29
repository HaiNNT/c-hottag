package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

func TestPlanSetsTheTierAndUnits(t *testing.T) {
	_, s := autoHome(t)
	if code, out, errs := runChottag(t, "plan", "B", "max20x"); code != exit.OK || out != "plan B: max20x (20 units)\n" {
		t.Fatalf("plan = %d %q %q", code, out, errs)
	}
	if code, out, _ := runChottag(t, "plan", "b", "team", "--units", "8"); code != exit.OK || out != "plan B: team (8 units)\n" {
		t.Fatalf("plan --units = %d %q", code, out)
	}
	st, _ := s.Load()
	if st.Accounts[1].Plan != "team" || st.Accounts[1].Units != 8 {
		t.Fatalf("B = %+v", st.Accounts[1])
	}
	// Without --units the tier's own units apply again.
	if code, out, _ := runChottag(t, "plan", "B", "pro"); code != exit.OK || out != "plan B: pro (1 units)\n" {
		t.Fatalf("plan pro = %d %q", code, out)
	}
	if st, _ := s.Load(); st.Accounts[1].Units != 0 {
		t.Fatalf("units = %d, want the override cleared", st.Accounts[1].Units)
	}
}

// TestPlanRejectsBadInputAndWritesNothing: a bad tier or units is exit 2;
// an unknown account is exit 1 with unknown_account.
func TestPlanRejectsBadInputAndWritesNothing(t *testing.T) {
	home, _ := autoHome(t)
	before, _ := os.ReadFile(filepath.Join(home, "state.json"))
	for _, args := range [][]string{
		{"plan"}, {"plan", "B"}, {"plan", "B", "max"}, {"plan", "B", "Max20x"}, {"plan", "B", "pro", "extra"},
		{"plan", "B", "pro", "--units", "0"}, {"plan", "B", "pro", "--units", "1001"}, {"plan", "B", "pro", "--units", "x"},
	} {
		if code, out, errs := runChottag(t, args...); code != exit.Usage || out != "" || errs == "" {
			t.Errorf("%q = %d %q %q, want exit 2", args, code, out, errs)
		}
	}
	if code, _, errs := runChottag(t, "plan", "Zed", "pro"); code != exit.Error || errs == "" {
		t.Errorf("unknown account = %d %q, want exit 1", code, errs)
	}
	after, _ := os.ReadFile(filepath.Join(home, "state.json"))
	if string(before) != string(after) {
		t.Fatal("a refused plan changed state.json")
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "plan sets a tier", command: "plan",
			setup: func(t *testing.T) []string { autoHome(t); return []string{"plan", "B", "max20x", "--units", "25"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["account"] != "B" || doc["plan"] != "max20x" || doc["units"] != float64(25) {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "plan for an unknown account", command: "plan",
			setup:    func(t *testing.T) []string { autoHome(t); return []string{"plan", "Zed", "pro"} },
			wantExit: exit.Error, wantCode: codeUnknownAccount,
		},
	)
}
