package task

import (
	"strings"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

func batteryKinds(evs []Event, robot string) []string {
	var out []string
	for _, e := range evs {
		if e.Type == "battery" && e.Robot == robot {
			out = append(out, e.Kind)
		}
	}
	return out
}

func docksTo(h *harness, robot string) []Command {
	var out []Command
	for _, c := range h.sentTo(robot) {
		if c.Type == "dock" {
			out = append(out, c)
		}
	}
	return out
}

func (h *harness) eligible(robot string) bool {
	h.t.Helper()
	h.mu()
	defer h.unmu()
	return h.m.ineligible(*h.robot(robot), nil, h.now) == ""
}

// The robot stays out of the task pool from below 25 % until 90 %.
func TestBatteryHysteresis(t *testing.T) {
	for _, tc := range []struct {
		name    string
		levels  []float64 // reported one after another, a tick each
		mode    string    // reported with the last level
		flagged bool      // needs charge after the last level
		wantEvs string
	}{
		{"24 at first report", []float64{24}, "IDLE", true, "low"},
		{"25 at first report", []float64{25}, "IDLE", false, ""},
		{"26 at first report", []float64{26}, "IDLE", false, ""},
		{"drops to 24.9", []float64{30, 24.9}, "IDLE", true, "low"},
		{"back to 26 without charging", []float64{30, 24, 26}, "IDLE", true, "low"},
		{"charging stopped at 60", []float64{30, 24, 60}, "IDLE", true, "low"},
		{"charged to 89", []float64{30, 24, 89}, "IDLE", true, "low"},
		{"charged to 90", []float64{30, 24, 90}, "IDLE", false, "low charged"},
		{"90 while still CHARGING", []float64{30, 24, 90}, "CHARGING", false, "low charged"},
		{"flaps around 25 once flagged", []float64{30, 24, 26, 24, 26}, "IDLE", true, "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
			for i, b := range tc.levels {
				h.robot("a").Battery = b
				if i == len(tc.levels)-1 {
					h.robot("a").Mode = tc.mode
				}
				h.tick()
			}
			h.mu()
			flagged := h.m.needsCharge(*h.robot("a"))
			h.unmu()
			if flagged != tc.flagged {
				t.Fatalf("needs charge = %v, want %v", flagged, tc.flagged)
			}
			// A robot that never needed charge (no dock sent) is eligible now.
			if !tc.flagged && len(docksTo(h, "a")) == 0 && tc.mode == "IDLE" && !h.eligible("a") {
				t.Fatal("unflagged robot is not eligible")
			}
			kinds := batteryKinds(h.events, "a")
			// A flagged IDLE robot is also sent a dock; only the flag events matter here.
			var flags []string
			for _, k := range kinds {
				if k != "dock_sent" {
					flags = append(flags, k)
				}
			}
			if got := strings.Join(flags, " "); got != tc.wantEvs {
				t.Fatalf("battery events %q, want %q", got, tc.wantEvs)
			}
		})
	}
}

func TestDockSentResentThenBacksOff(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	h.robot("a").Battery = 20
	h.tick()
	docks := docksTo(h, "a")
	if len(docks) != 1 || !strings.HasPrefix(docks[0].CmdID, "dock.a.") ||
		docks[0].ExpiresAt != float64(h.now.Add(10*time.Second).UnixMilli())/1000 {
		t.Fatalf("docks %+v, want one with expires_at now + 10 s", docks)
	}
	h.advance(9750 * time.Millisecond)
	if n := len(docksTo(h, "a")); n != 1 {
		t.Fatalf("%d docks before DockTimeout", n)
	}
	h.advance(250 * time.Millisecond)
	docks = docksTo(h, "a")
	if len(docks) != 2 || docks[1].CmdID == docks[0].CmdID {
		t.Fatalf("docks %+v, want a second one with a new cmd_id", docks)
	}
	h.advance(10 * time.Second)
	if n := len(docksTo(h, "a")); n != 3 {
		t.Fatalf("%d docks after 20 s, want 3", n)
	}
	h.advance(10 * time.Second) // third timed out: back off
	want := "low dock_sent dock_sent dock_sent dock_failed"
	if got := strings.Join(batteryKinds(h.events, "a"), " "); got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
	h.advance(59 * time.Second)
	if n := len(docksTo(h, "a")); n != 3 {
		t.Fatalf("%d docks during the back-off, want 3", n)
	}
	h.advance(time.Second)
	if n := len(docksTo(h, "a")); n != 4 {
		t.Fatalf("%d docks after the back-off, want 4", n)
	}

	// It docks now: no more docks, and it charges.
	h.ack("a", docksTo(h, "a")[3].CmdID, "accepted")
	h.robot("a").Mode = "DOCKING"
	h.advance(30 * time.Second)
	h.robot("a").Mode, h.robot("a").Battery = "CHARGING", 60
	h.advance(30 * time.Second)
	if n := len(docksTo(h, "a")); n != 4 {
		t.Fatalf("%d docks while DOCKING/CHARGING, want 4", n)
	}
	h.robot("a").Battery = 100
	h.tick()
	h.robot("a").Mode = "IDLE"
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	h.want(tk.ID, Assigned, "a")
	want = "low dock_sent dock_sent dock_sent dock_failed dock_sent docking charged"
	if got := strings.Join(batteryKinds(h.events, "a"), " "); got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
}

