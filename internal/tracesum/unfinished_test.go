package tracesum_test

import (
	"bytes"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
	"github.com/HaiNNT/c-hottag/internal/tracesum"
)

// streamLog is a log with one finished stream (a1), three open ones (b2 and
// c3 on /v1/messages, d4 on a session events route) and one non-stream
// request with no id.
func streamLog() []tracelog.Record {
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	msg := func(kind, id string, sec int) tracelog.Record {
		return tracelog.Record{T: t0.Add(time.Duration(sec) * time.Second), Kind: kind, ID: id, Form: "mitm", Method: "POST",
			Host: "api.anthropic.com", Path: "/v1/messages", Class: "serving", Auth: "oauth-access", Status: 200, RespType: "text/event-stream"}
	}
	return []tracelog.Record{
		msg("head", "a1a1a1a1", 1),
		msg("head", "b2b2b2b2", 2),
		{T: t0.Add(3 * time.Second), Kind: "req", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: "/v1/models", Class: "untouched", Auth: "oauth-access", Status: 200},
		msg("req", "a1a1a1a1", 1),
		msg("head", "c3c3c3c3", 4),
		{T: t0.Add(5 * time.Second), Kind: "head", ID: "d4d4d4d4", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: "/v1/sessions/{id}/events", Class: "remote", Auth: "oauth-access", Status: 200, RespType: "text/event-stream"},
	}
}

func TestSummarizeCountsOnlyReqRecords(t *testing.T) {
	s := tracesum.Summarize(streamLog())
	if len(s.Routes) != 2 {
		t.Fatalf("routes %+v, want /v1/messages and /v1/models only (a head is not a request)", s.Routes)
	}
	for _, r := range s.Routes {
		if r.Count != 1 {
			t.Errorf("route %s count %d, want 1", r.Path, r.Count)
		}
	}
}

func TestSummarizeReportsUnfinishedStreams(t *testing.T) {
	s := tracesum.Summarize(streamLog())
	want := []tracesum.Unfinished{
		{Route: "GET api.anthropic.com /v1/sessions/{id}/events", Count: 1},
		{Route: "POST api.anthropic.com /v1/messages", Count: 2},
	}
	if !reflect.DeepEqual(s.Unfinished, want) {
		t.Fatalf("unfinished %+v, want %+v", s.Unfinished, want)
	}
	var buf bytes.Buffer
	if err := s.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"## Unfinished streams", "unfinished streams: 3"} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("text missing %q:\n%s", w, buf.String())
		}
	}
	// tabwriter pads the route column, so match the count after any spaces.
	if !regexp.MustCompile(`(?m)^POST api\.anthropic\.com /v1/messages +2$`).MatchString(buf.String()) {
		t.Errorf("text missing the /v1/messages row with count 2:\n%s", buf.String())
	}
}

// TestSummarizePairsByCountPerID: two streams that drew the same random id,
// one finished, leave exactly one unfinished.
func TestSummarizePairsByCountPerID(t *testing.T) {
	h := tracelog.Record{Kind: "head", ID: "0a1b2c3d", Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages"}
	r := h
	r.Kind = "req"
	s := tracesum.Summarize([]tracelog.Record{h, h, r})
	want := []tracesum.Unfinished{{Route: "POST api.anthropic.com /v1/messages", Count: 1}}
	if !reflect.DeepEqual(s.Unfinished, want) {
		t.Fatalf("unfinished %+v, want %+v", s.Unfinished, want)
	}
}

func TestSummarizeOldLogHasNoUnfinishedStreams(t *testing.T) {
	s := tracesum.Summarize([]tracelog.Record{
		{Kind: "req", Form: "mitm", Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages", Status: 200, RespType: "text/event-stream"},
	})
	if s.Unfinished != nil {
		t.Fatalf("unfinished %+v, want none", s.Unfinished)
	}
	var buf bytes.Buffer
	s.WriteText(&buf)
	if !strings.Contains(buf.String(), "unfinished streams: 0") {
		t.Fatalf("text missing the zero count:\n%s", buf.String())
	}
}
