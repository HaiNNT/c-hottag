package doctor

import (
	"errors"
	"strings"
	"testing"
)

// fakeCheck reports a problem until Fix sets fixed (or fails with fixErr).
type fakeCheck struct {
	id      string
	hint    string
	fixErr  error
	noFix   bool
	fixed   bool
	detects int
	fixes   int
}

func (f *fakeCheck) check() Check {
	c := Check{ID: f.id, Detect: func(*Env) Finding {
		f.detects++
		if f.fixed {
			return Finding{Status: StatusOK, Detail: f.id + " is fine"}
		}
		return Finding{Status: StatusProblem, Detail: f.id + " is broken", Hint: f.hint}
	}}
	if !f.noFix {
		c.Fix = func(*Env) error {
			f.fixes++
			if f.fixErr != nil {
				return f.fixErr
			}
			f.fixed = true
			return nil
		}
	}
	return c
}

func okCheck(id string) Check {
	return Check{ID: id, Detect: func(*Env) Finding { return Finding{Status: StatusOK, Detail: "fine"} }}
}

func TestRunWithoutFixNeverRunsAFix(t *testing.T) {
	f := &fakeCheck{id: "x"}
	rows, err := Run(&Env{}, []Check{f.check()}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := Row{ID: "x", Status: StatusProblem, Detail: "x is broken", Hint: FixHint}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows = %+v, want [%+v]", rows, want)
	}
	if f.fixes != 0 {
		t.Fatalf("Fix ran %d time(s) without --fix", f.fixes)
	}
}

func TestRunFixRepairsThenDetectsAgain(t *testing.T) {
	f := &fakeCheck{id: "x"}
	rows, err := Run(&Env{}, []Check{f.check()}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := Row{ID: "x", Status: StatusFixed, Detail: "x is broken"}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows = %+v, want [%+v]", rows, want)
	}
	if f.fixes != 1 || f.detects != 2 {
		t.Fatalf("fixes = %d, detects = %d; want 1 and 2", f.fixes, f.detects)
	}
}

