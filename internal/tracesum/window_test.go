package tracesum_test

import (
	"reflect"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
	"github.com/HaiNNT/c-hottag/internal/tracesum"
)

func TestSinceLastTraceOnStartsAtTheLastMark(t *testing.T) {
	recs := []tracelog.Record{
		{Kind: "req", Path: "/a"},
		{Kind: "mark", Mark: tracesum.TraceOnMark},
		{Kind: "req", Path: "/b"},
		{Kind: "mark", Mark: tracesum.TraceOnMark},
		{Kind: "req", Path: "/c"},
		{Kind: "req", Path: "/d", Mark: tracesum.TraceOnMark}, // only a mark record counts
		{Kind: "mark", Mark: "trace on later"},                // the exact text only
	}
	if got := tracesum.SinceLastTraceOn(recs); !reflect.DeepEqual(got, recs[3:]) {
		t.Fatalf("window = %+v, want from the last trace-on mark (index 3) on, mark included", got)
	}
}

func TestSinceLastTraceOnWithNoMarkIsTheWholeLog(t *testing.T) {
	recs := []tracelog.Record{{Kind: "mark", Mark: "open usage"}, {Kind: "req", Path: "/a"}}
	if got := tracesum.SinceLastTraceOn(recs); !reflect.DeepEqual(got, recs) {
		t.Fatalf("window = %+v, want the whole log (a `trace run` log has no trace-on mark)", got)
	}
	if got := tracesum.SinceLastTraceOn(nil); len(got) != 0 {
		t.Fatalf("window of nil = %+v", got)
	}
}
