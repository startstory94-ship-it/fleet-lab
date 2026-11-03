package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/task"
)

type fakePub struct {
	mu   sync.Mutex
	got  []string // topic|body
	fail int      // next publishes that fail
}

func (p *fakePub) Publish(_ context.Context, topic string, _ byte, retain bool, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail > 0 {
		p.fail--
		return errors.New("broker away")
	}
	p.got = append(p.got, topic+"|"+string(body))
	return nil
}

func (p *fakePub) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

func newTestOutbox() *outbox {
	q := newOutbox(slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.retryDelay = time.Millisecond
	return q
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Robot commands are bounded (the manager re-sends); elevator messages are
// never refused; trips for a car coalesce to the newest.
func TestOutboxLimitsAndCoalescing(t *testing.T) {
	q := newTestOutbox()
	for i := range maxRobotCmds {
		if err := q.Send("r1", task.Command{CmdID: string(rune('a' + i%26))}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := q.Send("r1", task.Command{CmdID: "over"}); err == nil {
		t.Fatal("robot command accepted beyond the limit")
	}
	for range 10 {
		if err := q.PublishTurn("E1", elevator.Turn{Trip: "T", N: 1}); err != nil {
			t.Fatal("turn refused")
		}
	}
	for i := range 5 {
		q.PublishTrip("E1", elevator.Trip{ID: "T", Stage: []string{"a", "b", "c", "d", "e"}[i]})
	}
	q.PublishTrip("E2", elevator.Trip{ID: "U", Stage: "x"})
	trips := 0
	for _, m := range q.q {
		if m.kind == kindTrip {
			trips++
		}
	}
	if trips != 2 {
		t.Fatalf("%d trip messages waiting, want 1 per car", trips)
	}
	pub := &fakePub{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx, pub)
	waitFor(t, "all sent", func() bool { return len(pub.sent()) == maxRobotCmds+10+2 })
	last := pub.sent()[maxRobotCmds+10]
	if want := `elevator/E1/trip|{"trip":"T","boot":"","elevator":"","stage":"e"`; last[:len(want)] != want {
		t.Fatalf("trip sent %q, want the newest", last)
	}
}

// A trip publish that fails (broker away) is retried until it goes out,
// unless a newer one replaced it; a clear goes out as an empty retained
// message.
func TestOutboxRetriesTrips(t *testing.T) {
	q := newTestOutbox()
	pub := &fakePub{fail: 3}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.PublishTrip("E1", elevator.Trip{ID: "T", Stage: "travel"})
	go q.run(ctx, pub)
	waitFor(t, "trip retried", func() bool { return len(pub.sent()) == 1 })
	q.Command("E1", elevator.Cmd{CmdID: "c1"})
	q.ClearTrip("E1")
	waitFor(t, "clear", func() bool { return len(pub.sent()) == 3 })
	if s := pub.sent()[2]; s != "elevator/E1/trip|" {
		t.Fatalf("clear sent as %q", s)
	}
	// A failed elevator command is not retried here (the conductor does).
	pub.mu.Lock()
	pub.fail = 1
	pub.mu.Unlock()
	q.Command("E1", elevator.Cmd{CmdID: "c2"})
	q.Command("E1", elevator.Cmd{CmdID: "c3"})
	waitFor(t, "c3", func() bool { return len(pub.sent()) == 4 })
}
