package shim

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/proxyauth"
	"github.com/HaiNNT/c-hottag/internal/store"
)

func writeStateWithPool(t *testing.T, home string, port int, pool string) {
	t.Helper()
	if _, err := (store.Store{Dir: home}).Update(func(s *store.State) error {
		s.Port = port
		if pool != "" {
			s.Pools = map[string]store.Pool{pool: {}}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func poolEnv(t *testing.T, kv ...string) []string {
	return append([]string{"PATH=" + filepath.Dir(fakeClaude(t))}, kv...)
}

func TestRunPoolUnsetAndEmptyMeanDefault(t *testing.T) {
	for _, env := range [][]string{nil, {"CHOTTAG_POOL="}} {
		home := t.TempDir()
		s, _ := proxyauth.LoadOrCreate(home)
		srv := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: "t", PID: os.Getpid()})
		port := mustPort(t, srv.URL)
		writeStateWithPort(t, home, port)
		var got execCall
		restore := swapExec(home, &got)
		if code := Run(nil, home, poolEnv(t, env...), ownVersion, io.Discard, io.Discard); code != 0 {
			t.Fatalf("Run = %d", code)
		}
		restore()
		sessionSID(t, s, got.env, port)
		if len(got.live) != 1 || got.live[0].Pool != store.DefaultPool {
			t.Fatalf("registry = %+v", got.live)
		}
	}
}

func TestRunPoolWorkMintsAWorkCredentialAndRegistersIt(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	srv := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: "0.8.0", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPool(t, home, port, "work")
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	if code := Run(nil, home, poolEnv(t, "CHOTTAG_POOL=work"), ownVersion, io.Discard, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := s.SessionProxyURL("127.0.0.1:"+strconv.Itoa(port), "work", got.live[0].SID)
	if envGet(got.env, "HTTPS_PROXY") != want {
		t.Fatal("HTTPS_PROXY is not the chottag.work.<sid> session URL")
	}
	if got.live[0].Pool != "work" {
		t.Fatalf("registry pool = %q", got.live[0].Pool)
	}
}

func TestRunUnknownOrInvalidPoolRefusesAndNeverExecs(t *testing.T) {
	for _, name := range []string{"nope", "Work", "a b", "default2x-way-too-long-name"} {
		home := t.TempDir()
		s, _ := proxyauth.LoadOrCreate(home)
		srv := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: "t", PID: os.Getpid()})
		writeStateWithPool(t, home, mustPort(t, srv.URL), "work")
		var got execCall
		restore := swapExec(home, &got)
		var errb bytes.Buffer
		code := Run(nil, home, poolEnv(t, "CHOTTAG_POOL="+name), ownVersion, io.Discard, &errb)
		restore()
		if code != 2 {
			t.Fatalf("%q: exit %d, want 2", name, code)
		}
		if got.bin != "" {
			t.Fatalf("%q: claude was exec'd", name)
		}
		if want := "chottag: no pool named \"" + name + "\" (chottag pool lists them)\n"; errb.String() != want {
			t.Fatalf("%q: stderr = %q", name, errb.String())
		}
	}
}

func TestRunLegacyPathStillValidatesThePool(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	srv := preSessionHealthServer(t, s, proxy.Health{Chottag: true, Version: "0.5.1", PID: os.Getpid()})
	port := mustPort(t, srv.URL)
	writeStateWithPool(t, home, port, "work")
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	if code := Run(nil, home, poolEnv(t, "CHOTTAG_POOL=nope"), ownVersion, io.Discard, &errb); code != 2 || got.bin != "" {
		t.Fatalf("typo on legacy path: exit %d, exec %q", code, got.bin)
	}
}

// A daemon that predates pools cannot read a version 2 state.json, so a
// session in a non-default pool would go out on Home's own login: the shim
// refuses it, exit 2, and never starts claude.
func TestRunNonDefaultPoolRefusedByADaemonThatPredatesPools(t *testing.T) {
	cases := []struct {
		name, version string
		legacy        bool // a pre-session daemon (before 0.6.0)
	}{
		{"0.5.1 (no sessions)", "0.5.1", true},
		{"0.6.0", "0.6.0", false},
		{"0.7.1", "0.7.1", false},
		{"no version", "", false},
		{"unparseable", "weird", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			s, _ := proxyauth.LoadOrCreate(home)
			h := proxy.Health{Chottag: true, Version: c.version, PID: os.Getpid()}
			srv := provingHealthServer(t, s, h)
			if c.legacy {
				srv = preSessionHealthServer(t, s, h)
			}
			writeStateWithPool(t, home, mustPort(t, srv.URL), "work")
			var got execCall
			defer swapExec(home, &got)()
			var errb bytes.Buffer
			code := Run(nil, home, poolEnv(t, "CHOTTAG_POOL=work"), ownVersion, io.Discard, &errb)
			if code != 2 || got.bin != "" {
				t.Fatalf("exit %d, exec %q, stderr %q", code, got.bin, errb.String())
			}
			shown := c.version
			if shown == "" {
				shown = "unknown"
			}
			want := "chottag: the running daemon (" + shown + ") predates pools, so a \"work\" session would not stay in its pool; run: chottag daemon restart\n"
			if errb.String() != want {
				t.Fatalf("stderr = %q, want %q", errb.String(), want)
			}
		})
	}
}

