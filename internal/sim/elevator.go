package sim

import (
	"math"
	"slices"
	"sort"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

// Elevators.
//
// A Car is a simulated elevator on the elevator channel: it takes call, goto,
// hold and release (each cmd_id once), travels FloorS seconds per floor,
// opens and closes its door in DoorS seconds, keeps the door open while held
// (until hold_s runs out or release) and never closes it on a robot in the
// door zone. It publishes its state on every change and every second.
//
// Riding robots follow docs/ELEVATOR.md: a ride command is accepted at the
// lobby; on its boarding turn the robot announces silence_s and loses its
// fleet link (no state, no acks, no commands) but keeps the elevator
// channel; it moves through the door zone in DoorMove, reports zone clear
// (or failed), and exits on its exit turn. Once out, its link comes back
// after LinkBack: it sends a state without silence_s, its queued robot
// events (newest first, so the server has to order them) and the ride's
// "done" with the floor it is on.
//
// Failure injection: FailExits (a robot's next exits fail), BlockDoor (every
// exit at a floor fails), SetUnresponsive (the car ignores commands and
// stops publishing), WrongFloor (a robot comes out reporting another floor)
// and ExtraSilence (a robot's link comes back later than it announced).

// Car is one simulated elevator. Read it with Fleet.Car.
type Car struct {
	ID     string
	Floor  int
	Door   string
	Moving bool
	Hold   bool
	Trip   string
	Load   int
	// Zone lists robots in the door zone now; MaxZone is the most ever at once.
	Zone    []string
	MaxZone int
	// Log records what robots did at the door, in order.
	Log []string

	cfg          elevator.CarConfig
	pos          float64 // floor position while moving
	target       int
	doorLeft     float64
	holdUntil    time.Time
	holdFor      time.Duration
	seq          uint64
	lastPub      time.Time
	pubDirty     bool
	applied      map[string]bool
	lastCmd      string
	unresponsive bool
	blocked      map[int]bool
}

type elevMsg struct {
	at   time.Time
	seq  int
	car  string
	cmd  *elevator.Cmd
	turn *elevator.Turn
	zone *elevator.Zone
	st   *elevator.State
}

// AddElevator adds a car, idle at its first floor with the door closed.
func (f *Fleet) AddElevator(cfg elevator.CarConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &Car{ID: cfg.ID, Floor: cfg.Floors[0], Door: elevator.DoorClosed, cfg: cfg, pos: float64(cfg.Floors[0]),
		target: cfg.Floors[0], applied: map[string]bool{}, blocked: map[int]bool{}, pubDirty: true}
	f.cars[cfg.ID] = c
	f.carNames = append(f.carNames, cfg.ID)
	sort.Strings(f.carNames)
}

// OnElevator sets where car states and robots' zone messages go
// (task.Manager.HandleElevatorState and HandleZone).
func (f *Fleet) OnElevator(state func(car string, s elevator.State), zone func(car string, z elevator.Zone)) {
	f.onState, f.onZone = state, zone
}

// OnRobotEvent sets where robot events go (task.Manager.HandleRobotEvent).
func (f *Fleet) OnRobotEvent(h func(robot string, e elevator.RobotEvent)) { f.onEvent = h }

// SetDoorMove sets how long a robot takes through the door zone (default 2 s).
func (f *Fleet) SetDoorMove(d time.Duration) {
	f.mu.Lock()
	f.doorMove = d
	f.mu.Unlock()
}

// SetLinkBack sets how long after leaving the car a robot's Wi-Fi returns
// (default 1 s).
func (f *Fleet) SetLinkBack(d time.Duration) {
	f.mu.Lock()
	f.linkBack = d
	f.mu.Unlock()
}

// FailExits makes the robot's next n exits fail (blocked in front of it).
func (f *Fleet) FailExits(robot string, n int) {
	f.mu.Lock()
	f.robots[robot].failExits = n
	f.mu.Unlock()
}

// BlockDoor makes every exit at floor fail (something stands at the door).
func (f *Fleet) BlockDoor(car string, floor int, blocked bool) {
	f.mu.Lock()
	f.cars[car].blocked[floor] = blocked
	f.mu.Unlock()
}

// SetUnresponsive makes the car ignore commands and stop publishing.
func (f *Fleet) SetUnresponsive(car string, v bool) {
	f.mu.Lock()
	f.cars[car].unresponsive = v
	f.mu.Unlock()
}

// WrongFloor makes the robot's next exit report floor instead of the car's.
func (f *Fleet) WrongFloor(robot string, floor int) {
	f.mu.Lock()
	f.robots[robot].wrongFloor = &floor
	f.mu.Unlock()
}

// ExtraSilence delays the robot's link after its next exit by d beyond
// LinkBack, past what it announced.
func (f *Fleet) ExtraSilence(robot string, d time.Duration) {
	f.mu.Lock()
	f.robots[robot].extraSilence = d
	f.mu.Unlock()
}

// Rescue: a human takes a robot out of the car onto the car's floor. It
// sends no zone message; its link comes back after LinkBack.
func (f *Fleet) Rescue(robot string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.robots[robot]
	if r.ride == nil || r.rideState != "inside" {
		return
	}
	c := f.cars[r.ride.Elevator]
	c.Load--
	p := c.cfg.LobbyAt(c.Floor)
	r.Floor, r.X, r.Y = c.Floor, p.X, p.Y
	r.rideState, r.Mode = "out", "IDLE"
	r.linkAt = f.now().Add(f.linkBack)
	c.Log = append(c.Log, r.Name+" rescued")
	f.robotEvent(r, "rescued", c.Floor, "taken out by a human")
}

// Car returns a copy of one car.
func (f *Fleet) Car(id string) Car {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *f.cars[id]
	c.Zone = slices.Clone(c.Zone)
	c.Log = slices.Clone(c.Log)
	return c
}

// Riding maps robots that hold a ride to their ride state (waiting,
// boarding, inside, exiting, out).
func (f *Fleet) Riding() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for n, r := range f.robots {
		if r.ride != nil {
			out[n] = r.rideState
		}
	}
	return out
}

