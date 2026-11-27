// Package msgusage reads token usage out of a /v1/messages response while the
// body passes through to the caller unchanged. It keeps only the message id,
// the model, the usage numbers and usage.speed: never any content text. Its
// memory is bounded: an SSE event larger than MaxEventBytes is skipped, and a
// non-stream body is inspected only up to MaxJSONBytes.
package msgusage

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// MaxEventBytes is the most SSE event data held at once; a larger event is
// skipped.
const MaxEventBytes = 64 << 10

// MaxJSONBytes is the most of a non-SSE body that is inspected.
const MaxJSONBytes = 1 << 20

// Usage is what a response reported. It holds no content text.
type Usage struct {
	ID, Model, Speed string
	In, Out, CR      int64
	CW5m, CW1h, Web  int64
}

// Tee returns a ReadCloser that passes body through unchanged and, once (at
// EOF, a read error, or Close, whichever first), calls done with the usage it
// saw. ok is false when no message_start / top-level usage was seen. done runs
// on the goroutine that triggered it; a panic in done is recovered and dropped.
func Tee(body io.ReadCloser, sse bool, done func(u Usage, ok bool)) io.ReadCloser {
	return &teeReader{body: body, sse: sse, done: done}
}

type teeReader struct {
	body io.ReadCloser
	sse  bool
	done func(Usage, bool)

	mu       sync.Mutex
	finished bool
	u        Usage
	ok       bool
	hw       int

	sawBreakdown bool // a cache_creation object has been applied

	// JSON mode.
	buf []byte

	// SSE mode.
	carry    []byte // partial line carried across reads
	skipLine bool   // discarding the rest of an oversize line
	event    string
	data     []byte
	dataOver bool
}

type usageJSON struct {
	In            *int64  `json:"input_tokens"`
	Out           *int64  `json:"output_tokens"`
	CR            *int64  `json:"cache_read_input_tokens"`
	CW            *int64  `json:"cache_creation_input_tokens"`
	Speed         *string `json:"speed"`
	CacheCreation *struct {
		M5 *int64 `json:"ephemeral_5m_input_tokens"`
		H1 *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Server *struct {
		Web *int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

func set(dst *int64, v *int64) {
	if v != nil {
		*dst = *v
	}
}

// apply merges a usage object: any field present overrides.
func (t *teeReader) apply(x *usageJSON) {
	set(&t.u.In, x.In)
	set(&t.u.Out, x.Out)
	set(&t.u.CR, x.CR)
	if x.CacheCreation != nil {
		set(&t.u.CW5m, x.CacheCreation.M5)
		set(&t.u.CW1h, x.CacheCreation.H1)
		t.sawBreakdown = true
	} else if !t.sawBreakdown {
		// A flat total counts as 5m only until a breakdown has been seen: a
		// message_delta repeats the total without the breakdown.
		set(&t.u.CW5m, x.CW)
	}
	if x.Server != nil {
		set(&t.u.Web, x.Server.Web)
	}
	if x.Speed != nil {
		t.u.Speed = *x.Speed
	}
	t.ok = true
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.body.Read(p)
	t.mu.Lock()
	if n > 0 && !t.finished {
		if t.sse {
			t.scanSSE(p[:n])
		} else {
			t.scanJSON(p[:n])
		}
	}
	if err != nil {
		t.finish()
	}
	t.mu.Unlock()
	return n, err
}

func (t *teeReader) Close() error {
	t.mu.Lock()
	t.finish()
	t.mu.Unlock()
	return t.body.Close()
}

func (t *teeReader) highWater() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.hw
}

func (t *teeReader) note(n int) {
	if n > t.hw {
		t.hw = n
	}
}

// finish reports once. The caller holds t.mu.
func (t *teeReader) finish() {
	if t.finished {
		return
	}
	t.finished = true
	if !t.sse {
		t.parseJSON()
	}
	t.buf, t.carry, t.data = nil, nil, nil
	u, ok := t.u, t.ok
	if t.done == nil {
		return
	}
	defer func() { _ = recover() }()
	t.done(u, ok)
}

func (t *teeReader) scanJSON(b []byte) {
	room := MaxJSONBytes - len(t.buf)
	if room <= 0 {
		return
	}
	if len(b) > room {
		b = b[:room]
	}
	t.buf = append(t.buf, b...)
	t.note(len(t.buf))
}

func (t *teeReader) parseJSON() {
	var m struct {
		ID    string     `json:"id"`
		Model string     `json:"model"`
		Usage *usageJSON `json:"usage"`
	}
	if json.Unmarshal(t.buf, &m) != nil || m.Usage == nil {
		return
	}
	t.u.ID, t.u.Model = m.ID, m.Model
	t.apply(m.Usage)
}

func (t *teeReader) relevant() bool {
	return t.event == "message_start" || t.event == "message_delta"
}

func (t *teeReader) scanSSE(p []byte) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.carryPartial(p)
			return
		}
		if t.skipLine {
			t.skipLine = false
		} else if len(t.carry) == 0 {
			t.line(p[:i])
		} else {
			t.carryPartial(p[:i])
			if !t.skipLine {
				t.line(t.carry)
			}
			t.skipLine = false
		}
		t.carry = t.carry[:0]
		p = p[i+1:]
	}
}

