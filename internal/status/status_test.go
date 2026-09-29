package status_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/HaiNNT/c-hottag/internal/creds"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/usage"
)

// saveFile writes f to path via the same two steps status.Save used to run
// in one (F34): status.Marshal, then status.WriteBytes. Save itself is
// gone — nothing in production called it after the split — so every test
// that needs a file on disk to Load back goes through this composition
// directly instead.
func saveFile(t *testing.T, path string, f status.File) {
	t.Helper()
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(path, b); err != nil {
		t.Fatal(err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	in := status.File{Version: 1, Serving: "B", Remote: "A"}
	in.Observe("B", usage.Snapshot{Known: true, At: time.Unix(1789870000, 0),
		FiveHour: usage.Window{Utilization: 0.06, HasUtilization: true, Known: true},
		SevenDay: usage.Window{Utilization: 0.23, HasUtilization: true, Known: true}}, usage.Verdict{})
	saveFile(t, p, in)
	out, err := status.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Usage == nil {
		t.Fatalf("accounts = %+v, want one with usage", out.Accounts)
	}
	if p := out.Accounts[0].Usage.FiveHourPct; p == nil || *p != 6 {
		t.Errorf("5h pct = %v, want 6 (0-100 wire units, not the 0-1 fraction)", p)
	}
	if out.Accounts[0].Usage.Source != "observed" {
		t.Errorf("source = %q, want observed", out.Accounts[0].Usage.Source)
	}
}

func TestLoadTreatsAMissingFileAsEmptyNotAnError(t *testing.T) {
	f, err := status.Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("err = %v; a missing cache must not be an error", err)
	}
	if len(f.Accounts) != 0 {
		t.Error("want empty file")
	}
}

func TestLoadTreatsACorruptFileAsEmptyNotAnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := status.Load(p); err != nil {
		t.Fatalf("err = %v; a corrupt cache must be discarded, not fatal", err)
	}
}

func TestFreshGoesStaleAfterTheWindow(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("B", usage.Snapshot{Known: true, At: base}, usage.Verdict{})
	if !f.Fresh("B", base.Add(status.StaleAfter-time.Second)) {
		t.Error("should still be fresh just inside the window")
	}
	if f.Fresh("B", base.Add(status.StaleAfter+time.Second)) {
		t.Error("should be stale just outside the window")
	}
}

func TestFreshIsFalseExactlyAtStaleAfter(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("B", usage.Snapshot{Known: true, At: base}, usage.Verdict{})
	if f.Fresh("B", base.Add(status.StaleAfter)) {
		t.Error("exactly StaleAfter old should already be stale")
	}
}

func TestFreshIsFalseForAnAccountWithNoUsage(t *testing.T) {
	var f status.File
	f.SetPassthrough("B", "home login")
	if f.Fresh("B", time.Unix(1789870000, 0)) {
		t.Error("an account with a row but no Usage must not read as fresh")
	}
}

func TestFreshIsFalseForAnUnknownAccount(t *testing.T) {
	var f status.File
	if f.Fresh("nope", time.Unix(1789870000, 0)) {
		t.Error("an account not in the file must not read as fresh")
	}
}

func TestObserveRecordsALimitAndTheRollUp(t *testing.T) {
	base := time.Unix(1789870000, 0)
	until := time.Unix(1790179200, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base},
		usage.Verdict{Limited: true, Until: until, Window: "seven_day"})
	if !f.Accounts[0].Limited || !f.Accounts[0].LimitedUntil.Equal(until) {
		t.Fatalf("account = %+v, want limited until %v", f.Accounts[0], until)
	}
	if !f.Limits.AllLimited {
		t.Error("one account, and it is limited -> AllLimited")
	}
	if !f.Limits.NextReset.Equal(until) {
		t.Errorf("NextReset = %v, want %v", f.Limits.NextReset, until)
	}
	if f.Limits.NextResetAccount != "C" {
		t.Errorf("NextResetAccount = %q, want C", f.Limits.NextResetAccount)
	}
}

func TestAllLimitedIsFalseWhenAnyAccountHasQuota(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: base.Add(time.Hour)})
	f.Observe("D", usage.Snapshot{Known: true, At: base}, usage.Verdict{})
	if f.Limits.AllLimited {
		t.Fatal("AllLimited = true while D has quota")
	}
}

// A zero LimitedUntil means "clearing time unknown", not "clears first". If
// the roll-up let it win regardless of observation order, an account with a
// known reset time observed AFTER one with an unknown reset would get its
// real NextReset clobbered back to zero.
func TestRollUpSkipsAnUnknownUntilEvenWhenObservedFirst(t *testing.T) {
	base := time.Unix(1789870000, 0)
	until := time.Unix(1790179200, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: time.Time{}})
	f.Observe("D", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: until})
	if !f.Limits.NextReset.Equal(until) {
		t.Errorf("NextReset = %v, want %v (the unknown-until account must not win)", f.Limits.NextReset, until)
	}
	if f.Limits.NextResetAccount != "D" {
		t.Errorf("NextResetAccount = %q, want D", f.Limits.NextResetAccount)
	}
}