// Command implements elevator.Link.
func (f *Fleet) Command(car string, c elevator.Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	f.elevIn = append(f.elevIn, elevMsg{at: f.now().Add(f.elevDelay), seq: f.sent, car: car, cmd: &c})
	return nil
}

// PublishTrip implements elevator.Link. Robots act on turns and on fleet
// cancels; the trip message is kept for tests.
func (f *Fleet) PublishTrip(car string, t elevator.Trip) error {
	f.mu.Lock()
	f.trips[car] = t
	f.mu.Unlock()
	return nil
}

// PublishTurn implements elevator.Link.
func (f *Fleet) PublishTurn(car string, t elevator.Turn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	f.elevIn = append(f.elevIn, elevMsg{at: f.now().Add(f.elevDelay), seq: f.sent, car: car, turn: &t})
	return nil
}

// ClearTrip implements elevator.Link: the retained trip is emptied.
func (f *Fleet) ClearTrip(car string) error {
	f.mu.Lock()
	f.cleared[car]++
	f.mu.Unlock()
	return nil
}

// Cleared counts retained-trip clears for car.
func (f *Fleet) Cleared(car string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleared[car]
}

// DropTurns loses the next n turns sent to robot.
func (f *Fleet) DropTurns(robot string, n int) {
	f.mu.Lock()
	f.robots[robot].dropTurns = n
	f.mu.Unlock()
}

// DropZones loses robot's next n zone answers (clear or failed).
func (f *Fleet) DropZones(robot string, n int) {
	f.mu.Lock()
	f.robots[robot].dropZones = n
	f.mu.Unlock()
}

// LastTrip is the last trip published for car.
func (f *Fleet) LastTrip(car string) elevator.Trip {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trips[car].Clone()
}

