package task

import (
	"strings"
	"testing"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/fleet"
)

// 6a review: R5, R6 (adapted from the reviewer's proofs) and task-side tests
// for mutants that survived the first round.

// R5: a trip adopted after a restart becomes this run's (Boot), so its own
// retained echoes, and echoes of what was published when it ended, are
// never adopted again.
func TestReviewRestoredTripEchoIgnored(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	ride := rh.toRide(tk.ID, "r1")
	rh.carState(1, elevator.DoorOpen)
	rh.ack("r1", ride.CmdID, "accepted")
	rh.answer(elevator.ZoneClear)
	rh.m.Flush()
	saved := rh.link.trips[len(rh.link.trips)-1]

	delete(rh.robots.robots, "r1")
	rh.robots.names = nil
	rh.m = rh.manager("b1")
	rh.m.Restore()
	rh.m.LinkUp()
	rh.m.RestoreTrip("E1", saved)
	if tr := rh.trip(); tr.Boot != "b1" {
		t.Fatalf("adopted trip keeps boot %q", tr.Boot)
	}
	rh.carState(4, elevator.DoorOpen)
	idx, cmds := len(rh.link.trips), len(rh.link.cmds)
	rh.answer(elevator.ZoneClear) // last robot out: the trip ends in this call
	if v := rh.m.Elevators()[0]; v.Trip != nil {
		t.Fatal("trip not finished")
	}
	if rh.link.clears != 1 {
		t.Fatalf("retained trip cleared %d times, want 1", rh.link.clears)
	}
	// Every message published for this trip comes back (even with the old
	// boot, as a broker might still hold): none is adopted.
	for _, e := range append(rh.link.trips[idx:], saved) {
		rh.m.RestoreTrip("E1", e)
		if v := rh.m.Elevators()[0]; v.Trip != nil {
			t.Fatalf("ended trip re-adopted from a %s echo (boot %s)", e.Stage, e.Boot)
		}
	}
	if extra := rh.link.cmds[cmds:]; len(extra) > 1 {
		t.Fatalf("car commands after the trip ended: %+v", extra)
	}
}

// R6: an exit turn that is never answered (robot inside, its silence long
// over) does not leave the task RUNNING forever: retried, then stuck, the
// task goes to another robot; the car keeps trying the exit.
func TestReviewTaskSettledAfterUnansweredExit(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0), idle("r2", 2, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	rh.tick()
	r := rh.task(tk.ID).Robot
	ride := rh.toRide(tk.ID, r)
	rh.carState(1, elevator.DoorOpen)
	rh.ack(r, ride.CmdID, "accepted")
	rh.answer(elevator.ZoneClear)
	rh.robot(r).Conn = fleet.Offline // in the car, its silence long over
	rh.carState(4, elevator.DoorOpen)
	for range 12 {
		rh.advance(5 * time.Second)
		rh.carState(4, elevator.DoorOpen)
	}
	got := rh.task(tk.ID)
	if got.Robot == r || len(got.FailedOn) == 0 || got.FailedOn[0] != r || !strings.Contains(strings.Join(got.FailedOn, ","), "r") {
		t.Fatalf("task %+v, want taken from %s and handed on", got, r)
	}
	if tr := rh.trip(); tr.Riders[0].Status != elevator.RiderStuck && tr.Stage != elevator.StageExiting {
		t.Fatalf("trip %+v", tr)
	}
}