// The reverse order is the one that actually discriminates: with a real
// Until observed first, a naive "first Limited account wins" or "unguarded
// next.IsZero() || a.LimitedUntil.Before(next)" implementation lets the
// zero-Until account observed second overwrite it, since next.IsZero() is
// false but the zero-skip guard is what stops it from being compared at
// all. Reversing TestRollUpSkipsAnUnknownUntilEvenWhenObservedFirst's order
// does NOT discriminate (next starts zero either way), so this case is the
// one that must exist.
func TestRollUpSkipsAnUnknownUntilObservedSecond(t *testing.T) {
	base := time.Unix(1789870000, 0)
	until := time.Unix(1790179200, 0)
	var f status.File
	f.Observe("D", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: until})
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: time.Time{}})
	if !f.Limits.NextReset.Equal(until) {
		t.Errorf("NextReset = %v, want %v (a later unknown-until account must not overwrite it)", f.Limits.NextReset, until)
	}
	if f.Limits.NextResetAccount != "D" {
		t.Errorf("NextResetAccount = %q, want D", f.Limits.NextResetAccount)
	}
}

// rollUp (the write path) never expires a limit: nothing else would ever
// clear it (M1c has no poll), so a limit whose reset time has already
// passed must still read as limited to Observe/EnsureAccounts/SetPassthrough
// — it is RollUpAt, not rollUp, that a report consumer uses to stop
// asserting a stale reset (see TestRollUpAt* below).
func TestRollUpDoesNotExpireAPastLimitedUntil(t *testing.T) {
	base := time.Unix(1789870000, 0)
	past := base.Add(-time.Hour)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: past})
	if !f.Limits.AllLimited {
		t.Fatal("AllLimited = false; rollUp must not expire a past LimitedUntil on the write path")
	}
	if !f.Limits.NextReset.Equal(past) {
		t.Errorf("NextReset = %v, want %v unchanged", f.Limits.NextReset, past)
	}
}

// RollUpAt must stop asserting a reset date that has already gone by: an
// account limited until a time before now is no longer confirmed limited (no
// poll will ever re-check it), so it must not hold AllLimited true or pin
// NextReset to a date already in the past — the self-sustaining trap this
// fix exists to break.
func TestRollUpAtExpiresAPastLimitedUntil(t *testing.T) {
	base := time.Unix(1789870000, 0)
	past := base.Add(-time.Hour)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: past})
	f.RollUpAt(base)
	if f.Limits.AllLimited {
		t.Fatal("AllLimited = true; a LimitedUntil already in the past must not count as confirmed limited")
	}
	if !f.Limits.NextReset.IsZero() {
		t.Errorf("NextReset = %v, want zero; an expired reset must not be asserted as the next one", f.Limits.NextReset)
	}
}

// A LimitedUntil still in the future must still count, so RollUpAt does not
// simply clear every limit regardless of the clock.
func TestRollUpAtKeepsAFutureLimitedUntil(t *testing.T) {
	base := time.Unix(1789870000, 0)
	future := base.Add(time.Hour)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: future})
	f.RollUpAt(base)
	if !f.Limits.AllLimited {
		t.Fatal("AllLimited = false; a LimitedUntil still in the future must still count as confirmed limited")
	}
	if !f.Limits.NextReset.Equal(future) {
		t.Errorf("NextReset = %v, want %v", f.Limits.NextReset, future)
	}
}

// A zero LimitedUntil means UNKNOWN, not expired (§6.2): RollUpAt must not
// treat it as a past reset just because it compares before "now" as a raw
// time.Time value.
func TestRollUpAtDoesNotExpireAZeroLimitedUntil(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true})
	f.RollUpAt(base)
	if !f.Limits.AllLimited {
		t.Fatal("AllLimited = false; a zero (unknown) LimitedUntil must still count as confirmed limited")
	}
}

// An unknown snapshot must not silently clear a limit we already know about,
// and must not touch the usage data or freshness clock either: "I learned
// nothing" must leave everything exactly where it was.
func TestObserveWithAnUnknownSnapshotKeepsAKnownLimit(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base,
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 0.5}},
		usage.Verdict{Limited: true, Until: base.Add(time.Hour)})
	f.Observe("C", usage.Snapshot{Known: false, At: base.Add(time.Minute)}, usage.Verdict{})
	if !f.Accounts[0].Limited {
		t.Fatal("a headerless response cleared a known limit; it must leave it alone")
	}
	if p := f.Accounts[0].Usage.FiveHourPct; p == nil || *p != 50 {
		t.Errorf("5h pct = %v, want 50 unchanged; an unknown snapshot must not wipe it", p)
	}
	if !f.Accounts[0].Usage.UpdatedAt.Equal(base) {
		t.Errorf("UpdatedAt = %v, want %v unchanged; an unknown snapshot must not refresh staleness", f.Accounts[0].Usage.UpdatedAt, base)
	}
}

// Overall must say "allowed" explicitly to clear a limit. An empty or
// unrecognised Overall is not positive evidence of health.
func TestObserveDoesNotClearALimitOnAnEmptyOverall(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: base.Add(time.Hour)})
	f.Observe("C", usage.Snapshot{Known: true, At: base.Add(time.Minute), Overall: ""}, usage.Verdict{})
	if !f.Accounts[0].Limited {
		t.Fatal("an empty Overall cleared a known limit; only an explicit \"allowed\" may")
	}
}

