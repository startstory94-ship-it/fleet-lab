package task

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/fleet"
)

// Elevator rides.
//
// A leg whose waypoint is on another floor than the robot runs in phases:
//
//	PhaseLobby  goto the elevator lobby on the robot's floor (a normal goto)
//	PhaseWait   at the lobby, queued in the car's batch (no command out)
//	PhaseRide   ride command sent; the car's conductor (elevator.Car) runs
//	            the trip, the robot acks "done" once out of the car with
//	            its Wi-Fi back
//	""          goto to the waypoint on the destination floor
//
// If the robot comes out on another floor than the waypoint's, the leg is
// planned again from there (lobby, wait, ride). The task stays RUNNING
// through the phases.
//
// What is the robot's and what is the car's: while the robot waits for its
// boarding turn, the task is followed like any command (ack, resend, robot
// lost). From its boarding turn until it is accounted out of the car, the
// conductor owns it: the task is neither failed for silence or OFFLINE nor
// taken back for battery. Once out, the usual rules apply again.
const (
	PhaseLobby = "lobby"
	PhaseWait  = "wait"
	PhaseRide  = "ride"
)

func (m *Manager) initCars() {
	for _, cfg := range m.d.Elevators.Elevators {
		id := cfg.ID
		m.cars[id] = elevator.NewCar(cfg, m.d.Elevators.Timing, m.d.Boot, m.d.Lift,
			func(kind, robot, trip, detail string) { m.elevEvent(id, kind, robot, trip, detail) }, m.d.Log)
		m.carIDs = append(m.carIDs, id)
	}
	sort.Strings(m.carIDs)
}

// carFor is the first car (by id) serving both floors.
func (m *Manager) carFor(from, to int) *elevator.Car {
	for _, id := range m.carIDs {
		if c := m.cars[id]; c.Cfg.Serves(from) && c.Cfg.Serves(to) {
			return c
		}
	}
	return nil
}

// prepareLeg sets up the current leg for a robot on floor and names its
// first command; the caller sends it with sendGoto. Without elevators every
// leg is a plain goto, as before stage 6.
func (m *Manager) prepareLeg(t *Task, floor int, now time.Time) error {
	w := t.Waypoints[t.Leg]
	t.Phase, t.Elevator, t.Trip = "", "", ""
	if len(m.cars) == 0 || floor == w.Floor {
		m.newCmd(t, now)
		return nil
	}
	// pick keeps such robots off cross-floor tasks; this catches one whose
	// can_ride turned false after it was assigned.
	if r, ok := m.robotNow(t.Robot); ok && !r.CanRide {
		return errCannotRide
	}
	c := m.carFor(floor, w.Floor)
	if c == nil {
		return fmt.Errorf("no elevator serves floors %d and %d", floor, w.Floor)
	}
	t.Phase, t.Elevator, t.From = PhaseLobby, c.Cfg.ID, floor
	t.Hops++
	m.newCmd(t, now)
	return nil
}

// errCannotRide: the robot must ride for this leg but says it cannot.
var errCannotRide = errors.New("robot cannot ride elevators")

// failLeg handles a prepareLeg error: a robot that cannot ride only spends
// its own attempt (another robot may take the task); anything else is final.
func (m *Manager) failLeg(t *Task, now time.Time, err error) {
	if errors.Is(err, errCannotRide) {
		m.fail(t, now, fmt.Sprintf("robot %s cannot ride elevators", t.Robot), false)
		return
	}
	m.failForGood(t, now, err.Error())
}

// failForGood ends a task that no robot can do.
func (m *Manager) failForGood(t *Task, now time.Time, reason string) {
	m.leaveElevator(t, now, reason)
	t.Error, t.Finished, t.restoring = reason, now, false
	m.set(t, now, t.Robot, "failed", Failed, reason)
}

// target is where the current command drives to.
func (m *Manager) target(t *Task) (float64, float64) {
	if t.Phase == PhaseLobby {
		if c := m.cars[t.Elevator]; c != nil {
			p := c.Cfg.LobbyAt(t.From)
			return p.X, p.Y
		}
	}
	w := t.Waypoints[t.Leg]
	return w.X, w.Y
}

