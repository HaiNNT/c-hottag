package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/store"
)

// shimFirstPATH lays out the post-`chottag setup` PATH (issue #2, R145):
// <home>/bin/claude, a fake chottag shim that reports another account's
// identity, comes first, and a fake real claude reporting realEmail comes
// later. It returns the home and the real claude's path.
func shimFirstPATH(t *testing.T, realEmail string) (home, realClaude string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' '{\"loggedIn\":true,\"email\":\"serving@example.com\"}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	realClaude = writeFakeClaude(t, `{"loggedIn":true,"email":"`+realEmail+`"}`)
	t.Setenv("PATH", strings.Join([]string{binDir, filepath.Dir(realClaude)}, string(os.PathListSeparator)))
	return home, realClaude
}

func TestRealClaudeBinSkipsChottagsOwnBin(t *testing.T) {
	home, real := shimFirstPATH(t, "a@example.com")
	got, err := realClaudeBin("", home)
	if err != nil || got != real {
		t.Fatalf("realClaudeBin(\"\") = %q, %v; want %q", got, err, real)
	}
}

func TestRealClaudeBinHonoursAnExplicitPath(t *testing.T) {
	home, _ := shimFirstPATH(t, "a@example.com")
	got, err := realClaudeBin("/explicit/claude", home)
	if err != nil || got != "/explicit/claude" {
		t.Fatalf("realClaudeBin(explicit) = %q, %v", got, err)
	}
}

func TestRealClaudeBinFailsWhenOnlyTheShimExists(t *testing.T) {
	home, _ := shimFirstPATH(t, "a@example.com")
	t.Setenv("PATH", strings.Join([]string{filepath.Join(home, "bin")}, string(os.PathListSeparator)))
	if got, err := realClaudeBin("", home); err == nil {
		t.Fatalf("realClaudeBin = %q, nil; want an error", got)
	}
}

func TestLoginRunsTheRealClaudeNotTheShim(t *testing.T) {
	_, real := shimFirstPATH(t, "real@example.com")
	var gotBin string
	stubAuthExec(t, func(bin, slotDir, _ string) error {
		gotBin = bin
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	})
	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if gotBin != real {
		t.Errorf("auth login ran %q, want the real claude %q", gotBin, real)
	}
	h, _ := home()
	st, err := (store.Store{Dir: h}).Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Find("A")
	if err != nil {
		t.Fatal(err)
	}
	if a.Email != "real@example.com" {
		t.Errorf("recorded email %q, want the real claude's identity", a.Email)
	}
}

func TestLoginHonoursAnExplicitClaude(t *testing.T) {
	shimFirstPATH(t, "real@example.com")
	explicit := writeFakeClaude(t, `{"loggedIn":true,"email":"explicit@example.com"}`)
	var gotBin string
	stubAuthExec(t, func(bin, slotDir, _ string) error {
		gotBin = bin
		return os.WriteFile(filepath.Join(slotDir, ".credentials.json"), []byte(`{}`), 0o600)
	})
	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"--claude", explicit, "A"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogin = %d; stderr=%q", got, errBuf.String())
	}
	if gotBin != explicit {
		t.Errorf("auth login ran %q, want %q", gotBin, explicit)
	}
}

func TestLoginWithNoRealClaudeKeepsTheLoginFailedCode(t *testing.T) {
	home, _ := shimFirstPATH(t, "real@example.com")
	t.Setenv("PATH", strings.Join([]string{filepath.Join(home, "bin")}, string(os.PathListSeparator)))
	var out, errBuf bytes.Buffer
	if got := runLogin([]string{"A"}, strings.NewReader(""), newReporter(true, &out, &errBuf)); got != exit.Error {
		t.Fatalf("runLogin = %d, want %d; stdout=%q", got, exit.Error, out.String())
	}
	if !strings.Contains(out.String(), string(codeLoginFailed)) {
		t.Errorf("stdout %q lacks code %q", out.String(), string(codeLoginFailed))
	}
}

