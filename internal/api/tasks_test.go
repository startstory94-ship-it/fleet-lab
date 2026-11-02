package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

type nopSender struct{}

func (nopSender) Send(string, task.Command) error { return nil }

func newTaskServer(t *testing.T) (*httptest.Server, *task.Manager, *fleet.Registry) {
	t.Helper()
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	m := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}})
	srv := httptest.NewServer(New(g, hub, m, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv, m, g
}

func post(t *testing.T, url, body string, v any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		decode(t, resp, v)
	}
	return resp.StatusCode
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
}

func TestPostTask(t *testing.T) {
	srv, _, _ := newTaskServer(t)
	var tk task.Task
	code := post(t, srv.URL+"/tasks", `{"waypoints":[{"x":1.5,"y":-2,"floor":1},{"x":3,"y":0,"floor":2}]}`, &tk)
	if code != http.StatusCreated || tk.ID == "" || tk.State != task.Queued || len(tk.Waypoints) != 2 ||
		tk.Waypoints[1] != (task.Waypoint{X: 3, Y: 0, Floor: 2}) {
		t.Fatalf("POST /tasks = %d %+v", code, tk)
	}

	for _, body := range []string{
		``,
		`not json`,
		`{}`,
		`{"waypoints":[]}`,
		`{"waypoints":[{"x":1,"y":2}]}`, // floor missing
		`{"waypoints":[{"x":"1","y":2,"floor":1}]}`, // string, not a number
		`{"waypoints":[{"x":1,"y":2,"floor":1.5}]}`, // floor not an integer
		`{"waypoints":[{"x":1,"y":2,"floor":1,"z":0}]}`,
		`{"waypoint":[{"x":1,"y":2,"floor":1}]}`,
	} {
		var e map[string]string
		if code := post(t, srv.URL+"/tasks", body, &e); code != http.StatusBadRequest || e["error"] == "" {
			t.Errorf("POST %q = %d %v, want 400 with error", body, code, e)
		}
	}
	var list []task.Task
	if get(t, srv.URL+"/tasks", &list); len(list) != 1 {
		t.Fatalf("invalid posts created tasks: %+v", list)
	}
}

func TestGetTasks(t *testing.T) {
	srv, m, g := newTaskServer(t)
	g.ApplyState("r1", fleet.StateMsg{Boot: "b", Seq: 1, Mode: "IDLE", Battery: 90})
	for range 3 {
		post(t, srv.URL+"/tasks", `{"waypoints":[{"x":1,"y":1,"floor":0}]}`, nil)
	}
	m.Tick() // r1 takes t-1

	var all, queued, assigned []task.Task
	if code := get(t, srv.URL+"/tasks", &all); code != 200 || len(all) != 3 || all[0].ID != "t-1" || all[2].ID != "t-3" {
		t.Fatalf("GET /tasks = %d %+v", code, all)
	}
	if get(t, srv.URL+"/tasks?state=QUEUED", &queued); len(queued) != 2 {
		t.Fatalf("QUEUED = %+v", queued)
	}
	if get(t, srv.URL+"/tasks?state=ASSIGNED", &assigned); len(assigned) != 1 || assigned[0].Robot != "r1" {
		t.Fatalf("ASSIGNED = %+v", assigned)
	}
	var none []task.Task
	if code := get(t, srv.URL+"/tasks?state=DONE", &none); code != 200 || none == nil || len(none) != 0 {
		t.Fatalf("DONE = %d %+v, want an empty list", code, none)
	}
	if code := get(t, srv.URL+"/tasks?state=done", nil); code != http.StatusBadRequest {
		t.Fatalf("unknown state = %d, want 400", code)
	}

	var one task.Task
	if code := get(t, srv.URL+"/tasks/t-1", &one); code != 200 || one.State != task.Assigned || one.CmdID == "" {
		t.Fatalf("GET /tasks/t-1 = %d %+v", code, one)
	}
	if code := get(t, srv.URL+"/tasks/t-99", nil); code != http.StatusNotFound {
		t.Fatalf("GET /tasks/t-99 = %d, want 404", code)
	}
}