// A robot that leaves the dock before it is charged is sent back.
func TestDockAgainAfterChargingStoppedEarly(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	h.robot("a").Battery = 20
	h.tick()
	h.robot("a").Mode = "CHARGING"
	h.advance(time.Second)
	h.robot("a").Mode, h.robot("a").Battery = "IDLE", 60 // someone cancelled the charge
	h.tick()
	if n := len(docksTo(h, "a")); n != 2 {
		t.Fatalf("%d docks, want a second one right after the robot left the dock", n)
	}
}

func TestNoDockWhenRobotCannotGo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(r *fleet.Robot)
	}{
		{"STALE", func(r *fleet.Robot) { r.Conn = fleet.Stale }},
		{"OFFLINE", func(r *fleet.Robot) { r.Conn = fleet.Offline }},
		{"IN_ELEVATOR", func(r *fleet.Robot) { r.Conn = fleet.InElevator }},
		{"ERROR", func(r *fleet.Robot) { r.Mode, r.Error = "ERROR", "bumper" }},
		{"error while IDLE", func(r *fleet.Robot) { r.Error = "bumper" }},
		{"already DOCKING", func(r *fleet.Robot) { r.Mode = "DOCKING" }},
		{"already CHARGING", func(r *fleet.Robot) { r.Mode = "CHARGING" }},
		{"MOVING a stray goto", func(r *fleet.Robot) { r.Mode, r.TaskID = "MOVING", "go1" }},
		{"IDLE but reports a task_id", func(r *fleet.Robot) { r.TaskID = "go1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
			h.robot("a").Battery = 20
			tc.change(h.robot("a"))
			h.advance(2 * time.Minute)
			if d := docksTo(h, "a"); len(d) != 0 {
				t.Fatalf("docks %+v", d)
			}
		})
	}
}

// A low robot finishes its task before it docks.
func TestBusyRobotDocksAfterItsTask(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1}, Waypoint{X: 2, Floor: 1})
	h.robot("a").Battery = 20 // low but above critical
	h.advance(5 * time.Second)
	if d := docksTo(h, "a"); len(d) != 0 {
		t.Fatalf("docked a robot holding a task: %+v", d)
	}
	h.ack("a", tk.CmdID, "done")
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", ""
	h.tick() // leg 1 is assigned: still holds the task
	tk = h.want(tk.ID, Assigned, "a")
	if d := docksTo(h, "a"); len(d) != 0 {
		t.Fatalf("docked between legs: %+v", d)
	}
	h.ack("a", tk.CmdID, "accepted")
	h.ack("a", tk.CmdID, "done")
	h.want(tk.ID, Done, "a")
	h.tick()
	if d := docksTo(h, "a"); len(d) != 1 {
		t.Fatalf("docks %+v after the task, want 1", d)
	}
	if got := h.task(tk.ID); got.Attempts != 0 {
		t.Fatalf("attempts %d", got.Attempts)
	}
}

// At the critical level the robot is cancelled; the task stays on it until
// it has stopped, then goes to another robot without using an attempt. The
// robot docks once nothing it was told to drive can still land.
func TestCriticalBatteryHandsTaskBackAndDocks(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	old := tk.CmdID
	h.robot("a").Battery = 20
	h.advance(5 * time.Second)
	h.want(tk.ID, Running, "a")

	h.robot("a").Battery = 9.9
	h.tick()
	cmds := h.sentTo("a")
	if last := cmds[len(cmds)-1]; last.Type != "cancel" {
		t.Fatalf("sent to a: %+v, want a cancel", cmds)
	}
	h.advance(time.Second) // still driving: nobody else gets it yet
	h.want(tk.ID, Running, "a")

	h.ack("a", old, "cancelled")
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", ""
	h.tick()
	got := h.want(tk.ID, Assigned, "b")
	if got.Attempts != 0 || len(got.FailedOn) != 0 || !strings.Contains(got.Error, "battery critical") {
		t.Fatalf("handed back as %+v, want no attempt used and no failed_on", got)
	}
	h.ack("b", got.CmdID, "accepted")
	h.ack("b", got.CmdID, "done")
	if got := h.want(tk.ID, Done, "b"); got.Attempts != 0 {
		t.Fatalf("attempts %d", got.Attempts)
	}
	h.advance(12 * time.Second) // the cancel (sent at the hand-back) may land until + 15 s
	if d := docksTo(h, "a"); len(d) != 0 {
		t.Fatalf("docked while a watched goto may still land: %+v", d)
	}
	h.advance(2 * time.Second)
	if d := docksTo(h, "a"); len(d) != 1 {
		t.Fatalf("docks %+v, want 1 once the watch ended", d)
	}
}

