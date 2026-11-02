// Package fleet keeps the server's view of every robot.
package fleet

import (
	"sort"
	"sync"
	"time"
)

// PoseMsg is the payload of fleet/<robot>/pose.
type PoseMsg struct {
	Boot  string  `json:"boot"`
	Seq   uint64  `json:"seq"`
	Stamp float64 `json:"stamp"`
	Frame string  `json:"frame"`
	Floor int     `json:"floor"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Yaw   float64 `json:"yaw"`
	V     float64 `json:"v"`
}

// StateMsg is the payload of fleet/<robot>/state.
type StateMsg struct {
	Boot    string  `json:"boot"`
	Seq     uint64  `json:"seq"`
	Stamp   float64 `json:"stamp"`
	Mode    string  `json:"mode"`
	Battery float64 `json:"battery"`
	TaskID  string  `json:"task_id"`
	Error   string  `json:"error"`
}

// Robot is a snapshot of one robot as the server last saw it.
type Robot struct {
	Name    string  `json:"name"`
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

	// LastSeen is when the server last accepted any message from this robot.
	// It uses the server clock, so robot clock skew cannot affect liveness.
	LastSeen time.Time `json:"last_seen"`

	poseBoot, stateBoot string
	poseSeq, stateSeq   uint64
}

// Registry is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	robots map[string]*Robot
	now    func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{robots: make(map[string]*Robot), now: time.Now}
}

func (g *Registry) get(name string) *Robot {
	r, ok := g.robots[name]
	if !ok {
		r = &Robot{Name: name}
		g.robots[name] = r
	}
	return r
}

// fresh reports whether (boot, seq) is newer than what was last accepted.
// A different boot means the sender restarted and its seq began again.
func fresh(lastBoot string, lastSeq uint64, boot string, seq uint64) bool {
	return boot != lastBoot || seq > lastSeq
}

// ApplyPose records a pose message. It returns false if the message is stale
// or a duplicate and was ignored.
func (g *Registry) ApplyPose(name string, m PoseMsg) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.get(name)
	if !fresh(r.poseBoot, r.poseSeq, m.Boot, m.Seq) {
		return false
	}
	r.poseBoot, r.poseSeq = m.Boot, m.Seq
	r.Frame, r.Floor = m.Frame, m.Floor
	r.X, r.Y, r.Yaw, r.V = m.X, m.Y, m.Yaw, m.V
	r.LastSeen = g.now()
	return true
}

// ApplyState records a state message. It returns false if the message is
// stale or a duplicate and was ignored.
func (g *Registry) ApplyState(name string, m StateMsg) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.get(name)
	if !fresh(r.stateBoot, r.stateSeq, m.Boot, m.Seq) {
		return false
	}
	r.stateBoot, r.stateSeq = m.Boot, m.Seq
	r.Mode, r.Battery, r.TaskID, r.Error = m.Mode, m.Battery, m.TaskID, m.Error
	r.LastSeen = g.now()
	return true
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
