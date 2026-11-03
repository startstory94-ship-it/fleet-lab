package elevator

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"fleetlab/internal/fleet"
)

// Car conducts one elevator: it batches robots waiting at its lobbies, plans
// a trip, calls the car, and gives the door to one robot at a time.
//
// The conductor is the only one that tells a robot to move through the door
// (a Turn on elevator/<id>/turn). A turn is given only when
//   - no other turn is open (the last one was answered with zone clear or
//     failed, or timed out),
//   - the car reports the door open at that floor for this trip, recently,
//   - and the car reported its door zone empty (door_zone 0) in a state
//     received after the last turn ended.
//
// The last condition is the positive "zone empty" signal: a robot that is
// slow, silent or stuck in the door keeps everyone behind it waiting. A turn
// that times out is never a reason to let the next robot move; it counts as
// a failed try (retry, then carry or stuck). The open turn is re-sent with
// the same n every HoldRefresh, so a lost turn or a lost answer is repaired.
//
// Robots inside the car have no Wi-Fi; they act on the ride plan they
// accepted before boarding and on turns, both on the elevator channel.
//
// A Car is not safe for concurrent use: the task manager drives it under its
// own lock. It never calls back into the manager; what the manager must do
// with tasks comes back as Notices.
type Car struct {
	Cfg   CarConfig
	tm    Timing
	boot  string
	link  Link
	event func(kind, robot, trip, detail string)
	log   *slog.Logger

	state     State
	stateBoot string
	heard     time.Time
	stateN    uint64 // states accepted
	emptyN    uint64 // stateN of the last state with door_zone 0
	cmds      int
	waiting   map[string]*waiter
	trip      *Trip
	trips     int
	backoff   time.Time
	ended     map[string]bool // trips that ended here; their echoes are ignored

	// Runtime of the current trip (not published).
	acceptBy    time.Time
	arriveBy    time.Time
	travelBy    time.Time
	lastHold    time.Time
	lastCmd     time.Time
	lastCmdMsg  Cmd
	turnSent    time.Time // last publish of the open turn
	published   time.Time // last successful trip publish
	at          time.Time // now of the call being handled
	stuckAt     time.Time // when the car went blocked
	resolvedN   uint64    // stateN when the last turn ended or was given
	released    bool
	travelLate  bool
	leaseLate   bool
	silentLate  bool
	turnAt      map[string]time.Time // robot -> start of its last turn
	needPublish bool
}

type waiter struct {
	robot       string
	floor, dest int
	cmdID       string // the ride command it will get
	since       time.Time
}

// Notice kinds: what the task manager must do.
const (
	// NoticePlanned: the robot rides; send it the ride command (RidePlan).
	NoticePlanned = "planned"
	// NoticeAborted: the trip ended before the robot boarded; it is at the
	// lobby. Cancel its ride; Fault says it caused the abort.
	NoticeAborted = "aborted"
	// NoticeLeft: the robot did not board but the trip goes on without it.
	NoticeLeft = "left"
	// NoticeStuck: the robot could not get out (or was never confirmed out
	// before the lease ran out); it needs a human. Its task should go to
	// another robot. The car stays reserved until it is confirmed out.
	NoticeStuck = "stuck"
	// NoticeReleased: an operator released the car; nothing is known about
	// the robot. Its task should go to another robot.
	NoticeReleased = "released"
	// NoticeNoRide: the robot waits for its own floor; it needs no ride.
	NoticeNoRide = "no_ride"
)

type Notice struct {
	Kind   string
	Trip   string
	Robot  string
	CmdID  string
	Fault  bool
	Reason string
}

// NewCar creates the conductor of one car. boot is the server run id; it
// goes into trip and command ids. event receives conductor events (see
// docs/ELEVATOR.md for the kinds); it is called synchronously and must not
// block.
func NewCar(cfg CarConfig, tm Timing, boot string, link Link, event func(kind, robot, trip, detail string), log *slog.Logger) *Car {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if event == nil {
		event = func(string, string, string, string) {}
	}
	return &Car{Cfg: cfg, tm: tm, boot: boot, link: link, event: event, log: log,
		waiting: map[string]*waiter{}, turnAt: map[string]time.Time{}, ended: map[string]bool{}}
}

// RobotLookup gives the registry's view of a robot.
type RobotLookup func(name string) (fleet.Robot, bool)

// Wait puts a robot standing at the lobby of floor in the queue for dest.
// cmdID names the ride command it will get if it rides.
func (c *Car) Wait(robot string, floor, dest int, cmdID string, now time.Time) {
	if w, ok := c.waiting[robot]; ok && w.floor == floor && w.dest == dest {
		w.cmdID = cmdID
		return
	}
	c.waiting[robot] = &waiter{robot: robot, floor: floor, dest: dest, cmdID: cmdID, since: now}
}

// Unwait takes a robot out of the queue.
func (c *Car) Unwait(robot string) { delete(c.waiting, robot) }

// Waiting reports whether robot is queued.
func (c *Car) Waiting(robot string) bool { return c.waiting[robot] != nil }

// Trip returns a copy of the current trip.
func (c *Car) Trip() (Trip, bool) {
	if c.trip == nil {
		return Trip{}, false
	}
	return c.trip.Clone(), true
}

// Rider returns robot's part of trip, if that trip is current.
func (c *Car) Rider(trip, robot string) (Rider, string, bool) {
	if c.trip == nil || c.trip.ID != trip {
		return Rider{}, "", false
	}
	r := c.trip.rider(robot)
	if r == nil {
		return Rider{}, "", false
	}
	return *r, c.trip.Stage, true
}

