package doctor

import (
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

// TestPlanUnknownIsOKWhenEveryMaxAccountHasASize: every account has an
// explicit, known plan (item 5, review round 2: neither "max" nor "", a
// fresh install's own default, counts), so the row is ok.
func TestPlanUnknownIsOKWhenEveryMaxAccountHasASize(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error {
		st.Accounts[0].Plan = "max20x"
		return nil
	})
	r := rowByID(t, mustRun(t, ti.env, []Check{planUnknownCheck()}, true), "plan-unknown")
	if r.Status != StatusOK || r.Hint != "" {
		t.Fatalf("%+v, want ok", r)
	}
}

// TestPlanUnknownNamesEveryMaxAccountWithoutASize pins spec §7's info row.
func TestPlanUnknownNamesEveryMaxAccountWithoutASize(t *testing.T) {
	ti := newTestInstall(t)
	ti.update(func(st *store.State) error {
		st.Accounts[0].Plan = "max"
		st.Accounts = append(st.Accounts,
			store.Account{Name: "Pro1", Dir: st.Accounts[0].Dir + "-pro", Plan: "pro"},
			store.Account{Name: "Big", Dir: st.Accounts[0].Dir + "-big", Plan: "max"})
		return nil
	})
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, []Check{planUnknownCheck()}, true), "plan-unknown")
	if r.Status != StatusInfo {
		t.Fatalf("%+v, want info (never a problem)", r)
	}
	if !strings.Contains(r.Detail, "treated as max5x") || !strings.HasSuffix(r.Detail, ": A, Big") {
		t.Fatalf("detail = %q, want both Max accounts named", r.Detail)
	}
	if r.Hint != "chottag plan A max5x (or max20x)" {
		t.Fatalf("hint = %q", r.Hint)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

// TestPlanUnknownAlsoNamesAnAccountWithNoPlanAtAll is item 5 (review round
// 2): tierOf treats a blank plan ("", never set by login/adopt or the
// user) the same as "max" — both default to max5x (S2) — so the doctor row
// must name it too, not just the narrower "max" case spec §7 first named.
func TestPlanUnknownAlsoNamesAnAccountWithNoPlanAtAll(t *testing.T) {
	ti := newTestInstall(t) // account "A" starts with no plan set at all
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, []Check{planUnknownCheck()}, true), "plan-unknown")
	if r.Status != StatusInfo {
		t.Fatalf("%+v, want info (never a problem)", r)
	}
	if !strings.Contains(r.Detail, "treated as max5x") || !strings.HasSuffix(r.Detail, ": A") {
		t.Fatalf("detail = %q, want A named even though its plan was never set to \"max\"", r.Detail)
	}
	if r.Hint != "chottag plan A max5x (or max20x)" {
		t.Fatalf("hint = %q", r.Hint)
	}
	assertChangedOnly(t, before, ti.snapshot())
}
