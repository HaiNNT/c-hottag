package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
)

// notifyHome points CHOTTAG_HOME at a fresh, empty dir.
func notifyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	return home
}

// storedNotify returns state.json's raw "notify" member, or "absent".
func storedNotify(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	v, ok := raw["notify"]
	if !ok {
		return "absent"
	}
	return string(v)
}

func TestNotifyDefaultsToOnAndWritesNothing(t *testing.T) {
	home := notifyHome(t)
	code, out, errs := runChottag(t, "notify")
	if code != exit.OK || out != "notify on\n" {
		t.Fatalf("notify = %d %q; stderr %q, want 0 %q", code, out, errs, "notify on\n")
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("`notify` with no verb wrote state.json (stat: %v): showing the setting must write nothing", err)
	}
}

func TestNotifyOffPersistsAndIsShown(t *testing.T) {
	home, s := seedTwoAccounts(t)
	if code, out, errs := runChottag(t, "notify", "off"); code != exit.OK || out != "notify off\n" {
		t.Fatalf("notify off = %d %q; stderr %q", code, out, errs)
	}
	if got := storedNotify(t, home); got != "false" {
		t.Fatalf("state.json notify = %s, want false", got)
	}
	if code, out, _ := runChottag(t, "notify"); code != exit.OK || out != "notify off\n" {
		t.Fatalf("notify = %d %q, want the stored off", code, out)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 2 || st.Accounts[0].Name != "A" || st.Accounts[1].Name != "B" || st.Serving != "A" {
		t.Fatalf("state = %+v, want the rest of state.json untouched", st)
	}
}

// TestNotifyOffOnAHomeWithNoStateJSONCreatesIt covers a verb with no
// state.json yet at all (not even seeded accounts): the file must still be
// created, holding the new setting.
func TestNotifyOffOnAHomeWithNoStateJSONCreatesIt(t *testing.T) {
	home := notifyHome(t)
	if code, out, errs := runChottag(t, "notify", "off"); code != exit.OK || out != "notify off\n" {
		t.Fatalf("notify off = %d %q; stderr %q", code, out, errs)
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); err != nil {
		t.Fatalf("state.json missing after `notify off` on a home with none: %v", err)
	}
	if got := storedNotify(t, home); got != "false" {
		t.Fatalf("state.json notify = %s, want false", got)
	}
}

func TestNotifyOnWritesAnExplicitTrue(t *testing.T) {
	home, _ := seedTwoAccounts(t)
	if code, _, errs := runChottag(t, "notify", "off"); code != exit.OK {
		t.Fatalf("notify off = %d; stderr %q", code, errs)
	}
	if code, out, errs := runChottag(t, "notify", "on"); code != exit.OK || out != "notify on\n" {
		t.Fatalf("notify on = %d %q; stderr %q", code, out, errs)
	}
	if got := storedNotify(t, home); got != "true" {
		t.Fatalf("state.json notify = %s, want true", got)
	}
}

func TestNotifyRejectsBadInputAndWritesNothing(t *testing.T) {
	home := notifyHome(t)
	for _, args := range [][]string{
		{"notify", "maybe"},
		{"notify", "on", "off"},
		{"notify", "--loud"},
	} {
		code, out, errs := runChottag(t, args...)
		if code != exit.Usage || out != "" || !strings.Contains(errs, "notify") {
			t.Errorf("%q = %d %q; stderr %q, want exit 2 and a notify message", args, code, out, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused notify wrote state.json (stat: %v)", err)
	}
}

func TestNotifyBadVerbNamesIt(t *testing.T) {
	notifyHome(t)
	_, _, errs := runChottag(t, "notify", "maybe")
	if errs != "chottag: notify takes on or off, not \"maybe\"\n" {
		t.Fatalf("stderr = %q", errs)
	}
}

func init() {
	registerJSONCases(
		jsonCase{
			name: "notify shows the default", command: "notify",
			setup: func(t *testing.T) []string { notifyHome(t); return []string{"notify"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["notify"] != true {
					t.Errorf("doc = %v, want notify true", doc)
				}
			},
		},
		jsonCase{
			name: "notify off", command: "notify",
			setup: func(t *testing.T) []string { seedTwoAccounts(t); return []string{"notify", "off"} },
			check: func(t *testing.T, doc map[string]any) {
				if doc["notify"] != false {
					t.Errorf("doc = %v, want notify false", doc)
				}
			},
		},
		jsonCase{
			name: "notify with a bad verb", command: "notify",
			setup:    func(t *testing.T) []string { notifyHome(t); return []string{"notify", "maybe"} },
			wantExit: exit.Usage, wantCode: codeUsage,
		},
	)
}
