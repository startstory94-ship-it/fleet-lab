package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/live"
	"fleetlab/internal/task"
)

func newServer(t *testing.T) (*httptest.Server, *fleet.Registry, *events.Hub) {
	t.Helper()
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	tasks := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}})
	srv := httptest.NewServer(New(g, hub, tasks, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	// Right after a start there are none: an empty list, not null (the
	// dashboard reads the latest event from it on every reconnect).
	if code, body := getRaw(t, srv.URL+"/events?n=1"); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("GET /events?n=1 with no events = %d %q, want []", code, body)
	}
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

func TestDashboardServed(t *testing.T) {
	srv, _, _ := newServer(t)
	for _, tc := range []struct{ path, ctype, contains string }{
		{"/", "text/html; charset=utf-8", "<canvas"},
		{"/app.js", "text/javascript; charset=utf-8", "/ws/live"},
		{"/style.css", "text/css; charset=utf-8", "{"},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != tc.ctype ||
			!strings.Contains(string(body), tc.contains) {
			t.Errorf("GET %s = %d %q (%d bytes), want 200 %q containing %q",
				tc.path, resp.StatusCode, resp.Header.Get("Content-Type"), len(body), tc.ctype, tc.contains)
		}
	}
	if code := get(t, srv.URL+"/nope.js", nil); code != http.StatusNotFound {
		t.Errorf("GET /nope.js = %d, want 404", code)
	}
}

func TestLiveFirstMessageIsSnapshot(t *testing.T) {
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	tasks := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}})
	frames := live.New(live.DefaultConfig(), g, tasks)
	srv := httptest.NewServer(New(g, hub, tasks, frames, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()

	for i, name := range []string{"robot_1", "robot_2"} {
		g.ApplyPose(name, fleet.PoseMsg{Boot: "b", Seq: 1, Floor: 1, X: float64(i)})
		g.ApplyState(name, fleet.StateMsg{Boot: "b", Seq: 1, Mode: "IDLE", Battery: ptr(20.0)})
	}
	tasks.Tick() // the battery policy flags both
	frames.Tick()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/live", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	var f live.Frame
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != live.Snapshot || f.Seq != 1 || len(f.Robots) != 2 ||
		f.Robots[0].Name != "robot_1" || f.Robots[1].X != 1 || !f.Robots[0].NeedsCharge {
		t.Fatalf("first frame = %+v, want a snapshot of both robots, flagged", f)
	}

	g.ApplyPose("robot_2", fleet.PoseMsg{Boot: "b", Seq: 2, Floor: 1, X: 3})
	frames.Tick()
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != live.Delta || f.Seq != 2 || len(f.Robots) != 1 || f.Robots[0].X != 3 {
		t.Fatalf("second frame = %+v, want a delta with robot_2 only", f)
	}

	var st live.Stats
	if code := get(t, srv.URL+"/live/stats", &st); code != 200 || st.Subscribers != 1 || st.Seq != 2 {
		t.Fatalf("GET /live/stats = %d %+v", code, st)
	}
}

func getRaw(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func ptr[T any](v T) *T { return &v }

func TestLiveOffIs503(t *testing.T) {
	srv, _, _ := newServer(t) // frames == nil
	for _, path := range []string{"/ws/live", "/live/stats"} {
		if code := get(t, srv.URL+path, nil); code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", path, code)
		}
	}
}

func newLiveServer(t *testing.T, cfg live.Config) (*httptest.Server, *fleet.Registry, *live.Broadcaster) {
	t.Helper()
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	tasks := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}})
	frames := live.New(cfg, g, tasks)
	srv := httptest.NewServer(New(g, hub, tasks, frames, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv, g, frames
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/live"
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting for " + what)
}

func TestLiveSubscriberLimit(t *testing.T) {
	srv, _, frames := newLiveServer(t, live.Config{Buffer: 4, MaxSubscribers: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, _, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first subscriber", func() bool { return frames.Stats().Subscribers == 1 })
	_, resp, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second dial err=%v resp=%v, want 503", err, resp)
	}
	// Closing the client unsubscribes it on the server, freeing the slot.
	c1.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "unsubscribe after close", func() bool { return frames.Stats().Subscribers == 0 })
	c2, _, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatalf("dial after the slot was freed: %v", err)
	}
	c2.CloseNow()
}

// A client that stops reading is closed after the write timeout; a client
// that reads keeps receiving all along.
func TestLiveSlowClientClosed(t *testing.T) {
	old := writeTimeout
	writeTimeout = 200 * time.Millisecond
	defer func() { writeTimeout = old }()
	srv, g, frames := newLiveServer(t, live.Config{Buffer: 4, MaxSubscribers: 8})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	slow, _, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.CloseNow() // never read: its socket buffers fill up
	fast, _, err := websocket.Dial(ctx, wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.CloseNow()
	fast.SetReadLimit(1 << 24)
	var got atomic.Int64
	go func() {
		for {
			if _, _, err := fast.Read(ctx); err != nil {
				return
			}
			got.Add(1)
		}
	}()
	waitFor(t, "two subscribers", func() bool { return frames.Stats().Subscribers == 2 })

	// Big frames (500 robots, all moving) fill the slow client's buffers fast.
	seq := uint64(0)
	tick := func() {
		seq++
		for i := 0; i < 500; i++ {
			g.ApplyPose(fmt.Sprintf("robot_%03d", i), fleet.PoseMsg{Boot: "b", Seq: seq, Floor: 1, X: float64(seq), Y: float64(i)})
		}
		frames.Tick()
	}
	for frames.Stats().Subscribers == 2 {
		if ctx.Err() != nil {
			t.Fatal("slow client never closed: no write timeout?")
		}
		tick()
		time.Sleep(time.Millisecond)
	}
	before := got.Load()
	for i := 0; i < 20; i++ {
		tick()
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, "fast client still receiving", func() bool { return got.Load() > before })
	if st := frames.Stats(); st.Subscribers != 1 {
		t.Fatalf("subscribers = %d, want only the fast one", st.Subscribers)
	}
}

// Every response says which server run it came from; a new run has a new id.
func TestBootHeader(t *testing.T) {
	srv1, _, _ := newServer(t)
	srv2, _, _ := newServer(t)
	boot := func(url string) string {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get(BootHeader)
	}
	a, b := boot(srv1.URL+"/events?n=1"), boot(srv1.URL+"/robots")
	c := boot(srv2.URL + "/events?n=1")
	if a == "" || a != b || a == c {
		t.Fatalf("boot ids %q %q (same run) %q (other run)", a, b, c)
	}
}