func TestCancelTask(t *testing.T) {
	srv, m, g := newTaskServer(t)
	g.ApplyState("r1", fleet.StateMsg{Boot: "b", Seq: 1, Mode: "IDLE", Battery: 90})
	post(t, srv.URL+"/tasks", `{"waypoints":[{"x":1,"y":1,"floor":0}]}`, nil)
	post(t, srv.URL+"/tasks", `{"waypoints":[{"x":1,"y":1,"floor":0}]}`, nil)
	m.Tick()

	for _, tc := range []struct {
		id   string
		code int
	}{
		{"t-1", http.StatusOK},       // assigned
		{"t-2", http.StatusOK},       // queued
		{"t-1", http.StatusConflict}, // already cancelled
		{"t-99", http.StatusNotFound},
	} {
		var tk task.Task
		code := post(t, srv.URL+"/tasks/"+tc.id+"/cancel", "", &tk)
		if code != tc.code {
			t.Fatalf("cancel %s = %d, want %d", tc.id, code, tc.code)
		}
		if code == http.StatusOK && tk.State != task.Cancelled {
			t.Fatalf("cancel %s returned %+v", tc.id, tk)
		}
	}
	if code := get(t, srv.URL+"/tasks/t-1/cancel", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on cancel = %d, want 405", code)
	}
}

func TestPostTaskBodyLimits(t *testing.T) {
	srv, _, _ := newTaskServer(t)
	for _, body := range []string{
		`{"waypoints":[{"x":1,"y":2,"floor":1}]}{"waypoints":[]}`,
		`{"waypoints":[{"x":1,"y":2,"floor":1}]} trailing`,
		`{"waypoints":[{"x":1,"y":2,"floor":1}]}]`,
	} {
		if code := post(t, srv.URL+"/tasks", body, nil); code != http.StatusBadRequest {
			t.Errorf("POST %q = %d, want 400", body, code)
		}
	}
	ok := `{"waypoints":[{"x":1,"y":2,"floor":1}]}` + "\n  "
	if code := post(t, srv.URL+"/tasks", ok, nil); code != http.StatusCreated {
		t.Errorf("trailing whitespace = %d, want 201", code)
	}

	big := `{"waypoints":[` + strings.Repeat(`{"x":1,"y":2,"floor":1},`, 60000) + `{"x":1,"y":2,"floor":1}]}`
	var e map[string]string
	if code := post(t, srv.URL+"/tasks", big, &e); code != http.StatusRequestEntityTooLarge || e["error"] == "" {
		t.Fatalf("%d-byte body = %d %v, want 413", len(big), code, e)
	}
	var list []task.Task
	if get(t, srv.URL+"/tasks", &list); len(list) != 1 {
		t.Fatalf("%d tasks, want only the valid one", len(list))
	}
}

type brokenStore struct{}

func (brokenStore) Save(task.Task) error       { return errors.New("disk full") }
func (brokenStore) Load() ([]task.Task, error) { return nil, nil }

func TestPostTaskStoreFailure(t *testing.T) {
	hub := events.NewHub()
	g := fleet.NewRegistry(fleet.DefaultConfig(), hub.Publish)
	m := task.NewManager(task.DefaultConfig(), task.Deps{Robots: g, Sender: nopSender{}, Store: brokenStore{}})
	srv := httptest.NewServer(New(g, hub, m, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()

	var e map[string]string
	if code := post(t, srv.URL+"/tasks", `{"waypoints":[{"x":1,"y":2,"floor":1}]}`, &e); code != http.StatusInternalServerError || e["error"] == "" {
		t.Fatalf("POST with a failing store = %d %v, want 500", code, e)
	}
	var list []task.Task
	if get(t, srv.URL+"/tasks", &list); len(list) != 0 {
		t.Fatalf("unsaved task listed: %+v", list)
	}
}
