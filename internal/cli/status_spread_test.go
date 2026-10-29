package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// spreadSessions seeds one live registry entry and cache row per placement
// ("A" for a session placed on A); the row's last-used account is A throughout.
func spreadSessions(t *testing.T, home string, pool string, base int, placed ...string) []sessions.Activity {
	t.Helper()
	var acts []sessions.Activity
	for i, p := range placed {
		sid := fmt.Sprintf("%02x", base+i)
		sid = strings.Repeat(sid, 16)
		moveEntry(t, home, fmt.Sprint(base+i), session.Session{Port: movePort, SID: sid, Pool: pool, Started: time.Now().Add(time.Duration(i) * time.Second)})
		acts = append(acts, sessions.Activity{SID: sid, Pool: pool, Account: "A", Placed: p, LastSeen: time.Now()})
	}
	return acts
}

func TestStatusSpreadHeaderListsServing(t *testing.T) {
	home, s := moveHome(t)
	if _, err := s.Update(func(st *store.State) error {
		st.Serving, st.Remote = "B", "C"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	acts := spreadSessions(t, home, "default", 1, "B", "A", "B", "B")
	moveCache(t, home, status.File{Sessions: acts})
	_, out, _ := runChottag(t, "status")
	first, _, _ := strings.Cut(out, "\n")
	if want := "serving: A 1 · B 3   fallback: B   remote: C"; first != want {
		t.Fatalf("first line %q, want %q\n%s", first, want, out)
	}
	if strings.Contains(out, "live sessions:") {
		t.Fatalf("spread status keeps the live sessions line:\n%s", out)
	}
}

func TestStatusSerialHeaderUnchanged(t *testing.T) {
	home, s := moveHome(t)
	if _, err := s.Update(func(st *store.State) error {
		st.SetPolicy(store.PolicySerial)
		st.Serving, st.Remote = "B", "C"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	acts := spreadSessions(t, home, "default", 1, "B", "A")
	moveCache(t, home, status.File{Sessions: acts})
	_, out, _ := runChottag(t, "status")
	first, _, _ := strings.Cut(out, "\n")
	if want := "serving: B   remote: C"; first != want {
		t.Fatalf("first line %q, want %q", first, want)
	}
	if !strings.Contains(out, "live sessions: 2 (A 1, B 1)\n") {
		t.Fatalf("serial status lost the live sessions line:\n%s", out)
	}
	_, js, _ := runChottag(t, "--json", "status")
	if strings.Contains(js, "servingAccounts") {
		t.Fatalf("serial document has servingAccounts:\n%s", js)
	}
}

func TestStatusServingAccountsJSON(t *testing.T) {
	home, s := moveHome(t)
	if _, err := s.Update(func(st *store.State) error {
		if err := st.AddPool("work"); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "D", Dir: filepath.Join(home, "accounts", "D"), PoolList: []string{"work"}}); err != nil {
			return err
		}
		if err := st.Add(store.Account{Name: "E", Dir: filepath.Join(home, "accounts", "E"), PoolList: []string{"work"}}); err != nil {
			return err
		}
		return st.SetPoolPolicy("work", store.PolicySpread)
	}); err != nil {
		t.Fatal(err)
	}
	acts := spreadSessions(t, home, "default", 1, "B", "B", "A")
	acts = append(acts, spreadSessions(t, home, "work", 10, "E")...)
	moveCache(t, home, status.File{Sessions: acts})
	_, out, _ := runChottag(t, "--json", "status")
	doc := decodeOneDocument(t, out)
	if got := fmt.Sprint(doc["servingAccounts"]); got != "[map[name:A sessions:1] map[name:B sessions:2]]" {
		t.Fatalf("top level servingAccounts = %s", got)
	}
	var work map[string]any
	for _, p := range doc["pools"].([]any) {
		if m := p.(map[string]any); m["name"] == "work" {
			work = m
		}
	}
	if got := fmt.Sprint(work["servingAccounts"]); got != "[map[name:E sessions:1]]" {
		t.Fatalf("work servingAccounts = %s\n%s", got, out)
	}
}

// Review focus 5: default spreads, work stays serial.
func TestStatusMixedPools(t *testing.T) {
	home, s := moveHome(t)
	if _, err := s.Update(func(st *store.State) error {
		st.Serving = "B"
		if err := st.AddPool("work"); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "D", Dir: filepath.Join(home, "accounts", "D"), PoolList: []string{"work"}})
	}); err != nil {
		t.Fatal(err)
	}
	acts := spreadSessions(t, home, "default", 1, "B", "B")
	acts = append(acts, sessions.Activity{SID: strings.Repeat("dd", 16), Pool: "work", Account: "D", LastSeen: time.Now()})
	moveEntry(t, home, "10", session.Session{Port: movePort, SID: acts[len(acts)-1].SID, Pool: "work", Started: time.Now()})
	moveCache(t, home, status.File{Sessions: acts})
	_, out, _ := runChottag(t, "status")
	if first, _, _ := strings.Cut(out, "\n"); !strings.HasPrefix(first, "serving: B 2   fallback: B   remote:") {
		t.Fatalf("default's header is not spread-shaped: %q", first)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "pool work") && strings.Contains(line, "fallback") {
			t.Fatalf("serial pool line changed: %q", line)
		}
	}
	_, js, _ := runChottag(t, "--json", "status")
	doc := decodeOneDocument(t, js)
	if _, ok := doc["servingAccounts"]; !ok {
		t.Fatal("default lacks servingAccounts")
	}
	for _, p := range doc["pools"].([]any) {
		if m := p.(map[string]any); m["name"] == "work" {
			if _, ok := m["servingAccounts"]; ok {
				t.Fatalf("serial pool has servingAccounts: %v", m)
			}
		}
	}
}
