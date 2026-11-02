package sim_test

import (
	"strings"
	"testing"
	"time"

	"fleetlab/internal/fleet"
	"fleetlab/internal/sim"
	"fleetlab/internal/task"
)

// world wires a real Registry and Manager to a simulated fleet of three
// robots docked at (0,0), (2,0), (4,0), all on one fake clock.
type world struct {
	t   *testing.T
	now time.Time
	reg *fleet.Registry
	mgr *task.Manager
	fl  *sim.Fleet

	// checkOverlap fails the test as soon as two robots drive the same task.
	// A robot whose link is down cannot be stopped by the server, so it is
	// left out of the check until reconnectGrace after its link returns.
	checkOverlap bool
	linkBack     map[string]time.Time
}

const (
	step           = 250 * time.Millisecond // server tick
	substeps       = 5                      // sim steps per tick; overlap is checked after each
	reconnectGrace = 2 * time.Second
)

func newWorld(t *testing.T, seed uint64) *world {
	w := &world{t: t, now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), linkBack: map[string]time.Time{}}
	clock := func() time.Time { return w.now }
	w.reg = fleet.NewRegistry(fleet.DefaultConfig(), nil)
	w.reg.SetClock(clock)
	w.fl = sim.New(w.reg, clock, 0.8, seed)
	// Commands and acks take 50-350 ms, so they overlap and arrive out of
	// order (a cancel can overtake its goto).
	w.fl.SetDelay(50*time.Millisecond, 300*time.Millisecond)
	for i, name := range []string{"robot_1", "robot_2", "robot_3"} {
		w.fl.Add(name, float64(2*i), 0)
	}
	w.mgr = task.NewManager(task.DefaultConfig(), task.Deps{Now: clock, Robots: w.reg, Sender: w.fl})
	w.fl.OnAck(w.mgr.HandleAck)
	w.mgr.LinkUp()
	w.fl.Step(0) // first reports: poses only, states follow
	return w
}

// tick is one 250 ms server tick, in fleetd's order, with the robots moving
// (and the overlap check running) in finer steps in between.
func (w *world) tick() {
	const sub = step / substeps
	for range substeps {
		w.now = w.now.Add(sub)
		w.fl.Step(sub)
		w.overlap()
	}
	w.reg.Tick()
	w.mgr.Tick()
	w.overlap()
}

func (w *world) overlap() {
	if !w.checkOverlap {
		return
	}
	owner := map[string]string{}
	for robot, cmd := range w.fl.Driving() {
		if w.fl.Silent(robot) || w.now.Sub(w.linkBack[robot]) < reconnectGrace {
			continue
		}
		id, _, _ := strings.Cut(cmd, ".")
		if other, ok := owner[id]; ok {
			w.t.Fatalf("%s: task %s driven by %s and %s at once", w.now.Format("15:04:05.000"), id, other, robot)
		}
		owner[id] = robot
	}
}

func (w *world) setSilent(name string, silent bool) {
	w.fl.SetSilent(name, silent)
	if !silent {
		w.linkBack[name] = w.now
	}
}

func (w *world) run(d time.Duration) {
	for end := w.now.Add(d); w.now.Before(end); {
		w.tick()
	}
}

// runUntil ticks until cond holds; it fails the test after limit.
func (w *world) runUntil(what string, limit time.Duration, cond func() bool) {
	w.t.Helper()
	for end := w.now.Add(limit); w.now.Before(end); {
		w.tick()
		if cond() {
			return
		}
	}
	w.t.Fatalf("not %s after %s; tasks: %+v", what, limit, w.mgr.List(""))
}

func (w *world) submit(wps ...task.Waypoint) string {
	w.t.Helper()
	tk, err := w.mgr.Submit(wps)
	if err != nil {
		w.t.Fatal(err)
	}
	return tk.ID
}

func (w *world) task(id string) task.Task {
	tk, _ := w.mgr.Get(id)
	return tk
}

func (w *world) allDone() bool {
	for _, tk := range w.mgr.List("") {
		if tk.State != task.Done {
			return false
		}
	}
	return true
}

func wp(x, y float64) task.Waypoint { return task.Waypoint{X: x, Y: y, Floor: 1} }

// tenTasks submits a mix of one- and two-leg tasks around the docks.
func (w *world) tenTasks() {
	w.submit(wp(1, 3))
	w.submit(wp(5, 2), wp(6, 5))
	w.submit(wp(-2, 4))
	w.submit(wp(3, 6))
	w.submit(wp(0, -3), wp(2, -4))
	w.submit(wp(7, 0))
	w.submit(wp(4, 4))
	w.submit(wp(-1, -1), wp(1, 1))
	w.submit(wp(6, -2))
	w.submit(wp(2, 8))
}

func TestTenTasksThreeRobots(t *testing.T) {
	w := newWorld(t, 1)
	w.checkOverlap = true
	w.tenTasks()
	w.runUntil("all DONE", 5*time.Minute, w.allDone)

	perRobot := map[string]int{}
	for _, tk := range w.mgr.List("") {
		perRobot[tk.Robot]++
		if tk.Attempts != 0 || tk.Dispatches != 1 {
			t.Errorf("%s: %d attempts, %d dispatches; want a clean single run", tk.ID, tk.Attempts, tk.Dispatches)
		}
	}
	for _, name := range []string{"robot_1", "robot_2", "robot_3"} {
		if perRobot[name] == 0 {
			t.Errorf("%s did no task: %v", name, perRobot)
		}
		if r := w.fl.Robot(name); hasDuplicate(r.Started) {
			t.Errorf("%s ran a goto twice: %v", name, r.Started)
		}
	}
}