func TestLogoutRunsTheRealClaudeNotTheShim(t *testing.T) {
	home, real := shimFirstPATH(t, "real@example.com")
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: n + "@example.com", Dir: dir}); err != nil {
				return err
			}
		}
		st.Serving, st.Remote = "A", "A"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var gotBin string
	stubAuthExec(t, func(bin, _, _ string) error { gotBin = bin; return nil })
	stubCredsDelete(t, nil)
	var out, errBuf bytes.Buffer
	if got := runLogout([]string{"--yes", "B"}, strings.NewReader(""), newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runLogout = %d; stderr=%q", got, errBuf.String())
	}
	if gotBin != real {
		t.Errorf("auth logout ran %q, want %q", gotBin, real)
	}
}

func TestAdoptProbesWithTheRealClaudeNotTheShim(t *testing.T) {
	home, _ := shimFirstPATH(t, "real@example.com")
	if err := os.MkdirAll(filepath.Join(home, "accounts", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if got := runAdopt(nil, newReporter(false, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runAdopt = %d; stderr=%q", got, errBuf.String())
	}
	st, err := (store.Store{Dir: home}).Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Find("A")
	if err != nil || a.Email != "real@example.com" {
		t.Fatalf("adopted %+v, %v; want the real claude's identity", a, err)
	}
}

// The daemon's token refresh runs `claude mcp list` in a slot. Through the
// shim it would pick up the proxy and have its profile fetch swapped to the
// serving account, writing that account's email into the slot (issue #2).
func TestRefresherNeverExecsTheShim(t *testing.T) {
	home, real := shimFirstPATH(t, "real@example.com")
	// A cached RealClaude that names the shim itself must be ignored.
	shimPath := filepath.Join(home, "bin", "claude")
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error { st.RealClaude = shimPath; return nil }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47821")
	rc := newRefresher("", home)
	var gotBin string
	var gotEnv []string
	rc.Run = func(_ context.Context, bin string, _, env []string) error {
		gotBin, gotEnv = bin, env
		return nil
	}
	if err := rc.Refresh(context.Background(), filepath.Join(home, "accounts", "A")); err != nil {
		t.Fatal(err)
	}
	if gotBin != real {
		t.Errorf("refresh ran %q, want the real claude %q", gotBin, real)
	}
	env := strings.Join(gotEnv, "\n")
	if !strings.Contains(env, "CHOTTAG_BYPASS=1") || strings.Contains(env, "HTTPS_PROXY=") {
		t.Errorf("refresh env = %v, want CHOTTAG_BYPASS=1 and no proxy", gotEnv)
	}
}

func TestRefresherUsesAValidCachedRealClaudeAndAnExplicitPath(t *testing.T) {
	home, _ := shimFirstPATH(t, "real@example.com")
	cached := writeFakeClaude(t, `{}`)
	if _, err := (store.Store{Dir: home}).Update(func(st *store.State) error { st.RealClaude = cached; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := newRefresher("", home).Resolve(); err != nil || got != cached {
		t.Errorf("Resolve = %q, %v; want the cached %q", got, err, cached)
	}
	if got := newRefresher("/explicit/claude", home).Bin; got != "/explicit/claude" {
		t.Errorf("Bin = %q, want the explicit path", got)
	}
}

func TestRefresherWithNoRealClaudeFailsLoudAtRefreshTime(t *testing.T) {
	home, _ := shimFirstPATH(t, "real@example.com")
	t.Setenv("PATH", filepath.Join(home, "bin"))
	rc := newRefresher("", home)
	rc.Run = func(context.Context, string, []string, []string) error {
		t.Error("the refresh ran a binary although no real claude exists")
		return nil
	}
	err := rc.Refresh(context.Background(), filepath.Join(home, "accounts", "A"))
	if err == nil || !strings.Contains(err.Error(), "no real `claude`") {
		t.Fatalf("Refresh = %v, want the no-real-claude error", err)
	}
}

// I1: the daemon outlives a claude that moves; each refresh re-resolves.
func TestRefresherFollowsAClaudeThatMovesWhileTheDaemonRuns(t *testing.T) {
	home, first := shimFirstPATH(t, "real@example.com")
	rc := newRefresher("", home)
	var got []string
	rc.Run = func(_ context.Context, bin string, _, _ []string) error { got = append(got, bin); return nil }
	slot := filepath.Join(home, "accounts", "A")
	if err := rc.Refresh(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	// The claude is uninstalled and a new one appears in another directory.
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	second := writeFakeClaude(t, `{}`)
	t.Setenv("PATH", strings.Join([]string{filepath.Join(home, "bin"), filepath.Dir(second)}, string(os.PathListSeparator)))
	if err := rc.Refresh(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("refresh binaries = %v, want [%s %s]", got, first, second)
	}
}

// adoptIdentityRead seeds A (a@example.com, AOrg) and B (b@example.com, BOrg),
// makes B's slot read as email/org, runs adopt, and returns B and the output.
func adoptIdentityRead(t *testing.T, email, org string) (store.Account, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	loggedIn := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: strings.ToLower(n) + "@example.com", Org: n + "Org", Dir: dir, LoggedInAt: loggedIn}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(home, "fake-claude")
	script := "#!/bin/sh\ncase \"$CLAUDE_CONFIG_DIR\" in\n*/B) echo '{\"loggedIn\":true,\"email\":\"" + email + "\",\"orgName\":\"" + org + "\"}' ;;\n*) echo '{\"loggedIn\":true,\"email\":\"a@example.com\",\"orgName\":\"AOrg\"}' ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if got := runAdopt([]string{"--claude", fake}, newReporter(true, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runAdopt = %d; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Find("B")
	if err != nil {
		t.Fatal(err)
	}
	return *b, out.String()
}

// The corruption's signature: only the email moved, onto another registered
// account's, and the org stayed.
func TestAdoptRefusesTheCorruptionSignature(t *testing.T) {
	b, out := adoptIdentityRead(t, "a@example.com", "BOrg")
	if b.Email != "b@example.com" || b.Org != "BOrg" || !b.LoggedInAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("B = %+v, want its email, org and LoggedInAt untouched", b)
	}
	if !strings.Contains(out, `"code": "identity_suspect"`) || !strings.Contains(out, "chottag login B") {
		t.Errorf("no identity_suspect warning naming `chottag login B` in %s", out)
	}
}

func TestAdoptKeepsAnUnchangedSharedEmail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, n := range []string{"A", "B"} {
			dir := filepath.Join(home, "accounts", n)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: n, Email: "alice@example.com", Org: n + "Org", Dir: dir}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(home, "fake-claude")
	script := "#!/bin/sh\ncase \"$CLAUDE_CONFIG_DIR\" in\n*/B) echo '{\"loggedIn\":true,\"email\":\"alice@example.com\",\"orgName\":\"BOrg\"}' ;;\n*) echo '{\"loggedIn\":true,\"email\":\"alice@example.com\",\"orgName\":\"AOrg\"}' ;;\nesac\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if got := runAdopt([]string{"--claude", fake}, newReporter(true, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runAdopt = %d; stderr=%q", got, errBuf.String())
	}
	if strings.Contains(out.String(), "identity_suspect") {
		t.Errorf("an unchanged shared email was refused: %s", out.String())
	}
}

func TestAdoptTakesAnEmailChangeThatAlsoChangesTheOrg(t *testing.T) {
	b, out := adoptIdentityRead(t, "a@example.com", "AOrg")
	if b.Email != "a@example.com" || b.Org != "AOrg" || strings.Contains(out, "identity_suspect") {
		t.Errorf("B = %+v, out %s; want the re-login onto the other org adopted", b, out)
	}
}

func TestAdoptTakesAnEmailChangeToAnUnregisteredEmail(t *testing.T) {
	b, out := adoptIdentityRead(t, "carol@example.com", "BOrg")
	if b.Email != "carol@example.com" || strings.Contains(out, "identity_suspect") {
		t.Errorf("B = %+v, out %s; want the new email adopted", b, out)
	}
}

type seedAcct struct{ name, email, org string }

// adoptTarget seeds the accounts (serving is the default pool's serving
// account), makes target's slot read as email/org (every other slot reads as
// its own stored identity), runs adopt, and returns target and the JSON.
func adoptTarget(t *testing.T, accts []seedAcct, serving, target, email, org string) (store.Account, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CHOTTAG_HOME", home)
	s := store.Store{Dir: home}
	if _, err := s.Update(func(st *store.State) error {
		for _, a := range accts {
			dir := filepath.Join(home, "accounts", a.name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			if err := st.Add(store.Account{Name: a.name, Email: a.email, Org: a.org, Dir: dir}); err != nil {
				return err
			}
		}
		st.Serving = serving
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$CLAUDE_CONFIG_DIR\" in\n"
	for _, a := range accts {
		e, o := a.email, a.org
		if a.name == target {
			e, o = email, org
		}
		script += "*/" + a.name + `) printf '%s\n' '{"loggedIn":true,"email":"` + e + `","orgName":"` + o + `"}' ;;` + "\n"
	}
	script += "esac\n"
	fake := filepath.Join(home, "fake-claude")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if got := runAdopt([]string{"--claude", fake}, newReporter(true, &out, &errBuf)); got != exit.OK {
		t.Fatalf("runAdopt = %d; stderr=%q", got, errBuf.String())
	}
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.Find(target)
	if err != nil {
		t.Fatal(err)
	}
	return *a, out.String()
}

// M1: A was damaged (it records C's email), and C is serving. The fixed
// daemon's refresh makes A's slot read its true email again, which B (one
// login, another org) also records: adopting it is the healing.
func TestAdoptHealsAnAccountOntoAnEmailSharedWithANonServingAccount(t *testing.T) {
	a, out := adoptTarget(t, []seedAcct{
		{"C", "carol@example.com", "COrg"},
		{"A", "carol@example.com", "AOrg"},
		{"B", "alice@example.com", "BOrg"},
	}, "C", "A", "alice@example.com", "AOrg")
	if a.Email != "alice@example.com" || strings.Contains(out, "identity_suspect") {
		t.Errorf("A = %+v, out %s; want the heal adopted", a, out)
	}
}

// M2: a blank stored org is unknown, so an email change onto a serving
// account's email is refused.
func TestAdoptRefusesAnEmailChangeOntoAServingEmailWhenTheStoredOrgIsBlank(t *testing.T) {
	b, out := adoptTarget(t, []seedAcct{
		{"A", "a@example.com", "AOrg"},
		{"B", "b@example.com", ""},
	}, "A", "B", "a@example.com", "BOrg")
	if b.Email != "b@example.com" || !strings.Contains(out, `"code": "identity_suspect"`) {
		t.Errorf("B = %+v, out %s; want it refused", b, out)
	}
}

// A first fill is not a change.
func TestAdoptFillsABlankStoredEmail(t *testing.T) {
	b, out := adoptTarget(t, []seedAcct{
		{"A", "a@example.com", "AOrg"},
		{"B", "", "BOrg"},
	}, "A", "B", "a@example.com", "BOrg")
	if b.Email != "a@example.com" || strings.Contains(out, "identity_suspect") {
		t.Errorf("B = %+v, out %s; want the first fill adopted", b, out)
	}
}

// Serving in a pool other than default counts too.
func TestSuspectIdentityCountsAServingAccountInAnyPool(t *testing.T) {
	st := &store.State{
		Accounts: []store.Account{
			{Name: "A", Email: "a@example.com", Org: "AOrg"},
			{Name: "B", Email: "b@example.com", Org: "BOrg"},
			{Name: "P", Email: "p@example.com", Org: "POrg"},
		},
		Serving: "A",
		Pools:   map[string]store.Pool{"work": {Serving: "P"}},
	}
	b := &st.Accounts[1]
	if !suspectIdentity(st, b, "p@example.com", "BOrg") {
		t.Error("an email change onto another pool's serving account was not refused")
	}
	if suspectIdentity(st, b, "carol@example.com", "BOrg") {
		t.Error("an unregistered email was refused")
	}
	if suspectIdentity(st, &st.Accounts[0], "a@example.com", "AOrg") {
		t.Error("an unchanged identity was refused")
	}
}
