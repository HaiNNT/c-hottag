package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/session"
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
	if code != 0 || out != "chottag: work\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineRoutedDaemonDown(t *testing.T) {
	home, port := statuslineEnv(t, false, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port)
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "chottag: down\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineRoutedViaProxyEnvOnly(t *testing.T) {
	home, port := statuslineEnv(t, true, nil)
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag:s3cr3t-value@127.0.0.1:%d", port))
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "chottag: work\n" {
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
	if code != 0 || out != "chottag: off\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineNoHomeIsOff(t *testing.T) {
	statuslineEnv(t, true, nil)
	empty := t.TempDir()
	t.Setenv("HTTPS_PROXY", "http://chottag:x@127.0.0.1:47821")
	code, out, _ := runHome(t, empty, "statusline")
	if code != 0 || out != "chottag: off\n" {
		t.Fatalf("got %d %q", code, out)
	}
}

func TestStatuslineSeamErrorIsOff(t *testing.T) {
	home, port := statuslineEnv(t, true, map[int]int{}) // every lookup errors
	registerLive(t, home, os.Getpid(), port)
	code, out, _ := runHome(t, home, "statusline")
	if code != 0 || out != "chottag: off\n" {
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
	if code != 0 || out != "chottag: off\n" {
		t.Fatalf("level 10 must not count: %d %q", code, out)
	}
	// Now the live pid is level 8 (after 900006 = level 7), the last checked.
	chain[900006] = os.Getpid()
	code, out, _ = runHome(t, home, "statusline")
	if code != 0 || out != "chottag: work\n" {
		t.Fatalf("level 8 must count: %d %q", code, out)
	}
}

func TestStatuslineJSONShapes(t *testing.T) {
	home, port := statuslineEnv(t, true, map[int]int{os.Getppid(): os.Getpid()})
	registerLive(t, home, os.Getpid(), port)
	_, out, _ := runHome(t, home, "statusline", "--json")
	want := `{"version":1,"ok":true,"warnings":[],"session":"routed","daemon":"up","serving":"work"}`
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
	if code != 0 || out != "chottag: off\n" {
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
		if code != 0 || out != "chottag: off\n" {
			t.Fatalf("%s: got %d %q", v, code, out)
		}
	}
}
