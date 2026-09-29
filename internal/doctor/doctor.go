// Package doctor checks a chottag install and repairs what it safely can.
// Every check reads through Env. Nothing here writes except a Check's Fix,
// and Run calls a Fix only under --fix. The package never reads a
// credential and never contacts the network: imports_test.go enforces
// both.
package doctor

import (
	"fmt"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/status"
)

// Row statuses (spec §2.3). A Detect returns only ok, problem or info.
// fixed and skipped are Run's.
const (
	StatusOK      = "ok"
	StatusFixed   = "fixed"
	StatusProblem = "problem"
	StatusInfo    = "info"
	StatusSkipped = "skipped"
)

// FixHint is the hint a fixable problem gets when --fix was not given
// (spec §2.1).
const FixHint = "chottag doctor --fix"

// Check is one row of spec §2.2.
type Check struct {
	ID     string // stable, kebab-case: "daemon", "rc-block", "token:B"
	Detect func(*Env) Finding
	Fix    func(*Env) error // nil = report only
	// Gate: when this check ends as a problem, every later check is
	// skipped. Only "setup" gates (spec §2.2 row 1, D3).
	Gate bool
}

// Finding is what Detect saw.
type Finding struct {
	Status string // ok | problem | info
	Detail string // one line; never a token, a body or an owners.json id
	// Hint is the command the user runs. On a problem, "" means Fix
	// repairs it, and Run shows FixHint without --fix. A report-only case
	// of a fixable check sets a hint, so Fix never runs for it.
	Hint string
	// Err is an internal error, not a finding: an unreadable state.json.
	// Run stops and returns it, and the command exits 1 (spec §2.3).
	Err error
	// Installed and LastTraced are row 15's (version-drift) JSON fields:
	// the installed Claude Code version and the last traced one. Every
	// other row leaves them empty, and Row omits them.
	Installed  string
	LastTraced *status.TracedVersion
}

// Internal is a Finding that stops the run with err.
func Internal(err error) Finding { return Finding{Err: err} }

// Row is one reported line: the text row and the JSON check object.
type Row struct {
	ID         string                `json:"id"`
	Status     string                `json:"status"`
	Detail     string                `json:"detail"`
	Hint       string                `json:"hint"`
	Installed  string                `json:"installed,omitempty"`
	LastTraced *status.TracedVersion `json:"lastTraced,omitempty"`
}

// Run runs checks in order (spec §2.1's fix flow, ruling D2):
//   - A Detect that says problem, with Fix != nil and no hint of its own,
//     is fixable.
//   - Under fix, a fixable problem runs Fix, then Detect again. The row is
//     fixed when the second Detect says ok. Otherwise it stays a problem,
//     with the fix error in Detail.
//   - Without fix, a fixable problem gets FixHint, and no Fix ever runs.
//   - A gating check that ends as a problem skips every later check.
//
// A panic in a check, or a Finding with Err, stops the run. Run returns
// the rows so far and the error, which is an internal error (exit 1).
func Run(env *Env, checks []Check, fix bool) ([]Row, error) {
	rows := make([]Row, 0, len(checks))
	gate := ""
	for _, c := range checks {
		if gate != "" {
			rows = append(rows, Row{ID: oneLine(c.ID), Status: StatusSkipped, Detail: "skipped: the " + gate + " check found a problem"})
			continue
		}
		row, err := runOne(env, c, fix)
		if err != nil {
			return rows, err
		}
		rows = append(rows, row)
		if c.Gate && row.Status == StatusProblem {
			gate = c.ID
		}
	}
	return rows, nil
}

func runOne(env *Env, c Check, fix bool) (row Row, err error) {
	defer func() {
		if p := recover(); p != nil {
			row, err = Row{}, fmt.Errorf("check %s panicked: %v", c.ID, p)
		}
	}()
	f, err := detect(env, c)
	if err != nil {
		return Row{}, err
	}
	fixable := f.Status == StatusProblem && c.Fix != nil && f.Hint == ""
	switch {
	case fixable && fix:
		fixErr := c.Fix(env)
		again, err := detect(env, c)
		if err != nil {
			return Row{}, err
		}
		if again.Status == StatusOK {
			f = Finding{Status: StatusFixed, Detail: f.Detail}
		} else {
			detail := again.Detail
			if fixErr != nil {
				detail += "; the fix failed: " + fixErr.Error()
			}
			f = Finding{Status: StatusProblem, Detail: detail, Hint: again.Hint}
		}
	case fixable:
		f.Hint = FixHint
	}
	return Row{ID: oneLine(c.ID), Status: f.Status, Detail: oneLine(f.Detail), Hint: oneLine(f.Hint), Installed: oneLine(f.Installed), LastTraced: f.LastTraced}, nil
}

func detect(env *Env, c Check) (Finding, error) {
	f := c.Detect(env)
	if f.Err != nil {
		return Finding{}, fmt.Errorf("check %s: %w", c.ID, f.Err)
	}
	switch f.Status {
	case StatusOK, StatusProblem, StatusInfo:
		return f, nil
	}
	return Finding{}, fmt.Errorf("check %s: Detect returned status %q; only ok, problem and info are allowed", c.ID, f.Status)
}

// oneLine replaces every control character with a space, so a hand-edited
// account name or an error text can never break a row across lines.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
