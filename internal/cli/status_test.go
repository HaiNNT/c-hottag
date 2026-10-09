package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usage"
)

// seedState registers two accounts, sets serving and remote to them, and
// returns the underlying store so a test can mutate it further.
func seedState(t *testing.T, home, serving, remote string) store.Store {
	t.Helper()
	s := seed(t, home, serving, remote) // seed's Add makes the first account both serving and remote
	if _, err := s.Update(func(st *store.State) error {
		st.Serving = serving
		st.Remote = remote
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// seedStateWithOrgs registers a single account carrying an Org, so a test
// can check that status shows it (F16: two accounts can share an email, so
// email alone is ambiguous).
func seedStateWithOrgs(t *testing.T, home string) {
	t.Helper()
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		dir, err := s.SlotDir("A")
		if err != nil {
			return err
		}
		return st.Add(store.Account{Name: "A", Email: "a@example.com", Org: "Acme", Dir: dir})
	}); err != nil {
		t.Fatal(err)
	}
}

func knownSnapshot(fiveHourPct, sevenDayPct float64, at time.Time) usage.Snapshot {
	return usage.Snapshot{
		Known: true,
		At:    at,
		FiveHour: usage.Window{
			Utilization: fiveHourPct, HasUtilization: true, Known: true, Status: "allowed",
		},
		SevenDay: usage.Window{
			Utilization: sevenDayPct, HasUtilization: true, Known: true, Status: "allowed",
		},
		Overall: "allowed",
	}
}

func noVerdict() usage.Verdict { return usage.Verdict{} }

// saveStatus writes f to home's status cache via the composition
// status.Save used to run in one step before the Marshal/WriteBytes split
// (F34): Save itself is gone — nothing in production called it — so every
// test here that needs a status cache file on disk goes through Marshal
// then WriteBytes directly instead.
func saveStatus(t *testing.T, home string, f status.File) {
	t.Helper()
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

func TestStatusShowsUnknownRatherThanZeroForAnUnobservedAccount(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	_, out, _ := runHome(t, home, "status")
	if !strings.Contains(out, "unknown") {
		t.Fatalf("output = %q; an unobserved account must read unknown, never 0%%", out)
	}
	if strings.Contains(out, "0%") {
		t.Error("printed 0% for an account chottag has never observed")
	}
}

func TestStatusMarksAPassthroughAccount(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	var f status.File
	f.SetPassthrough("D", "token stale")
	saveStatus(t, home, f)
	_, out, _ := runHome(t, home, "status")
	if !strings.Contains(out, "token stale") {
		t.Fatalf("output = %q; a passthrough account must say why, or the user believes they switched (F20)", out)
	}
}

func TestStatusJSONMatchesTheCacheSchema(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	var f status.File
	f.Observe("D", knownSnapshot(0.06, 0.23, time.Unix(1789870000, 0)), noVerdict())
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errb)
	}
	if got["serving"] != "D" {
		t.Errorf("serving = %v, want D", got["serving"])
	}
}

// TestStatusJSONStopsAssertingAnExpiredLimitInTheRollUp pins that `status
// --json`'s roll-up (limits.allLimited / limits.nextReset) is actually wired
// to the clock-aware RollUpAt, not just File's own clockless Limits already
// sitting in the loaded cache. An account whose LimitedUntil is already in
// the past must not hold allLimited true or pin nextReset to a date already
// gone by (fix 2): M1c has no poll, so nothing but this recomputation would
// ever stop the trap.
func TestStatusJSONStopsAssertingAnExpiredLimitInTheRollUp(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	var f status.File
	f.Observe("D", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Limited: true, Until: time.Now().Add(-time.Hour)})
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status", "--json")
	var got status.File
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errb)
	}
	if got.Limits.AllLimited {
		t.Fatal("limits.allLimited = true; D's LimitedUntil is already in the past and must not count as confirmed limited")
	}
	if !got.Limits.NextReset.IsZero() {
		t.Errorf("limits.nextReset = %v, want zero; an expired reset must not be asserted as the next one", got.Limits.NextReset)
	}
}