// stepElevators delivers elevator-channel messages that arrived, moves the
// cars and the robots at their doors. Called with f.mu held; returns what
// goes to the server.
func (f *Fleet) stepElevators(dt time.Duration, now time.Time) []elevMsg {
	sort.SliceStable(f.elevIn, func(i, j int) bool {
		a, b := f.elevIn[i], f.elevIn[j]
		return a.at.Before(b.at) || a.at.Equal(b.at) && a.seq < b.seq
	})
	n := 0
	for n < len(f.elevIn) && !f.elevIn[n].at.After(now) {
		m := f.elevIn[n]
		switch {
		case m.cmd != nil:
			f.carCommand(f.cars[m.car], *m.cmd, now)
		case m.turn != nil:
			for _, name := range f.names {
				r := f.robots[name]
				if r.ride != nil && r.ride.Trip == m.turn.Trip && r.Name == m.turn.Robot {
					f.turn(r, *m.turn, now)
				}
			}
		}
		n++
	}
	f.elevIn = append(f.elevIn[:0], f.elevIn[n:]...)
	for _, id := range f.carNames {
		f.moveCar(f.cars[id], dt.Seconds(), now)
	}
	for _, name := range f.names {
		f.doorStep(f.robots[name], dt.Seconds(), now)
	}
	for _, id := range f.carNames {
		c := f.cars[id]
		if c.unresponsive || !c.pubDirty && now.Sub(c.lastPub) < time.Second {
			continue
		}
		c.seq++
		c.lastPub, c.pubDirty = now, false
		dir := "idle"
		if c.Moving {
			dir = "up"
			if c.target < c.Floor {
				dir = "down"
			}
		}
		s := elevator.State{Boot: "simcar", Seq: c.seq, Stamp: float64(now.UnixMilli()) / 1000, Floor: c.Floor, Dir: dir,
			Door: c.Door, Moving: c.Moving, Hold: c.Hold, Trip: c.Trip, Load: c.Load, CmdID: c.lastCmd, DoorZone: len(c.Zone)}
		f.elevOut = append(f.elevOut, elevMsg{at: now.Add(f.elevDelay), car: id, st: &s})
	}
	keep := f.elevOut[:0]
	var out []elevMsg
	for _, m := range f.elevOut {
		if m.at.After(now) {
			keep = append(keep, m)
		} else {
			out = append(out, m)
		}
	}
	f.elevOut = keep
	return out
}

func (f *Fleet) carCommand(c *Car, cmd elevator.Cmd, now time.Time) {
	if c.unresponsive || c.applied[cmd.CmdID] {
		return
	}
	c.applied[cmd.CmdID] = true
	c.lastCmd = cmd.CmdID
	c.pubDirty = true
	switch cmd.Cmd {
	case elevator.CmdCall, elevator.CmdGoto:
		c.Trip, c.target = cmd.Trip, cmd.Floor
		c.holdFor = time.Duration(cmd.HoldS * float64(time.Second))
		if cmd.Floor == c.Floor && !c.Moving {
			// Already here: (re)open and hold.
			c.Hold, c.holdUntil = true, now.Add(c.holdFor)
			if c.Door != elevator.DoorOpen {
				c.Door, c.doorLeft = elevator.DoorOpening, c.cfg.DoorS
			}
			return
		}
		c.Hold = false // close and go
	case elevator.CmdHold:
		if cmd.Trip != c.Trip || c.Moving || c.Floor != cmd.Floor {
			return
		}
		// hold keeps an open (or opening) door open; it does not reopen a
		// closed one (goto the same floor does).
		if c.Door == elevator.DoorOpen || c.Door == elevator.DoorOpening {
			c.Hold, c.holdUntil = true, now.Add(time.Duration(cmd.HoldS*float64(time.Second)))
		}
	case elevator.CmdRelease:
		if cmd.Trip == c.Trip {
			c.Hold, c.Trip = false, ""
		}
	}
}

func (f *Fleet) moveCar(c *Car, dt float64, now time.Time) {
	if c.Hold && !now.Before(c.holdUntil) {
		c.Hold, c.pubDirty = false, true
	}
	switch c.Door {
	case elevator.DoorOpening:
		c.doorLeft -= dt
		if c.doorLeft <= 0 {
			c.Door, c.pubDirty = elevator.DoorOpen, true
		}
		return
	case elevator.DoorOpen:
		if c.Hold || len(c.Zone) > 0 {
			return
		}
		c.Door, c.doorLeft, c.pubDirty = elevator.DoorClosing, c.cfg.DoorS, true
		return
	case elevator.DoorClosing:
		if len(c.Zone) > 0 { // never on a robot
			c.Door, c.doorLeft, c.pubDirty = elevator.DoorOpening, c.cfg.DoorS, true
			return
		}
		c.doorLeft -= dt
		if c.doorLeft <= 0 {
			c.Door, c.pubDirty = elevator.DoorClosed, true
		}
		return
	}
	if c.target == c.Floor && !c.Moving {
		return
	}
	// Door closed: travel.
	c.Moving = true
	step := dt / c.cfg.FloorS
	d := float64(c.target) - c.pos
	if math.Abs(d) <= step {
		c.pos, c.Floor, c.Moving = float64(c.target), c.target, false
		c.Door, c.doorLeft, c.pubDirty = elevator.DoorOpening, c.cfg.DoorS, true
		if c.Trip != "" {
			c.Hold, c.holdUntil = true, now.Add(c.holdFor)
		}
		return
	}
	c.pos += math.Copysign(step, d)
	fl := int(math.Floor(c.pos + 1e-9)) // last floor passed
	if d < 0 {
		fl = int(math.Ceil(c.pos - 1e-9))
	}
	if fl != c.Floor {
		c.Floor, c.pubDirty = fl, true
	}
}

