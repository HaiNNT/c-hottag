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
