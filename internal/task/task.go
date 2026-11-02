// Package task assigns tasks (lists of waypoints) to robots and follows each
// one through commands and acks until it is done, failed or cancelled.
package task

import (
	"errors"
	"time"

	"fleetlab/internal/fleet"
)

// Task states.
const (
	Queued    = "QUEUED"    // waiting for a robot
	Assigned  = "ASSIGNED"  // goto sent for the current leg, not yet accepted
	Running   = "RUNNING"   // robot accepted the current leg
	Done      = "DONE"      // every leg finished
	Failed    = "FAILED"    // gave up after too many failed attempts
	Cancelled = "CANCELLED" // cancelled through the API
)

// States lists every task state, for validating filters.
var States = []string{Queued, Assigned, Running, Done, Failed, Cancelled}

// Finished reports whether state is terminal.
func Finished(state string) bool {
	return state == Done || state == Failed || state == Cancelled
}

type Waypoint struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Floor int     `json:"floor"`
}

// Task is the persisted record of one task. Copies are handed out; the
// manager owns the originals.
type Task struct {
	ID        string     `json:"id"`
	Seq       uint64     `json:"seq"` // creation order; the queue is FIFO by Seq
	Waypoints []Waypoint `json:"waypoints"`
	State     string     `json:"state"`
	Robot     string     `json:"robot,omitempty"`
	Leg       int        `json:"leg"`      // index of the waypoint being driven to
	Attempts  int        `json:"attempts"` // failed attempts so far
	// Dispatches counts assignments to a robot. It is part of CmdID (with
	// the server's boot id) so a cmd_id is never reused, even after a restart
	// that lost the last writes.
	Dispatches int    `json:"dispatches"`
	CmdID      string `json:"cmd_id,omitempty"` // goto of the current leg
	Error      string `json:"error,omitempty"`  // why the last attempt failed
	// FailedOn lists robots that failed this task. They are skipped while
	// another eligible robot is free.
	FailedOn []string `json:"failed_on,omitempty"`

	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Started  time.Time `json:"started,omitzero"` // first accepted leg
	Finished time.Time `json:"finished,omitzero"`

	// Tracking that is not persisted.
	waitFrom     time.Time // last sign of life for CmdID (send, ack or robot report)
	resends      int       // resends of CmdID since the last sign of life
	lastSent     time.Time // last time the goto for CmdID was sent
	legDeadline  time.Time // the leg times out after this (zero: not set yet)
	lastFollow   time.Time // previous follow, to pause the leg clock while unreachable
	restoring    bool      // loaded after a restart, robot has not confirmed the leg
	restoreUntil time.Time // grace end; zero until the first MQTT connection
	probed       bool      // restored leg was re-sent once to ask the robot
	handBack     bool      // battery critical: robot cancelled, requeue once it stopped
}

func (t *Task) active() bool { return t.State == Assigned || t.State == Running }

// Command is published to fleet/<robot>/cmd.
type Command struct {
	CmdID     string  `json:"cmd_id"`
	Type      string  `json:"type"` // goto, cancel, dock
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Yaw       float64 `json:"yaw"`
	ExpiresAt float64 `json:"expires_at"` // unix seconds
}

// Ack arrives on fleet/<robot>/cmd_ack.
type Ack struct {
	CmdID   string `json:"cmd_id"`
	Status  string `json:"status"` // accepted, done, failed, cancelled
	Message string `json:"message"`
}

// Event reports a task state change or a battery policy step.
type Event struct {
	Type  string // task, battery
	Time  time.Time
	Task  string
	Robot string
	// task: assigned, running, leg_done, done, requeued, failed, cancelled
	// battery: low, dock_sent, docking, charged, dock_failed
	Kind   string
	From   string // task state before
	To     string // task state after
	Detail string
}

// RobotSource gives the current view of every robot (fleet.Registry).
type RobotSource interface {
	Snapshot() []fleet.Robot
}

// Sender delivers a command to a robot. It must not block or call back into
// the manager: it is called with the manager's lock held.
type Sender interface {
	Send(robot string, cmd Command) error
}

// Store persists tasks. The manager calls it outside its lock (from Submit
// and Flush), so it must be safe for concurrent use.
type Store interface {
	Save(t Task) error
	Load() ([]Task, error)
}

var (
	ErrNotFound = errors.New("task not found")
	ErrFinished = errors.New("task already finished")
	ErrInvalid  = errors.New("invalid task")
)
