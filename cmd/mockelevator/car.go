package main

import (
	"math"
	"slices"
	"sort"
	"time"

	"fleetlab/internal/elevator"
)

// car is one mock elevator, the elevator side of docs/ELEVATOR.md (3.6,
// 3.7, 5). It is stepped with explicit times so it can be tested without
// MQTT or a real clock:
//
//   - call / goto: reserve for the trip, close the door (never on anything in
//     the door zone), travel floor_s per floor, open in door_s and hold the
//     door hold_s. Already there: (re)open and hold.
//   - hold: only an open (or opening) door at that floor for that trip is
//     kept open hold_s more; a closed or closing door is not reopened.
//   - release: end the hold and the reservation; the door closes as usual.
//   - each cmd_id is applied once.
//   - door_zone counts robots between their zone "enter" and their own
//     "clear"/"failed" (a set of names, independent of turns), plus one for
//     an injected door blockage. An answer for an older turn n than the
//     robot's latest "enter" does not take it out (a late or repeated copy).
//     The door never closes while door_zone is not 0; a standing car whose
//     door is closed or closing opens it again, and it never starts to
//     travel with something in the door zone.
//   - state on every change and at least every second.
//
// Failure injection: unresponsive (commands ignored, nothing published) and
// door blocked (something stands in the door zone).
type car struct {
	cfg  elevator.CarConfig
	boot string
	seq  uint64

	floor    int
	pos      float64 // floor position while moving
	target   int
	moving   bool
	door     string
	doorLeft float64 // seconds until the door is open/closed
	hold     bool
	holdTill time.Time
	holdFor  time.Duration
	trip     string
	lastCmd  string

	applied  map[string]bool
	appliedQ []string // oldest first, to bound applied
	zone     map[string]bool
	enterN   map[string]turnRef // robot -> trip and n of its latest enter
	inside   map[string]bool

	unresponsive bool
	doorBlocked  bool

	last    time.Time // last step
	lastPub time.Time
	dirty   bool
}

const maxApplied = 4096

type turnRef struct {
	trip string
	n    int
}

func newCar(cfg elevator.CarConfig, boot string, startFloor int) *car {
	if !cfg.Serves(startFloor) {
		startFloor = cfg.Floors[0]
	}
	return &car{cfg: cfg, boot: boot, floor: startFloor, pos: float64(startFloor), target: startFloor,
		door: elevator.DoorClosed, applied: map[string]bool{}, zone: map[string]bool{}, enterN: map[string]turnRef{},
		inside: map[string]bool{}, dirty: true}
}

// command applies an elevator command. It reports whether it was applied
// (false: unresponsive, a repeated cmd_id or an invalid command).
func (c *car) command(cmd elevator.Cmd, now time.Time) bool {
	if c.unresponsive || cmd.CmdID == "" || c.applied[cmd.CmdID] {
		return false
	}
	switch cmd.Cmd {
	case elevator.CmdCall, elevator.CmdGoto:
		if !c.cfg.Serves(cmd.Floor) || cmd.Trip == "" {
			return false
		}
	case elevator.CmdHold, elevator.CmdRelease:
	default:
		return false
	}
	c.remember(cmd.CmdID)
	c.lastCmd = cmd.CmdID
	c.dirty = true
	switch cmd.Cmd {
	case elevator.CmdCall, elevator.CmdGoto:
		c.trip, c.target = cmd.Trip, cmd.Floor
		c.holdFor = secs(cmd.HoldS)
		if cmd.Floor == c.floor && !c.moving {
			// Already here: (re)open and hold.
			c.hold, c.holdTill = true, now.Add(c.holdFor)
			if c.door != elevator.DoorOpen && c.door != elevator.DoorOpening {
				c.door, c.doorLeft = elevator.DoorOpening, c.cfg.DoorS
			}
			return true
		}
		c.hold = false // close and go
	case elevator.CmdHold:
		if cmd.Trip != c.trip || c.moving || c.floor != cmd.Floor {
			return true // applied, but nothing to do
		}
		if c.door == elevator.DoorOpen || c.door == elevator.DoorOpening {
			c.hold, c.holdTill = true, now.Add(secs(cmd.HoldS))
		}
	case elevator.CmdRelease:
		if cmd.Trip == c.trip {
			c.hold, c.trip = false, ""
		}
	}
	return true
}

func (c *car) remember(id string) {
	c.applied[id] = true
	c.appliedQ = append(c.appliedQ, id)
	if len(c.appliedQ) > maxApplied {
		delete(c.applied, c.appliedQ[0])
		c.appliedQ = c.appliedQ[1:]
	}
}