// This is the mirror of TestObserveWithAnUnknownSnapshotKeepsAKnownLimit: a
// response that reports only Unified-Status: allowed (its 5h headers absent
// this round) must not wipe a known 5h percentage to nil, and must not clear
// a limit that the SAME response's own 7d window still reports rejected.
// Replacing Usage wholesale, and gating the clear on s.Overall alone, both
// produced exactly this: a row that reads "7d 100% ... ok".
func TestObserveMergesUsagePerWindowAndDoesNotClearOnAMirroredOverall(t *testing.T) {
	base := time.Unix(1789870000, 0)
	until := base.Add(time.Hour)
	var f status.File
	f.Observe("C", usage.Snapshot{
		Known: true, At: base, Overall: "rejected",
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 0.42, Status: "allowed"},
		SevenDay: usage.Window{Known: true, HasUtilization: true, Utilization: 1.0, Status: "rejected"},
	}, usage.Verdict{Limited: true, Until: until, Window: "seven_day"})

	// A later response: overall says allowed, and carries no 5h headers at
	// all this round, but its own 7d window is STILL rejected at 100%.
	f.Observe("C", usage.Snapshot{
		Known: true, At: base.Add(time.Minute), Overall: "allowed",
		SevenDay: usage.Window{Known: true, HasUtilization: true, Utilization: 1.0, Status: "rejected"},
	}, usage.Verdict{})

	if p := f.Accounts[0].Usage.FiveHourPct; p == nil || *p != 42 {
		t.Errorf("5h pct = %v, want 42 unchanged; a response silent on 5h must not wipe it", p)
	}
	if !f.Accounts[0].Limited {
		t.Fatal("Limited = false; the response's own 7d window still says rejected, so Overall: allowed must not clear it")
	}
}

// The positive case that TestObserveMergesUsagePerWindowAndDoesNotClearOnAMirroredOverall
// must not accidentally make impossible to reach: when the window that did
// the limiting genuinely reports allowed, the limit DOES clear.
func TestObserveClearsALimitWhenTheLimitingWindowReportsAllowed(t *testing.T) {
	base := time.Unix(1789870000, 0)
	until := base.Add(time.Hour)
	var f status.File
	f.Observe("C", usage.Snapshot{
		Known: true, At: base, Overall: "rejected",
		SevenDay: usage.Window{Known: true, HasUtilization: true, Utilization: 1.0, Status: "rejected"},
	}, usage.Verdict{Limited: true, Until: until, Window: "seven_day"})

	f.Observe("C", usage.Snapshot{
		Known: true, At: base.Add(time.Minute), Overall: "allowed",
		SevenDay: usage.Window{Known: true, HasUtilization: true, Utilization: 0.1, Status: "allowed"},
	}, usage.Verdict{})

	if f.Accounts[0].Limited {
		t.Fatal("Limited = true; the limiting window itself now reports allowed, so the limit should have cleared")
	}
}

func TestSetPassthroughRecordsTheReason(t *testing.T) {
	var f status.File
	f.SetPassthrough("B", "home login")
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough != "home login" {
		t.Fatalf("accounts = %+v, want one with passthrough %q", f.Accounts, "home login")
	}
}

// TestSetTokenStampsTokenAtOnlyWhenANonNeedsLoginStateChanges pins fix
// round 1 item 1's rule for every state EXCEPT needs-login: TokenAt records
// when Token last changed, not every call, so a caller that re-asserts the
// same OK/expiring state without a real change does not push the
// timestamp forward.
func TestSetTokenStampsTokenAtOnlyWhenANonNeedsLoginStateChanges(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.SetToken("B", creds.StateOK, base)
	if f.Accounts[0].Token != creds.StateOK || !f.Accounts[0].TokenAt.Equal(base) {
		t.Fatalf("accounts = %+v, want ok stamped at %v", f.Accounts, base)
	}
	later := base.Add(time.Hour)
	f.SetToken("B", creds.StateOK, later)
	if !f.Accounts[0].TokenAt.Equal(base) {
		t.Fatalf("TokenAt = %v, want unchanged at %v: the state did not change", f.Accounts[0].TokenAt, base)
	}
	f.SetToken("B", creds.StateNeedsLogin, later)
	if f.Accounts[0].Token != creds.StateNeedsLogin || !f.Accounts[0].TokenAt.Equal(later) {
		t.Fatalf("accounts = %+v, want needs-login stamped at %v", f.Accounts, later)
	}
}

// TestSetTokenReStampsTokenAtOnNeedsLoginEvenWhenUnchanged pins item 4
// (review round 2): a needs-login observed again after a re-login must
// still count. planAccounts treats a needs-login as cleared once
// LoggedInAt is after TokenAt (fix round 1 item 1), so a needs-login that
// persists ACROSS a re-login — the daemon re-observes it, unchanged, after
// LoggedInAt has moved forward — must re-stamp TokenAt, or it silently
// reads as stale and clears even though the daemon just confirmed it
// again. A non-needs-login state keeps the old rule (pinned above):
// TokenAt records only a real change.
func TestSetTokenReStampsTokenAtOnNeedsLoginEvenWhenUnchanged(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.SetToken("B", creds.StateNeedsLogin, base)
	if f.Accounts[0].Token != creds.StateNeedsLogin || !f.Accounts[0].TokenAt.Equal(base) {
		t.Fatalf("accounts = %+v, want needs-login stamped at %v", f.Accounts, base)
	}
	later := base.Add(time.Hour)
	f.SetToken("B", creds.StateNeedsLogin, later)
	if !f.Accounts[0].TokenAt.Equal(later) {
		t.Fatalf("TokenAt = %v, want re-stamped at %v: a needs-login observed again must still count, even unchanged", f.Accounts[0].TokenAt, later)
	}
}

