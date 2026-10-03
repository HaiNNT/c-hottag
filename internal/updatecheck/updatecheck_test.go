package updatecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLatest(t *testing.T) {
	const body = `{"tag_name":"v0.6.0","published_at":"2026-09-30T12:00:00Z","draft":false,"prerelease":true}`
	var gotPath, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAccept = r.URL.Path, r.Header.Get("Accept")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	rel, err := Latest(context.Background(), srv.Client(), srv.URL, "Acme/chottag")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/Acme/chottag/releases/latest" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	want := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if rel.Tag != "v0.6.0" || rel.Version != "0.6.0" || !rel.PublishedAt.Equal(want) || rel.Draft || !rel.Prerelease {
		t.Errorf("rel = %+v", rel)
	}
}

func TestLatestErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{"404", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusNotFound) }, "404"},
		{"403 rate limit", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "rate limited", http.StatusForbidden) }, "403"},
		{"non-JSON", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }, "decode"},
		{"empty tag", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }, "tag"},
		{"2 MiB body", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":"v0.6.0","pad":"` + strings.Repeat("x", 2<<20) + `"}`))
		}, "1 MiB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			_, err := Latest(context.Background(), srv.Client(), srv.URL, "Acme/chottag")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}

func TestLatestContextCancelled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Latest(ctx, srv.Client(), srv.URL, "Acme/chottag"); err == nil {
		t.Fatal("want an error from an expired context")
	}
}

// The ordering cases are ported from internal/cli's TestSemverNewer, which
// asked semverNewer(a, b) = "is b newer than a". Newer takes the candidate
// first, so here b is the candidate and a is the current version.
func TestNewer(t *testing.T) {
	cases := []struct {
		name      string
		a, b      string
		wantNewer bool
	}{
		{"equal", "0.3.0", "0.3.0", false},
		{"plain newer patch", "0.3.0", "0.3.1", true},
		{"plain older patch", "0.3.1", "0.3.0", false},
		{"rc < release", "0.3.0-rc1", "0.3.0", true},
		{"release < rc of next", "0.3.0", "0.3.0-rc1", false},
		{"release < describe", "0.3.0", "0.3.0-5-gabc1234", true},
		{"describe < release", "0.3.0-5-gabc1234", "0.3.0", false},
		{"describe N ordering", "0.3.0-3-gabc1234", "0.3.0-5-gdef5678", true},
		{"describe N ordering reversed", "0.3.0-5-gdef5678", "0.3.0-3-gabc1234", false},
		{"-dirty does not outrank a clean describe of the same N", "0.3.0-5-gabc1234", "0.3.0-5-gabc1234-dirty", false},
		{"a clean describe does not outrank its own -dirty of the same N", "0.3.0-5-gabc1234-dirty", "0.3.0-5-gabc1234", false},
		{"+build metadata is ignored", "0.3.0+build1", "0.3.0+build2", false},
		{"+build metadata ignored even on a describe suffix", "0.3.0-5-gabc1234+meta1", "0.3.0-5-gabc1234+meta2", false},
		{"a release is not older than dev", "0.3.0", "dev", false},
		{"dev vs dev", "dev", "dev", false},
		{"pre-release identifier: shorter list ranks lower", "0.3.0-alpha", "0.3.0-alpha.1", true},
		{"pre-release identifier: lexical", "0.3.0-alpha", "0.3.0-beta", true},
		{"pre-release identifier: numeric ranks below alphanumeric", "0.3.0-alpha.1", "0.3.0-alpha.beta", true},
		{"pre-release identifier: numeric compares numerically, not lexically", "0.3.0-alpha.2", "0.3.0-alpha.10", true},
		{"a bare -dirty tag build outranks its own clean release", "0.3.0", "0.3.0-dirty", true},
		{"a clean release does not outrank its own dirty tag build", "0.3.0-dirty", "0.3.0", false},
		{"a real describe outranks an rc of the same base", "0.3.0-rc1", "0.3.0-5-gabc1234", true},
		{"a describe with commits outranks a bare dirty-at-tag build", "0.3.0-dirty", "0.3.0-5-gabc1234", true},
		{"a bare dirty-at-tag build does not outrank a describe with commits", "0.3.0-5-gabc1234", "0.3.0-dirty", false},
		{"a leading v is stripped defensively", "v0.3.0", "v0.3.1", true},
		{"one side spelled with v, the other without", "v0.3.0", "0.3.1", true},
		{"overflowing numeric identifier still compares as a longer number", "0.3.0-5", "0.3.0-99999999999999999999", true},
		{"overflowing numeric identifier reversed", "0.3.0-99999999999999999999", "0.3.0-5", false},
		{"two overflowing numeric identifiers of equal length compare lexically", "0.3.0-10000000000000000001", "0.3.0-10000000000000000002", true},
		// New cases.
		{"minor bump", "0.5.1", "0.6.0", true},
		{"major bump", "0.9.0", "1.0.0", true},
		{"v-prefixed release over plain current", "0.5.1", "v0.6.0", true},
		{"a dev build never has a newer release", "dev", "0.3.0", false},
		{"an unparseable candidate is never newer", "0.3.0", "latest", false},
		{"an empty candidate is never newer", "0.3.0", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Newer(c.b, c.a); got != c.wantNewer {
				t.Errorf("Newer(%q, %q) = %v, want %v", c.b, c.a, got, c.wantNewer)
			}
		})
	}
}

