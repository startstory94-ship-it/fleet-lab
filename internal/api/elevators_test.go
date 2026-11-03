package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

func postRelease(t *testing.T, url string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// POST /elevators/{id}/release: 404 unknown, 409 no trip, the same
// same-site JSON rules as the other POSTs, 200 releases a trip.
func TestReleaseElevator(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	g.SetClock(func() time.Time { return now })
	tasks := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}, Now: func() time.Time { return now },
		Elevators: elevator.DefaultConfig(), Lift: nopLift{}})
	srv := httptest.NewServer(New(g, hub, tasks, nil, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()

	if c := postRelease(t, srv.URL+"/elevators/E9/release", nil); c != http.StatusNotFound {
		t.Fatalf("unknown elevator: %d", c)
	}
	if c := postRelease(t, srv.URL+"/elevators/E1/release", nil); c != http.StatusConflict {
		t.Fatalf("no trip: %d", c)
	}
	// A robot waiting at the lobby makes a trip.
	bat := 80.0
	g.ApplyPose("r1", fleet.PoseMsg{Boot: "b", Seq: 1, Floor: 1, X: 10})
	g.ApplyState("r1", fleet.StateMsg{Boot: "b", Seq: 1, Mode: "IDLE", Battery: &bat})
	tasks.HandleElevatorState("E1", elevator.State{Boot: "c", Seq: 1, Floor: 1, Door: elevator.DoorClosed})
	if _, err := tasks.Submit([]task.Waypoint{{X: 3, Y: 3, Floor: 4}}); err != nil {
		t.Fatal(err)
	}
	tasks.Tick()
	tk := tasks.List("")[0]
	tasks.HandleAck("r1", task.Ack{CmdID: tk.CmdID, Status: "done"})
	now = now.Add(4 * time.Second)
	tasks.HandleElevatorState("E1", elevator.State{Boot: "c", Seq: 2, Floor: 1, Door: elevator.DoorClosed})
	tasks.Tick()
	if v := tasks.Elevators()[0]; v.Trip == nil {
		t.Fatal("no trip planned")
	}
	for _, h := range []map[string]string{
		{"Content-Type": "text/plain"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "http://evil.example"},
	} {
		if c := postRelease(t, srv.URL+"/elevators/E1/release", h); c != http.StatusUnsupportedMediaType && c != http.StatusForbidden {
			t.Fatalf("headers %v: %d", h, c)
		}
	}
	if v := tasks.Elevators()[0]; v.Trip == nil {
		t.Fatal("a refused request released the car")
	}
	if c := postRelease(t, srv.URL+"/elevators/E1/release", nil); c != http.StatusOK {
		t.Fatalf("release: %d", c)
	}
	if v := tasks.Elevators()[0]; v.Trip != nil {
		t.Fatal("still has a trip")
	}
	got := tasks.List("")[0]
	if got.Phase != task.PhaseWait || got.Attempts != 0 {
		t.Fatalf("task %+v, want back in the lobby queue", got)
	}
}