func (m *Manager) rideID(t *Task) string {
	return fmt.Sprintf("%s.%d.%d.%s.R%d", t.ID, t.Leg, t.Dispatches, m.d.Boot, t.Hops)
}

// enterWait queues the robot (at the lobby) for a trip.
func (m *Manager) enterWait(t *Task, now time.Time) {
	c := m.cars[t.Elevator]
	if c == nil {
		m.fail(t, now, "unknown elevator "+t.Elevator, false)
		return
	}
	t.Phase, t.Trip, t.CmdID, t.restoring = PhaseWait, "", "", false
	t.Hops++ // the ride it will get is a new command
	t.waitFrom, t.resends, t.probed = now, 0, false
	t.legDeadline, t.lastFollow, t.strayRide = time.Time{}, time.Time{}, time.Time{}
	c.Wait(t.Robot, t.From, t.Waypoints[t.Leg].Floor, m.rideID(t), now)
	m.save(t)
	m.d.Log.Info("waiting for elevator", "task", t.ID, "robot", t.Robot, "elevator", c.Cfg.ID, "floor", t.From, "dest", t.Waypoints[t.Leg].Floor)
}

// leaveElevator takes the robot out of its car's queue or trip (only before
// it has moved: a robot in the car stays in the trip) and clears the phase.
func (m *Manager) leaveElevator(t *Task, now time.Time, reason string) {
	c := m.cars[t.Elevator]
	var ns []elevator.Notice
	if c != nil {
		switch t.Phase {
		case PhaseWait:
			c.Unwait(t.Robot)
		case PhaseRide:
			_, ns = c.Drop(t.Robot, reason, now)
		}
	}
	t.Phase, t.Elevator, t.Trip = "", "", ""
	if c != nil {
		m.handleNotices(c, ns, now)
	}
}

// sendRide sends (or re-sends, same cmd_id) the ride command.
func (m *Manager) sendRide(t *Task, now time.Time) {
	t.lastSent = now
	cmd := Command{CmdID: t.CmdID, Type: "ride", ExpiresAt: m.expires(now)}
	if c := m.cars[t.Elevator]; c != nil {
		if p, ok := c.RidePlan(t.Robot); ok && p.Trip == t.Trip {
			cmd.Ride = &p
			cmd.X, cmd.Y = p.Lobby.X, p.Lobby.Y
		}
	}
	m.send(t.Robot, cmd)
}

// heldBy is the active task of robot.
func (m *Manager) heldBy(robot string) *Task {
	for _, t := range m.order {
		if t.active() && t.Robot == robot {
			return t
		}
	}
	return nil
}

// riding is robot's active task on trip, if its phase is still ride.
func (m *Manager) riding(robot, trip string) *Task {
	if t := m.heldBy(robot); t != nil && t.Phase == PhaseRide && t.Trip == trip {
		return t
	}
	return nil
}

// handleNotices does what a car asks for its riders' tasks.
func (m *Manager) handleNotices(c *elevator.Car, ns []elevator.Notice, now time.Time) {
	for _, n := range ns {
		switch n.Kind {
		case elevator.NoticePlanned:
			t := m.heldBy(n.Robot)
			if t == nil || t.Phase != PhaseWait || t.Elevator != c.Cfg.ID {
				_, more := c.Drop(n.Robot, "no task waiting for this car", now)
				m.handleNotices(c, more, now)
				continue
			}
			t.Phase, t.Trip = PhaseRide, n.Trip
			m.newCmd(t, now)
			if t.CmdID != n.CmdID {
				m.d.Log.Error("ride cmd_id mismatch", "task", t.ID, "task_cmd", t.CmdID, "car_cmd", n.CmdID)
			}
			m.save(t)
			m.sendRide(t, now)
		case elevator.NoticeAborted, elevator.NoticeLeft:
			t := m.riding(n.Robot, n.Trip)
			if t == nil {
				continue
			}
			m.sendCancel(m.addWatch(t, now), now) // drop the plan it accepted
			if n.Fault {
				t.Phase = "" // already out of the trip
				m.fail(t, now, n.Reason, false)
				continue
			}
			m.enterWait(t, now) // same lobby, next trip
		case elevator.NoticeStuck:
			t := m.riding(n.Robot, n.Trip)
			if t == nil {
				continue
			}
			t.Phase = ""
			m.fail(t, now, n.Reason, true) // the car keeps it; another robot takes the task
		case elevator.NoticeReleased:
			t := m.riding(n.Robot, n.Trip)
			if t == nil {
				continue
			}
			m.sendCancel(m.addWatch(t, now), now)
			t.Phase = ""
			m.requeue(t, now, n.Reason) // an operator's call, not the robot's failure
		case elevator.NoticeNoRide:
			t := m.heldBy(n.Robot)
			if t == nil || t.Phase != PhaseWait || t.Elevator != c.Cfg.ID {
				continue
			}
			// Already on the waypoint's floor: finish the leg with a goto.
			t.Phase, t.Elevator, t.Trip = "", "", ""
			m.newCmd(t, now)
			m.save(t)
			m.sendGoto(t, now)
		}
	}
}

