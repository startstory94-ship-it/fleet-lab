// Package sim is an in-process fleet for integration tests: robots that take
// commands from the task manager, drive in straight lines on a fake clock,
// feed state into a real fleet.Registry and ack like the ROS adapter.
//
// Robots drain their battery per metre driven. A dock command sends a robot
// back to where it was added; there it is CHARGING until 100 %, then IDLE,
// like mock_fleet.py.
//
// No MQTT, no ROS. Commands and acks can be delayed with random jitter, so
// they arrive late and out of order (a cancel can overtake its goto). A link
// outage holds commands and acks until it ends, like a robot whose network
// dropped while it kept driving.
package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

// Robot is one simulated robot. Read it with Fleet.Robot.
type Robot struct {
	Name    string
	X, Y    float64
	Floor   int
	Mode    string // IDLE, MOVING, DOCKING, CHARGING, ERROR
	Error   string
	Battery float64
	Goal    string // cmd_id of the goto being driven, "" when none
	// Started lists every goto this robot began driving.
	Started []string
	// IdleCancels counts cancels that found nothing to cancel.
	IdleCancels int
	// Charges counts arrivals at the dock.
	Charges int

	gx, gy   float64
	dockX    float64
	dockY    float64
	silent   bool
	inbox    []inCmd
	outbox   []outAck
	book     map[string]*task.Ack // cmd_id -> last ack (nil: seen, no ack yet)
	reports  int
	stateSeq uint64
	poseSeq  uint64

	// Elevator ride (elevator.go).
	ride         *elevator.RidePlan
	rideCmd      string
	rideState    string // waiting, boarding, inside, exiting, out
	results      map[int]elevator.Zone
	moveN        int
	moveAct      string
	moveFloor    int
	moveLeft     float64 // seconds left through the door zone
	linkAt       time.Time
	failExits    int
	wrongFloor   *int
	extraSilence time.Duration
	events       []elevator.RobotEvent // queued until the link is up
	evSeq        uint64
	zoneSeq      uint64
	exitLobby    elevator.Point
	dropTurns    int
	dropZones    int
}

type inCmd struct {
	at  time.Time
	seq int // send order, to keep equal times stable
	cmd task.Command
}

type outAck struct {
	at  time.Time
	ack task.Ack
}

// Fleet implements task.Sender.
type Fleet struct {
	mu       sync.Mutex
	reg      *fleet.Registry
	now      func() time.Time
	speed    float64
	robots   map[string]*Robot
	names    []string
	rng      *rand.Rand
	delay    time.Duration // every command and ack takes delay + [0, jitter)
	jitter   time.Duration
	sent     int
	dropAcks float64
	dropped  int
	onAck    func(robot string, a task.Ack)
	drain    float64 // % per metre driven
	charge   float64 // % per second on the dock

	cars      map[string]*Car
	carNames  []string
	trips     map[string]elevator.Trip
	cleared   map[string]int
	elevIn    []elevMsg // to cars and robots
	elevOut   []elevMsg // to the server
	elevDelay time.Duration
	doorMove  time.Duration
	linkBack  time.Duration
	onState   func(car string, s elevator.State)
	onZone    func(car string, z elevator.Zone)
	onEvent   func(robot string, e elevator.RobotEvent)
}

// New creates an empty fleet. Robots move at speed m/s.
func New(reg *fleet.Registry, now func() time.Time, speed float64, seed uint64) *Fleet {
	return &Fleet{reg: reg, now: now, speed: speed, robots: make(map[string]*Robot),
		rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), onAck: func(string, task.Ack) {},
		charge: 1, cars: map[string]*Car{}, trips: map[string]elevator.Trip{}, cleared: map[string]int{},
		elevDelay: 50 * time.Millisecond, doorMove: 2 * time.Second, linkBack: time.Second,
		onState: func(string, elevator.State) {}, onZone: func(string, elevator.Zone) {},
		onEvent: func(string, elevator.RobotEvent) {}}
}

// OnAck sets where acks go (task.Manager.HandleAck).
func (f *Fleet) OnAck(h func(robot string, a task.Ack)) { f.onAck = h }

// Add places an IDLE robot on floor 1, at its dock, with 90 % battery. Its
// first report is a pose only, as when an adapter starts: the server sees the
// robot before its state.
func (f *Fleet) Add(name string, x, y float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.robots[name] = &Robot{Name: name, X: x, Y: y, Floor: 1, Mode: "IDLE", Battery: 90,
		dockX: x, dockY: y, book: make(map[string]*task.Ack)}
	f.names = append(f.names, name)
	sort.Strings(f.names)
}

// SetBatteryModel sets the drain in % per metre driven (default 0) and the
// charge rate in % per second on the dock (default 1).
func (f *Fleet) SetBatteryModel(drainPerM, chargeRate float64) {
	f.mu.Lock()
	f.drain, f.charge = drainPerM, chargeRate
	f.mu.Unlock()
}

// SetBattery sets one robot's battery level in %.
func (f *Fleet) SetBattery(name string, pct float64) {
	f.mu.Lock()
	f.robots[name].Battery = pct
	f.mu.Unlock()
}

