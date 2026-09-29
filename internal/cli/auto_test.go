package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// autoHome seeds A (max20x, serving) and B (plan unknown) in a temp home.
func autoHome(t *testing.T) (string, store.Store) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		if err := st.Add(store.Account{Name: "A", Plan: "max20x", Dir: filepath.Join(home, "accounts", "A")}); err != nil {
			return err
		}
		return st.Add(store.Account{Name: "B", Dir: filepath.Join(home, "accounts", "B")})
	}); err != nil {
		t.Fatal(err)
	}
	return home, s
}

// readAutoState returns state.json's raw "auto" member, or "" if absent.
func readAutoState(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	return string(raw["auto"])
}

func TestAutoShowsTheDefaultsAndWritesNothing(t *testing.T) {
	home, _ := autoHome(t)
	before, _ := os.ReadFile(filepath.Join(home, "state.json"))
	code, out, errs := runChottag(t, "auto")
	if code != exit.OK {
		t.Fatalf("auto = %d; stderr %q", code, errs)
	}
	for _, want := range []string{
		"auto: on · mode balanced\n",
		"switch points: 5h pro 88% · max5x 93% · team 93% · max20x 98%\n",
		"               7d pro 93% · max5x 98% · team 98% · max20x 99%\n",
		"holds: 5h 30m · 7d 3h · cooldown 15m\n",
		"  A      max20x   unknown",
		"  B      -        unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("auto output lacks %q:\n%s", want, out)
		}
	}
	after, _ := os.ReadFile(filepath.Join(home, "state.json"))
	if string(before) != string(after) {
		t.Fatal("`auto` with no verb wrote state.json")
	}
}

// TestAutoShowsOnForALegacyStateFile is Review Focus 1 at the command: a
// pre-M4 state.json's {"enabled": false, "threshold": 95} reads as on.
func TestAutoShowsOnForALegacyStateFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	legacy := `{"version":1,"accounts":[{"name":"A","dir":"` + filepath.Join(home, "accounts", "A") + `","addedAt":"2026-09-20T10:00:00Z"}],` +
		`"serving":"A","remote":"A","port":47821,"auto":{"enabled":false,"threshold":95}}`
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runChottag(t, "auto")
	if code != exit.OK || !strings.HasPrefix(out, "auto: on · mode balanced\n") {
		t.Fatalf("auto = %d %q, want on (R68)", code, out)
	}
}

func TestAutoOffAndOnPersist(t *testing.T) {
	home, s := autoHome(t)
	if code, out, errs := runChottag(t, "auto", "off"); code != exit.OK || !strings.HasPrefix(out, "auto: off · mode balanced\n") {
		t.Fatalf("auto off = %d %q %q", code, out, errs)
	}
	if st, _ := s.Load(); st.AutoOn() {
		t.Fatal("auto off did not persist")
	}
	if got := readAutoState(t, home); !strings.Contains(got, `"enabled": false`) {
		t.Fatalf(`"auto" = %s`, got)
	}
	if code, out, _ := runChottag(t, "auto", "on"); code != exit.OK || !strings.HasPrefix(out, "auto: on") {
		t.Fatalf("auto on = %d %q", code, out)
	}
}

func TestAutoModeSetsAndValidates(t *testing.T) {
	home, s := autoHome(t)
	if code, out, _ := runChottag(t, "auto", "mode", "cache-optimize"); code != exit.OK ||
		!strings.Contains(out, "mode cache-optimize") || !strings.Contains(out, "5h pro 100%") || !strings.Contains(out, "holds: 5h 0s · 7d 0s · cooldown 0s") {
		t.Fatalf("auto mode cache-optimize = %d %q", code, out)
	}
	if st, _ := s.Load(); st.Auto == nil || st.Auto.Mode != "cache-optimize" {
		t.Fatalf("mode not stored: %+v", st.Auto)
	}
	before := readAutoState(t, home)
	code, out, errs := runChottag(t, "auto", "mode", "continuous")
	if code != exit.Usage || out != "" || errs != "chottag: auto mode takes balanced or cache-optimize, not \"continuous\"\n" {
		t.Fatalf("auto mode continuous = %d %q %q, want exit 2", code, out, errs)
	}
	if readAutoState(t, home) != before {
		t.Fatal("a refused mode changed state.json")
	}
}

// TestAutoSetValidatesTheBounds pins spec §7: a switch point is an integer
// from 50 to 100; a duration from 0 to 24h; anything else is exit 2 and
// writes nothing.
func TestAutoSetValidatesTheBounds(t *testing.T) {
	home, s := autoHome(t)
	if code, out, errs := runChottag(t, "auto", "set", "5h.max20x", "95"); code != exit.OK ||
		!strings.Contains(out, "max20x 95%") || !strings.Contains(out, "overrides: 5h.max20x (chottag auto reset clears them)") {
		t.Fatalf("auto set = %d %q %q", code, out, errs)
	}
	if code, _, _ := runChottag(t, "auto", "set", "hold7d", "24h"); code != exit.OK {
		t.Fatalf("auto set hold7d 24h = %d", code)
	}
	before := readAutoState(t, home)
	for _, args := range [][]string{
		{"auto", "set", "5h.pro", "49"}, {"auto", "set", "5h.pro", "101"}, {"auto", "set", "7d.team", "ninety"},
		{"auto", "set", "hold5h", "25h"}, {"auto", "set", "cooldown", "-1m"}, {"auto", "set", "threshold", "95"},
		{"auto", "set", "5h.pro"}, {"auto", "sideways"}, {"auto", "on", "off"},
	} {
		code, out, errs := runChottag(t, args...)
		if code != exit.Usage || out != "" || errs == "" {
			t.Errorf("%q = %d %q %q, want exit 2 with a message", args, code, out, errs)
		}
	}
	if readAutoState(t, home) != before {
		t.Fatal("a refused setting changed state.json")
	}
	st, _ := s.Load()
	if st.Auto.SwitchPoints["5h.max20x"] != 95 || st.Auto.Hold7d != "24h" {
		t.Fatalf("Auto = %+v", st.Auto)
	}
}

