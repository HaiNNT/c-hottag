package doctor

import "testing"

// daemonVersionRow runs daemonVersionCheck() alone against ti's env.
func daemonVersionRow(t *testing.T, ti *testInstall) Row {
	t.Helper()
	return rowByID(t, mustRun(t, ti.env, []Check{daemonVersionCheck()}, false), "daemon-version")
}

// TestDaemonVersionOKWhenNoDaemonIsRunning pins the first of the check's
// four cases (public release design §2.4): a stopped daemon is the normal
// state between launches (D13), never a problem, so the check has to read
// ok rather than merely absent.
func TestDaemonVersionOKWhenNoDaemonIsRunning(t *testing.T) {
	ti := newTestInstall(t)
	ti.healthy = false
	r := daemonVersionRow(t, ti)
	if r.Status != StatusOK || r.Detail != "no daemon running" || r.Hint != "" {
		t.Fatalf("daemon-version = %+v, want ok, no daemon running, no hint", r)
	}
}

// TestDaemonVersionOKWhenVersionsMatch is the second case: a running daemon
// whose health version equals this chottag's own.
func TestDaemonVersionOKWhenVersionsMatch(t *testing.T) {
	ti := newTestInstall(t)
	ti.daemonVersion = testSelfVersion
	r := daemonVersionRow(t, ti)
	if r.Status != StatusOK || r.Detail != "daemon runs "+testSelfVersion || r.Hint != "" {
		t.Fatalf("daemon-version = %+v, want ok naming the matching version", r)
	}
}

// TestDaemonVersionProblemWhenVersionsDiffer is the third case: a running
// daemon whose health version differs from this chottag's own is a
// problem, with the restart hint — never fixed automatically, since a
// restart interrupts every live session (the user's call).
func TestDaemonVersionProblemWhenVersionsDiffer(t *testing.T) {
	ti := newTestInstall(t)
	ti.daemonVersion = "0.3.0"
	r := daemonVersionRow(t, ti)
	if r.Status != StatusProblem || r.Detail != "the daemon runs 0.3.0, this chottag is "+testSelfVersion || r.Hint != "chottag daemon restart" {
		t.Fatalf("daemon-version = %+v, want a problem naming both versions with the restart hint", r)
	}
	if daemonVersionCheck().Fix != nil {
		t.Fatal("daemonVersionCheck has a Fix; a restart interrupts sessions, so it must stay the user's call")
	}
}

// TestDaemonVersionInfoWhenTheDaemonReportsAnEmptyVersion mirrors
// status.go's own probe guard (`ok && version != ""`): a daemon that
// answers but reports no usable version must not be treated as a mismatch
// against this chottag's own (which would print the malformed "the daemon
// runs , this chottag is <v>"). It reads info, not problem, and carries no
// restart hint.
func TestDaemonVersionInfoWhenTheDaemonReportsAnEmptyVersion(t *testing.T) {
	ti := newTestInstall(t)
	ti.daemonVersion = ""
	r := daemonVersionRow(t, ti)
	if r.Status != StatusInfo || r.Detail != "the daemon did not report its version" || r.Hint != "" {
		t.Fatalf("daemon-version = %+v, want info, \"the daemon did not report its version\", no hint", r)
	}
}

// TestDaemonVersionNeverFixed checks that --fix leaves a version mismatch
// exactly as it found it: no Fix means Run never repairs it, whatever fix
// says.
func TestDaemonVersionNeverFixed(t *testing.T) {
	ti := newTestInstall(t)
	ti.daemonVersion = "0.3.0"
	before := ti.snapshot()
	r := rowByID(t, mustRun(t, ti.env, []Check{daemonVersionCheck()}, true), "daemon-version")
	if r.Status != StatusProblem || r.Hint != "chottag daemon restart" {
		t.Fatalf("daemon-version under --fix = %+v, want the same report-only problem", r)
	}
	assertChangedOnly(t, before, ti.snapshot())
}
