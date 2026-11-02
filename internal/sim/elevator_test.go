package sim_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/fleet"
	"fleetlab/internal/sim"
	"fleetlab/internal/task"
)

// liftWorld is a world with one elevator, E1 (floors 1-10, three slots,
// lobby (10, 0) on every floor, 1 s per floor, 1 s door), and checks that
// run after every sim step:
//   - never two robots in the door zone at once (one at a time),
//   - never one task driven by two robots (world.overlap),
//   - no robot shown OFFLINE (unless allowOffline).
type liftWorld struct {
	*world
	mu           sync.Mutex
	conns        []fleet.Event // registry events
	events       []task.Event
	allowOffline bool
	wps          map[string][]task.Waypoint
}

// liftTiming is the conductor timing of newLiftWorld; tests may change it
// (and restore it with t.Cleanup).
var liftTiming = elevator.DefaultTiming()

var liftCar = elevator.CarConfig{ID: "E1", Capacity: 3, Floors: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	Lobby: elevator.Point{X: 10, Y: 0}, FloorS: 1, DoorS: 1}

func newLiftWorld(t *testing.T, seed uint64, robots map[string][2]float64) *liftWorld {
	lw := &liftWorld{wps: map[string][]task.Waypoint{}}
	w := &world{t: t, now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), linkBack: map[string]time.Time{}}
	lw.world = w
	clock := func() time.Time { return w.now }
	w.reg = fleet.NewRegistry(fleet.DefaultConfig(), func(e fleet.Event) {
		lw.mu.Lock()
		lw.conns = append(lw.conns, e)
		lw.mu.Unlock()
		if e.Type == "conn" && e.To == fleet.Offline && !lw.allowOffline {
			t.Errorf("%s: %s shown OFFLINE (%s)", w.now.Format("15:04:05.000"), e.Robot, e.Detail)
		}
	})
	w.reg.SetClock(clock)
	w.fl = sim.New(w.reg, clock, 0.8, seed)
	w.fl.SetDelay(50*time.Millisecond, 200*time.Millisecond)
	names := make([]string, 0, len(robots))
	for n := range robots {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		w.fl.Add(n, robots[n][0], robots[n][1])
	}
	w.fl.AddElevator(liftCar)
	w.mgr = task.NewManager(task.DefaultConfig(), task.Deps{Now: clock, Robots: w.reg, Sender: w.fl,
		Elevators: elevator.Config{Elevators: []elevator.CarConfig{liftCar}, Timing: liftTiming},
		Lift:      w.fl,
		Emit: func(e task.Event) {
			lw.events = append(lw.events, e)
			lw.checkFloor(e)
			if w.onEvent != nil {
				w.onEvent(e)
			}
		}})
	w.fl.OnAck(w.mgr.HandleAck)
	w.fl.OnElevator(w.mgr.HandleElevatorState, w.mgr.HandleZone)
	w.fl.OnRobotEvent(w.mgr.HandleRobotEvent)
	w.mgr.LinkUp()
	w.checkOverlap = true
	w.fl.Step(0)
	return lw
}

func (lw *liftWorld) tick() {
	w := lw.world
	const sub = step / substeps
	for range substeps {
		w.now = w.now.Add(sub)
		w.fl.Step(sub)
		w.overlap()
		if z := w.fl.Car("E1").Zone; len(z) > 1 {
			w.t.Fatalf("%s: robots %v move through the door at once", w.now.Format("15:04:05.000"), z)
		}
	}
	w.reg.Tick()
	w.mgr.Tick()
	w.overlap()
}

func (lw *liftWorld) runUntil(what string, limit time.Duration, cond func() bool) {
	lw.t.Helper()
	for end := lw.now.Add(limit); lw.now.Before(end); {
		lw.tick()
		if cond() {
			return
		}
	}
	lw.t.Fatalf("not %s after %s; tasks %+v\nelevator %+v\nevents:\n%s", what, limit, lw.mgr.List(""), lw.mgr.Elevators(), lw.elevLog())
}

func (lw *liftWorld) run(d time.Duration) {
	for end := lw.now.Add(d); lw.now.Before(end); {
		lw.tick()
	}
}

