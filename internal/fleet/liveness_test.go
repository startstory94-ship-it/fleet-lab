package fleet

import (
	"testing"
	"time"
)

// feeder sends increasing-seq state messages for one robot.
type feeder struct {
	g    *Registry
	name string
	seq  uint64
}

func (f *feeder) state(m StateMsg) {
	f.seq++
	m.Boot, m.Seq = "b", f.seq
	if m.Host == "" {
		m.Host = "pc1"
	}
	if m.Mode == "" {
		m.Mode = "IDLE"
	}
	f.g.ApplyState(f.name, m)
}

func (f *feeder) heartbeat() { f.state(StateMsg{}) }

func conn(t *testing.T, g *Registry, name string) string {
	t.Helper()
	r, ok := g.Get(name)
	if !ok {
		t.Fatalf("robot %s missing", name)
	}
	return r.Conn
}

// advance moves the fake clock forward in small steps, ticking each time,
// the way the server's ticker would.
func advance(g *Registry, now *time.Time, d time.Duration) {
	const step = 250 * time.Millisecond
	for end := now.Add(d); now.Before(end); {
		*now = now.Add(step)
		g.Tick()
	}
}

func TestShortGapIsStaleNotOffline(t *testing.T) {
	g, now, _ := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.heartbeat()

	advance(g, now, 1500*time.Millisecond)
	if c := conn(t, g, "r1"); c != Online {
		t.Fatalf("after 1.5s: %s, want ONLINE", c)
	}
	advance(g, now, 1*time.Second) // 2.5s total, like a slow Wi-Fi roam
	if c := conn(t, g, "r1"); c != Stale {
		t.Fatalf("after 2.5s: %s, want STALE", c)
	}
	advance(g, now, 10*time.Second)
	if c := conn(t, g, "r1"); c != Stale {
		t.Fatalf("after 12.5s: %s, want still STALE", c)
	}
}

func TestLongGapGoesOffline(t *testing.T) {
	g, now, evs := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.heartbeat()

	advance(g, now, 21*time.Second)
	if c := conn(t, g, "r1"); c != Offline {
		t.Fatalf("after 21s: %s, want OFFLINE", c)
	}
	var path []string
	for _, e := range *evs {
		if e.Type == "conn" {
			path = append(path, e.To)
		}
	}
	if len(path) != 2 || path[0] != Stale || path[1] != Offline {
		t.Errorf("conn events = %v, want [STALE OFFLINE]", path)
	}
}

func TestRecoveryNeedsConsecutiveMessages(t *testing.T) {
	g, now, _ := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.heartbeat()
	advance(g, now, 3*time.Second)

	f.heartbeat()
	f.heartbeat()
	if c := conn(t, g, "r1"); c != Stale {
		t.Fatalf("after 2 messages: %s, want STALE", c)
	}

	// A gap in the middle of recovery resets the streak.
	advance(g, now, 3*time.Second)
	f.heartbeat()
	if c := conn(t, g, "r1"); c != Stale {
		t.Fatalf("streak survived a gap: %s", c)
	}
	f.heartbeat()
	f.heartbeat()
	if c := conn(t, g, "r1"); c != Online {
		t.Fatalf("after 3 consecutive messages: %s, want ONLINE", c)
	}
}

func TestAdapterOfflineDropsItsRobotsOnly(t *testing.T) {
	g, _, _ := newRecordingRegistry()
	(&feeder{g: g, name: "a1"}).state(StateMsg{Host: "pc1"})
	(&feeder{g: g, name: "a2"}).state(StateMsg{Host: "pc1"})
	(&feeder{g: g, name: "b1"}).state(StateMsg{Host: "pc2"})

	g.AdapterOffline("pc1")

	for name, want := range map[string]string{"a1": Offline, "a2": Offline, "b1": Online} {
		if c := conn(t, g, name); c != want {
			t.Errorf("%s: %s, want %s", name, c, want)
		}
	}
}

func TestAnnouncedBlackoutIsNotOffline(t *testing.T) {
	g, now, evs := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.state(StateMsg{Mode: "RIDING", SilenceS: 60})
	if c := conn(t, g, "r1"); c != InElevator {
		t.Fatalf("after announcement: %s, want IN_ELEVATOR", c)
	}

	advance(g, now, 45*time.Second) // longer than OfflineAfter, inside the blackout
	if c := conn(t, g, "r1"); c != InElevator {
		t.Fatalf("during blackout: %s, want IN_ELEVATOR", c)
	}

	f.state(StateMsg{Mode: "MOVING"}) // out of the elevator
	if c := conn(t, g, "r1"); c != Online {
		t.Fatalf("after blackout: %s, want ONLINE", c)
	}
	for _, e := range *evs {
		if e.Type == "elevator_overdue" {
			t.Error("unexpected elevator_overdue")
		}
	}
}

func TestPoseDuringBlackoutDoesNotEndIt(t *testing.T) {
	g, _, _ := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.state(StateMsg{Mode: "BOARDING", SilenceS: 60})
	// The adapter may still send a pose or two before Wi-Fi drops.
	g.ApplyPose("r1", PoseMsg{Boot: "b", Seq: 1})
	if c := conn(t, g, "r1"); c != InElevator {
		t.Fatalf("pose ended the blackout: %s", c)
	}
}

func TestOverdueBlackoutRaisesAlarmThenTimesOut(t *testing.T) {
	g, now, evs := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.state(StateMsg{Mode: "RIDING", SilenceS: 30})

	advance(g, now, 31*time.Second)
	if c := conn(t, g, "r1"); c != Stale {
		t.Fatalf("just after overdue: %s, want STALE", c)
	}
	overdue := 0
	for _, e := range *evs {
		if e.Type == "elevator_overdue" {
			overdue++
		}
	}
	if overdue != 1 {
		t.Fatalf("elevator_overdue events = %d, want 1", overdue)
	}

	// OFFLINE counts from the end of the blackout, not from the announcement.
	advance(g, now, 15*time.Second)
	if c := conn(t, g, "r1"); c != Stale {
		t.Fatalf("15s after overdue: %s, want STALE", c)
	}
	advance(g, now, 10*time.Second)
	if c := conn(t, g, "r1"); c != Offline {
		t.Fatalf("25s after overdue: %s, want OFFLINE", c)
	}
}

func TestModeAndErrorEvents(t *testing.T) {
	g, _, evs := newRecordingRegistry()
	f := &feeder{g: g, name: "r1"}
	f.heartbeat()
	f.state(StateMsg{Mode: "ERROR", Error: "simulated_fault"})
	f.state(StateMsg{Mode: "ERROR", Error: "simulated_fault"}) // no change, no event

	var got []string
	for _, e := range *evs {
		got = append(got, e.Type+":"+e.To)
	}
	want := []string{"mode:IDLE", "mode:ERROR", "error:simulated_fault"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}