// RidePlan is robot's part of the current trip, for its ride command.
func (c *Car) RidePlan(robot string) (RidePlan, bool) {
	t := c.trip
	if t == nil {
		return RidePlan{}, false
	}
	r := t.rider(robot)
	if r == nil {
		return RidePlan{}, false
	}
	order := make([]Rider, len(t.Riders))
	copy(order, t.Riders)
	sort.Slice(order, func(i, j int) bool { return order[i].Board < order[j].Board })
	names := make([]string, len(order))
	for i, o := range order {
		names[i] = o.Robot
	}
	return RidePlan{Trip: t.ID, Elevator: c.Cfg.ID, Origin: t.Origin, Direction: dirName(t.Dir),
		ExitFloor: r.Exit, Board: r.Board, Slot: r.Slot, Boarding: names, Stops: append([]int(nil), t.Stops...),
		SilenceS: r.SilenceS, TurnTimeoutS: c.tm.TurnTimeout.Seconds(),
		Lobby: c.Cfg.LobbyAt(t.Origin), ExitLobby: c.Cfg.LobbyAt(r.Exit)}, true
}

// State records a state message from the car.
func (c *Car) State(s State, now time.Time) []Notice {
	c.at = now
	if !c.heard.IsZero() && s.Boot == c.stateBoot && s.Seq <= c.state.Seq {
		return nil // old or duplicate
	}
	c.state, c.stateBoot, c.heard = s, s.Boot, now
	c.stateN++
	if s.DoorZone == 0 {
		c.emptyN = c.stateN
	}
	return c.progress(now, nil)
}

// Accepted records that robot accepted its ride command.
func (c *Car) Accepted(robot string, now time.Time) []Notice {
	c.at = now
	if c.trip == nil {
		return nil
	}
	r := c.trip.rider(robot)
	if r == nil || r.Accepted {
		return nil
	}
	r.Accepted = true
	c.publish()
	return c.progress(now, nil)
}

// Drop takes a robot out of the trip before it has moved (status waiting).
// It returns false when the robot is past that point: it may be in the car
// and the trip must carry it to the end.
func (c *Car) Drop(robot, reason string, now time.Time) (bool, []Notice) {
	c.at = now
	if c.trip == nil {
		return false, nil
	}
	r := c.trip.rider(robot)
	if r == nil || r.Status != RiderWaiting {
		return false, nil
	}
	r.Status = RiderLeft
	c.ev(StageAborted, robot, robot+" left the trip before boarding: "+reason)
	c.publish()
	return true, c.progress(now, nil)
}

// Out records that robot is outside the car on floor (it acked its ride
// after its Wi-Fi came back).
func (c *Car) Out(robot string, floor int, now time.Time) []Notice {
	c.at = now
	if c.trip == nil {
		return nil
	}
	r := c.trip.rider(robot)
	if r == nil || Accounted(r.Status) {
		return nil
	}
	if r.Status == RiderWaiting {
		r.Status = RiderLeft
	} else {
		r.Status, r.OutFloor = RiderOut, floor
		c.ev("exited", robot, fmt.Sprintf("%s reported out at floor %d after reconnecting", robot, floor))
	}
	c.endTurnOf(robot)
	c.publish()
	return c.progress(now, nil)
}

// Zone handles a robot's zone message.
func (c *Car) Zone(z Zone, now time.Time) []Notice {
	c.at = now
	t := c.trip
	if t == nil || z.Trip != t.ID {
		c.log.Debug("zone message for another trip", "car", c.Cfg.ID, "trip", z.Trip, "robot", z.Robot)
		return nil
	}
	if z.Action != ActBoard && z.Action != ActExit || z.Event != ZoneClear && z.Event != ZoneFailed && z.Event != ZoneEnter {
		c.log.Warn("zone message with unknown action or event; ignored", "car", c.Cfg.ID, "robot", z.Robot,
			"action", z.Action, "event", z.Event)
		return nil
	}
	if z.Event == ZoneEnter {
		return nil // informational: the car's door_zone is what counts
	}
	r := t.rider(z.Robot)
	if r == nil {
		return nil
	}
	var out []Notice
	if t.Turn != nil && z.N == t.Turn.N && z.Robot == t.Turn.Robot && z.Action == t.Turn.Action {
		out = c.resolve(r, z, now)
	} else {
		out = c.late(r, z)
	}
	return append(out, c.progress(now, nil)...)
}

// Tick runs the clocks: batching, deadlines, hold refresh, re-sent turns,
// lease, and accounting for robots seen back on the fleet link. robots may
// be nil.
func (c *Car) Tick(now time.Time, robots RobotLookup) []Notice {
	c.at = now
	return c.progress(now, robots)
}

// Republish publishes the current trip again (after an MQTT reconnect: a
// retained message published while the broker was away may be missing).
func (c *Car) Republish() {
	if c.trip != nil {
		c.publish()
	}
}

// Ended reports whether a trip ended in this run (its retained echoes are
// ignored).
func (c *Car) Ended(trip string) bool { return c.ended[trip] }

