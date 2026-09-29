package cli

import (
	"path/filepath"
	"time"

	"github.com/HaiNNT/c-hottag/internal/proxy"
	"github.com/HaiNNT/c-hottag/internal/rotate"
	"github.com/HaiNNT/c-hottag/internal/store"
	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

// The daemon's trace.jsonl bound (M2c spec §3, the F68/F75 lesson): a log
// whose writer lives as long as the daemon must be bounded. Smaller than
// proxy.jsonl's keep (5) because a trace is read once, right after it.
const (
	daemonTraceMaxBytes = 8 << 20
	daemonTraceKeep     = 2
)

// lazyTraceLog opens <home>/trace.jsonl at the first traced record, so a
// daemon that never traces never creates it. It sits under a
// tracelog.Writer, which serialises every Write and Close under its own
// mutex, so it needs no lock of its own. A failed open is returned for that
// record and retried on the next one.
type lazyTraceLog struct {
	cfg    rotate.Config
	w      *rotate.Writer
	closed bool
}

func (l *lazyTraceLog) Write(p []byte) (int, error) {
	if l.closed {
		return 0, rotate.ErrClosed
	}
	if l.w == nil {
		w, err := rotate.Open(l.cfg)
		if err != nil {
			return 0, err
		}
		l.w = w
	}
	return l.w.Write(p)
}

// Close is idempotent, and creates nothing when nothing was written.
func (l *lazyTraceLog) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	if l.w == nil {
		return nil
	}
	return l.w.Close()
}

// daemonTrace is the daemon's side of trace mode (M2c spec §3): the switch
// in state.json read per request, the bounded trace log, and the version
// seen, stamped into status.json.
type daemonTrace struct {
	state func() (store.State, error)
	sink  *statusSink
	now   func() time.Time
	log   *tracelog.Writer
}

// newDaemonTrace is a package var, like newDaemonPoller and newStatusSinkFn,
// so a test can wrap it to observe the *daemonTrace runProxyWithSignal
// actually builds and wires in (T5 M3). Production never overrides it.
var newDaemonTrace = func(home string, state func() (store.State, error), sink *statusSink) *daemonTrace {
	return &daemonTrace{
		state: state,
		sink:  sink,
		now:   time.Now,
		log: tracelog.NewWriter(&lazyTraceLog{cfg: rotate.Config{
			Path:     filepath.Join(home, "trace.jsonl"),
			MaxBytes: daemonTraceMaxBytes,
			Keep:     daemonTraceKeep,
		}}),
	}
}

// tracing is proxy.Config.Tracing: true while state.json's window is open.
// store.Cache re-reads state.json only when it changed, so `trace on|off`
// applies to the next request with no restart. An unreadable state.json
// reads as off: tracing costs a body read on every request (plan ruling 6).
func (dt *daemonTrace) tracing() bool {
	st, err := dt.state()
	return err == nil && st.TracingAt(dt.now())
}

// onClaudeVersion is proxy.Config.OnClaudeVersion.
func (dt *daemonTrace) onClaudeVersion(version string) {
	dt.sink.noteTracedVersion(version, dt.now())
}

// wire returns cfg with the three trace fields set. runProxyWithSignal's
// only call to it is `cfg = dt.wire(cfg)`.
func (dt *daemonTrace) wire(cfg proxy.Config) proxy.Config {
	cfg.Tracing = dt.tracing
	cfg.TraceLog = dt.log
	cfg.OnClaudeVersion = dt.onClaudeVersion
	return cfg
}

// Close closes the trace log. A record written after it fails with
// rotate.ErrClosed, which reaches the throttled "trace log write failed"
// line, never the request.
func (dt *daemonTrace) Close() error { return dt.log.Close() }

// noteTracedVersion stamps status.json's trace.lastTraced (spec §3, ruling
// T7). It queues a write only when SetLastTraced reports a change: the first
// traced request of a version, or a different version. So a traced session
// costs one write per version, not one per request. Like setDaemon, it
// queues directly, because the change check is its own rate limit.
func (c *statusSink) noteTracedVersion(version string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file.SetLastTraced(version, at) {
		c.queueLocked()
	}
}
