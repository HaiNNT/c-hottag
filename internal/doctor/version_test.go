package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

var versionTracedAt = time.Date(2026, 9, 25, 10, 40, 0, 0, time.Local)

func withLastTraced(t *testing.T, ti *testInstall, version string) {
	t.Helper()
	var f status.File
	f.SetLastTraced(version, versionTracedAt)
	writeTestStatus(t, ti.home, f)
}

func versionRow(t *testing.T, ti *testInstall) Row {
	t.Helper()
	return rowByID(t, mustRun(t, ti.env, []Check{versionDriftCheck()}, false), "version-drift")
}

func TestVersionDriftOKWhenTheInstalledVersionWasTraced(t *testing.T) {
	ti := newTestInstall(t)
	withLastTraced(t, ti, "2.1.282")
	r := versionRow(t, ti)
	if r.Status != StatusOK || r.Detail != "Claude Code 2.1.282 was traced on Sep 25 10:40" || r.Hint != "" || r.Installed != "2.1.282" {
		t.Fatalf("row = %+v", r)
	}
	if r.LastTraced == nil || r.LastTraced.ClaudeVersion != "2.1.282" || !r.LastTraced.At.Equal(versionTracedAt) {
		t.Fatalf("lastTraced = %+v", r.LastTraced)
	}
	if len(ti.versionCalls) != 1 || ti.versionCalls[0] != ti.claude {
		t.Fatalf("ClaudeVersion asked about %q, want only row 8's resolved claude %q", ti.versionCalls, ti.claude)
	}
}

func TestVersionDriftInfoWhenTheInstalledVersionIsNewer(t *testing.T) {
	ti := newTestInstall(t)
	withLastTraced(t, ti, "2.1.281")
	r := versionRow(t, ti)
	want := "Claude Code 2.1.282 has not been traced (last traced: 2.1.281 at Sep 25 10:40)"
	if r.Status != StatusInfo || r.Detail != want || r.Hint != "chottag trace on" || r.Installed != "2.1.282" || r.LastTraced == nil {
		t.Fatalf("row = %+v, want info %q with hint chottag trace on", r, want)
	}
}

func TestVersionDriftInfoWhenNothingWasTraced(t *testing.T) {
	ti := newTestInstall(t)
	r := versionRow(t, ti)
	if r.Status != StatusInfo || r.Detail != "no trace recorded on this machine" || r.Hint != "chottag trace on" || r.Installed != "2.1.282" || r.LastTraced != nil {
		t.Fatalf("row = %+v", r)
	}
}

func TestVersionDriftInfoWhenTheVersionCannotBeRead(t *testing.T) {
	cases := map[string]func(ti *testInstall){
		"no real claude": func(ti *testInstall) {
			ti.env.ResolveClaude = func(string, string, string) (string, error) { return "", errors.New("chottag: no claude on PATH") }
		},
		"--version failed": func(ti *testInstall) { ti.claudeVersionErr = errors.New("exit status 1") },
		"--version timed out": func(ti *testInstall) {
			ti.claudeVersionErr = fmt.Errorf("did not finish: %w", context.DeadlineExceeded)
		},
		"garbage output":      func(ti *testInstall) { ti.claudeVersion = "Claude Code, version unknown\n" },
		"empty output":        func(ti *testInstall) { ti.claudeVersion = "" },
		"version on line two": func(ti *testInstall) { ti.claudeVersion = "warning: something\n2.1.282 (Claude Code)\n" },
		// PF5: the mutation strings.Cut(out, "\n") -> out, "", true would
		// pass the whole output to TrimSpace instead of just the first
		// line. TrimSpace only trims leading/trailing whitespace, so on
		// "\n2.1.282 (Claude Code)\n" it would strip the leading blank line
		// and the trailing newline, leaving "2.1.282 (Claude Code)" — which
		// DOES match installedVersionRe, so the mutation would survive
		// unless the first-line cut is what actually runs. This case pins
		// "only the first line" against that mutation.
		"blank first line": func(ti *testInstall) { ti.claudeVersion = "\n2.1.282 (Claude Code)\n" },
		// A 7-digit part is never a real Claude Code version. The major
		// part here has 7 digits directly followed by the "." that ends
		// it, so no 1-6 digit prefix of it lines up with that "." — the
		// bounded regexp (1-6 digits per part) has no match at all, unlike
		// an unbounded one, which would happily capture all 7 digits.
		"7-digit part": func(ti *testInstall) { ti.claudeVersion = "1234567.1.282 (Claude Code)\n" },
		// Without the trailing (?:[^0-9]|$) boundary, this would truncate
		// to "2.1.123456" instead of failing to match.
		"7-digit last part": func(ti *testInstall) { ti.claudeVersion = "2.1.1234567 (Claude Code)\n" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			ti := newTestInstall(t)
			withLastTraced(t, ti, "2.1.282")
			breakIt(ti)
			r := versionRow(t, ti)
			if r.Status != StatusInfo || r.Detail != "could not read the installed version" || r.Hint != "" || r.Installed != "" {
				t.Fatalf("row = %+v", r)
			}
		})
	}
}

// TestVersionDriftIsNeverAProblem is T8 and Review Focus 5: across every
// state, the row is ok or info, so doctor never exits 3 over drift.
func TestVersionDriftIsNeverAProblem(t *testing.T) {
	for _, traced := range []string{"", "2.1.282", "2.1.281", "9.9.9"} {
		for _, out := range []string{"2.1.282 (Claude Code)\n", "", "junk"} {
			ti := newTestInstall(t)
			if traced != "" {
				withLastTraced(t, ti, traced)
			}
			ti.claudeVersion = out
			if r := versionRow(t, ti); r.Status != StatusOK && r.Status != StatusInfo {
				t.Errorf("traced %q, output %q: row = %+v, want ok or info", traced, out, r)
			}
		}
	}
}

// TestOnlyTheVersionDriftRowCarriesTheExtraKeys is the row contract: every
// other row's JSON stays exactly id, status, detail, hint.
func TestOnlyTheVersionDriftRowCarriesTheExtraKeys(t *testing.T) {
	ti := newTestInstall(t)
	withLastTraced(t, ti, "2.1.282")
	for _, r := range mustRun(t, ti.env, All([]string{"A"}), false) {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := "detail hint id status"
		if r.ID == "version-drift" {
			want = "detail hint id installed lastTraced status"
		}
		if got := fmt.Sprint(keys); got != "["+want+"]" {
			t.Errorf("%s keys = %v, want [%s]", r.ID, keys, want)
		}
	}
}
