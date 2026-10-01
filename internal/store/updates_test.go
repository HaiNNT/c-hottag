package store_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/store"
)

func TestUpdateDefaultsWhenAbsent(t *testing.T) {
	st := store.Default()
	if !st.UpdateCheckOn() {
		t.Error("UpdateCheckOn() = false with no updates key, want on")
	}
	if st.AutoUpdateOn() {
		t.Error("AutoUpdateOn() = true with no updates key, want off")
	}
}

func TestUpdateSettersAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error { st.SetUpdateCheck(false); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.UpdateCheckOn() || st.AutoUpdateOn() {
		t.Errorf("after SetUpdateCheck(false): check=%v auto=%v, want both off", st.UpdateCheckOn(), st.AutoUpdateOn())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(b), `"check": false`) {
		t.Errorf("state.json = %s, want it to contain check false", b)
	}
	if _, err := s.Update(func(st *store.State) error { st.SetAutoUpdate(true); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err = s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !st.UpdateCheckOn() || !st.AutoUpdateOn() {
		t.Errorf("after SetAutoUpdate(true): check=%v auto=%v, want both on", st.UpdateCheckOn(), st.AutoUpdateOn())
	}
	if _, err := s.Update(func(st *store.State) error { st.SetAutoUpdate(false); return nil }); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Load()
	if !st.UpdateCheckOn() || st.AutoUpdateOn() {
		t.Errorf("after SetAutoUpdate(false): check=%v auto=%v, want check kept on, auto off", st.UpdateCheckOn(), st.AutoUpdateOn())
	}
}

func TestUpdatesAbsentWritesNoKey(t *testing.T) {
	b, err := json.Marshal(store.Default())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "updates") {
		t.Errorf("default state marshals to %s, want no updates key", b)
	}
}

func TestAutoRestartDefaultsOnAndRoundTrips(t *testing.T) {
	if !store.Default().RestartOn() {
		t.Fatal("RestartOn() = false with no updates key, want on")
	}
	dir := t.TempDir()
	s := store.Store{Dir: dir}
	if _, err := s.Update(func(st *store.State) error { st.SetAutoRestart(false); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.RestartOn() || !st.UpdateCheckOn() || st.AutoUpdateOn() {
		t.Errorf("after SetAutoRestart(false): restart=%v check=%v auto=%v, want only check on", st.RestartOn(), st.UpdateCheckOn(), st.AutoUpdateOn())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if !strings.Contains(string(b), `"restart": false`) {
		t.Errorf("state.json = %s, want it to contain restart false", b)
	}
	if _, err := s.Update(func(st *store.State) error { st.SetAutoRestart(true); return nil }); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.Load(); !st.RestartOn() {
		t.Error("RestartOn() = false after SetAutoRestart(true)")
	}
}
