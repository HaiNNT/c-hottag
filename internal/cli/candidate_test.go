package cli

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func threeAccounts(t *testing.T) *store.State {
	t.Helper()
	home := t.TempDir()
	st := store.Default()
	for _, n := range []string{"A", "B", "C"} {
		if err := st.Add(store.Account{Name: n, Dir: filepath.Join(home, "accounts", n)}); err != nil {
			t.Fatal(err)
		}
	}
	st.Serving = "A"
	return &st
}

func limitedFile(account string, until time.Time, updated time.Time) *status.File {
	f := &status.File{Accounts: []status.Account{{
		Name:         account,
		Limited:      true,
		LimitedUntil: until,
		Usage:        &status.Usage{UpdatedAt: updated},
	}}}
	return f
}

// A fresh, known limit skips the account.
func TestNextCandidateSkipsAKnownLimitedAccount(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := limitedFile("B", now.Add(2*time.Hour), now.Add(-time.Minute))
	got, skips, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "C" {
		t.Errorf("candidate = %q, want C: B is limited and must be skipped", got.Name)
	}
	if len(skips) != 1 || skips[0].Name != "B" || skips[0].Reason != "limited" {
		t.Errorf("skips = %+v, want one limited skip for B", skips)
	}
}

// R54/F161: staleness only matters when the reset time is UNKNOWN (zero
// LimitedUntil). With no known reset, stale usage cannot say the limit still
// holds, so it is treated as not limited — the expensive-to-be-wrong-in
// direction is a false "limited" silently moving the user off a healthy
// account.
func TestNextCandidateDoesNotSkipOnStaleLimitDataWithAnUnknownReset(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := &status.File{Accounts: []status.Account{{
		Name:    "B",
		Limited: true,
		// LimitedUntil zero: unknown reset.
		Usage: &status.Usage{UpdatedAt: now.Add(-status.StaleAfter - time.Minute)},
	}}}
	got, skips, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "B" {
		t.Errorf("candidate = %q, want B: a stale limit with an unknown reset is unknown, not limited (R54)", got.Name)
	}
	if len(skips) != 0 {
		t.Errorf("skips = %+v, want none", skips)
	}
}

// R54/F161: a limit whose reset is KNOWN and still in the future is honoured
// however old the usage is — status showed "limited until HH:MM" and `next`
// must agree, not silently route back onto the account that refused.
func TestNextCandidateSkipsAStaleLimitWithAKnownFutureReset(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := limitedFile("B", now.Add(2*time.Hour), now.Add(-status.StaleAfter-time.Hour))
	got, skips, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "C" {
		t.Errorf("candidate = %q, want C: B's reset is known and still future, so stale usage does not excuse it (R54)", got.Name)
	}
	if len(skips) != 1 || skips[0].Name != "B" || skips[0].Reason != "limited" || skips[0].Until != f.Accounts[0].LimitedUntil {
		t.Errorf("skips = %+v, want one limited skip for B with its known until", skips)
	}
}

// R54/F161: if EVERY other account is limited with a known future reset,
// however stale the usage, `next` has nothing to offer: exit 3
// (ErrNoCandidate), same as with fresh data.
func TestNextCandidateReturnsErrNoCandidateWhenEveryAccountHasAStaleButKnownFutureLimit(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	stale := now.Add(-status.StaleAfter - time.Hour)
	f := &status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, LimitedUntil: now.Add(2 * time.Hour), Usage: &status.Usage{UpdatedAt: stale}},
		{Name: "C", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: stale}},
	}}
	_, skips, _, err := nextCandidate(st, f, now, false)
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("err = %v, want ErrNoCandidate", err)
	}
	if len(skips) != 2 {
		t.Fatalf("skips = %+v, want two", skips)
	}
}

