package cli_test

// Tests for how `status` and `statusline` show an available update (R124).

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// withCLIVersion sets the running version for the test.
func withCLIVersion(t *testing.T, v string) {
	t.Helper()
	orig := cli.Version
	cli.Version = v
	t.Cleanup(func() { cli.Version = orig })
}

func writeUpdateCache(t *testing.T, home string, u *status.Update, accts ...status.Account) {
	t.Helper()
	if err := os.MkdirAll(home+"/cache", 0o700); err != nil {
		t.Fatal(err)
	}
	saveStatus(t, home, status.File{Accounts: accts, Update: u})
}

func availableUpdate() *status.Update {
	return &status.Update{Latest: "0.6.0", CheckedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), Available: true}
}

func TestStatusShowsAnAvailableUpdate(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	home := t.TempDir()
	seedState(t, home, "D", "A")
	writeUpdateCache(t, home, availableUpdate())
	_, out, _ := runHome(t, home, "status")
	if want := "update: 0.6.0 available (run: chottag update)\n"; !strings.Contains(out, want) {
		t.Fatalf("status = %q, want it to contain %q", out, want)
	}
}

func TestStatusShowsNoUpdateLineWhenNoneIsAvailable(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	home := t.TempDir()
	seedState(t, home, "D", "A")
	u := availableUpdate()
	u.Available = false
	writeUpdateCache(t, home, u)
	if _, out, _ := runHome(t, home, "status"); strings.Contains(out, "update:") {
		t.Errorf("status = %q, want no update line", out)
	}
	// No cache at all.
	home2 := t.TempDir()
	seedState(t, home2, "D", "A")
	if _, out, _ := runHome(t, home2, "status"); strings.Contains(out, "update:") {
		t.Errorf("status = %q, want no update line", out)
	}
}

// A cache written by an older daemon still says available after the user has
// updated: the line must not claim an update the running binary already is.
func TestStatusHidesAnUpdateTheRunningVersionAlreadyHas(t *testing.T) {
	withCLIVersion(t, "0.6.0")
	home := t.TempDir()
	seedState(t, home, "D", "A")
	writeUpdateCache(t, home, availableUpdate())
	if _, out, _ := runHome(t, home, "status"); strings.Contains(out, "update:") {
		t.Errorf("status = %q, want no update line", out)
	}
}

func TestStatusJSONHasUpdateAndUpdates(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	home := t.TempDir()
	s := seedState(t, home, "D", "A")
	writeUpdateCache(t, home, availableUpdate())
	get := func() map[string]any {
		_, out, errb := runHome(t, home, "status", "--json")
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not JSON: %v\n%s\n%s", err, out, errb)
		}
		return got
	}
	got := get()
	up, _ := got["update"].(map[string]any)
	sw, _ := got["updates"].(map[string]any)
	if up["latest"] != "0.6.0" || up["available"] != true {
		t.Errorf("update = %v, want the cache", got["update"])
	}
	if sw["check"] != true || sw["auto"] != false {
		t.Errorf("updates = %v, want check true, auto false", got["updates"])
	}
	if _, err := s.Update(func(st *store.State) error { st.SetAutoUpdate(true); return nil }); err != nil {
		t.Fatal(err)
	}
	if sw, _ := get()["updates"].(map[string]any); sw["check"] != true || sw["auto"] != true {
		t.Errorf("updates = %v, want both true", sw)
	}
	if _, err := s.Update(func(st *store.State) error { st.SetUpdateCheck(false); return nil }); err != nil {
		t.Fatal(err)
	}
	if sw, _ := get()["updates"].(map[string]any); sw["check"] != false {
		t.Errorf("updates = %v, want check false", sw)
	}
}

func TestStatusJSONHasNoUpdateWithoutACache(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "D", "A")
	_, out, _ := runHome(t, home, "status", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["update"]; ok {
		t.Errorf("update present with no cache: %v", got["update"])
	}
	if _, ok := got["updates"]; !ok {
		t.Error("updates missing: the switches come from state.json")
	}
}

func TestStatuslineShowsAnAvailableUpdate(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	home, _ := v2Env(t)
	var f status.File
	f.Update = availableUpdate()
	f.Accounts = []status.Account{{Name: "work"}}
	saveStatus(t, home, f)
	_, out, _ := runHome(t, home, "statusline")
	if want := "c» work · 5h – · 7d – · 2/2 ok · ↑0.6.0\n"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	_, jout, _ := runHome(t, home, "statusline", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jout), &got); err != nil {
		t.Fatal(err)
	}
	if got["updateAvailable"] != "0.6.0" {
		t.Errorf("json = %s, want updateAvailable 0.6.0", jout)
	}
}

