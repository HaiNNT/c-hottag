package status_test

import (
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

var lrT0 = time.Date(2026, 11, 21, 15, 4, 5, 0, time.UTC)

func lrRow(t *testing.T, f status.File, name string) status.Account {
	t.Helper()
	for _, a := range f.Accounts {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no row for %s", name)
	return status.Account{}
}

func TestSetLoginRefusedStampsAndKeepsFirst(t *testing.T) {
	var f status.File
	if !f.SetLoginRefused("alice", 403, lrT0, time.Time{}) {
		t.Fatal("first stamp reported no change")
	}
	if f.SetLoginRefused("alice", 401, lrT0.Add(time.Minute), time.Time{}) {
		t.Fatal("second refusal in the same episode reported a change")
	}
	a := lrRow(t, f, "alice")
	if !a.LoginRefused.Equal(lrT0) || a.LoginRefusedStatus != 403 {
		t.Fatalf("row = %+v, want first episode kept", a)
	}
}

func TestSetLoginRefusedRestampsAfterLogin(t *testing.T) {
	var f status.File
	f.SetLoginRefused("alice", 403, lrT0, time.Time{})
	login := lrT0.Add(time.Hour)
	later := login.Add(time.Minute)
	if !f.SetLoginRefused("alice", 401, later, login) {
		t.Fatal("refusal after a newer login did not re-stamp")
	}
	a := lrRow(t, f, "alice")
	if !a.LoginRefused.Equal(later) || a.LoginRefusedStatus != 401 {
		t.Fatalf("row = %+v", a)
	}
	// A login older than the mark does not supersede it.
	if f.SetLoginRefused("alice", 403, later.Add(time.Minute), login) {
		t.Fatal("login older than the mark re-stamped")
	}
}

func TestSetLoginRefusedEmptyNameMakesNoRow(t *testing.T) {
	var f status.File
	if f.SetLoginRefused("", 403, lrT0, time.Time{}) || len(f.Accounts) != 0 {
		t.Fatalf("empty name changed the file: %+v", f.Accounts)
	}
}

func TestSetLoginRefusedIsCaseInsensitive(t *testing.T) {
	var f status.File
	f.EnsureAccounts([]string{"Alice"})
	f.SetLoginRefused("alice", 403, lrT0, time.Time{})
	if len(f.Accounts) != 1 || f.Accounts[0].LoginRefused.IsZero() {
		t.Fatalf("accounts = %+v", f.Accounts)
	}
}

func TestClearLoginRefused(t *testing.T) {
	var f status.File
	if _, changed := f.ClearLoginRefused("alice"); changed {
		t.Fatal("clearing an account with no row changed something")
	}
	if len(f.Accounts) != 0 {
		t.Fatal("clear created a row")
	}
	f.SetLoginRefused("alice", 403, lrT0, time.Time{})
	since, changed := f.ClearLoginRefused("alice")
	if !changed || !since.Equal(lrT0) {
		t.Fatalf("since=%v changed=%v", since, changed)
	}
	a := lrRow(t, f, "alice")
	if !a.LoginRefused.IsZero() || a.LoginRefusedStatus != 0 {
		t.Fatalf("row = %+v", a)
	}
	if _, changed := f.ClearLoginRefused("alice"); changed {
		t.Fatal("second clear reported a change")
	}
}

func TestLoginRefusedNow(t *testing.T) {
	a := status.Account{LoginRefused: lrT0}
	for _, c := range []struct {
		loggedIn time.Time
		want     bool
	}{
		{time.Time{}, true},
		{lrT0.Add(-time.Hour), true},
		{lrT0, true},
		{lrT0.Add(time.Second), false},
	} {
		if got := a.LoginRefusedNow(c.loggedIn); got != c.want {
			t.Errorf("loggedIn %v: got %v want %v", c.loggedIn, got, c.want)
		}
	}
	if (status.Account{}).LoginRefusedNow(time.Time{}) {
		t.Error("an unmarked account reads refused")
	}
}

func TestLoginRefusedJSONOmittedWhenZeroAndRoundTrips(t *testing.T) {
	var f status.File
	f.EnsureAccounts([]string{"alice"})
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "loginRefused") {
		t.Fatalf("zero value serialized: %s", b)
	}
	f.SetLoginRefused("alice", 403, lrT0, time.Time{})
	b, err = status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"loginRefused": "2026-11-21T15:04:05Z"`, `"loginRefusedStatus": 403`} {
		if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), strings.Join(strings.Fields(want), " ")) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
	path := status.Path(t.TempDir())
	saveFile(t, path, f)
	got, err := status.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := lrRow(t, got, "alice")
	if !a.LoginRefused.Equal(lrT0) || a.LoginRefusedStatus != 403 {
		t.Fatalf("round trip lost it: %+v", a)
	}
}

func TestClearDaemonKeepsLoginRefused(t *testing.T) {
	var f status.File
	f.SetLoginRefused("alice", 403, lrT0, time.Time{})
	f.SetDaemon(47850, 0, 0, 0, lrT0)
	f.ClearDaemon()
	if lrRow(t, f, "alice").LoginRefused.IsZero() {
		t.Fatal("ClearDaemon dropped the mark")
	}
}
