package tracesum_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
	"github.com/HaiNNT/c-hottag/internal/tracesum"
)

func TestSummarize(t *testing.T) {
	envHash := tracelog.HashID("env_01ABCDEFGH12345678")
	recs := []tracelog.Record{
		{Kind: "mark", Mark: "rc-server"},
		{Kind: "req", Form: "absolute", Method: "POST", Host: "api.anthropic.com", Path: "/v1/environments/bridge", Class: "remote", Auth: "oauth-access", Status: 200,
			RespShape: map[string]any{"environment_id": "id:" + envHash, "nested": map[string]any{"x": "string"}}},
		{Kind: "req", Form: "absolute", Method: "GET", Host: "api.anthropic.com", Path: "/v1/environments/{id}/work/poll", PathIDs: []string{envHash}, Class: "untouched", Auth: "oauth-access", Status: 200},
		{Kind: "req", Form: "absolute", Method: "GET", Host: "api.anthropic.com", Path: "/v1/environments/{id}/work/poll", PathIDs: []string{envHash}, Class: "untouched", Auth: "jwt", Status: 204},
		{Kind: "tunnel", Form: "blind", Host: "statsig.example:443", Status: 200},
		{Kind: "tunnel", Form: "mitm", Host: "api.anthropic.com:443", Err: "client TLS handshake: bad certificate"},
	}
	s := tracesum.Summarize(recs)
	if len(s.Routes) != 2 {
		t.Fatalf("routes %+v", s.Routes)
	}
	var poll tracesum.Route
	for _, r := range s.Routes {
		if strings.HasSuffix(r.Path, "/work/poll") {
			poll = r
		}
	}
	if poll.Count != 2 || !reflect.DeepEqual(poll.Statuses, []int{200, 204}) || !reflect.DeepEqual(poll.Auth, []string{"jwt", "oauth-access"}) || poll.FirstMark != "rc-server" {
		t.Fatalf("poll %+v", poll)
	}
	want := tracesum.Link{From: "POST api.anthropic.com /v1/environments/bridge", Field: ".environment_id", To: "GET api.anthropic.com /v1/environments/{id}/work/poll"}
	if len(s.Links) != 1 || s.Links[0] != want {
		t.Fatalf("links %+v", s.Links)
	}
	if len(s.Tunnels) != 2 || !reflect.DeepEqual(s.Marks, []string{"rc-server"}) {
		t.Fatalf("tunnels %+v marks %v", s.Tunnels, s.Marks)
	}
	var buf bytes.Buffer
	if err := s.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Routes", "/v1/environments/bridge", "## Tunnels", "statsig.example:443", "## ID links", ".environment_id", "bad certificate"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("text missing %q:\n%s", want, buf.String())
		}
	}
}

// TestSummarizeFirstMarkUsesStartTime pins a long-lived request (e.g. an SSE
// stream or RC long-poll) to the mark that was current when the request
// *started*, not the mark current when its record was *written* (which
// happens at request end, and so can trail later marks in the log). Here the
// request's T is between marks "a" and "b", but it is appended to the log
// (slice order) only after mark "b" — the way a long-running request would be
// written after later, faster requests finish.
func TestSummarizeFirstMarkUsesStartTime(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := []tracelog.Record{
		{Kind: "mark", Mark: "a", T: t0},
		{Kind: "mark", Mark: "b", T: t0.Add(2 * time.Second)},
		{Kind: "req", Form: "absolute", Method: "GET", Host: "api.anthropic.com", Path: "/v1/thing", Class: "untouched", Auth: "oauth-access", Status: 200, T: t0.Add(1 * time.Second)},
	}
	s := tracesum.Summarize(recs)
	if len(s.Routes) != 1 || s.Routes[0].FirstMark != "a" {
		t.Fatalf("routes %+v, want FirstMark=a", s.Routes)
	}
}