// AllLimited is only meaningful once every configured account has a row:
// computed over just the accounts the cache happens to have observed, one
// limited account among untouched others reads as "all accounts limited".
func TestEnsureAccountsMakesAllLimitedHonest(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: base.Add(time.Hour)})
	f.EnsureAccounts([]string{"B", "C", "D"})
	if f.Limits.AllLimited {
		t.Fatal("AllLimited = true with B and D unobserved and unlimited")
	}
	if len(f.Accounts) != 3 {
		t.Fatalf("accounts = %+v, want a row for each of B, C, D", f.Accounts)
	}
}

func TestEnsureAccountsDoesNotDuplicateAnAlreadyObservedAccount(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("C", usage.Snapshot{Known: true, At: base}, usage.Verdict{})
	f.EnsureAccounts([]string{"C"})
	if len(f.Accounts) != 1 {
		t.Fatalf("accounts = %+v, want C only once", f.Accounts)
	}
}

func TestEnsureAccountsPrunesRowsForRemovedAccounts(t *testing.T) {
	var f status.File
	f.EnsureAccounts([]string{"A", "B", "C"})
	f.Observe("C", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Limited: true, Until: time.Now().Add(time.Hour), Window: "seven_day"})

	// C is logged out and removed from state.json.
	f.EnsureAccounts([]string{"A", "B"})

	for _, a := range f.Accounts {
		if a.Name == "C" {
			t.Fatal("a row survived for an account no longer in state.json")
		}
	}
}

// The ghost row's real cost: it can win nextResetAccount and make status
// announce a reset for an account the user no longer has (R27's path).
func TestAPrunedAccountCannotWinNextReset(t *testing.T) {
	now := time.Now()
	var f status.File
	f.EnsureAccounts([]string{"A", "C"})
	// C is limited and resets sooner than A.
	f.Observe("A", usage.Snapshot{Known: true, At: now},
		usage.Verdict{Limited: true, Until: now.Add(2 * time.Hour), Window: "seven_day"})
	f.Observe("C", usage.Snapshot{Known: true, At: now},
		usage.Verdict{Limited: true, Until: now.Add(1 * time.Hour), Window: "seven_day"})
	if f.Limits.NextResetAccount != "C" {
		t.Fatalf("precondition failed: NextResetAccount = %q, want C", f.Limits.NextResetAccount)
	}

	f.EnsureAccounts([]string{"A"})
	if f.Limits.NextResetAccount == "C" {
		t.Error("a removed account still wins NextResetAccount")
	}
}

// An empty configured set is a legitimate state (the user removed their last
// account, and state.json was written atomically), not a torn read — names
// is the authority, so EnsureAccounts(nil) prunes every row, not just some.
func TestEnsureAccountsWithNoNamesPrunesEverything(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: base.Add(time.Hour)})
	f.Observe("B", usage.Snapshot{Known: true, At: base}, usage.Verdict{})

	f.EnsureAccounts(nil)

	if len(f.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want none left after EnsureAccounts(nil)", f.Accounts)
	}
	if f.Limits != (status.Limits{}) {
		t.Errorf("Limits = %+v, want zeroed after every account is pruned", f.Limits)
	}
}

// Pruning must compare case-insensitively, exactly like every other lookup
// in this file (has, account): an account registered as "alice" and
// reconfigured as "Alice" (or vice versa) must survive, not be pruned as if
// it were a different account.
//
// The kept row takes the configured spelling (F175): state.json is the
// authority on the name, so a case-only rename must not leave the old
// spelling in status.json forever.
func TestEnsureAccountsPruningIsCaseInsensitive(t *testing.T) {
	var f status.File
	f.EnsureAccounts([]string{"alice"})
	f.EnsureAccounts([]string{"Alice"})
	if len(f.Accounts) != 1 || f.Accounts[0].Name != "Alice" {
		t.Fatalf("accounts = %+v, want one row, respelled \"Alice\" as configured", f.Accounts)
	}
}

// limitedRow makes a File whose rows are the given members, with name
// limited until an hour from base.
func limitedRow(t *testing.T, base time.Time, members []status.Member, limited string) status.File {
	t.Helper()
	var f status.File
	f.EnsureRoster(members)
	f.Observe(limited, usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: base.Add(time.Hour), Window: "five_hour"})
	return f
}

func rowNamed(f *status.File, name string) *status.Account {
	for i := range f.Accounts {
		if f.Accounts[i].Name == name {
			return &f.Accounts[i]
		}
	}
	return nil
}

// F171: a rename keeps the slot's dir, so the row follows the dir and keeps
// the account's recorded limit instead of being pruned and re-seeded empty.
func TestEnsureRosterCarriesARowAcrossARename(t *testing.T) {
	base := time.Unix(1789870000, 0)
	f := limitedRow(t, base, []status.Member{{Name: "A", Dir: "/s/a"}, {Name: "C", Dir: "/s/c"}}, "A")
	f.EnsureRoster([]status.Member{{Name: "work", Dir: "/s/a"}, {Name: "C", Dir: "/s/c"}})
	if len(f.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want 2", f.Accounts)
	}
	w := rowNamed(&f, "work")
	if w == nil || !w.Limited || w.Dir != "/s/a" {
		t.Fatalf("work row = %+v, want A's limited row carried across under the new name", w)
	}
	if rowNamed(&f, "A") != nil {
		t.Fatalf("an \"A\" row survived the rename: %+v", f.Accounts)
	}
}

