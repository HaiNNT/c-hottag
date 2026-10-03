package cli

// R156, R157: `update` and `update --check` end with what's new and the
// plugin step. The releases list is always a fake: no test reaches the network.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/exit"
	"github.com/HaiNNT/c-hottag/internal/updatecheck"
)

func stubReleases(t *testing.T, rels []updatecheck.Release, err error) {
	t.Helper()
	orig := updateReleases
	updateReleases = func(context.Context, *url.URL, string) ([]updatecheck.Release, error) { return rels, err }
	t.Cleanup(func() { updateReleases = orig })
}

func relNote(v, body string) updatecheck.Release {
	return updatecheck.Release{Tag: "v" + v, Version: v, Body: body, URL: "https://github.com/Acme/chottag/releases/tag/v" + v}
}

var sampleReleases = []updatecheck.Release{
	relNote("0.3.2", "Newer than the install."),
	relNote("0.3.1", "Lead of 0.3.1.\n\nSecond paragraph."),
	relNote("0.3.0", "Already installed."),
}

const pluginMarketplaceCmd = "claude plugin marketplace update c-hottag"

func TestUpdateCheckPrintsWhatsNewAndThePluginStep(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--check"}, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	got := out.String()
	for _, want := range []string{
		"update available: 0.3.0 -> 0.3.1",
		"What's new:\n  0.3.1: Lead of 0.3.1.\n    https://github.com/Acme/chottag/releases/tag/v0.3.1\n",
		pluginMarketplaceCmd, "claude plugin update chottag@c-hottag", "/reload-plugins",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "0.3.2") || strings.Contains(got, "Already installed") {
		t.Errorf("stdout names a release outside (0.3.0, 0.3.1]:\n%s", got)
	}
}