// elev lists elevator events of kind as "robot detail".
func (lw *liftWorld) elev(kind string) []task.Event {
	var out []task.Event
	for _, e := range lw.events {
		if e.Type == "elevator" && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (lw *liftWorld) elevLog() string {
	var b strings.Builder
	for _, e := range lw.events {
		if e.Type == "elevator" || e.Type == "task" {
			fmt.Fprintf(&b, "  %s %s/%s %s %s\n", e.Time.Format("15:04:05.0"), e.Type, e.Kind, e.Robot, e.Detail)
		}
	}
	return b.String()
}

func fwp(x, y float64, floor int) task.Waypoint { return task.Waypoint{X: x, Y: y, Floor: floor} }

// near3 places three robots a step from the lobby, so they reach it within
// the batching window and ride together.
var near3 = map[string][2]float64{"robot_1": {8.5, 0}, "robot_2": {8.5, 1}, "robot_3": {8.5, -1}}

// submit records the waypoints so leg ends can be checked against the floor
// the robot is really on.
func (lw *liftWorld) submit(wps ...task.Waypoint) string {
	id := lw.world.submit(wps...)
	lw.wps[id] = wps
	return id
}

// checkFloor fails the test when a leg is reported finished while the robot
// is on another floor than the waypoint's.
func (lw *liftWorld) checkFloor(e task.Event) {
	if e.Type != "task" || e.Kind != "done" && e.Kind != "leg_done" {
		return
	}
	wps := lw.wps[e.Task]
	reached := len(wps) - 1
	if e.Kind == "leg_done" {
		var leg int
		fmt.Sscanf(e.Detail, "leg %d/", &leg)
		reached = leg - 1
	}
	if r := lw.fl.Robot(e.Robot); r.Floor != wps[reached].Floor {
		lw.t.Errorf("%s: %s %s by %s on floor %d, waypoint %d is on floor %d",
			lw.now.Format("15:04:05.000"), e.Task, e.Kind, e.Robot, r.Floor, reached, wps[reached].Floor)
	}
}

func (lw *liftWorld) robotOf(id string) string {
	lw.runUntil(id+" assigned", 5*time.Second, func() bool { return lw.task(id).Robot != "" })
	return lw.task(id).Robot
}

func TestRideThreeFloorsTogether(t *testing.T) {
	lw := newLiftWorld(t, 1, near3)
	ids := map[int]string{}
	for _, fl := range []int{3, 7, 5} {
		ids[fl] = lw.submit(fwp(12, 4, fl))
	}
	lw.runUntil("all DONE", 3*time.Minute, lw.allDone)

	if n := len(lw.elev("trip_planned")); n != 1 {
		t.Fatalf("%d trips, want 1 shared trip\n%s", n, lw.elevLog())
	}
	robotFor := map[int]string{}
	for fl, id := range ids {
		tk := lw.task(id)
		robotFor[fl] = tk.Robot
		if r := lw.fl.Robot(tk.Robot); r.Floor != fl || r.X != 12 || r.Y != 4 {
			t.Errorf("%s for floor %d ended at floor %d (%.1f, %.1f)", tk.Robot, fl, r.Floor, r.X, r.Y)
		}
		if tk.Attempts != 0 || tk.Dispatches != 1 {
			t.Errorf("%s: %d attempts, %d dispatches", id, tk.Attempts, tk.Dispatches)
		}
	}
	// Board 7, 5, 3; exit 3, 5, 7. The door log shows strict alternation:
	// each robot's start is followed by its own clear before the next start.
	want := []string{
		robotFor[7] + " board start", robotFor[7] + " board clear",
		robotFor[5] + " board start", robotFor[5] + " board clear",
		robotFor[3] + " board start", robotFor[3] + " board clear",
		robotFor[3] + " exit start", robotFor[3] + " exit clear",
		robotFor[5] + " exit start", robotFor[5] + " exit clear",
		robotFor[7] + " exit start", robotFor[7] + " exit clear",
	}
	car := lw.fl.Car("E1")
	if !slices.Equal(car.Log, want) {
		t.Fatalf("door log\n %v\nwant\n %v", car.Log, want)
	}
	if car.MaxZone != 1 {
		t.Fatalf("max robots in the door zone %d, want 1", car.MaxZone)
	}
	// Exits happened at the right floors, the door robot first.
	var exits []string
	for _, e := range lw.elev("exited") {
		exits = append(exits, e.Robot)
	}
	if !slices.Equal(exits, []string{robotFor[3], robotFor[5], robotFor[7]}) {
		t.Fatalf("exit order %v", exits)
	}
	for _, e := range lw.elev("exited") {
		fl := 0
		for f, r := range robotFor {
			if r == e.Robot {
				fl = f
			}
		}
		if !strings.Contains(e.Detail, "at floor "+strconv.Itoa(fl)) {
			t.Errorf("exit of %s: %s, want floor %d", e.Robot, e.Detail, fl)
		}
	}
	// Each robot was IN_ELEVATOR (announced), and never OFFLINE (checked live).
	inElev := map[string]bool{}
	for _, e := range lw.conns {
		if e.Type == "conn" && e.To == fleet.InElevator {
			inElev[e.Robot] = true
		}
		if e.Type == "elevator_overdue" {
			t.Errorf("overdue: %+v", e)
		}
	}
	if len(inElev) != 3 {
		t.Fatalf("IN_ELEVATOR for %v, want all three", inElev)
	}
	// Robot events from the blackout arrive newest first; they are recorded
	// in seq order.
	last := map[string]int{}
	n := 0
	for _, e := range lw.elev("robot_event") {
		var seq int
		fmt.Sscanf(e.Detail, "#%d", &seq)
		if seq <= last[e.Robot] {
			t.Fatalf("robot events of %s out of order: #%d after #%d", e.Robot, seq, last[e.Robot])
		}
		last[e.Robot] = seq
		n++
	}
	if n < 9 {
		t.Fatalf("%d robot events recorded, want accepted+boarded+exited for each", n)
	}
	if _, ok := lw.fl.LastTrip("E1"), true; ok && lw.fl.LastTrip("E1").Stage != elevator.StageDone {
		t.Fatalf("last trip stage %s", lw.fl.LastTrip("E1").Stage)
	}
}

func TestRideExitFailsOnceThenSucceeds(t *testing.T) {
	lw := newLiftWorld(t, 2, near3)
	id5 := lw.submit(fwp(12, 4, 5))
	r5 := lw.robotOf(id5)
	lw.fl.FailExits(r5, 1)
	lw.submit(fwp(12, 4, 3))
	lw.submit(fwp(12, 4, 7))
	lw.runUntil("all DONE", 3*time.Minute, lw.allDone)
	fails := lw.elev("exit_failed")
	if len(fails) != 1 || fails[0].Robot != r5 || !strings.Contains(fails[0].Detail, "retrying") {
		t.Fatalf("exit_failed %+v, want one retry for %s", fails, r5)
	}
	if n := len(lw.elev("replanned")); n != 0 {
		t.Fatalf("%d replans, want none after a successful retry", n)
	}
	if r := lw.fl.Robot(r5); r.Floor != 5 {
		t.Fatalf("%s at floor %d", r5, r.Floor)
	}
	if lw.task(id5).Attempts != 0 {
		t.Fatalf("attempt used: %+v", lw.task(id5))
	}
}

// A robot that cannot get out twice is carried to the next stop; it comes
// out on the wrong floor and rides back. Nobody else is held up.
func TestRideExitFailsTwiceIsCarried(t *testing.T) {
	lw := newLiftWorld(t, 3, near3)
	id3 := lw.submit(fwp(12, 4, 3))
	r3 := lw.robotOf(id3)
	lw.fl.FailExits(r3, 2)
	id5 := lw.submit(fwp(12, 4, 5))
	id7 := lw.submit(fwp(12, 4, 7))
	lw.runUntil("all DONE", 4*time.Minute, lw.allDone)

	var carried, routed bool
	for _, e := range lw.elev("replanned") {
		if e.Robot == r3 && strings.Contains(e.Detail, "exits at floor 5 instead of 3") {
			carried = true
		}
		if e.Robot == r3 && strings.Contains(e.Detail, "came out at floor 5, not 3") {
			routed = true
		}
	}
	if !carried || !routed {
		t.Fatalf("carried %v, rerouted %v\n%s", carried, routed, lw.elevLog())
	}
	if n := len(lw.elev("trip_planned")); n != 2 {
		t.Fatalf("%d trips, want 2 (the shared one, then %s back down)", n, r3)
	}
	for id, fl := range map[string]int{id3: 3, id5: 5, id7: 7} {
		tk := lw.task(id)
		if r := lw.fl.Robot(tk.Robot); r.Floor != fl {
			t.Errorf("%s ended on floor %d, want %d", tk.Robot, r.Floor, fl)
		}
		if tk.Attempts != 0 {
			t.Errorf("%s used %d attempts", id, tk.Attempts)
		}
	}
	// At floor 5 the carried robot (in front) got out before the robot that
	// was planned for 5.
	car := lw.fl.Car("E1")
	r5 := lw.task(id5).Robot
	i3 := slices.Index(car.Log, r3+" exit clear")
	i5 := slices.Index(car.Log, r5+" exit clear")
	if i3 < 0 || i5 < 0 || i3 > i5 {
		t.Fatalf("door log %v: %s should exit before %s at floor 5", car.Log, r3, r5)
	}
}

// The door at the last stop is blocked: the robot cannot get out, the car
// stays reserved (no other trip), its task goes to another robot, and the
// car is freed only when the robot is confirmed out (a human helped it).
func TestRideStuckAtLastStopNeedsHuman(t *testing.T) {
	lw := newLiftWorld(t, 4, near3)
	lw.allowOffline = true // the stuck robot is silent for minutes
	id7 := lw.submit(fwp(12, 4, 7))
	r7 := lw.robotOf(id7)
	lw.submit(fwp(12, 4, 3))
	lw.fl.BlockDoor("E1", 7, true)
	lw.runUntil("stuck alarm", 3*time.Minute, func() bool {
		for _, e := range lw.elev("exit_failed") {
			if strings.Contains(e.Detail, "needs a human") {
				return true
			}
		}
		return false
	})
	lw.runUntil("task handed to another robot", 30*time.Second, func() bool {
		tk := lw.task(id7)
		return tk.Robot != r7 && tk.Robot != ""
	})
	if tk := lw.task(id7); tk.Attempts != 1 {
		t.Fatalf("attempts %d, want 1 (stuck counts)", tk.Attempts)
	}
	lw.run(90 * time.Second)
	if tr, ok := tripOf(lw.mgr, "E1"); !ok || tr.Stage != elevator.StageBlocked {
		t.Fatalf("car not kept blocked: %+v", tr)
	}
	if n := len(lw.elev("trip_planned")); n != 1 {
		t.Fatalf("%d trips while the car holds a stuck robot", n)
	}
	// A human gets it out; it reconnects at floor 7 and the car is free.
	lw.fl.BlockDoor("E1", 7, false)
	lw.fl.Rescue(r7)
	lw.runUntil("all DONE", 3*time.Minute, lw.allDone)
	if r := lw.fl.Robot(lw.task(id7).Robot); r.Floor != 7 {
		t.Fatalf("task 7 done on floor %d", r.Floor)
	}
	if len(lw.elev("released")) < 2 {
		t.Fatalf("car not released twice:\n%s", lw.elevLog())
	}
}

func tripOf(m *task.Manager, car string) (elevator.Trip, bool) {
	for _, v := range m.Elevators() {
		if v.ID == car && v.Trip != nil {
			return *v.Trip, true
		}
	}
	return elevator.Trip{}, false
}

// The car does not answer: the trip is aborted before anyone boards, the
// robots wait, and ride once the car is back.
func TestRideElevatorUnresponsiveAborts(t *testing.T) {
	lw := newLiftWorld(t, 5, near3)
	lw.fl.SetUnresponsive("E1", true)
	lw.submit(fwp(12, 4, 3))
	lw.submit(fwp(12, 4, 5))
	lw.runUntil("aborted", time.Minute, func() bool { return len(lw.elev("aborted")) > 0 })
	if d := lw.elev("aborted")[0].Detail; !strings.Contains(d, "not responding") {
		t.Fatalf("abort reason %q", d)
	}
	lw.run(20 * time.Second)
	if n := len(lw.elev("boarding")); n != 0 {
		t.Fatalf("%d boarding turns with a dead car", n)
	}
	for robot, st := range lw.fl.Riding() {
		t.Fatalf("%s still holds a ride (%s) after the abort", robot, st)
	}
	if n := len(lw.elev("trip_planned")); n != 1 {
		t.Fatalf("%d trips planned during the back-off", n)
	}
	lw.fl.SetUnresponsive("E1", false)
	lw.runUntil("all DONE", 3*time.Minute, lw.allDone)
	for _, tk := range lw.mgr.List("") {
		if tk.Attempts != 0 {
			t.Errorf("%s: %d attempts; a dead car is not the robot's fault", tk.ID, tk.Attempts)
		}
	}
}

// A robot reports another floor than planned after it came out: its route
// is planned again from there.
func TestRideWrongFloorExitIsReplanned(t *testing.T) {
	lw := newLiftWorld(t, 6, near3)
	id := lw.submit(fwp(12, 4, 6))
	r := lw.robotOf(id)
	lw.fl.WrongFloor(r, 4)
	lw.runUntil("DONE", 3*time.Minute, lw.allDone)
	found := false
	for _, e := range lw.elev("replanned") {
		found = found || e.Robot == r && strings.Contains(e.Detail, "came out at floor 4, not 6")
	}
	if !found {
		t.Fatalf("no replan:\n%s", lw.elevLog())
	}
	if got := lw.fl.Robot(r); got.Floor != 6 {
		t.Fatalf("%s on floor %d, want 6", r, got.Floor)
	}
	if n := len(lw.elev("trip_planned")); n != 2 {
		t.Fatalf("%d trips, want 2", n)
	}
}

// Wi-Fi comes back later than announced: the server raises
// elevator_overdue and shows STALE, not OFFLINE, and the task goes on.
func TestRideLinkLaterThanAnnounced(t *testing.T) {
	lw := newLiftWorld(t, 7, near3)
	id := lw.submit(fwp(12, 4, 4))
	r := lw.robotOf(id)
	lw.fl.ExtraSilence(r, 40*time.Second)
	lw.runUntil("DONE", 3*time.Minute, lw.allDone)
	overdue, stale := false, false
	for _, e := range lw.conns {
		overdue = overdue || e.Type == "elevator_overdue" && e.Robot == r
		stale = stale || e.Type == "conn" && e.Robot == r && e.To == fleet.Stale
	}
	if !overdue || !stale {
		t.Fatalf("overdue %v, stale %v: %+v", overdue, stale, lw.conns)
	}
	if tk := lw.task(id); tk.Attempts != 0 || tk.Robot != r {
		t.Fatalf("task %+v, want done by %s without a failed attempt", tk, r)
	}
}

// Much later than announced: OFFLINE after the ride, so the task goes to
// another robot (the robot is out of the car, the usual rules apply).
func TestRideLinkLostAfterExit(t *testing.T) {
	lw := newLiftWorld(t, 8, near3)
	lw.allowOffline = true
	id := lw.submit(fwp(12, 4, 4))
	r := lw.robotOf(id)
	lw.fl.ExtraSilence(r, 70*time.Second)
	lw.runUntil("DONE", 5*time.Minute, lw.allDone)
	tk := lw.task(id)
	if tk.Robot == r || tk.Attempts != 1 {
		t.Fatalf("task %+v, want another robot after one attempt", tk)
	}
	if got := lw.fl.Robot(r); got.Goal != "" {
		t.Fatalf("%s still holds %s", r, got.Goal)
	}
}

// Ten tasks over several floors, four robots spread over two floors' worth
// of driving. Everything finishes; the per-step checks prove one robot at a
// time in the door and one robot per task.
func TestRideMixedLoad(t *testing.T) {
	robots := map[string][2]float64{"robot_1": {0, 0}, "robot_2": {3, 2}, "robot_3": {6, -2}, "robot_4": {9, 3}}
	lw := newLiftWorld(t, 9, robots)
	lw.submit(fwp(2, 2, 3))
	lw.submit(fwp(4, 1, 5), fwp(1, 1, 2))
	lw.submit(fwp(5, 5, 1))
	lw.submit(fwp(3, -2, 7))
	lw.submit(fwp(6, 3, 3))
	lw.submit(fwp(1, 4, 9), fwp(2, 2, 9))
	lw.submit(fwp(7, 0, 2))
	lw.submit(fwp(0, 3, 6), fwp(4, 4, 1))
	lw.submit(fwp(8, -1, 4))
	lw.submit(fwp(2, 6, 8))
	lw.runUntil("all DONE", 20*time.Minute, lw.allDone)
	car := lw.fl.Car("E1")
	if car.MaxZone != 1 {
		t.Fatalf("max in door zone %d", car.MaxZone)
	}
	trips := len(lw.elev("trip_planned"))
	shared := 0
	for _, e := range lw.elev("trip_planned") {
		_, after, _ := strings.Cut(e.Detail, "board ")
		riders, _, _ := strings.Cut(after, ";")
		if strings.Contains(riders, ",") {
			shared++
		}
	}
	if shared == 0 {
		t.Fatalf("no shared trip in %d; the load does not exercise sharing", trips)
	}
	for _, tk := range lw.mgr.List("") {
		if tk.Attempts != 0 {
			t.Errorf("%s: %d attempts (%s)", tk.ID, tk.Attempts, tk.Error)
		}
	}
	for _, name := range []string{"robot_1", "robot_2", "robot_3", "robot_4"} {
		if r := lw.fl.Robot(name); hasDuplicate(r.Started) {
			t.Errorf("%s ran a command twice: %v", name, r.Started)
		}
	}
	t.Logf("%d trips, %d shared, finished at +%s", trips, shared, lw.now.Sub(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)))
}