// SetDelay makes every command and ack take delay plus a random part of
// jitter. Jitter larger than the gap between two commands reorders them.
func (f *Fleet) SetDelay(delay, jitter time.Duration) {
	f.mu.Lock()
	f.delay, f.jitter = delay, jitter
	f.mu.Unlock()
}

// DropAcks makes every robot lose each ack with probability p (the robot
// still remembers it and repeats it for a duplicate command).
func (f *Fleet) DropAcks(p float64) {
	f.mu.Lock()
	f.dropAcks = p
	f.mu.Unlock()
}

// Dropped counts acks lost so far.
func (f *Fleet) Dropped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}

// Fault puts the robot in ERROR; a goto in progress ends as failed.
func (f *Fleet) Fault(name, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fault(f.robots[name], reason)
}

func (f *Fleet) fault(r *Robot, reason string) {
	if r.Goal != "" {
		f.finish(r, "failed", "robot in ERROR: "+reason)
	}
	r.Mode, r.Error = "ERROR", reason
}

// SetSilent cuts (true) or restores (false) the robot's link. While silent it
// keeps driving but sends no state and no acks and receives no commands.
func (f *Fleet) SetSilent(name string, silent bool) {
	f.mu.Lock()
	f.robots[name].silent = silent
	f.mu.Unlock()
}

// Robot returns a copy of one robot.
func (f *Fleet) Robot(name string) Robot {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := *f.robots[name]
	r.Started = append([]string(nil), r.Started...)
	return r
}

// Driving maps robot name -> cmd_id of the goto it is driving.
func (f *Fleet) Driving() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string)
	for n, r := range f.robots {
		if r.Goal != "" {
			out[n] = r.Goal
		}
	}
	return out
}

// Silent reports whether the robot's link is down.
func (f *Fleet) Silent(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.robots[name].silent
}

// Send queues a command for the robot. It never blocks and never calls back.
func (f *Fleet) Send(robot string, c task.Command) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.robots[robot]
	if !ok {
		return fmt.Errorf("no robot %q", robot)
	}
	f.sent++
	r.inbox = append(r.inbox, inCmd{at: f.arrival(), seq: f.sent, cmd: c})
	return nil
}

// arrival is when something sent now reaches the other side.
func (f *Fleet) arrival() time.Time {
	d := f.delay
	if f.jitter > 0 {
		d += time.Duration(f.rng.Int64N(int64(f.jitter)))
	}
	return f.now().Add(d)
}

type delivery struct {
	robot string
	ack   task.Ack
}

// Step advances every robot by dt: handle commands that have arrived, drive,
// report state and deliver acks that have arrived. Call it after moving the
// fake clock.
func (f *Fleet) Step(dt time.Duration) {
	f.mu.Lock()
	now := f.now()
	var out []delivery
	for _, name := range f.names {
		r := f.robots[name]
		if !r.silent {
			sort.Slice(r.inbox, func(i, j int) bool {
				a, b := r.inbox[i], r.inbox[j]
				return a.at.Before(b.at) || a.at.Equal(b.at) && a.seq < b.seq
			})
			n := 0
			for n < len(r.inbox) && !r.inbox[n].at.After(now) {
				f.handle(r, r.inbox[n].cmd, now)
				n++
			}
			r.inbox = append(r.inbox[:0], r.inbox[n:]...)
		}
		f.drive(r, dt.Seconds(), now)
		if r.silent {
			continue
		}
		f.report(r)
		keep := r.outbox[:0]
		for _, a := range r.outbox {
			switch {
			case a.at.After(now):
				keep = append(keep, a)
			case f.rng.Float64() < f.dropAcks:
				f.dropped++
			default:
				out = append(out, delivery{r.Name, a.ack})
			}
		}
		r.outbox = keep
	}
	elev := f.stepElevators(dt, now)
	type evDelivery struct {
		robot string
		ev    elevator.RobotEvent
	}
	var evs []evDelivery
	for _, name := range f.names {
		r := f.robots[name]
		if r.silent || len(r.events) == 0 {
			continue
		}
		// Newest first: after a blackout the server must put them in order.
		for i := len(r.events) - 1; i >= 0; i-- {
			evs = append(evs, evDelivery{name, r.events[i]})
		}
		r.events = r.events[:0]
	}
	f.mu.Unlock()

	// Outside the lock: the manager may send commands back synchronously.
	for _, m := range elev {
		switch {
		case m.st != nil:
			f.onState(m.car, *m.st)
		case m.zone != nil:
			f.onZone(m.car, *m.zone)
		}
	}
	for _, e := range evs {
		f.onEvent(e.robot, e.ev)
	}
	for _, d := range out {
		f.onAck(d.robot, d.ack)
	}
}