func TestUpdateCheckJSONHasWhatsNewAndPlugin(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	code, doc, errText := runUpdateJSON(t, "--check")
	if code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errText)
	}
	raw, _ := json.Marshal(doc)
	var res struct {
		WhatsNew []updatecheck.Note `json:"whatsNew"`
		Plugin   *pluginStep        `json:"plugin"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.WhatsNew) != 1 || res.WhatsNew[0].Version != "0.3.1" || res.WhatsNew[0].Summary != "Lead of 0.3.1." || res.WhatsNew[0].URL != "https://github.com/Acme/chottag/releases/tag/v0.3.1" {
		t.Errorf("whatsNew = %+v", res.WhatsNew)
	}
	if res.Plugin == nil || len(res.Plugin.Commands) != 3 || res.Plugin.Commands[0] != pluginMarketplaceCmd || res.Plugin.Note == "" {
		t.Errorf("plugin = %+v", res.Plugin)
	}
}

func TestUpdateCheckUpToDateHasNeitherBlock(t *testing.T) {
	withVersion(t, "0.3.1")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	_, doc, _ := runUpdateJSON(t, "--check")
	if _, ok := doc["whatsNew"]; ok {
		t.Errorf("whatsNew present when up to date: %v", doc)
	}
	if _, ok := doc["plugin"]; ok {
		t.Errorf("plugin present when up to date: %v", doc)
	}
}

// A failed releases fetch only drops the block: the check still succeeds, and
// the plugin step stays.
func TestUpdateCheckWhatsNewFetchFailureDropsOnlyTheBlock(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, nil, errors.New("boom"))

	code, doc, errText := runUpdateJSON(t, "--check")
	if code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errText)
	}
	if _, ok := doc["whatsNew"]; ok {
		t.Errorf("whatsNew present after a failed fetch: %v", doc)
	}
	if doc["plugin"] == nil {
		t.Errorf("plugin missing: %v", doc)
	}
}

func TestUpdateInstallEndsWithWhatsNewAndThePluginStep(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("#!/bin/sh\necho fake-chottag\n"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	var out, errb bytes.Buffer
	if code := runUpdate(nil, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	got := out.String()
	iDaemon := strings.Index(got, "daemon not running")
	iNew := strings.Index(got, "What's new:\n  0.3.1: Lead of 0.3.1.")
	iPlugin := strings.Index(got, pluginMarketplaceCmd)
	if iDaemon < 0 || iNew < iDaemon || iPlugin < iNew {
		t.Errorf("want the daemon line, then what's new, then the plugin step:\n%s", got)
	}
}

// A rollback installs an older release: no plugin step, no summary.
func TestUpdateRollbackHasNoPluginStep(t *testing.T) {
	withVersion(t, "0.3.1")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.0", []byte("#!/bin/sh\necho fake-chottag\n"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.0": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	var out, errb bytes.Buffer
	if code := runUpdate([]string{"--version", "v0.3.0"}, newReporter(false, &out, &errb)); code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "What's new") || strings.Contains(out.String(), "plugin") {
		t.Errorf("a rollback printed the extras:\n%s", out.String())
	}
}

func TestUpdateInstallJSONHasWhatsNewAndPlugin(t *testing.T) {
	withVersion(t, "0.3.0")
	h := updateHome(t)
	fx := goodRelease(t, "0.3.1", []byte("#!/bin/sh\necho fake-chottag\n"))
	base, _ := fakeGH(t, nil, "v0.3.1", nil, map[string]releaseFixture{"v0.3.1": fx})
	gh := bareUpdateGH(t, base)
	child, _ := fakeChild(t, h, true, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	code, doc, errText := runUpdateJSON(t)
	if code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errText)
	}
	wn, _ := doc["whatsNew"].([]any)
	if doc["installed"] != true || len(wn) != 1 {
		t.Fatalf("doc = %v", doc)
	}
	first, _ := wn[0].(map[string]any)
	if first["version"] != "0.3.1" || first["summary"] != "Lead of 0.3.1." {
		t.Errorf("whatsNew[0] = %v", first)
	}
	plugin, _ := doc["plugin"].(map[string]any)
	cmds, _ := plugin["commands"].([]any)
	if len(cmds) != 3 || cmds[0] != pluginMarketplaceCmd || plugin["note"] == "" {
		t.Errorf("plugin = %v", plugin)
	}
}

// The releases list is asked about the repo update works against: --repo's.
func TestUpdateWhatsNewAsksTheResolvedRepo(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	var asked []string
	updateReleases = func(_ context.Context, _ *url.URL, repo string) ([]updatecheck.Release, error) {
		asked = append(asked, repo)
		return sampleReleases, nil
	}

	if code, _, errText := runUpdateJSON(t, "--check", "--repo", "Acme/other"); code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errText)
	}
	if len(asked) != 1 || asked[0] != "Acme/other" {
		t.Errorf("asked = %v, want [Acme/other]", asked)
	}
}

// --check --version T names the release itself: the extras still follow it.
func TestUpdateCheckWithAnExplicitNewerVersionHasTheExtras(t *testing.T) {
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.0", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	stubReleases(t, sampleReleases, nil)

	code, doc, errText := runUpdateJSON(t, "--check", "--version", "v0.3.1")
	if code != exit.OK {
		t.Fatalf("exit = %d: %s", code, errText)
	}
	wn, _ := doc["whatsNew"].([]any)
	if len(wn) != 1 || doc["plugin"] == nil {
		t.Errorf("doc = %v", doc)
	}
}

// The daemon's own install child (R156/R157 extras are for people) makes no
// releases request and prints no plugin step.
func TestUpdateFromTheDaemonSkipsTheExtras(t *testing.T) {
	t.Setenv(autoUpdateEnv, "1")
	withVersion(t, "0.3.0")
	updateHome(t)
	gh, _ := fakeGH(t, nil, "v0.3.1", nil, nil)
	child, _ := fakeChild(t, "", false, nil)
	stubUpdateSeams(t, gh, child, neverRunningProbe)
	updateReleases = func(context.Context, *url.URL, string) ([]updatecheck.Release, error) {
		t.Error("updateReleases was called for the daemon's own install")
		return nil, nil
	}

	_, doc, _ := runUpdateJSON(t, "--check")
	if doc["whatsNew"] != nil || doc["plugin"] != nil {
		t.Errorf("doc = %v", doc)
	}
}
