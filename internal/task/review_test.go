package task

// Tests from the stage 3 review. TestReview* started from the reviewer's
// failing tests (adapted to LinkUp, store errors and the new cmd_id format);
// the rest cover the other findings.

import (
	"errors"
	"slices"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

func cancelsTo(h *harness, robot string) int {
	n := 0
	for _, c := range h.sentTo(robot) {
		if c.Type == "cancel" {
			n++
		}
	}
	return n
}

// restart replaces the manager with a fresh one on the same store, the way
// fleetd starts again. Robots are unknown until the test adds them.
func (h *harness) restart(boot string) {
	h.t.Helper()
	if err := h.m.Flush(); err != nil {
		h.t.Fatal(err)
	}
	h.robots = &fakeRobots{robots: map[string]*fleet.Robot{}}
	h.sender = &fakeSender{}
	h.m = NewManager(DefaultConfig(), Deps{Now: func() time.Time { return h.now }, Robots: h.robots,
		Sender: h.sender, Store: h.store, Boot: boot, Emit: func(e Event) { h.events = append(h.events, e) }})
	if err := h.m.Restore(); err != nil {
		h.t.Fatal(err)
	}
}

// Finding 1: after the restart grace, the watch must survive the robot's
// first pose (Mode "", TaskID "").
func TestReviewRestartOrphanDroppedOnFirstPose(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.restart("b1")
	h.m.LinkUp()
	h.advance(26 * time.Second) // a never shows up; grace runs out
	h.want(tk.ID, Queued, "")
	h.addRobot(fleet.Robot{Name: "a", Conn: fleet.Online, Floor: 1, HasPose: true}) // first pose, no state yet
	h.tick()
	h.robot("a").Mode, h.robot("a").TaskID = "MOVING", tk.CmdID // state: still on the old goto
	h.addRobot(idle("b", 9, 0))
	h.advance(10 * time.Second)
	got := h.task(tk.ID)
	if got.Robot != "b" || cancelsTo(h, "a") == 0 {
		t.Fatalf("task on %q, %d cancels to a; want b to have it and a cancelled", got.Robot, cancelsTo(h, "a"))
	}
}

// Finding 2: user cancel while ASSIGNED; the goto lands after the cancel.
func TestReviewCancelAssignedOrphanDropped(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	tk = h.want(tk.ID, Assigned, "a")
	h.m.Cancel(tk.ID)
	h.tick()                                                    // state still shows nothing: goto in flight
	h.robot("a").Mode, h.robot("a").TaskID = "MOVING", tk.CmdID // goto lands after the cancel
	h.advance(10 * time.Second)
	if n := cancelsTo(h, "a"); n < 2 {
		t.Fatalf("%d cancels; a drives cancelled %s and was not cancelled again", n, tk.CmdID)
	}
}

// Finding 3: a deaf but ONLINE robot must not get the same task back every
// attempt while another robot idles.
func TestReviewDeafRobotBurnsAllAttempts(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 5, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.advance(80 * time.Second)
	got := h.task(tk.ID)
	if len(h.sentTo("b")) == 0 {
		t.Fatalf("b never used; task %s attempts=%d dispatches=%d err=%q", got.State, got.Attempts, got.Dispatches, got.Error)
	}
}

type failStore struct {
	memStore
	fail bool
}

func (s *failStore) Save(t Task) error {
	if s.fail {
		return errors.New("disk full")
	}
	return s.memStore.Save(t)
}

// Finding 5: a failed save must fail Submit, and no id may be reused.
func TestReviewIDReuseAfterFailedSave(t *testing.T) {
	st := &failStore{memStore: memStore{tasks: map[string]Task{}}}
	rb := &fakeRobots{robots: map[string]*fleet.Robot{}}
	now := time.Now()
	m := NewManager(DefaultConfig(), Deps{Now: func() time.Time { return now }, Robots: rb, Sender: &fakeSender{}, Store: st})
	first, _ := m.Submit([]Waypoint{{X: 1}})
	st.fail = true
	if _, err := m.Submit([]Waypoint{{X: 2}}); err == nil {
		t.Fatal("Submit succeeded although the task was not saved")
	}
	if got := m.List(""); len(got) != 1 {
		t.Fatalf("unsaved task is listed: %+v", got)
	}
	st.fail = false
	m2 := NewManager(DefaultConfig(), Deps{Now: func() time.Time { return now }, Robots: rb, Sender: &fakeSender{}, Store: st})
	m2.Restore()
	b, err := m2.Submit([]Waypoint{{X: 3}})
	if err != nil || b.ID == first.ID {
		t.Fatalf("after restart: %+v %v; reuses %s", b, err, first.ID)
	}
}

// Finding 2: a no-response failure must keep watching the robot while the
// last resend may still land.
func TestReviewNoResponseOrphanDroppedImmediately(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 50, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	cmd := h.want(tk.ID, Assigned, "a").CmdID
	h.advance(12 * time.Second)
	h.want(tk.ID, Assigned, "b")
	h.robot("a").Mode, h.robot("a").TaskID = "MOVING", cmd // the last resend lands now
	h.advance(5 * time.Second)
	if cancelsTo(h, "a") == 0 {
		t.Fatalf("a drives %s, b drives the same task, a never cancelled", cmd)
	}
}

// Finding 2: the watch ends once the goto cannot land any more.
func TestWatchEndsAfterCmdCanNoLongerLand(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	h.m.Cancel(tk.ID)
	h.advance(16 * time.Second) // cancel sent at 0: + CmdTTL 10 s + margin 5 s
	h.mu()
	n := len(h.m.watches)
	h.unmu()
	if n != 0 {
		t.Fatalf("%d watches left after the goto expired", n)
	}
}

func (h *harness) mu()   { h.m.mu.Lock() }
func (h *harness) unmu() { h.m.mu.Unlock() }

// Finding 4: watches are lost on restart; the safety net still cancels a
// robot that comes back driving the cmd_id of a task it no longer has.
func TestReturningRobotCancelledAfterRestart(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	old := tk.CmdID
	h.robot("a").Conn = fleet.Offline
	h.tick()
	tk = h.want(tk.ID, Assigned, "b")
	h.ack("b", tk.CmdID, "accepted")
	h.ack("b", tk.CmdID, "done")
	h.want(tk.ID, Done, "b")

	h.restart("b1")
	h.m.LinkUp()
	h.addRobot(fleet.Robot{Name: "a", Conn: fleet.Online, Mode: "MOVING", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, TaskID: old})
	h.advance(2750 * time.Millisecond)
	if n := cancelsTo(h, "a"); n != 0 {
		t.Fatalf("cancelled before AckTimeout: %d", n)
	}
	h.advance(500 * time.Millisecond)
	if n := cancelsTo(h, "a"); n != 1 {
		t.Fatalf("%d cancels to a, want 1", n)
	}
	h.advance(3 * time.Second)
	if n := cancelsTo(h, "a"); n != 2 {
		t.Fatalf("%d cancels to a while it still drives %s, want a repeat", n, old)
	}
}

// Finding 2: right after "done" the robot's state may still show the leg for
// a moment; that must not trigger a cancel.
func TestNoCancelForBriefStaleReportAfterDone(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.ack("a", tk.CmdID, "done")
	h.robot("a").Mode = "IDLE" // TaskID still shows the finished leg
	h.advance(time.Second)
	h.robot("a").TaskID = ""
	h.advance(10 * time.Second)
	if n := cancelsTo(h, "a"); n != 0 {
		t.Fatalf("%d cancels after a normal finish", n)
	}
}

// Finding 1: the grace starts at the first MQTT connection and lasts
// RestoreGrace (above OfflineAfter).
func TestRestoreGraceStartsAtLinkUp(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.restart("b1")
	h.advance(time.Minute) // broker unreachable
	h.want(tk.ID, Running, "a")
	h.m.LinkUp()
	h.advance(24 * time.Second)
	h.want(tk.ID, Running, "a")
	h.advance(2 * time.Second)
	got := h.want(tk.ID, Queued, "")
	if got.Attempts != 0 {
		t.Fatalf("restart requeue used an attempt: %+v", got)
	}
}

// Finding 1: at restart a STALE or IN_ELEVATOR robot keeps its task (like
// any STALE robot) until it reports the leg or goes OFFLINE.
func TestRestoreKeepsTaskOfUnreachableRobot(t *testing.T) {
	for _, conn := range []string{fleet.Stale, fleet.InElevator} {
		t.Run(conn, func(t *testing.T) {
			h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
			tk := h.start("a", Waypoint{X: 1, Floor: 1})
			h.restart("b1")
			h.m.LinkUp()
			h.addRobot(fleet.Robot{Name: "a", Conn: conn, Mode: "MOVING", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true})
			h.advance(40 * time.Second)
			h.want(tk.ID, Running, "a")
			if len(h.sentTo("a")) != 0 {
				t.Fatalf("sent %+v to an unreachable robot", h.sentTo("a"))
			}
			h.robot("a").Conn = fleet.Offline
			h.tick()
			h.want(tk.ID, Queued, "")
		})
	}
	t.Run("STALE robot reporting the leg", func(t *testing.T) {
		h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
		tk := h.start("a", Waypoint{X: 1, Floor: 1})
		h.restart("b1")
		h.m.LinkUp()
		h.addRobot(fleet.Robot{Name: "a", Conn: fleet.Stale, Mode: "MOVING", Floor: 1, Battery: 80, BatteryKnown: true, HasPose: true, TaskID: tk.CmdID})
		h.tick()
		h.robot("a").Conn = fleet.Online
		h.advance(20 * time.Second)
		h.want(tk.ID, Running, "a")
	})
}

// Finding 5: cmd_ids differ between server runs even if the last writes
// were lost, so the adapter never mistakes a new goto for a duplicate.
func TestCmdIDsDifferAcrossBoots(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	before := h.want(tk.ID, Assigned, "a").CmdID
	h.m.mu.Lock()
	clear(h.m.dirty) // the assignment never reached the database
	h.m.mu.Unlock()

	h.restart("b1")
	h.addRobot(idle("a", 0, 0))
	h.tick()
	after := h.want(tk.ID, Assigned, "a").CmdID
	if after == before {
		t.Fatalf("cmd_id %s reused after restart", after)
	}
}

// Finding 5: a failed write stays pending and is retried.
func TestFlushRetriesFailedWrites(t *testing.T) {
	st := &failStore{memStore: memStore{tasks: map[string]Task{}}}
	rb := &fakeRobots{robots: map[string]*fleet.Robot{}}
	m := NewManager(DefaultConfig(), Deps{Robots: rb, Sender: &fakeSender{}, Store: st})
	tk, _ := m.Submit([]Waypoint{{X: 1}})
	st.fail = true
	m.Cancel(tk.ID)
	if err := m.Flush(); err == nil {
		t.Fatal("Flush hid a failed write")
	}
	if st.tasks[tk.ID].State != Queued {
		t.Fatal("store changed although the write failed")
	}
	st.fail = false
	if err := m.Flush(); err != nil || st.tasks[tk.ID].State != Cancelled {
		t.Fatalf("retry: %v, stored %s", err, st.tasks[tk.ID].State)
	}
}

// Finding 7: a leg that takes far too long is cancelled and requeued.
func TestLegTimeout(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 20, 0))
	tk := h.start("a", Waypoint{X: 3, Floor: 1}) // 3 m / 0.3 m/s + 30 s = 40 s
	h.advance(39 * time.Second)
	h.want(tk.ID, Running, "a")

	h.robot("a").Conn = fleet.Stale // unreachable time does not count
	h.advance(10 * time.Second)
	h.robot("a").Conn = fleet.Online
	h.want(tk.ID, Running, "a")

	h.advance(2 * time.Second)
	got := h.want(tk.ID, Assigned, "b")
	if got.Attempts != 1 || !slices.Contains(got.FailedOn, "a") || cancelsTo(h, "a") != 1 {
		t.Fatalf("after timeout: %+v, %d cancels to a", got, cancelsTo(h, "a"))
	}
}

