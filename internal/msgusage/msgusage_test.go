package msgusage

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

const startEv = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":2,"cache_creation_input_tokens":36230,"cache_read_input_tokens":28263,"output_tokens":1,"cache_creation":{"ephemeral_1h_input_tokens":36230,"ephemeral_5m_input_tokens":0}}}}

`

const deltaEv = `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":84}}

event: message_stop
data: {"type":"message_stop"}

`

const stopOnly = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func contentEv(text string) string {
	return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n"
}

func stream(text string) string {
	return startEv + "event: ping\ndata: {\"type\": \"ping\"}\n\n" + contentEv(text) + deltaEv
}

var want = Usage{ID: "msg_01", Model: "claude-opus-5-5", In: 2, Out: 84, CR: 28263, CW1h: 36230}

type result struct {
	n  int
	u  Usage
	ok bool
}

func run(t *testing.T, in string, sse bool, r func(io.Reader) io.Reader) (string, result) {
	t.Helper()
	var res result
	var src io.Reader = strings.NewReader(in)
	if r != nil {
		src = r(src)
	}
	tee := Tee(io.NopCloser(src), sse, func(u Usage, ok bool) { res.n++; res.u, res.ok = u, ok })
	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatal(err)
	}
	_ = tee.Close()
	return string(out), res
}

type oneByte struct{ r io.Reader }

func (o oneByte) Read(p []byte) (int, error) { return o.r.Read(p[:1]) }

func TestSSEUsage(t *testing.T) {
	in := stream("hello")
	out, res := run(t, in, true, nil)
	if out != in {
		t.Fatal("bytes differ")
	}
	if !res.ok || res.u != want || res.n != 1 {
		t.Fatalf("got %+v", res)
	}
}

func TestSSECRLF(t *testing.T) {
	in := strings.ReplaceAll(stream("hello"), "\n", "\r\n")
	out, res := run(t, in, true, nil)
	if out != in || !res.ok || res.u != want {
		t.Fatalf("got %+v", res)
	}
}

func TestSSEHugeDeltaNotBuffered(t *testing.T) {
	in := stream(strings.Repeat("x", 5<<20))
	var res result
	tee := Tee(io.NopCloser(strings.NewReader(in)), true, func(u Usage, ok bool) { res.u, res.ok = u, ok })
	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatal("bytes differ")
	}
	if hw := tee.(*teeReader).highWater(); hw > MaxEventBytes {
		t.Fatalf("high water %d", hw)
	}
	if !res.ok || res.u != want {
		t.Fatalf("got %+v", res)
	}
}

func TestSSEHugeDeltaOneByteChunks(t *testing.T) {
	in := stream(strings.Repeat("x", 200<<10))
	var res result
	tee := Tee(io.NopCloser(oneByte{strings.NewReader(in)}), true, func(u Usage, ok bool) { res.u, res.ok = u, ok })
	out, _ := io.ReadAll(tee)
	if string(out) != in || !res.ok || res.u != want {
		t.Fatalf("got %+v", res)
	}
	if hw := tee.(*teeReader).highWater(); hw > MaxEventBytes {
		t.Fatalf("high water %d", hw)
	}
}

func TestSSECancelledAfterStart(t *testing.T) {
	var res result
	tee := Tee(io.NopCloser(strings.NewReader(startEv)), true, func(u Usage, ok bool) { res.n++; res.u, res.ok = u, ok })
	buf := make([]byte, len(startEv))
	if _, err := io.ReadFull(tee, buf); err != nil {
		t.Fatal(err)
	}
	if res.n != 0 {
		t.Fatal("done too early")
	}
	_ = tee.Close()
	_ = tee.Close()
	if res.n != 1 || !res.ok || res.u.Out != 1 || res.u.In != 2 {
		t.Fatalf("got %+v", res)
	}
}

func TestSSEChunkBoundaries(t *testing.T) {
	in := stream("hello")
	out, res := run(t, in, true, func(r io.Reader) io.Reader { return oneByte{r} })
	if out != in || !res.ok || res.u != want {
		t.Fatalf("got %+v", res)
	}
}

func TestSSEDeltaOverridesInputFields(t *testing.T) {
	in := startEv + "event: message_delta\ndata: {\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":7,\"output_tokens\":5,\"speed\":\"fast\",\"server_tool_use\":{\"web_search_requests\":3}}}\n\n"
	_, res := run(t, in, true, nil)
	u := res.u
	if u.In != 9 || u.CR != 7 || u.Out != 5 || u.Speed != "fast" || u.Web != 3 || u.CW1h != 36230 {
		t.Fatalf("got %+v", u)
	}
}

func TestDeltaRepeatingFlatTotalDoesNotDoubleCount(t *testing.T) {
	in := startEv + "event: message_delta\ndata: {\"usage\":{\"input_tokens\":2,\"cache_creation_input_tokens\":36230,\"cache_read_input_tokens\":28263,\"output_tokens\":84}}\n\n"
	_, res := run(t, in, true, nil)
	if res.u.CW5m != 0 || res.u.CW1h != 36230 || res.u.Out != 84 {
		t.Fatalf("got %+v", res.u)
	}
}

func TestCacheCreationFallbackIs5m(t *testing.T) {
	in := "event: message_start\ndata: {\"message\":{\"id\":\"m\",\"model\":\"x\",\"usage\":{\"cache_creation_input_tokens\":11}}}\n\n"
	_, res := run(t, in, true, nil)
	if res.u.CW5m != 11 || res.u.CW1h != 0 {
		t.Fatalf("got %+v", res.u)
	}
}

func TestJSONUsage(t *testing.T) {
	in := `{"id":"msg_9","type":"message","model":"claude-haiku","content":[{"type":"text","text":"SECRET"}],"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":6,"speed":"standard"}}`
	out, res := run(t, in, false, nil)
	if out != in || !res.ok {
		t.Fatalf("got %+v", res)
	}
	w := Usage{ID: "msg_9", Model: "claude-haiku", In: 3, Out: 4, CR: 5, CW5m: 6, Speed: "standard"}
	if res.u != w {
		t.Fatalf("got %+v", res.u)
	}
}

func TestNoUsage(t *testing.T) {
	_, res := run(t, contentEv("hi")+stopOnly, true, nil)
	if res.ok || res.n != 1 {
		t.Fatalf("got %+v", res)
	}
	_, res = run(t, `{"error":{"type":"x"}}`, false, nil)
	if res.ok {
		t.Fatalf("got %+v", res)
	}
}

func TestDoneOnce(t *testing.T) {
	_, res := run(t, stream("a"), true, nil)
	if res.n != 1 {
		t.Fatalf("n=%d", res.n)
	}
}

func TestDonePanicRecovered(t *testing.T) {
	tee := Tee(io.NopCloser(strings.NewReader(stream("a"))), true, func(Usage, bool) { panic("boom") })
	if _, err := io.ReadAll(tee); err != nil {
		t.Fatal(err)
	}
	_ = tee.Close()
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("reset") }

func TestReadErrorFinishes(t *testing.T) {
	n := 0
	tee := Tee(io.NopCloser(io.MultiReader(strings.NewReader(startEv), errReader{})), true, func(u Usage, ok bool) { n++ })
	_, err := io.ReadAll(tee)
	if err == nil {
		t.Fatal("want error passthrough")
	}
	_ = tee.Close()
	if n != 1 {
		t.Fatalf("n=%d", n)
	}
}

func TestNoTextRetained(t *testing.T) {
	in := stream("SECRET-CANARY-7f3")
	_, res := run(t, in, true, nil)
	if strings.Contains(fmt.Sprintf("%+v", res.u), "SECRET-CANARY-7f3") {
		t.Fatal("text leaked")
	}
	jin := `{"id":"a","model":"b","content":[{"text":"SECRET-CANARY-7f3"}],"usage":{"input_tokens":1}}`
	_, res = run(t, jin, false, nil)
	if strings.Contains(fmt.Sprintf("%+v", res.u), "SECRET-CANARY-7f3") {
		t.Fatal("text leaked")
	}
}

func TestJSONBounded(t *testing.T) {
	in := `{"content":"` + strings.Repeat("y", 2<<20) + `","usage":{"input_tokens":1}}`
	tee := Tee(io.NopCloser(strings.NewReader(in)), false, func(Usage, bool) {})
	out, _ := io.ReadAll(tee)
	if !bytes.Equal(out, []byte(in)) {
		t.Fatal("bytes differ")
	}
	if hw := tee.(*teeReader).highWater(); hw > MaxJSONBytes {
		t.Fatalf("high water %d", hw)
	}
}
