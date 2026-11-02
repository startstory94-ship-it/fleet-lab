package task

import (
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

// fakeRobots returns robots in insertion order (not sorted), so tie-breaks
// must come from the manager itself.
type fakeRobots struct {
	names  []string
	robots map[string]*fleet.Robot
}

func (f *fakeRobots) Snapshot() []fleet.Robot {
	out := make([]fleet.Robot, 0, len(f.names))
	for _, n := range f.names {
		out = append(out, *f.robots[n])
	}
	return out
}

type sent struct {
	robot string
	cmd   Command
}

type fakeSender struct{ sent []sent }

func (f *fakeSender) Send(robot string, c Command) error {
	f.sent = append(f.sent, sent{robot, c})
	return nil
}

// memStore is an in-memory Store.
type memStore struct{ tasks map[string]Task }

func (s *memStore) Save(t Task) error {
	s.tasks[t.ID] = t
	return nil
}

func (s *memStore) Load() ([]Task, error) {
	var out []Task
	for _, t := range s.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

type harness struct {
	t      *testing.T
	now    time.Time
	robots *fakeRobots
	sender *fakeSender
	store  *memStore
	events []Event
	m      *Manager
}

func newHarness(t *testing.T, cfg Config, robots ...fleet.Robot) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		now:    time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
		robots: &fakeRobots{robots: map[string]*fleet.Robot{}},
		sender: &fakeSender{},
		store:  &memStore{tasks: map[string]Task{}},
	}
	for _, r := range robots {
		h.addRobot(r)
	}
	h.m = NewManager(cfg, Deps{
		Now: func() time.Time { return h.now }, Robots: h.robots, Sender: h.sender, Store: h.store,
		Emit: func(e Event) { h.events = append(h.events, e) },
		Boot: "b0",
	})
	return h
}

func (h *harness) addRobot(r fleet.Robot) {
	r2 := r
	h.robots.names = append(h.robots.names, r.Name)
	h.robots.robots[r.Name] = &r2
}

func idle(name string, x, y float64) fleet.Robot {
	return fleet.Robot{Name: name, Conn: fleet.Online, Mode: "IDLE", Battery: 80, BatteryKnown: true, HasPose: true, Floor: 1, X: x, Y: y}
}

func (h *harness) robot(name string) *fleet.Robot { return h.robots.robots[name] }

func (h *harness) submit(wps ...Waypoint) Task {
	h.t.Helper()
	t, err := h.m.Submit(wps)
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

func (h *harness) tick() { h.m.Tick() }

// advance moves the clock in 250 ms steps, ticking each time.
func (h *harness) advance(d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); {
		h.now = h.now.Add(250 * time.Millisecond)
		h.m.Tick()
	}
}

func (h *harness) task(id string) Task {
	h.t.Helper()
	t, ok := h.m.Get(id)
	if !ok {
		h.t.Fatalf("task %s missing", id)
	}
	return t
}

func (h *harness) want(id, state, robot string) Task {
	h.t.Helper()
	t := h.task(id)
	if t.State != state || t.Robot != robot {
		h.t.Fatalf("task %s = %s on %q, want %s on %q (error %q)", id, t.State, t.Robot, state, robot, t.Error)
	}
	return t
}

// sentTo lists commands sent to robot, oldest first.
func (h *harness) sentTo(robot string) []Command {
	var out []Command
	for _, s := range h.sender.sent {
		if s.robot == robot {
			out = append(out, s.cmd)
		}
	}
	return out
}

func (h *harness) ack(robot, cmdID, status string) {
	h.m.HandleAck(robot, Ack{CmdID: cmdID, Status: status})
}

// start submits one task, assigns it and has the robot accept it.
func (h *harness) start(robot string, wps ...Waypoint) Task {
	h.t.Helper()
	t := h.submit(wps...)
	h.tick()
	t = h.want(t.ID, Assigned, robot)
	h.ack(robot, t.CmdID, "accepted")
	h.robot(robot).Mode, h.robot(robot).TaskID = "MOVING", t.CmdID
	return h.want(t.ID, Running, robot)
}

