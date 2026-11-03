package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

func newServer(t *testing.T) (*httptest.Server, *fleet.Registry, *events.Hub) {
	t.Helper()
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	tasks := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}})
	srv := httptest.NewServer(New(g, hub, tasks, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv, g, hub
}

func get(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func TestRobots(t *testing.T) {
	srv, g, _ := newServer(t)
	g.ApplyState("robot_1", fleet.StateMsg{Boot: "b", Seq: 1, Mode: "IDLE", Battery: ptr(50.0)})

	var list []fleet.Robot
	if code := get(t, srv.URL+"/robots", &list); code != 200 || len(list) != 1 || list[0].Name != "robot_1" {
		t.Fatalf("GET /robots = %d %+v", code, list)
	}
	var one fleet.Robot
	if code := get(t, srv.URL+"/robots/robot_1", &one); code != 200 || one.Battery != 50 {
		t.Fatalf("GET /robots/robot_1 = %d %+v", code, one)
	}
	if code := get(t, srv.URL+"/robots/nope", nil); code != http.StatusNotFound {
		t.Fatalf("GET /robots/nope = %d, want 404", code)
	}
}

func TestEventsLimit(t *testing.T) {
	srv, _, hub := newServer(t)
	for i := 1; i <= 5; i++ {
		hub.Publish(fleet.Event{Seq: uint64(i)})
	}
	var evs []fleet.Event
	if get(t, srv.URL+"/events?n=2", &evs); len(evs) != 2 || evs[1].Seq != 5 {
		t.Fatalf("GET /events?n=2 = %+v", evs)
	}
	if get(t, srv.URL+"/events", &evs); len(evs) != 5 {
		t.Fatalf("GET /events returned %d, want 5", len(evs))
	}
}

func TestEventsSince(t *testing.T) {
	srv, _, hub := newServer(t)
	for i := 1; i <= 5; i++ {
		hub.Publish(fleet.Event{Seq: uint64(i)})
	}
	var evs []fleet.Event
	if get(t, srv.URL+"/events?since=3", &evs); len(evs) != 2 || evs[0].Seq != 4 || evs[1].Seq != 5 {
		t.Fatalf("GET /events?since=3 = %+v", evs)
	}
	if code := get(t, srv.URL+"/events?since=5", &evs); code != 200 || evs == nil || len(evs) != 0 {
		t.Fatalf("GET /events?since=5 = %d %+v, want an empty list", code, evs)
	}
	if get(t, srv.URL+"/events?since=0", &evs); len(evs) != 5 {
		t.Fatalf("GET /events?since=0 returned %d, want 5", len(evs))
	}
	if code := get(t, srv.URL+"/events?since=x", nil); code != http.StatusBadRequest {
		t.Fatalf("since=x = %d, want 400", code)
	}
}

func TestWebSocketStreamsEvents(t *testing.T) {
	srv, g, _ := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	// The subscription is registered inside the handler; retry until an event
	// published after it arrives.
	got := make(chan fleet.Event, 1)
	go func() {
		var e fleet.Event
		if wsjson.Read(ctx, c, &e) == nil {
			got <- e
		}
	}()
	for seq := uint64(1); ; seq++ {
		g.ApplyState("robot_1", fleet.StateMsg{Boot: "b", Seq: seq, Mode: []string{"IDLE", "MOVING"}[seq%2]})
		select {
		case e := <-got:
			if e.Robot != "robot_1" {
				t.Fatalf("event = %+v", e)
			}
			return
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("no event over WebSocket")
		}
	}
}

func ptr[T any](v T) *T { return &v }
