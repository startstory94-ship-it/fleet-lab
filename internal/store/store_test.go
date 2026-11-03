package store

import (
	"path/filepath"
	"testing"
	"time"

	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

func open(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRoundTrip(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "t.db"))
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	in := []task.Task{
		{ID: "t-2", Seq: 2, State: task.Queued, Waypoints: []task.Waypoint{{X: 1, Y: 2, Floor: 3}}, Created: at, Updated: at},
		{ID: "t-1", Seq: 1, State: task.Running, Robot: "r1", Leg: 1, Attempts: 2, Dispatches: 3, CmdID: "t-1.1.3",
			Waypoints: []task.Waypoint{{X: 0.5}, {X: -4, Y: 7.25, Floor: 2}}, Error: "failed: blocked",
			Created: at, Updated: at.Add(time.Minute), Started: at.Add(time.Second)},
	}
	for _, tk := range in {
		if err := db.Save(tk); err != nil {
			t.Fatal(err)
		}
	}
	in[1].State = task.Done // update in place
	in[1].Finished = at.Add(2 * time.Minute)
	if err := db.Save(in[1]); err != nil {
		t.Fatal(err)
	}

	out, err := db.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ID != "t-1" || out[1].ID != "t-2" {
		t.Fatalf("loaded %+v, want t-1, t-2 in seq order", out)
	}
	got, want := out[0], in[1]
	if got.State != want.State || got.Robot != want.Robot || got.Leg != want.Leg || got.Attempts != want.Attempts ||
		got.Dispatches != want.Dispatches || got.CmdID != want.CmdID || got.Error != want.Error ||
		len(got.Waypoints) != 2 || got.Waypoints[1] != want.Waypoints[1] ||
		!got.Created.Equal(want.Created) || !got.Started.Equal(want.Started) || !got.Finished.Equal(want.Finished) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, want)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Save(task.Task{ID: "t-1", Seq: 1, State: task.Queued, Waypoints: []task.Waypoint{{X: 1}}})
	db.Close()
	out, err := open(t, path).Load()
	if err != nil || len(out) != 1 || out[0].ID != "t-1" {
		t.Fatalf("after reopen: %+v %v", out, err)
	}
}

type robots struct{ list []fleet.Robot }

func (r *robots) Snapshot() []fleet.Robot { return r.list }

type sender struct{ cmds []task.Command }

func (s *sender) Send(_ string, c task.Command) error {
	s.cmds = append(s.cmds, c)
	return nil
}

// restart builds a fresh manager on the same database, the way fleetd does on
// startup.
func restart(t *testing.T, prev *task.Manager, db *DB, now *time.Time, rb *robots) (*task.Manager, *sender) {
	t.Helper()
	if prev != nil {
		if err := prev.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	s := &sender{}
	m := task.NewManager(task.DefaultConfig(), task.Deps{
		Now: func() time.Time { return *now }, Robots: rb, Sender: s, Store: db,
	})
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.LinkUp() // the broker answers at once
	return m, s
}

func TestRestartRules(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "t.db"))
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	rb := &robots{}
	m, _ := restart(t, nil, db, &now, rb)

	// Three robots each get a task, two more wait, one is finished.
	rb.list = []fleet.Robot{
		{Name: "a", Conn: fleet.Online, Mode: "IDLE", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true},
		{Name: "b", Conn: fleet.Online, Mode: "IDLE", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, X: 10},
		{Name: "c", Conn: fleet.Online, Mode: "IDLE", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, X: 20},
	}
	for _, x := range []float64{0, 10, 20, 30, 40} {
		m.Submit([]task.Waypoint{{X: x, Floor: 1}})
	}
	m.Tick()
	ta, _ := m.Get("t-1")
	tb, _ := m.Get("t-2")
	tc, _ := m.Get("t-3")
	m.HandleAck("a", task.Ack{CmdID: ta.CmdID, Status: "accepted"})
	m.HandleAck("b", task.Ack{CmdID: tb.CmdID, Status: "accepted"})
	m.HandleAck("c", task.Ack{CmdID: tc.CmdID, Status: "accepted"})
	m.HandleAck("c", task.Ack{CmdID: tc.CmdID, Status: "done"})
	m.Cancel("t-5")

	// The server stops. After it starts again a still drives its goto, while b
	// no longer reports t-2 (and does not answer when asked).
	rb.list = nil
	m, s := restart(t, m, db, &now, rb)
	for id, want := range map[string]string{"t-1": task.Running, "t-2": task.Running, "t-3": task.Done,
		"t-4": task.Queued, "t-5": task.Cancelled} {
		if got, _ := m.Get(id); got.State != want {
			t.Fatalf("restored %s = %s, want %s", id, got.State, want)
		}
	}
	m.Tick()                                                                // no robots known yet: nothing happens
	rb.list = []fleet.Robot{{Name: "b", Conn: fleet.Online, HasPose: true}} // a pose but no state yet
	m.Tick()
	if len(s.cmds) != 0 {
		t.Fatalf("sent %+v before any robot state was seen", s.cmds)
	}

	now = now.Add(time.Second)
	rb.list = []fleet.Robot{
		{Name: "a", Conn: fleet.Online, Mode: "MOVING", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, TaskID: ta.CmdID},
		{Name: "b", Conn: fleet.Online, Mode: "MOVING", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, X: 10, TaskID: "other"},
		{Name: "c", Conn: fleet.Online, Mode: "MOVING", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, X: 20, TaskID: "other"},
	}
	m.Tick()
	if got, _ := m.Get("t-1"); got.State != task.Running || got.Robot != "a" {
		t.Fatalf("t-1 = %+v, want kept RUNNING on a", got)
	}
	// b does not report t-2, so it is asked once with the same cmd_id.
	if len(s.cmds) != 1 || s.cmds[0].CmdID != tb.CmdID {
		t.Fatalf("sent %+v, want one re-send of %s", s.cmds, tb.CmdID)
	}

	now = now.Add(24 * time.Second) // grace (25 s from LinkUp) is over
	m.Tick()
	got, _ := m.Get("t-2")
	if got.State != task.Queued || got.Attempts != 0 {
		t.Fatalf("t-2 = %+v, want back in the queue without using an attempt", got)
	}
	if got, _ := m.Get("t-1"); got.State != task.Running {
		t.Fatalf("t-1 = %s after grace, want RUNNING", got.State)
	}
	m.Flush()
	if out, _ := db.Load(); out[1].State != task.Queued {
		t.Fatalf("stored t-2 = %s, want QUEUED", out[1].State)
	}
}