func TestStatuslineUpdateSuffixTakesNoColour(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	home, now := v2Env(t)
	var f status.File
	f.Update = availableUpdate()
	f.Accounts = []status.Account{{Name: "work", Limited: true, Usage: usageOf(now, 100, 20, now.Add(time.Hour), time.Time{})}}
	saveStatus(t, home, f)
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "")
	_, out, _ := runHome(t, home, "statusline")
	if !strings.HasSuffix(out, " · ↑0.6.0\n") {
		t.Fatalf("out = %q, want the plain update suffix last", out)
	}
}

func TestStatuslineOmitsTheUpdateWhenNoneIsAvailableOrOffOrDown(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	// Not available.
	home, _ := v2Env(t)
	u := availableUpdate()
	u.Available = false
	var f status.File
	f.Update, f.Accounts = u, []status.Account{{Name: "work"}}
	saveStatus(t, home, f)
	_, out, _ := runHome(t, home, "statusline")
	_, jout, _ := runHome(t, home, "statusline", "--json")
	if strings.Contains(out, "↑") || strings.Contains(jout, "updateAvailable") {
		t.Errorf("shown though none is available: %q %s", out, jout)
	}
	// Daemon down.
	home2, port2 := statuslineEnv(t, false, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home2, os.Getpid(), port2)
	saveStatus(t, home2, status.File{Update: availableUpdate()})
	_, out, _ = runHome(t, home2, "statusline")
	_, jout, _ = runHome(t, home2, "statusline", "--json")
	if strings.Contains(out, "↑") || strings.Contains(jout, "updateAvailable") {
		t.Errorf("shown with the daemon down: %q %s", out, jout)
	}
	// Not routed (off).
	home3, _ := statuslineEnv(t, true, nil)
	saveStatus(t, home3, status.File{Update: availableUpdate()})
	_, out, _ = runHome(t, home3, "statusline")
	_, jout, _ = runHome(t, home3, "statusline", "--json")
	if strings.Contains(out, "↑") || strings.Contains(jout, "updateAvailable") {
		t.Errorf("shown while off: %q %s", out, jout)
	}
}

func TestStatuslineHidesAnUpdateTheRunningVersionAlreadyHas(t *testing.T) {
	withCLIVersion(t, "0.6.0")
	home, _ := v2Env(t)
	var f status.File
	f.Update, f.Accounts = availableUpdate(), []status.Account{{Name: "work"}}
	saveStatus(t, home, f)
	if _, out, _ := runHome(t, home, "statusline"); strings.Contains(out, "↑") {
		t.Errorf("out = %q, want no update suffix", out)
	}
}

// The `up` line (routed, daemon up, no serving account) never shows an update.
func TestStatuslineUpStateOmitsTheUpdate(t *testing.T) {
	withCLIVersion(t, "0.5.0")
	home, port := statuslineEnv(t, true, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port)
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error { st.Serving = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	saveStatus(t, home, status.File{Update: availableUpdate()})
	_, out, _ := runHome(t, home, "statusline")
	_, jout, _ := runHome(t, home, "statusline", "--json")
	if out != "c» up\n" || strings.Contains(jout, "updateAvailable") {
		t.Errorf("out = %q json = %s, want the bare up line and no updateAvailable", out, jout)
	}
}

func TestStatuslineShowsARestartPendingAndOmitsItWhenNone(t *testing.T) {
	withCLIVersion(t, "0.6.0")
	home, _ := v2Env(t)
	var f status.File
	f.Accounts = []status.Account{{Name: "work"}}
	f.SetRestartPending("0.6.0")
	saveStatus(t, home, f)
	_, out, _ := runHome(t, home, "statusline")
	if want := "c» work · 5h – · 7d – · 2/2 ok · ⟳0.6.0\n"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	_, jout, _ := runHome(t, home, "statusline", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jout), &got); err != nil {
		t.Fatal(err)
	}
	if got["restartPending"] != "0.6.0" {
		t.Errorf("json = %s, want restartPending 0.6.0", jout)
	}
	f.SetRestartPending("")
	saveStatus(t, home, f)
	_, out, _ = runHome(t, home, "statusline")
	_, jout, _ = runHome(t, home, "statusline", "--json")
	if strings.Contains(out, "⟳") || strings.Contains(jout, "restartPending") {
		t.Errorf("shown with nothing pending: %q %s", out, jout)
	}
}

// status --json says what the text says: an update the CLI already has is
// not available, whatever the cache last recorded.
func TestStatusJSONUpdateAvailableIsComputedNotTheRawCache(t *testing.T) {
	withCLIVersion(t, "0.6.0")
	home := t.TempDir()
	seedState(t, home, "D", "A")
	writeUpdateCache(t, home, availableUpdate()) // cache: available, latest 0.6.0
	_, out, errb := runHome(t, home, "status", "--json")
	var got struct {
		Update struct {
			Latest    string `json:"latest"`
			Available bool   `json:"available"`
		} `json:"update"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s\n%s", err, out, errb)
	}
	if got.Update.Latest != "0.6.0" || got.Update.Available {
		t.Fatalf("update = %+v, want latest kept and available false after the install", got.Update)
	}
}
