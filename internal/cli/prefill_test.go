package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// loginWithSubscription runs `chottag login NAME` against a fake claude
// whose `auth status --json` reports sub, and returns the stored account.
func loginWithSubscription(t *testing.T, home, name, sub string) store.Account {
	t.Helper()
	fake := writeFakeClaude(t, `{"loggedIn":true,"email":"`+strings.ToLower(name)+`@example.com","subscriptionType":"`+sub+`"}`)
	stubAuthExec(t, func(_, slotDir, _ string) error {
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	})
	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", fake, name}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	st, err := (store.Store{Dir: home}).Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Find(name)
	if err != nil {
		t.Fatal(err)
	}
	return *a
}

// TestLoginPrefillsThePlanFromSubscriptionType pins spec §2 and F200: pro
// and team are unambiguous; max is kept as "max" (size unknown); anything
// else leaves the plan unset.
func TestLoginPrefillsThePlanFromSubscriptionType(t *testing.T) {
	for sub, want := range map[string]string{"pro": "pro", "team": "team", "max": "max", "enterprise": ""} {
		t.Run(sub, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CHOTTAG_HOME", home)
			if a := loginWithSubscription(t, home, "A", sub); a.Plan != want {
				t.Fatalf("plan = %q, want %q", a.Plan, want)
			}
		})
	}
}

// A plan the user set with `chottag plan` survives a re-login.
func TestLoginKeepsAPlanTheUserSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	loginWithSubscription(t, home, "A", "max")
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error {
		st.Accounts[0].Plan = "max20x"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if a := loginWithSubscription(t, home, "A", "max"); a.Plan != "max20x" {
		t.Fatalf("plan = %q after a re-login, want the user's max20x", a.Plan)
	}
}

func TestAdoptPrefillsThePlan(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"B", "C"} {
		if err := os.MkdirAll(filepath.Join(dir, "accounts", n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(dir, "fake-claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '{\"loggedIn\":true,\"email\":\"x@example.com\",\"subscriptionType\":\"team\"}'\n"), 0o755)
	t.Setenv("CHOTTAG_HOME", dir)
	if code, _, errb := runChottag(t, "adopt", "--claude", fake); code != 0 {
		t.Fatalf("adopt = %d %q", code, errb)
	}
	st, err := (store.Store{Dir: dir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 2 || st.Accounts[0].Plan != "team" || st.Accounts[1].Plan != "team" {
		t.Fatalf("accounts = %+v, want both pre-filled team", st.Accounts)
	}
	// A second adopt of an already registered, unplanned account backfills.
	if _, err := (store.Store{Dir: dir}).Update(func(st *store.State) error {
		st.Accounts[0].Plan = ""
		st.Accounts[1].Plan = "pro"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runChottag(t, "adopt", "--claude", fake); code != 0 {
		t.Fatalf("second adopt = %d %q", code, errb)
	}
	st, _ = (store.Store{Dir: dir}).Load()
	if st.Accounts[0].Plan != "team" || st.Accounts[1].Plan != "pro" {
		t.Fatalf("accounts = %+v, want B backfilled and C's pro kept", st.Accounts)
	}
}