// Restore adopts a trip published by an earlier fleetd run (the retained
// elevator/<id>/trip). A trip nobody had boarded yet is aborted; one with
// robots in the car goes on from its stage with fresh timers. The trip
// becomes this run's (Boot), so its own retained messages are never taken
// for an earlier run's again.
func (c *Car) Restore(t Trip, now time.Time) []Notice {
	c.at = now
	if c.trip != nil || c.ended[t.ID] || t.Stage == StageDone || t.Stage == StageAborted || t.Car != c.Cfg.ID {
		return nil
	}
	tt := t.Clone()
	tt.Boot = c.boot
	c.trip = &tt
	c.resetRuntime(now)
	c.released = t.Stage == StageBlocked
	if t.Stage == StagePlanned {
		return c.abort(now, "fleetd restarted before boarding", false, nil)
	}
	tt.Lease = now.Add(c.estimateRest() + c.tm.LeaseMargin)
	c.ev("replanned", "", fmt.Sprintf("trip restored after a fleetd restart at stage %s, floor %d", t.Stage, t.Floor))
	switch t.Stage {
	case StageTravel:
		c.travelBy = now.Add(c.travelTime(c.Cfg.Floors[0], c.Cfg.Floors[len(c.Cfg.Floors)-1]) + c.tm.TravelMargin)
		c.send(now, Cmd{Cmd: CmdGoto, Floor: tt.Floor, HoldS: c.tm.HoldFor.Seconds()})
	case StageBoarding, StageExiting:
		// The door may have closed while we were down: goto the same floor
		// reopens it. The open turn is re-sent (same n) once the door is open.
		c.send(now, Cmd{Cmd: CmdGoto, Floor: tt.Floor, HoldS: c.tm.HoldFor.Seconds()})
		if tt.Turn != nil {
			tt.Turn.Deadline = now.Add(c.tm.TurnTimeout)
			c.turnAt[tt.Turn.Robot] = now
		}
	case StageBlocked:
		c.stuckAt = now
	}
	c.publish()
	return c.progress(now, nil)
}

// ForceRelease ends the current trip on an operator's word: the car is
// released, robots that had not moved are sent back to wait, and every
// robot not confirmed out is reported (NoticeReleased) so its task goes to
// another robot. It returns false if there is no trip.
func (c *Car) ForceRelease(now time.Time, who string) (bool, []Notice) {
	c.at = now
	t := c.trip
	if t == nil {
		return false, nil
	}
	var out []Notice
	var unknown []string
	for _, r := range t.Riders {
		switch {
		case r.Status == RiderWaiting:
			out = append(out, Notice{Kind: NoticeAborted, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Reason: "car released by " + who})
		case !Accounted(r.Status):
			unknown = append(unknown, r.Robot)
			if !r.Notified {
				out = append(out, Notice{Kind: NoticeReleased, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID,
					Reason: "elevator " + c.Cfg.ID + " released by " + who})
			}
		}
	}
	t.Detail = "released by " + who
	if len(unknown) > 0 {
		t.Detail += "; not confirmed out: " + strings.Join(unknown, ",")
	}
	c.end(now, StageDone, "released", t.Detail, !c.released)
	return true, out
}

func (c *Car) resetRuntime(now time.Time) {
	c.acceptBy, c.arriveBy, c.travelBy = time.Time{}, time.Time{}, time.Time{}
	c.lastHold, c.lastCmd, c.lastCmdMsg = time.Time{}, time.Time{}, Cmd{}
	c.turnSent, c.published, c.stuckAt = time.Time{}, time.Time{}, time.Time{}
	c.released, c.travelLate, c.leaseLate, c.silentLate = false, false, false, false
	c.resolvedN = c.stateN
	clear(c.turnAt)
}

// progress moves the trip on as far as it can go now.
func (c *Car) progress(now time.Time, robots RobotLookup) []Notice {
	var out []Notice
	if c.trip != nil && robots != nil {
		out = append(out, c.account(now, robots)...)
	}
	if c.trip != nil {
		switch c.trip.Stage {
		case StagePlanned:
			out = append(out, c.progressPlanned(now)...)
		case StageBoarding, StageExiting:
			out = append(out, c.progressGate(now)...)
		case StageTravel:
			out = append(out, c.progressTravel(now)...)
		case StageBlocked:
			out = append(out, c.progressBlocked(now)...)
		}
	}
	if c.trip != nil {
		out = append(out, c.checkLease(now)...)
	}
	if c.trip != nil && (c.needPublish || now.Sub(c.published) >= c.tm.Republish) {
		c.publish()
	}
	if c.trip == nil {
		out = append(out, c.maybePlan(now)...)
	}
	return out
}

