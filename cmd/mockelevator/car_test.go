package main

import (
	"testing"
	"time"

	"fleetlab/internal/elevator"
)

var t0 = time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)

func testCar() *car {
	return newCar(elevator.CarConfig{ID: "E1", Capacity: 3, Floors: []int{1, 2, 3, 4, 5}, FloorS: 2, DoorS: 1}, "b", 1)
}

// run steps the car every 50 ms from *now for d and returns the last state
// published (nil if none) and how many were published.
func steps(c *car, now *time.Time, d time.Duration) (*elevator.State, int) {
	var last *elevator.State
	n := 0
	for end := now.Add(d); now.Before(end); {
		*now = now.Add(50 * time.Millisecond)
		if s := c.step(*now); s != nil {
			last, n = s, n+1
		}
	}
	return last, n
}

func cmd(id, kind, trip string, floor int, hold float64) elevator.Cmd {
	return elevator.Cmd{CmdID: id, Cmd: kind, Trip: trip, Floor: floor, HoldS: hold}
}

func TestCallOpensAndHolds(t *testing.T) {
	c, now := testCar(), t0
	steps(c, &now, time.Second)
	if !c.command(cmd("c1", elevator.CmdCall, "T1", 1, 5), now) {
		t.Fatal("call not applied")
	}
	s, _ := steps(c, &now, 1500*time.Millisecond)
	if s.Door != elevator.DoorOpen || !s.Hold || s.Trip != "T1" || s.Floor != 1 || s.CmdID != "c1" {
		t.Fatalf("after call: %+v", *s)
	}
	// Held for 5 s from the call, then it closes by itself.
	s, _ = steps(c, &now, 3*time.Second)
	if s.Door != elevator.DoorOpen {
		t.Fatalf("closed during hold: %+v", *s)
	}
	s, _ = steps(c, &now, 3*time.Second)
	if s.Door != elevator.DoorClosed || s.Hold {
		t.Fatalf("not closed after hold_s: %+v", *s)
	}
	if s.Trip != "T1" {
		t.Fatalf("reservation lost without release: %+v", *s)
	}
}

func TestGotoTravelsAndNeverClosesOnARobot(t *testing.T) {
	c, now := testCar(), t0
	c.command(cmd("c1", elevator.CmdCall, "T1", 1, 15), now)
	steps(c, &now, 2*time.Second)
	c.zoneMsg(elevator.Zone{Trip: "T1", N: 1, Robot: "r1", Action: elevator.ActBoard, Event: elevator.ZoneEnter})
	s, _ := steps(c, &now, 100*time.Millisecond)
	if s == nil || s.DoorZone != 1 {
		t.Fatalf("door_zone not published at once: %+v", s)
	}
	c.command(cmd("c2", elevator.CmdGoto, "T1", 4, 15), now)
	s, _ = steps(c, &now, 10*time.Second)
	if s.Door != elevator.DoorOpen || s.Moving || s.Floor != 1 {
		t.Fatalf("left with a robot in the door zone: %+v", *s)
	}
	c.zoneMsg(elevator.Zone{Trip: "T1", N: 1, Robot: "r1", Action: elevator.ActBoard, Event: elevator.ZoneClear})
	sawMoving, sawFloor2 := false, false
	for range 200 {
		now = now.Add(50 * time.Millisecond)
		if s := c.step(now); s != nil {
			sawMoving = sawMoving || s.Moving
			sawFloor2 = sawFloor2 || s.Floor == 2 && s.Moving
		}
	}
	s, _ = steps(c, &now, time.Second)
	if !sawMoving || !sawFloor2 {
		t.Fatalf("no moving state passing floor 2 (moving %v, floor 2 %v)", sawMoving, sawFloor2)
	}
	if s.Floor != 4 || s.Door != elevator.DoorOpen || !s.Hold || s.Load != 1 || s.DoorZone != 0 || s.Dir != "idle" {
		t.Fatalf("not open at 4 with one robot inside: %+v", *s)
	}
}