// Two accounts swapping names in one step: rows follow their dirs, not
// their old names.
func TestEnsureRosterFollowsDirsThroughANameSwap(t *testing.T) {
	base := time.Unix(1789870000, 0)
	f := limitedRow(t, base, []status.Member{{Name: "A", Dir: "/s/a"}, {Name: "B", Dir: "/s/b"}}, "A")
	f.EnsureRoster([]status.Member{{Name: "B", Dir: "/s/a"}, {Name: "A", Dir: "/s/b"}})
	if b := rowNamed(&f, "B"); b == nil || !b.Limited || b.Dir != "/s/a" {
		t.Fatalf("B row = %+v, want /s/a's limited row", b)
	}
	if a := rowNamed(&f, "A"); a == nil || a.Limited || a.Dir != "/s/b" {
		t.Fatalf("A row = %+v, want /s/b's unlimited row", a)
	}
}

// A new slot that reuses a removed account's name is a different login: it
// must not inherit the old slot's limit.
func TestEnsureRosterDoesNotHandANewSlotTheOldSlotsRow(t *testing.T) {
	base := time.Unix(1789870000, 0)
	f := limitedRow(t, base, []status.Member{{Name: "B", Dir: "/s/old"}}, "B")
	f.EnsureRoster([]status.Member{{Name: "B", Dir: "/s/new"}})
	if len(f.Accounts) != 1 || f.Accounts[0].Limited || f.Accounts[0].Dir != "/s/new" {
		t.Fatalf("accounts = %+v, want one fresh unlimited B row for /s/new", f.Accounts)
	}
}

// A row written before rows carried a dir is matched by name once, and
// takes the dir from then on.
func TestEnsureRosterAdoptsARowWithNoDirByName(t *testing.T) {
	base := time.Unix(1789870000, 0)
	var f status.File
	f.EnsureAccounts([]string{"A"})
	f.Observe("A", usage.Snapshot{Known: true, At: base}, usage.Verdict{Limited: true, Until: base.Add(time.Hour)})
	f.EnsureRoster([]status.Member{{Name: "A", Dir: "/s/a"}})
	if len(f.Accounts) != 1 || !f.Accounts[0].Limited || f.Accounts[0].Dir != "/s/a" {
		t.Fatalf("accounts = %+v, want the old A row, now with dir /s/a", f.Accounts)
	}
	f.EnsureRoster([]status.Member{{Name: "work", Dir: "/s/a"}})
	if len(f.Accounts) != 1 || f.Accounts[0].Name != "work" || !f.Accounts[0].Limited {
		t.Fatalf("accounts = %+v, want the row renamed to work, still limited", f.Accounts)
	}
}

// EnsureAccounts (names only) never drops a row's dir.
func TestEnsureAccountsKeepsARowsDir(t *testing.T) {
	var f status.File
	f.EnsureRoster([]status.Member{{Name: "A", Dir: "/s/a"}})
	f.EnsureAccounts([]string{"A"})
	if len(f.Accounts) != 1 || f.Accounts[0].Dir != "/s/a" {
		t.Fatalf("accounts = %+v, want A's dir kept", f.Accounts)
	}
}

// Observe's do-not-create guard must be reachable: SetPassthrough with an
// empty reason is a clear, not a create.
func TestSetPassthroughEmptyDoesNotCreateARow(t *testing.T) {
	var f status.File
	f.SetPassthrough("Unknown", "")
	if len(f.Accounts) != 0 {
		t.Fatalf("Accounts = %d after clearing passthrough on an unknown account, want 0", len(f.Accounts))
	}
}

func TestSetPassthroughWithAReasonStillCreates(t *testing.T) {
	var f status.File
	f.SetPassthrough("A", "stale token")
	if len(f.Accounts) != 1 || f.Accounts[0].Passthrough != "stale token" {
		t.Fatalf("Accounts = %+v, want one row carrying the reason", f.Accounts)
	}
}

// A clear on an account that DOES already have a row must still take effect:
// the empty-reason no-op in SetPassthrough is gated on !f.has(account), not
// on reason alone, so an existing account recovering from passthrough must
// see its mark actually cleared, not silently kept.
func TestSetPassthroughClearsAnExistingReason(t *testing.T) {
	var f status.File
	f.SetPassthrough("A", "stale token")
	f.SetPassthrough("A", "")
	if got := f.Accounts[0].Passthrough; got != "" {
		t.Errorf("Passthrough = %q after clearing, want empty", got)
	}
}

func TestObserveWithAnUnknownSnapshotDoesNotCreateARow(t *testing.T) {
	var f status.File
	f.Observe("ghost", usage.Snapshot{Known: false}, usage.Verdict{})
	if len(f.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want none; a headerless response for an unseen account must not create a row", f.Accounts)
	}
}