func TestAutoResetClearsOverridesOnly(t *testing.T) {
	_, s := autoHome(t)
	for _, args := range [][]string{{"auto", "off"}, {"auto", "mode", "cache-optimize"}, {"auto", "set", "5h.pro", "90"}, {"auto", "set", "cooldown", "1m"}} {
		if code, _, errs := runChottag(t, args...); code != exit.OK {
			t.Fatalf("%q = %d %q", args, code, errs)
		}
	}
	code, out, _ := runChottag(t, "auto", "reset")
	if code != exit.OK || strings.Contains(out, "overrides:") || !strings.HasPrefix(out, "auto: off · mode cache-optimize") {
		t.Fatalf("auto reset = %d %q", code, out)
	}
	st, _ := s.Load()
	if st.AutoOn() || st.Auto.Mode != "cache-optimize" || st.Auto.SwitchPoints != nil || st.Auto.Cooldown != "" {
		t.Fatalf("after reset: %+v", st.Auto)
	}
}

// TestAutoShowsAccountsDecisionAndLastSwitch: usage from the cache, the
// daemon's decision while its heartbeat is fresh, and the last switch.
func TestAutoShowsAccountsDecisionAndLastSwitch(t *testing.T) {
	home, _ := autoHome(t)
	now := time.Now()
	f := status.File{
		Accounts: []status.Account{{Name: "A", Usage: &status.Usage{
			FiveHourPct: autoPct(91), SevenDayPct: autoPct(40), UpdatedAt: now, Source: "observed",
			FiveHourResetsAt: now.Add(2 * time.Hour), SevenDayResetsAt: now.Add(72 * time.Hour),
		}}},
		Daemon: &status.Daemon{Heartbeat: now},
	}
	f.SetAuto(status.Auto{Mode: "balanced", Decision: "staying on A (5h 91% < 98%)",
		LastSwitch: &status.AutoSwitch{From: "B", To: "A", Trigger: "limit", Window: "5h", At: now.Add(-time.Hour), Retried: true}})
	b, _ := status.Marshal(f)
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runChottag(t, "auto")
	reset5 := now.Add(2 * time.Hour).Local().Format("15:04")
	for _, want := range []string{
		"  A      max20x   91%", // columns widen to fit B's "unknown"
		" " + reset5 + " ",
		"40%",
		"decision: staying on A (5h 91% < 98%)\n",
		"last switch: B -> A " + now.Add(-time.Hour).Local().Format("15:04") + " (limit 5h), request resent\n",
	} {
		if code != exit.OK || !strings.Contains(out, want) {
			t.Errorf("auto output lacks %q:\n%s", want, out)
		}
	}
	// A stopped daemon's decision is not shown (ruling 9); the last switch is.
	f.Daemon.Heartbeat = now.Add(-10 * time.Minute)
	b, _ = status.Marshal(f)
	status.WriteBytes(status.Path(home), b)
	_, out, _ = runChottag(t, "auto")
	if strings.Contains(out, "decision:") || !strings.Contains(out, "last switch:") {
		t.Fatalf("with a stale heartbeat:\n%s", out)
	}
}

func TestAutoJSONCarriesTheEffectiveSettings(t *testing.T) {
	autoHome(t)
	runChottag(t, "auto", "set", "5h.max20x", "95")
	code, out, _ := runChottag(t, "auto", "--json")
	if code != exit.OK {
		t.Fatalf("auto --json = %d %s", code, out)
	}
	var doc struct {
		Enabled      bool           `json:"enabled"`
		Mode         string         `json:"mode"`
		SwitchPoints map[string]int `json:"switchPoints"`
		Hold5h       string         `json:"hold5h"`
		Overrides    []string       `json:"overrides"`
		Accounts     []struct {
			Name, Plan, Tier string
			Units            float64
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Enabled || doc.Mode != "balanced" || doc.SwitchPoints["5h.max20x"] != 95 || doc.SwitchPoints["7d.pro"] != 93 ||
		doc.Hold5h != "30m" || len(doc.Overrides) != 1 || len(doc.Accounts) != 2 ||
		doc.Accounts[0].Tier != "max20x" || doc.Accounts[0].Units != 20 || doc.Accounts[1].Plan != "" || doc.Accounts[1].Tier != "max5x" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestDurText(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 30 * time.Minute: "30m", 3 * time.Hour: "3h", 90 * time.Minute: "1h30m", 45 * time.Second: "45s"} {
		if got := durText(d); got != want {
			t.Errorf("durText(%v) = %q, want %q", d, got, want)
		}
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "auto shows the settings", command: "auto",
			setup: func(t *testing.T) []string { autoHome(t); return []string{"auto"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["enabled"] != true || doc["mode"] != "balanced" {
					t.Errorf("doc = %v", doc)
				}
			},
		},
		jsonCase{
			name: "auto mode with an unknown mode", command: "auto",
			setup:    func(t *testing.T) []string { autoHome(t); return []string{"auto", "mode", "turbo"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}