func TestEligibility(t *testing.T) {
	m := NewManager(DefaultConfig(), Deps{})
	base := idle("r1", 0, 0)
	for _, tc := range []struct {
		name   string
		change func(r *fleet.Robot)
		busy   bool
		want   bool
	}{
		{"idle online robot", func(*fleet.Robot) {}, false, true},
		{"STALE", func(r *fleet.Robot) { r.Conn = fleet.Stale }, false, false},
		{"OFFLINE", func(r *fleet.Robot) { r.Conn = fleet.Offline }, false, false},
		{"IN_ELEVATOR", func(r *fleet.Robot) { r.Conn = fleet.InElevator }, false, false},
		{"MOVING", func(r *fleet.Robot) { r.Mode = "MOVING" }, false, false},
		{"CHARGING", func(r *fleet.Robot) { r.Mode = "CHARGING" }, false, false},
		{"mode ERROR", func(r *fleet.Robot) { r.Mode = "ERROR" }, false, false},
		{"error set while IDLE", func(r *fleet.Robot) { r.Error = "bumper" }, false, false},
		{"battery below LowBattery", func(r *fleet.Robot) { r.Battery = 24.9 }, false, false},
		{"battery at LowBattery", func(r *fleet.Robot) { r.Battery = 25 }, false, true},
		{"reports a task", func(r *fleet.Robot) { r.TaskID = "t-9.0.1" }, false, false},
		{"holds another task", func(*fleet.Robot) {}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.change(&r)
			why := m.ineligible(r, map[string]bool{"r1": tc.busy}, time.Time{})
			if (why == "") != tc.want {
				t.Fatalf("eligible = %v (%q), want %v", why == "", why, tc.want)
			}
		})
	}
}

func TestNearestRobot(t *testing.T) {
	onFloor := func(r fleet.Robot, f int) fleet.Robot { r.Floor = f; return r }
	for _, tc := range []struct {
		name   string
		robots []fleet.Robot
		wp     Waypoint
		want   string
	}{
		{"nearest wins", []fleet.Robot{idle("a", 0, 0), idle("b", 5, 0), idle("c", 9, 0)},
			Waypoint{X: 6, Floor: 1}, "b"},
		{"other floor penalised even when closer",
			[]fleet.Robot{onFloor(idle("a", 6, 0), 2), idle("b", 50, 0)}, Waypoint{X: 6, Floor: 1}, "b"},
		{"other floor used when nothing else", []fleet.Robot{onFloor(idle("a", 6, 0), 2)},
			Waypoint{X: 0, Floor: 1}, "a"},
		{"tie goes to first name", []fleet.Robot{idle("b", 1, 0), idle("a", -1, 0)}, Waypoint{Floor: 1}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig(), tc.robots...)
			task := h.submit(tc.wp)
			h.tick()
			h.want(task.ID, Assigned, tc.want)
		})
	}
}

func TestQueueIsFIFO(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	first := h.submit(Waypoint{X: 9, Floor: 1})
	second := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	h.want(first.ID, Assigned, "a")
	h.want(second.ID, Queued, "")
}

func TestGotoCommand(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.submit(Waypoint{X: 3, Y: 4, Floor: 1})
	h.tick()
	cmds := h.sentTo("a")
	want := Command{CmdID: task.ID + ".0.1.b0", Type: "goto", X: 3, Y: 4,
		ExpiresAt: float64(h.now.Add(10*time.Second).UnixMilli()) / 1000}
	if len(cmds) != 1 || cmds[0] != want {
		t.Fatalf("sent %+v, want %+v", cmds, want)
	}
}