// carryPartial holds the start of a line that continues in the next read. A
// line that would push the held bytes past MaxEventBytes, or a data line of an
// event that is not parsed, is dropped up to its newline instead.
func (t *teeReader) carryPartial(b []byte) {
	if t.skipLine {
		return
	}
	if len(t.carry)+len(b)+len(t.data) > MaxEventBytes {
		t.dropLine(true)
		return
	}
	t.carry = append(t.carry, b...)
	t.note(len(t.carry) + len(t.data))
	if !t.relevant() && bytes.HasPrefix(t.carry, []byte("data:")) {
		t.dropLine(false)
	}
}

func (t *teeReader) dropLine(over bool) {
	t.carry = t.carry[:0]
	t.skipLine = true
	if over {
		t.dataOver = true
		t.data = t.data[:0]
	}
}

// line handles one complete line without its newline.
func (t *teeReader) line(l []byte) {
	l = bytes.TrimSuffix(l, []byte("\r"))
	switch {
	case len(l) == 0:
		t.dispatch()
	case bytes.HasPrefix(l, []byte("event:")):
		v := bytes.TrimPrefix(bytes.TrimPrefix(l, []byte("event:")), []byte(" "))
		if len(v) > 32 {
			v = nil
		}
		t.event = string(v)
	case bytes.HasPrefix(l, []byte("data:")):
		if !t.relevant() || t.dataOver {
			return
		}
		v := bytes.TrimPrefix(bytes.TrimPrefix(l, []byte("data:")), []byte(" "))
		if len(t.data)+len(v)+1 > MaxEventBytes {
			t.dataOver = true
			t.data = t.data[:0]
			return
		}
		if len(t.data) > 0 {
			t.data = append(t.data, '\n')
		}
		t.data = append(t.data, v...)
		t.note(len(t.data))
	}
}

func (t *teeReader) dispatch() {
	defer func() {
		t.event = ""
		t.data = t.data[:0]
		t.dataOver = false
	}()
	if !t.relevant() || t.dataOver || len(t.data) == 0 {
		return
	}
	switch t.event {
	case "message_start":
		var m struct {
			Message struct {
				ID    string     `json:"id"`
				Model string     `json:"model"`
				Usage *usageJSON `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(t.data, &m) != nil {
			return
		}
		t.u.ID, t.u.Model = m.Message.ID, m.Message.Model
		t.ok = true
		if m.Message.Usage != nil {
			t.apply(m.Message.Usage)
		}
	case "message_delta":
		var m struct {
			Usage *usageJSON `json:"usage"`
		}
		if json.Unmarshal(t.data, &m) != nil || m.Usage == nil {
			return
		}
		t.apply(m.Usage)
	}
}