// The same guard, but with the exact Verdict shape Classify's headerless
// refusal branch actually returns (Refusal: true, a fixed Reason): an
// account chottag has never observed must not get a row just because the
// one response it produced happened to be an unclassifiable refusal.
func TestObserveWithAHeaderlessRefusalForAnUnknownAccountDoesNotCreateARow(t *testing.T) {
	var f status.File
	f.Observe("ghost", usage.Snapshot{Known: false}, usage.Verdict{
		Refusal: true,
		Reason:  "refusal carried no unified rate-limit headers; not treated as a limit",
	})
	if len(f.Accounts) != 0 {
		t.Fatalf("accounts = %+v, want none; a headerless refusal for an unseen account must not create a row", f.Accounts)
	}
}

// This is fix round item 2's central case: Classify's headerless-refusal
// branch is, by construction, the !s.Known case — a 500, a 401, a gateway
// error, exactly where the operator has nothing else to read. If Reason
// only updates inside `if s.Known`, this case can never set it. The
// account must already have a row (from an earlier observation) for
// Observe to touch it at all — an unseen account is covered by
// TestObserveWithAHeaderlessRefusalForAnUnknownAccountDoesNotCreateARow.
func TestObserveSetsReasonOnAHeaderlessRefusalForAKnownAccount(t *testing.T) {
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()}, usage.Verdict{})
	f.Observe("A", usage.Snapshot{Known: false}, usage.Verdict{
		Refusal: true,
		Reason:  "headerless refusal",
	})
	if got := f.Accounts[0].Reason; got != "headerless refusal" {
		t.Errorf("Reason = %q, want the headerless refusal's reason", got)
	}
}

// The mirror of the above: a healthy response that also carries no unified
// headers (Known=false, Refusal=false — e.g. a 200 with no rate-limit
// headers at all) must still clear a stale reason, exactly as a healthy
// KNOWN response does. Gating the clear on s.Known would leave an old
// reason describing a response that is no longer the last one.
func TestObserveClearsReasonOnAHeaderlessHealthyResponse(t *testing.T) {
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()}, usage.Verdict{
		Refusal: true,
		Reason:  "unified status is allowed; not a quota refusal",
	})
	if got := f.Accounts[0].Reason; got == "" {
		t.Fatal("test setup: Reason should be set by the refusal before the recovery this test checks")
	}
	f.Observe("A", usage.Snapshot{Known: false}, usage.Verdict{})
	if got := f.Accounts[0].Reason; got != "" {
		t.Errorf("Reason = %q after a headerless healthy response, want cleared", got)
	}
}

func TestSaveIsAtomicAndPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	saveFile(t, p, status.File{Version: 1})
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	ents, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Errorf("left %d files behind, want just the cache (no temp file)", len(ents))
	}
}

// omitempty has no effect on a struct type: a zero time.Time must be tagged
// omitzero, or it serialises as "0001-01-01T00:00:00Z", which a non-Go
// reader (jq, a statusline script) compares against the clock and concludes
// the limit already lifted. This must check the raw bytes: a Go round-trip
// test passes either way and proves nothing about the wire format.
func TestSaveOmitsUnknownTimesEntirelyRatherThanAsYearOne(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	var f status.File
	// FiveHour window known but its own reset unreported; SevenDay window not
	// reported at all; no limit -> LimitedUntil unknown; no limited accounts
	// -> Limits.NextReset unknown.
	f.Observe("B", usage.Snapshot{Known: true, At: time.Unix(1789870000, 0),
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 0.1}},
		usage.Verdict{})
	saveFile(t, p, f)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	limits, _ := m["limits"].(map[string]any)
	if _, ok := limits["nextReset"]; ok {
		t.Errorf("limits.nextReset present, want key absent (unknown, not year 1); file:\n%s", raw)
	}
	accounts, _ := m["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	acct, _ := accounts[0].(map[string]any)
	if _, ok := acct["limitedUntil"]; ok {
		t.Errorf("limitedUntil present, want key absent; file:\n%s", raw)
	}
	usageObj, _ := acct["usage"].(map[string]any)
	if _, ok := usageObj["fiveHourResetsAt"]; ok {
		t.Errorf("fiveHourResetsAt present, want key absent (window known but reset unreported); file:\n%s", raw)
	}
	if _, ok := usageObj["sevenDayResetsAt"]; ok {
		t.Errorf("sevenDayResetsAt present, want key absent (window not reported at all); file:\n%s", raw)
	}
}

