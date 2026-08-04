package service

import (
	"errors"
	"sync"

	"github.com/Aznyi/HarborMaster/internal/domain"
)

// ErrTooManySubscribers reports that the SSE subscriber limit is reached.
//
// A limit exists because each subscriber costs a buffered channel and a held
// HTTP connection, and the endpoint is unauthenticated: without a cap, anything
// able to reach the port could pin memory and connections by opening streams.
var ErrTooManySubscribers = errors.New("event stream subscriber limit reached")

// eventBroadcaster fans persisted events out to SSE subscribers.
//
// The invariant that shapes everything here: publishing must never block. The
// publisher is the event processor, and a subscriber that stopped reading --
// a paused browser tab, a client on a slow link, a `curl` left in a terminal --
// must not be able to stall event processing for the whole application, let
// alone back-pressure the Docker stream reader.
//
// So every send is non-blocking. A subscriber whose buffer is full has the
// event dropped and its drop counter incremented; the handler reports the gap
// to the client, which can reload the paginated history to fill it. Dropping
// for one slow reader is strictly better than stalling for everyone.
//
// Ownership: subscribers own their channel's lifetime through unsubscribe,
// which is the only closer. Publish never closes anything, so there is no
// send-on-closed race between a publisher and a departing subscriber -- the
// broadcaster's lock covers both.
type eventBroadcaster struct {
	mu sync.Mutex

	limit      int
	bufferSize int
	next       int64
	subs       map[int64]*eventSubscriber

	// closed stops new subscriptions during shutdown.
	closed bool
}

// eventSubscriber is one connected SSE client.
type eventSubscriber struct {
	id     int64
	events chan domain.DockerEvent
	// dropped counts events this subscriber missed because its buffer was
	// full. Guarded by the broadcaster's mutex.
	dropped int64
}

// StreamSubscription is a live handle on the broadcast stream.
//
// The holder must call Close exactly once, which unsubscribes and releases the
// slot against the subscriber limit. A deferred Close in the HTTP handler is
// what makes a client disconnect release its slot.
type StreamSubscription struct {
	// Events delivers redacted, already-persisted events. It is closed when
	// Close is called or the broadcaster shuts down.
	Events <-chan domain.DockerEvent

	closeOnce sync.Once
	close     func()
}

// NewStreamSubscription wraps a channel and a release function as a
// subscription.
//
// Exported because StreamSubscription is what the API layer's engine interface
// hands back, so anything standing in for the engine -- a test double, or a
// second broadcaster implementation -- needs a way to construct one without
// reaching into this package's internals.
//
// release must be idempotent-safe: Close calls it at most once.
func NewStreamSubscription(events <-chan domain.DockerEvent, release func()) *StreamSubscription {
	return &StreamSubscription{Events: events, close: release}
}

// Close releases the subscription. Safe to call more than once.
func (s *StreamSubscription) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.close != nil {
			s.close()
		}
	})
}

func newEventBroadcaster(limit, bufferSize int) *eventBroadcaster {
	if bufferSize < 1 {
		bufferSize = 1
	}
	return &eventBroadcaster{
		limit:      limit,
		bufferSize: bufferSize,
		subs:       make(map[int64]*eventSubscriber),
	}
}

// Subscribe registers a new SSE client.
//
// Returns ErrTooManySubscribers when the cap is reached, which the handler
// renders as 503 rather than queueing: a client told "try later" can back off,
// while one held in a queue occupies the connection it was denied.
func (b *eventBroadcaster) Subscribe() (*StreamSubscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrTooManySubscribers
	}
	if b.limit > 0 && len(b.subs) >= b.limit {
		return nil, ErrTooManySubscribers
	}

	b.next++
	sub := &eventSubscriber{
		id:     b.next,
		events: make(chan domain.DockerEvent, b.bufferSize),
	}
	b.subs[sub.id] = sub

	return &StreamSubscription{
		Events: sub.events,
		close:  func() { b.unsubscribe(sub.id) },
	}, nil
}

// unsubscribe removes a subscriber and closes its channel.
//
// The lock is held across the close, and Publish sends under the same lock, so
// a send can never race a close. This is the single closer for the channel.
func (b *eventBroadcaster) unsubscribe(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sub, ok := b.subs[id]
	if !ok {
		return
	}
	delete(b.subs, id)
	close(sub.events)
}

// Publish fans an event out to every subscriber, never blocking.
//
// Returns how many subscribers dropped it, which the caller logs at debug so a
// persistently slow client is diagnosable.
func (b *eventBroadcaster) Publish(event domain.DockerEvent) (dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, sub := range b.subs {
		select {
		case sub.events <- event:
		default:
			// The subscriber is not keeping up. Drop for it alone.
			sub.dropped++
			dropped++
		}
	}
	return dropped
}

// Subscribers reports the current subscriber count.
func (b *eventBroadcaster) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Limit reports the configured subscriber cap. Zero means unlimited.
func (b *eventBroadcaster) Limit() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit
}

// Close disconnects every subscriber and refuses new ones.
//
// Called during shutdown so an SSE handler blocked on its channel returns
// rather than holding the HTTP server's drain open.
func (b *eventBroadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true
	for id, sub := range b.subs {
		delete(b.subs, id)
		close(sub.events)
	}
}