func TestRestoredLegFinishedWhileDown(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "t.db"))
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	rb := &robots{list: []fleet.Robot{{Name: "a", Conn: fleet.Online, Mode: "IDLE", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true}}}
	m, _ := restart(t, nil, db, &now, rb)
	m.Submit([]task.Waypoint{{X: 1, Floor: 1}})
	m.Tick()
	tk, _ := m.Get("t-1")
	m.HandleAck("a", task.Ack{CmdID: tk.CmdID, Status: "accepted"})

	m, s := restart(t, m, db, &now, rb) // a is IDLE with no task: it finished
	m.Tick()
	if len(s.cmds) != 1 || s.cmds[0].CmdID != tk.CmdID {
		t.Fatalf("sent %+v, want a re-send of %s", s.cmds, tk.CmdID)
	}
	m.HandleAck("a", task.Ack{CmdID: tk.CmdID, Status: "done"}) // adapter repeats its last ack
	if got, _ := m.Get("t-1"); got.State != task.Done {
		t.Fatalf("t-1 = %s, want DONE", got.State)
	}
}

func TestNewIDsContinueAfterRestart(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "t.db"))
	now := time.Now()
	m, _ := restart(t, nil, db, &now, &robots{})
	m.Submit([]task.Waypoint{{X: 1}})
	m.Submit([]task.Waypoint{{X: 2}})
	m, _ = restart(t, m, db, &now, &robots{})
	tk, _ := m.Submit([]task.Waypoint{{X: 3}})
	if tk.ID != "t-3" {
		t.Fatalf("new task id %s, want t-3", tk.ID)
	}
}

func TestCorruptRowIsSkipped(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "t.db"))
	db.Save(task.Task{ID: "t-1", Seq: 1, State: task.Queued, Waypoints: []task.Waypoint{{X: 1}}})
	for _, row := range []struct{ id, data string }{
		{"t-2", `{"id":"t-2", truncated`},
		{"t-3", `{"id":"t-3","seq":3,"state":"QUEUED"}`}, // no waypoints
	} {
		if _, err := db.db.Exec(`INSERT INTO tasks (id, seq, state, data) VALUES (?, ?, 'QUEUED', ?)`,
			row.id, row.id[2:], row.data); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	m, _ := restart(t, nil, db, &now, &robots{}) // fails the test if Restore errors
	if got := m.List(""); len(got) != 1 || got[0].ID != "t-1" {
		t.Fatalf("loaded %+v, want only t-1", got)
	}
	tk, err := m.Submit([]task.Waypoint{{X: 2}})
	if err != nil || tk.ID != "t-4" {
		t.Fatalf("new task %s %v, want t-4 (ids of skipped rows are not reused)", tk.ID, err)
	}
}