func TestResendSameCmdIDThenFail(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 50, 0))
	task := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	first := h.want(task.ID, Assigned, "a").CmdID

	h.advance(2750 * time.Millisecond)
	if n := len(h.sentTo("a")); n != 1 {
		t.Fatalf("resent before AckTimeout: %d sends", n)
	}
	h.advance(250 * time.Millisecond)
	cmds := h.sentTo("a")
	if len(cmds) != 2 || cmds[1].CmdID != first || cmds[1].ExpiresAt <= cmds[0].ExpiresAt {
		t.Fatalf("after 3 s: %+v, want a resend of %s with a later expiry", cmds, first)
	}
	h.advance(6 * time.Second) // resends 2 and 3
	if n := len(h.sentTo("a")); n != 4 {
		t.Fatalf("after 9 s: %d sends, want 4", n)
	}
	h.want(task.ID, Assigned, "a")

	// a stays ONLINE and IDLE but deaf: it failed the task and cools down,
	// so the farther robot b gets it.
	h.advance(3 * time.Second)
	got := h.want(task.ID, Assigned, "b")
	if got.Attempts != 1 || got.CmdID == first || got.Error == "" {
		t.Fatalf("after giving up: %+v, want attempt 1 and a new cmd_id", got)
	}
	for _, c := range h.sentTo("a") {
		if c.CmdID != first {
			t.Fatalf("unexpected command to a: %+v", c)
		}
	}
}

func TestStaleRobotPausesResends(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	h.robot("a").Conn = fleet.Stale
	h.advance(15 * time.Second) // STALE never reaches OFFLINE here
	h.want(task.ID, Assigned, "a")
	if n := len(h.sentTo("a")); n != 1 {
		t.Fatalf("%d sends while STALE, want no resends", n)
	}
}

func TestDoneAdvancesLegs(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.start("a", Waypoint{X: 1, Floor: 1}, Waypoint{X: 2, Floor: 1})
	leg0 := task.CmdID

	h.ack("a", leg0, "done")
	task = h.want(task.ID, Assigned, "a")
	if task.Leg != 1 || task.CmdID == leg0 {
		t.Fatalf("after leg 0 done: %+v", task)
	}
	cmds := h.sentTo("a")
	if last := cmds[len(cmds)-1]; last.CmdID != task.CmdID || last.X != 2 {
		t.Fatalf("next leg command %+v", last)
	}

	// A repeated "done" for leg 0 (QoS 1 redelivery, or the adapter answering
	// a resend) must not finish leg 1 as well.
	h.ack("a", leg0, "done")
	if got := h.task(task.ID); got.Leg != 1 || got.State != Assigned {
		t.Fatalf("duplicate done moved the task: %+v", got)
	}

	h.ack("a", task.CmdID, "accepted")
	h.ack("a", task.CmdID, "done")
	got := h.want(task.ID, Done, "a")
	if got.Leg != 2 || got.Finished.IsZero() || got.Started.IsZero() {
		t.Fatalf("done task %+v", got)
	}
	h.ack("a", task.CmdID, "done") // duplicate final done
	if h.task(task.ID).State != Done {
		t.Fatal("duplicate done changed a finished task")
	}
}

func TestRobotReportCountsAsAccepted(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	task = h.want(task.ID, Assigned, "a")
	h.robot("a").Mode, h.robot("a").TaskID = "MOVING", task.CmdID // "accepted" was lost
	h.tick()
	h.want(task.ID, Running, "a")
}

func TestLostDoneAckIsAskedAgain(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.start("a", Waypoint{X: 1, Floor: 1})
	h.advance(5 * time.Second)
	if n := len(h.sentTo("a")); n != 1 {
		t.Fatalf("%d sends while the robot reports the leg, want 1", n)
	}
	// The robot finished but the "done" ack never arrived.
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", ""
	h.advance(3 * time.Second)
	cmds := h.sentTo("a")
	if len(cmds) != 2 || cmds[1].CmdID != task.CmdID {
		t.Fatalf("sent %+v, want a resend of %s", cmds, task.CmdID)
	}
	h.ack("a", task.CmdID, "done") // the adapter repeats its last ack
	h.want(task.ID, Done, "a")
}