func TestRunFixThatFailsKeepsTheProblemWithTheError(t *testing.T) {
	f := &fakeCheck{id: "x", fixErr: errors.New("disk full")}
	rows, err := Run(&Env{}, []Check{f.check()}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := Row{ID: "x", Status: StatusProblem, Detail: "x is broken; the fix failed: disk full"}
	if rows[0] != want {
		t.Fatalf("row = %+v, want %+v", rows[0], want)
	}
}

func TestRunFixThatDoesNotTakeKeepsTheProblem(t *testing.T) {
	c := Check{
		ID:     "x",
		Detect: func(*Env) Finding { return Finding{Status: StatusProblem, Detail: "still broken"} },
		Fix:    func(*Env) error { return nil },
	}
	rows, err := Run(&Env{}, []Check{c}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Row{ID: "x", Status: StatusProblem, Detail: "still broken"}); rows[0] != want {
		t.Fatalf("row = %+v, want %+v", rows[0], want)
	}
}

func TestRunNeverFixesAReportOnlyProblem(t *testing.T) {
	f := &fakeCheck{id: "ca", hint: "move both files away"}
	rows, err := Run(&Env{}, []Check{f.check()}, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Row{ID: "ca", Status: StatusProblem, Detail: "ca is broken", Hint: "move both files away"}); rows[0] != want {
		t.Fatalf("row = %+v, want %+v", rows[0], want)
	}
	if f.fixes != 0 {
		t.Fatalf("Fix ran %d time(s) for a problem that carries its own hint", f.fixes)
	}
}

func TestRunGivesAProblemWithNoFixNoFixHint(t *testing.T) {
	f := &fakeCheck{id: "x", noFix: true}
	rows, err := Run(&Env{}, []Check{f.check()}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Hint != "" {
		t.Fatalf("hint = %q, want none: a check without Fix cannot be repaired by --fix", rows[0].Hint)
	}
}

func TestRunGateSkipsEveryLaterCheck(t *testing.T) {
	gate := Check{ID: "setup", Gate: true, Detect: func(*Env) Finding {
		return Finding{Status: StatusProblem, Detail: "not set up", Hint: "chottag setup"}
	}}
	later := &fakeCheck{id: "tree"}
	rows, err := Run(&Env{}, []Check{gate, later.check(), okCheck("path")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].Status != StatusProblem {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows[1:] {
		want := Row{ID: r.ID, Status: StatusSkipped, Detail: "skipped: the setup check found a problem"}
		if r != want {
			t.Errorf("row = %+v, want %+v", r, want)
		}
	}
	if later.detects != 0 || later.fixes != 0 {
		t.Fatalf("a skipped check ran: detects = %d, fixes = %d", later.detects, later.fixes)
	}
}

func TestRunGateThatPassesRunsTheRest(t *testing.T) {
	gate := okCheck("setup")
	gate.Gate = true
	later := &fakeCheck{id: "tree"}
	rows, err := Run(&Env{}, []Check{gate, later.check()}, false)
	if err != nil {
		t.Fatal(err)
	}
	if rows[1].Status != StatusProblem {
		t.Fatalf("rows = %+v, want tree to run", rows)
	}
}

func TestRunTurnsAPanicIntoAnInternalError(t *testing.T) {
	for _, where := range []string{"detect", "fix"} {
		t.Run(where, func(t *testing.T) {
			after := &fakeCheck{id: "after"}
			boom := Check{
				ID: "boom",
				Detect: func(*Env) Finding {
					if where == "detect" {
						panic("nil map")
					}
					return Finding{Status: StatusProblem, Detail: "broken"}
				},
				Fix: func(*Env) error { panic("nil map") },
			}
			rows, err := Run(&Env{}, []Check{okCheck("first"), boom, after.check()}, true)
			if err == nil || !strings.Contains(err.Error(), "check boom panicked: nil map") {
				t.Fatalf("err = %v, want the panic as an internal error", err)
			}
			if len(rows) != 1 || rows[0].ID != "first" {
				t.Fatalf("rows = %+v, want only the rows before the panic", rows)
			}
			if after.detects != 0 {
				t.Fatal("a check after the panic ran")
			}
		})
	}
}

func TestRunStopsAtAnInternalFinding(t *testing.T) {
	corrupt := errors.New("state.json is corrupt")
	c := Check{ID: "roles", Detect: func(*Env) Finding { return Internal(corrupt) }}
	_, err := Run(&Env{}, []Check{c, okCheck("later")}, false)
	if !errors.Is(err, corrupt) || !strings.Contains(err.Error(), "check roles") {
		t.Fatalf("err = %v, want it to wrap the finding's error and name the check", err)
	}
}

func TestRunRejectsAStatusADetectMayNotReturn(t *testing.T) {
	for _, s := range []string{StatusFixed, StatusSkipped, "", "warn"} {
		c := Check{ID: "x", Detect: func(*Env) Finding { return Finding{Status: s} }}
		if _, err := Run(&Env{}, []Check{c}, false); err == nil {
			t.Errorf("status %q was accepted", s)
		}
	}
}

func TestRunKeepsEveryFieldOnOneLine(t *testing.T) {
	c := Check{ID: "token:a\nb", Detect: func(*Env) Finding {
		return Finding{Status: StatusProblem, Detail: "line1\nline2\ttab\x1b[31m", Hint: "run\rthis"}
	}}
	rows, err := Run(&Env{}, []Check{c}, false)
	if err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	for _, s := range []string{r.ID, r.Detail, r.Hint} {
		if strings.ContainsAny(s, "\n\r\t\x1b") {
			t.Errorf("%q still holds a control character", s)
		}
	}
	if r.ID != "token:a b" {
		t.Errorf("id = %q, want %q", r.ID, "token:a b")
	}
}
