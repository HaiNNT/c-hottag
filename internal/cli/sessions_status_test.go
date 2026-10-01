package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/sessions"
	"github.com/HaiNNT/c-hottag/internal/status"
)

const (
	sidA1 = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	sidA2 = "a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2a2"
	sidB1 = "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"
	sidC1 = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// seedEntry writes one live registry entry under name (digits: Live reads only <pid>.json). Every entry names this
// test process's pid, which is certainly alive: alive() checks only that the
// pid exists (the seedLiveSessions trick).
func seedEntry(t *testing.T, home, name string, s session.Session) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.PID = os.Getpid()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "run", name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeSessionsCache(t *testing.T, home string, acts ...sessions.Activity) {
	t.Helper()
	b, err := status.Marshal(status.File{Sessions: acts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

func act(sid, account string, requests, conversations int) sessions.Activity {
	return sessions.Activity{SID: sid, Pool: "default", Account: account, LastSeen: t0, Requests: requests, Conversations: conversations}
}

// fourSessions: two identified on A, one on B, one identified with no account
// yet, and one unidentified (an old shim's entry).
func fourSessions(t *testing.T, home string) {
	t.Helper()
	seedState(t, home, "A", "B")
	seedEntry(t, home, "1", session.Session{Port: 47850, SID: sidA1, Pool: "default", Started: t0.Add(1 * time.Minute)})
	seedEntry(t, home, "2", session.Session{Port: 47850, SID: sidA2, Pool: "default", Started: t0.Add(2 * time.Minute)})
	seedEntry(t, home, "3", session.Session{Port: 47850, SID: sidB1, Pool: "default", Started: t0.Add(3 * time.Minute)})
	seedEntry(t, home, "4", session.Session{Port: 47850, SID: sidC1, Pool: "default", Started: t0.Add(4 * time.Minute)})
	seedEntry(t, home, "0", session.Session{Port: 47850, Started: t0})
	writeSessionsCache(t, home, act(sidA1, "A", 5, 2), act(sidA2, "A", 1, 1), act(sidB1, "B", 17, 1))
}

func TestStatusTextCountsSessionsPerAccount(t *testing.T) {
	home := t.TempDir()
	fourSessions(t, home)
	_, out, _ := runHome(t, home, "status")
	if !strings.Contains(out, "live sessions: 5 (A 2, B 1, – 2)\n") {
		t.Fatalf("text = %q", out)
	}
}

func TestStatusJSONListsSessionsByStarted(t *testing.T) {
	home := t.TempDir()
	fourSessions(t, home)
	_, out, _ := runHome(t, home, "status", "--json")
	var doc struct {
		Daemon   struct{ LiveSessions int }
		Sessions []map[string]any
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Daemon.LiveSessions != 5 || len(doc.Sessions) != 5 {
		t.Fatalf("doc = %s", out)
	}
	un := doc.Sessions[0]
	if _, has := un["sid"]; has || un["pid"] == nil || un["started"] == nil || un["account"] != nil {
		t.Fatalf("unidentified entry = %v", un)
	}
	a1 := doc.Sessions[1]
	if a1["sid"] != sidA1[:8] || a1["pool"] != "default" || a1["account"] != "A" ||
		a1["requests"] != 5.0 || a1["conversations"] != 2.0 || a1["lastSeen"] == nil || a1["started"] == nil {
		t.Fatalf("a1 = %v", a1)
	}
	if doc.Sessions[3]["sid"] != sidB1[:8] || doc.Sessions[3]["account"] != "B" {
		t.Fatalf("b1 = %v", doc.Sessions[3])
	}
	pending := doc.Sessions[4] // identified, never inferred
	if pending["sid"] != sidC1[:8] || pending["account"] != nil || pending["requests"] != nil {
		t.Fatalf("pending = %v", pending)
	}
	if strings.Contains(out, sidA1) {
		t.Fatalf("a full sid reached the output: %s", out)
	}
}

func TestStatusJSONHasNoSessionsKeyWhenNoneLive(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "A", "B")
	// The cache remembers a session no registry entry names.
	writeSessionsCache(t, home, act(sidA1, "A", 5, 1))
	_, out, _ := runHome(t, home, "status", "--json")
	if strings.Contains(out, `"sessions"`) || strings.Contains(out, sidA1) {
		t.Fatalf("a dead session was reported: %s", out)
	}
	_, text, _ := runHome(t, home, "status")
	if strings.Contains(text, "live sessions") {
		t.Fatalf("text = %q", text)
	}
}

// --- statusline ---

func sessionLineEnv(t *testing.T, chain map[int]int) (home string, port int) {
	t.Helper()
	home, port = statuslineEnv(t, true, chain)
	writeSessionsCacheWithAccounts(t, home, act(sidA1, "other", 3, 1))
	return home, port
}

func writeSessionsCacheWithAccounts(t *testing.T, home string, acts ...sessions.Activity) {
	t.Helper()
	b, err := status.Marshal(status.File{Accounts: []status.Account{{Name: "work"}, {Name: "other"}}, Sessions: acts})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(home), b); err != nil {
		t.Fatal(err)
	}
}

func TestStatuslineAccountFromProxyEnvSessionAndNoPasswordLeak(t *testing.T) {
	home, port := sessionLineEnv(t, nil)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag.default.%s:pw-s3cr3t-9f@127.0.0.1:%d", sidA1, port))
	_, out, _ := runHome(t, home, "statusline")
	if out != "c» other · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("line = %q", out)
	}
	_, jout, _ := runHome(t, home, "statusline", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jout), &got); err != nil {
		t.Fatal(err)
	}
	if got["account"] != "other" || got["serving"] != "work" {
		t.Fatalf("json = %s", jout)
	}
	for _, o := range []string{out, jout} {
		if strings.Contains(o, "s3cr3t") || strings.Contains(o, sidA1) {
			t.Fatalf("password or sid leaked: %q", o)
		}
	}
}

func TestStatuslineAccountFromRegistryEntryOfTheAncestor(t *testing.T) {
	home, port := sessionLineEnv(t, map[int]int{os.Getppid(): os.Getpid()})
	seedEntry(t, home, "9", session.Session{Port: port, SID: sidA1, Pool: "default", Started: t0})
	_, out, _ := runHome(t, home, "statusline")
	if out != "c» other · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("line = %q", out)
	}
}