// ride handles a ride command at the lobby (mirrors what the adapter must do).
func (f *Fleet) rideCmd(r *Robot, c task.Command, now time.Time) {
	if r.Mode == "ERROR" {
		f.ack(r, c.CmdID, "failed", "in ERROR ("+r.Error+")")
		return
	}
	if c.Ride == nil {
		f.ack(r, c.CmdID, "failed", "ride without a plan")
		return
	}
	if r.ride != nil && r.rideState != "waiting" {
		f.ack(r, c.CmdID, "failed", "already riding "+r.ride.Trip)
		return
	}
	if r.Goal != "" {
		f.finish(r, "cancelled", "replaced by "+c.CmdID) // a goto, or a ride still waiting at the lobby
	}
	p := *c.Ride
	r.ride, r.rideCmd, r.rideState = &p, c.CmdID, "waiting"
	r.Goal, r.Mode = c.CmdID, "WAITING_ELEVATOR"
	r.results = map[int]elevator.Zone{}
	r.Started = append(r.Started, c.CmdID)
	f.ack(r, c.CmdID, "accepted", "")
	f.robotEvent(r, "ride_accepted", r.Floor, "")
}

// cancelRide handles a cancel aimed at the robot's ride: only a ride not
// yet started through the door can be dropped, and only by a cancel whose
// target is that ride; one for another command does nothing.
func (f *Fleet) cancelRide(r *Robot, c task.Command) {
	if c.Target != r.rideCmd || r.rideState != "waiting" {
		f.ack(r, c.CmdID, "failed", "not the current ride, or riding")
		return
	}
	f.ack(r, r.rideCmd, "cancelled", "ride cancelled at the lobby")
	f.robotEvent(r, "ride_cancelled", r.Floor, "")
	r.ride, r.rideCmd, r.rideState, r.Goal, r.Mode = nil, "", "", "", "IDLE"
	f.ack(r, c.CmdID, "accepted", "")
}

func (f *Fleet) turn(r *Robot, t elevator.Turn, now time.Time) {
	if r.dropTurns > 0 {
		r.dropTurns-- // lost on the way
		return
	}
	if z, ok := r.results[t.N]; ok {
		f.zone(r, z) // already answered: answer again
		return
	}
	if r.moveN == t.N && r.moveLeft > 0 {
		return // moving on it
	}
	c := f.cars[r.ride.Elevator]
	if t.Action == elevator.ActExit && r.rideState == "out" || t.Action == elevator.ActBoard && r.rideState == "inside" {
		// Already done (its clear was lost or came late): say so again for this turn.
		f.zone(r, elevator.Zone{Trip: t.Trip, N: t.N, Robot: r.Name, Action: t.Action, Event: elevator.ZoneClear, Floor: r.Floor})
		return
	}
	switch {
	case t.Action == elevator.ActBoard && r.rideState == "waiting":
		// Announce the blackout, then lose the fleet link.
		r.stateSeq++
		f.reg.ApplyState(r.Name, fleet.StateMsg{Boot: "sim", Seq: r.stateSeq, Host: "sim", Mode: "BOARDING",
			Battery: &r.Battery, TaskID: r.Goal, SilenceS: r.ride.SilenceS})
		r.silent, r.rideState, r.Mode = true, "boarding", "BOARDING"
	case t.Action == elevator.ActExit && r.rideState == "inside":
		r.rideState, r.Mode = "exiting", "EXITING"
	default:
		return
	}
	r.moveN, r.moveAct, r.moveLeft, r.moveFloor, r.exitLobby = t.N, t.Action, f.doorMove.Seconds(), t.Floor, t.Lobby
	f.send(r, elevator.Zone{Trip: t.Trip, N: t.N, Robot: r.Name, Action: t.Action, Event: elevator.ZoneEnter, Floor: c.Floor})
	c.Zone = append(c.Zone, r.Name)
	c.pubDirty = true
	c.MaxZone = max(c.MaxZone, len(c.Zone))
	c.Log = append(c.Log, r.Name+" "+t.Action+" start")
}

