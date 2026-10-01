package sessions

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func one(t *testing.T, tr *Tracker) Activity {
	t.Helper()
	s := tr.Snapshot()
	if len(s) != 1 {
		t.Fatalf("want 1 entry, got %d", len(s))
	}
	return s[0]
}

func TestSeenEmptySIDNoop(t *testing.T) {
	tr := NewTracker()
	tr.Seen("", "default", "A", true, "n", t0)
	if len(tr.Snapshot()) != 0 {
		t.Fatal("empty sid recorded")
	}
}

func TestSeenRules(t *testing.T) {
	tests := []struct {
		name string
		run  func(tr *Tracker)
		want Activity
	}{
		{"requests count every call", func(tr *Tracker) {
			tr.Seen("s", "default", "", false, "", t0)
			tr.Seen("s", "default", "", false, "", t0)
			tr.Seen("s", "default", "", true, "", t0)
		}, Activity{SID: "s", Pool: "default", LastSeen: t0, Requests: 3}},
		{"account only on inference", func(tr *Tracker) {
			tr.Seen("s", "default", "A", false, "", t0)
		}, Activity{SID: "s", Pool: "default", LastSeen: t0, Requests: 1}},
		{"account set then kept by non-inference", func(tr *Tracker) {
			tr.Seen("s", "default", "A", true, "", t0)
			tr.Seen("s", "default", "B", false, "", t0)
		}, Activity{SID: "s", Pool: "default", Account: "A", LastSeen: t0, Requests: 2}},
		{"empty account keeps previous", func(tr *Tracker) {
			tr.Seen("s", "default", "A", true, "", t0)
			tr.Seen("s", "default", "", true, "", t0)
		}, Activity{SID: "s", Pool: "default", Account: "A", LastSeen: t0, Requests: 2}},
		{"account changes on inference", func(tr *Tracker) {
			tr.Seen("s", "default", "A", true, "", t0)
			tr.Seen("s", "default", "B", true, "", t0)
		}, Activity{SID: "s", Pool: "default", Account: "B", LastSeen: t0, Requests: 2}},
		{"lastSeen is the maximum", func(tr *Tracker) {
			tr.Seen("s", "default", "", false, "", t0.Add(time.Minute))
			tr.Seen("s", "default", "", false, "", t0)
		}, Activity{SID: "s", Pool: "default", LastSeen: t0.Add(time.Minute), Requests: 2}},
		{"first native id counts one", func(tr *Tracker) {
			tr.Seen("s", "default", "", false, "n1", t0)
			tr.Seen("s", "default", "", false, "n1", t0)
		}, Activity{SID: "s", Pool: "default", LastSeen: t0, Requests: 2, Conversations: 1}},
		{"changed native id adds one", func(tr *Tracker) {
			tr.Seen("s", "default", "", false, "n1", t0)
			tr.Seen("s", "default", "", false, "n2", t0)
			tr.Seen("s", "default", "", false, "", t0)
			tr.Seen("s", "default", "", false, "n2", t0)
		}, Activity{SID: "s", Pool: "default", LastSeen: t0, Requests: 4, Conversations: 2}},
		{"no native id no conversation", func(tr *Tracker) {
			tr.Seen("s", "default", "", false, "", t0)
		}, Activity{SID: "s", Pool: "default", LastSeen: t0, Requests: 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tc.run(tr)
			if got := one(t, tr); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestSnapshotSortedAndCopies(t *testing.T) {
	tr := NewTracker()
	tr.Seen("b", "default", "", false, "", t0)
	tr.Seen("a", "default", "", false, "", t0)
	s := tr.Snapshot()
	if len(s) != 2 || s[0].SID != "a" || s[1].SID != "b" {
		t.Fatalf("not sorted: %+v", s)
	}
	s[0].Requests = 99
	if tr.Snapshot()[0].Requests != 1 {
		t.Fatal("snapshot aliases internal state")
	}
}

func TestForget(t *testing.T) {
	tr := NewTracker()
	tr.Seen("old", "default", "", false, "", t0)
	tr.Seen("oldkept", "default", "", false, "", t0)
	tr.Seen("edge", "default", "", false, "", t0.Add(Grace))
	tr.Seen("fresh", "default", "", false, "", t0.Add(Grace+time.Second))
	now := t0.Add(2 * Grace)
	tr.Forget(func(sid string) bool { return sid == "oldkept" }, now, Grace)
	var got []string
	for _, a := range tr.Snapshot() {
		got = append(got, a.SID)
	}
	if strings.Join(got, ",") != "edge,fresh,oldkept" {
		t.Fatalf("got %v", got)
	}
	tr.Forget(func(string) bool { return false }, t0.Add(Grace+time.Second+Grace+time.Nanosecond), Grace)
	if len(tr.Snapshot()) != 0 {
		// edge (t0+10m) older than 10m by > grace, fresh by > grace, oldkept too
		t.Fatalf("left %+v", tr.Snapshot())
	}
}

func TestForgetExactlyGraceKept(t *testing.T) {
	tr := NewTracker()
	tr.Seen("s", "default", "", false, "", t0)
	tr.Forget(func(string) bool { return false }, t0.Add(Grace), Grace)
	if len(tr.Snapshot()) != 1 {
		t.Fatal("dropped at exactly grace")
	}
}

func TestJSONNeverHasNativeID(t *testing.T) {
	tr := NewTracker()
	const native = "9f1c2d3e-native-session-id"
	tr.Seen("s", "default", "A", true, native, t0)
	b, err := json.Marshal(tr.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), native) {
		t.Fatalf("native id leaked: %s", b)
	}
}

func TestConcurrent(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				tr.Seen("s", "default", "A", i%2 == 0, "n", t0.Add(time.Duration(i)))
				_ = tr.Snapshot()
			}
		}()
	}
	wg.Wait()
	if got := one(t, tr).Requests; got != 1600 {
		t.Fatalf("requests %d", got)
	}
}

func TestPeekCountsWithoutRecording(t *testing.T) {
	tr := NewTracker()
	if got := tr.Peek("s1", ""); got != 0 {
		t.Fatalf("unknown sid, no native id: %d, want 0", got)
	}
	if got := tr.Peek("s1", "n1"); got != 1 {
		t.Fatalf("unknown sid, first native id: %d, want 1", got)
	}
	if len(tr.Snapshot()) != 0 {
		t.Fatal("Peek recorded a session")
	}
	tr.Seen("s1", "default", "A", true, "n1", t0)
	if got := tr.Peek("s1", "n1"); got != 1 {
		t.Fatalf("same native id: %d, want 1", got)
	}
	if got := tr.Peek("s1", "n2"); got != 2 {
		t.Fatalf("new native id: %d, want 2", got)
	}
	if got := tr.Peek("s1", ""); got != 1 {
		t.Fatalf("no native id: %d, want 1", got)
	}
	if one(t, tr).Conversations != 1 {
		t.Fatal("Peek changed the count")
	}
}

func TestAccountIsTheLastInferenceAccount(t *testing.T) {
	tr := NewTracker()
	if tr.Account("s1") != "" {
		t.Fatal("unknown sid has an account")
	}
	tr.Seen("s1", "default", "B", true, "n1", t0)
	tr.Seen("s1", "default", "A", false, "n1", t0) // not inference: ignored
	if got := tr.Account("s1"); got != "B" {
		t.Fatalf("Account = %q, want B", got)
	}
}
