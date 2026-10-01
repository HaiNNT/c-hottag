package cli_test

import (
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/usage"
)

// jsonTimeString formats v exactly the way encoding/json would when
// marshalling a time.Time field, using time.Time's own MarshalJSON rather
// than a hand-picked layout string, so a golden test built from it stays
// correct regardless of RFC3339Nano's trailing-zero-trimming behaviour.
func jsonTimeString(t *testing.T, v time.Time) string {
	t.Helper()
	b, err := v.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	return s[1 : len(s)-1] // strip the surrounding quotes
}

// TestStatusJSONGoldenOutput pins the milestone's stated exit criterion
// ("status --json golden output", spec §5.1: additive changes only) against
// a realistic cache: a limited account with a known (still-future) reset, an
// account whose limit has already ELAPSED, a healthy account with
// percentages, an account chottag has never observed, and serving/remote
// set.
//
// This compares the actual bytes against a hand-typed literal, deliberately
// NOT built by marshalling a status.File and NOT verified by unmarshalling
// the output back into one. Either of those round-trips through the same
// struct tags the production code uses, so a field renamed on Account,
// Usage or Limits marshals out and unmarshals back in under its new name
// without the test ever noticing — which is exactly how this schema drifted
// unnoticed before. The only thing here that isn't a literal is the small
// set of clock-derived timestamps, and those are produced by time.Time's
// own MarshalJSON, not by status package code. M1d-d adds the §5.3 header
// (ok, warnings) after version; everything after it is byte-identical to
// M1d-c.
func TestStatusJSONGoldenOutput(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	limitedUntilB := now.Add(2 * time.Hour) // still in the future: RollUpAt must not have expired it
	fiveHourResetB := now.Add(3 * time.Hour)
	fiveHourResetC := now.Add(4 * time.Hour)
	sevenDayResetC := now.Add(48 * time.Hour)
	elapsedUntilE := now.Add(-5 * time.Minute) // already gone by: E's own window reset 5 minutes ago
	updatedAtE := now.Add(-20 * time.Minute)   // stale (> status.StaleAfter old)

	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, name := range []string{"B", "C", "E", "D"} {
			dir, err := s.SlotDir(name)
			if err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: name, Dir: dir}); err != nil {
				return err
			}
		}
		for i := range st.Accounts {
			switch st.Accounts[i].Name {
			case "B":
				st.Accounts[i].Email = "b@example.com"
				st.Accounts[i].Org = "Widget Co"
			case "C":
				st.Accounts[i].Email = "c@example.com"
				st.Accounts[i].Plan = "max20x"
			case "E":
				st.Accounts[i].Email = "e@example.com"
			case "D":
				st.Accounts[i].Email = "d@example.com"
			}
		}
		st.Serving, st.Remote = "C", "B"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var f status.File
	// B: limited (by its 7d window) with a known reset, but still carries
	// usage for both windows — a real refusal response reports both.
	f.Observe("B", usage.Snapshot{
		Known: true, At: now, Overall: "rejected",
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 0.42, Status: "allowed", ResetsAt: fiveHourResetB},
		SevenDay: usage.Window{Known: true, HasUtilization: true, Utilization: 1.0, Status: "rejected", ResetsAt: limitedUntilB},
	}, usage.Verdict{Limited: true, Until: limitedUntilB, Window: "seven_day"})
	// C: healthy, with known percentages on both windows.
	f.Observe("C", usage.Snapshot{
		Known: true, At: now, Overall: "allowed",
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 0.06, Status: "allowed", ResetsAt: fiveHourResetC},
		SevenDay: usage.Window{Known: true, HasUtilization: true, Utilization: 0.23, Status: "allowed", ResetsAt: sevenDayResetC},
	}, usage.Verdict{})
	// E: was limited (by its 5h window), but that window's own reset has
	// already gone by and nothing has told chottag since (M1c has no poll).
	// The --json projection must report limited: false while still keeping
	// limitedUntil so a consumer can see what elapsed.
	f.Observe("E", usage.Snapshot{
		Known: true, At: updatedAtE, Overall: "rejected",
		FiveHour: usage.Window{Known: true, HasUtilization: true, Utilization: 1.0, Status: "rejected", ResetsAt: elapsedUntilE},
	}, usage.Verdict{Limited: true, Until: elapsedUntilE, Window: "five_hour"})
	// D: never observed; EnsureAccounts (inside runStatus) seeds its row.
	// M4: the daemon's auto-switch view, passed through as stored.
	f.SetAuto(status.Auto{Mode: "balanced", Decision: "holding C (5h 96%, resets in 9m)",
		LastSwitch: &status.AutoSwitch{From: "B", To: "C", Trigger: "limit", Window: "7d", At: now}})
	saveStatus(t, home, f)

	_, out, errb := runHome(t, home, "status", "--json")
	if errb != "" {
		t.Fatalf("stderr = %q, want empty", errb)
	}

	want := "{\n" +
		"  \"version\": 1,\n" +
		"  \"ok\": true,\n" +
		"  \"warnings\": [],\n" +
		"  \"accounts\": [\n" +
		"    {\n" +
		"      \"name\": \"B\",\n" +
		"      \"email\": \"b@example.com\",\n" +
		"      \"org\": \"Widget Co\",\n" +
		"      \"usage\": {\n" +
		"        \"fiveHourPct\": 42,\n" +
		"        \"sevenDayPct\": 100,\n" +
		"        \"fiveHourResetsAt\": \"" + jsonTimeString(t, fiveHourResetB) + "\",\n" +
		"        \"sevenDayResetsAt\": \"" + jsonTimeString(t, limitedUntilB) + "\",\n" +
		"        \"updatedAt\": \"" + jsonTimeString(t, now) + "\",\n" +
		"        \"source\": \"observed\"\n" +
		"      },\n" +
		"      \"limited\": true,\n" +
		"      \"limitedUntil\": \"" + jsonTimeString(t, limitedUntilB) + "\",\n" +
		"      \"window\": \"seven_day\",\n" +
		"      \"rotate\": true,\n" +
		"      \"stale\": false\n" +
		"    },\n" +
		"    {\n" +
		"      \"name\": \"C\",\n" +
		"      \"email\": \"c@example.com\",\n" +
		"      \"usage\": {\n" +
		"        \"fiveHourPct\": 6,\n" +
		"        \"sevenDayPct\": 23,\n" +
		"        \"fiveHourResetsAt\": \"" + jsonTimeString(t, fiveHourResetC) + "\",\n" +
		"        \"sevenDayResetsAt\": \"" + jsonTimeString(t, sevenDayResetC) + "\",\n" +
		"        \"updatedAt\": \"" + jsonTimeString(t, now) + "\",\n" +
		"        \"source\": \"observed\"\n" +
		"      },\n" +
		"      \"rotate\": true,\n" +
		"      \"plan\": \"max20x\",\n" +
		"      \"stale\": false\n" +
		"    },\n" +
		"    {\n" +
		"      \"name\": \"E\",\n" +
		"      \"email\": \"e@example.com\",\n" +
		"      \"usage\": {\n" +
		"        \"fiveHourPct\": 100,\n" +
		"        \"fiveHourResetsAt\": \"" + jsonTimeString(t, elapsedUntilE) + "\",\n" +
		"        \"updatedAt\": \"" + jsonTimeString(t, updatedAtE) + "\",\n" +
		"        \"source\": \"observed\"\n" +
		"      },\n" +
		"      \"limitedUntil\": \"" + jsonTimeString(t, elapsedUntilE) + "\",\n" +
		"      \"window\": \"five_hour\",\n" +
		"      \"rotate\": true,\n" +
		"      \"stale\": true\n" +
		"    },\n" +
		"    {\n" +
		"      \"name\": \"D\",\n" +
		"      \"email\": \"d@example.com\",\n" +
		"      \"rotate\": true,\n" +
		"      \"stale\": true\n" +
		"    }\n" +
		"  ],\n" +
		"  \"serving\": \"C\",\n" +
		"  \"remote\": \"B\",\n" +
		"  \"limits\": {\n" +
		"    \"allLimited\": false,\n" +
		"    \"nextReset\": \"" + jsonTimeString(t, limitedUntilB) + "\",\n" +
		"    \"nextResetAccount\": \"B\"\n" +
		"  },\n" +
		"  \"auto\": {\n" +
		"    \"mode\": \"balanced\",\n" +
		"    \"decision\": \"holding C (5h 96%, resets in 9m)\",\n" +
		"    \"lastSwitch\": {\n" +
		"      \"from\": \"B\",\n" +
		"      \"to\": \"C\",\n" +
		"      \"trigger\": \"limit\",\n" +
		"      \"window\": \"7d\",\n" +
		"      \"at\": \"" + jsonTimeString(t, now) + "\"\n" +
		"    },\n" +
		"    \"userChosen\": false,\n" +
		"    \"burnRate\": 0\n" +
		"  },\n" +
		"  \"updates\": {\n" +
		"    \"check\": true,\n" +
		"    \"auto\": false,\n" +
		"    \"restart\": true\n" +
		"  }\n" +
		"}\n"

	if out != want {
		t.Fatalf("status --json output mismatch.\ngot:\n%s\nwant:\n%s", out, want)
	}
}
