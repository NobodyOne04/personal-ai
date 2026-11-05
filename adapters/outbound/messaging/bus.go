// Package messaging implements ports.EventPublisher with a lightweight
// in-process event bus. No external broker is introduced without a concrete
// requirement.
//
// Delivery semantics, deliberately simple:
//   - Publish never blocks on consumers. Each subscriber has its own bounded
//     queue and its own goroutine; if a queue is full the event is dropped for
//     that subscriber only and counted (see Dropped).
//   - Events reach one subscriber in publish order.
//   - Delivery is at-most-once and in-memory only: events are lost on restart
//     and are not a source of truth. State that must survive belongs in the
//     database.
//   - A panicking handler is recovered and logged; it does not affect others.
package messaging

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"

	"personal-ai/core/ports"
)

// DefaultBuffer is the per-subscriber queue size when none is configured.
const DefaultBuffer = 64

// ErrClosed is returned by Publish and Subscribe after Close.
var ErrClosed = errors.New("event bus is closed")

// Handler receives events. It runs on the subscriber's own goroutine. The
// context is detached from the publisher's cancellation, so a finished
// request cannot cancel delivery of the events it caused.
type Handler func(ctx context.Context, e ports.Event)

type subscriber struct {
	ch      chan ports.Event
	types   map[ports.EventType]struct{} // nil means all types
	handler Handler
}

func (s *subscriber) wants(t ports.EventType) bool {
	if s.types == nil {
		return true
	}
	_, ok := s.types[t]
	return ok
}

// Bus is an in-process event bus. The zero value is not usable; call New.
type Bus struct {
	logger *slog.Logger
	buffer int

	mu     sync.RWMutex
	subs   map[uint64]*subscriber
	nextID uint64
	closed bool

	wg      sync.WaitGroup
	dropped atomic.Int64
}

var _ ports.EventPublisher = (*Bus)(nil)

// Option configures a Bus.
type Option func(*Bus)

// WithBuffer sets the per-subscriber queue size. Values below 1 are ignored.
func WithBuffer(n int) Option {
	return func(b *Bus) {
		if n >= 1 {
			b.buffer = n
		}
	}
}

// WithLogger sets the logger used for dropped events and handler panics.
func WithLogger(l *slog.Logger) Option {
	return func(b *Bus) {
		if l != nil {
			b.logger = l
		}
	}
}

// New creates a bus.
func New(opts ...Option) *Bus {
	b := &Bus{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		buffer: DefaultBuffer,
		subs:   make(map[uint64]*subscriber),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Subscribe registers a handler for the given event types, or for every type
// if none are given. It returns a function that unsubscribes; calling it more
// than once is safe. Events already queued for the subscriber are still
// delivered after unsubscribing.
func (b *Bus) Subscribe(h Handler, types ...ports.EventType) (unsubscribe func(), err error) {
	if h == nil {
		return nil, errors.New("handler is required")
	}

	sub := &subscriber{ch: make(chan ports.Event, b.buffer), handler: h}

	if len(types) > 0 {
		sub.types = make(map[ports.EventType]struct{}, len(types))
		for _, t := range types {
			sub.types[t] = struct{}{}
		}
	}

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}

	id := b.nextID
	b.nextID++
	b.subs[id] = sub
	b.wg.Add(1)
	b.mu.Unlock()

	go b.run(sub)

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			// Close may already have removed and closed it.
			if _, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(sub.ch)
			}
		})
	}, nil
}

// Publish queues the event for every interested subscriber and returns
// immediately. It never blocks on a slow consumer; see the package comment.
func (b *Bus) Publish(_ context.Context, e ports.Event) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return ErrClosed
	}
	for _, sub := range b.subs {
		if !sub.wants(e.Type) {
			continue
		}
		// Each subscriber gets its own copy of the Detail map so a handler
		// that mutates it cannot affect the others.
		ev := e
		ev.Detail = maps.Clone(e.Detail)
		select {
		case sub.ch <- ev:
		default:
			n := b.dropped.Add(1)
			b.logger.Warn("event dropped: subscriber queue full",
				"event_type", string(e.Type), "dropped_total", n)
		}
	}
	return nil
}

// Dropped returns how many deliveries have been dropped because a subscriber's
// queue was full.
func (b *Bus) Dropped() int64 { return b.dropped.Load() }

// Close stops accepting events and waits until every queued event has been
// handled. It is safe to call more than once. A handler that blocks forever
// will block Close; bound it with the caller's own timeout if needed.
func (b *Bus) Close() {
	b.mu.Lock()

	if !b.closed {
		b.closed = true
		for id, sub := range b.subs {
			delete(b.subs, id)
			close(sub.ch)
		}
	}
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *Bus) run(sub *subscriber) {
	defer b.wg.Done()
	for e := range sub.ch {
		b.deliver(sub, e)
	}
}

func (b *Bus) deliver(sub *subscriber, e ports.Event) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("event handler panicked", "event_type", string(e.Type), "panic", r)
		}
	}()
	sub.handler(context.Background(), e)
}