// An adapter whose clock runs ahead answers "failed: expired". That uses up
// one attempt and the task goes on elsewhere.
func TestExpiredAckUsesAnAttempt(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0), idle("b", 9, 0))
	tk := h.submit(Waypoint{X: 1, Floor: 1})
	h.tick()
	cmd := h.want(tk.ID, Assigned, "a").CmdID
	h.m.HandleAck("a", Ack{CmdID: cmd, Status: "failed", Message: "expired"})
	h.tick()
	got := h.want(tk.ID, Assigned, "b")
	if got.Attempts != 1 || got.Error != "failed: expired" {
		t.Fatalf("after expired: %+v", got)
	}
	h.ack("b", got.CmdID, "accepted")
	h.ack("b", got.CmdID, "done")
	h.want(tk.ID, Done, "b")
	if n := cancelsTo(h, "a"); n != 0 {
		t.Fatalf("%d cancels to a, which reported the failure itself", n)
	}
}

func TestCooldownMakesRobotIneligible(t *testing.T) {
	m := NewManager(DefaultConfig(), Deps{})
	now := time.Now()
	m.cooldown["r1"] = now.Add(time.Second)
	if why := m.ineligible(idle("r1", 0, 0), nil, now); why == "" {
		t.Fatal("robot in cooldown is eligible")
	}
	if why := m.ineligible(idle("r1", 0, 0), nil, now.Add(time.Second)); why != "" {
		t.Fatalf("robot still ineligible after cooldown: %s", why)
	}
}

// A robot that failed a task gets it again only when nobody else is free.
func TestFailedRobotUsedWhenNoOtherIsFree(t *testing.T) {
	h := newHarness(t, DefaultConfig(), idle("a", 0, 0))
	tk := h.start("a", Waypoint{X: 1, Floor: 1})
	h.ack("a", tk.CmdID, "failed")
	h.robot("a").Mode, h.robot("a").TaskID = "IDLE", ""
	h.tick()
	h.want(tk.ID, Assigned, "a")
}
