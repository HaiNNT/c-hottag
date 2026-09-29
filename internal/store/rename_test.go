package store_test

import (
	"errors"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func renameFixture(t *testing.T) store.State {
	t.Helper()
	st := store.Default()
	for _, a := range []store.Account{acct("A", "a@example.com"), acct("B", "b@example.com")} {
		if err := st.Add(a); err != nil {
			t.Fatal(err)
		}
	}
	st.Serving, st.Remote = "B", "B"
	return st
}

func TestRenameMovesEveryRoleAndKeepsTheSlot(t *testing.T) {
	st := renameFixture(t)
	from, roles, err := st.Rename("b", "Bee")
	if err != nil {
		t.Fatal(err)
	}
	if from != "B" || len(roles) != 2 || roles[0] != "serving" || roles[1] != "remote" {
		t.Fatalf("from = %q, roles = %v; want B, [serving remote]", from, roles)
	}
	b := st.Accounts[1]
	if b.Name != "Bee" || b.Dir != "/slots/B" || b.Email != "b@example.com" {
		t.Fatalf("account = %+v, want the name changed and the slot and email kept", b)
	}
	if st.Serving != "Bee" || st.Remote != "Bee" {
		t.Fatalf("roles = %q/%q, want Bee/Bee", st.Serving, st.Remote)
	}
}

func TestRenameReturnsNoRoleForAnAccountWithoutOne(t *testing.T) {
	st := renameFixture(t)
	_, roles, err := st.Rename("A", "Ay")
	if err != nil || roles == nil || len(roles) != 0 {
		t.Fatalf("roles = %#v, err = %v; want an empty, non-nil slice", roles, err)
	}
}

func TestRenameRefusesAClashCaseInsensitively(t *testing.T) {
	st := renameFixture(t)
	if _, _, err := st.Rename("B", "a"); !errors.Is(err, store.ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if st.Accounts[0].Name != "A" || st.Accounts[1].Name != "B" || st.Serving != "B" {
		t.Fatalf("a refused rename changed the state: %+v", st)
	}
}

func TestRenameAllowsACaseOnlyChange(t *testing.T) {
	st := renameFixture(t)
	from, _, err := st.Rename("B", "b")
	if err != nil || from != "B" || st.Accounts[1].Name != "b" || st.Serving != "b" {
		t.Fatalf("from = %q, err = %v, state = %+v; want B renamed to b", from, err, st)
	}
}

func TestRenameValidatesTheNewName(t *testing.T) {
	st := renameFixture(t)
	for _, bad := range []string{"", "bad/name", ".dot", "-dash", "has space"} {
		if _, _, err := st.Rename("B", bad); err == nil {
			t.Errorf("Rename to %q succeeded", bad)
		}
	}
	if st.Accounts[1].Name != "B" {
		t.Fatalf("state changed: %+v", st)
	}
}

func TestRenameNeverResolvesAPrefixOrAnEmail(t *testing.T) {
	st := renameFixture(t)
	for _, q := range []string{"b@example.com", "Bx", ""} {
		if _, _, err := st.Rename(q, "Z"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Rename(%q) err = %v, want ErrNotFound", q, err)
		}
	}
	st2 := store.Default()
	if err := st2.Add(acct("Alpha", "")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st2.Rename("Al", "Z"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a prefix resolved: err = %v", err)
	}
}

func TestRenameAcceptsAnInvalidOldName(t *testing.T) {
	st := store.Default()
	st.Accounts = []store.Account{{Name: "old name", Dir: "/slots/x"}}
	st.Serving = "old name"
	if _, roles, err := st.Rename("old name", "fixed"); err != nil || st.Accounts[0].Name != "fixed" || len(roles) != 1 {
		t.Fatalf("err = %v, roles = %v, state = %+v", err, roles, st)
	}
}
