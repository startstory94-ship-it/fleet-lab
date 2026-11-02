package events

import (
	"testing"

	"fleetlab/internal/fleet"
)

func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	h := NewHub()
	slow, unsubSlow := h.Subscribe()
	defer unsubSlow()
	fast, unsubFast := h.Subscribe()
	defer unsubFast()

	// Nobody reads `slow`; publishing more than its buffer must not block.
	n := bufferSize + 10
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			h.Publish(fleet.Event{Robot: "r1"})
			<-fast.C
		}
		close(done)
	}()
	<-done

	if got := slow.Dropped(); got != 10 {
		t.Errorf("slow dropped %d, want 10", got)
	}
	if got := fast.Dropped(); got != 0 {
		t.Errorf("fast dropped %d, want 0", got)
	}
}

func TestRecentKeepsLatest(t *testing.T) {
	h := NewHub()
	for i := 0; i < historySize+5; i++ {
		h.Publish(fleet.Event{Detail: string(rune('a' + i%26))})
	}
	if got := len(h.Recent(0)); got != historySize {
		t.Errorf("history = %d, want %d", got, historySize)
	}
	if got := h.Recent(3); len(got) != 3 {
		t.Errorf("Recent(3) returned %d", len(got))
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	s, unsub := h.Subscribe()
	unsub()
	h.Publish(fleet.Event{})
	select {
	case <-s.C:
		t.Error("event delivered after unsubscribe")
	default:
	}
}