func TestSameMajor(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.5.1", "0.6.0", true},
		{"0.9.0", "1.0.0", false},
		{"1.2.3", "v1.9.0", true},
		{"1.0.0-rc1", "1.0.0", true},
		{"2.0.0", "1.9.9", false},
		{"dev", "0.6.0", false},
		{"0.6.0", "dev", false},
		{"dev", "dev", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := SameMajor(c.a, c.b); got != c.want {
			t.Errorf("SameMajor(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParses(t *testing.T) {
	cases := map[string]bool{
		"0.6.0":            true,
		"v0.6.0":           true,
		"0.6.0-rc1":        true,
		"0.6.0-5-gabc1234": true,
		"0.6.0+build":      true,
		"dev":              false,
		"":                 false,
		"1.2":              false,
		"latest":           false,
		"v":                false,
	}
	for v, want := range cases {
		if got := Parses(v); got != want {
			t.Errorf("Parses(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	for v, want := range map[string]bool{
		"0.6.0":        false,
		"v0.6.0":       false,
		"0.6.0-rc.1":   true,
		"v0.6.0-beta":  true,
		"0.6.0+build":  false,
		"0.6.0-3-gabc": false, // a describe suffix is a build after 0.6.0, not a pre-release
		"dev":          false,
		"":             false,
	} {
		if got := IsPrerelease(v); got != want {
			t.Errorf("IsPrerelease(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestListAndWhatsNew(t *testing.T) {
	const body = `[
	 {"tag_name":"v0.9.2","html_url":"https://github.com/Acme/chottag/releases/tag/v0.9.2","body":"Lead two.\n\nMore.","draft":false,"prerelease":false},
	 {"tag_name":"v1.0.0-rc.1","html_url":"https://github.com/Acme/chottag/releases/tag/rc","body":"RC","prerelease":true},
	 {"tag_name":"v0.9.3","html_url":"https://github.com/Acme/chottag/releases/tag/v0.9.3","body":"Draft","draft":true},
	 {"tag_name":"v0.9.1","html_url":"https://github.com/Acme/chottag/releases/tag/v0.9.1","body":"Lead\r\none \u001b[31mred\u001b[0m\u0007 and\ttab.\r\n\r\nSecond.","draft":false},
	 {"tag_name":"v0.9.0","html_url":"https://github.com/Acme/chottag/releases/tag/v0.9.0","body":"Old"},
	 {"tag_name":"","body":"no tag"}
	]`
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	rels, err := List(context.Background(), srv.Client(), srv.URL, "Acme/chottag")
	if err != nil {
		t.Fatal(err)
	}
	if gotURI != "/repos/Acme/chottag/releases?per_page=20" {
		t.Errorf("uri = %q", gotURI)
	}
	if len(rels) != 5 {
		t.Fatalf("got %d releases, want 5 (the one without a tag skipped)", len(rels))
	}
	notes := WhatsNew(rels, "0.9.0", "")
	if len(notes) != 2 || notes[0].Version != "0.9.2" || notes[1].Version != "0.9.1" {
		t.Fatalf("notes = %+v", notes)
	}
	if notes[1].Summary != "Lead one red and tab." || notes[1].URL != "https://github.com/Acme/chottag/releases/tag/v0.9.1" {
		t.Errorf("note = %+v", notes[1])
	}
	if got := WhatsNew(rels, "0.9.0", "0.9.1"); len(got) != 1 || got[0].Version != "0.9.1" {
		t.Errorf("upTo 0.9.1: %+v", got)
	}
	if got := WhatsNew(rels, "dev", ""); len(got) != 0 {
		t.Errorf("a dev build has no newer release: %+v", got)
	}
}

func TestWhatsNewKeepsFiveNewestFirst(t *testing.T) {
	var rels []Release
	for _, v := range []string{"0.9.1", "0.9.10", "0.9.2", "0.9.3", "0.9.4", "0.9.5", "0.9.6", "0.9.7"} {
		rels = append(rels, Release{Tag: "v" + v, Version: v, Body: "x"})
	}
	notes := WhatsNew(rels, "0.9.0", "")
	var got []string
	for _, n := range notes {
		got = append(got, n.Version)
	}
	if strings.Join(got, " ") != "0.9.10 0.9.7 0.9.6 0.9.5 0.9.4" {
		t.Errorf("got %v", got)
	}
}

func TestListErrors(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"json":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) },
		"big":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(make([]byte, 5<<20)) },
	} {
		srv := httptest.NewServer(h)
		if _, err := List(context.Background(), srv.Client(), srv.URL, "A/b"); err == nil {
			t.Errorf("%s: want an error", name)
		}
		srv.Close()
	}
}

func TestLead(t *testing.T) {
	if got := Lead("\n\n  First line\nsecond line\n\nNext para"); got != "First line second line" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 450)
	got := Lead(long)
	if r := []rune(got); len(r) != SummaryMax+1 || r[len(r)-1] != '…' {
		t.Errorf("cut to %d runes: %q...", len(r), string(r[:5]))
	}
	if got := Lead("a\x00b\x1b]0;title\x07c\u202e"); strings.ContainsAny(got, "\x00\x1b\x07") || strings.Contains(got, "title") {
		t.Errorf("not cleaned: %q", got)
	}
	// Bidi overrides and isolates, zero-width characters and the BOM are format characters.
	if got := Lead("a\u202ab\u202ec\u2066d\u2069e\u200bf\u200fg\ufeffh"); got != "abcdefgh" {
		t.Errorf("format characters kept: %q", got)
	}
	if Lead("") != "" {
		t.Error("empty body")
	}
}

func TestWhatsNewCleansVersionAndChecksTheURL(t *testing.T) {
	rels := []Release{
		{Tag: "v0.9.2+a\u202eb", Version: "0.9.2+a\u202eb", Body: "x", URL: "https://github.com/Acme/chottag/releases/tag/v0.9.2"},
		{Tag: "v0.9.1", Version: "0.9.1", Body: "x", URL: "javascript:alert(1)"},
		{Tag: "v0.9.3", Version: "0.9.3", Body: "x", URL: "https://evil.example.com/x"},
	}
	notes := WhatsNew(rels, "0.9.0", "")
	if len(notes) != 3 {
		t.Fatalf("notes = %+v", notes)
	}
	for _, n := range notes {
		if strings.ContainsRune(n.Version, '\u202e') {
			t.Errorf("version not cleaned: %q", n.Version)
		}
		if n.Version != "0.9.2+ab" && n.URL != "" {
			t.Errorf("a non-github.com URL kept: %+v", n)
		}
	}
	if notes[1].URL == "" {
		t.Errorf("a github.com URL dropped: %+v", notes)
	}
}