func TestDoorZoneCountsRobotsOnce(t *testing.T) {
	c := testCar()
	z := func(robot, act, ev string) { c.zoneMsg(elevator.Zone{Robot: robot, Action: act, Event: ev}) }
	z("r1", elevator.ActBoard, elevator.ZoneEnter)
	z("r1", elevator.ActBoard, elevator.ZoneEnter) // repeated: still one
	z("r2", elevator.ActExit, elevator.ZoneEnter)
	if c.doorZone() != 2 {
		t.Fatalf("door_zone %d, want 2", c.doorZone())
	}
	z("r3", elevator.ActExit, elevator.ZoneClear) // never entered: no change
	z("r1", elevator.ActBoard, elevator.ZoneFailed)
	if c.doorZone() != 1 {
		t.Fatalf("door_zone %d, want 1", c.doorZone())
	}
	z("r2", elevator.ActExit, elevator.ZoneClear)
	z("r2", elevator.ActExit, elevator.ZoneClear) // answered again
	if c.doorZone() != 0 {
		t.Fatalf("door_zone %d, want 0", c.doorZone())
	}
	c.setFaults(false, true)
	if c.doorZone() != 1 {
		t.Fatal("blocked door not counted")
	}
}

func TestHoldDoesNotReopenAndCmdIDOnce(t *testing.T) {
	c, now := testCar(), t0
	c.command(cmd("c1", elevator.CmdCall, "T1", 1, 2), now)
	s, _ := steps(c, &now, 5*time.Second)
	if s.Door != elevator.DoorClosed {
		t.Fatalf("door %s, want closed after hold ran out", s.Door)
	}
	c.command(cmd("h1", elevator.CmdHold, "T1", 1, 10), now)
	if s, _ = steps(c, &now, 2*time.Second); s.Door != elevator.DoorClosed || s.Hold {
		t.Fatalf("hold reopened a closed door: %+v", *s)
	}
	if c.command(cmd("c1", elevator.CmdCall, "T1", 1, 2), now) {
		t.Fatal("same cmd_id applied twice")
	}
	if s, _ = steps(c, &now, 2*time.Second); s.Door != elevator.DoorClosed {
		t.Fatalf("repeated cmd_id opened the door: %+v", *s)
	}
	// goto the same floor reopens.
	c.command(cmd("g1", elevator.CmdGoto, "T1", 1, 10), now)
	if s, _ = steps(c, &now, 2*time.Second); s.Door != elevator.DoorOpen || !s.Hold {
		t.Fatalf("goto same floor did not reopen: %+v", *s)
	}
	// hold for another trip does nothing; release of this trip frees it.
	c.command(cmd("h2", elevator.CmdHold, "T2", 1, 60), now)
	c.command(cmd("r1", elevator.CmdRelease, "T1", 0, 0), now)
	s, _ = steps(c, &now, 3*time.Second)
	if s.Door != elevator.DoorClosed || s.Trip != "" || s.Hold || s.CmdID != "r1" {
		t.Fatalf("after release: %+v", *s)
	}
	if c.command(cmd("bad", elevator.CmdGoto, "T3", 9, 10), now) {
		t.Fatal("goto to a floor the car does not serve was applied")
	}
}

func TestUnresponsiveIgnoresAndPublishesNothing(t *testing.T) {
	c, now := testCar(), t0
	steps(c, &now, time.Second)
	c.setFaults(true, false)
	if c.command(cmd("c1", elevator.CmdCall, "T1", 3, 10), now) {
		t.Fatal("unresponsive car took a command")
	}
	if _, n := steps(c, &now, 5*time.Second); n != 0 {
		t.Fatalf("unresponsive car published %d states", n)
	}
	c.setFaults(false, false)
	if s, n := steps(c, &now, 1100*time.Millisecond); n < 1 || s.Trip != "" {
		t.Fatalf("not publishing again: %d %+v", n, s)
	}
	// Published at least every second while nothing changes.
	if _, n := steps(c, &now, 5*time.Second); n < 4 || n > 6 {
		t.Fatalf("%d states in 5 s, want about 5", n)
	}
}

