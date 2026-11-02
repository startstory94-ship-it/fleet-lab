package elevator

import (
	"strings"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

// 6a review: R1-R4 (adapted from the reviewer's proofs) and tests that kill
// mutants which survived the first round.

// R1: an exit turn that is never answered is re-sent (same n), then counted
// as a failed try (the robot is still inside), then stuck at the last stop
// (task handed on once), and retried every StuckRetry. Never stranded.
func TestReviewUnansweredExitIsRetriedNotStranded(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	h.answer(ZoneClear)
	h.open(3)
	n0 := h.trip().Turn.N
	h.live(DefaultTiming().TurnTimeout-time.Second, 3, 0)
	resent := 0
	for _, tn := range h.link.turns {
		if tn.N == n0 {
			resent++
		}
	}
	if resent < 3 {
		t.Fatalf("exit turn published %d times in 14 s, want re-sent every 5 s", resent)
	}
	h.live(2*DefaultTiming().TurnTimeout, 3, 0)
	tr := h.trip()
	if tr.Stage != StageBlocked || tr.rider("a").Status != RiderStuck {
		t.Fatalf("after two unanswered tries: stage %s, a %s", tr.Stage, tr.rider("a").Status)
	}
	if n := h.countNotices(NoticeStuck); n != 1 {
		t.Fatalf("%d stuck notices, want 1", n)
	}
	before := strings.Count(h.turnLog(), ":")
	h.live(DefaultTiming().StuckRetry+5*time.Second, 3, 0)
	if strings.Count(h.turnLog(), ":") <= before {
		t.Fatalf("no exit turn again after StuckRetry: %q", h.turnLog())
	}
	h.answer(ZoneClear)
	if h.car.trip != nil {
		t.Fatalf("not released after the retried exit: %+v", h.car.trip)
	}
	if n := h.countNotices(NoticeStuck); n != 1 {
		t.Fatalf("%d stuck notices after the retry, want still 1", n)
	}
}

func (h *carH) countNotices(kind string) int {
	n := 0
	for _, x := range h.notices {
		if x.Kind == kind {
			n++
		}
	}
	return n
}

// R2: a timed-out turn never lets the next robot move while the door zone
// is occupied, and a clear does not either until the zone is seen empty.
func TestReviewTimeoutDoesNotGrantNext(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.acceptAll()
	h.open(1)
	first := h.trip().Turn.Robot
	h.live(3*DefaultTiming().TurnTimeout, 1, 1)
	if strings.Count(h.turnLog(), ":") != 1 {
		t.Fatalf("turns %q while %s's zone is occupied", h.turnLog(), first)
	}
	h.live(time.Second, 1, 0) // zone empty: first robot retried, nobody else
	if h.turnLog() != first+":board@1 "+first+":board@1" {
		t.Fatalf("turns %q, want %s retried", h.turnLog(), first)
	}
	tn := h.trip().Turn
	h.take(h.car.Zone(Zone{Trip: tn.Trip, N: tn.N, Robot: tn.Robot, Action: tn.Action, Event: ZoneClear, Floor: 1}, h.now))
	h.live(10*time.Second, 1, 1) // answered, but the car still senses something
	if strings.Count(h.turnLog(), ":") != 2 {
		t.Fatalf("next robot moved with the door zone occupied: %q", h.turnLog())
	}
	h.live(time.Second, 1, 0)
	if strings.Count(h.turnLog(), ":") != 3 {
		t.Fatalf("next robot not moving once the zone is empty: %q", h.turnLog())
	}
}

// R3: no turn with the door closed; goto the same floor reopens it.
func TestReviewNoTurnWithDoorClosed(t *testing.T) {
	h := newCarH(t)
	h.state(3, DoorClosed, "")
	tr := Trip{ID: "E1-old-1", Boot: "old", Car: "E1", Stage: StageExiting, Origin: 1, Dir: 1, Stops: []int{3, 5}, Floor: 3,
		Riders: []Rider{
			{Robot: "a", Dest: 3, Exit: 3, Board: 1, Slot: 0, Status: RiderInside, Accepted: true},
			{Robot: "b", Dest: 5, Exit: 5, Board: 0, Slot: 1, Status: RiderInside, Accepted: true},
		}, N: 2}
	h.take(h.car.Restore(tr, h.now))
	h.state(3, DoorClosed, "E1-old-1")
	h.tick(6 * time.Second)
	h.state(3, DoorClosed, "E1-old-1")
	if len(h.link.turns) != 0 || strings.Count(h.cmdLog(), "goto@3") < 2 {
		t.Fatalf("turns %q cmds %q: want no turn and goto 3 repeated", h.turnLog(), h.cmdLog())
	}
	h.state(3, DoorOpen, "E1-old-1")
	if h.turnLog() != "a:exit@3" {
		t.Fatalf("turns %q", h.turnLog())
	}
	if tn := h.link.turns[0]; tn.Lobby != testCar.LobbyAt(3) {
		t.Fatalf("turn lobby %+v", tn.Lobby)
	}
	h.answer(ZoneClear)
	h.open(5)
	n := strings.Count(h.turnLog(), ":")
	tn := *h.trip().Turn
	h.take(h.car.Zone(Zone{Trip: tn.Trip, N: tn.N, Robot: tn.Robot, Action: tn.Action, Event: ZoneFailed, Floor: 5}, h.now))
	h.state(5, DoorClosed, "E1-old-1") // the car closed on its own
	h.tick(time.Second)
	h.state(5, DoorClosed, "E1-old-1")
	if strings.Count(h.turnLog(), ":") != n {
		t.Fatalf("turn with the door closed: %q", h.turnLog())
	}
	if !strings.HasSuffix(h.cmdLog(), "goto@5") {
		t.Fatalf("door not reopened: %q", h.cmdLog())
	}
}

// R4: a robot that gave no answer at 3 is carried to 5, where it stands in
// front: it exits first, a late "failed" from 3 changes nothing, and the car
// never goes back.
func TestReviewLateExitFailureKeepsOrder(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3() // a->3 slot0, c->5 slot1, b->7 slot2
	h.acceptAll()
	h.open(1)
	for range 3 {
		h.answer(ZoneClear)
	}
	h.open(3)
	old := *h.trip().Turn
	h.live(2*DefaultTiming().TurnTimeout, 3, 0)
	if a := h.trip().rider("a"); a.Exit != 5 || a.Status != RiderInside {
		t.Fatalf("a %+v, want carried to 5", a)
	}
	h.take(h.car.Zone(Zone{Trip: old.Trip, N: old.N, Robot: "a", Action: ActExit, Event: ZoneFailed, Floor: 3, Reason: "late"}, h.now))
	if a := h.trip().rider("a"); a.Exit != 5 || a.Status != RiderInside {
		t.Fatalf("late failed changed a: %+v", a)
	}
	h.open(5)
	if tn := h.trip().Turn; tn == nil || tn.Robot != "a" {
		t.Fatalf("turn at 5 %+v, want a (in front) first", tn)
	}
	h.answer(ZoneClear)
	if tn := h.trip().Turn; tn == nil || tn.Robot != "c" {
		t.Fatalf("turn %+v, want c", tn)
	}
	h.answer(ZoneClear)
	h.open(7)
	h.answer(ZoneClear)
	if h.car.trip != nil || strings.Contains(h.cmdLog(), "goto@5 goto@3") {
		t.Fatalf("cmds %q", h.cmdLog())
	}
}

// Only the robot that failed, and robots behind it, are carried: one in
// front that got out stays out, and nobody in front is ever moved on.
func TestReviewCarryOnlyFromTheFailedRobotBack(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.car.Wait("x", 1, 3, "cx", h.now)
	h.car.Wait("b", 1, 7, "cb", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	for range 3 {
		h.answer(ZoneClear)
	}
	h.open(3)
	h.answer(ZoneClear)  // a (slot 0) out
	h.answer(ZoneFailed) // x
	h.answer(ZoneFailed) // x gives up
	a, x, b := h.trip().rider("a"), h.trip().rider("x"), h.trip().rider("b")
	if a.Status != RiderOut || a.Exit != 3 || x.Exit != 7 || b.Exit != 7 {
		t.Fatalf("a %+v x %+v b %+v", a, x, b)
	}
	h.open(7)
	h.answer(ZoneClear)
	h.answer(ZoneClear)
	if !strings.HasSuffix(h.turnLog(), "x:exit@7 b:exit@7") {
		t.Fatalf("turns %q", h.turnLog())
	}
	// A robot in front that is still inside blocks the door for one
	// behind it, even if the one behind gets out here.
	h2 := newCarH(t)
	h2.state(3, DoorClosed, "")
	tr := Trip{ID: "E1-old-2", Boot: "old", Car: "E1", Stage: StageExiting, Origin: 1, Dir: 1, Stops: []int{3, 5}, Floor: 3,
		Riders: []Rider{
			{Robot: "p", Dest: 5, Exit: 5, Board: 1, Slot: 0, Status: RiderInside, Accepted: true},
			{Robot: "q", Dest: 3, Exit: 3, Board: 0, Slot: 1, Status: RiderInside, Accepted: true},
		}, N: 2}
	h2.take(h2.car.Restore(tr, h2.now))
	h2.state(3, DoorOpen, "E1-old-2")
	if len(h2.link.turns) != 0 {
		t.Fatalf("q got the door behind p: %q", h2.turnLog())
	}
	if q := h2.trip().rider("q"); q.Exit != 5 {
		t.Fatalf("q %+v, want carried to 5 behind p", q)
	}
}

func TestReviewLostTurnAndAnswerRepaired(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	tn := *h.trip().Turn
	h.live(6*time.Second, 1, 0) // turn lost, or its answer lost
	same := 0
	for _, x := range h.link.turns {
		if x.N == tn.N {
			same++
		}
	}
	if same < 2 || h.trip().Turn.N != tn.N {
		t.Fatalf("turn %d published %d times", tn.N, same)
	}
	h.answer(ZoneClear)
	if h.trip().Stage != StageTravel {
		t.Fatalf("stage %s", h.trip().Stage)
	}
}

func TestReviewStateDeduplicated(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.seq = 10
	h.open(1)
	h.answer(ZoneClear) // a boards; the car leaves for 3
	h.take(h.car.State(State{Boot: "c", Seq: 5, Floor: 3, Door: DoorOpen, Trip: h.trip().ID}, h.now))
	if h.trip().Stage != StageTravel {
		t.Fatalf("old state taken: stage %s", h.trip().Stage)
	}
	h.take(h.car.State(State{Boot: "c2", Seq: 1, Floor: 3, Door: DoorOpen, Trip: h.trip().ID}, h.now))
	if h.trip().Stage != StageExiting {
		t.Fatalf("new car boot ignored: stage %s", h.trip().Stage)
	}
}

func TestReviewLeaseAbortsUnboardedTrip(t *testing.T) {
	h := newCarH(t)
	tm := DefaultTiming()
	tm.AcceptTimeout, tm.CallMargin, tm.LeaseMargin = time.Hour, time.Hour, time.Second
	h.car = NewCar(testCar, tm, "b0", h.link, func(kind, robot, trip, detail string) {
		h.events = append(h.events, kind+" "+robot+": "+detail)
	}, nil)
	h.state(9, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(tm.BatchWindow)
	lease := h.trip().Lease
	for h.now.Before(lease) {
		h.tick(time.Second)
		h.state(9, DoorClosed, "") // alive, never comes
	}
	h.tick(250 * time.Millisecond)
	if h.car.trip != nil || !strings.Contains(strings.Join(h.events, "|"), "lease expired before boarding") {
		t.Fatalf("trip %+v events %v", h.car.trip, h.events)
	}
	if h.countNotices(NoticeAborted) != 1 {
		t.Fatalf("notices %+v", h.notices)
	}
}

// Lease expiry with a robot inside: its task is handed on (stuck notice,
// once) and the car stays reserved.
func TestReviewLeaseSettlesRobotInside(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	h.answer(ZoneClear)
	h.tick(h.trip().Lease.Sub(h.now) + time.Minute) // never arrives
	if h.car.trip == nil || h.countNotices(NoticeStuck) != 1 {
		t.Fatalf("trip %+v notices %+v", h.car.trip, h.notices)
	}
}

// account: a robot ONLINE is out of the car only if it was heard after its
// turn began (a report from before boarding proves nothing).
func TestReviewAccountNeedsReportAfterTurn(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	turn := h.now
	h.answer(ZoneClear)
	h.robots["a"] = fleet.Robot{Name: "a", Conn: fleet.Online, Mode: "IDLE", HasPose: true, Floor: 1, LastSeen: turn.Add(-time.Second)}
	h.tick(time.Second)
	if st := h.trip().rider("a").Status; st != RiderInside {
		t.Fatalf("a %s from a report made before its turn", st)
	}
	h.robots["a"] = fleet.Robot{Name: "a", Conn: fleet.Online, Mode: "IDLE", HasPose: true, Floor: 3, LastSeen: h.now}
	h.tick(250 * time.Millisecond)
	if h.car.trip != nil {
		t.Fatal("not settled by a report after its turn")
	}
}

func TestReviewZoneValuesValidated(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	tn := *h.trip().Turn
	for _, z := range []Zone{
		{Trip: tn.Trip, N: tn.N, Robot: "a", Action: "jump", Event: ZoneFailed},
		{Trip: tn.Trip, N: tn.N, Robot: "a", Action: ActBoard, Event: "maybe"},
		{Trip: tn.Trip, N: tn.N, Robot: "a", Action: ActExit, Event: ZoneFailed}, // not this turn's action
		{Trip: tn.Trip, N: tn.N, Robot: "a", Action: ActBoard, Event: ZoneEnter},
	} {
		h.take(h.car.Zone(z, h.now))
	}
	if r := h.trip().rider("a"); r.Status != RiderBoarding || r.Tries != 0 || h.trip().Turn.N != tn.N {
		t.Fatalf("a %+v turn %+v after invalid answers", r, h.trip().Turn)
	}
}

func TestReviewSameFloorWaiterNeedsNoRide(t *testing.T) {
	h := newCarH(t)
	h.car.Wait("a", 4, 4, "ca", h.now)
	h.tick(250 * time.Millisecond)
	if h.car.Waiting("a") || len(h.notices) != 1 || h.notices[0].Kind != NoticeNoRide {
		t.Fatalf("notices %+v", h.notices)
	}
}

// The retained trip: a failed publish is retried, the trip is re-published
// periodically, the retained message is cleared when the trip ends, and an
// echo of an ended trip is never adopted again.
func TestReviewTripPublishRetriedAndCleared(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.link.fail = 2
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	if len(h.link.trips) != 0 {
		t.Fatal("publish should have failed")
	}
	h.tick(500 * time.Millisecond)
	if len(h.link.trips) == 0 {
		t.Fatal("failed trip publish not retried")
	}
	h.acceptAll()
	n := len(h.link.trips)
	h.live(DefaultTiming().Republish+time.Second, 9, 0)
	if len(h.link.trips) == n {
		t.Fatal("trip not re-published periodically")
	}
	h.open(1)
	h.answer(ZoneClear)
	h.open(3)
	h.answer(ZoneClear)
	if h.car.trip != nil || h.link.clears != 1 {
		t.Fatalf("trip %+v clears %d", h.car.trip, h.link.clears)
	}
	for _, echo := range h.link.trips {
		echo.Boot = "older"
		if ns := h.car.Restore(echo, h.now); h.car.trip != nil || len(ns) != 0 {
			t.Fatalf("ended trip re-adopted from its %s echo", echo.Stage)
		}
	}
}

func TestReviewForceRelease(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.acceptAll()
	h.open(1)
	h.answer(ZoneClear) // b inside; c, a waiting
	ok, ns := h.car.ForceRelease(h.now, "operator")
	if !ok || h.car.trip != nil || h.link.clears != 1 || !strings.HasSuffix(h.cmdLog(), "release@1") {
		t.Fatalf("ok %v trip %+v clears %d cmds %q", ok, h.car.trip, h.link.clears, h.cmdLog())
	}
	kinds := map[string]string{}
	for _, n := range ns {
		kinds[n.Robot] = n.Kind
	}
	if kinds["b"] != NoticeReleased || kinds["c"] != NoticeReleased || kinds["a"] != NoticeAborted { // c has its boarding turn
		t.Fatalf("notices %+v", ns)
	}
	if ok, _ := h.car.ForceRelease(h.now, "operator"); ok {
		t.Fatal("released a car with no trip")
	}
}

func TestReviewParseTimingRules(t *testing.T) {
	for _, bad := range []string{
		`{"timing":{"turn_timeout_s":0}}`,
		`{"timing":{"hold_refresh_s":15,"hold_for_s":15}}`,
		`{"timing":{"hold_refresh_s":20}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Parse([]byte(`{"timing":{"hold_refresh_s":2,"hold_for_s":6}}`)); err != nil {
		t.Fatal(err)
	}
}
