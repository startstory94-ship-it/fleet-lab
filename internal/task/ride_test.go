package task

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/fleet"
)

type liftLink struct {
	cmds   []elevator.Cmd
	trips  []elevator.Trip
	turns  []elevator.Turn
	clears int
}

func (l *liftLink) Command(car string, c elevator.Cmd) error {
	l.cmds = append(l.cmds, c)
	return nil
}
func (l *liftLink) PublishTrip(car string, t elevator.Trip) error {
	l.trips = append(l.trips, t)
	return nil
}
func (l *liftLink) PublishTurn(car string, t elevator.Turn) error {
	l.turns = append(l.turns, t)
	return nil
}
func (l *liftLink) ClearTrip(car string) error {
	l.clears++
	return nil
}

var rideCar = elevator.CarConfig{ID: "E1", Capacity: 3, Floors: []int{1, 2, 3, 4, 5, 6},
	Lobby: elevator.Point{X: 10, Y: 0}, Lobbies: map[string]elevator.Point{"3": {X: 11, Y: 1}}, FloorS: 1, DoorS: 1}

type rideH struct {
	*harness
	link  *liftLink
	carSq uint64
}

// newRideH is a harness whose manager has one car, E1 (floors 1-6, lobby
// (10, 0), (11, 1) on floor 3).
func newRideH(t *testing.T, cfg Config, robots ...fleet.Robot) *rideH {
	h := newHarness(t, cfg, robots...)
	rh := &rideH{harness: h, link: &liftLink{}}
	h.m = rh.manager("b0")
	return rh
}

func (rh *rideH) manager(boot string) *Manager {
	h := rh.harness
	return NewManager(h.m.cfg, Deps{
		Now: func() time.Time { return h.now }, Robots: h.robots, Sender: h.sender, Store: h.store,
		Emit: func(e Event) { h.events = append(h.events, e) }, Boot: boot,
		Elevators: elevator.Config{Elevators: []elevator.CarConfig{rideCar}, Timing: elevator.DefaultTiming()},
		Lift:      rh.link,
	})
}

func (rh *rideH) last(robot string) Command {
	rh.t.Helper()
	cs := rh.sentTo(robot)
	if len(cs) == 0 {
		rh.t.Fatalf("nothing sent to %s", robot)
	}
	return cs[len(cs)-1]
}

func (rh *rideH) carState(floor int, door string) {
	rh.carSq++
	trip := ""
	if v := rh.m.Elevators(); v[0].Trip != nil {
		trip = v[0].Trip.ID
	}
	rh.m.HandleElevatorState("E1", elevator.State{Boot: "c", Seq: rh.carSq, Floor: floor, Door: door, Trip: trip})
}

func (rh *rideH) trip() elevator.Trip {
	rh.t.Helper()
	v := rh.m.Elevators()[0]
	if v.Trip == nil {
		rh.t.Fatal("no trip")
	}
	return *v.Trip
}

// answer replies to the current turn.
func (rh *rideH) answer(event string) {
	rh.t.Helper()
	tn := rh.trip().Turn
	if tn == nil {
		rh.t.Fatal("no turn")
	}
	rh.m.HandleZone("E1", elevator.Zone{Trip: tn.Trip, N: tn.N, Robot: tn.Robot, Action: tn.Action, Event: event, Floor: tn.Floor})
	if v := rh.m.Elevators()[0]; v.Trip != nil && v.Trip.Floor == tn.Floor && v.Trip.Stage != elevator.StageTravel {
		rh.carState(tn.Floor, elevator.DoorOpen) // the car: door zone empty again
	}
}

func (rh *rideH) ackFloor(robot, cmdID, status string, floor int) {
	rh.m.HandleAck(robot, Ack{CmdID: cmdID, Status: status, Floor: &floor})
}

// toLobbyAndWait assigns id to robot, drives it to the lobby (acks) and
// lets the batch window pass. It returns the ride command.
func (rh *rideH) toRide(id, robot string) Command {
	rh.t.Helper()
	rh.tick()
	g := rh.last(robot)
	rh.ack(robot, g.CmdID, "accepted")
	rh.ack(robot, g.CmdID, "done")
	rh.carState(1, elevator.DoorClosed)
	rh.advance(elevator.DefaultTiming().BatchWindow)
	r := rh.last(robot)
	if r.Type != "ride" {
		rh.t.Fatalf("last command %+v, want a ride", r)
	}
	return r
}