// TestStatusJSONMarksAnExpiredLimitFalseButKeepsLimitedUntil pins the
// per-account half of fix 2 (added after the initial fix wave, once a
// self-review flagged that the roll-up alone left the document
// self-contradictory): limits.allLimited/nextReset stopping to assert a
// past reset is not enough if that same account's own row still claims
// limited: true forever, since M1c has no poll and a consumer reading
// .accounts[].limited directly would never see it clear. limitedUntil
// itself must still be present, so a consumer can see what elapsed.
func TestStatusJSONMarksAnExpiredLimitFalseButKeepsLimitedUntil(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	past := time.Now().Add(-time.Hour)
	var f status.File
	f.Observe("D", usage.Snapshot{Known: true, At: time.Now()}, usage.Verdict{Limited: true, Until: past})
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status", "--json")
	var got status.File
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errb)
	}
	for _, a := range got.Accounts {
		if a.Name != "D" {
			continue
		}
		if a.Limited {
			t.Fatal("D.Limited = true; a LimitedUntil already in the past must not count as confirmed limited in the --json projection")
		}
		if !a.LimitedUntil.Equal(past) {
			t.Errorf("D.LimitedUntil = %v, want %v kept so a consumer can see what elapsed", a.LimitedUntil, past)
		}
	}
}

// TestStatusJSONKeepsLimitedTrueWhileLimitedUntilIsStillFuture is the
// counterpart: an account whose reset has not yet happened must still
// report limited: true, so the fix above cannot be satisfied by simply
// always emitting limited: false.
func TestStatusJSONKeepsLimitedTrueWhileLimitedUntilIsStillFuture(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	future := time.Now().Add(time.Hour)
	var f status.File
	f.Observe("D", usage.Snapshot{Known: true, At: time.Now()}, usage.Verdict{Limited: true, Until: future})
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status", "--json")
	var got status.File
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errb)
	}
	for _, a := range got.Accounts {
		if a.Name != "D" && a.Limited {
			t.Fatalf("account %s unexpectedly limited", a.Name)
		}
		if a.Name == "D" && !a.Limited {
			t.Fatal("D.Limited = false; a LimitedUntil still in the future must still report limited: true")
		}
	}
}

// TestStatusJSONMarksStaleUsageExplicitly pins contract 5: with a
// stale (> status.StaleAfter old) observation, the human table already
// renders "unknown" instead of the raw percentages, but --json used to keep
// emitting them as if they were live, with nothing telling a machine
// consumer not to trust them. The percentages must still be present
// (they're useful even stale) alongside an explicit stale: true.
func TestStatusJSONMarksStaleUsageExplicitly(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	var f status.File
	f.Observe("D", knownSnapshot(0.5, 0.1, time.Now().Add(-2*status.StaleAfter)), noVerdict())
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status", "--json")
	var got status.File
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errb)
	}
	for _, a := range got.Accounts {
		if a.Name != "D" {
			continue
		}
		if !a.Stale {
			t.Fatalf("D.Stale = false, want true for an observation older than StaleAfter")
		}
		if a.Usage == nil || a.Usage.FiveHourPct == nil || *a.Usage.FiveHourPct != 50 {
			t.Fatalf("D.Usage = %+v, want the raw percentages kept alongside stale: true", a.Usage)
		}
	}
}

func TestStatusShowsTheOrgBesideTheEmail(t *testing.T) {
	home := t.TempDir()
	seedStateWithOrgs(t, home)
	_, out, _ := runHome(t, home, "status")
	if !strings.Contains(out, "Acme") {
		t.Fatalf("output = %q; two accounts can share an email (F16), so the org must be shown", out)
	}
}

// TestStatusJSONEmitsEmptyAccountsArrayNotNull pins that `chottag status
// --json` is safe to pipe into jq even before anything is configured:
// encoding/json renders a nil slice as `null`, which `jq '.accounts[]'`
// chokes on, unlike `[]`.
func TestStatusJSONEmitsEmptyAccountsArrayNotNull(t *testing.T) {
	home := t.TempDir() // no accounts registered at all
	_, out, errb := runHome(t, home, "status", "--json")
	if strings.Contains(out, "null") {
		t.Fatalf("output = %q (stderr %q); accounts must render [], never null", out, errb)
	}
	if !strings.Contains(out, `"accounts": []`) {
		t.Fatalf("output = %q, want an explicit empty accounts array", out)
	}
}

