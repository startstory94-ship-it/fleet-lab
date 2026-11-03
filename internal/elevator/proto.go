package elevator

import (
	"strings"
	"time"
)

// MQTT topics of the elevator channel. It keeps working inside the car,
// where the robot's Wi-Fi (fleet/*) does not. See docs/ELEVATOR.md.
const (
	TopicCmd   = "elevator/%s/cmd"   // server -> car: Cmd
	TopicState = "elevator/%s/state" // car -> all: State (retained)
	TopicTrip  = "elevator/%s/trip"  // server -> robots, a restarted fleetd: Trip (retained, cleared at the end)
	TopicTurn  = "elevator/%s/turn"  // server -> robots: Turn
	TopicZone  = "elevator/%s/zone"  // robot -> server: Zone

	// Subscriptions of fleetd.
	SubState = "elevator/+/state"
	SubTrip  = "elevator/+/trip"
	SubZone  = "elevator/+/zone"
	// Robot events queued during a blackout arrive on the fleet link.
	SubRobotEvent = "fleet/+/event"
)

// SplitTopic returns the car id and the last part of elevator/<id>/<kind>.
func SplitTopic(topic string) (car, kind string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) != 3 || parts[0] != "elevator" || parts[1] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// Elevator commands (Cmd.Cmd).
const (
	CmdCall    = "call"    // reserve for trip, go to floor, open, hold
	CmdGoto    = "goto"    // close, go to floor, open, hold
	CmdHold    = "hold"    // keep the door open hold_s more seconds
	CmdRelease = "release" // stop holding, close, end the reservation
)

// Cmd is published on elevator/<id>/cmd.
type Cmd struct {
	CmdID string  `json:"cmd_id"` // the car applies each cmd_id once
	Cmd   string  `json:"cmd"`
	Trip  string  `json:"trip"`
	Floor int     `json:"floor"`            // call, goto
	HoldS float64 `json:"hold_s,omitempty"` // call, goto, hold
}

// Door states.
const (
	DoorOpen    = "open"
	DoorClosed  = "closed"
	DoorOpening = "opening"
	DoorClosing = "closing"
)

// State is published by the car on elevator/<id>/state when it changes and
// every second, retained.
type State struct {
	Boot   string  `json:"boot"` // car controller run; seq restarts with it
	Seq    uint64  `json:"seq"`
	Stamp  float64 `json:"stamp"`  // car clock, unix seconds (informational)
	Floor  int     `json:"floor"`  // current (or last passed) floor
	Dir    string  `json:"dir"`    // up, down, idle
	Door   string  `json:"door"`   // open, closed, opening, closing
	Moving bool    `json:"moving"` // between floors
	Hold   bool    `json:"hold"`   // door held open on request
	Trip   string  `json:"trip"`   // reservation, "" when free
	Load   int     `json:"load"`   // robots the car senses inside (informational)
	CmdID  string  `json:"cmd_id"` // last command applied
	// DoorZone is how many robots (or anything) are in the door zone now, from
	// the car's door sensors (the mock: robots between their zone "enter" and
	// "clear"/"failed"). The conductor gives the next turn only after a state
	// with door_zone 0 that came after the last turn ended.
	DoorZone int `json:"door_zone"`
}

// Trip stages.
const (
	StagePlanned  = "planned"  // ride commands sent, car called; nobody moves yet
	StageBoarding = "boarding" // door open at the origin, robots board one at a time
	StageTravel   = "travel"   // car on its way to Trip.Floor
	StageExiting  = "exiting"  // door open at Trip.Floor, robots exit one at a time
	StageBlocked  = "blocked"  // every stop done, a robot is (maybe) still inside
	StageDone     = "done"     // car released and free
	StageAborted  = "aborted"  // ended before anyone boarded
)

// Rider statuses.
const (
	RiderWaiting  = "waiting"   // at the lobby, before its boarding turn
	RiderBoarding = "boarding"  // has the turn to board
	RiderInside   = "inside"    // boarded (zone clear)
	RiderExiting  = "exiting"   // has the turn to exit
	RiderOut      = "out"       // exited (zone clear) or seen back on the fleet link
	RiderLeft     = "left"      // never boarded; stays outside
	RiderUnsureIn = "unsure_in" // boarding turns timed out: treated as inside until seen elsewhere
	RiderStuck    = "stuck"     // could not exit at the last stop: needs a human (retried every StuckRetry)
)

// Accounted reports whether the rider is known to be outside the car.
func Accounted(status string) bool { return status == RiderOut || status == RiderLeft }

// Rider is one robot of a trip.
type Rider struct {
	Robot    string  `json:"robot"`
	CmdID    string  `json:"cmd_id"` // its ride command
	Dest     int     `json:"dest"`   // planned exit floor
	Exit     int     `json:"exit"`   // current exit floor (moves on when carried)
	Board    int     `json:"board"`  // boarding order
	Slot     int     `json:"slot"`   // 0 is the door
	Status   string  `json:"status"`
	Accepted bool    `json:"accepted"`
	Tries    int     `json:"tries"` // failed or unanswered turns at the current stop
	OutFloor int     `json:"out_floor,omitempty"`
	SilenceS float64 `json:"silence_s"`
	Notified bool    `json:"notified,omitempty"` // its task was already handed on (stuck, lease)
}

