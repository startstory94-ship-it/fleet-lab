package fleet

import (
	"encoding/json"
	"testing"
	"time"
)

func newTestRegistry() (*Registry, *time.Time) {
	g, now, _ := newRecordingRegistry()
	return g, now
}

// newRecordingRegistry returns a registry on a fake clock that records events.
func newRecordingRegistry() (*Registry, *time.Time, *[]Event) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	var evs []Event
	g := NewRegistry(DefaultConfig(), func(e Event) { evs = append(evs, e) })
	g.now = func() time.Time { return now }
	return g, &now, &evs
}

func TestApplyPoseDropsStaleAndDuplicate(t *testing.T) {
	g, _ := newTestRegistry()

	if !g.ApplyPose("r1", PoseMsg{Boot: "a", Seq: 5, X: 5}) {
		t.Fatal("first message rejected")
	}
	if g.ApplyPose("r1", PoseMsg{Boot: "a", Seq: 5, X: 99}) {
		t.Error("duplicate seq accepted")
	}
	if g.ApplyPose("r1", PoseMsg{Boot: "a", Seq: 3, X: 3}) {
		t.Error("older seq accepted")
	}
	if !g.ApplyPose("r1", PoseMsg{Boot: "a", Seq: 6, X: 6}) {
		t.Error("newer seq rejected")
	}
	if r, _ := g.Get("r1"); r.X != 6 {
		t.Errorf("X = %v, want 6", r.X)
	}
}

func TestNewBootResetsSeq(t *testing.T) {
	g, _ := newTestRegistry()
	g.ApplyState("r1", StateMsg{Boot: "a", Seq: 100, Mode: "MOVING"})

	// Adapter restarted: seq starts over under a new boot id.
	if !g.ApplyState("r1", StateMsg{Boot: "b", Seq: 1, Mode: "IDLE"}) {
		t.Fatal("first message after restart rejected")
	}
	if g.ApplyState("r1", StateMsg{Boot: "b", Seq: 1, Mode: "ERROR"}) {
		t.Error("duplicate after restart accepted")
	}
	if r, _ := g.Get("r1"); r.Mode != "IDLE" {
		t.Errorf("Mode = %q, want IDLE", r.Mode)
	}
}

func TestPoseAndStateSeqAreIndependent(t *testing.T) {
	g, _ := newTestRegistry()
	g.ApplyPose("r1", PoseMsg{Boot: "a", Seq: 50})
	if !g.ApplyState("r1", StateMsg{Boot: "a", Seq: 1, Mode: "IDLE"}) {
		t.Error("state seq 1 rejected because of pose seq 50")
	}
}

func TestLastSeenUsesServerClock(t *testing.T) {
	g, now := newTestRegistry()
	g.ApplyPose("r1", PoseMsg{Boot: "a", Seq: 1, Stamp: 1})
	*now = now.Add(3 * time.Second)
	g.ApplyState("r1", StateMsg{Boot: "a", Seq: 1, Stamp: 1})
	r, _ := g.Get("r1")
	if !r.LastSeen.Equal(*now) {
		t.Errorf("LastSeen = %v, want %v", r.LastSeen, *now)
	}

	// A rejected message must not refresh LastSeen.
	*now = now.Add(time.Second)
	g.ApplyState("r1", StateMsg{Boot: "a", Seq: 1})
	if r2, _ := g.Get("r1"); !r2.LastSeen.Equal(r.LastSeen) {
		t.Error("rejected message refreshed LastSeen")
	}
}

func TestSnapshotIsSortedCopy(t *testing.T) {
	g, _ := newTestRegistry()
	g.ApplyPose("robot_2", PoseMsg{Boot: "a", Seq: 1})
	g.ApplyPose("robot_1", PoseMsg{Boot: "a", Seq: 1})
	s := g.Snapshot()
	if len(s) != 2 || s[0].Name != "robot_1" || s[1].Name != "robot_2" {
		t.Fatalf("snapshot = %+v", s)
	}
	s[0].X = 42
	if r, _ := g.Get("robot_1"); r.X == 42 {
		t.Error("snapshot shares memory with registry")
	}
}

func TestCanRideDefaultsToTrue(t *testing.T) {
	g, _ := newTestRegistry()
	yes, no := true, false
	g.ApplyPose("pose_only", PoseMsg{Boot: "a", Seq: 1})
	for name, tc := range map[string]struct {
		msg  StateMsg
		want bool
	}{
		"missing": {StateMsg{Boot: "a", Seq: 1, Mode: "IDLE"}, true},
		"false":   {StateMsg{Boot: "a", Seq: 1, Mode: "IDLE", CanRide: &no}, false},
		"true":    {StateMsg{Boot: "a", Seq: 1, Mode: "IDLE", CanRide: &yes}, true},
	} {
		g.ApplyState(name, tc.msg)
		if r, _ := g.Get(name); r.CanRide != tc.want {
			t.Errorf("%s: CanRide = %v, want %v", name, r.CanRide, tc.want)
		}
	}
	if r, _ := g.Get("pose_only"); !r.CanRide {
		t.Error("a robot known only by its pose must count as able to ride")
	}
	// The flag follows the latest state, not the first.
	g.ApplyState("true", StateMsg{Boot: "a", Seq: 2, Mode: "IDLE", CanRide: &no})
	g.ApplyState("false", StateMsg{Boot: "a", Seq: 2, Mode: "IDLE"})
	if r, _ := g.Get("true"); r.CanRide {
		t.Error("later can_ride:false ignored")
	}
	if r, _ := g.Get("false"); !r.CanRide {
		t.Error("later state without can_ride should mean yes again")
	}

	var m StateMsg
	if err := json.Unmarshal([]byte(`{"boot":"a","seq":1,"can_ride":false}`), &m); err != nil || m.CanRide == nil || *m.CanRide {
		t.Errorf("can_ride:false not parsed: %+v %v", m, err)
	}
}
