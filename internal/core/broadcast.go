package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// subscriptionBuffer is how many entity ids a subscriber may fall behind.
const subscriptionBuffer = 256

// ErrSubscriptionClosed is returned by Recv after Close.
var ErrSubscriptionClosed = errors.New("subscription closed")

// Hub fans entity ids out to subscribers without ever blocking the publisher.
type Hub struct {
	mu          sync.Mutex
	subscribers map[*Subscription]struct{}
}

// NewHub is a hub without subscribers.
func NewHub() *Hub {
	return &Hub{subscribers: map[*Subscription]struct{}{}}
}

// Subscribe starts receiving ids published from now on.
func (h *Hub) Subscribe() *Subscription {
	sub := &Subscription{
		hub:    h,
		ids:    make(chan string, subscriptionBuffer),
		closed: make(chan struct{}),
	}
	h.mu.Lock()
	h.subscribers[sub] = struct{}{}
	h.mu.Unlock()
	return sub
}

// Publish hands id to every subscriber. A subscriber whose buffer is full
// misses the id and is marked as lagging instead of slowing everyone down.
func (h *Hub) Publish(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subscribers {
		select {
		case sub.ids <- id:
		default:
			sub.lagged.Store(true)
		}
	}
}

// Subscription is one receiver of a Hub.
type Subscription struct {
	hub    *Hub
	ids    chan string
	lagged atomic.Bool
	closed chan struct{}
	once   sync.Once
}

// Recv waits for the next entity id. When the subscriber fell behind it drops
// whatever is still queued and returns lagged=true with an empty id: the caller
// must then treat every entity as changed.
func (s *Subscription) Recv(ctx context.Context) (id string, lagged bool, err error) {
	if s.lagged.Swap(false) {
		s.drain()
		return "", true, nil
	}
	select {
	case id := <-s.ids:
		return id, false, nil
	case <-ctx.Done():
		return "", false, ctx.Err()
	case <-s.closed:
		return "", false, ErrSubscriptionClosed
	}
}

func (s *Subscription) drain() {
	for {
		select {
		case <-s.ids:
		default:
			return
		}
	}
}

// Close stops delivery. It is safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.mu.Lock()
		delete(s.hub.subscribers, s)
		s.hub.mu.Unlock()
		close(s.closed)
	})
}