func (c *Car) maybePlan(now time.Time) []Notice {
	if c.trip != nil || now.Before(c.backoff) || len(c.waiting) == 0 {
		return nil
	}
	var out []Notice
	for _, w := range c.waiting {
		if w.floor == w.dest {
			out = append(out, Notice{Kind: NoticeNoRide, Robot: w.robot, Reason: "already on its floor"})
			delete(c.waiting, w.robot)
		}
	}
	var first *waiter
	for _, w := range c.waiting {
		if first == nil || w.since.Before(first.since) || w.since.Equal(first.since) && w.robot < first.robot {
			first = w
		}
	}
	if first == nil || now.Sub(first.since) < c.tm.BatchWindow {
		return out
	}
	req := Request{Origin: first.floor, Dir: sign(first.dest - first.floor), Capacity: c.Cfg.Capacity}
	byRobot := map[string]*waiter{}
	for _, w := range c.waiting {
		if w.floor == first.floor {
			req.Robots = append(req.Robots, Want{w.robot, w.dest})
			byRobot[w.robot] = w
		}
	}
	sort.Slice(req.Robots, func(i, j int) bool { return req.Robots[i].Robot < req.Robots[j].Robot })
	p := PlanTrip(req)
	if len(p.Seats) == 0 {
		return out
	}
	c.trips++
	t := &Trip{ID: fmt.Sprintf("%s-%s-%d", c.Cfg.ID, c.boot, c.trips), Boot: c.boot, Car: c.Cfg.ID, Stage: StagePlanned,
		Origin: p.Origin, Dir: p.Dir, Stops: p.Stops, Floor: p.Origin, Planned: now}
	for _, s := range p.Seats {
		t.Riders = append(t.Riders, Rider{Robot: s.Robot, CmdID: byRobot[s.Robot].cmdID, Dest: s.Dest, Exit: s.Dest,
			Board: s.Board, Slot: s.Slot, Status: RiderWaiting})
		delete(c.waiting, s.Robot)
	}
	c.trip = t
	c.resetRuntime(now)
	c.setSilences()

	from := c.Cfg.Floors[len(c.Cfg.Floors)-1] // unknown: assume the far end
	if !c.heard.IsZero() {
		from = c.state.Floor
	}
	call := c.travelTime(from, t.Origin) + c.Cfg.doorTime()
	t.Lease = now.Add(call + c.estimateRest() + c.tm.LeaseMargin)
	c.acceptBy = now.Add(c.tm.AcceptTimeout)
	c.arriveBy = now.Add(call + c.tm.CallMargin)

	var deferred []string
	for _, d := range p.Deferred {
		deferred = append(deferred, fmt.Sprintf("%s(%s)", d.Robot, d.Reason))
	}
	detail := fmt.Sprintf("from %d %s: board %s; stops %v", t.Origin, dirName(t.Dir), strings.Join(p.Riders(), ","), t.Stops)
	if len(deferred) > 0 {
		detail += "; next trip " + strings.Join(deferred, ",")
	}
	c.ev("trip_planned", "", detail)
	c.send(now, Cmd{Cmd: CmdCall, Floor: t.Origin, HoldS: c.tm.HoldFor.Seconds()})
	c.ev("called", "", fmt.Sprintf("to floor %d", t.Origin))
	c.publish()
	for _, r := range t.Riders {
		out = append(out, Notice{Kind: NoticePlanned, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID})
	}
	return out
}

// travelTime estimates the car's travel between two floors with door close
// and open.
func (c *Car) travelTime(from, to int) time.Duration {
	return time.Duration(abs(to-from))*c.Cfg.floorTime() + 2*c.Cfg.doorTime()
}

// setSilences sets each rider's announced blackout: from its boarding turn
// to the end of its exit turn, plus SilenceMargin.
func (c *Car) setSilences() {
	t := c.trip
	n := len(t.Riders)
	at := map[int]time.Duration{} // stop -> travel time from the origin's door close
	prev, sum := t.Origin, time.Duration(0)
	for _, s := range t.Stops {
		sum += c.travelTime(prev, s)
		at[s] = sum
		prev = s
	}
	te := c.tm.TurnEstimate
	for i := range t.Riders {
		r := &t.Riders[i]
		d := time.Duration(n-r.Board)*te + at[r.Exit] + time.Duration(r.Slot+1)*te + c.tm.SilenceMargin
		r.SilenceS = d.Round(time.Second).Seconds()
	}
}

// estimateRest is the time from boarding to the last exit.
func (c *Car) estimateRest() time.Duration {
	t := c.trip
	n := time.Duration(len(t.Riders))
	d := 2 * n * c.tm.TurnEstimate
	prev := t.Origin
	for _, s := range t.Stops {
		d += c.travelTime(prev, s)
		prev = s
	}
	return d + c.Cfg.doorTime()
}

// fresh reports whether the car was heard within Silence.
func (c *Car) fresh(now time.Time) bool { return !c.heard.IsZero() && now.Sub(c.heard) < c.tm.Silence }

// atGate reports whether the last state shows the door open at floor for
// the current trip.
func (c *Car) atGate(floor int) bool {
	s := c.state
	return !c.heard.IsZero() && c.trip != nil && s.Trip == c.trip.ID && s.Floor == floor &&
		s.Door == DoorOpen && !s.Moving
}

// zoneEmpty is the positive signal: a state with door_zone 0 arrived after
// the last turn ended (or was given).
func (c *Car) zoneEmpty() bool { return c.emptyN > c.resolvedN }

func (c *Car) progressPlanned(now time.Time) []Notice {
	t := c.trip
	all, some := true, false
	for _, r := range t.Riders {
		if r.Status == RiderWaiting {
			some = true
			all = all && r.Accepted
		}
	}
	if !some {
		return c.abort(now, "no rider left", false, nil)
	}
	if all && c.atGate(t.Origin) && c.fresh(now) {
		t.Stage = StageBoarding
		c.lastHold = now
		c.publish()
		return c.progressGate(now)
	}
	if !now.Before(c.acceptBy) && !all {
		faulty := map[string]bool{}
		var names []string
		for _, r := range t.Riders {
			if r.Status == RiderWaiting && !r.Accepted {
				faulty[r.Robot] = true
				names = append(names, r.Robot)
			}
		}
		return c.abort(now, "no accept from "+strings.Join(names, ","), false, faulty)
	}
	silentFor := now.Sub(c.heard)
	if c.heard.IsZero() || c.heard.Before(t.Planned) {
		silentFor = now.Sub(t.Planned)
	}
	if silentFor >= c.tm.Silence {
		return c.abort(now, fmt.Sprintf("elevator not responding (no state for %s)", silentFor.Round(time.Second)), true, nil)
	}
	if !now.Before(c.arriveBy) {
		return c.abort(now, fmt.Sprintf("elevator not open at floor %d in time", t.Origin), true, nil)
	}
	if !c.atGate(t.Origin) && now.Sub(c.lastCmd) >= c.tm.CmdRetry {
		c.resend(now)
	}
	return nil
}

