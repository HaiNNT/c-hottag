package journal

import (
	"fmt"
	"testing"
	"time"
)

// u makes a valid native id from a short label.
func u(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("00000000-0000-0000-0000-%012x", int(s[0]))
}

func lost(pid int, native string, ended time.Time) Entry {
	return Entry{PID: pid, Started: ended.Add(-time.Hour), Dir: "/Users/alice/r", Native: u(native), Ended: ended, Outcome: OutcomeLost}
}

func natives(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Native)
	}
	return out
}

func TestLostBatchWindowAndFilters(t *testing.T) {
	es := []Entry{
		lost(1, "A", t0.Add(10*time.Second)),
		lost(2, "B", t0),
		lost(3, "C", t0.Add(-10*time.Minute)), // earlier crash: outside the window
		lost(4, "", t0),                       // never sent a request
		{PID: 5, Native: u("D"), Ended: t0, Outcome: OutcomeExited},
		func() Entry { e := lost(6, "E", t0); e.Resumed = t0.Add(time.Minute); return e }(),
		lost(7, "F", t0), {PID: 8, SID: "x", Native: u("F")}, // F runs again (open, native)
		lost(9, "G", t0), {PID: 10, SID: "y", ResumeOf: u("G")}, // G resumed by cmux (open, resumeOf)
	}
	got := natives(LostBatch(es, t0.Add(time.Hour)))
	want := []string{u("A"), u("B")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("LostBatch natives = %v, want %v", got, want)
	}
}

func TestLostBatchDedupsNativeKeepingNewest(t *testing.T) {
	es := []Entry{lost(1, "A", t0), lost(2, "A", t0.Add(5*time.Second))}
	got := LostBatch(es, t0.Add(time.Hour))
	if len(got) != 1 || got[0].PID != 2 {
		t.Fatalf("got %+v, want only pid 2", got)
	}
}

func TestLostBatchEmpty(t *testing.T) {
	if got := LostBatch(nil, t0); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

const nat1 = "11111111-2222-3333-4444-555555555555"

func TestCommand(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Dir: "/Users/alice/repo", Native: nat1}, `cd '/Users/alice/repo' && claude --resume ` + nat1 + ``},
		{Entry{Dir: "/Users/alice/repo", Native: nat1, Pool: "default"}, `cd '/Users/alice/repo' && claude --resume ` + nat1 + ``},
		{Entry{Dir: "/Users/alice/it's here", Native: nat1, Pool: "work"}, `cd '/Users/alice/it'\''s here' && CHOTTAG_POOL=work claude --resume ` + nat1 + ``},
	}
	for _, c := range cases {
		if got := Command(c.e); got != c.want {
			t.Errorf("Command(%+v) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestValidNative(t *testing.T) {
	for s, want := range map[string]bool{
		nat1:                                   true,
		"ABCDEFAB-1234-5678-9abc-def012345678": true,
		"":                                     false,
		"x; touch /tmp/p":                      false,
		"$(id)":                                false,
		nat1 + "\n":                            false,
		nat1 + "; id":                          false,
		"N1":                                   false,
		"1111111122223333444455555555555555":   false,
	} {
		if got := ValidNative(s); got != want {
			t.Errorf("ValidNative(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestLostBatchExcludesInvalidNative(t *testing.T) {
	es := []Entry{lost(1, "A", t0)}
	bad := lost(2, "B", t0)
	bad.Native = "x; touch /tmp/p"
	if got := LostBatch(append(es, bad), t0.Add(time.Hour)); len(got) != 1 || got[0].PID != 1 {
		t.Fatalf("got %+v, want only pid 1", got)
	}
}

func TestLostBatchOnlyRecent(t *testing.T) {
	es := []Entry{lost(1, "A", t0)}
	if got := LostBatch(es, t0.Add(23*time.Hour)); len(got) != 1 {
		t.Fatalf("lost 23h ago: got %+v, want it in the batch", got)
	}
	if got := LostBatch(es, t0.Add(25*time.Hour)); len(got) != 0 {
		t.Fatalf("lost 25h ago: got %+v, want none", got)
	}
}

func TestResumeID(t *testing.T) {
	x, y := u("X"), u("Y")
	cases := []struct {
		name string
		e    Entry
		want string
	}{
		{"native only", Entry{Native: y}, y},
		{"resumed session uses ResumeOf", Entry{ResumeOf: x, Native: y}, x},
		{"fork uses its own native", Entry{ResumeOf: x, Native: y, Fork: true}, y},
		{"invalid ResumeOf falls back", Entry{ResumeOf: "not-a-uuid", Native: y}, y},
	}
	for _, c := range cases {
		if got := c.e.ResumeID(); got != c.want {
			t.Errorf("%s: ResumeID = %q, want %q", c.name, got, c.want)
		}
	}
	e := Entry{Dir: "/Users/alice/r", ResumeOf: x, Native: y}
	if got, want := Command(e), "cd '/Users/alice/r' && claude --resume "+x; got != want {
		t.Errorf("Command = %q, want %q", got, want)
	}
	e.Fork = true
	if got, want := Command(e), "cd '/Users/alice/r' && claude --resume "+y; got != want {
		t.Errorf("fork Command = %q, want %q", got, want)
	}
}

func TestLostBatchDedupsByResumeID(t *testing.T) {
	a := lost(1, "A", t0)
	a.ResumeOf = u("X")
	b := lost(2, "B", t0.Add(5*time.Second))
	b.ResumeOf = u("X")
	got := LostBatch([]Entry{a, b}, t0.Add(time.Hour))
	if len(got) != 1 || got[0].PID != 2 {
		t.Fatalf("got %+v, want only pid 2", got)
	}
}

func TestLostBatchRunningResumeIDExcludes(t *testing.T) {
	l := lost(1, "A", t0)
	l.ResumeOf = u("X")
	open := Entry{PID: 2, SID: "s", Native: u("X")}
	if got := LostBatch([]Entry{l, open}, t0.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("open session with native X: got %+v", got)
	}
	open = Entry{PID: 2, SID: "s", ResumeOf: u("X"), Native: u("Z")}
	if got := LostBatch([]Entry{l, open}, t0.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("open session resuming X: got %+v", got)
	}
	// The lost entry's own Native is not its resume id, so a session running
	// that id does not exclude it.
	other := Entry{PID: 3, SID: "s2", Native: u("A")}
	if got := LostBatch([]Entry{l, other}, t0.Add(time.Hour)); len(got) != 1 {
		t.Fatalf("got %+v, want the lost entry kept", got)
	}
}
