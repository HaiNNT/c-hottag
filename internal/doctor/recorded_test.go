package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
)

func writeTestStatus(t *testing.T, home string, f status.File) {
	t.Helper()
	b, err := status.Marshal(f)
	must(t, err)
	must(t, status.WriteBytes(status.Path(home), b))
}

// writeTestOwners writes owners.json in its on-disk shape: key -> {account, at}.
func writeTestOwners(t *testing.T, home string, entries map[string]string) {
	t.Helper()
	m := map[string]map[string]string{}
	for k, a := range entries {
		m[k] = map[string]string{"account": a, "at": "2026-09-24T09:00:00Z"}
	}
	b, err := json.Marshal(m)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(home, "owners.json"), b, 0o600))
}

func TestNeedsLoginIsCredsValue(t *testing.T) {
	if needsLogin != string(creds.StateNeedsLogin) {
		t.Fatalf("needsLogin = %q, creds says %q", needsLogin, creds.StateNeedsLogin)
	}
}

func TestReportChecksOnAHealthyInstall(t *testing.T) {
	ti := newTestInstall(t)
	rows := mustRun(t, ti.env, ReportChecks([]string{"A"}), false)
	for id, want := range map[string]string{"token:A": StatusOK, "owners": StatusOK, "route-drift": StatusInfo, "limits": StatusOK, "version-drift": StatusInfo} {
		if r := rowByID(t, rows, id); r.Status != want {
			t.Errorf("%+v, want %s", r, want)
		}
	}
	if r := rowByID(t, rows, "token:A"); r.Detail != "not checked (no traffic yet)" {
		t.Errorf("token:A detail = %q", r.Detail)
	}
}

func TestTokenNeedsLoginIsAProblemWithTheLoginHint(t *testing.T) {
	ti := newTestInstall(t)
	writeTestStatus(t, ti.home, status.File{Accounts: []status.Account{{Name: "a", Token: creds.StateNeedsLogin}}})
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, ReportChecks([]string{"A"}), true), "token:A")
	if r.Status != StatusProblem || r.Hint != "chottag login A; allow Keychain access if macOS asks" {
		t.Fatalf("token:A = %+v", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestTokenOtherRecordedStatesAreOK(t *testing.T) {
	for _, s := range []creds.TokenState{creds.StateOK, creds.StateExpiring, creds.StateStale} {
		ti := newTestInstall(t)
		writeTestStatus(t, ti.home, status.File{Accounts: []status.Account{{Name: "A", Token: s}}})
		r := rowByID(t, mustRun(t, ti.env, ReportChecks([]string{"A"}), false), "token:A")
		if r.Status != StatusOK || !strings.Contains(r.Detail, "last recorded token state: "+string(s)) {
			t.Errorf("%s: %+v, want ok", s, r)
		}
	}
}

func TestOwnersCountsUnknownNamesWithoutListingIDs(t *testing.T) {
	ti := newTestInstall(t)
	writeTestOwners(t, ti.home, map[string]string{
		"artifact:secret-id-1":  "B",
		"session:secret-id-2":   "B",
		"artifact:secret-id-3":  "A",
		"connector:secret-id-4": "x\ny",
	})
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, ReportChecks([]string{"A"}), true), "owners")
	if r.Status != StatusInfo {
		t.Fatalf("owners = %+v, want info (F170)", r)
	}
	for _, want := range []string{"3 of 4 entries", `2 name "B"`, `1 name "x\ny"`} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q lacks %q", r.Detail, want)
		}
	}
	if strings.Contains(r.Detail, "secret-id") || strings.Contains(r.Hint, "secret-id") {
		t.Fatalf("an owners.json id reached the row: %+v", r)
	}
	if !strings.Contains(r.Hint, "chottag rename") || !strings.Contains(r.Hint, "chottag own") {
		t.Fatalf("hint = %q, want chottag own and chottag rename", r.Hint)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestOwnersCorruptFileIsInfoAndLeftAlone(t *testing.T) {
	ti := newTestInstall(t)
	must(t, os.WriteFile(filepath.Join(ti.home, "owners.json"), []byte("{"), 0o600))
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, ReportChecks([]string{"A"}), true), "owners")
	if r.Status != StatusInfo || !strings.Contains(r.Detail, "not valid JSON") {
		t.Fatalf("owners = %+v", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}

func TestRouteDrift(t *testing.T) {
	fresh, old := testNow.Add(-10*time.Second), testNow.Add(-time.Hour)
	for _, tc := range []struct {
		name         string
		daemon       *status.Daemon
		status, hint string
	}{
		{"no daemon yet", nil, StatusInfo, ""},
		{"running, no drift", &status.Daemon{Heartbeat: fresh}, StatusOK, ""},
		{"running with drift", &status.Daemon{Heartbeat: fresh, RouteDrift: 3}, StatusProblem, "chottag trace on"},
		{"stopped with drift", &status.Daemon{Heartbeat: old, RouteDrift: 3}, StatusInfo, "chottag trace on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			writeTestStatus(t, ti.home, status.File{Daemon: tc.daemon})
			r := rowByID(t, mustRun(t, ti.env, []Check{routeDriftCheck()}, true), "route-drift")
			if r.Status != tc.status || r.Hint != tc.hint {
				t.Fatalf("route-drift = %+v, want %s with hint %q", r, tc.status, tc.hint)
			}
		})
	}
}

func TestLimits(t *testing.T) {
	soon, later := testNow.Add(time.Hour), testNow.Add(2*time.Hour)
	for _, tc := range []struct {
		name           string
		accounts       []status.Account
		status, detail string
	}{
		{"nothing recorded", nil, StatusOK, "no usage recorded yet"},
		{"one account free", []status.Account{{Name: "A", Limited: true, LimitedUntil: later}, {Name: "B"}}, StatusOK, "not limited"},
		{"all limited", []status.Account{{Name: "A", Limited: true, LimitedUntil: later}, {Name: "B", Limited: true, LimitedUntil: soon}},
			StatusInfo, "the earliest reset is " + soon.Local().Format("Jan 2 15:04") + " (B)"},
		{"no reset known", []status.Account{{Name: "A", Limited: true}}, StatusInfo, "no reset time is known"},
		{"a reset already passed", []status.Account{{Name: "A", Limited: true, LimitedUntil: later}, {Name: "B", Limited: true, LimitedUntil: testNow.Add(-time.Minute)}},
			StatusOK, "not limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ti := newTestInstall(t)
			writeTestStatus(t, ti.home, status.File{Accounts: tc.accounts})
			r := rowByID(t, mustRun(t, ti.env, []Check{limitsCheck()}, true), "limits")
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("limits = %+v, want %s containing %q", r, tc.status, tc.detail)
			}
		})
	}
}

func TestReportChecksNeverWrite(t *testing.T) {
	ti := newTestInstall(t)
	writeTestStatus(t, ti.home, status.File{
		Accounts: []status.Account{{Name: "A", Token: creds.StateNeedsLogin, Limited: true, LimitedUntil: testNow.Add(time.Hour)}},
		Daemon:   &status.Daemon{Heartbeat: testNow, RouteDrift: 2},
	})
	writeTestOwners(t, ti.home, map[string]string{"artifact:x": "Gone"})
	before := ti.snapshot()
	mustRun(t, ti.env, ReportChecks([]string{"A"}), true)
	assertChangedOnly(t, before, ti.snapshot())
}
