package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunPrintsOneVerdictPerPath(t *testing.T) {
	in := "/api/frame/deploy/prepare\n\n/api/not-a-route\n/v1/messages\n/api/oauth/validate\n/api/hello\n"
	var out bytes.Buffer
	if err := run(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	want := "explicit\ndefault\nexplicit\nexplicit\ndefault\n"
	if got := out.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCheckAnyMethodAndForm(t *testing.T) {
	// POST-only and absolute-form-only rules still count as explicit.
	if got := check("/api/oauth/validate"); got != "explicit" {
		t.Errorf("validate = %s", got)
	}
	if got := check("/v1/code/sessions/{id}/events"); got != "explicit" {
		t.Errorf("session events = %s", got)
	}
}
