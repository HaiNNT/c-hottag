package cli_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

const poolSID = "0123456789abcdef0123456789abcdef"

// poolStatuslineEnv is statuslineEnv plus a pool work holding C and D
// (serving C); the default pool keeps "work" and "other".
func poolStatuslineEnv(t *testing.T) (home string, port int) {
	t.Helper()
	home, port = statuslineEnv(t, true, nil)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.AddPool("work"); err != nil {
			return err
		}
		for _, n := range []string{"C", "D"} {
			d, err := s.SlotDir(n)
			if err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Dir: d, PoolList: []string{"work"}}); err != nil {
				return err
			}
		}
		return st.SetPoolServing("work", "C")
	}); err != nil {
		t.Fatal(err)
	}
	return home, port
}

// A session in a non-default pool shows its pool after the account, serves
// from the pool's serving account, and counts the pool's own accounts (M8).
func TestStatuslineNamesANonDefaultPool(t *testing.T) {
	home, port := poolStatuslineEnv(t)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag.work.%s:x@127.0.0.1:%d", poolSID, port))
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» C [work] · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("got %d %q", code, out)
	}
	_, out, _ = runHome(t, home, "--json", "statusline")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["pool"] != "work" || doc["serving"] != "C" || doc["account"] != "C" {
		t.Fatalf("json = %v", doc)
	}
}

// A default-pool session names no pool, even when other pools exist.
func TestStatuslineDefaultPoolSessionNamesNoPool(t *testing.T) {
	home, port := poolStatuslineEnv(t)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag.default.%s:x@127.0.0.1:%d", poolSID, port))
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("got %d %q", code, out)
	}
	_, out, _ = runHome(t, home, "--json", "statusline")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["pool"]; ok {
		t.Fatalf("a default session carries a pool: %v", doc)
	}
}
