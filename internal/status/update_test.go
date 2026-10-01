package status_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/status"
)

func TestFileWithoutUpdateWritesNoKey(t *testing.T) {
	b, err := json.Marshal(status.File{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"update"`) {
		t.Errorf("File without Update marshals to %s, want no update key", b)
	}
}

func TestFileUpdateRoundTrips(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	in := status.File{Version: 1, Update: &status.Update{
		Latest: "0.5.0", PublishedAt: at.Add(-48 * time.Hour), CheckedAt: at,
		Available: true, Notified: "0.5.0",
		Auto: &status.AutoAttempt{Version: "0.5.0", At: at, OK: false, Error: "boom"},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out status.File
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	u := out.Update
	if u == nil || u.Latest != "0.5.0" || !u.Available || u.Notified != "0.5.0" ||
		!u.CheckedAt.Equal(at) || !u.PublishedAt.Equal(at.Add(-48*time.Hour)) ||
		u.Auto == nil || u.Auto.OK || u.Auto.Error != "boom" || u.Auto.Version != "0.5.0" {
		t.Fatalf("round trip = %+v, want the original", u)
	}
	var empty status.File
	b, _ = json.Marshal(status.File{Update: &status.Update{}})
	if err := json.Unmarshal(b, &empty); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "checkedAt") || strings.Contains(string(b), "auto") {
		t.Errorf("empty Update marshals to %s, want zero fields omitted", b)
	}
}