// A trip restored at boarding: a robot still waiting for its turn is the
// task manager's again at once (restoring cleared), so losing it before
// boarding fails its task and takes it out of the trip.
func TestReviewRestoredRideConfirmed(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 1), idle("r2", 9, 0))
	t1 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	t2 := rh.submit(Waypoint{X: 3, Y: 3, Floor: 5})
	rh.tick()
	for _, id := range []string{t1.ID, t2.ID} {
		r := rh.task(id).Robot
		rh.ack(r, rh.last(r).CmdID, "done")
	}
	rh.carState(1, elevator.DoorClosed)
	rh.advance(elevator.DefaultTiming().BatchWindow)
	for _, id := range []string{t1.ID, t2.ID} {
		r := rh.task(id).Robot
		rh.ack(r, rh.last(r).CmdID, "accepted")
	}
	rh.carState(1, elevator.DoorOpen) // boarding: first turn out, the other waits
	rh.m.Flush()
	saved := rh.link.trips[len(rh.link.trips)-1]
	if saved.Stage != elevator.StageBoarding {
		t.Fatalf("saved %s", saved.Stage)
	}
	waiting := ""
	for _, rd := range saved.Riders {
		if rd.Status == elevator.RiderWaiting {
			waiting = rd.Robot
		}
	}
	rh.m = rh.manager("b1")
	rh.m.Restore()
	rh.m.LinkUp()
	rh.m.RestoreTrip("E1", saved)
	rh.tick()
	rh.robot(waiting).Conn = fleet.Offline
	rh.tick()
	var task Task
	for _, id := range []string{t1.ID, t2.ID} {
		if tk := rh.task(id); tk.FailedOn != nil && tk.FailedOn[0] == waiting {
			task = tk
		}
	}
	if task.ID == "" || task.Attempts != 1 {
		t.Fatalf("task of %s not failed after it went OFFLINE before boarding: %+v", waiting, rh.m.List(""))
	}
	if rd, _, _ := rh.m.cars["E1"].Rider(saved.ID, waiting); rd.Status != elevator.RiderLeft {
		t.Fatalf("%s still in the trip: %+v", waiting, rd)
	}
}

func TestReviewReleaseElevator(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 9, 0))
	if _, err := rh.m.ReleaseElevator("E9"); err != ErrNoElevator {
		t.Fatalf("unknown car: %v", err)
	}
	if _, err := rh.m.ReleaseElevator("E1"); err != ErrNoTrip {
		t.Fatalf("no trip: %v", err)
	}
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	ride := rh.toRide(tk.ID, "r1")
	rh.carState(1, elevator.DoorOpen)
	rh.ack("r1", ride.CmdID, "accepted")
	rh.answer(elevator.ZoneClear) // inside
	v, err := rh.m.ReleaseElevator("E1")
	if err != nil || v.Trip != nil || rh.link.clears != 1 {
		t.Fatalf("release: %v %+v clears %d", err, v.Trip, rh.link.clears)
	}
	got := rh.task(tk.ID)
	if got.State != Queued && got.Robot == "r1" || got.Attempts != 0 || got.Phase != "" {
		t.Fatalf("task %+v, want requeued without an attempt", got)
	}
	if c := rh.last("r1"); c.Type != "cancel" || c.Target != ride.CmdID {
		t.Fatalf("last %+v, want a cancel targeting the ride", c)
	}
	released := false
	for _, e := range rh.events {
		released = released || e.Type == "elevator" && e.Kind == "released" && strings.Contains(e.Detail, "operator")
	}
	if !released {
		t.Fatalf("no released event: %s", rh.elevKinds())
	}
}

// A robot queued for a ride to its own floor finishes the leg with a goto.
func TestReviewNoRideNeeded(t *testing.T) {
	rh := newRideH(t, DefaultConfig(), idle("r1", 9, 0))
	tk := rh.submit(Waypoint{X: 3, Y: 3, Floor: 4})
	rh.tick()
	g := rh.last("r1")
	rh.ack("r1", g.CmdID, "done")
	tt := rh.m.tasks[tk.ID]
	rh.m.cars["E1"].Unwait("r1")
	rh.m.cars["E1"].Wait("r1", 4, 4, rh.m.rideID(tt), rh.now) // as if queued on floor 4
	rh.tick()
	c := rh.last("r1")
	if c.Type != "goto" || c.X != 3 || rh.task(tk.ID).Phase != "" {
		t.Fatalf("last %+v task %+v", c, rh.task(tk.ID))
	}
}

func TestReviewRobotEventDuplicateWhileWaiting(t *testing.T) {
	cfg := DefaultConfig()
	rh := newRideH(t, cfg, idle("r1", 9, 0))
	ev := func(seq uint64) {
		rh.m.HandleRobotEvent("r1", elevator.RobotEvent{Boot: "a", Seq: seq, Event: "e", Stamp: 1})
	}
	ev(1)
	rh.advance(cfg.EventReorder)
	ev(3)
	ev(3) // duplicate while 2 is missing
	rh.advance(cfg.EventReorder)
	n := 0
	for _, e := range rh.events {
		if e.Kind == "robot_event" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d events recorded, want 2 (#1, #3 once)", n)
	}
}