// zoneMsg counts the door zone from a robot's zone message.
func (c *car) zoneMsg(z elevator.Zone) {
	if z.Robot == "" {
		return
	}
	before := c.doorZone()
	switch z.Event {
	case elevator.ZoneEnter:
		c.zone[z.Robot] = true
		if last, ok := c.enterN[z.Robot]; !ok || last.trip != z.Trip || z.N > last.n {
			c.enterN[z.Robot] = turnRef{z.Trip, z.N} // n counts per trip
		}
	case elevator.ZoneClear, elevator.ZoneFailed:
		if last, ok := c.enterN[z.Robot]; ok && last.trip == z.Trip && z.N < last.n {
			return // answer for an older turn: the robot may be in the zone for a newer one
		}
		delete(c.zone, z.Robot)
		if z.Event == elevator.ZoneClear {
			// Load is informational: in after a boarding clear, out after an exit clear.
			if z.Action == elevator.ActBoard {
				c.inside[z.Robot] = true
			} else if z.Action == elevator.ActExit {
				delete(c.inside, z.Robot)
			}
		}
	}
	if c.doorZone() != before {
		c.dirty = true
	}
	c.dirty = c.dirty || z.Event == elevator.ZoneClear
}

func (c *car) doorZone() int {
	n := len(c.zone)
	if c.doorBlocked {
		n++
	}
	return n
}

func (c *car) setFaults(unresponsive, blocked bool) {
	if c.doorBlocked != blocked {
		c.dirty = true
	}
	if c.unresponsive && !unresponsive {
		c.dirty = true // publish right away when it comes back
	}
	c.unresponsive, c.doorBlocked = unresponsive, blocked
}

// advance moves the car (door, travel, hold) up to now.
func (c *car) advance(now time.Time) {
	dt := 0.0
	if !c.last.IsZero() && now.After(c.last) {
		dt = now.Sub(c.last).Seconds()
	}
	if c.last.IsZero() || now.After(c.last) {
		c.last = now
	}
	c.move(dt, now)
}

// step advances the car to now and returns the state to publish, if any.
func (c *car) step(now time.Time) *elevator.State {
	c.advance(now)
	if c.unresponsive || !c.dirty && now.Sub(c.lastPub) < time.Second {
		return nil
	}
	c.seq++
	c.lastPub, c.dirty = now, false
	s := c.state(now)
	return &s
}

func (c *car) state(now time.Time) elevator.State {
	dir := "idle"
	if c.moving {
		dir = "up"
		if float64(c.target) < c.pos {
			dir = "down"
		}
	}
	return elevator.State{Boot: c.boot, Seq: c.seq, Stamp: float64(now.UnixMilli()) / 1000, Floor: c.floor, Dir: dir,
		Door: c.door, Moving: c.moving, Hold: c.hold, Trip: c.trip, Load: len(c.inside), CmdID: c.lastCmd,
		DoorZone: c.doorZone()}
}

func (c *car) move(dt float64, now time.Time) {
	if c.hold && !now.Before(c.holdTill) {
		c.hold, c.dirty = false, true
	}
	switch c.door {
	case elevator.DoorOpening:
		c.doorLeft -= dt
		if c.doorLeft <= 0 {
			c.door, c.dirty = elevator.DoorOpen, true
		}
		return
	case elevator.DoorOpen:
		if c.hold || c.doorZone() > 0 {
			return
		}
		c.door, c.doorLeft, c.dirty = elevator.DoorClosing, c.cfg.DoorS, true
		return
	case elevator.DoorClosing, elevator.DoorClosed:
		if c.doorZone() > 0 && !c.moving { // never on a robot: open again
			left := c.cfg.DoorS // fully closed: a whole opening
			if c.door == elevator.DoorClosing {
				left -= max(c.doorLeft, 0) // back the way it came
			}
			c.door, c.doorLeft, c.dirty = elevator.DoorOpening, left, true
			return
		}
		if c.door == elevator.DoorClosing {
			c.doorLeft -= dt
			if c.doorLeft <= 0 {
				c.door, c.dirty = elevator.DoorClosed, true
			}
			return
		}
	}
	if c.target == c.floor && !c.moving {
		return
	}
	// Door closed: travel.
	if !c.moving {
		c.moving, c.dirty = true, true
	}
	step := dt / c.cfg.FloorS
	d := float64(c.target) - c.pos
	if math.Abs(d) <= step {
		c.pos, c.floor, c.moving = float64(c.target), c.target, false
		c.door, c.doorLeft, c.dirty = elevator.DoorOpening, c.cfg.DoorS, true
		if c.trip != "" {
			c.hold, c.holdTill = true, now.Add(c.holdFor)
		}
		return
	}
	c.pos += math.Copysign(step, d)
	fl := int(math.Floor(c.pos + 1e-9)) // last floor passed
	if d < 0 {
		fl = int(math.Ceil(c.pos - 1e-9))
	}
	if fl != c.floor {
		c.floor, c.dirty = fl, true
	}
}

// zoneNames lists the robots in the door zone (for logs).
func (c *car) zoneNames() []string {
	out := make([]string, 0, len(c.zone))
	for n := range c.zone {
		out = append(out, n)
	}
	sort.Strings(out)
	return slices.Clip(out)
}

func secs(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
