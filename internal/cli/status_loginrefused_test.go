package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// R201: `status` shows an account whose login the daemon recorded refused, and
// a login newer than the mark hides it in the text and in --json.
func TestStatusShowsLoginRefusedUntilALaterLogin(t *testing.T) {
	home := t.TempDir()
	s := seedState(t, home, "D", "A")
	since := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	var f status.File
	f.EnsureAccounts([]string{"A", "D"})
	f.SetLoginRefused("A", 403, since, time.Time{})
	saveStatus(t, home, f)

	_, out, _ := runHome(t, home, "status")
	want := "login refused (403) since " + since.Local().Format("15:04")
	if !strings.Contains(out, want) {
		t.Fatalf("output lacks %q:\n%s", want, out)
	}
	_, jsonOut, _ := runHome(t, home, "status", "--json")
	var doc struct {
		Accounts []struct {
			Name               string `json:"name"`
			LoginRefused       string `json:"loginRefused"`
			LoginRefusedStatus int    `json:"loginRefusedStatus"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range doc.Accounts {
		if a.Name == "A" {
			found = true
			if got, err := time.Parse(time.RFC3339, a.LoginRefused); err != nil || !got.Equal(since) || a.LoginRefusedStatus != 403 {
				t.Fatalf("A = %+v, want the mark", a)
			}
		}
	}
	if !found {
		t.Fatalf("no account A in %s", jsonOut)
	}

	if _, err := s.Update(func(st *store.State) error {
		for i := range st.Accounts {
			if st.Accounts[i].Name == "A" {
				st.Accounts[i].LoggedInAt = since.Add(time.Minute)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, out, _ = runHome(t, home, "status")
	if strings.Contains(out, "login refused") {
		t.Fatalf("a later login must hide the mark:\n%s", out)
	}
	_, jsonOut, _ = runHome(t, home, "status", "--json")
	if strings.Contains(jsonOut, "loginRefused") {
		t.Fatalf("--json still carries the superseded mark:\n%s", jsonOut)
	}
}