func (rh *rideH) elevKinds() string {
	var b []string
	for _, e := range rh.events {
		if e.Type == "elevator" {
			b = append(b, e.Kind)
		}
	}
	return strings.Join(b, " ")
}

func TestRideLegPhases(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 5, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	rh.tick()
	g := rh.last("r1")
	if g.Type != "goto" || g.X != 10 || g.Y != 0 || !strings.HasSuffix(g.CmdID, ".L1") {
		t.Fatalf("first command %+v, want a goto to the lobby (10, 0) with an .L id", g)
	}
	rh.want(tk.ID, Assigned, "r1")
	rh.ack("r1", g.CmdID, "accepted")
	rh.ack("r1", g.CmdID, "done")
	if got := rh.task(tk.ID); got.Phase != PhaseWait || got.CmdID != "" || got.State != Running {
		t.Fatalf("after the lobby: %+v", got)
	}
	rh.carState(1, elevator.DoorClosed)
	rh.advance(2 * time.Second)
	if n := len(rh.sentTo("r1")); n != 1 {
		t.Fatalf("ride sent inside the batch window")
	}
	rh.advance(time.Second)
	ride := rh.last("r1")
	if ride.Type != "ride" || ride.Ride == nil || !strings.HasSuffix(ride.CmdID, ".R2") {
		t.Fatalf("ride %+v", ride)
	}
	p := ride.Ride
	if p.ExitFloor != 4 || p.Origin != 1 || p.Slot != 0 || p.Board != 0 || p.Elevator != "E1" || p.SilenceS <= 0 ||
		p.Lobby != (elevator.Point{X: 10}) || len(p.Boarding) != 1 || p.Direction != "up" {
		t.Fatalf("plan %+v", p)
	}
	rh.carState(1, elevator.DoorOpen)
	if len(rh.link.turns) != 0 {
		t.Fatal("turn before the ride was accepted")
	}
	rh.ack("r1", ride.CmdID, "accepted")
	if len(rh.link.turns) != 1 || rh.link.turns[0].Action != elevator.ActBoard {
		t.Fatalf("turns %+v", rh.link.turns)
	}
	// It boards, announces its blackout and goes silent for a long time:
	// the task is the conductor's, not failed.
	rh.robot("r1").Conn, rh.robot("r1").TaskID = fleet.InElevator, ride.CmdID
	rh.answer(elevator.ZoneClear)
	rh.advance(30 * time.Second)
	rh.robot("r1").Conn = fleet.Offline // way past its blackout
	rh.advance(30 * time.Second)
	if got := rh.task(tk.ID); got.State != Running || got.Attempts != 0 {
		t.Fatalf("task while riding: %+v", got)
	}
	rh.carState(4, elevator.DoorOpen)
	rh.answer(elevator.ZoneClear)
	if v := rh.m.Elevators()[0]; v.Trip != nil {
		t.Fatalf("car not released: %+v", v.Trip)
	}
	// fleetd's own retained trip coming back late must not revive the trip.
	rh.m.RestoreTrip("E1", rh.link.trips[2])
	if v := rh.m.Elevators()[0]; v.Trip != nil {
		t.Fatalf("own echo adopted as a restored trip: %+v", v.Trip)
	}
	// Back on the fleet link at floor 4: done, then the goto to the waypoint.
	r := rh.robot("r1")
	r.Conn, r.Floor, r.TaskID, r.Mode = fleet.Online, 4, "", "IDLE"
	rh.ackFloor("r1", ride.CmdID, "done", 4)
	g2 := rh.last("r1")
	if g2.Type != "goto" || g2.X != 3 || g2.Y != 3 || strings.Contains(g2.CmdID, ".L") || strings.Contains(g2.CmdID, ".R") {
		t.Fatalf("after the ride: %+v", g2)
	}
	rh.ack("r1", g2.CmdID, "done")
	rh.want(tk.ID, Done, "r1")
	if k := rh.elevKinds(); k != "trip_planned called boarding boarded departed overdue stop exiting exited released" {
		t.Fatalf("elevator events %q", k)
	}
}

func TestRideNoElevatorForFloorFails(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 5, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 9})
	rh.tick()
	if got := rh.task(tk.ID); got.State != Failed || !strings.Contains(got.Error, "no elevator serves floors 1 and 9") {
		t.Fatalf("task %+v", got)
	}
	if n := len(rh.sentTo("r1")); n != 0 {
		t.Fatalf("%d commands sent", n)
	}
	// Same floor: a plain goto, no elevator.
	tk2 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 1})
	rh.tick()
	if g := rh.last("r1"); g.Type != "goto" || g.X != 3 || strings.Contains(g.CmdID, ".L") {
		t.Fatalf("same-floor leg: %+v (task %s)", g, tk2.ID)
	}
}

