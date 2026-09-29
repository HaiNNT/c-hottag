package notify

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// QueueSize is how many notices may wait behind the one being sent.
const QueueSize = 8

// SendTimeout bounds one send. osascript normally returns well inside a
// second; a hung one is killed here.
const SendTimeout = 5 * time.Second

type notice struct{ title, body string }

// Dispatcher delivers notices on its own goroutine, one at a time, in the
// order they were enqueued.
//
// Every notice handed to Enqueue is either delivered or counted in Errors
// exactly once: a full queue, a failed send, a timed-out send, or a notice
// still queued or in flight at Close. Nothing is retried (M2 spec §4).
type Dispatcher struct {
	n       Notifier
	timeout time.Duration
	queue   chan notice
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	errs    atomic.Uint64

	// mu orders Enqueue's send on queue against Close's close(queue).
	mu     sync.Mutex
	closed bool
}

// NewDispatcher starts the sender. timeout bounds each send; a non-positive
// timeout defaults to SendTimeout (a zero timeout would otherwise expire
// before the notifier ever ran).
func NewDispatcher(n Notifier, timeout time.Duration) *Dispatcher {
	if timeout <= 0 {
		timeout = SendTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		n:       n,
		timeout: timeout,
		queue:   make(chan notice, QueueSize),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go d.loop()
	return d
}

func (d *Dispatcher) loop() {
	defer close(d.done)
	for m := range d.queue {
		if d.ctx.Err() != nil {
			d.errs.Add(1) // Close came before its turn: not delivered
			continue
		}
		ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
		err := d.n.Send(ctx, m.title, m.body)
		cancel()
		if err != nil {
			d.errs.Add(1)
		}
	}
}

// Enqueue hands a notice to the sender and returns at once, so it is safe
// on a request goroutine. A full queue drops the notice and counts it.
// After Close it does nothing.
func (d *Dispatcher) Enqueue(title, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	select {
	case d.queue <- notice{title, body}:
	default:
		d.errs.Add(1)
	}
}

// Errors reports how many notices were not delivered.
func (d *Dispatcher) Errors() uint64 { return d.errs.Load() }

// Close cancels the send in flight, counts what is still queued, and waits
// for the sender to stop. It is safe to call more than once.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	d.cancel()
	<-d.done
}