// Once a pool exists, state.json is version 2 and an older daemon cannot read
// it: it would send every session, default included, on Home's own login, so
// the shim refuses them all. With no extra pool nothing changes. A daemon that
// reads pools (0.8.0 or later, or this very build) serves every session.
func TestRunOlderDaemonAndVersionTwoStateRefuseEverySession(t *testing.T) {
	for _, tc := range []struct {
		version, env string
		pools        bool
		wantExit     int
	}{
		{"0.7.1", "CHOTTAG_POOL=default", true, 2},
		{"0.7.1", "CHOTTAG_POOL=", true, 2},
		{"0.7.1", "", true, 2},
		{"", "CHOTTAG_POOL=", true, 2},
		{"0.7.1", "CHOTTAG_POOL=", false, 0}, // no extra pool: today's behaviour
		{"0.7.1", "CHOTTAG_POOL=default", false, 0},
		{"0.8.0", "CHOTTAG_POOL=work", true, 0},
		{"0.9.0", "CHOTTAG_POOL=", true, 0},
		{ownVersion, "CHOTTAG_POOL=work", true, 0},
	} {
		home := t.TempDir()
		s, _ := proxyauth.LoadOrCreate(home)
		srv := provingHealthServer(t, s, proxy.Health{Chottag: true, Version: tc.version, PID: os.Getpid()})
		pool := ""
		if tc.pools {
			pool = "work"
		}
		writeStateWithPool(t, home, mustPort(t, srv.URL), pool)
		var got execCall
		restore := swapExec(home, &got)
		var errb bytes.Buffer
		code := Run(nil, home, poolEnv(t, tc.env), ownVersion, io.Discard, &errb)
		restore()
		if code != tc.wantExit || (tc.wantExit == 2) != (got.bin == "") || strings.Contains(errb.String(), "predates") != (tc.wantExit == 2) {
			t.Errorf("%s on %q (pools %v): exit %d, exec %q, stderr %q", tc.env, tc.version, tc.pools, code, got.bin, errb.String())
		}
		if tc.wantExit == 2 && !strings.Contains(errb.String(), "chottag daemon restart") {
			t.Errorf("no restart advice: %q", errb.String())
		}
	}
}

func TestRunLegacyPathDefaultPoolPrintsNoPoolsMessage(t *testing.T) {
	home := t.TempDir()
	s, _ := proxyauth.LoadOrCreate(home)
	srv := preSessionHealthServer(t, s, proxy.Health{Chottag: true, Version: "0.5.1", PID: os.Getpid()})
	writeStateWithPort(t, home, mustPort(t, srv.URL))
	var got execCall
	defer swapExec(home, &got)()
	var errb bytes.Buffer
	if code := Run(nil, home, poolEnv(t, "CHOTTAG_POOL=default"), ownVersion, io.Discard, &errb); code != 0 || strings.Contains(errb.String(), "predates") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}