// Legs chain across floors: leg 2 on another floor goes through the car
// from the floor of leg 1.
func TestRideSecondLegOnOtherFloor(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 5, 0))
	tk := rh.submit(Waypoint{X: 1, Y: 1, Floor: 1}, Waypoint{X: 2, Y: 2, Floor: 3})
	rh.tick()
	g := rh.last("r1")
	rh.ack("r1", g.CmdID, "done")
	g2 := rh.last("r1")
	if g2.X != 10 || !strings.HasSuffix(g2.CmdID, ".L1") || rh.task(tk.ID).Phase != PhaseLobby {
		t.Fatalf("leg 2 starts %+v", g2)
	}
}

func TestRideAbortReturnsRobotsToTheQueue(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	ride := rh.toRide(tk.ID, "r1")
	rh.ack("r1", ride.CmdID, "accepted")
	// The car never answers again.
	rh.advance(elevator.DefaultTiming().Silence)
	cancel := rh.last("r1")
	if cancel.Type != "cancel" || !strings.HasPrefix(cancel.CmdID, ride.CmdID+".x") {
		t.Fatalf("after the abort: %+v, want a cancel aimed at %s", cancel, ride.CmdID)
	}
	got := rh.task(tk.ID)
	if got.Phase != PhaseWait || got.Attempts != 0 || got.Robot != "r1" {
		t.Fatalf("task %+v, want back in the queue without an attempt", got)
	}
	if len(rh.link.turns) != 0 {
		t.Fatal("a turn was given")
	}
	// Back-off, then a new ride with a new cmd_id.
	rh.carState(1, elevator.DoorClosed)
	rh.advance(elevator.DefaultTiming().Backoff)
	again := rh.last("r1")
	if again.Type != "ride" || again.CmdID == ride.CmdID {
		t.Fatalf("second ride %+v (first %s)", again, ride.CmdID)
	}
}

func TestRideMissingAcceptBlamesThatRobotOnly(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 9, 1), idle("r2", 9, 0))
	t1 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	t2 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 5})
	rh.tick()
	for _, r := range []string{"r1", "r2"} {
		g := rh.last(r)
		rh.ack(r, g.CmdID, "done")
	}
	rh.carState(1, elevator.DoorClosed)
	rh.advance(elevator.DefaultTiming().BatchWindow)
	ride1 := rh.last("r1")
	rh.ack("r1", ride1.CmdID, "accepted")
	for range 9 { // r2 says nothing; the car is fine
		rh.advance(time.Second)
		rh.carState(1, elevator.DoorOpen)
	}
	if n := len(rh.sentTo("r2")); n < 3 {
		t.Fatalf("ride not re-sent to r2: %d commands", n)
	}
	rh.advance(time.Second)
	if got := rh.task(t2.ID); got.Attempts != 1 || !strings.Contains(got.Error, "no accept from r2") {
		t.Fatalf("r2's task %+v, want requeued with an attempt", got)
	}
	if got := rh.task(t1.ID); got.Attempts != 0 || got.Phase != PhaseWait {
		t.Fatalf("r1's task %+v, want queued again without an attempt", got)
	}
	if len(rh.link.turns) != 0 {
		t.Fatal("turn given without every accept")
	}
}