// A missing cache is the same case as stale: unknown, so nothing is skipped.
func TestNextCandidateDoesNotSkipWhenThereIsNoCache(t *testing.T) {
	st := threeAccounts(t)
	got, skips, _, err := nextCandidate(st, &status.File{}, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "B" || len(skips) != 0 {
		t.Errorf("candidate = %q skips = %+v, want B with no skips", got.Name, skips)
	}
}

// An expired limit is not a limit.
func TestNextCandidateIgnoresALimitThatHasAlreadyReset(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := limitedFile("B", now.Add(-time.Minute), now.Add(-time.Minute))
	got, _, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "B" {
		t.Errorf("candidate = %q, want B: LimitedUntil is in the past", got.Name)
	}
}

// R54/F161: an already-reset LimitedUntil is not a limit regardless of how
// stale the usage is — a known past reset is not "still limited but stale",
// it is over.
func TestNextCandidateIgnoresAStaleLimitThatHasAlreadyReset(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := limitedFile("B", now.Add(-time.Minute), now.Add(-status.StaleAfter-time.Hour))
	got, _, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "B" {
		t.Errorf("candidate = %q, want B: LimitedUntil is in the past, whatever the usage's age", got.Name)
	}
}

// Boundary: LimitedUntil exactly equal to now is "the window has reset", not
// "one instant still limited" — knownLimit uses !After(now), not Before(now).
// A mutant that swapped in Before(now) would read this instant as still
// limited and skip B; only an exact-equality fixture catches that (a
// two-value comparison at any other point cannot distinguish the two forms).
func TestNextCandidateTreatsAResetExactlyAtNowAsAlreadyReset(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := limitedFile("B", now, now)
	got, _, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "B" {
		t.Errorf("candidate = %q, want B: LimitedUntil == now is already reset, not still limited", got.Name)
	}
}

func TestNextCandidateReturnsErrNoCandidateWhenEveryOtherAccountIsLimited(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := &status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, LimitedUntil: now.Add(2 * time.Hour), Usage: &status.Usage{UpdatedAt: now}},
		{Name: "C", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: now}},
	}}
	_, skips, _, err := nextCandidate(st, f, now, false)
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("err = %v, want ErrNoCandidate", err)
	}
	if len(skips) != 2 {
		t.Fatalf("skips = %+v, want two", skips)
	}
	// Earliest reset first, so the printed advice leads with the soonest.
	if !skips[0].Until.Before(skips[1].Until) {
		t.Errorf("skips = %+v, want them ordered earliest reset first", skips)
	}
}

// A limited account with an UNKNOWN reset time (LimitedUntil zero) must
// still be skipped: status.Account.LimitedUntil's doc comment and
// knownLimit's own comment both say zero means unknown, not expired.
// Dropping the `!a.LimitedUntil.IsZero() &&` guard in knownLimit makes a
// fresh, limited, unknown-reset account read as "already reset" — routing
// straight back onto the account that just refused — and only the
// `.After(now)` half of that guard was previously exercised
// (TestNextCandidateIgnoresALimitThatHasAlreadyReset uses a non-zero,
// past LimitedUntil).
func TestNextCandidateSkipsALimitWithAnUnknownResetTime(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := &status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, Usage: &status.Usage{UpdatedAt: now}}, // LimitedUntil zero: unknown
	}}
	got, skips, _, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "C" {
		t.Errorf("candidate = %q, want C: B is limited (unknown reset time) and must be skipped", got.Name)
	}
	if len(skips) != 1 || skips[0].Name != "B" || skips[0].Reason != "limited" || !skips[0].Until.IsZero() {
		t.Errorf("skips = %+v, want one limited skip for B with an unknown (zero) reset time", skips)
	}
}

// M3: the sort's zero/non-zero tie-break must put a known reset time ahead
// of an unknown one, so the printed advice leads with an account the user
// can act on ("wait until HH:MM") rather than one it can only say "limited,
// no known reset" about.
func TestNextCandidateSortsAKnownResetBeforeAnUnknownOne(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	f := &status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, Usage: &status.Usage{UpdatedAt: now}}, // unknown reset time
		{Name: "C", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: now}},
	}}
	_, skips, _, err := nextCandidate(st, f, now, false)
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("err = %v, want ErrNoCandidate", err)
	}
	if len(skips) != 2 {
		t.Fatalf("skips = %+v, want two", skips)
	}
	if skips[0].Name != "C" || skips[0].Until.IsZero() {
		t.Errorf("skips[0] = %+v, want C's known reset time first", skips[0])
	}
	if skips[1].Name != "B" || !skips[1].Until.IsZero() {
		t.Errorf("skips[1] = %+v, want B's unknown reset time last", skips[1])
	}
}