// The cancel is repeated while the robot drives on; once it stops and its
// final ack is lost, the same cmd_id asks the adapter to repeat it.
func TestCriticalHandBackWatchedAndAsked(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.robot("a").Battery = 5
	h.tick()
	h.advance(3 * time.Second) // cancel lost; a still reports the goto
	if n := cancelsTo(h, "a"); n != 2 {
		t.Fatalf("%d cancels, want a repeat", n)
	}
	h.want(tk.ID, Running, "a")
	if d := docksTo(h, "a"); len(d) != 0 {
		t.Fatalf("docked while driving: %+v", d)
	}
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", "" // stopped, "cancelled" lost
	h.advance(3 * time.Second)
	cmds := h.sentTo("a")
	if last := cmds[len(cmds)-1]; last.Type != "goto" || last.CmdID != tk.CmdID {
		t.Fatalf("sent %+v, want the same goto to get the last ack again", cmds)
	}
	h.ack("a", tk.CmdID, "cancelled")
	if got := h.want(tk.ID, Queued, ""); got.Attempts != 0 {
		t.Fatalf("attempts %d", got.Attempts)
	}
}

// The robot reached the goal before the cancel: its "done" finishes a last
// leg, and the next leg of a longer task goes to another robot.
func TestCriticalHandBackDoneFirst(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1}, Waypoint{X: 2, Floor: 1})
	h.robot("a").Battery = 5
	h.tick()
	h.ack("a", tk.CmdID, "done")
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", ""
	h.tick()
	got := h.want(tk.ID, Assigned, "b")
	if got.Leg != 1 || got.Attempts != 0 {
		t.Fatalf("after done during hand-back: %+v, want leg 1 on b", got)
	}
	for _, c := range h.sentTo("a") {
		if c.Type == "goto" && c.CmdID != tk.CmdID {
			t.Fatalf("leg 1 sent to the critical robot: %+v", c)
		}
	}
}

// Review 4-1: the robot has finished the last leg (IDLE, no task_id) and the
// "done" is still on its way when the state shows 9 %: nothing is taken back.
func TestCriticalSkipsLegFinishedWithDoneInFlight(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.robot("a").Battery = 20
	h.advance(time.Second)
	r := h.robot("a")
	r.Mode, r.TaskID, r.X, r.Battery = "IDLE", "", 1, 9
	h.tick()
	h.ack("a", tk.CmdID, "done") // arrives one tick later
	if got := h.want(tk.ID, Done, "a"); got.Dispatches != 1 {
		t.Fatalf("finished task re-run: %+v", got)
	}
}

// Review 4-1, between legs: leg 0 finished, its "done" in flight at 9 %.
func TestCriticalSkipsBetweenLegs(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1}, Waypoint{X: 2, Floor: 1})
	r := h.robot("a")
	r.Mode, r.TaskID, r.X, r.Battery = "IDLE", "", 1, 9
	h.tick()
	h.want(tk.ID, Running, "a")
	h.ack("a", tk.CmdID, "done")
	got := h.want(tk.ID, Assigned, "a")
	if got.Leg != 1 || got.Dispatches != 1 || cancelsTo(h, "a") != 0 {
		t.Fatalf("after leg 0: %+v, %d cancels; leg 0 must not run again", got, cancelsTo(h, "a"))
	}
	// Leg 1 lands: now the robot drives it at 9 %, so it is handed back.
	r.Mode, r.TaskID = "MOVING", got.CmdID
	h.tick()
	if cancelsTo(h, "a") != 1 {
		t.Fatal("robot driving leg 1 at 9 % not cancelled")
	}
}