// Critical battery: taken back at the lobby or before boarding starts;
// never once the robot has its turn or is inside; after the ride its next
// goto is handed back as usual.
func TestRideCriticalBattery(t *testing.T) {
	cfg := DefaultConfig()
	t.Run("waiting at the lobby", func(t *testing.T) {
		rh := newRideH(t, cfg, idle("r1", 9, 0))
		tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
		rh.tick()
		rh.ack("r1", rh.last("r1").CmdID, "done")
		rh.robot("r1").Battery = 5
		rh.tick()
		if got := rh.task(tk.ID); got.State != Queued || got.Attempts != 0 || got.Phase != "" {
			t.Fatalf("task %+v", got)
		}
		if v := rh.m.Elevators()[0]; len(v.Waiting) != 0 {
			t.Fatalf("still queued at the car: %+v", v.Waiting)
		}
		rh.tick()
		if c := rh.last("r1"); c.Type != "dock" {
			t.Fatalf("last %+v, want a dock", c)
		}
	})
	t.Run("accepted, before boarding", func(t *testing.T) {
		rh := newRideH(t, cfg, idle("r1", 9, 0))
		tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
		ride := rh.toRide(tk.ID, "r1")
		rh.ack("r1", ride.CmdID, "accepted")
		rh.robot("r1").TaskID = ride.CmdID
		rh.robot("r1").Battery = 5
		rh.tick()
		if c := rh.last("r1"); c.Type != "cancel" || !strings.HasPrefix(c.CmdID, ride.CmdID) {
			t.Fatalf("last %+v", c)
		}
		if got := rh.task(tk.ID); got.State != Queued || got.Attempts != 0 {
			t.Fatalf("task %+v", got)
		}
	})
	t.Run("riding", func(t *testing.T) {
		rh := newRideH(t, cfg, idle("r1", 9, 0))
		tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
		ride := rh.toRide(tk.ID, "r1")
		rh.carState(1, elevator.DoorOpen)
		rh.ack("r1", ride.CmdID, "accepted")
		r := rh.robot("r1")
		r.TaskID, r.Battery, r.Mode = ride.CmdID, 5, "BOARDING" // its turn, still online for a moment
		before := len(rh.sentTo("r1"))
		rh.advance(time.Second)
		rh.answer(elevator.ZoneClear)
		r.Conn = fleet.InElevator
		rh.advance(time.Minute)
		if n := len(rh.sentTo("r1")); n != before {
			t.Fatalf("%v sent to a riding robot", rh.sentTo("r1")[before:])
		}
		rh.carState(4, elevator.DoorOpen)
		rh.answer(elevator.ZoneClear)
		r.Conn, r.Floor, r.Mode, r.TaskID = fleet.Online, 4, "IDLE", ""
		rh.ackFloor("r1", ride.CmdID, "done", 4)
		g := rh.last("r1")
		r.TaskID, r.Mode = g.CmdID, "MOVING"
		rh.tick()
		if c := rh.last("r1"); c.Type != "cancel" || rh.task(tk.ID).Robot != "r1" {
			t.Fatalf("after the ride: %+v; want the goto handed back", c)
		}
	})
}

func TestRideWrongFloorReplans(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 9, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	ride := rh.toRide(tk.ID, "r1")
	rh.carState(1, elevator.DoorOpen)
	rh.ack("r1", ride.CmdID, "accepted")
	rh.answer(elevator.ZoneClear)
	rh.carState(4, elevator.DoorOpen)
	rh.answer(elevator.ZoneClear)
	r := rh.robot("r1")
	r.Floor = 3
	rh.ackFloor("r1", ride.CmdID, "done", 3)
	g := rh.last("r1")
	got := rh.task(tk.ID)
	if g.Type != "goto" || g.X != 11 || g.Y != 1 || got.Phase != PhaseLobby || got.From != 3 || got.Attempts != 0 {
		t.Fatalf("after a wrong-floor exit: %+v task %+v; want a goto to floor 3's lobby (11, 1)", g, got)
	}
	found := false
	for _, e := range rh.events {
		found = found || e.Type == "elevator" && e.Kind == "replanned" && strings.Contains(e.Detail, "came out at floor 3, not 4")
	}
	if !found {
		t.Fatalf("no replanned event: %s", rh.elevKinds())
	}
}

func TestRideUserCancel(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 9, 1), idle("r2", 9, 0))
	t1 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	t2 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 5})
	rh.tick()
	for _, r := range []string{"r1", "r2"} {
		rh.ack(r, rh.last(r).CmdID, "done")
	}
	if _, err := rh.m.Cancel(t2.ID); err != nil { // waiting: just leaves the queue
		t.Fatal(err)
	}
	if v := rh.m.Elevators()[0]; len(v.Waiting) != 1 || v.Waiting[0].Robot != "r1" {
		t.Fatalf("waiting %+v", v.Waiting)
	}
	rh.carState(1, elevator.DoorClosed)
	rh.advance(elevator.DefaultTiming().BatchWindow)
	ride := rh.last("r1")
	rh.carState(1, elevator.DoorOpen)
	rh.ack("r1", ride.CmdID, "accepted")
	rh.answer(elevator.ZoneClear) // r1 is in the car
	if _, err := rh.m.Cancel(t1.ID); err != nil {
		t.Fatal(err)
	}
	// The car still takes it to its stop.
	if tr := rh.trip(); tr.Stage != elevator.StageTravel || tr.Riders[0].Status != elevator.RiderInside {
		t.Fatalf("trip %+v", tr)
	}
	rh.carState(4, elevator.DoorOpen)
	rh.answer(elevator.ZoneClear)
	if v := rh.m.Elevators()[0]; v.Trip != nil {
		t.Fatal("car not released")
	}
}