// Review 6b-3: a late copy of an older turn's answer must not take a robot
// out of the door zone while it is in it for a newer turn.
func TestReviewStaleAnswerKeepsRobotInZone(t *testing.T) {
	c, now := testCar(), t0
	c.command(cmd("c1", elevator.CmdCall, "T1", 1, 3), now)
	steps(c, &now, 1500*time.Millisecond)
	z := func(n int, ev string) {
		c.zoneMsg(elevator.Zone{Trip: "T1", N: n, Robot: "r1", Action: elevator.ActExit, Event: ev})
	}
	z(4, elevator.ZoneEnter)
	z(4, elevator.ZoneFailed)
	z(5, elevator.ZoneEnter)
	z(4, elevator.ZoneFailed) // re-sent answer for n=4 while r1 is in the zone for n=5
	s, _ := steps(c, &now, 6*time.Second)
	if s.DoorZone != 1 || s.Door != elevator.DoorOpen {
		t.Fatalf("r1 dropped from the zone by an old answer: %+v", *s)
	}
	z(5, elevator.ZoneClear)
	if s, _ = steps(c, &now, 100*time.Millisecond); s == nil || s.DoorZone != 0 {
		t.Fatalf("the answer for n=5 did not clear the zone: %+v", s)
	}
}

// Review 6b-5: a robot entering at a standing car whose door is closed (or
// closing) opens it again, and the car never travels with door_zone > 0.
func TestReviewEnterOnClosedDoorReopens(t *testing.T) {
	c, now := testCar(), t0
	c.command(cmd("c1", elevator.CmdCall, "T1", 1, 1), now)
	if s, _ := steps(c, &now, 5*time.Second); s.Door != elevator.DoorClosed {
		t.Fatalf("door %s, want closed after hold", s.Door)
	}
	c.zoneMsg(elevator.Zone{Trip: "T1", N: 1, Robot: "r1", Action: elevator.ActBoard, Event: elevator.ZoneEnter})
	s, _ := steps(c, &now, 2*time.Second)
	if s.Door != elevator.DoorOpen || s.DoorZone != 1 {
		t.Fatalf("door not reopened for a robot in the zone: %+v", *s)
	}
	// A goto while it is there: the car waits, door open, and never moves
	// with the robot in the zone.
	c.command(cmd("g1", elevator.CmdGoto, "T1", 4, 10), now)
	for range 200 {
		now = now.Add(50 * time.Millisecond)
		if s := c.step(now); s != nil && (s.Moving || s.Floor != 1) && s.DoorZone > 0 {
			t.Fatalf("travelling with door_zone %d: %+v", s.DoorZone, *s)
		}
	}
	c.zoneMsg(elevator.Zone{Trip: "T1", N: 1, Robot: "r1", Action: elevator.ActBoard, Event: elevator.ZoneClear})
	if s, _ = steps(c, &now, 15*time.Second); s.Floor != 4 || s.Door != elevator.DoorOpen {
		t.Fatalf("did not go once the zone was clear: %+v", *s)
	}
}

// Review 6b-5: closing door with a robot entering reopens; a blocked door
// keeps a standing car open and it never leaves.
func TestReviewBlockedDoorNeverTravels(t *testing.T) {
	c, now := testCar(), t0
	c.command(cmd("c1", elevator.CmdCall, "T1", 1, 1), now)
	steps(c, &now, 2*time.Second)
	c.setFaults(false, true)
	c.command(cmd("g1", elevator.CmdGoto, "T1", 3, 10), now)
	for range 400 {
		now = now.Add(50 * time.Millisecond)
		if s := c.step(now); s != nil && (s.Moving || s.Door == elevator.DoorClosed) {
			t.Fatalf("blocked door: %+v", *s)
		}
	}
}

// n starts again with every trip: a robot's answers in a new trip count even
// if their n is below its last enter of the previous trip (found by e2e).
func TestZoneNumbersRestartPerTrip(t *testing.T) {
	c := testCar()
	z := func(trip string, n int, ev string) {
		c.zoneMsg(elevator.Zone{Trip: trip, N: n, Robot: "r1", Action: elevator.ActExit, Event: ev})
	}
	z("T1", 5, elevator.ZoneEnter)
	z("T1", 5, elevator.ZoneClear)
	z("T2", 1, elevator.ZoneEnter)
	z("T2", 1, elevator.ZoneClear)
	if c.doorZone() != 0 {
		t.Fatalf("door_zone %d after the clear of the new trip", c.doorZone())
	}
	z("T2", 2, elevator.ZoneEnter)
	z("T2", 1, elevator.ZoneClear) // a repeat of the older answer in the same trip
	if c.doorZone() != 1 {
		t.Fatalf("door_zone %d, want 1: the robot is in the zone for n=2", c.doorZone())
	}
}