// progressGate runs a boarding or exiting gate: door open, the open turn,
// and the next turn when the door zone is known to be empty.
func (c *Car) progressGate(now time.Time) []Notice {
	t := c.trip
	var out []Notice
	if t.Stage == StageExiting && c.allAccounted() {
		c.finish(now, "every robot is out")
		return nil
	}
	fresh := c.fresh(now)
	if !fresh && !c.silentLate {
		c.silentLate = true
		c.ev("overdue", "", fmt.Sprintf("car not answering at floor %d; no robot moves until it does", t.Floor))
	} else if fresh {
		c.silentLate = false
	}
	open := fresh && c.atGate(t.Floor)
	if !open && now.Sub(c.lastCmd) >= c.tm.CmdRetry {
		// Door closed (hold ran out, restart) or not heard: goto the same
		// floor reopens and holds it.
		c.send(now, Cmd{Cmd: CmdGoto, Floor: t.Floor, HoldS: c.tm.HoldFor.Seconds()})
	}
	if open && now.Sub(c.lastHold) >= c.tm.HoldRefresh {
		c.lastHold = now
		c.send(now, Cmd{Cmd: CmdHold, Floor: t.Floor, HoldS: c.tm.HoldFor.Seconds()})
	}
	if t.Turn != nil {
		if !now.Before(t.Turn.Deadline) {
			out = append(out, c.turnTimeout(now)...)
		} else if open && now.Sub(c.turnSent) >= c.tm.HoldRefresh {
			c.publishTurn() // same n: repairs a lost turn or a lost answer
		}
	}
	for c.trip != nil && c.trip.Turn == nil && (c.trip.Stage == StageBoarding || c.trip.Stage == StageExiting) {
		if !open || !c.zoneEmpty() {
			break // wait for the door open and the zone positively empty
		}
		if r := c.nextInGate(); r != nil {
			c.grant(r, now)
			break
		}
		out = append(out, c.gateDone(now)...)
	}
	return out
}

// possiblyInside: the conductor treats the robot as in the car.
func possiblyInside(status string) bool {
	return status == RiderInside || status == RiderUnsureIn || status == RiderExiting || status == RiderStuck
}

// nextInGate is the robot whose turn is next. Boarding: by boarding order.
// Exiting: only the front-most robot that may be inside can move; it gets
// the turn if it gets out here. Nobody behind it is ever given the door.
func (c *Car) nextInGate() *Rider {
	t := c.trip
	var best *Rider
	for i := range t.Riders {
		r := &t.Riders[i]
		switch t.Stage {
		case StageBoarding:
			if r.Status == RiderWaiting && (best == nil || r.Board < best.Board) {
				best = r
			}
		case StageExiting:
			if possiblyInside(r.Status) && (best == nil || r.Slot < best.Slot) {
				best = r
			}
		}
	}
	if t.Stage == StageExiting && best != nil && (best.Exit != t.Floor || best.Status == RiderStuck) {
		return nil
	}
	return best
}

func (c *Car) grant(r *Rider, now time.Time) {
	t := c.trip
	t.N++
	act, status, kind := ActBoard, RiderBoarding, "boarding"
	if t.Stage == StageExiting {
		act, status, kind = ActExit, RiderExiting, "exiting"
	}
	r.Status = status
	t.Turn = &Turn{Trip: t.ID, N: t.N, Robot: r.Robot, Action: act, Floor: t.Floor, Lobby: c.Cfg.LobbyAt(t.Floor),
		TimeoutS: c.tm.TurnTimeout.Seconds(), Deadline: now.Add(c.tm.TurnTimeout)}
	c.turnAt[r.Robot] = now
	c.resolvedN = c.stateN
	c.ev(kind, r.Robot, fmt.Sprintf("turn %d: %s %s at floor %d, slot %d", t.N, r.Robot, act, t.Floor, r.Slot))
	c.publishTurn()
	c.publish()
}

// endTurnOf closes the open turn if it is robot's.
func (c *Car) endTurnOf(robot string) {
	if t := c.trip; t != nil && t.Turn != nil && t.Turn.Robot == robot {
		t.Turn = nil
		c.resolvedN = c.stateN
	}
}

func (c *Car) resolve(r *Rider, z Zone, now time.Time) []Notice {
	t := c.trip
	act := t.Turn.Action
	t.Turn = nil
	c.resolvedN = c.stateN
	var out []Notice
	switch {
	case act == ActBoard && z.Event == ZoneClear:
		r.Status, r.Tries = RiderInside, 0
		c.ev("boarded", r.Robot, fmt.Sprintf("%s in slot %d", r.Robot, r.Slot))
	case act == ActBoard:
		r.Status = RiderLeft
		c.ev(StageAborted, r.Robot, fmt.Sprintf("%s could not board (%s); the trip goes on without it", r.Robot, z.Reason))
		out = append(out, Notice{Kind: NoticeLeft, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Fault: true, Reason: "could not board: " + z.Reason})
	case z.Event == ZoneClear:
		r.Status, r.OutFloor = RiderOut, t.Floor
		c.ev("exited", r.Robot, fmt.Sprintf("%s at floor %d", r.Robot, t.Floor))
	default:
		out = append(out, c.failedTry(r, "exit_failed", fmt.Sprintf("%s at floor %d (%s)", r.Robot, t.Floor, z.Reason))...)
	}
	c.publish()
	return out
}

// failedTry counts a failed exit (reported, or no answer in time: the robot
// is treated as still inside). Within ExitRetries it gets the door again;
// after that it is carried to the next stop, or stuck at the last one.
func (c *Car) failedTry(r *Rider, kind, what string) []Notice {
	r.Tries++
	r.Status = RiderInside
	if r.Tries <= c.tm.ExitRetries {
		c.ev(kind, r.Robot, fmt.Sprintf("%s, try %d; retrying", what, r.Tries))
		return nil
	}
	c.ev(kind, r.Robot, fmt.Sprintf("%s, gave up after %d tries", what, r.Tries))
	return c.carry(r)
}

