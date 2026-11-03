// Package events fans fleet events out to many listeners (WebSocket clients)
// without letting a slow listener hold up the rest.
package events

import (
	"sync"

	"fleetlab/internal/fleet"
)

const (
	historySize = 200 // recent events kept for GET /events and new clients
	bufferSize  = 64  // per-subscriber queue; when full, the event is dropped for that subscriber
)

// Sub is one listener. Read events from C; Dropped counts events lost
// because the listener fell behind.
type Sub struct {
	C       chan fleet.Event
	mu      sync.Mutex
	dropped int
}

func (s *Sub) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

type Hub struct {
	mu      sync.Mutex
	subs    map[*Sub]struct{}
	history []fleet.Event
}

func NewHub() *Hub {
	return &Hub{subs: make(map[*Sub]struct{})}
}

// Publish never blocks: a subscriber whose queue is full misses the event.
func (h *Hub) Publish(e fleet.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.history = append(h.history, e)
	if len(h.history) > historySize {
		h.history = h.history[len(h.history)-historySize:]
	}
	for s := range h.subs {
		select {
		case s.C <- e:
		default:
			s.mu.Lock()
			s.dropped++
			s.mu.Unlock()
		}
	}
}

// Subscribe registers a listener. Call the returned func to unsubscribe.
func (h *Hub) Subscribe() (*Sub, func()) {
	s := &Sub{C: make(chan fleet.Event, bufferSize)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
	}
}

// Since returns the kept events numbered after seq, oldest first. If the
// first one is not seq+1, older ones were already dropped.
func (h *Hub) Since(seq uint64) []fleet.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := len(h.history)
	for i > 0 && h.history[i-1].Seq > seq {
		i--
	}
	return append([]fleet.Event{}, h.history[i:]...)
}

// Recent returns up to n of the latest events, oldest first.
func (h *Hub) Recent(n int) []fleet.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n <= 0 || n > len(h.history) {
		n = len(h.history)
	}
	return append([]fleet.Event{}, h.history[len(h.history)-n:]...) // [] not null in JSON
}
