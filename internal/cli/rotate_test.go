package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func seedTwoAccounts(t *testing.T) (home string, s store.Store) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s = store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")})
	}); err != nil {
		t.Fatal(err)
	}
	return home, s
}

func TestRotateOffExcludesTheAccountFromRotation(t *testing.T) {
	_, s := seedTwoAccounts(t)
	var out, errBuf bytes.Buffer
	if got := runRotate([]string{"B", "off"}, newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runRotate = %d, stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Find("B")
	if err != nil {
		t.Fatal(err)
	}
	if a.Rotates() {
		t.Error("B still rotates after `rotate B off`")
	}
}

func TestRotateOnPutsTheAccountBack(t *testing.T) {
	_, s := seedTwoAccounts(t)
	var out, errBuf bytes.Buffer
	runRotate([]string{"B", "off"}, newReporter(false, &out, &errBuf))
	if got := runRotate([]string{"B", "on"}, newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runRotate = %d, stderr=%q", got, errBuf.String())
	}
	st, _ := s.Load()
	a, _ := st.Find("B")
	if !a.Rotates() {
		t.Error("B does not rotate after `rotate B on`")
	}
}

// Symmetric with `remote [<name>]`: the no-verb form reports, it does not
// change anything.
func TestRotateWithNoVerbPrintsTheCurrentSettingAndChangesNothing(t *testing.T) {
	_, s := seedTwoAccounts(t)
	var out, errBuf bytes.Buffer
	runRotate([]string{"B", "off"}, newReporter(false, &out, &errBuf))
	out.Reset()
	if got := runRotate([]string{"B"}, newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runRotate = %d, stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(out.String(), "off") {
		t.Errorf("stdout = %q, want it to report that B is off", out.String())
	}
	st, _ := s.Load()
	a, _ := st.Find("B")
	if a.Rotates() {
		t.Error("the no-verb form changed the setting")
	}
}

// The seeded default (both accounts rotating) is the mirror image of
// TestRotateWithNoVerbPrintsTheCurrentSettingAndChangesNothing: that test
// seeds B to off, so a bug that makes the no-verb form fall through to the
// update path (using the zero-value `want == false`) reasserts the same
// `off` state and goes unnoticed. Querying an account that is still ON
// catches it: a fallthrough would force NoRotate=true and silently turn a
// read-only query into a rotation-disabling write.
func TestRotateWithNoVerbDoesNotTurnAnOnAccountOff(t *testing.T) {
	_, s := seedTwoAccounts(t)
	var out, errBuf bytes.Buffer
	if got := runRotate([]string{"B"}, newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runRotate = %d, stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(out.String(), "on") {
		t.Errorf("stdout = %q, want it to report that B is on", out.String())
	}
	st, _ := s.Load()
	a, _ := st.Find("B")
	if !a.Rotates() {
		t.Error("the no-verb form turned B off")
	}
}

// Warn, do not refuse: `tag` warns rather than refusing too, and refusing
// here would make the flag depend on the order the user sets it in.
func TestRotateOffOnTheLastRotatingAccountWarnsButSucceeds(t *testing.T) {
	_, s := seedTwoAccounts(t)
	var out, errBuf bytes.Buffer
	runRotate([]string{"A", "off"}, newReporter(false, &out, &errBuf))
	errBuf.Reset()
	if got := runRotate([]string{"B", "off"}, newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runRotate = %d, want OK; stderr=%q", got, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no account left in rotation") {
		t.Errorf("stderr = %q, want a warning that nothing is left in rotation", errBuf.String())
	}
	st, _ := s.Load()
	a, _ := st.Find("B")
	if a.Rotates() {
		t.Error("B still rotates: the warning must not have cancelled the change")
	}
}

func TestRotateRejectsAnUnknownVerb(t *testing.T) {
	seedTwoAccounts(t)
	var out, errBuf bytes.Buffer
	if got := runRotate([]string{"B", "maybe"}, newReporter(false, &out, &errBuf)); got != exit.Usage {
		t.Errorf("runRotate = %d, want exit.Usage", got)
	}
}
