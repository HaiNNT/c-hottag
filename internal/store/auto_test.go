package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

// rawState returns state.json's top-level members as raw JSON.
func rawState(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func threeAccountStore(t *testing.T) store.Store {
	t.Helper()
	s := store.Store{Dir: t.TempDir()}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B", "C"} {
			if err := st.Add(acct(n, "")); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAbsentAutoIsOnAndWritesNoKey pins S1 and spec §7: absent means on,
// and a state that never set anything writes no "auto" key.
func TestAbsentAutoIsOnAndWritesNoKey(t *testing.T) {
	s := threeAccountStore(t)
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Auto != nil || !st.AutoOn() {
		t.Fatalf("Auto = %+v, AutoOn = %v; want nil and on", st.Auto, st.AutoOn())
	}
	if _, ok := rawState(t, s.Dir)["auto"]; ok {
		t.Fatal(`state.json has an "auto" key, but nothing set one`)
	}
}

// TestLegacyAutoBlockReadsAsOnAndIsDroppedOnWrite is Review Focus 1: every
// pre-M4 state.json holds {"enabled": false, "threshold": 95}, which no
// command set. It must read as on, and the next write must drop it.
func TestLegacyAutoBlockReadsAsOnAndIsDroppedOnWrite(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":1,"accounts":[{"name":"A","dir":"/slots/A","addedAt":"2026-09-20T10:00:00Z"}],` +
		`"serving":"A","remote":"A","port":47821,"auto":{"enabled":false,"threshold":95}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.Store{Dir: dir}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Auto != nil || !st.AutoOn() {
		t.Fatalf("legacy block: Auto = %+v, AutoOn = %v; want absent and on (R68)", st.Auto, st.AutoOn())
	}
	if _, err := s.Update(func(*store.State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if v, ok := rawState(t, dir)["auto"]; ok {
		t.Fatalf(`the next write kept the legacy block: "auto": %s`, v)
	}
}

func TestAutoOffIsWrittenExplicitly(t *testing.T) {
	s := threeAccountStore(t)
	if _, err := s.Update(func(st *store.State) error { st.SetAutoEnabled(false); return nil }); err != nil {
		t.Fatal(err)
	}
	var a map[string]json.RawMessage
	if err := json.Unmarshal(rawState(t, s.Dir)["auto"], &a); err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || string(a["enabled"]) != "false" {
		t.Fatalf(`"auto" = %v, want exactly {"enabled": false}`, a)
	}
	st, _ := s.Load()
	if st.AutoOn() {
		t.Fatal("auto off did not persist")
	}
	if _, err := s.Update(func(st *store.State) error { st.SetAutoEnabled(true); return nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Load(); !st.AutoOn() || st.Auto == nil || st.Auto.Enabled == nil || !*st.Auto.Enabled {
		t.Fatalf("auto on = %+v, want an explicit true", st.Auto)
	}
}

// TestAutoSettingsWriteOnlyWhatWasSet: mode, one switch point and one
// duration; the rest stay absent.
func TestAutoSettingsWriteOnlyWhatWasSet(t *testing.T) {
	s := threeAccountStore(t)
	if _, err := s.Update(func(st *store.State) error {
		st.SetAutoMode("cache-optimize")
		if err := st.SetAutoSetting("5h.max20x", "95"); err != nil {
			return err
		}
		return st.SetAutoSetting("cooldown", "10m")
	}); err != nil {
		t.Fatal(err)
	}
	var a map[string]json.RawMessage
	if err := json.Unmarshal(rawState(t, s.Dir)["auto"], &a); err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || string(a["mode"]) != `"cache-optimize"` || string(a["cooldown"]) != `"10m"` {
		t.Fatalf(`"auto" = %v, want exactly mode, switchPoints and cooldown`, a)
	}
	st, _ := s.Load()
	if st.Auto.SwitchPoints["5h.max20x"] != 95 || !st.AutoOn() {
		t.Fatalf("Auto = %+v", st.Auto)
	}
}

func TestSetAutoSettingRefusesANonIntegerSwitchPoint(t *testing.T) {
	st := store.Default()
	if err := st.SetAutoSetting("5h.pro", "ninety"); err == nil {
		t.Fatal("a non-integer switch point was stored")
	}
	if st.Auto != nil {
		t.Fatalf("a refused setting left Auto = %+v", st.Auto)
	}
}

// TestResetAutoSettingsKeepsTheSwitchAndTheMode: reset clears overrides
// only, and a block left empty disappears.
func TestResetAutoSettingsKeepsTheSwitchAndTheMode(t *testing.T) {
	st := store.Default()
	st.SetAutoEnabled(false)
	st.SetAutoMode("cache-optimize")
	_ = st.SetAutoSetting("7d.pro", "90")
	_ = st.SetAutoSetting("hold5h", "10m")
	_ = st.SetAutoSetting("hold7d", "1h")
	st.ResetAutoSettings()
	a := st.AutoSettings()
	if a.Enabled == nil || *a.Enabled || a.Mode != "cache-optimize" || a.SwitchPoints != nil || a.Hold5h != "" || a.Hold7d != "" || a.Cooldown != "" {
		t.Fatalf("after reset: %+v", a)
	}
	only := store.Default()
	_ = only.SetAutoSetting("cooldown", "1m")
	only.ResetAutoSettings()
	if only.Auto != nil {
		t.Fatalf("a reset that leaves nothing must remove the block, got %+v", only.Auto)
	}
}

// TestAutoEditsNeverLeakIntoACopy: store.Cache hands out State copies, so
// an edit must replace the block, never write through a shared pointer.
func TestAutoEditsNeverLeakIntoACopy(t *testing.T) {
	st := store.Default()
	_ = st.SetAutoSetting("5h.pro", "90")
	cp := st
	_ = st.SetAutoSetting("5h.pro", "80")
	st.SetAutoEnabled(false)
	if cp.Auto.SwitchPoints["5h.pro"] != 90 || !cp.AutoOn() {
		t.Fatalf("the copy saw a later edit: %+v", cp.Auto)
	}
	got := st.AutoSettings()
	got.SwitchPoints["5h.pro"] = 1
	if st.Auto.SwitchPoints["5h.pro"] != 80 {
		t.Fatal("AutoSettings returned a map shared with the State")
	}
}

func TestPlanAndUnitsRoundTripAndAreOmittedUntilSet(t *testing.T) {
	s := threeAccountStore(t)
	if strings.Contains(string(rawState(t, s.Dir)["accounts"]), "plan") {
		t.Fatal("a plan key was written before any was set")
	}
	if _, err := s.Update(func(st *store.State) error {
		st.Accounts[1].Plan, st.Accounts[1].Units = "max20x", 25
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Load()
	if st.Accounts[1].Plan != "max20x" || st.Accounts[1].Units != 25 || st.Accounts[0].Plan != "" {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
}

func TestSwapServingMovesServing(t *testing.T) {
	s := threeAccountStore(t)
	st, err := s.SwapServing("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if st.Serving != "B" {
		t.Fatalf("serving = %q, want B (the registered spelling)", st.Serving)
	}
	if st, _ := s.Load(); st.Serving != "B" {
		t.Fatalf("on disk serving = %q", st.Serving)
	}
}

// TestSwapServingLosesToAConcurrentTag is Review Focus 2 at the store: a
// tag that lands between the daemon's decision and its write wins.
func TestSwapServingLosesToAConcurrentTag(t *testing.T) {
	s := threeAccountStore(t)
	if _, err := s.Update(func(st *store.State) error { st.Serving = "C"; return nil }); err != nil {
		t.Fatal(err)
	}
	_, err := s.SwapServing("A", "B")
	if !errors.Is(err, store.ErrServingChanged) {
		t.Fatalf("err = %v, want ErrServingChanged", err)
	}
	if st, _ := s.Load(); st.Serving != "C" {
		t.Fatalf("serving = %q, want the tagged C untouched", st.Serving)
	}
}

func TestSwapServingRefusesAnUnknownTarget(t *testing.T) {
	s := threeAccountStore(t)
	if _, err := s.SwapServing("A", "Zed"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if st, _ := s.Load(); st.Serving != "A" {
		t.Fatalf("serving = %q, want A", st.Serving)
	}
}