// Turn gives one robot the door. Published on elevator/<id>/turn.
type Turn struct {
	Trip     string    `json:"trip"`
	N        int       `json:"n"` // increases by one per turn of the trip
	Robot    string    `json:"robot"`
	Action   string    `json:"action"` // board, exit
	Floor    int       `json:"floor"`
	Lobby    Point     `json:"lobby"` // where to come out (exit) on this floor
	TimeoutS float64   `json:"timeout_s"`
	Deadline time.Time `json:"deadline"` // server clock; for the dashboard
}

// Turn actions.
const (
	ActBoard = "board"
	ActExit  = "exit"
)

// Trip is the conductor's whole view of one ride. It is published retained
// on elevator/<id>/trip at every step, which tells robots the stage and lets
// a restarted fleetd pick the trip up again.
type Trip struct {
	ID      string    `json:"trip"`
	Boot    string    `json:"boot"` // fleetd run that runs it (set again when a run adopts it)
	Car     string    `json:"elevator"`
	Stage   string    `json:"stage"`
	Origin  int       `json:"origin"`
	Dir     int       `json:"dir"`
	Stops   []int     `json:"stops"` // planned stops in travel order
	Floor   int       `json:"floor"` // current gate floor or travel target
	Riders  []Rider   `json:"riders"`
	Turn    *Turn     `json:"turn,omitempty"`
	N       int       `json:"n"`
	Planned time.Time `json:"planned"`
	Lease   time.Time `json:"lease"`
	Detail  string    `json:"detail,omitempty"` // why it was aborted or blocked
}

func (t *Trip) rider(robot string) *Rider {
	for i := range t.Riders {
		if t.Riders[i].Robot == robot {
			return &t.Riders[i]
		}
	}
	return nil
}

// Clone returns a deep copy.
func (t Trip) Clone() Trip {
	c := t
	c.Stops = append([]int(nil), t.Stops...)
	c.Riders = append([]Rider(nil), t.Riders...)
	if t.Turn != nil {
		tt := *t.Turn
		c.Turn = &tt
	}
	return c
}

// Zone events.
const (
	ZoneClear  = "clear"  // the robot is through the door zone
	ZoneFailed = "failed" // it could not get through; back at its slot (exit) or the lobby (board)
	ZoneEnter  = "enter"  // it starts into the door zone (for the car; the conductor ignores it)
)

// Zone is published by a robot on elevator/<id>/zone: "enter" when it starts
// into the door zone on its turn, "clear" or "failed" when the turn ends.
type Zone struct {
	Trip   string  `json:"trip"`
	N      int     `json:"n"` // the turn it answers
	Robot  string  `json:"robot"`
	Action string  `json:"action"` // board, exit
	Event  string  `json:"event"`  // enter, clear, failed
	Floor  int     `json:"floor"`
	Reason string  `json:"reason,omitempty"`
	Seq    uint64  `json:"seq"`   // the robot's event seq
	Stamp  float64 `json:"stamp"` // robot clock
}

// RobotEvent is published on fleet/<robot>/event. Events from the blackout
// are queued by the robot and sent when its link is back, so the server
// orders them by seq.
type RobotEvent struct {
	Boot   string  `json:"boot"`
	Seq    uint64  `json:"seq"`
	Stamp  float64 `json:"stamp"` // robot clock, unix seconds
	Event  string  `json:"event"` // waiting, boarded, exited, exit_failed, ...
	Trip   string  `json:"trip,omitempty"`
	CmdID  string  `json:"cmd_id,omitempty"`
	Floor  int     `json:"floor"`
	Detail string  `json:"detail,omitempty"`
}

// RidePlan is the robot's part of a trip, carried by the ride command. The
// robot needs nothing else from the fleet link until it is out of the car.
type RidePlan struct {
	Trip         string   `json:"trip"`
	Elevator     string   `json:"elevator"`
	Origin       int      `json:"origin"`
	Direction    string   `json:"direction"` // up, down
	ExitFloor    int      `json:"exit_floor"`
	Board        int      `json:"board"` // boarding order, 0 first
	Slot         int      `json:"slot"`  // 0 is the door
	Boarding     []string `json:"boarding"`
	Stops        []int    `json:"stops"`
	SilenceS     float64  `json:"silence_s"`
	TurnTimeoutS float64  `json:"turn_timeout_s"`
	Lobby        Point    `json:"lobby"`      // where it waits on the origin
	ExitLobby    Point    `json:"exit_lobby"` // where it comes out on exit_floor
}

// Link publishes on the elevator channel. It must not block or call back:
// it is called with the task manager's lock held.
type Link interface {
	Command(car string, c Cmd) error
	PublishTrip(car string, t Trip) error
	PublishTurn(car string, t Turn) error
	// ClearTrip empties the retained trip (a zero-length retained publish)
	// once the final stage was published.
	ClearTrip(car string) error
}

func dirName(d int) string {
	if d < 0 {
		return "down"
	}
	return "up"
}
