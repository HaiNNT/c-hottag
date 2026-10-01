package cli

// Tests for the update check's CLI side (R124): the update lock,
// --no-restart, the --auto-check / --auto-install switches, and --check
// writing the status cache. updateFetch is always stubbed: no test here
// reaches the network.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/fsutil"
	"github.com/HaiNNT/c-hottag/internal/status"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

// stubFetch swaps updateFetch for the test; the repo it was asked about is
// recorded in *repos.
func stubFetch(t *testing.T, rel updatecheck.Release, err error) *[]string {
	t.Helper()
	repos := &[]string{}
	orig := updateFetch
	updateFetch = func(_ context.Context, _ *url.URL, repo string) (updatecheck.Release, error) {
		*repos = append(*repos, repo)
		return rel, err
	}
	t.Cleanup(func() { updateFetch = orig })
	return repos
}

func release(tag string, published time.Time) updatecheck.Release {
	return updatecheck.Release{Tag: tag, Version: strings.TrimPrefix(tag, "v"), PublishedAt: published}
}

func runUpdateJSON(t *testing.T, args ...string) (int, map[string]any, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runUpdate(args, newReporter(true, &out, &errb))
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out.String())
	}
	return code, doc, errb.String()
}

func TestUpdateHoldingTheLockRefusesAnInstall(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	if err := os.MkdirAll(filepath.Join(h, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := fsutil.TryLock(filepath.Join(h, "run", "update.lock"))
	if err != nil || !ok {
		t.Fatalf("could not take the lock: ok=%v err=%v", ok, err)
	}
	defer unlock()
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, ghCalls := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, childCalls := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)

	code, doc, _ := runUpdateJSON(t)
	if code != exit.Error {
		t.Fatalf("exit = %d, want 1", code)
	}
	errObj, _ := doc["error"].(map[string]any)
	if doc["ok"] != false || errObj["code"] != "update_in_progress" || !strings.Contains(errObj["message"].(string), "another chottag update is running") {
		t.Errorf("doc = %v, want update_in_progress / another chottag update is running", doc)
	}
	if hasDownloadCall(*ghCalls) || len(*childCalls) != 0 {
		t.Errorf("a refused update downloaded or ran a child: gh=%v child=%v", *ghCalls, *childCalls)
	}
}

func TestUpdateReleasesTheLockWhenDone(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, bareUpdateGH(t, base), child, neverRunningProbe)
	if code, _, errb := runUpdateJSON(t); code != exit.OK {
		t.Fatalf("exit = %d; %s", code, errb)
	}
	unlock, ok, err := fsutil.TryLock(filepath.Join(h, "run", "update.lock"))
	if err != nil || !ok {
		t.Fatalf("the lock is still held after update returned: ok=%v err=%v", ok, err)
	}
	unlock()
}

func TestUpdateNoRestartLeavesTheDaemonAlone(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("payload"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	child, childCalls := fakeChild(t, h, true, nil)
	// A running daemon and no live session: without --no-restart this restarts.
	stubUpdateSeams(t, bareUpdateGH(t, base), child, func(int) (bool, string) { return true, "0.3.0" })

	code, doc, errb := runUpdateJSON(t, "--no-restart")
	if code != exit.OK {
		t.Fatalf("exit = %d; %s", code, errb)
	}
	if doc["daemon"] != "not-restarted" || doc["installed"] != true {
		t.Errorf("doc = %v, want installed with daemon not-restarted", doc)
	}
	for _, c := range *childCalls {
		if len(c.args) == 2 && c.args[0] == "daemon" {
			t.Errorf("--no-restart ran %v", c.args)
		}
	}
}

func TestUpdateNoRestartAndRestartConflict(t *testing.T) {
	updateHome(t)
	code, doc, _ := runUpdateJSON(t, "--no-restart", "--restart")
	if code != exit.Usage || doc["ok"] != false {
		t.Errorf("exit = %d doc = %v, want a usage error", code, doc)
	}
}

func TestUpdateAutoSwitches(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantText  string
		wantCheck bool
		wantAuto  bool
	}{
		{"check off", []string{"--auto-check", "off"}, "update check: off; auto-install: off; auto-restart: on\n", false, false},
		{"check on", []string{"--auto-check", "on"}, "update check: on; auto-install: off; auto-restart: on\n", true, false},
		{"install on turns the check on", []string{"--auto-install", "on"}, "update check: on; auto-install: on; auto-restart: on\n", true, true},
		{"both", []string{"--auto-check", "on", "--auto-install", "on"}, "update check: on; auto-install: on; auto-restart: on\n", true, true},
		{"restart off leaves the others", []string{"--auto-restart", "off"}, "update check: on; auto-install: off; auto-restart: off\n", true, false},
		{"restart on", []string{"--auto-restart", "on"}, "update check: on; auto-install: off; auto-restart: on\n", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := updateHome(t)
			stubUpdateSeams(t, nil, nil, nil) // any gh, child or probe call would panic on nil
			var out, errb bytes.Buffer
			if code := runUpdate(c.args, newReporter(false, &out, &errb)); code != exit.OK {
				t.Fatalf("exit = %d; %s", code, errb.String())
			}
			if out.String() != c.wantText {
				t.Errorf("stdout = %q, want %q", out.String(), c.wantText)
			}
			st, err := (store.Store{Dir: h}).Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.UpdateCheckOn() != c.wantCheck || st.AutoUpdateOn() != c.wantAuto {
				t.Errorf("state check=%v auto=%v, want %v %v", st.UpdateCheckOn(), st.AutoUpdateOn(), c.wantCheck, c.wantAuto)
			}
			code, doc, _ := runUpdateJSON(t, c.args...)
			u, _ := doc["updates"].(map[string]any)
			wantRestart := !strings.Contains(c.wantText, "auto-restart: off")
			if code != exit.OK || u["check"] != c.wantCheck || u["auto"] != c.wantAuto || u["restart"] != wantRestart {
				t.Errorf("json = %v, want updates {check:%v auto:%v}", doc, c.wantCheck, c.wantAuto)
			}
		})
	}
}