// followRide follows a task in PhaseWait or PhaseRide.
func (m *Manager) followRide(t *Task, r *fleet.Robot, now time.Time) {
	c := m.cars[t.Elevator]
	if c == nil {
		m.fail(t, now, "unknown elevator "+t.Elevator, false)
		return
	}
	unreachable := r == nil || r.Conn == fleet.Stale || r.Conn == fleet.InElevator
	lost := ""
	switch {
	case unreachable:
	case r.Conn == fleet.Offline:
		lost = "robot " + r.Name + " went OFFLINE"
	case r.Mode == "ERROR":
		lost = "robot " + r.Name + " in ERROR: " + r.Error
	}
	if t.Phase == PhaseWait {
		if lost != "" {
			m.fail(t, now, lost+" at the elevator lobby", false)
		}
		return
	}
	rider, _, inTrip := c.Rider(t.Trip, t.Robot)
	switch {
	case inTrip && rider.Status == elevator.RiderLeft:
		// Out of the trip without a notice (e.g. reported out before boarding).
		m.sendCancel(m.addWatch(t, now), now)
		m.enterWait(t, now)
		return
	case inTrip && rider.Status == elevator.RiderWaiting:
		// Before its boarding turn: an ordinary command.
		switch {
		case unreachable:
			t.waitFrom = now
		case lost != "":
			m.fail(t, now, lost+" before boarding", true)
		case r.TaskID == t.CmdID:
			t.waitFrom = now
			if !rider.Accepted {
				m.confirm(t, now)
				m.handleNotices(c, c.Accepted(t.Robot, now), now)
			}
		case rider.Accepted:
		case now.Sub(t.waitFrom) >= m.cfg.AckTimeout && t.resends < m.cfg.MaxResends:
			t.resends++
			t.waitFrom = now
			m.sendRide(t, now) // the car aborts the trip at its AcceptTimeout
		}
		return
	case inTrip && !elevator.Accounted(rider.Status):
		t.waitFrom = now // in the car or at the door: the conductor's
		return
	}
	// Out of the car, or the trip is over: wait for its "done".
	switch {
	case unreachable:
		t.waitFrom = now
	case lost != "":
		m.fail(t, now, lost+" after the ride", true)
	case r.TaskID == t.CmdID && !inTrip:
		// It still holds a plan for a trip the car no longer runs (aborted
		// or forgotten across a restart) and has not acked it done. Give it
		// a little time to finish walking out, then make it drop the plan.
		if t.strayRide.IsZero() {
			t.strayRide = now
		}
		if now.Sub(t.strayRide) >= 2*m.cfg.AckTimeout {
			m.dropStrayRide(t, r, now)
		}
	case r.TaskID == t.CmdID:
		t.waitFrom = now
	case now.Sub(t.waitFrom) < m.cfg.AckTimeout:
	case t.resends >= m.cfg.MaxResends:
		m.fail(t, now, fmt.Sprintf("no done for %s after %d resends", t.CmdID, t.resends), true)
	default:
		t.resends++
		t.waitFrom = now
		m.sendRide(t, now) // the adapter repeats its last ack
	}
}