// carry moves a robot that could not get out at the current stop to the
// next stop ahead of the car. With none it is stuck and needs a human.
// Robots behind it that were to get out here are carried by gateDone: they
// cannot pass it.
func (c *Car) carry(r *Rider) []Notice {
	t := c.trip
	from := t.Floor
	r.Tries = 0
	if next, ok := c.nextStop(from); ok {
		r.Status, r.Exit = RiderInside, next
		c.ev("replanned", r.Robot, fmt.Sprintf("%s exits at floor %d instead of %d (could not get out at %d)", r.Robot, next, from, from))
		return nil
	}
	r.Status = RiderStuck
	t.Detail = r.Robot + " cannot get out; needs a human"
	c.ev("exit_failed", r.Robot, fmt.Sprintf("%s cannot get out at the last stop %d; needs a human", r.Robot, from))
	if r.Notified {
		return nil
	}
	r.Notified = true
	return []Notice{{Kind: NoticeStuck, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Fault: true, Reason: "stuck in elevator " + c.Cfg.ID}}
}

// nextStop is the first planned stop beyond floor in the direction of travel.
func (c *Car) nextStop(floor int) (int, bool) {
	t := c.trip
	for _, s := range t.Stops {
		if (s-floor)*t.Dir > 0 {
			return s, true
		}
	}
	return 0, false
}

// turnTimeout: no answer in TurnTimeout. The robot behind does not move.
func (c *Car) turnTimeout(now time.Time) []Notice {
	t := c.trip
	r := t.rider(t.Turn.Robot)
	act := t.Turn.Action
	t.Turn = nil
	c.resolvedN = c.stateN
	defer c.publish()
	if r == nil {
		return nil
	}
	what := fmt.Sprintf("no zone message from %s (%s) in %s", r.Robot, act, c.tm.TurnTimeout)
	if act == ActExit {
		return c.failedTry(r, "overdue", what+"; treated as still inside")
	}
	r.Tries++
	if r.Tries <= c.tm.ExitRetries {
		r.Status = RiderWaiting
		c.ev("overdue", r.Robot, fmt.Sprintf("%s, try %d; retrying once the door zone is empty", what, r.Tries))
		return nil
	}
	r.Status, r.Tries = RiderUnsureIn, 0
	c.ev("overdue", r.Robot, what+"; treated as inside, the next robot boards once the door zone is empty")
	return nil
}

// late handles a zone message that does not answer the open turn: an
// answer to an older turn, or a repeat.
func (c *Car) late(r *Rider, z Zone) []Notice {
	t := c.trip
	switch {
	case z.Action == ActExit && z.Event == ZoneClear && !Accounted(r.Status) && r.Status != RiderWaiting:
		r.Status, r.OutFloor = RiderOut, z.Floor
		c.endTurnOf(r.Robot)
		c.ev("exited", r.Robot, fmt.Sprintf("%s at floor %d (late zone clear)", r.Robot, z.Floor))
	case z.Action == ActBoard && z.Event == ZoneClear && (r.Status == RiderUnsureIn || r.Status == RiderWaiting && r.Tries > 0):
		// It boarded after its turn timed out (slow): inside, no retry needed.
		r.Status, r.Tries = RiderInside, 0
		c.ev("boarded", r.Robot, fmt.Sprintf("%s in slot %d (late zone clear)", r.Robot, r.Slot))
	case z.Action == ActBoard && z.Event == ZoneFailed && r.Status == RiderUnsureIn:
		r.Status = RiderLeft
		c.ev(StageAborted, r.Robot, fmt.Sprintf("%s could not board (%s, late)", r.Robot, z.Reason))
		c.publish()
		return []Notice{{Kind: NoticeLeft, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Fault: true, Reason: "could not board: " + z.Reason}}
	default:
		// A late "failed" for an exit: the timeout already counted the robot
		// as still inside, and it stays in front; nothing to change.
		return nil
	}
	c.publish()
	return nil
}

func (c *Car) gateDone(now time.Time) []Notice {
	t := c.trip
	var out []Notice
	if t.Stage == StageExiting {
		// Robots that were to get out here but stand behind one that could
		// not: carried too.
		for i := range t.Riders {
			r := &t.Riders[i]
			if (r.Status == RiderInside || r.Status == RiderUnsureIn) && r.Exit == t.Floor {
				out = append(out, c.carry(r)...)
			}
		}
	}
	if next, ok := c.nextExit(); ok {
		t.Stage, t.Floor = StageTravel, next
		from := c.state.Floor
		if c.heard.IsZero() {
			from = t.Origin
		}
		c.travelBy = now.Add(c.travelTime(from, next) + c.tm.TravelMargin)
		c.travelLate = false
		c.send(now, Cmd{Cmd: CmdGoto, Floor: next, HoldS: c.tm.HoldFor.Seconds()})
		c.ev("departed", "", fmt.Sprintf("to floor %d", next))
		c.publish()
		return out
	}
	if t.Stage == StageBoarding {
		c.finish(now, "nobody boarded")
		return out
	}
	if c.allAccounted() {
		c.finish(now, "every robot is out")
		return out
	}
	c.block(now)
	return out
}