func TestUpdateAutoCheckOffAlsoTurnsAutoInstallOff(t *testing.T) {
	h := updateHome(t)
	stubUpdateSeams(t, nil, nil, nil)
	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--auto-install", "on"}, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatal(errb.String())
	}
	out.Reset()
	if code := runUpdate([]string{"--auto-check", "off"}, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatal(errb.String())
	}
	st, _ := (store.Store{Dir: h}).Load()
	if st.UpdateCheckOn() || st.AutoUpdateOn() || out.String() != "update check: off; auto-install: off; auto-restart: on\n" {
		t.Errorf("check=%v auto=%v out=%q, want both off", st.UpdateCheckOn(), st.AutoUpdateOn(), out.String())
	}
}

func TestUpdateAutoSwitchUsageErrors(t *testing.T) {
	cases := [][]string{
		{"--auto-check", "maybe"},
		{"--auto-install", ""},
		{"--auto-check", "on", "--check"},
		{"--auto-install", "on", "--version", "v0.3.1"},
		{"--auto-check", "off", "--restart"},
		{"--auto-install", "off", "--no-restart"},
		{"--auto-check", "on", "extra"},
		{"--auto-check", "off", "--auto-install", "on"},
		{"--auto-restart", "maybe"},
		{"--auto-restart", "on", "--check"},
		{"--auto-restart", "off", "--version", "v0.3.1"},
		{"--auto-restart", "off", "--restart"},
		{"--auto-restart", "off", "--no-restart"},
		{"--auto-restart", "off", "--repo", "Acme/chottag"},
		{"--auto-restart", "on", "extra"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h := updateHome(t)
			stubUpdateSeams(t, nil, nil, nil)
			code, doc, _ := runUpdateJSON(t, args...)
			if code != exit.Usage || doc["ok"] != false {
				t.Errorf("exit = %d doc = %v, want a usage error", code, doc)
			}
			st, _ := (store.Store{Dir: h}).Load()
			if st.Updates.Check != nil || st.Updates.Auto != nil || st.Updates.Restart != nil {
				t.Errorf("a refused command changed the switches: %+v", st.Updates)
			}
		})
	}
}

func loadUpdateCache(t *testing.T, h string) *status.Update {
	t.Helper()
	f, err := status.Load(status.Path(h))
	if err != nil {
		t.Fatal(err)
	}
	return f.Update
}