// rideAccepted handles "accepted" for the current ride command.
func (m *Manager) rideAccepted(t *Task, now time.Time) {
	m.confirm(t, now)
	c := m.cars[t.Elevator]
	if c != nil {
		if _, _, ok := c.Rider(t.Trip, t.Robot); ok {
			m.handleNotices(c, c.Accepted(t.Robot, now), now)
			return
		}
	}
	// A plan for a trip that is gone (it was planned before a restart and
	// not picked up again): the robot must drop it and queue again.
	m.d.Log.Warn("ride accepted for a trip that is gone; cancelling", "task", t.ID, "robot", t.Robot, "trip", t.Trip)
	m.sendCancel(m.addWatch(t, now), now)
	if c != nil {
		m.enterWait(t, now)
		return
	}
	m.requeue(t, now, "ride for an unknown elevator "+t.Elevator)
}

// dropStrayRide cancels a ride whose trip is gone and queues the robot
// again if it is still at the lobby it left from.
func (m *Manager) dropStrayRide(t *Task, r *fleet.Robot, now time.Time) {
	m.d.Log.Warn("robot holds a ride for a trip that is gone; cancelling", "task", t.ID, "robot", t.Robot, "trip", t.Trip)
	m.sendCancel(m.addWatch(t, now), now)
	if r.Floor == t.From && m.cars[t.Elevator] != nil {
		m.enterWait(t, now)
		return
	}
	m.requeue(t, now, "ride for a trip that is gone")
}

// rideDone handles the ride's "done": the robot is out of the car and its
// Wi-Fi is back.
func (m *Manager) rideDone(t *Task, a Ack, now time.Time) {
	m.confirm(t, now)
	w := t.Waypoints[t.Leg]
	floor, known := 0, false
	if a.Floor != nil {
		floor, known = *a.Floor, true
	} else if r, ok := m.robotNow(t.Robot); ok && r.HasPose {
		floor, known = r.Floor, true
	}
	c := m.cars[t.Elevator]
	if !known {
		floor = w.Floor
		if c != nil {
			if rd, _, ok := c.Rider(t.Trip, t.Robot); ok {
				floor = rd.Exit
			}
		}
	}
	trip := t.Trip
	if c != nil {
		m.handleNotices(c, c.Out(t.Robot, floor, now), now)
	}
	if floor == w.Floor {
		t.Phase, t.Elevator, t.Trip = "", "", ""
		m.newCmd(t, now)
		m.save(t)
		m.sendGoto(t, now)
		return
	}
	carID := t.Elevator
	if err := m.prepareLeg(t, floor, now); err != nil {
		m.failLeg(t, now, err)
		return
	}
	m.elevEvent(carID, "replanned", t.Robot, trip,
		fmt.Sprintf("%s came out at floor %d, not %d; new route from there (%s)", t.Robot, floor, w.Floor, t.CmdID))
	m.save(t)
	m.sendGoto(t, now)
}

// robotNow reads one robot from the registry. The manager -> registry lock
// order allows it.
func (m *Manager) robotNow(name string) (fleet.Robot, bool) {
	for _, r := range m.d.Robots.Snapshot() {
		if r.Name == name {
			return r, true
		}
	}
	return fleet.Robot{}, false
}

// criticalAtElevator applies the critical battery level to a robot in
// PhaseWait or PhaseRide: it gives the task back only while it has not
// started moving through the door. A robot riding finishes the ride; once
// out, its goto to the waypoint is handed back like any leg.
func (m *Manager) criticalAtElevator(t *Task, r fleet.Robot, now time.Time) {
	reason := fmt.Sprintf("robot %s battery critical (%.1f%%)", r.Name, r.Battery)
	switch t.Phase {
	case PhaseWait:
		m.requeue(t, now, reason)
	case PhaseRide:
		c := m.cars[t.Elevator]
		if c == nil {
			return
		}
		rider, stage, ok := c.Rider(t.Trip, t.Robot)
		if !ok || rider.Status != elevator.RiderWaiting || stage != elevator.StagePlanned {
			return // boarding has started: ride first
		}
		m.sendCancel(m.addWatch(t, now), now)
		m.requeue(t, now, reason)
	}
}