func TestStatuslineFallsBackToServing(t *testing.T) {
	// A session whose first inference has not happened yet (Review Focus 5),
	// an unidentified registry entry, and a legacy credential all show serving.
	home, port := sessionLineEnv(t, nil)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag.default.%s:x@127.0.0.1:%d", sidC1, port))
	_, out, _ := runHome(t, home, "statusline")
	if out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("no first inference: %q", out)
	}
	_, jout, _ := runHome(t, home, "statusline", "--json")
	if !strings.Contains(jout, `"account": "work"`) && !strings.Contains(jout, `"account":"work"`) {
		t.Fatalf("json = %s", jout)
	}

	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag:legacy-pw@127.0.0.1:%d", port))
	_, out, _ = runHome(t, home, "statusline")
	if out != "c» work · 5h – · 7d – · 2/2 ok\n" || strings.Contains(out, "legacy-pw") {
		t.Fatalf("legacy: %q", out)
	}

	home2, port2 := sessionLineEnv(t, map[int]int{os.Getppid(): os.Getpid()})
	t.Setenv("HTTPS_PROXY", "")
	registerLive(t, home2, os.Getpid(), port2)
	_, out, _ = runHome(t, home2, "statusline")
	if out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("unidentified entry: %q", out)
	}
}

func TestStatuslineMalformedProxyUserIsNotASession(t *testing.T) {
	home, port := sessionLineEnv(t, nil)
	for _, user := range []string{
		"chottag.default." + strings.ToUpper(sidA1),
		"chottag.default." + sidA1[:31],
		"chottag.Default." + sidA1,
		"chottag.default.extra." + sidA1,
	} {
		t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://%s:pw@127.0.0.1:%d", user, port))
		_, out, _ := runHome(t, home, "statusline")
		if strings.Contains(out, "other") {
			t.Fatalf("user %q matched a session: %q", user, out)
		}
	}
}

func TestStatuslineLegacyProxyMakesNoAncestorWalk(t *testing.T) {
	home, port := sessionLineEnv(t, nil)
	calls := 0
	t.Cleanup(cli.SetParentPIDForTest(func(int) (int, error) { calls++; return 0, fmt.Errorf("no") }))
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag:legacy-pw@127.0.0.1:%d", port))
	if _, out, _ := runHome(t, home, "statusline"); !strings.Contains(out, "work") {
		t.Fatalf("line = %q", out)
	}
	if calls != 0 {
		t.Fatalf("parentPID called %d times for a routed legacy credential", calls)
	}
}
