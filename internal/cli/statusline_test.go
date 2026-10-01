package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/session"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// statuslineEnv seeds a home serving "work", clears HTTPS_PROXY, and stubs
// the probe (up or not) and the ppid seam (a chain, pid -> parent).
func statuslineEnv(t *testing.T, up bool, chain map[int]int) (home string, port int) {
	t.Helper()
	home = t.TempDir()
	seedState(t, home, "work", "other")
	st, err := store.Store{Dir: home}.Load()
	if err != nil {
		t.Fatal(err)
	}
	port = st.ResolvedPort()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CMUX_WORKSPACE_ID", "")
	t.Cleanup(cli.SetStatuslineProbeForTest(func(int) bool { return up }))
	t.Cleanup(cli.SetParentPIDForTest(func(pid int) (int, error) {
		p, ok := chain[pid]
		if !ok {
			return 0, fmt.Errorf("no parent for %d", pid)
		}
		return p, nil
	}))
	return home, port
}

func registerLive(t *testing.T, home string, pid, port int) {
	t.Helper()
	reg, err := session.Open(filepath.Join(home, "run"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(pid, port); err != nil {
		t.Fatal(err)
	}
}

func TestStatuslineRoutedViaAncestorDaemonUp(t *testing.T) {
	home, port := statuslineEnv(t, true, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port) // the test's own pid is alive
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineRoutedDaemonDown(t *testing.T) {
	home, port := statuslineEnv(t, false, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port)
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» down\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineRoutedViaProxyEnvOnly(t *testing.T) {
	home, port := statuslineEnv(t, true, nil)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag:s3cr3t-value@127.0.0.1:%d", port))
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("got %d %q", code, out)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("secret leaked: %q", out)
	}
	_, jout, _ := runHome(t, home, "statusline", "--json")
	if strings.Contains(jout, "s3cr3t") {
		t.Fatalf("secret leaked in json: %q", jout)
	}
}

func TestStatuslineProxyEnvForAnotherPortIsOff(t *testing.T) {
	home, port := statuslineEnv(t, true, nil)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag:x@127.0.0.1:%d", port+1))
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» off\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineNoHomeIsOff(t *testing.T) {
	statuslineEnv(t, true, nil)
	empty := t.TempDir()
	t.Setenv("HTTPS_PROXY", "http://chottag:x@127.0.0.1:47821")
	code, out, _ := runHome(t, empty, "statusline")
	if code != 0 || out != "c» off\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineSeamErrorIsOff(t *testing.T) {
	home, port := statuslineEnv(t, true, map[int]int{}) // every lookup errors
	registerLive(t, home, os.Getpid(), port)
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» off\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineStopsAfterEightLevels(t *testing.T) {
	// Levels count from 1 at the parent: 900001..900008 are levels 2..9 and
	// the live pid is level 10, past the limit of 8.
	chain := map[int]int{}
	prev := os.Getppid()
	for i := 1; i <= 8; i++ {
		chain[prev] = 900000 + i
		prev = 900000 + i
	}
	chain[prev] = os.Getpid()
	home, port := statuslineEnv(t, true, chain)
	registerLive(t, home, os.Getpid(), port)
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» off\n" {
		t.Fatalf("level 10 must not count: %d %q", code, out)
	}
	// Now the live pid is level 8 (after 900006 = level 7), the last checked.
	chain[900006] = os.Getpid()
	code, out, _ = runHome(t, home, "statusline")
	if code != 0 || out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("level 8 must count: %d %q", code, out)
	}
}

func TestStatuslineJSONShapes(t *testing.T) {
	home, port := statuslineEnv(t, true, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port)
	_, out, _ := runHome(t, home, "statusline", "--json")
	want := `{"version":1,"ok":true,"warnings":[],"session":"routed","daemon":"up","serving":"work","okAccounts":2,"rotationAccounts":2}`
	if compact(t, out) != want {
		t.Fatalf("routed json = %s", out)
	}

	home2, _ := statuslineEnv(t, true, nil)
	_, out, _ = runHome(t, home2, "statusline", "--json")
	want = `{"version":1,"ok":true,"warnings":[],"session":"home","daemon":"unknown","serving":""}`
	if compact(t, out) != want {
		t.Fatalf("home json = %s", out)
	}

	home3, port3 := statuslineEnv(t, false, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home3, os.Getpid(), port3)
	_, out, _ = runHome(t, home3, "statusline", "--json")
	want = `{"version":1,"ok":true,"warnings":[],"session":"routed","daemon":"down","serving":""}`
	if compact(t, out) != want {
		t.Fatalf("down json = %s", out)
	}
}

func compact(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return b.String()
}

func TestStatusLiveSessionsCountsRegisteredPids(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "work", "other")
	_, out, _ := runHome(t, home, "status", "--json")
	var none struct {
		Daemon *struct {
			LiveSessions int `json:"liveSessions"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(out), &none); err != nil {
		t.Fatal(err)
	}
	if none.Daemon != nil && none.Daemon.LiveSessions != 0 {
		t.Fatalf("want 0 live sessions, got %+v", none.Daemon)
	}

	registerLive(t, home, os.Getpid(), 47821)
	_, out, _ = runHome(t, home, "status")
	if !strings.Contains(out, "live sessions: 1\n") {
		t.Fatalf("text = %q", out)
	}
	_, out, _ = runHome(t, home, "status", "--json")
	var got struct {
		Daemon struct {
			LiveSessions int `json:"liveSessions"`
		} `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Daemon.LiveSessions != 1 {
		t.Fatalf("daemon.liveSessions = %d\n%s", got.Daemon.LiveSessions, out)
	}
}

func TestStatusLiveSessionsZeroWithDaemonRunning(t *testing.T) {
	home := t.TempDir()
	seedState(t, home, "work", "other")
	runningDaemon(t, home)
	_, out, _ := runHome(t, home, "status", "--json")
	if !strings.Contains(out, `"liveSessions": 0`) {
		t.Fatalf("json = %s", out)
	}
}

func TestStatuslineCycleInChainIsOff(t *testing.T) {
	ppid := os.Getppid()
	home, port := statuslineEnv(t, true, map[int]int{ppid: ppid})
	registerLive(t, home, os.Getpid(), port)
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» off\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineProxyEnvWrongHostOrUserIsOffAndUnprobed(t *testing.T) {
	for _, v := range []string{"http://chottag:x@10.0.0.5:%d", "http://alice:x@127.0.0.1:%d"} {
		home, port := statuslineEnv(t, true, nil)
		t.Cleanup(cli.SetStatuslineProbeForTest(func(int) bool {
			t.Fatal("probed although the session is not routed")
			return false
		}))
		t.Setenv("HTTPS_PROXY", fmt.Sprintf(v, port))
		code, out, _ := runHome(t, home, "statusline")
		if code != 0 || out != "c» off\n" {
			t.Fatalf("%s: got %d %q", v, code, out)
		}
	}
}

// v2Env is a routed, daemon-up home whose clock is fixed at 2026-10-01 12:00
// UTC, with a status cache written by writeCache.
func v2Env(t *testing.T) (home string, now time.Time) {
	t.Helper()
	home, port := statuslineEnv(t, true, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port)
	now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	t.Cleanup(cli.SetStatuslineNowForTest(func() time.Time { return now }))
	return home, now
}

func pct(v float64) *float64 { return &v }

func writeCache(t *testing.T, home string, accts ...status.Account) {
	t.Helper()
	b, err := status.Marshal(status.File{Accounts: accts})
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

func usageOf(now time.Time, five, seven float64, r5, r7 time.Time) *status.Usage {
	return &status.Usage{FiveHourPct: pct(five), SevenDayPct: pct(seven), FiveHourResetsAt: r5, SevenDayResetsAt: r7, UpdatedAt: now, Source: "observed"}
}

func TestStatuslineV2FullLine(t *testing.T) {
	home, now := v2Env(t)
	writeCache(t, home,
		status.Account{Name: "work", Usage: usageOf(now, 41.6, 18.2, now.Add(7*time.Hour), now.Add(50*time.Hour))},
		status.Account{Name: "other", Limited: true, LimitedUntil: now.Add(time.Hour)})
	_, out, _ := runHome(t, home, "statusline")
	if want := "c» work · 5h 42% · 7d 18% · ↻ 19:00 · 1/2 ok\n"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	_, jout, _ := runHome(t, home, "statusline", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(jout), &got); err != nil {
		t.Fatal(err)
	}
	if got["fiveHourPct"] != 41.6 || got["sevenDayPct"] != 18.2 || got["okAccounts"] != 1.0 || got["rotationAccounts"] != 2.0 ||
		got["resetsAt"] != "2026-10-01T19:00:00Z" {
		t.Fatalf("json = %s", jout)
	}
}

func TestStatuslineResetRule(t *testing.T) {
	cases := []struct {
		name   string
		five   float64
		r5, r7 time.Duration
		want   string
	}{
		{"low 5h takes the earlier", 10, 3 * time.Hour, 2 * time.Hour, "↻ 14:00"},
		{"at 80 takes the 5h reset", 80, 5 * time.Hour, 2 * time.Hour, "↻ 17:00"},
		{"above 24h prints the weekday", 10, 30 * time.Hour, 60 * time.Hour, "↻ Fri 18:00"},
		{"only 7d known", 10, 0, 3 * time.Hour, "↻ 15:00"},
		{"none known", 10, 0, 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, now := v2Env(t)
			var r5, r7 time.Time
			if c.r5 != 0 {
				r5 = now.Add(c.r5)
			}
			if c.r7 != 0 {
				r7 = now.Add(c.r7)
			}
			writeCache(t, home, status.Account{Name: "work", Usage: usageOf(now, c.five, 5, r5, r7)})
			_, out, _ := runHome(t, home, "statusline")
			if c.want == "" {
				if strings.Contains(out, "↻") {
					t.Fatalf("got %q", out)
				}
				return
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("got %q want %q", out, c.want)
			}
		})
	}
}

func TestStatuslineNoRotationOmitsOK(t *testing.T) {
	home, _ := v2Env(t)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for i := range st.Accounts {
			st.Accounts[i].NoRotate = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runHome(t, home, "statusline")
	if strings.Contains(out, "ok") {
		t.Fatalf("got %q", out)
	}
}

func TestStatuslineLabelStates(t *testing.T) {
	home, _ := v2Env(t)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error { st.Label = "dev"; return nil }); err != nil {
		t.Fatal(err)
	}
	_, out, _ := runHome(t, home, "statusline")
	if want := "c» dev · work · 5h – · 7d – · 2/2 ok\n"; out != want {
		t.Fatalf("got %q", out)
	}
	t.Cleanup(cli.SetStatuslineProbeForTest(func(int) bool { return false }))
	if _, out, _ = runHome(t, home, "statusline"); out != "c» dev down\n" {
		t.Fatalf("got %q", out)
	}
	t.Setenv("HTTPS_PROXY", "")
	t.Cleanup(cli.SetParentPIDForTest(func(int) (int, error) { return 0, fmt.Errorf("x") }))
	if _, out, _ = runHome(t, home, "statusline"); out != "c» dev off\n" {
		t.Fatalf("got %q", out)
	}
}

func TestStatuslineUpWithoutServing(t *testing.T) {
	home, _ := v2Env(t)
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error { st.Serving = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := runHome(t, home, "statusline"); out != "c» up\n" {
		t.Fatalf("got %q", out)
	}
}

func TestStatuslineColour(t *testing.T) {
	home, now := v2Env(t)
	writeCache(t, home, status.Account{Name: "work", Limited: true, Usage: usageOf(now, 100, 20, now.Add(time.Hour), time.Time{})})
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", "")
	_, out, _ := runHome(t, home, "statusline")
	if !strings.HasPrefix(out, "\x1b[1;38;5;33mc»\x1b[0m work") {
		t.Fatalf("mark not painted blue: %q", out)
	}
	if !strings.Contains(out, "\x1b[1;38;5;160m5h 100%\x1b[0m · \x1b[1;38;5;160m7d 20%\x1b[0m") {
		t.Fatalf("limited fields not red: %q", out)
	}
	if strings.Contains(out, "\x1b[0m · ↻ \x1b") || strings.Contains(out, "ok\x1b") {
		t.Fatalf("more than the mark and limited fields painted: %q", out)
	}
	t.Setenv("NO_COLOR", "1")
	if _, out, _ = runHome(t, home, "statusline"); strings.Contains(out, "\x1b") {
		t.Fatalf("NO_COLOR ignored: %q", out)
	}
}

type pillCall struct{ ws, value, color string }

func stubCmux(t *testing.T, err error) *[]pillCall {
	t.Helper()
	var calls []pillCall
	t.Cleanup(cli.SetCmuxSetStatusForTest(func(_ context.Context, ws, v, c string) error {
		calls = append(calls, pillCall{ws, v, c})
		return err
	}))
	return &calls
}

func TestStatuslinePillThrottle(t *testing.T) {
	home, now := v2Env(t)
	calls := stubCmux(t, nil)
	t.Setenv("CMUX_WORKSPACE_ID", "ws:1/a b")
	t.Setenv("NO_COLOR", "")
	writeCache(t, home, status.Account{Name: "work", Usage: usageOf(now, 10, 5, time.Time{}, time.Time{})})
	runHome(t, home, "statusline", "--cmux")
	runHome(t, home, "statusline", "--cmux")
	if len(*calls) != 1 {
		t.Fatalf("calls = %v", *calls)
	}
	c := (*calls)[0]
	if c.ws != "ws:1/a b" || c.value != "c» work · 5h 10% · 7d 5% · 2/2 ok" || c.color != "#005FAF" {
		t.Fatalf("call = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(home, "run", "pill-ws_1_a_b.txt")); err != nil {
		t.Fatal(err)
	}
	writeCache(t, home, status.Account{Name: "work", Usage: usageOf(now, 11, 5, time.Time{}, time.Time{})})
	runHome(t, home, "statusline", "--cmux")
	if len(*calls) != 2 {
		t.Fatalf("a changed line must send again: %v", *calls)
	}
}

func TestStatuslinePillLabelColourAndNoEnv(t *testing.T) {
	home, _ := v2Env(t)
	calls := stubCmux(t, nil)
	runHome(t, home, "statusline", "--cmux")
	if len(*calls) != 0 {
		t.Fatalf("no pill without CMUX_WORKSPACE_ID: %v", *calls)
	}
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error { st.Label = "dev"; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CMUX_WORKSPACE_ID", "w")
	runHome(t, home, "statusline", "--cmux")
	if len(*calls) != 1 || (*calls)[0].color != "#FFAF00" {
		t.Fatalf("calls = %v", *calls)
	}
}

func TestStatuslinePillErrorIgnoredAndNotCached(t *testing.T) {
	home, _ := v2Env(t)
	calls := stubCmux(t, fmt.Errorf("cmux gone"))
	t.Setenv("CMUX_WORKSPACE_ID", "w")
	code, out, _ := runHome(t, home, "statusline", "--cmux")
	if code != 0 || !strings.HasPrefix(out, "c» work") {
		t.Fatalf("got %d %q", code, out)
	}
	runHome(t, home, "statusline", "--cmux")
	if len(*calls) != 2 {
		t.Fatalf("a failed send must not be cached: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(home, "run", "pill-w.txt")); err == nil {
		t.Fatal("throttle file written after a failed send")
	}
}

func TestStatuslineCorruptCacheStillPrints(t *testing.T) {
	home, _ := v2Env(t)
	if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(status.Path(home), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "c» work · 5h – · 7d – · 2/2 ok\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineStaleUsagePrintsDash(t *testing.T) {
	home, now := v2Env(t)
	writeCache(t, home, status.Account{Name: "work", Usage: usageOf(now.Add(-status.StaleAfter-time.Minute), 50, 50, now.Add(time.Hour), time.Time{})})
	_, out, _ := runHome(t, home, "statusline")
	if want := "c» work · 5h – · 7d – · 2/2 ok\n"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestStatuslinePastFiveHourResetTakesSevenDay(t *testing.T) {
	home, now := v2Env(t)
	writeCache(t, home, status.Account{Name: "work", Usage: usageOf(now, 90, 5, now.Add(-time.Hour), now.Add(3*time.Hour))})
	_, out, _ := runHome(t, home, "statusline")
	if !strings.Contains(out, "↻ 15:00") {
		t.Fatalf("got %q", out)
	}
}

func TestStatuslineCacheNameCaseInsensitive(t *testing.T) {
	home, now := v2Env(t)
	writeCache(t, home, status.Account{Name: "WORK", Usage: usageOf(now, 10, 5, time.Time{}, time.Time{})})
	_, out, _ := runHome(t, home, "statusline")
	if !strings.Contains(out, "5h 10%") {
		t.Fatalf("got %q", out)
	}
}

func TestStatuslinePillWorkspaceCannotEscapeRun(t *testing.T) {
	home, _ := v2Env(t)
	stubCmux(t, nil)
	t.Setenv("CMUX_WORKSPACE_ID", "../../evil")
	runHome(t, home, "statusline", "--cmux")
	if _, err := os.Stat(filepath.Join(home, "run", "pill-______evil.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestStatuslineBareHasNoSideEffectInCmux(t *testing.T) {
	home, _ := v2Env(t)
	calls := stubCmux(t, nil)
	t.Setenv("CMUX_WORKSPACE_ID", "ws1")
	for _, args := range [][]string{{"statusline"}, {"statusline", "--json"}} {
		if code, _, _ := runHome(t, home, args...); code != 0 {
			t.Fatalf("%v exit %d", args, code)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a bare statusline called cmux: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(home, "run")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(home, "run"))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "pill-") {
				t.Fatalf("throttle file written: %s", e.Name())
			}
		}
	}
}

func TestStatuslineCmuxWithJSON(t *testing.T) {
	home, _ := v2Env(t)
	calls := stubCmux(t, nil)
	t.Setenv("CMUX_WORKSPACE_ID", "ws1")
	code, out, _ := runHome(t, home, "statusline", "--cmux", "--json")
	if code != 0 || !strings.Contains(out, `"serving": "work"`) && !strings.Contains(out, `"serving":"work"`) {
		t.Fatalf("got %d %q", code, out)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %v", *calls)
	}
}