// block: every stop is done but a robot may still be inside. The door is
// released; the car stays reserved; exits are tried again every StuckRetry.
func (c *Car) block(now time.Time) {
	t := c.trip
	c.send(now, Cmd{Cmd: CmdRelease, Floor: t.Floor})
	c.released = true
	t.Stage, t.Turn = StageBlocked, nil
	c.stuckAt = now
	var who []string
	for _, r := range t.Riders {
		if !Accounted(r.Status) {
			who = append(who, r.Robot+"("+r.Status+")")
		}
	}
	if t.Detail == "" {
		t.Detail = "not confirmed out: " + strings.Join(who, ",")
	}
	c.ev("overdue", "", "car kept reserved; not confirmed out: "+strings.Join(who, ","))
	c.publish()
}

// progressBlocked retries the exit of robots still (maybe) inside, at the
// car's floor, every StuckRetry.
func (c *Car) progressBlocked(now time.Time) []Notice {
	t := c.trip
	if c.allAccounted() {
		c.finish(now, "every robot is out")
		return nil
	}
	if now.Sub(c.stuckAt) < c.tm.StuckRetry {
		return nil
	}
	if c.fresh(now) && c.state.Floor != t.Floor {
		t.Floor = c.state.Floor // moved by hand: try where it is
	}
	var who []string
	for i := range t.Riders {
		r := &t.Riders[i]
		if !Accounted(r.Status) && r.Status != RiderWaiting {
			r.Status, r.Exit, r.Tries = RiderInside, t.Floor, 0
			who = append(who, r.Robot)
		}
	}
	t.Stage, c.released = StageExiting, false
	c.lastCmd = time.Time{}
	c.resolvedN = c.stateN
	c.ev("replanned", "", fmt.Sprintf("trying the exit again at floor %d for %s", t.Floor, strings.Join(who, ",")))
	c.publish()
	return c.progressGate(now)
}

// nextExit is the nearest exit floor ahead of any robot still riding.
func (c *Car) nextExit() (int, bool) {
	t := c.trip
	best, ok := 0, false
	for _, r := range t.Riders {
		if r.Status != RiderInside && r.Status != RiderUnsureIn {
			continue
		}
		if !ok || (r.Exit-best)*t.Dir < 0 {
			best, ok = r.Exit, true
		}
	}
	return best, ok
}

func (c *Car) progressTravel(now time.Time) []Notice {
	t := c.trip
	if _, ok := c.nextExit(); !ok {
		// Everyone riding was accounted out on the way (seen back on the
		// fleet link): nothing to stop for.
		if c.allAccounted() {
			c.finish(now, "every robot is out")
		} else {
			c.block(now)
		}
		return nil
	}
	if c.atGate(t.Floor) && c.fresh(now) {
		t.Stage = StageExiting
		c.lastHold = now
		c.resolvedN = c.stateN - 1 // the state that showed the door open counts for the zone
		n := 0
		for _, r := range t.Riders {
			if possiblyInside(r.Status) && r.Exit == t.Floor {
				n++
			}
		}
		c.ev("stop", "", fmt.Sprintf("floor %d, %d to get out", t.Floor, n))
		c.publish()
		return c.progressGate(now)
	}
	if !now.Before(c.travelBy) && !c.travelLate {
		c.travelLate = true
		c.ev("overdue", "", fmt.Sprintf("car not open at floor %d in time (robots inside)", t.Floor))
	}
	if now.Sub(c.lastCmd) >= c.tm.CmdRetry {
		c.resend(now)
	}
	return nil
}

// account settles robots from the registry: a robot back on the fleet link
// (ONLINE, heard after its last turn began) is not in the car, because
// Wi-Fi does not reach inside.
func (c *Car) account(now time.Time, robots RobotLookup) []Notice {
	t := c.trip
	var out []Notice
	changed := false
	for i := range t.Riders {
		r := &t.Riders[i]
		at, hadTurn := c.turnAt[r.Robot]
		// A robot with its boarding turn open is at the lobby by design (the
		// turn may be lost and re-sent): only the turn and its retries settle it.
		if Accounted(r.Status) || r.Status == RiderWaiting || r.Status == RiderBoarding || !hadTurn && t.Stage == StagePlanned {
			continue
		}
		rb, ok := robots(r.Robot)
		if !ok || rb.Conn != fleet.Online || rb.Mode == "" || !rb.HasPose || !rb.LastSeen.After(at) {
			continue
		}
		changed = true
		c.endTurnOf(r.Robot)
		if r.Status == RiderUnsureIn && rb.Floor == t.Origin {
			r.Status = RiderLeft
			c.ev(StageAborted, r.Robot, fmt.Sprintf("%s did not board (seen at the lobby)", r.Robot))
			out = append(out, Notice{Kind: NoticeLeft, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Reason: "missed its boarding turn"})
			continue
		}
		r.Status, r.OutFloor = RiderOut, rb.Floor
		c.ev("exited", r.Robot, fmt.Sprintf("%s seen on the fleet link at floor %d", r.Robot, rb.Floor))
	}
	if changed {
		c.publish()
	}
	return out
}

func (c *Car) allAccounted() bool {
	for _, r := range c.trip.Riders {
		if !Accounted(r.Status) {
			return false
		}
	}
	return true
}

// checkLease: past the lease a trip nobody boarded is aborted; otherwise an
// alarm is raised and every robot not confirmed out is reported stuck (its
// task goes to another robot). The car stays reserved until they are out
// or an operator releases it.
func (c *Car) checkLease(now time.Time) []Notice {
	t := c.trip
	if c.leaseLate || now.Before(t.Lease) {
		return nil
	}
	if t.Stage == StagePlanned {
		return c.abort(now, "lease expired before boarding", false, nil)
	}
	c.leaseLate = true
	var who []string
	var out []Notice
	for i := range t.Riders {
		r := &t.Riders[i]
		if Accounted(r.Status) || r.Status == RiderWaiting {
			continue
		}
		who = append(who, r.Robot)
		if !r.Notified {
			r.Notified = true
			out = append(out, Notice{Kind: NoticeStuck, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Fault: true,
				Reason: "not confirmed out of elevator " + c.Cfg.ID + " when its lease expired"})
		}
	}
	c.ev("overdue", "", fmt.Sprintf("lease expired at stage %s; car stays reserved until out: %s", t.Stage, strings.Join(who, ",")))
	c.publish()
	return out
}