// restoreWait follows a PhaseWait task loaded after a restart.
func (m *Manager) restoreWait(t *Task, r *fleet.Robot, now time.Time) {
	c := m.cars[t.Elevator]
	if r != nil && r.Mode == "" {
		r = nil
	}
	if c != nil && r != nil && r.Conn == fleet.Online && r.TaskID == "" && r.Mode != "ERROR" && r.HasPose && r.Floor == t.From {
		t.restoring = false
		t.waitFrom = now
		c.Wait(t.Robot, t.From, t.Waypoints[t.Leg].Floor, m.rideID(t), now)
		m.d.Log.Info("waiting task confirmed after restart", "task", t.ID, "robot", t.Robot)
		return
	}
	if t.restoreUntil.IsZero() || r != nil && (r.Conn == fleet.Stale || r.Conn == fleet.InElevator) {
		return
	}
	if !now.Before(t.restoreUntil) {
		m.requeue(t, now, "not confirmed at the lobby by "+t.Robot+" after restart")
	}
}

// restoreRide follows a PhaseRide task loaded after a restart. The trip
// comes back from the retained elevator/<id>/trip (RestoreTrip); until then,
// or if it never comes, the usual restart grace applies.
func (m *Manager) restoreRide(t *Task, r *fleet.Robot, now time.Time) {
	if c := m.cars[t.Elevator]; c != nil {
		if _, _, ok := c.Rider(t.Trip, t.Robot); ok {
			t.restoring, t.waitFrom = false, now
			return
		}
	}
	if r != nil && r.Mode == "" {
		r = nil
	}
	if t.restoreUntil.IsZero() || r != nil && (r.Conn == fleet.Stale || r.Conn == fleet.InElevator) {
		return
	}
	if !now.Before(t.restoreUntil) {
		t.lastSent = now
		m.sendCancel(m.addWatch(t, now), now)
		m.requeue(t, now, "ride not confirmed by "+t.Robot+" after restart")
		return
	}
	// It may have finished the ride while we were down: the same cmd_id
	// makes the adapter repeat its last ack ("done").
	if r != nil && r.Conn == fleet.Online && !t.probed {
		t.probed = true
		m.sendRide(t, now)
	}
}

// tickCars runs every car's clocks and plans trips.
func (m *Manager) tickCars(byName map[string]*fleet.Robot, now time.Time) {
	lookup := func(name string) (fleet.Robot, bool) {
		if r := byName[name]; r != nil {
			return *r, true
		}
		return fleet.Robot{}, false
	}
	for _, id := range m.carIDs {
		c := m.cars[id]
		m.handleNotices(c, c.Tick(now, lookup), now)
	}
	m.releaseEvents(now)
}

// HandleElevatorState processes elevator/<id>/state.
func (m *Manager) HandleElevatorState(car string, s elevator.State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.cars[car]; c != nil {
		now := m.d.Now()
		m.handleNotices(c, c.State(s, now), now)
	}
}

// HandleZone processes elevator/<id>/zone.
func (m *Manager) HandleZone(car string, z elevator.Zone) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.cars[car]; c != nil {
		now := m.d.Now()
		m.handleNotices(c, c.Zone(z, now), now)
	}
}

// RestoreTrip adopts a trip from the retained elevator/<id>/trip if an
// earlier fleetd run published it and it is not over. fleetd's own messages
// coming back are ignored.
func (m *Manager) RestoreTrip(car string, t elevator.Trip) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cars[car]
	if c == nil || t.Boot == m.d.Boot || c.Ended(t.ID) {
		return // ours (an echo), or a trip that already ended here
	}
	now := m.d.Now()
	m.d.Log.Info("trip found from an earlier run", "elevator", car, "trip", t.ID, "stage", t.Stage)
	m.handleNotices(c, c.Restore(t, now), now)
}

// ErrNoTrip: the elevator has no trip to release.
var (
	ErrNoTrip     = errors.New("elevator has no trip")
	ErrNoElevator = errors.New("no such elevator")
)

