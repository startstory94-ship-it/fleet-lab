package sim_test

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// 6a review: sim cases the first round could not catch.

// Robots slower through the door (40 s) than a turn and its retry (30 s): the
// next robot still never starts while one is in the door zone, and the
// trip ends with everyone on their floor.
func TestRideRobotsSlowerThanTurnTimeout(t *testing.T) {
	// The server knows its robots are slow (silence estimates), but the turn
	// timeout stays shorter than one move.
	old := liftTiming
	liftTiming.TurnEstimate = 45 * time.Second
	t.Cleanup(func() { liftTiming = old })
	lw := newLiftWorld(t, 11, near3)
	lw.fl.SetDoorMove(40 * time.Second) // longer than a turn and its retry
	for _, fl := range []int{3, 7, 5} {
		lw.submit(fwp(12, 4, fl))
	}
	lw.runUntil("all DONE", 10*time.Minute, lw.allDone)
	car := lw.fl.Car("E1")
	if car.MaxZone != 1 {
		t.Fatalf("max in door zone %d", car.MaxZone)
	}
	if len(lw.elev("overdue")) == 0 {
		t.Fatal("no turn timed out; the test does not exercise slow robots")
	}
	// Each start is followed by that robot's own end before anyone else starts.
	var mover string
	for _, l := range car.Log {
		name, what, _ := strings.Cut(l, " ")
		switch {
		case strings.HasSuffix(what, "start"):
			if mover != "" {
				t.Fatalf("%s started while %s was in the door zone: %v", name, mover, car.Log)
			}
			mover = name
		default:
			mover = ""
		}
	}
}

// A lost turn and a lost zone answer are repaired by the re-sent turn.
func TestRideLostTurnAndZone(t *testing.T) {
	lw := newLiftWorld(t, 12, near3)
	id := lw.submit(fwp(12, 4, 4))
	r := lw.robotOf(id)
	lw.fl.DropTurns(r, 2) // the first and its first re-send
	lw.fl.DropZones(r, 1)
	lw.runUntil("DONE", 3*time.Minute, lw.allDone)
	if n := len(lw.elev("trip_planned")); n != 1 {
		t.Fatalf("%d trips\n%s", n, lw.elevLog())
	}
	if tk := lw.task(id); tk.Attempts != 0 {
		t.Fatalf("attempt used: %+v", tk)
	}
}

// Carried past a blocked door, and then out at a floor it did not expect:
// the slot layout keeps it in front at the next stop, and its route is
// planned again from where it really is.
func TestRideCarriedThenWrongFloor(t *testing.T) {
	lw := newLiftWorld(t, 13, near3)
	id3 := lw.submit(fwp(12, 4, 3))
	r3 := lw.robotOf(id3)
	lw.fl.FailExits(r3, 2)
	lw.fl.WrongFloor(r3, 6) // comes out at 5 but reports 6
	id5 := lw.submit(fwp(12, 4, 5))
	lw.submit(fwp(12, 4, 7))
	lw.runUntil("all DONE", 5*time.Minute, lw.allDone)
	car := lw.fl.Car("E1")
	r5 := lw.task(id5).Robot
	i3 := slices.Index(car.Log, r3+" exit clear")
	i5 := slices.Index(car.Log, r5+" exit clear")
	if i3 < 0 || i5 < 0 || i3 > i5 {
		t.Fatalf("door log %v: carried %s (in front) must exit before %s", car.Log, r3, r5)
	}
	routed := false
	for _, e := range lw.elev("replanned") {
		routed = routed || e.Robot == r3 && strings.Contains(e.Detail, "came out at floor 6, not 3")
	}
	if !routed {
		t.Fatalf("no replan from floor 6:\n%s", lw.elevLog())
	}
	if f := lw.fl.Robot(r3).Floor; f != 3 {
		t.Fatalf("%s ended on floor %d", r3, f)
	}
	if lw.fl.Cleared("E1") < 2 {
		t.Fatalf("retained trip cleared %d times for 2+ trips", lw.fl.Cleared("E1"))
	}
}