func TestUpdateCheckWritesTheCache(t *testing.T) {
	published := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		version   string
		rel       updatecheck.Release
		wantAvail bool
	}{
		{"newer", "0.3.0", release("v0.3.1", published), true},
		{"equal", "0.3.1", release("v0.3.1", published), false},
		{"older", "0.4.0", release("v0.3.1", published), false},
		{"a dev build never shows one", "dev", release("v0.3.1", published), false},
		{"a pre-release is not available", "0.3.0", updatecheck.Release{Tag: "v0.3.1", Version: "0.3.1", PublishedAt: published, Prerelease: true}, false},
		{"a draft is not available", "0.3.0", updatecheck.Release{Tag: "v0.3.1", Version: "0.3.1", PublishedAt: published, Draft: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withVersion(t, c.version)
			h := updateHome(t)
			repos := stubFetch(t, c.rel, nil)
			stubUpdateSeams(t, nil, nil, nil) // --check no longer needs gh
			before := time.Now()
			code, doc, errb := runUpdateJSON(t, "--check")
			if code != exit.OK {
				t.Fatalf("exit = %d; %s", code, errb)
			}
			if len(*repos) != 1 || (*repos)[0] != defaultRepo {
				t.Errorf("fetched %v, want [%s]", *repos, defaultRepo)
			}
			if doc["latest"] != "0.3.1" {
				t.Errorf("doc = %v, want latest 0.3.1", doc)
			}
			u := loadUpdateCache(t, h)
			if u == nil {
				t.Fatal("status.json has no update")
			}
			if u.Latest != "0.3.1" || !u.PublishedAt.Equal(published) || u.Available != c.wantAvail || u.Error != "" || u.CheckedAt.Before(before) {
				t.Errorf("update = %+v, want latest 0.3.1, publishedAt %v, available %v, no error, checkedAt >= %v", *u, published, c.wantAvail, before)
			}
		})
	}
}

// The cache `update --check` writes applies the daemon's own rules for
// `available`: no semver pre-release, nothing already installed.
func TestUpdateCheckCacheAppliesTheDaemonsAvailabilityRules(t *testing.T) {
	published := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		rel       updatecheck.Release
		installed string
		wantAvail bool
	}{
		{"a semver pre-release tag", release("v0.3.1-rc.1", published), "", false},
		{"already installed, not yet running", release("v0.3.1", published), "0.3.1", false},
		{"newer than the installed one", release("v0.3.2", published), "0.3.1", true},
		{"installed unknown counts the running version only", release("v0.3.1", published), "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			withVersion(t, "0.3.0")
			h := updateHome(t)
			stubFetch(t, c.rel, nil)
			stubUpdateSeams(t, nil, nil, nil)
			stubInstalled(t, c.installed)
			if code, _, errb := runUpdateJSON(t, "--check"); code != exit.OK {
				t.Fatal(errb)
			}
			if u := loadUpdateCache(t, h); u == nil || u.Available != c.wantAvail {
				t.Errorf("update = %+v, want available %v", u, c.wantAvail)
			}
		})
	}
}

func TestUpdateCheckKeepsNotifiedAndAuto(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	var f status.File
	f.Update = &status.Update{Notified: "0.3.1", Auto: &status.AutoAttempt{Version: "0.3.1", OK: false, Error: "refused"}}
	b, err := status.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := status.WriteBytes(status.Path(h), b); err != nil {
		t.Fatal(err)
	}
	stubFetch(t, release("v0.3.1", time.Now().Add(-48*time.Hour)), nil)
	stubUpdateSeams(t, nil, nil, nil)
	if code, _, errb := runUpdateJSON(t, "--check"); code != exit.OK {
		t.Fatal(errb)
	}
	u := loadUpdateCache(t, h)
	if u == nil || u.Notified != "0.3.1" || u.Auto == nil || u.Auto.Error != "refused" {
		t.Errorf("update = %+v, want notified and auto kept", u)
	}
}

func TestUpdateCheckFetchErrorIsRecordedAndFails(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	stubFetch(t, updatecheck.Release{}, errors.New("updatecheck: latest release: unexpected status 403 Forbidden"))
	stubUpdateSeams(t, nil, nil, nil)
	code, doc, _ := runUpdateJSON(t, "--check")
	errObj, _ := doc["error"].(map[string]any)
	if code != exit.Error || errObj["code"] != "update_failed" {
		t.Fatalf("exit = %d doc = %v, want update_failed", code, doc)
	}
	u := loadUpdateCache(t, h)
	if u == nil || !strings.Contains(u.Error, "403") || u.CheckedAt.IsZero() || u.Available {
		t.Errorf("update = %+v, want the error and checkedAt recorded", u)
	}
}

func TestUpdateCheckWithAVersionWritesNoCache(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	stubUpdateSeams(t, nil, nil, nil) // neither gh nor a fetch: nothing is asked
	if code, _, errb := runUpdateJSON(t, "--check", "--version", "v0.3.1"); code != exit.OK {
		t.Fatal(errb)
	}
	if _, err := os.Stat(status.Path(h)); err == nil {
		t.Error("--check --version wrote status.json")
	}
}