// Restart while a robot is in the car: the retained trip brings the trip
// back; the task is not requeued after the restart grace, and finishes.
func TestRideRestartMidTrip(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	ride := rh.toRide(tk.ID, "r1")
	rh.carState(1, elevator.DoorOpen)
	rh.ack("r1", ride.CmdID, "accepted")
	rh.answer(elevator.ZoneClear)
	if err := rh.m.Flush(); err != nil {
		t.Fatal(err)
	}
	saved := rh.link.trips[len(rh.link.trips)-1]
	if saved.Stage != elevator.StageTravel {
		t.Fatalf("saved stage %s", saved.Stage)
	}

	// New server run; the robot is in the car (never heard by the new registry).
	delete(rh.robots.robots, "r1")
	rh.robots.names = nil
	rh.m = rh.manager("b1")
	if err := rh.m.Restore(); err != nil {
		t.Fatal(err)
	}
	rh.m.LinkUp()
	rh.m.RestoreTrip("E1", saved)
	rh.m.RestoreTrip("E1", rh.link.trips[len(rh.link.trips)-1]) // its own echo: ignored
	rh.advance(cfg.RestoreGrace + 5*time.Second)
	if got := rh.task(tk.ID); got.State != Running || got.Robot != "r1" || got.Phase != PhaseRide {
		t.Fatalf("task after the grace: %+v", got)
	}
	rh.carState(4, elevator.DoorOpen)
	if tr := rh.trip(); tr.Turn == nil || tr.Turn.Robot != "r1" || tr.Turn.Action != elevator.ActExit {
		t.Fatalf("trip after restart %+v", tr)
	}
	rh.answer(elevator.ZoneClear)
	rh.addRobot(fleet.Robot{Name: "r1", Conn: fleet.Online, Mode: "IDLE", HasPose: true, CanRide: true, Floor: 4, Battery: 80, BatteryKnown: true})
	rh.ackFloor("r1", ride.CmdID, "done", 4)
	g := rh.last("r1")
	rh.ack("r1", g.CmdID, "done")
	rh.want(tk.ID, Done, "r1")
}

// Restart with no trip to restore (retained message lost): the ride is
// cancelled and the task requeued after the grace, like any restored task.
func TestRideRestartWithoutTrip(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	ride := rh.toRide(tk.ID, "r1")
	rh.ack("r1", ride.CmdID, "accepted")
	rh.m.Flush()
	rh.m = rh.manager("b1")
	rh.m.Restore()
	rh.m.LinkUp()
	r := rh.robot("r1")
	r.TaskID, r.Mode = ride.CmdID, "WAITING_ELEVATOR"
	rh.tick()
	if c := rh.last("r1"); c.CmdID != ride.CmdID {
		t.Fatalf("restored ride not probed: %+v", c)
	}
	rh.ack("r1", ride.CmdID, "accepted") // it still holds the plan: drop it, queue again
	if c := rh.last("r1"); c.Type != "cancel" || !strings.HasPrefix(c.CmdID, ride.CmdID) {
		t.Fatalf("last %+v", c)
	}
	if got := rh.task(tk.ID); got.Phase != PhaseWait || got.Robot != "r1" {
		t.Fatalf("task %+v", got)
	}
}