// handle mirrors fleet_adapter.handle_cmd plus the mock robot.
func (f *Fleet) handle(r *Robot, c task.Command, now time.Time) {
	if last, seen := r.book[c.CmdID]; seen {
		if last != nil {
			r.outbox = append(r.outbox, outAck{f.arrival(), *last}) // not run again; repeat the last ack
		}
		return
	}
	r.book[c.CmdID] = nil
	if c.ExpiresAt > 0 && float64(now.UnixMilli())/1000 > c.ExpiresAt {
		f.ack(r, c.CmdID, "failed", "expired")
		return
	}
	switch c.Type {
	case "goto":
		if r.ride != nil {
			f.ack(r, c.CmdID, "failed", "holds ride "+r.rideCmd)
			return
		}
		if r.Goal != "" {
			f.finish(r, "cancelled", "preempted by "+c.CmdID)
		}
		f.ack(r, c.CmdID, "accepted", "")
		if r.Mode == "ERROR" { // the robot ignores the goal; the adapter then fails it
			f.ack(r, c.CmdID, "failed", "robot in ERROR: "+r.Error)
			return
		}
		r.Goal, r.gx, r.gy, r.Mode = c.CmdID, c.X, c.Y, "MOVING"
		r.Started = append(r.Started, c.CmdID)
	case "ride":
		f.rideCmd(r, c, now)
	case "cancel":
		if r.ride != nil {
			f.cancelRide(r, c)
			return
		}
		if r.Goal == "" && r.Mode != "DOCKING" {
			r.IdleCancels++
			f.ack(r, c.CmdID, "failed", "nothing to cancel")
			return
		}
		if r.Goal != "" {
			f.finish(r, "cancelled", "stopped before the goal")
		}
		r.Mode = "IDLE"
		f.ack(r, c.CmdID, "accepted", "")
	case "dock":
		// The adapter ends a tracked goto as cancelled once the dock is accepted.
		if r.ride != nil {
			f.ack(r, c.CmdID, "failed", "holds ride "+r.rideCmd)
			return
		}
		if r.Mode == "ERROR" {
			f.ack(r, c.CmdID, "failed", "in ERROR ("+r.Error+")")
			return
		}
		if r.Goal != "" {
			f.finish(r, "cancelled", "dock requested by "+c.CmdID)
		}
		r.Mode = "DOCKING"
		f.ack(r, c.CmdID, "accepted", "docking")
	case "reset_error":
		if r.Mode != "ERROR" {
			f.ack(r, c.CmdID, "failed", "not in ERROR")
			return
		}
		r.Mode, r.Error = "IDLE", ""
		f.ack(r, c.CmdID, "accepted", "")
	default:
		f.ack(r, c.CmdID, "failed", "unsupported in sim: "+c.Type)
	}
}

func (f *Fleet) drive(r *Robot, dt float64, now time.Time) {
	switch {
	case r.Mode == "CHARGING":
		r.Battery = min(100, r.Battery+f.charge*dt)
		if r.Battery >= 100 {
			r.Mode = "IDLE"
		}
	case r.Mode == "DOCKING":
		if f.move(r, r.dockX, r.dockY, dt) {
			r.Mode = "CHARGING"
			r.Charges++
		}
	case r.ride != nil:
		return // at the lobby or in the car: elevator.go moves it
	case r.Goal != "" && r.Mode != "ERROR":
		if f.move(r, r.gx, r.gy, dt) {
			r.Mode = "IDLE"
			f.finish(r, "done", "")
		}
	default:
		return
	}
	if r.Battery <= 0 && (r.Mode == "MOVING" || r.Mode == "DOCKING") {
		f.fault(r, "battery_empty")
	}
}

// move drives r toward (x, y) for dt seconds and reports arrival.
func (f *Fleet) move(r *Robot, x, y, dt float64) bool {
	dx, dy := x-r.X, y-r.Y
	dist := math.Hypot(dx, dy)
	step := min(dist, f.speed*dt)
	r.Battery = max(0, r.Battery-f.drain*step)
	if dist <= f.speed*dt {
		r.X, r.Y = x, y
		return true
	}
	r.X += step * dx / dist
	r.Y += step * dy / dist
	return false
}

// finish ends the current goto with a final ack.
func (f *Fleet) finish(r *Robot, status, msg string) {
	f.ack(r, r.Goal, status, msg)
	r.Goal = ""
}

func (f *Fleet) ack(r *Robot, cmdID, status, msg string) {
	a := task.Ack{CmdID: cmdID, Status: status, Message: msg}
	r.book[cmdID] = &a
	r.outbox = append(r.outbox, outAck{f.arrival(), a})
}

func (f *Fleet) report(r *Robot) {
	r.reports++
	r.poseSeq++
	f.reg.ApplyPose(r.Name, fleet.PoseMsg{Boot: "sim", Seq: r.poseSeq, Host: "sim", Frame: "map",
		Floor: r.Floor, X: r.X, Y: r.Y})
	if r.reports == 1 {
		return // the first state comes one step after the first pose
	}
	r.stateSeq++
	f.reg.ApplyState(r.Name, fleet.StateMsg{Boot: "sim", Seq: r.stateSeq, Host: "sim",
		Mode: r.Mode, Battery: &r.Battery, TaskID: r.Goal, Error: r.Error})
}