// abort ends a trip nobody has boarded. Robots still waiting get a notice.
func (c *Car) abort(now time.Time, reason string, carFault bool, faulty map[string]bool) []Notice {
	t := c.trip
	var out []Notice
	for _, r := range t.Riders {
		if r.Status == RiderWaiting {
			out = append(out, Notice{Kind: NoticeAborted, Trip: t.ID, Robot: r.Robot, CmdID: r.CmdID, Fault: faulty[r.Robot], Reason: reason})
		}
	}
	t.Detail = reason
	c.end(now, StageAborted, StageAborted, reason, true)
	if carFault {
		c.backoff = now.Add(c.tm.Backoff)
	}
	return out
}

func (c *Car) finish(now time.Time, detail string) {
	c.end(now, StageDone, "released", detail, !c.released)
}

// end closes the trip: release (if needed), the final stage published, then
// the retained message cleared, and the id remembered so a late echo is not
// adopted again.
func (c *Car) end(now time.Time, stage, kind, detail string, release bool) {
	t := c.trip
	if release {
		c.send(now, Cmd{Cmd: CmdRelease, Floor: t.Floor})
	}
	t.Stage, t.Turn = stage, nil
	c.ev(kind, "", detail)
	c.publish()
	if err := c.link.ClearTrip(c.Cfg.ID); err != nil {
		c.log.Warn("retained trip not cleared", "car", c.Cfg.ID, "err", err)
	}
	c.ended[t.ID] = true
	c.trip = nil
}

func (c *Car) send(now time.Time, cmd Cmd) {
	c.cmds++
	cmd.Trip = c.trip.ID
	cmd.CmdID = fmt.Sprintf("%s.c%d.%s", c.trip.ID, c.cmds, c.boot)
	c.lastCmd = now
	if cmd.Cmd == CmdCall || cmd.Cmd == CmdGoto {
		c.lastCmdMsg = cmd
	}
	if err := c.link.Command(c.Cfg.ID, cmd); err != nil {
		c.log.Warn("elevator command not sent", "car", c.Cfg.ID, "cmd", cmd.Cmd, "err", err)
	}
}

// resend repeats the last call/goto with the same cmd_id.
func (c *Car) resend(now time.Time) {
	if c.lastCmdMsg.CmdID == "" {
		return
	}
	c.lastCmd = now
	if err := c.link.Command(c.Cfg.ID, c.lastCmdMsg); err != nil {
		c.log.Warn("elevator command not sent", "car", c.Cfg.ID, "cmd", c.lastCmdMsg.Cmd, "err", err)
	}
}

// publish sends the whole trip (retained). A failure is retried by the
// next progress; it is also re-sent every Timing.Republish.
func (c *Car) publish() {
	if err := c.link.PublishTrip(c.Cfg.ID, c.trip.Clone()); err != nil {
		c.needPublish = true
		c.log.Warn("trip not published; will retry", "car", c.Cfg.ID, "err", err)
		return
	}
	c.needPublish = false
	c.published = c.at
}

func (c *Car) publishTurn() {
	if err := c.link.PublishTurn(c.Cfg.ID, *c.trip.Turn); err != nil {
		c.log.Warn("turn not published; will retry", "car", c.Cfg.ID, "err", err)
		c.turnSent = time.Time{}
		return
	}
	c.turnSent = c.at
}

func (c *Car) ev(kind, robot, detail string) {
	trip := ""
	if c.trip != nil {
		trip = c.trip.ID
	}
	c.event(kind, robot, trip, detail)
}

// View is GET /elevators.
type View struct {
	ID         string     `json:"id"`
	Capacity   int        `json:"capacity"`
	Floors     []int      `json:"floors"`
	Lobby      Point      `json:"lobby"`
	State      *State     `json:"state"` // nil until the car is heard
	Heard      time.Time  `json:"heard,omitzero"`
	Responding bool       `json:"responding"` // heard within Timing.Silence
	Trip       *Trip      `json:"trip"`
	Waiting    []WaitView `json:"waiting"`
	Backoff    time.Time  `json:"backoff_until,omitzero"`
}

type WaitView struct {
	Robot string    `json:"robot"`
	Floor int       `json:"floor"`
	Dest  int       `json:"dest"`
	Since time.Time `json:"since"`
}

func (c *Car) View(now time.Time) View {
	v := View{ID: c.Cfg.ID, Capacity: c.Cfg.Capacity, Floors: append([]int(nil), c.Cfg.Floors...), Lobby: c.Cfg.Lobby,
		Heard: c.heard, Responding: c.fresh(now), Waiting: []WaitView{}}
	if !c.heard.IsZero() {
		s := c.state
		v.State = &s
	}
	if c.trip != nil {
		t := c.trip.Clone()
		v.Trip = &t
	}
	if now.Before(c.backoff) {
		v.Backoff = c.backoff
	}
	for _, w := range c.waiting {
		v.Waiting = append(v.Waiting, WaitView{w.robot, w.floor, w.dest, w.since})
	}
	sort.Slice(v.Waiting, func(i, j int) bool { return v.Waiting[i].Robot < v.Waiting[j].Robot })
	return v
}
