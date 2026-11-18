// Package fleet keeps the server's view of every robot.
package fleet

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// PoseMsg is the payload of fleet/<robot>/pose.
type PoseMsg struct {
	Boot  string  `json:"boot"`
	Seq   uint64  `json:"seq"`
	Stamp float64 `json:"stamp"`
	Host  string  `json:"host"`
	Frame string  `json:"frame"`
	Floor int     `json:"floor"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Yaw   float64 `json:"yaw"`
	V     float64 `json:"v"`
}

// StateMsg is the payload of fleet/<robot>/state.
type StateMsg struct {
	Boot    string   `json:"boot"`
	Seq     uint64   `json:"seq"`
	Stamp   float64  `json:"stamp"`
	Host    string   `json:"host"`
	Mode    string   `json:"mode"`
	Battery *float64 `json:"battery"` // %; null or missing when the robot does not know
	TaskID  string   `json:"task_id"`
	Error   string   `json:"error"`

	// SilenceS announces a planned blackout (e.g. riding an elevator) of up to
	// this many seconds, counted from when the server receives the message.
	SilenceS float64 `json:"silence_s"`

	// CanRide says whether the robot can ride elevators. Missing means yes, so
	// the ROS adapter (which never sends it) keeps working.
	CanRide *bool `json:"can_ride,omitempty"`
}

// Connection states, judged by the server from what it receives.
const (
	Online     = "ONLINE"      // messages arriving normally
	Stale      = "STALE"       // short gap (Wi-Fi roaming); keep tasks, assign nothing new
	Offline    = "OFFLINE"     // long gap or adapter gone; tasks may be reassigned
	InElevator = "IN_ELEVATOR" // announced blackout; silence is expected
)

// Event is a change worth telling operators about.
type Event struct {
	// Seq increases by one per event, in the order the changes happened.
	// Listeners can use it to spot gaps (dropped events) or reordering.
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Robot string    `json:"robot"`
	Type  string    `json:"type"` // seen, conn, mode, error, elevator_overdue, adapter, task, battery, elevator
	Task  string    `json:"task,omitempty"`
	// Elevator is the car of an elevator event (type "elevator").
	Elevator string `json:"elevator,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Config holds the liveness thresholds.
type Config struct {
	StaleAfter   time.Duration // no message for this long -> STALE
	OfflineAfter time.Duration // no message for this long -> OFFLINE
	RecoverMsgs  int           // consecutive messages needed to leave STALE/OFFLINE
}

func DefaultConfig() Config {
	return Config{StaleAfter: 2 * time.Second, OfflineAfter: 20 * time.Second, RecoverMsgs: 3}
}

// Robot is a snapshot of one robot as the server last saw it.
type Robot struct {
	Name    string  `json:"name"`
	Host    string  `json:"host"`
	Conn    string  `json:"conn"`
	Frame   string  `json:"frame"`
	Floor   int     `json:"floor"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Yaw     float64 `json:"yaw"`
	V       float64 `json:"v"`
	Mode    string  `json:"mode"`
	Battery float64 `json:"battery"`
	TaskID  string  `json:"task_id"`
	Error   string  `json:"error"`
	// HasPose is set by the first pose. Before it, Floor, X and Y are zero
	// values, not a position.
	HasPose bool `json:"has_pose"`
	// BatteryKnown is false when the last state carried no battery; Battery
	// is then 0 and means nothing.
	BatteryKnown bool `json:"battery_known"`
	// CanRide is false only if the robot said so in its state. True until the
	// first state: a robot known by its pose alone is assumed to ride.
	CanRide bool `json:"can_ride"`

	// LastSeen is when the server last accepted any message from this robot.
	// It uses the server clock, so robot clock skew cannot affect liveness.
	LastSeen time.Time `json:"last_seen"`
	// SilentUntil is set while IN_ELEVATOR: the end of the announced blackout.
	SilentUntil time.Time `json:"silent_until,omitzero"`

	poseBoot, stateBoot string
	poseSeq, stateSeq   uint64
	okStreak            int
}

// Registry is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	robots map[string]*Robot
	cfg    Config
	now    func() time.Time
	emit   func(Event)
	evSeq  uint64

	rejected atomic.Uint64 // messages Ingest could not parse or Check refused
}

// Rejected counts messages refused by Ingest (bad JSON or values out of range).
func (g *Registry) Rejected() uint64 { return g.rejected.Load() }

// NewRegistry creates a registry. emit is called with the registry lock held,
// so events reach it in the order the changes happened; it must not block or
// call back into the registry. It may be nil.
func NewRegistry(cfg Config, emit func(Event)) *Registry {
	if emit == nil {
		emit = func(Event) {}
	}
	return &Registry{robots: make(map[string]*Robot), cfg: cfg, now: time.Now, emit: emit}
}

// SetClock replaces the clock used for liveness, for tests and simulations.
// Call it before the registry is used.
func (g *Registry) SetClock(now func() time.Time) { g.now = now }

// get returns the robot, creating it (with a "seen" event) on first contact.
func (g *Registry) get(name string, now time.Time, evs []Event) (*Robot, []Event) {
	r, ok := g.robots[name]
	if !ok {
		r = &Robot{Name: name, Conn: Online, CanRide: true}
		g.robots[name] = r
		evs = append(evs, Event{Time: now, Robot: name, Type: "seen"})
	}
	return r, evs
}

// flush numbers and emits events. Call it before releasing the lock.
func (g *Registry) flush(evs []Event) {
	for _, e := range evs {
		g.evSeq++
		e.Seq = g.evSeq
		g.emit(e)
	}
}

func setConn(r *Robot, to, detail string, now time.Time, evs []Event) []Event {
	if r.Conn == to {
		return evs
	}
	evs = append(evs, Event{Time: now, Robot: r.Name, Type: "conn", From: r.Conn, To: to, Detail: detail})
	r.Conn = to
	return evs
}

// fresh reports whether (boot, seq) is newer than what was last accepted.
// A different boot means the sender restarted and its seq began again.
func fresh(lastBoot string, lastSeq uint64, boot string, seq uint64) bool {
	return boot != lastBoot || seq > lastSeq
}

// heard updates liveness for an accepted message.
func (g *Registry) heard(r *Robot, now time.Time, evs []Event) []Event {
	r.LastSeen = now
	switch r.Conn {
	case Online, InElevator:
		// IN_ELEVATOR ends only on a state message without silence_s;
		// stray messages sent just before the blackout must not end it.
	default: // Stale, Offline: require a short streak before trusting the link again
		r.okStreak++
		if r.okStreak >= g.cfg.RecoverMsgs {
			r.okStreak = 0
			evs = setConn(r, Online, "recovered", now, evs)
		}
	}
	return evs
}

// ApplyPose records a pose message. It returns false if the message is stale
// or a duplicate and was ignored.
func (g *Registry) ApplyPose(name string, m PoseMsg) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	r, evs := g.get(name, now, nil)
	if !fresh(r.poseBoot, r.poseSeq, m.Boot, m.Seq) {
		return false
	}
	r.poseBoot, r.poseSeq = m.Boot, m.Seq
	if m.Host != "" {
		r.Host = m.Host
	}
	r.Frame, r.Floor, r.HasPose = m.Frame, m.Floor, true
	r.X, r.Y, r.Yaw, r.V = m.X, m.Y, m.Yaw, m.V
	g.flush(g.heard(r, now, evs))
	return true
}

// ApplyState records a state message. It returns false if the message is
// stale or a duplicate and was ignored.
func (g *Registry) ApplyState(name string, m StateMsg) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	r, evs := g.get(name, now, nil)
	if !fresh(r.stateBoot, r.stateSeq, m.Boot, m.Seq) {
		return false
	}
	r.stateBoot, r.stateSeq = m.Boot, m.Seq
	if r.Mode != "" && m.Mode != r.Mode { // the first state is covered by "seen"
		evs = append(evs, Event{Time: now, Robot: name, Type: "mode", From: r.Mode, To: m.Mode})
	}
	if m.Error != r.Error {
		evs = append(evs, Event{Time: now, Robot: name, Type: "error", From: r.Error, To: m.Error})
	}
	if m.Host != "" {
		r.Host = m.Host
	}
	r.Mode, r.TaskID, r.Error = m.Mode, m.TaskID, m.Error
	r.CanRide = m.CanRide == nil || *m.CanRide
	r.Battery, r.BatteryKnown = 0, m.Battery != nil
	if m.Battery != nil {
		r.Battery = *m.Battery
	}
	if m.SilenceS > 0 {
		r.LastSeen = now
		r.okStreak = 0
		r.SilentUntil = now.Add(time.Duration(m.SilenceS * float64(time.Second)))
		evs = setConn(r, InElevator, "blackout announced", now, evs)
	} else if r.Conn == InElevator {
		r.LastSeen = now
		r.SilentUntil = time.Time{}
		evs = setConn(r, Online, "blackout ended", now, evs)
	} else {
		evs = g.heard(r, now, evs)
	}
	g.flush(evs)
	return true
}

// AdapterOffline marks every robot bridged by host OFFLINE at once. It is
// called when the adapter's LWT (or clean "offline") arrives.
func (g *Registry) AdapterOffline(host string) {
	g.mu.Lock()
	now := g.now()
	evs := []Event{{Time: now, Type: "adapter", Detail: host + " offline"}}
	for _, r := range g.robots {
		if r.Host == host {
			r.okStreak = 0
			r.SilentUntil = time.Time{}
			evs = setConn(r, Offline, "adapter offline", now, evs)
		}
	}
	g.flush(evs)
	g.mu.Unlock()
}

// Tick re-evaluates liveness from message age. Call it periodically.
func (g *Registry) Tick() {
	g.mu.Lock()
	now := g.now()
	var evs []Event
	for _, r := range g.robots {
		if r.Conn == InElevator {
			if now.Before(r.SilentUntil) {
				continue
			}
			// The announced blackout ran out and the robot has not come back.
			evs = append(evs, Event{Time: now, Robot: r.Name, Type: "elevator_overdue",
				Detail: "no message after announced blackout"})
			r.LastSeen, r.SilentUntil = r.SilentUntil, time.Time{}
			evs = setConn(r, Stale, "blackout overdue", now, evs)
			continue
		}
		age := now.Sub(r.LastSeen)
		if age > g.cfg.StaleAfter {
			r.okStreak = 0 // a gap breaks any recovery streak
		}
		switch {
		case age > g.cfg.OfflineAfter && r.Conn != Offline:
			evs = setConn(r, Offline, "no message for "+age.Round(time.Second).String(), now, evs)
		case age > g.cfg.StaleAfter && r.Conn == Online:
			evs = setConn(r, Stale, "no message for "+age.Round(100*time.Millisecond).String(), now, evs)
		}
	}
	g.flush(evs)
	g.mu.Unlock()
}

// Get returns a copy of one robot.
func (g *Registry) Get(name string) (Robot, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r, ok := g.robots[name]
	if !ok {
		return Robot{}, false
	}
	return *r, true
}

// Snapshot returns copies of all robots sorted by name.
func (g *Registry) Snapshot() []Robot {
	g.mu.RLock()
	out := make([]Robot, 0, len(g.robots))
	for _, r := range g.robots {
		out = append(out, *r)
	}
	g.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
