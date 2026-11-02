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

func TestRecentKeepsLatestInOrder(t *testing.T) {
	h := NewHub()
	total := historySize + 5
	for i := 1; i <= total; i++ {
		h.Publish(fleet.Event{Seq: uint64(i)})
	}
	all := h.Recent(0)
	if len(all) != historySize {
		t.Fatalf("history = %d, want %d", len(all), historySize)
	}
	if first, last := all[0].Seq, all[len(all)-1].Seq; first != 6 || last != uint64(total) {
		t.Errorf("history spans seq %d..%d, want 6..%d (oldest dropped first)", first, last, total)
	}
	got := h.Recent(3)
	if len(got) != 3 || got[0].Seq != uint64(total-2) || got[2].Seq != uint64(total) {
		t.Errorf("Recent(3) = %+v, want the last three oldest first", got)
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