// ReleaseElevator is the operator's force-release (POST
// /elevators/{id}/release): the car's trip ends now, robots that had not
// moved queue again, and the task of every robot not confirmed out goes to
// another robot (requeued without an attempt, the robot watched). Use it
// when a human has dealt with a robot stuck in the car.
func (m *Manager) ReleaseElevator(car string) (elevator.View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.cars[car]
	if c == nil {
		return elevator.View{}, ErrNoElevator
	}
	now := m.d.Now()
	ok, ns := c.ForceRelease(now, "operator")
	if !ok {
		return c.View(now), ErrNoTrip
	}
	m.handleNotices(c, ns, now)
	return c.View(now), nil
}

// RepublishTrips publishes every current trip again. fleetd calls it on
// each MQTT (re)connect.
func (m *Manager) RepublishTrips() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.carIDs {
		m.cars[id].Republish()
	}
}

// Elevators is GET /elevators.
func (m *Manager) Elevators() []elevator.View {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.d.Now()
	out := make([]elevator.View, 0, len(m.carIDs))
	for _, id := range m.carIDs {
		out = append(out, m.cars[id].View(now))
	}
	return out
}

func (m *Manager) elevEvent(car, kind, robot, trip, detail string) {
	now := m.d.Now()
	id := ""
	if t := m.heldBy(robot); robot != "" && t != nil {
		id = t.ID
	}
	if trip != "" {
		detail = trip + ": " + detail
	}
	m.d.Log.Info("elevator", "car", car, "event", kind, "robot", robot, "detail", detail)
	m.d.Emit(Event{Type: "elevator", Time: now, Task: id, Robot: robot, Kind: kind, Detail: detail, Elevator: car})
}

// Robot events (fleet/<robot>/event) arrive in a burst when a robot's link
// comes back after a blackout, possibly out of order. They are recorded in
// seq order: an event goes out when it is the next seq, or after it waited
// EventReorder (then a gap is skipped). A new boot starts a new sequence.
type eventBuf struct {
	boot    string
	last    uint64
	started bool
	pending []pendingEvent
}

type pendingEvent struct {
	ev elevator.RobotEvent
	at time.Time
}

// HandleRobotEvent records an event the robot sent on fleet/<robot>/event.
func (m *Manager) HandleRobotEvent(robot string, e elevator.RobotEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.d.Now()
	b := m.events[robot]
	if b == nil {
		b = &eventBuf{boot: e.Boot}
		m.events[robot] = b
	}
	if b.boot != e.Boot {
		m.flushEvents(robot, b, now, true) // the old run's leftovers first
		*b = eventBuf{boot: e.Boot}
	}
	if b.started && e.Seq <= b.last {
		return // duplicate or old
	}
	for _, p := range b.pending {
		if p.ev.Seq == e.Seq {
			return
		}
	}
	b.pending = append(b.pending, pendingEvent{e, now})
	sort.Slice(b.pending, func(i, j int) bool { return b.pending[i].ev.Seq < b.pending[j].ev.Seq })
	m.flushEvents(robot, b, now, false)
}

func (m *Manager) releaseEvents(now time.Time) {
	names := make([]string, 0, len(m.events))
	for n := range m.events {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m.flushEvents(n, m.events[n], now, false)
	}
}

func (m *Manager) flushEvents(robot string, b *eventBuf, now time.Time, all bool) {
	for len(b.pending) > 0 {
		p := b.pending[0]
		next := b.started && p.ev.Seq == b.last+1
		if !all && !next && now.Sub(p.at) < m.cfg.EventReorder {
			return
		}
		if b.started && p.ev.Seq > b.last+1 {
			m.d.Log.Warn("robot events missing", "robot", robot, "from", b.last+1, "to", p.ev.Seq-1)
		}
		b.pending = b.pending[1:]
		b.last, b.started = p.ev.Seq, true
		e := p.ev
		at := time.UnixMilli(int64(e.Stamp * 1000)).UTC().Format("15:04:05.000")
		detail := fmt.Sprintf("#%d %s at %s (robot clock), floor %d", e.Seq, e.Event, at, e.Floor)
		if e.Trip != "" {
			detail += ", trip " + e.Trip
		}
		if e.Detail != "" {
			detail += ": " + e.Detail
		}
		id := ""
		if t := m.heldBy(robot); t != nil {
			id = t.ID
		}
		m.d.Emit(Event{Type: "elevator", Time: now, Task: id, Robot: robot, Kind: "robot_event", Detail: detail})
	}
}