// Review 4-1b: after a restart the probe must get its chance to collect a
// "done" sent while the server was down.
func TestCriticalSkipsRestoredTask(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.restart("b1")
	h.m.LinkUp()
	r := idle("a", 1, 0)
	r.Battery = 5
	h.addRobot(r)
	h.tick() // probe
	h.ack("a", tk.CmdID, "done")
	if got := h.task(tk.ID); got.State != Done {
		t.Fatalf("restored task finished while down: %s error=%q sent=%v", got.State, got.Error, h.sentTo("a"))
	}
}

// Battery readings of an unreachable robot are old: no hand-back.
func TestCriticalWaitsForOnline(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.robot("a").Battery, h.robot("a").Conn = 5, fleet.Stale
	h.advance(10 * time.Second)
	if n := cancelsTo(h, "a"); n != 0 {
		t.Fatalf("%d cancels to a STALE robot", n)
	}
	h.want(tk.ID, Running, "a")
	h.robot("a").Conn = fleet.Online
	h.tick()
	if n := cancelsTo(h, "a"); n != 1 {
		t.Fatalf("%d cancels once ONLINE, want 1", n)
	}
}

func TestCriticalThreshold(t *testing.T) {
	for _, tc := range []struct {
		battery float64
		cancel  bool
	}{{10, false}, {9.99, true}} {
		h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
		h.start("a", Waypoint{X: 1, Floor: 1})
		h.robot("a").Battery = tc.battery
		h.tick()
		if got := cancelsTo(h, "a") == 1; got != tc.cancel {
			t.Errorf("at %.2f%%: cancelled %v, want %v", tc.battery, got, tc.cancel)
		}
	}
}

// Review 4-2: a dock sent just before the flag cleared may still land; a
// goto assigned meanwhile would be ended by it and cost an attempt.
func TestNoTaskWhileDockMayLand(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	h.robot("a").Battery = 24
	h.tick()
	if len(docksTo(h, "a")) != 1 {
		t.Fatal("no dock")
	}
	h.robot("a").Battery = 91 // charged by hand, or the reading jumps
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.advance(14 * time.Second)
	h.want(tk.ID, Queued, "")
	h.advance(time.Second) // dock sent at 0: CmdTTL 10 s + margin 5 s
	h.want(tk.ID, Assigned, "a")
}

// A flagged robot is not docked while a goto taken from it may still land.
func TestNoDockWhileWatched(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	h.want(tk.ID, Assigned, "a")
	h.m.Cancel(tk.ID) // watched until + 15 s
	h.robot("a").Battery = 20
	h.advance(14 * time.Second)
	if d := docksTo(h, "a"); len(d) != 0 {
		t.Fatalf("docked while watched: %+v", d)
	}
	h.advance(time.Second)
	if d := docksTo(h, "a"); len(d) != 1 {
		t.Fatalf("docks %+v after the watch, want 1", d)
	}
}

// A charger that stops below ResumeBattery: sent back once, accepted the
// second time (or backed off if still below LowBattery).
func TestChargerStoppingEarly(t *testing.T) {
	charge := func(h *harness, stopAt float64) {
		h.robot("a").Mode = "CHARGING"
		h.tick()
		h.robot("a").Mode, h.robot("a").Battery = "IDLE", stopAt
		h.tick()
	}
	t.Run("above LowBattery", func(t *testing.T) {
		h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
		h.robot("a").Battery = 20
		h.tick()
		charge(h, 70)
		if n := len(docksTo(h, "a")); n != 2 {
			t.Fatalf("%d docks after the first short charge, want 2", n)
		}
		charge(h, 75)
		if n := len(docksTo(h, "a")); n != 2 {
			t.Fatalf("%d docks after the second short charge, want 2", n)
		}
		want := "low dock_sent docking dock_sent docking charged"
		if got := strings.Join(batteryKinds(h.events, "a"), " "); got != want {
			t.Fatalf("events %q, want %q", got, want)
		}
		h.mu()
		flagged := h.m.needsCharge(*h.robot("a"))
		h.unmu()
		if flagged {
			t.Fatal("still flagged after two short charges")
		}
	})
	t.Run("below LowBattery", func(t *testing.T) {
		h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
		h.robot("a").Battery = 20
		h.tick()
		charge(h, 21)
		charge(h, 22)
		h.advance(59 * time.Second)
		if n := len(docksTo(h, "a")); n != 2 {
			t.Fatalf("%d docks during the back-off, want 2", n)
		}
		h.advance(time.Second)
		if n := len(docksTo(h, "a")); n != 3 {
			t.Fatalf("%d docks after the back-off, want 3", n)
		}
		if k := batteryKinds(h.events, "a"); !strings.Contains(strings.Join(k, " "), "dock_failed") {
			t.Fatalf("events %v, want dock_failed", k)
		}
	})
}