// TestStatusJSONReflectsRotate pins that the JSON `rotate` field is actually
// wired to state.json's NoRotate, not just a constant false on every row —
// a wrong value is worse than a missing one, since every consumer of the
// JSON schema trusts it.
func TestStatusJSONReflectsRotate(t *testing.T) {
	home := t.TempDir()
	s := seedState(t, home, "D", "A")
	if _, err := s.Update(func(st *store.State) error {
		for i := range st.Accounts {
			if st.Accounts[i].Name == "A" {
				st.Accounts[i].NoRotate = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	_, out, errb := runHome(t, home, "status", "--json")
	var got status.File
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\nstderr: %s", err, out, errb)
	}
	for _, a := range got.Accounts {
		switch a.Name {
		case "D":
			if !a.Rotate {
				t.Errorf("D.Rotate = false, want true (D is in rotation)")
			}
		case "A":
			if a.Rotate {
				t.Errorf("A.Rotate = true, want false (A is NoRotate)")
			}
		}
	}
}

// TestStatusErrorsGoToStderrNotStdout pins that stdout carries only the
// report (or, with --json, only JSON): `chottag status --json | jq` must
// not choke on an error line mixed into stdout, and every other command in
// this CLI already splits the streams this way.
func TestStatusErrorsGoToStderrNotStdout(t *testing.T) {
	home := t.TempDir()
	code, out, errb := runHome(t, home, "status", "--not-a-real-flag")
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty: a flag-parse error must not print to stdout", out)
	}
	if errb == "" {
		t.Fatal("stderr is empty, want the flag-parse error message")
	}
}

// Fix round 4, item 2: `status` used a bare fs.Parse, which stops at the
// first non-flag token — a stray account name — and returns no error,
// silently leaving it (and anything after it) unchecked. `status` takes no
// positional at all, so any one must be refused with exit 2, never ignored.
func TestStatusRejectsAStrayPositional(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	code, out, errb := runHome(t, home, "status", "stray")
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%q", code, errb)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
}

// The review's exact repro: a stray positional AHEAD of a bogus flag must
// not let the bogus flag go unchecked either — Go's flag.Parse stops
// entirely at the first non-flag token, so a bare fs.Parse never even looks
// at what follows it.
func TestStatusRejectsAStrayPositionalFollowedByABogusFlag(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	code, out, errb := runHome(t, home, "status", "stray", "--bogus")
	if code != 2 {
		t.Fatalf("code = %d, want 2; stderr=%q", code, errb)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
}

// TestStatusJSONLabel pins the install label (R118): top-level, omitted when empty.
func TestStatusJSONLabel(t *testing.T) {
	home := t.TempDir()
	s := seedState(t, home, "D", "A")
	get := func() map[string]any {
		_, out, errb := runHome(t, home, "status", "--json")
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not JSON: %v\n%s\n%s", err, out, errb)
		}
		return got
	}
	if _, ok := get()["label"]; ok {
		t.Error("label present with none set")
	}
	if _, err := s.Update(func(st *store.State) error { st.Label = "dev"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := get()["label"]; got != "dev" {
		t.Errorf("label = %v, want dev", got)
	}
}

// F268: an old reading is shown with its age, not as "unknown", and the
// JSON keeps stale: true with updatedAt.
func TestStatusShowsOldUsageWithItsAge(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	var f status.File
	f.Observe("D", knownSnapshot(0.45, 0.1, time.Now().Add(-2*time.Hour-time.Minute)), noVerdict())
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status")
	if !strings.Contains(out, "45% (2h ago)") || !strings.Contains(out, "10% (2h ago)") {
		t.Fatalf("output = %q (stderr %q); want old readings with their age", out, errb)
	}
	_, js, _ := runHome(t, home, "status", "--json")
	var got status.File
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, a := range got.Accounts {
		if a.Name == "D" && (!a.Stale || a.Usage == nil || a.Usage.UpdatedAt.IsZero()) {
			t.Fatalf("D = %+v, want stale: true and usage.updatedAt kept", a)
		}
	}
}

// R169: the age and STATE window follow the account's rotation, through the
// state.json overlay in runStatus.
func TestStatusWindowFollowsRotation(t *testing.T) {
	home := t.TempDir()
	s := seedState(t, home, "D", "A")
	if _, err := s.Update(func(st *store.State) error {
		for i := range st.Accounts {
			if st.Accounts[i].Name == "A" {
				st.Accounts[i].NoRotate = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var f status.File
	at := time.Now().Add(-time.Hour)
	f.Observe("A", knownSnapshot(0.45, 0.45, at), noVerdict())
	f.Observe("D", knownSnapshot(0.45, 0.45, at), noVerdict())
	saveStatus(t, home, f)
	_, out, errb := runHome(t, home, "status")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "A":
			if strings.Contains(line, "ago") || f[len(f)-1] != "ok" || !strings.Contains(line, "45%") {
				t.Errorf("rotation-off row = %q, want plain 45%% and ok", line)
			}
		case "D":
			if !strings.Contains(line, "45% (1h ago)") || f[len(f)-1] != "stale" {
				t.Errorf("rotating row = %q, want (1h ago) and stale", line)
			}
		}
	}
	if t.Failed() {
		t.Logf("output %q stderr %q", out, errb)
	}
}