// F7: the wraparound guard compares a.Name against st.Serving with
// strings.EqualFold, not ==. With a single account "A" and st.Serving spelled
// "a" (a case mismatch that can arise since Add and Find both treat names
// case-insensitively, e.g. runTag persisting a caller's own spelling), an
// exact-match comparison never recognises the loop has walked back to the
// already-serving account and would wrongly offer it up as its own
// replacement.
func TestNextCandidateEqualFoldGuardsAgainstAServingCaseMismatch(t *testing.T) {
	home := t.TempDir()
	st := store.Default()
	if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
		t.Fatal(err)
	}
	st.Serving = "a"
	_, _, _, err := nextCandidate(&st, &status.File{}, time.Now(), false)
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("err = %v, want ErrNoCandidate: A is the only account and is already serving (case-insensitively), so `next` must not return A itself", err)
	}
}

// TestNextCandidateFallsBackWhenNoAccountIsBelowItsSwitchPoint is item 2
// (review round 2): a manual `next` is a hard request to move, so it gets
// the planner's LIMIT-trigger fallback too. B is above its own switch
// point (95%, max5x's point is 93) but still has capacity (< 100%); C is
// out of rotation, so B is the only fallback candidate.
func TestNextCandidateFallsBackWhenNoAccountIsBelowItsSwitchPoint(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	pct := 95.0
	f := &status.File{Accounts: []status.Account{
		{Name: "B", Usage: &status.Usage{UpdatedAt: now, FiveHourPct: &pct, FiveHourResetsAt: now.Add(4 * time.Hour)}},
	}}
	st.Accounts[2].NoRotate = true // C: out of rotation
	got, skips, fellBack, err := nextCandidate(st, f, now, false)
	if err != nil {
		t.Fatalf("nextCandidate = %v, want B via the fallback", err)
	}
	if got.Name != "B" {
		t.Fatalf("candidate = %q, want B", got.Name)
	}
	if !fellBack {
		t.Fatal("fellBack = false, want true: B was picked via the fallback")
	}
	// B is the chosen fallback, not a skip (item 2, review round 3): only
	// C's unrelated out-of-rotation skip should remain.
	if len(skips) != 1 || skips[0].Name != "C" {
		t.Fatalf("skips = %+v, want just C (out of rotation)", skips)
	}
}

// TestNextCandidateFallbackNeverPicksAFullAccount is item 2: the fallback
// still excludes an account at its wall, even though it ignores switch
// points — an account genuinely at 100% can never actually serve.
func TestNextCandidateFallbackNeverPicksAFullAccount(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	pctC := 100.0
	f := &status.File{Accounts: []status.Account{
		{Name: "C", Usage: &status.Usage{UpdatedAt: now, FiveHourPct: &pctC, FiveHourResetsAt: now.Add(4 * time.Hour)}},
	}}
	st.Accounts[1].NoRotate = true // B: out of rotation
	_, skips, _, err := nextCandidate(st, f, now, false)
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("err = %v, want ErrNoCandidate: B is out of rotation and C is at its wall", err)
	}
	if len(skips) != 2 {
		t.Fatalf("skips = %+v, want two", skips)
	}
}

// --force ignores limits but still respects rotation: an account the user
// deliberately excluded is not a fallback.
func TestNextCandidateForceIgnoresLimitsButNotRotation(t *testing.T) {
	st := threeAccounts(t)
	now := time.Now()
	st.Accounts[1].NoRotate = true // B
	f := limitedFile("C", now.Add(time.Hour), now)
	got, _, _, err := nextCandidate(st, f, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "C" {
		t.Errorf("candidate = %q, want C: --force overrides the limit, not the rotation exclusion", got.Name)
	}
}