func TestRobotEventsRecordedInOrder(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0))
	ev := func(boot string, seq uint64) {
		rh.m.HandleRobotEvent("r1", elevator.RobotEvent{Boot: boot, Seq: seq, Event: fmt.Sprintf("e%d", seq), Stamp: 1}) //nolint
	}
	recorded := func() string {
		var b []string
		for _, e := range rh.events {
			if e.Kind == "robot_event" {
				var seq int
				fmt.Sscanf(e.Detail, "#%d", &seq)
				b = append(b, fmt.Sprint(seq))
			}
		}
		return strings.Join(b, ",")
	}
	ev("a", 3)
	ev("a", 1)
	ev("a", 2)
	if got := recorded(); got != "" {
		t.Fatalf("recorded %q before the first event waited", got)
	}
	rh.advance(cfg.EventReorder)
	if got := recorded(); got != "1,2,3" {
		t.Fatalf("recorded %q", got)
	}
	ev("a", 2) // duplicate
	ev("a", 5) // 4 missing: waits
	rh.tick()
	if got := recorded(); got != "1,2,3" {
		t.Fatalf("recorded %q", got)
	}
	ev("a", 4)
	if got := recorded(); got != "1,2,3,4,5" {
		t.Fatalf("recorded %q", got)
	}
	ev("a", 7) // 6 never comes
	rh.advance(cfg.EventReorder)
	if got := recorded(); got != "1,2,3,4,5,7" {
		t.Fatalf("recorded %q", got)
	}
	ev("b", 1) // adapter restarted
	rh.advance(cfg.EventReorder)
	if got := recorded(); got != "1,2,3,4,5,7,1" {
		t.Fatalf("recorded %q", got)
	}
}

func nonRider(r fleet.Robot) fleet.Robot { r.CanRide = false; return r }

// A robot that cannot ride is skipped for a cross-floor task even when it is
// the nearest; the farther robot that can ride gets it.
func TestRideSkipsRobotThatCannotRide(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), nonRider(idle("r1", 9, 0)), idle("r2", 50, 0))
	tk := rh.submit(Waypoint{X: 2, Y: 2, Floor: 3})
	rh.tick()
	rh.want(tk.ID, Assigned, "r2")
	if n := len(rh.sentTo("r1")); n != 0 {
		t.Fatalf("%d commands sent to the robot that cannot ride", n)
	}
}

// With only non-riding robots free, a cross-floor task stays queued and does
// not use up a robot: a same-floor task behind it still goes out.
func TestRideCrossFloorTaskWaitsForRider(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), nonRider(idle("r1", 9, 0)))
	far := rh.submit(Waypoint{X: 2, Y: 2, Floor: 3})
	near := rh.submit(Waypoint{X: 4, Y: 4, Floor: 1})
	rh.tick()
	rh.want(far.ID, Queued, "")
	rh.want(near.ID, Assigned, "r1")
	if got := rh.task(far.ID); got.Attempts != 0 || got.State != Queued {
		t.Fatalf("task %+v", got)
	}

	// A task whose later leg is on another floor counts too.
	rh2 := newRideH(t, DefaultConfig(), nonRider(idle("r1", 9, 0)))
	multi := rh2.submit(Waypoint{X: 1, Y: 1, Floor: 1}, Waypoint{X: 2, Y: 2, Floor: 3})
	rh2.tick()
	rh2.want(multi.ID, Queued, "")

	// A rider arriving takes the waiting task.
	rh.addRobot(idle("r2", 60, 0))
	rh.tick()
	rh.want(far.ID, Assigned, "r2")
}

// Without elevators nothing is held back: the flag only matters when the
// server can ride at all.
func TestCannotRideIgnoredWithoutElevators(t *testing.T) {
	h := newHarness(t, DefaultConfig(), nonRider(idle("r1", 9, 0)))
	tk := h.submit(Waypoint{X: 2, Y: 2, Floor: 3})
	h.tick()
	h.want(tk.ID, Assigned, "r1")
}

// A robot that stops being able to ride after it was assigned fails its own
// attempt at the cross-floor leg (no ride is sent) and another robot takes it.
func TestRideFlagFlipAfterAssignmentRequeues(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 5, 0), idle("r2", 50, 0))
	tk := rh.submit(Waypoint{X: 1, Y: 1, Floor: 1}, Waypoint{X: 2, Y: 2, Floor: 3})
	rh.tick()
	rh.want(tk.ID, Assigned, "r1")
	g := rh.last("r1")
	rh.ack("r1", g.CmdID, "accepted")
	rh.robot("r1").CanRide = false
	rh.ack("r1", g.CmdID, "done")

	got := rh.want(tk.ID, Queued, "")
	if got.Attempts != 1 || len(got.FailedOn) != 1 || got.FailedOn[0] != "r1" || !strings.Contains(got.Error, "cannot ride") {
		t.Fatalf("task %+v", got)
	}
	if l := rh.last("r1"); l.Type != "goto" || l.CmdID != g.CmdID {
		t.Fatalf("something was sent after the flip: %+v", l)
	}
	rh.robot("r1").Mode, rh.robot("r1").TaskID = "IDLE", ""
	rh.tick()
	rh.want(tk.ID, Assigned, "r2")
}