// Marshal must fill in Version when the caller leaves it zero, or the first
// real write lands "version": 0 on disk. This must check the raw bytes, and
// the input File must have a genuinely zero Version: Load also defaults
// Version to 1, so building f via Load(missing) would pass even with
// Marshal's own default removed, because f.Version would already be 1
// before Marshal ever saw it.
func TestSaveDefaultsVersionWhenUnset(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	var f status.File
	f.Observe("B", usage.Snapshot{Known: true, At: time.Unix(1789870000, 0)}, usage.Verdict{})
	if f.Version != 0 {
		t.Fatalf("test setup: f.Version = %d, want 0 so this test actually exercises Marshal's default", f.Version)
	}
	saveFile(t, p, f)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"version": 1`)) {
		t.Errorf("raw file does not contain \"version\": 1; file:\n%s", raw)
	}
}

// Token is a creds.TokenState, never a bare string: §6.1 forbids a bearer
// landing in this file, and the named type makes writing one unrepresentable
// rather than one careless assignment away. This also pins the wire format:
// it round-trips as the state string, not a struct or number.
func TestTokenFieldIsATokenStateAndRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "status.json")
	in := status.File{Version: 1, Accounts: []status.Account{{Name: "B", Token: creds.StateExpiring}}}
	saveFile(t, p, in)
	out, err := status.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Token != creds.StateExpiring {
		t.Fatalf("accounts = %+v, want Token = %v", out.Accounts, creds.StateExpiring)
	}
}

func TestSetDaemonStampsCountersAndHeartbeat(t *testing.T) {
	now := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	var f status.File
	f.SetDaemon(47821, 3, 5, 7, now)

	if f.Daemon == nil {
		t.Fatal("SetDaemon left File.Daemon nil")
	}
	if f.Daemon.Port != 47821 {
		t.Errorf("Port = %d, want 47821", f.Daemon.Port)
	}
	if !f.Daemon.Heartbeat.Equal(now) {
		t.Errorf("Heartbeat = %v, want %v", f.Daemon.Heartbeat, now)
	}
	if f.Daemon.RouteDrift != 3 || f.Daemon.ZeroIDExtractions != 5 || f.Daemon.OwnerWriteDrops != 7 {
		t.Errorf("counters = %d/%d/%d, want 3/5/7",
			f.Daemon.RouteDrift, f.Daemon.ZeroIDExtractions, f.Daemon.OwnerWriteDrops)
	}
	// Running IS stored (always false) but never READ BACK: a document
	// written moments before the daemon was killed would otherwise outlive
	// it saying "running: true" if that stored value were ever trusted.
	if f.Daemon.Running {
		t.Error("SetDaemon stored Running = true; it must only ever be computed at read time")
	}
}

// A previously stored running: true (a hand-edited file, or one left over
// from before F94's fix round 3 R6) must not survive a fresh SetDaemon call:
// without an explicit clear, it would sit unchanged in a long-lived daemon
// process's in-memory File and be re-marshalled as true on every subsequent
// stamp, since nothing else in the daemon's own write path ever looks at it.
func TestSetDaemonClearsAPreviouslyStoredRunning(t *testing.T) {
	now := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	f := status.File{Daemon: &status.Daemon{Running: true}}
	f.SetDaemon(47821, 0, 0, 0, now)
	if f.Daemon.Running {
		t.Error("SetDaemon left a previously stored Running = true in place, want it cleared")
	}
}

func TestDaemonRunningAtUsesHeartbeatAge(t *testing.T) {
	base := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"fresh", 0, true},
		{"just inside", status.DaemonStaleAfter - time.Millisecond, true},
		{"exactly at the boundary", status.DaemonStaleAfter, false},
		{"well past", 5 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f status.File
			f.SetDaemon(47821, 0, 0, 0, base)
			f.DaemonRunningAt(base.Add(tc.age))
			if f.Daemon.Running != tc.want {
				t.Errorf("Running = %v at age %v, want %v", f.Daemon.Running, tc.age, tc.want)
			}
		})
	}
}

// A home where no daemon ever ran must emit no daemon object at all, not one
// full of zeroes: absent means unknown, exactly as it does for a percentage.
func TestNoDaemonObjectWhenNoneEverRan(t *testing.T) {
	var f status.File
	f.DaemonRunningAt(time.Now())
	if f.Daemon != nil {
		t.Fatalf("Daemon = %+v, want nil when SetDaemon was never called", f.Daemon)
	}
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"daemon"`)) {
		t.Errorf("marshalled document contains a daemon key:\n%s", b)
	}
}

func TestObserveRecordsTheVerdictReason(t *testing.T) {
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()}, usage.Verdict{
		Limited: true,
		Window:  "seven_day",
		Reason:  "window seven_day rejected",
		Refusal: true,
	})
	if len(f.Accounts) != 1 {
		t.Fatalf("Accounts = %d, want 1", len(f.Accounts))
	}
	if got := f.Accounts[0].Reason; got != "window seven_day rejected" {
		t.Errorf("Reason = %q, want the verdict's reason", got)
	}
}

// Reason is bounded independently of usage.sanitize's own cap, so §6.1 does
// not rest on a bound enforced in another package.
//
// NOTE on what this does NOT claim. An earlier draft of this test also
// asserted the stored value cannot contain "sk-ant", which TRUNCATION CANNOT
// DELIVER: truncating "sk-ant-oat01-xxx…" to 64 characters still starts with
// "sk-ant" (the cap is 80 now, but the point is independent of its value).
// The assertion was unsatisfiable by the implementation prescribed
// three steps below, and the test would have failed with correct code.
//
// The real guarantee is structural, and belongs in a comment rather than a
// fake assertion: Verdict.Reason's only writers are the five sites in
// internal/usage/usage.go, each building a fixed string plus sanitize()d
// fragments — no server or user text reaches it unsanitised. If you ever add
// a writer that does, bounding will not save you and this test will not
// notice; sanitise at that writer.
func TestReasonIsBounded(t *testing.T) {
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Limited: true, Reason: strings.Repeat("x", 200), Refusal: true})
	if got := len(f.Accounts[0].Reason); got > 80 {
		t.Errorf("Reason length %d, want it bounded to maxReason (80)", got)
	}
}