// TestSummarizeDeterministicOrder pins the ordering of Routes and Tunnels
// entries that share every sort key used before the tie-break fields (Host,
// Path, Method for routes; Host for tunnels), so a route table diffed across
// versions never shows a line-order-only difference.
func TestSummarizeDeterministicOrder(t *testing.T) {
	recs := []tracelog.Record{
		{Kind: "req", Form: "absolute", Method: "GET", Host: "api.anthropic.com", Path: "/v1/thing", Class: "remote", Auth: "oauth-access", Status: 200},
		{Kind: "req", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: "/v1/thing", Class: "untouched", Auth: "oauth-access", Status: 200},
		{Kind: "tunnel", Form: "blind", Host: "statsig.example:443", Status: 200},
		{Kind: "tunnel", Form: "mitm", Host: "statsig.example:443", Status: 200},
	}
	wantRoutes := []struct{ Class, Form string }{
		{"remote", "absolute"},
		{"untouched", "mitm"},
	}
	wantTunnels := []struct{ Form string }{
		{"blind"},
		{"mitm"},
	}
	for i := 0; i < 50; i++ {
		s := tracesum.Summarize(recs)
		if len(s.Routes) != len(wantRoutes) {
			t.Fatalf("run %d: routes %+v", i, s.Routes)
		}
		for j, r := range s.Routes {
			if r.Class != wantRoutes[j].Class || r.Form != wantRoutes[j].Form {
				t.Fatalf("run %d: routes order %+v", i, s.Routes)
			}
		}
		if len(s.Tunnels) != len(wantTunnels) {
			t.Fatalf("run %d: tunnels %+v", i, s.Tunnels)
		}
		for j, tn := range s.Tunnels {
			if tn.Form != wantTunnels[j].Form {
				t.Fatalf("run %d: tunnels order %+v", i, s.Tunnels)
			}
		}
	}
}

func TestSummarizeLinksIDsThatDifferOnlyByPrefix(t *testing.T) {
	cse, sess := tracelog.HashID("cse_01ABCDEFGH12345678"), tracelog.HashID("session_01ABCDEFGH12345678")
	recs := []tracelog.Record{
		{Kind: "req", Form: "mitm", Method: "POST", Host: "api.anthropic.com", Path: "/v1/code/sessions", Class: "remote", Status: 200,
			RespShape: map[string]any{"session": map[string]any{"id": "id:" + cse}}},
		{Kind: "req", Form: "mitm", Method: "POST", Host: "api.anthropic.com", Path: "/v1/sessions/{id}/archive", PathIDs: []string{sess}, Class: "serving", Status: 200},
	}
	s := tracesum.Summarize(recs)
	want := tracesum.Link{From: "POST api.anthropic.com /v1/code/sessions", Field: ".session.id", To: "POST api.anthropic.com /v1/sessions/{id}/archive", Note: "cse_->session_"}
	if len(s.Links) != 1 || s.Links[0] != want {
		t.Fatalf("links %+v", s.Links)
	}
	var buf bytes.Buffer
	s.WriteText(&buf)
	if !strings.Contains(buf.String(), "(id prefix cse_->session_)") {
		t.Fatalf("text missing prefix note:\n%s", buf.String())
	}
}

func TestSummarizeLinksUnprefixedPathIDToPrefixedResponseID(t *testing.T) {
	cse := tracelog.HashID("cse_01ABCDEFGH12345678")
	bare := tracelog.HashID("01ABCDEFGH12345678")
	recs := []tracelog.Record{
		{Kind: "req", Form: "mitm", Method: "POST", Host: "api.anthropic.com", Path: "/v1/code/sessions", Class: "remote", Status: 200,
			RespShape: map[string]any{"session": map[string]any{"id": "id:" + cse}}},
		{Kind: "req", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: "/v1/sessions/{id}", PathIDs: []string{bare}, Class: "serving", Status: 200},
	}
	s := tracesum.Summarize(recs)
	want := tracesum.Link{From: "POST api.anthropic.com /v1/code/sessions", Field: ".session.id", To: "GET api.anthropic.com /v1/sessions/{id}", Note: "cse_->"}
	if len(s.Links) != 1 || s.Links[0] != want {
		t.Fatalf("links %+v", s.Links)
	}
}
