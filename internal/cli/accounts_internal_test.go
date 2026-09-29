package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// TestNextReturnsUserActionBySentinelNotByMessageText lives in this
// white-box (package cli) file rather than accounts_test.go (package
// cli_test) because it calls runTag and ErrNoCandidate unqualified — both
// unexported.
//
// The exit code for "nothing to switch to" must not depend on the wording of
// an error message. M1b selected it with
// strings.Contains(err.Error(), "no account to switch to"), so rewording the
// sentence would have silently turned exit 3 into exit 1.
func TestNextReturnsUserActionBySentinelNotByMessageText(t *testing.T) {
	if !errors.Is(fmt.Errorf("%w (every other account is out of rotation)", ErrNoCandidate), ErrNoCandidate) {
		t.Fatal("wrapped ErrNoCandidate must satisfy errors.Is")
	}
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")}); err != nil {
			return err
		}
		st.Accounts[1].NoRotate = true
		st.Serving = "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Deliberately the LITERAL 3, not exit.UserAction: comparing the
	// function's output to the same constant the function returns is a
	// tautology that cannot fail (F106). Spec §5 fixes the VALUE, so the
	// value is what this pins.
	var out, errBuf bytes.Buffer
	if got := runTag(nil, newReporter(false, &out, &errBuf)); got != 3 {
		t.Errorf("runTag() = %d, want 3 (exit.UserAction); stderr=%q", got, errBuf.String())
	}
}

// writeStatusFile marshals f to status.Path(home), for tests that need
// cache/status.json to already hold observed limit state.
func writeStatusFile(t *testing.T, home string, f status.File) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(status.Path(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(status.Path(home), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// M4: the success-path skip line must match spec §5's comma-joined form
// ("skipped B (out of rotation), D (limited until 17:00)"), naming which
// remedy applies to which account and carrying the reset time for a
// limited skip, rather than one line per skip with no time.
func TestNextSuccessSkipLineMatchesSpecFormat(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B", "D", "E"} {
			if err := st.Add(store.Account{Name: n, Dir: filepath.Join(home, "accounts", n)}); err != nil {
				return err
			}
		}
		st.Accounts[1].NoRotate = true // B
		st.Serving = "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	until := now.Add(90 * time.Minute)
	writeStatusFile(t, home, status.File{Accounts: []status.Account{
		{Name: "D", Limited: true, LimitedUntil: until, Usage: &status.Usage{UpdatedAt: now}},
	}})

	var out, errBuf bytes.Buffer
	if got := runTag(nil, newReporter(false, &out, &errBuf)); got != 0 {
		t.Fatalf("runTag = %d, want 0; stderr=%q", got, errBuf.String())
	}
	want := fmt.Sprintf("skipped B (out of rotation), D (limited until %s)\nserving: E\n", untilText(until, now))
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

func TestNextExitsUserActionAndPrintsResetTimesWhenEverythingIsLimited(t *testing.T) {
	home, s := seedTwoAccounts(t)
	now := time.Now()
	f := status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, LimitedUntil: now.Add(90 * time.Minute), Usage: &status.Usage{UpdatedAt: now}},
	}}
	writeStatusFile(t, home, f) // helper: marshals f to status.Path(home)
	_ = s

	var out, errBuf bytes.Buffer
	if got := runTag(nil, newReporter(false, &out, &errBuf)); got != 3 {
		t.Fatalf("runTag = %d, want 3; stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), untilText(now.Add(90*time.Minute), now)) {
		t.Errorf("stderr = %q, want it to name B's reset time", errBuf.String())
	}
}

func TestNextForceSwitchesToALimitedAccount(t *testing.T) {
	home, s := seedTwoAccounts(t)
	now := time.Now()
	writeStatusFile(t, home, status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: now}},
	}})
	var out, errBuf bytes.Buffer
	if got := runTag([]string{"--force"}, newReporter(false, &out, &errBuf)); got != 0 {
		t.Fatalf("runTag --force = %d; stderr=%q", got, errBuf.String())
	}
	st, _ := s.Load()
	if st.Serving != "B" {
		t.Errorf("serving = %q, want B", st.Serving)
	}
}

func TestTagWarnsWhenTheTargetIsLimitedButStillSwitches(t *testing.T) {
	home, s := seedTwoAccounts(t)
	now := time.Now()
	writeStatusFile(t, home, status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, LimitedUntil: now.Add(time.Hour), Usage: &status.Usage{UpdatedAt: now}},
	}})
	var out, errBuf bytes.Buffer
	if got := runTag([]string{"B"}, newReporter(false, &out, &errBuf)); got != 0 {
		t.Fatalf("runTag B = %d; stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "limited") {
		t.Errorf("stderr = %q, want a limit warning", errBuf.String())
	}
	st, _ := s.Load()
	if st.Serving != "B" {
		t.Errorf("serving = %q, want B: tag warns, it does not refuse", st.Serving)
	}
}

// Sibling of TestTagWarnsWhenTheTargetIsLimitedButStillSwitches: a limited
// account with an UNKNOWN reset time (zero LimitedUntil) takes the
// until.IsZero() branch of the warning (accounts.go), printing "is limited"
// with no "until" clause — that branch was otherwise unexercised.
func TestTagWarnsWhenTheTargetIsLimitedWithNoKnownResetTime(t *testing.T) {
	home, s := seedTwoAccounts(t)
	now := time.Now()
	writeStatusFile(t, home, status.File{Accounts: []status.Account{
		{Name: "B", Limited: true, Usage: &status.Usage{UpdatedAt: now}}, // LimitedUntil zero: unknown
	}})
	var out, errBuf bytes.Buffer
	if got := runTag([]string{"B"}, newReporter(false, &out, &errBuf)); got != 0 {
		t.Fatalf("runTag B = %d; stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "B is limited") {
		t.Errorf("stderr = %q, want a limit warning naming B", errBuf.String())
	}
	if strings.Contains(errBuf.String(), "until") {
		t.Errorf("stderr = %q, want no \"until\" clause: the reset time is unknown", errBuf.String())
	}
	st, _ := s.Load()
	if st.Serving != "B" {
		t.Errorf("serving = %q, want B: tag warns, it does not refuse", st.Serving)
	}
}