// Review 4-4: a robot whose state has no battery is never flagged or docked
// and takes tasks.
func TestRobotWithoutBattery(t *testing.T) {
	reg := fleet.NewRegistry(fleet.DefaultConfig(), nil)
	reg.ApplyPose("a", fleet.PoseMsg{Boot: "x", Seq: 1, Floor: 1})
	reg.ApplyState("a", fleet.StateMsg{Boot: "x", Seq: 1, Mode: "IDLE"}) // "battery": null
	ra, _ := reg.Get("a")
	h := newHarness(t, DefaultConfig(), ra)
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	h.want(tk.ID, Assigned, "a")
	h.advance(10 * time.Minute)
	if d, ev := docksTo(h, "a"), batteryKinds(h.events, "a"); len(d) != 0 || len(ev) != 0 {
		t.Fatalf("docks %v, battery events %v", d, ev)
	}
}

// Review 4-3: until its first pose a robot's position is unknown.
func TestStateBeforePose(t *testing.T) {
	reg := fleet.NewRegistry(fleet.DefaultConfig(), nil)
	b80 := 80.0
	reg.ApplyState("a", fleet.StateMsg{Boot: "x", Seq: 1, Mode: "IDLE", Battery: &b80})
	ra, _ := reg.Get("a")
	if ra.HasPose {
		t.Fatal("HasPose set by a state message")
	}
	t.Run("not eligible", func(t *testing.T) {
		b := idle("b", 3, 0)
		b.Floor = 0
		h := newHarness(t, DefaultConfig(), ra, b)
		tk := h.submit(Waypoint{X: 0, Floor: 0}) // a's phantom spot
		h.tick()
		h.want(tk.ID, Assigned, "b")
	})
	t.Run("no leg deadline from the phantom pose", func(t *testing.T) {
		// After a restart a robot may report its leg before its first pose.
		h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
		tk := h.start("a", Waypoint{X: 1, Floor: 1})
		h.restart("b1")
		h.m.LinkUp()
		r := ra
		r.Mode, r.TaskID = "MOVING", tk.CmdID
		h.addRobot(r)
		h.advance(5 * time.Second)
		h.robot("a").HasPose, h.robot("a").Floor, h.robot("a").X = true, 1, 61 // 60 m to go
		h.advance(60 * time.Second)
		if got := h.want(tk.ID, Running, "a"); got.Attempts != 0 {
			t.Fatalf("leg timed out on the phantom pose: %+v", got)
		}
	})
}

// When every robot needs charge, tasks wait: no FAILED, no attempts used.
func TestAllRobotsChargingKeepsTasksQueued(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	h.robot("a").Battery = 20
	h.robot("b").Mode, h.robot("b").Battery = "CHARGING", 50
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.advance(5 * time.Minute)
	got := h.want(tk.ID, Queued, "")
	if got.Attempts != 0 || got.Dispatches != 0 || got.Error != "" {
		t.Fatalf("waiting task changed: %+v", got)
	}
	h.robot("b").Battery = 100
	h.tick()
	h.robot("b").Mode = "IDLE"
	h.tick()
	h.want(tk.ID, Assigned, "b")
}

// After a restart the flags are rebuilt from the first state report.
func TestBatteryFlagsAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		battery float64
		flagged bool
	}{
		{"IDLE", 20, true},
		{"IDLE", 60, false}, // charging cut short before the restart is forgotten
		{"CHARGING", 60, true},
		{"DOCKING", 60, true},
		{"CHARGING", 95, false},
		{"MOVING", 20, true},
	} {
		h := newHarness(t, DefaultConfig())
		h.restart("b1")
		h.m.LinkUp()
		r := idle("a", 0, 0)
		r.Mode, r.Battery = tc.mode, tc.battery
		h.addRobot(r)
		h.tick()
		h.mu()
		got := h.m.charge["a"].flagged
		h.unmu()
		if got != tc.flagged {
			t.Errorf("%s at %.0f%%: flagged %v, want %v", tc.mode, tc.battery, got, tc.flagged)
		}
		if ev := batteryKinds(h.events, "a"); (len(ev) > 0 && ev[0] == "low") != tc.flagged {
			t.Errorf("%s at %.0f%%: events %v", tc.mode, tc.battery, ev)
		}
	}
}

// Dock acks are not task acks.
func TestDockAckIgnoredByTasks(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	h.robot("a").Battery = 20
	h.tick()
	d := docksTo(h, "a")[0]
	h.ack("a", d.CmdID, "failed")
	h.advance(9 * time.Second)
	if n := len(docksTo(h, "a")); n != 1 {
		t.Fatalf("%d docks; a refused dock waits for DockTimeout", n)
	}
}