func TestFaultMidTaskIsReassigned(t *testing.T) {
	w := newWorld(t, 2)
	w.checkOverlap = true
	id := w.submit(wp(0, 8)) // robot_1 is nearest
	w.runUntil("RUNNING", 5*time.Second, func() bool { return w.task(id).State == task.Running })
	if tk := w.task(id); tk.Robot != "robot_1" {
		t.Fatalf("assigned to %s, want robot_1", tk.Robot)
	}
	w.run(3 * time.Second)
	w.fl.Fault("robot_1", "bumper")

	w.runUntil("reassigned", 2*time.Second, func() bool {
		tk := w.task(id)
		return tk.Robot != "" && tk.Robot != "robot_1"
	})
	w.runUntil("DONE", time.Minute, func() bool { return w.task(id).State == task.Done })
	if tk := w.task(id); tk.Robot == "robot_1" || tk.Attempts != 1 {
		t.Fatalf("done task %+v, want another robot after one failed attempt", tk)
	}
}

func TestSilentRobotIsReplacedAndCancelledOnReturn(t *testing.T) {
	w := newWorld(t, 3)
	w.checkOverlap = true
	id := w.submit(wp(0, 30)) // robot_1, about 37 s of driving
	w.runUntil("RUNNING", 5*time.Second, func() bool { return w.task(id).State == task.Running })
	first := w.task(id).CmdID
	w.run(2 * time.Second)

	w.setSilent("robot_1", true)
	start := w.now
	w.runUntil("reassigned", 25*time.Second, func() bool {
		tk := w.task(id)
		return tk.Robot != "" && tk.Robot != "robot_1"
	})
	if gap := w.now.Sub(start); gap < fleet.DefaultConfig().OfflineAfter {
		t.Fatalf("reassigned after %s, before the robot was OFFLINE", gap)
	}
	if r, _ := w.reg.Get("robot_1"); r.Conn != fleet.Offline {
		t.Fatalf("robot_1 conn %s, want OFFLINE", r.Conn)
	}
	w.run(5 * time.Second)
	if r := w.fl.Robot("robot_1"); r.Goal != first {
		t.Fatalf("robot_1 should still be driving %s while silent, has %q", first, r.Goal)
	}

	w.setSilent("robot_1", false) // back, still on the old goto
	w.runUntil("robot_1 stopped", 5*time.Second, func() bool { return w.fl.Robot("robot_1").Goal == "" })
	if r := w.fl.Robot("robot_1"); r.Y >= 30 || r.Mode != "IDLE" {
		t.Fatalf("robot_1 %+v, want stopped short of the goal", r)
	}
	w.runUntil("DONE", 2*time.Minute, func() bool { return w.task(id).State == task.Done })
	if tk := w.task(id); tk.Robot == "robot_1" {
		t.Fatalf("done by %s, want the replacement", tk.Robot)
	}
}

func TestDroppedAcksStillFinishEverything(t *testing.T) {
	w := newWorld(t, 4)
	w.checkOverlap = true
	w.fl.DropAcks(0.3)
	w.tenTasks()
	w.runUntil("all DONE", 10*time.Minute, w.allDone)
	if n := w.fl.Dropped(); n < 5 {
		t.Fatalf("only %d acks dropped; the test is not exercising loss", n)
	}
	t.Logf("%d acks dropped, finished at +%s", w.fl.Dropped(), w.now.Sub(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)))
	for _, name := range []string{"robot_1", "robot_2", "robot_3"} {
		if r := w.fl.Robot(name); hasDuplicate(r.Started) {
			t.Errorf("%s ran a goto twice: %v", name, r.Started)
		}
	}
}

func hasDuplicate(ids []string) bool {
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return true
		}
		seen[id] = true
	}
	return false
}

// A user cancel right after assignment can overtake its goto; the robot then
// starts driving a cancelled task and must be stopped again.
func TestCancelOvertakenByGotoIsCancelledAgain(t *testing.T) {
	overtaken := 0
	for seed := uint64(1); seed <= 20; seed++ {
		w := newWorld(t, seed)
		w.fl.SetDelay(0, 1500*time.Millisecond)
		w.run(time.Second) // robots report state
		id := w.submit(wp(0, 20))
		w.runUntil("ASSIGNED", 2*time.Second, func() bool { return w.task(id).State == task.Assigned })
		if _, err := w.mgr.Cancel(id); err != nil {
			t.Fatal(err)
		}
		w.run(20 * time.Second)
		if r := w.fl.Robot(w.task(id).Robot); r.IdleCancels > 0 && len(r.Started) > 0 {
			overtaken++
		}
		if d := w.fl.Driving(); len(d) != 0 || w.task(id).State != task.Cancelled {
			t.Fatalf("seed %d: still driving %v, task %s", seed, d, w.task(id).State)
		}
	}
	if overtaken == 0 {
		t.Fatal("no seed made the goto arrive after the cancel; the test proves nothing")
	}
	t.Logf("goto overtook the cancel in %d of 20 runs", overtaken)
}