// The install path keeps the "a dev build can update to a release" rule even
// though updatecheck.Newer is false for an unparseable current version.
func TestReleaseNewerKeepsTheDevRule(t *testing.T) {
	if !releaseNewer("dev", "0.3.0") {
		t.Error("a dev build must be older than any release")
	}
	if releaseNewer("0.3.0", "dev") || releaseNewer("dev", "dev") {
		t.Error("an unparseable candidate is never newer")
	}
}

func TestStatusSinkSetUpdatePersistsAndCopies(t *testing.T) {
	home := t.TempDir()
	sink, err := newStatusSink(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &status.Update{Latest: "0.6.0", Available: true, Auto: &status.AutoAttempt{Version: "0.6.0", Error: "x"}}
	sink.setUpdate(u)
	u.Latest, u.Auto.Error = "changed", "changed" // the sink must not share the caller's values
	sink.setUpdate(nil)                           // ignored
	sink.Close()                                  // flushes the newest document
	got := loadUpdateCache(t, home)
	if got == nil || got.Latest != "0.6.0" || !got.Available || got.Auto == nil || got.Auto.Error != "x" {
		t.Errorf("update = %+v, want the value as first set", got)
	}
}

func TestUpdateRepoResolvesLikeUpdate(t *testing.T) {
	h := t.TempDir()
	if got := updateRepo(h); got != defaultRepo {
		t.Errorf("no install.json: repo = %q, want the default", got)
	}
	rec := installRecord{Repo: "Acme/chottag", Version: "0.3.0", Source: "release", InstalledAt: time.Now().UTC()}
	if err := writeInstallRecord(h, rec); err != nil {
		t.Fatal(err)
	}
	if got := updateRepo(h); got != "Acme/chottag" {
		t.Errorf("repo = %q, want install.json's", got)
	}
	rec.Repo = "not a repo"
	if err := writeInstallRecord(h, rec); err != nil {
		t.Fatal(err)
	}
	if got := updateRepo(h); got != defaultRepo {
		t.Errorf("invalid install.json repo: repo = %q, want the default", got)
	}
}

func TestUpdateCheckWithAnotherRepoWritesNoCache(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	repos := stubFetch(t, release("v0.3.1", time.Now().Add(-48*time.Hour)), nil)
	stubUpdateSeams(t, nil, nil, nil)
	if code, _, errb := runUpdateJSON(t, "--check", "--repo", "Acme/other"); code != exit.OK {
		t.Fatal(errb)
	}
	if len(*repos) != 1 || (*repos)[0] != "Acme/other" {
		t.Errorf("fetched %v, want Acme/other", *repos)
	}
	if _, err := os.Stat(status.Path(h)); err == nil {
		t.Error("--check --repo for another repo wrote the shared cache")
	}
	// The install's own repo, named explicitly, is recorded.
	if code, _, errb := runUpdateJSON(t, "--check", "--repo", defaultRepo); code != exit.OK {
		t.Fatal(errb)
	}
	if loadUpdateCache(t, h) == nil {
		t.Error("--check --repo <own repo> recorded nothing")
	}
}

func TestFetchLatestRequestsTheReleasesLatestURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"tag_name":"v0.6.0","published_at":"2026-09-30T09:00:00Z"}`))
	}))
	defer srv.Close()
	rel, err := fetchLatest(context.Background(), srv.Client(), srv.URL, "Acme/chottag")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/Acme/chottag/releases/latest" || rel.Version != "0.6.0" {
		t.Errorf("path = %q rel = %+v", gotPath, rel)
	}
}

// The production updateFetch dials through the upstream it is given.
func TestProductionUpdateFetchUsesTheUpstreamProxy(t *testing.T) {
	var gotHost string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		http.Error(w, "stop", http.StatusForbidden)
	}))
	defer proxy.Close()
	pu, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	// An https target through an http proxy is a CONNECT; the stub proxy
	// refuses it, so the fetch fails, but only after reaching the proxy.
	_, err = fetchViaUpstream(context.Background(), pu, "Acme/chottag")
	if err == nil {
		t.Fatal("want an error from the refusing proxy")
	}
	if gotHost != "api.github.com:443" {
		t.Errorf("the proxy was asked for %q, want api.github.com:443", gotHost)
	}
}

func TestUpdateUpstreamSkipsChottagsOwnProxy(t *testing.T) {
	h := updateHome(t)
	port, err := daemonPort(h)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTPS_PROXY", "http://corp.example.com:3128")
	if u, err := updateUpstream(h); err != nil || u == nil || u.Host != "corp.example.com:3128" {
		t.Errorf("upstream = %v, %v, want the shell's proxy", u, err)
	}
	t.Setenv("HTTPS_PROXY", fmt.Sprintf("http://chottag:x@127.0.0.1:%d", port))
	if u, err := updateUpstream(h); err != nil || u != nil {
		t.Errorf("upstream = %v, %v, want none for chottag's own proxy", u, err)
	}
	t.Setenv("HTTPS_PROXY", "")
	if u, err := updateUpstream(h); err != nil || u != nil {
		t.Errorf("upstream = %v, %v, want none", u, err)
	}
}

// TestUpdateDeferredTextSaysTheDaemonRestartsItselfUnlessOff pins R126's
// wording: with live sessions the restart is deferred, and the text says the
// daemon restarts itself when idle, unless auto-restart is off.
func TestUpdateDeferredTextSaysTheDaemonRestartsItselfUnlessOff(t *testing.T) {
	for _, c := range []struct {
		name   string
		off    bool
		daemon string // the running daemon's version
		want   string
	}{
		{"on", false, "0.6.0", "chottag: daemon restart deferred: 2 session(s) running; the daemon restarts itself when idle, or run: chottag daemon restart\n"},
		{"off", true, "0.6.0", "chottag: daemon restart deferred: 2 session(s) running; auto-restart is off, so the daemon keeps running 0.6.0: run chottag daemon restart once when convenient\n"},
		{"a daemon from before the loop", false, "0.5.0", "chottag: daemon restart deferred: 2 session(s) running; the running daemon (0.5.0) can't restart itself: run chottag daemon restart once when convenient\n"},
		{"a rollback", false, "0.7.0", "chottag: daemon restart deferred: 2 session(s) running; the running daemon (0.7.0) is newer than 0.6.1 and won't restart onto it: run chottag daemon restart once when convenient\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withVersion(t, "0.6.0")
			h := updateHome(t)
			seedLiveSessions(t, h, 2)
			if c.off {
				if _, err := (store.Store{Dir: h}).Update(func(st *store.State) error { st.SetAutoRestart(false); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			fx := goodRelease(t, "0.6.1", []byte("payload"))
			base, _ := fakeGH(t, nil, "v0.6.1", nil, map[string]releaseFixture{"v0.6.1": fx})
			gh := bareUpdateGH(t, base)
			child, _ := fakeChild(t, h, true, nil)
			stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, c.daemon })
			var out, errb bytes.Buffer
			if code := runUpdate(nil, newReporter(false, &out, &errb)); code != exit.OK {
				t.Fatalf("exit = %d; %s", code, errb.String())
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("stdout = %q, want it to contain %q", out.String(), c.want)
			}
		})
	}
}

// R129: `update --json` says whether the deferred restart happens on its own,
// so an agent knows to run `chottag daemon restart` to finish the update.
func TestUpdateJSONSaysWhetherTheDaemonRestartsItself(t *testing.T) {
	for _, c := range []struct {
		daemon string
		want   bool
	}{{"0.6.0", true}, {"0.5.0", false}} {
		t.Run(c.daemon, func(t *testing.T) {
			withVersion(t, "0.6.0")
			h := updateHome(t)
			seedLiveSessions(t, h, 2)
			fx := goodRelease(t, "0.6.1", []byte("payload"))
			base, _ := fakeGH(t, nil, "v0.6.1", nil, map[string]releaseFixture{"v0.6.1": fx})
			gh := bareUpdateGH(t, base)
			child, _ := fakeChild(t, h, true, nil)
			stubUpdateSeams(t, gh, child, func(int) (bool, string) { return true, c.daemon })
			var out, errb bytes.Buffer
			if code := runUpdate(nil, newReporter(true, &out, &errb)); code != exit.OK {
				t.Fatalf("exit = %d; %s", code, errb.String())
			}
			var doc struct {
				Daemon      string `json:"daemon"`
				SelfRestart *bool  `json:"selfRestart"`
			}
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("%v: %s", err, out.String())
			}
			if doc.Daemon != "deferred" || doc.SelfRestart == nil || *doc.SelfRestart != c.want {
				t.Fatalf("daemon=%q selfRestart=%v, want deferred and %v", doc.Daemon, doc.SelfRestart, c.want)
			}
		})
	}
}