func TestFailedAndCancelledRequeueUntilMaxAttempts(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.submit(Waypoint{X: 1, Floor: 1})
	for i, status := range []string{"failed", "cancelled", "failed", "failed"} {
		h.tick()
		cur := h.want(task.ID, Assigned, "a")
		h.ack("a", cur.CmdID, "accepted")
		h.ack("a", cur.CmdID, status)
		got := h.task(task.ID)
		if got.Attempts != i+1 {
			t.Fatalf("after %s #%d: attempts %d", status, i+1, got.Attempts)
		}
		wantState := Queued
		if i == 3 {
			wantState = Failed
		}
		if got.State != wantState {
			t.Fatalf("after %s #%d: %s, want %s", status, i+1, got.State, wantState)
		}
	}
	h.tick()
	if got := h.task(task.ID); got.State != Failed || got.Error != "failed" {
		t.Fatalf("failed task %+v", got)
	}
	if kinds := eventKinds(h.events); kinds[len(kinds)-1] != "failed" {
		t.Fatalf("events %v", kinds)
	}
}

func TestStaleAndForeignAcksIgnored(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	task := h.start("a", Waypoint{X: 1, Floor: 1})
	old := task.CmdID
	h.ack("a", old, "failed")
	h.robot("a").Mode, h.robot("a").TaskID = "ERROR", "" // a is out; b takes the task
	h.tick()
	task = h.want(task.ID, Assigned, "b")

	for _, a := range []struct{ robot, cmd, status string }{
		{"a", old, "done"},              // late ack for the old attempt
		{"a", old, "cancelled"},         // another late ack
		{"a", task.CmdID, "done"},       // right cmd_id, wrong robot
		{"b", "t-99.0.1", "done"},       // unknown task
		{"b", "go1", "done"},            // not a task command at all
		{"b", task.CmdID + "x", "done"}, // near miss
	} {
		h.ack(a.robot, a.cmd, a.status)
	}
	got := h.want(task.ID, Assigned, "b")
	if got.Attempts != 1 || got.Leg != 0 {
		t.Fatalf("stale acks changed the task: %+v", got)
	}
}

func TestRobotLoss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(r *fleet.Robot)
		requeue bool
	}{
		{"OFFLINE requeues", func(r *fleet.Robot) { r.Conn = fleet.Offline }, true},
		{"ERROR requeues", func(r *fleet.Robot) { r.Mode, r.Error, r.TaskID = "ERROR", "bumper", "" }, true},
		{"STALE keeps", func(r *fleet.Robot) { r.Conn = fleet.Stale }, false},
		{"IN_ELEVATOR keeps", func(r *fleet.Robot) { r.Conn = fleet.InElevator }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
			task := h.start("a", Waypoint{X: 1, Floor: 1})
			tc.change(h.robot("a"))
			h.advance(30 * time.Second)
			got := h.task(task.ID)
			if tc.requeue {
				if got.State != Queued || got.Robot != "" || got.Attempts != 1 {
					t.Fatalf("got %+v, want requeued with attempt 1", got)
				}
			} else if got.State != Running || got.Robot != "a" || got.Attempts != 0 {
				t.Fatalf("got %+v, want still RUNNING on a", got)
			}
		})
	}
}

