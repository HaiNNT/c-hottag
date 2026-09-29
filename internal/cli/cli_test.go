package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/cli"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run("chottag", args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionAndUsage(t *testing.T) {
	if code, out, _ := run(t, "version"); code != 0 || !strings.HasPrefix(out, "chottag ") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, errs := run(t, "nope"); code != 2 || !strings.Contains(errs, "usage") {
		t.Fatalf("unknown: %d %q", code, errs)
	}
}

func TestTraceEnvCreatesCA(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	code, out, _ := run(t, "trace", "env", "--listen", "127.0.0.1:50000")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	want := "export HTTPS_PROXY=\"http://chottag:$(cat \"" + filepath.Join(home, "ca", "proxy.secret") + "\")@127.0.0.1:50000\"\nexport NODE_EXTRA_CA_CERTS=\"" + filepath.Join(home, "ca", "ca.pem") + "\"\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if _, err := os.Stat(filepath.Join(home, "ca", "proxy.secret")); err != nil {
		t.Fatalf("trace env did not create the proxy secret: %v", err)
	}
}

// TestTraceEnvRejectsAListenWhosePortIsNotNumeric pins fix round 1 item 4:
// trace env never binds --listen, so unlike trace run (which would already
// fail at net.Listen) it must validate the address itself before ever
// embedding it, raw, in the printed HTTPS_PROXY line. net.SplitHostPort
// alone accepts this value (it only requires one unescaped colon, not a
// numeric port), so a caller that trusted that verdict would print
// HTTPS_PROXY with a shell metacharacter payload straight in it. The
// refusal gets its own "port must be 0-65535" message (fix round 2 item
// C), not the generic "must be loopback" text this test originally
// expected — a bad port and a bad host are different problems.
func TestTraceEnvRejectsAListenWhosePortIsNotNumeric(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	code, out, errs := run(t, "trace", "env", "--listen", `127.0.0.1:1"; touch x; "`)
	if code != 2 {
		t.Fatalf("code = %d, want 2 (usage)", code)
	}
	if out != "" {
		t.Fatalf("stdout = %q, want no export lines printed", out)
	}
	if !strings.Contains(errs, "port must be 0-65535") {
		t.Fatalf("stderr = %q, want it to explain the bad port", errs)
	}
}

// TestTraceEnvRejectsAnEmptyOrNamedListenPort pins the controller ruling in
// fix round 2 item C: neither no port at all nor a named service (which
// net.Dial would happily resolve) is accepted — only decimal digits
// 0-65535 are.
func TestTraceEnvRejectsAnEmptyOrNamedListenPort(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:", "127.0.0.1:http"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("CHOTTAG_HOME", t.TempDir())
			code, out, errs := run(t, "trace", "env", "--listen", addr)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (usage)", code)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want no export lines printed", out)
			}
			if !strings.Contains(errs, "port must be 0-65535") {
				t.Fatalf("stderr = %q, want it to explain the bad port", errs)
			}
		})
	}
}

func TestTraceMarkAndSummarize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	if code, _, errs := run(t, "trace", "mark", "open", "usage"); code != 0 {
		t.Fatalf("mark: %d %s", code, errs)
	}
	recs, err := tracelog.ReadAll(filepath.Join(home, "trace.jsonl"))
	if err != nil || len(recs) != 1 || recs[0].Mark != "open usage" {
		t.Fatalf("recs %+v err %v", recs, err)
	}
	code, out, _ := run(t, "trace", "summarize")
	if code != 0 || !strings.Contains(out, "open usage") {
		t.Fatalf("summarize: %d %q", code, out)
	}
}

func TestTraceRunRejectsBadSwap(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	if code, _, errs := run(t, "trace", "run", "--swap", "untouched=/tmp/x"); code != 2 || !strings.Contains(errs, "serving|remote") {
		t.Fatalf("bad swap: %d %q", code, errs)
	}
}

// TestTraceRunRejectsNonLoopbackListen guards against ever binding the proxy
// (which forwards a bearer, and optionally a swapped one) to a non-loopback
// address, which would let any other host on the network reach it.
func TestTraceRunRejectsNonLoopbackListen(t *testing.T) {
	t.Setenv("CHOTTAG_HOME", t.TempDir())
	if code, _, errs := run(t, "trace", "run", "--listen", "0.0.0.0:47821"); code != 2 || !strings.Contains(errs, "loopback") {
		t.Fatalf("non-loopback listen: %d %q", code, errs)
	}
}

func TestLsIsAnAliasForStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	var lsOut, lsErr, stOut, stErr bytes.Buffer
	lsCode := cli.Run("chottag", []string{"ls"}, &lsOut, &lsErr)
	stCode := cli.Run("chottag", []string{"status"}, &stOut, &stErr)
	if lsCode != stCode || lsOut.String() != stOut.String() {
		t.Errorf("ls = (%d, %q), status = (%d, %q): ls must be an alias",
			lsCode, lsOut.String(), stCode, stOut.String())
	}
}