// doorStep moves a robot through the door zone and brings its link back
// after it left the car.
func (f *Fleet) doorStep(r *Robot, dt float64, now time.Time) {
	if r.ride == nil {
		return
	}
	if r.rideState == "out" {
		if !now.Before(r.linkAt) {
			f.rideDone(r)
		}
		return
	}
	if r.moveLeft <= 0 {
		return
	}
	r.moveLeft -= dt
	if r.moveLeft > 0 {
		return
	}
	c := f.cars[r.ride.Elevator]
	c.Zone = slices.DeleteFunc(c.Zone, func(n string) bool { return n == r.Name })
	c.pubDirty = true
	z := elevator.Zone{Trip: r.ride.Trip, N: r.moveN, Robot: r.Name, Action: r.moveAct, Floor: c.Floor}
	if r.moveAct == elevator.ActBoard {
		r.rideState, r.Mode = "inside", "RIDING"
		c.Load++
		z.Event = elevator.ZoneClear
		c.Log = append(c.Log, r.Name+" board clear")
		f.robotEvent(r, "boarded", c.Floor, "")
	} else if c.blocked[c.Floor] || r.failExits > 0 {
		if r.failExits > 0 {
			r.failExits--
		}
		r.rideState, r.Mode = "inside", "RIDING"
		z.Event, z.Reason = elevator.ZoneFailed, "door zone blocked"
		c.Log = append(c.Log, r.Name+" exit failed")
		f.robotEvent(r, "exit_failed", c.Floor, "door zone blocked")
	} else {
		c.Load--
		floor := c.Floor
		if r.wrongFloor != nil {
			floor, r.wrongFloor = *r.wrongFloor, nil
		}
		p := r.exitLobby // from the turn
		r.Floor, r.X, r.Y = floor, p.X, p.Y
		r.rideState, r.Mode = "out", "IDLE"
		r.linkAt = now.Add(f.linkBack + r.extraSilence)
		r.extraSilence = 0
		z.Event = elevator.ZoneClear
		c.Log = append(c.Log, r.Name+" exit clear")
		f.robotEvent(r, "exited", floor, "")
	}
	f.zone(r, z)
}

func (f *Fleet) zone(r *Robot, z elevator.Zone) {
	r.results[z.N] = z
	if r.dropZones > 0 {
		r.dropZones-- // lost on the way
		return
	}
	f.send(r, z)
}

// send publishes on elevator/<id>/zone.
func (f *Fleet) send(r *Robot, z elevator.Zone) {
	r.zoneSeq++
	z.Seq, z.Stamp = r.zoneSeq, float64(f.now().UnixMilli())/1000
	f.elevOut = append(f.elevOut, elevMsg{at: f.now().Add(f.elevDelay), car: r.ride.Elevator, zone: &z})
}

// rideDone: Wi-Fi is back. State without silence_s (next report), queued
// events, then the ride's done with the floor.
func (f *Fleet) rideDone(r *Robot) {
	r.silent = false
	floor := r.Floor
	cmd := r.rideCmd
	r.ride, r.rideCmd, r.rideState, r.Goal = nil, "", "", ""
	a := task.Ack{CmdID: cmd, Status: "done", Floor: &floor}
	r.book[cmd] = &a
	r.outbox = append(r.outbox, outAck{f.arrival(), a})
}

// robotEvent queues an event for fleet/<robot>/event; it is sent when the
// link is up (see Step).
func (f *Fleet) robotEvent(r *Robot, ev string, floor int, detail string) {
	r.evSeq++
	e := elevator.RobotEvent{Boot: "sim", Seq: r.evSeq, Stamp: float64(f.now().UnixMilli()) / 1000, Event: ev,
		CmdID: r.rideCmd, Floor: floor, Detail: detail}
	if r.ride != nil {
		e.Trip = r.ride.Trip
	}
	r.events = append(r.events, e)
}