func TestReturnedRobotWithOldTaskIsCancelled(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	task := h.start("a", Waypoint{X: 1, Floor: 1})
	old := task.CmdID

	h.robot("a").Conn = fleet.Offline // still reports the goto it last had
	h.tick()
	task = h.want(task.ID, Assigned, "b")
	h.ack("b", task.CmdID, "accepted")
	h.robot("b").Mode, h.robot("b").TaskID = "MOVING", task.CmdID
	h.advance(5 * time.Second)
	if n := len(h.sentTo("a")); n != 1 {
		t.Fatalf("commands to an OFFLINE robot: %+v", h.sentTo("a"))
	}

	h.robot("a").Conn = fleet.Online // back, and still driving the old goto
	h.tick()
	cmds := h.sentTo("a")
	if len(cmds) != 2 || cmds[1].Type != "cancel" {
		t.Fatalf("sent to a: %+v, want a cancel", cmds)
	}
	h.advance(2 * time.Second)
	if len(h.sentTo("a")) != 2 {
		t.Fatal("cancel repeated before AckTimeout")
	}
	h.advance(time.Second) // cancel ack lost; robot still reports it
	if cmds := h.sentTo("a"); len(cmds) != 3 || cmds[2].Type != "cancel" || cmds[2].CmdID == cmds[1].CmdID {
		t.Fatalf("sent to a: %+v, want a second cancel with a new cmd_id", cmds)
	}

	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", "" // stopped
	h.ack("a", old, "cancelled")                        // must not touch the task
	h.advance(10 * time.Second)
	if n := len(h.sentTo("a")); n != 3 {
		t.Fatalf("%d commands to a after it stopped, want 3", n)
	}
	got := h.want(task.ID, Running, "b")
	if got.Attempts != 1 {
		t.Fatalf("attempts %d", got.Attempts)
	}
}

func TestUserCancel(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	running := h.start("a", Waypoint{X: 1, Floor: 1})
	queued := h.submit(Waypoint{X: 2, Floor: 1})

	got, err := h.m.Cancel(running.ID)
	if err != nil || got.State != Cancelled {
		t.Fatalf("cancel running: %+v %v", got, err)
	}
	cmds := h.sentTo("a")
	if last := cmds[len(cmds)-1]; last.Type != "cancel" {
		t.Fatalf("last command %+v, want cancel", last)
	}
	h.ack("a", cmds[len(cmds)-1].CmdID, "accepted")
	h.ack("a", running.CmdID, "cancelled") // the goto ends as cancelled
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", ""
	h.tick()
	if got := h.task(running.ID); got.State != Cancelled || got.Attempts != 0 {
		t.Fatalf("cancelled task was requeued: %+v", got)
	}
	h.want(queued.ID, Assigned, "a") // the robot is free for the next task

	if got, err := h.m.Cancel(queued.ID); err != nil || got.State != Cancelled {
		t.Fatalf("cancel assigned: %+v %v", got, err)
	}
	q2 := h.submit(Waypoint{X: 3, Floor: 1})
	if got, err := h.m.Cancel(q2.ID); err != nil || got.State != Cancelled {
		t.Fatalf("cancel queued: %+v %v", got, err)
	}
	if _, err := h.m.Cancel(q2.ID); !errors.Is(err, ErrFinished) {
		t.Fatalf("cancel twice: %v, want ErrFinished", err)
	}
	if _, err := h.m.Cancel("t-404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel unknown: %v, want ErrNotFound", err)
	}
}

func TestSubmitValidation(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	if _, err := h.m.Submit(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no waypoints: %v", err)
	}
	inf := math.Inf(1)
	if _, err := h.m.Submit([]Waypoint{{X: inf}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("infinite x: %v", err)
	}
}

func TestEventsAndPersistence(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	task := h.start("a", Waypoint{X: 1, Floor: 1}, Waypoint{X: 2, Floor: 1})
	h.ack("a", task.CmdID, "done")
	next := h.task(task.ID).CmdID
	h.ack("a", next, "accepted")
	h.ack("a", next, "done")

	want := []string{"assigned", "running", "leg_done", "running", "done"}
	if got := eventKinds(h.events); !equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if e := h.events[0]; e.Task != task.ID || e.Robot != "a" || e.From != Queued || e.To != Assigned {
		t.Fatalf("assigned event %+v", e)
	}
	if err := h.m.Flush(); err != nil {
		t.Fatal(err)
	}
	if s := h.store.tasks[task.ID]; s.State != Done || s.Leg != 2 {
		t.Fatalf("stored %+v", s)
	}
}

func eventKinds(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