// maxReason must be large enough for the reason an operator is most likely
// to need. Classify's headerless-refusal branch is the one that fires when
// there is nothing ELSE to read — no unified headers, so no window, no
// reset time, nothing but this sentence — and the final fix round is what
// made it reachable at all. At the original maxReason of 64 its 69-byte
// text arrived cut mid-phrase ("...not treated as a "). Driven through the
// real Classify rather than a literal, so a future edit to that string is
// caught here rather than silently re-truncated.
func TestTheHeaderlessRefusalReasonSurvivesTheBoundIntact(t *testing.T) {
	v := usage.Classify(http.StatusInternalServerError, http.Header{}, time.Now())
	if !v.Refusal {
		t.Fatalf("precondition failed: Classify(500, no headers) gave Refusal=false (%+v)", v)
	}
	var f status.File
	f.Observe("A", usage.Snapshot{At: time.Now()}, v)
	f.EnsureAccounts([]string{"A"})
	f.Observe("A", usage.Snapshot{At: time.Now()}, v)
	if got := f.Accounts[0].Reason; got != v.Reason {
		t.Errorf("stored reason %q (%d bytes), want the whole of %q (%d bytes): the one refusal with no other evidence must not arrive truncated",
			got, len(got), v.Reason, len(v.Reason))
	}
}

// The bound must land on a rune boundary, not a byte offset (fix round 2,
// item 5): a naive s[:64] can split a multi-byte rune in two, leaving a
// half-rune in status.json. This does not rely on usage.sanitize's ASCII
// output — that is exactly the dependency Account.Reason's doc comment says
// this bound refuses to lean on — so the input here is deliberately
// multi-byte. "x" followed by forty 2-byte "é" runes is 81 bytes long: the
// runes start at odd offsets 1, 3, 5, …, so byte offset 80 (maxReason) falls
// in the middle of one of them.
func TestReasonTruncatesOnARuneBoundary(t *testing.T) {
	reason := "x" + strings.Repeat("é", 40)
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Refusal: true, Reason: reason})
	got := f.Accounts[0].Reason
	if !utf8.ValidString(got) {
		t.Fatalf("Reason = %q is not valid UTF-8", got)
	}
	if len(got) > 80 {
		t.Errorf("Reason length %d, want it bounded to maxReason (80)", len(got))
	}
	if !strings.HasPrefix(reason, got) {
		t.Errorf("Reason = %q, want a prefix of the original", got)
	}
}

// A healthy response (v.Refusal false — Classify's status < 400 branch)
// must leave reason absent, not "not a refusal" on nearly every active
// account: §5.1 says reason is present on a refusal, not on every response
// (fix round 1, item 5 — the field's own justification, R33, is about
// refusals chottag declines to classify, and a healthy account is neither).
func TestObserveLeavesReasonAbsentOnAHealthyResponse(t *testing.T) {
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Reason: "not a refusal"})
	if got := f.Accounts[0].Reason; got != "" {
		t.Errorf("Reason = %q, want absent for a non-refusal (v.Refusal=false)", got)
	}
}

// The clearing case is the one most likely to be missed (fix round 1, item
// 5): a recovered account must not keep displaying the reason for a
// refusal that no longer applies.
func TestObserveClearsReasonOnRecovery(t *testing.T) {
	var f status.File
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Reason: "unified status is x; not a quota refusal", Refusal: true})
	if got := f.Accounts[0].Reason; got == "" {
		t.Fatal("test setup: Reason should be set by the refusal before the recovery this test checks")
	}
	f.Observe("A", usage.Snapshot{Known: true, At: time.Now()},
		usage.Verdict{Reason: "not a refusal"})
	if got := f.Accounts[0].Reason; got != "" {
		t.Errorf("Reason = %q after a healthy response, want cleared", got)
	}
}

// §6.1 made this mistake unrepresentable by typing the field; keep it that
// way. A bare string assignment must not compile.
func TestTokenFieldIsATokenStateNotAString(t *testing.T) {
	var a status.Account
	if reflect.TypeOf(a.Token) != reflect.TypeOf(creds.TokenState("")) {
		t.Fatalf("Account.Token is %T, want creds.TokenState", a.Token)
	}
}

// EnsureRoster reports a change only when a row was seeded, pruned,
// respelled or re-dirred: the daemon writes the file on it.
func TestEnsureRosterReportsWhetherAnythingChanged(t *testing.T) {
	var f status.File
	roster := []status.Member{{Name: "A", Dir: "/s/a"}, {Name: "B", Dir: "/s/b"}}
	if !f.EnsureRoster(roster) {
		t.Fatal("seeding two rows reported no change")
	}
	if f.EnsureRoster(roster) {
		t.Fatal("the same roster again reported a change")
	}
	for _, c := range []struct {
		name   string
		roster []status.Member
	}{
		{"rename", []status.Member{{Name: "work", Dir: "/s/a"}, {Name: "B", Dir: "/s/b"}}},
		{"respell", []status.Member{{Name: "WORK", Dir: "/s/a"}, {Name: "B", Dir: "/s/b"}}},
		{"prune", []status.Member{{Name: "WORK", Dir: "/s/a"}}},
		{"seed", []status.Member{{Name: "WORK", Dir: "/s/a"}, {Name: "C", Dir: "/s/c"}}},
	} {
		if !f.EnsureRoster(c.roster) {
			t.Fatalf("%s reported no change", c.name)
		}
	}
}
