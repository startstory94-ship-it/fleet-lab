package elevator

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

type fakeLink struct {
	cmds   []Cmd
	trips  []Trip
	turns  []Turn
	clears int
	fail   int // next PublishTrip calls that fail
}

func (l *fakeLink) Command(car string, c Cmd) error { l.cmds = append(l.cmds, c); return nil }
func (l *fakeLink) PublishTrip(car string, t Trip) error {
	if l.fail > 0 {
		l.fail--
		return errors.New("broker away")
	}
	l.trips = append(l.trips, t)
	return nil
}
func (l *fakeLink) PublishTurn(car string, t Turn) error { l.turns = append(l.turns, t); return nil }
func (l *fakeLink) ClearTrip(car string) error           { l.clears++; return nil }

// carH drives one Car on a fake clock. Car: floors 1-10, three slots,
// 1 s per floor, 1 s door.
type carH struct {
	t       *testing.T
	now     time.Time
	link    *fakeLink
	car     *Car
	events  []string // "kind robot: detail"
	notices []Notice
	seq     uint64
	robots  map[string]fleet.Robot
}

var testCar = CarConfig{ID: "E1", Capacity: 3, Floors: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	Lobby: Point{X: 10}, FloorS: 1, DoorS: 1}

func newCarH(t *testing.T) *carH {
	h := &carH{t: t, now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), link: &fakeLink{}, robots: map[string]fleet.Robot{}}
	h.car = NewCar(testCar, DefaultTiming(), "b0", h.link, func(kind, robot, trip, detail string) {
		h.events = append(h.events, kind+" "+robot+": "+detail)
	}, nil)
	return h
}

func (h *carH) lookup(name string) (fleet.Robot, bool) {
	r, ok := h.robots[name]
	return r, ok
}

func (h *carH) take(ns []Notice) { h.notices = append(h.notices, ns...) }

// tick advances the clock by d in 250 ms steps, ticking each time.
func (h *carH) tick(d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); {
		h.now = h.now.Add(250 * time.Millisecond)
		h.take(h.car.Tick(h.now, h.lookup))
	}
}

func (h *carH) trip() *Trip {
	h.t.Helper()
	if h.car.trip == nil {
		h.t.Fatal("no trip")
	}
	return h.car.trip
}

// state sends a car state (door zone empty).
func (h *carH) state(floor int, door string, trip string) { h.stateZ(floor, door, trip, 0) }

// stateZ sends a car state with zone robots in the door zone.
func (h *carH) stateZ(floor int, door string, trip string, zone int) {
	h.seq++
	h.take(h.car.State(State{Boot: "c", Seq: h.seq, Floor: floor, Door: door, Trip: trip, DoorZone: zone}, h.now))
}

// live advances d with the car reporting every second: door open at floor
// for the current trip, zone robots in the door zone.
func (h *carH) live(d time.Duration, floor, zone int) {
	for end := h.now.Add(d); h.now.Before(end); {
		h.tick(time.Second)
		trip := ""
		if h.car.trip != nil {
			trip = h.car.trip.ID
		}
		h.stateZ(floor, DoorOpen, trip, zone)
	}
}

// open: the car is at floor with the door open for the current trip.
func (h *carH) open(floor int) { h.state(floor, DoorOpen, h.trip().ID) }

// answer replies to the current turn.
func (h *carH) answer(event string) {
	h.t.Helper()
	tn := h.trip().Turn
	if tn == nil {
		h.t.Fatal("no turn to answer")
	}
	h.take(h.car.Zone(Zone{Trip: tn.Trip, N: tn.N, Robot: tn.Robot, Action: tn.Action, Event: event, Floor: tn.Floor}, h.now))
	h.state(tn.Floor, DoorOpen, tn.Trip) // the car: door zone empty again
}

func (h *carH) turnLog() string {
	var b []string
	seen := map[int]bool{}
	for _, t := range h.link.turns {
		if seen[t.N] {
			continue // re-sent with the same n
		}
		seen[t.N] = true
		b = append(b, fmt.Sprintf("%s:%s@%d", t.Robot, t.Action, t.Floor))
	}
	return strings.Join(b, " ")
}

func (h *carH) cmdLog() string {
	var b []string
	for _, c := range h.link.cmds {
		b = append(b, fmt.Sprintf("%s@%d", c.Cmd, c.Floor))
	}
	return strings.Join(b, " ")
}

func (h *carH) count(kind string) int {
	n := 0
	for _, e := range h.events {
		if strings.HasPrefix(e, kind+" ") {
			n++
		}
	}
	return n
}

// plan3 queues a(3) b(7) c(5) at floor 1 and ticks past the window.
func (h *carH) plan3() {
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.car.Wait("b", 1, 7, "cb", h.now)
	h.car.Wait("c", 1, 5, "cc", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.trip()
}

func (h *carH) acceptAll() {
	for _, r := range h.trip().Riders {
		h.take(h.car.Accepted(r.Robot, h.now))
	}
}

func TestCarBatchWindow(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(2 * time.Second)
	h.car.Wait("b", 1, 5, "cb", h.now) // inside the window: rides with a
	h.tick(750 * time.Millisecond)
	if h.car.trip != nil {
		t.Fatalf("planned at +2.75 s, before the 3 s window")
	}
	h.tick(250 * time.Millisecond)
	tr := h.trip()
	if got := tr.Riders; len(got) != 2 || got[0].Robot != "b" || got[1].Robot != "a" {
		t.Fatalf("riders %+v, want b then a", got)
	}
	h.car.Wait("c", 1, 4, "cc", h.now) // after planning: next trip
	if tr.rider("c") != nil || !h.car.Waiting("c") {
		t.Fatalf("late robot joined the planned trip")
	}
	var planned []string
	for _, n := range h.notices {
		if n.Kind == NoticePlanned {
			planned = append(planned, n.Robot+"="+n.CmdID)
		}
	}
	if strings.Join(planned, " ") != "b=cb a=ca" {
		t.Fatalf("planned notices %v", planned)
	}
	if h.cmdLog() != "call@1" || h.count("called") != 1 || h.count("trip_planned") != 1 {
		t.Fatalf("cmds %q events %v", h.cmdLog(), h.events)
	}
}

// Nobody gets a turn before every rider accepted and the car is open at
// the origin.
func TestCarAllAcceptBeforeBoarding(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.open(1)
	h.take(h.car.Accepted("a", h.now))
	h.take(h.car.Accepted("b", h.now))
	h.tick(4 * time.Second)
	h.open(1) // the car keeps publishing
	if len(h.link.turns) != 0 || h.trip().Stage != StagePlanned {
		t.Fatalf("turns %q before c accepted", h.turnLog())
	}
	h.take(h.car.Accepted("c", h.now))
	if h.turnLog() != "b:board@1" {
		t.Fatalf("turns %q, want b (to 7) boarding first", h.turnLog())
	}
	// Accepted but the car is not there yet: no turn either.
	h2 := newCarH(t)
	h2.state(4, DoorClosed, "")
	h2.plan3()
	h2.acceptAll()
	h2.state(2, DoorClosed, h2.trip().ID)
	h2.tick(time.Second)
	h2.state(1, DoorOpening, h2.trip().ID)
	if len(h2.link.turns) != 0 {
		t.Fatalf("turn before the door was open: %q", h2.turnLog())
	}
	h2.open(1)
	if h2.turnLog() != "b:board@1" {
		t.Fatalf("turns %q", h2.turnLog())
	}
}

func TestCarAbortOnMissingAccept(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.open(1)
	h.take(h.car.Accepted("a", h.now))
	h.take(h.car.Accepted("b", h.now))
	for h.now.Before(h.trip().Planned.Add(DefaultTiming().AcceptTimeout - 250*time.Millisecond)) {
		h.tick(250 * time.Millisecond)
		h.state(1, DoorOpen, h.trip().ID) // the car is fine
	}
	if h.car.trip == nil {
		t.Fatal("aborted before AcceptTimeout")
	}
	h.tick(250 * time.Millisecond)
	if h.car.trip != nil {
		t.Fatalf("not aborted at AcceptTimeout: %+v", h.car.trip)
	}
	if len(h.link.turns) != 0 {
		t.Fatalf("a robot got a turn: %q", h.turnLog())
	}
	got := map[string]bool{}
	for _, n := range h.notices {
		if n.Kind == NoticeAborted {
			got[n.Robot] = n.Fault
		}
	}
	if len(got) != 3 || !got["c"] || got["a"] || got["b"] {
		t.Fatalf("aborted notices %v, want all three, fault only c", got)
	}
	if !strings.HasSuffix(h.cmdLog(), "release@1") {
		t.Fatalf("cmds %q, want a release", h.cmdLog())
	}
	if last := h.link.trips[len(h.link.trips)-1]; last.Stage != StageAborted {
		t.Fatalf("last trip message %s", last.Stage)
	}
	// Not the car's fault: the robots queue again and ride right away.
	h.car.Wait("a", 1, 3, "ca2", h.now)
	h.tick(DefaultTiming().BatchWindow)
	if h.car.trip == nil {
		t.Fatal("no new trip after an abort that was not the car's fault")
	}
}

func TestCarAbortWhenElevatorSilent(t *testing.T) {
	h := newCarH(t) // never heard
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.tick(DefaultTiming().Silence - 250*time.Millisecond)
	if h.car.trip == nil {
		t.Fatal("aborted before Silence")
	}
	h.tick(250 * time.Millisecond)
	if h.car.trip != nil || h.count("aborted") != 1 || !strings.Contains(h.events[len(h.events)-1], "not responding") {
		t.Fatalf("trip %+v events %v", h.car.trip, h.events)
	}
	if len(h.link.turns) != 0 {
		t.Fatal("turn with a silent car")
	}
	for _, n := range h.notices {
		if n.Kind == NoticeAborted && n.Fault {
			t.Fatalf("robot blamed for the car: %+v", n)
		}
	}
	// Back-off: no trip for Timing.Backoff.
	h.car.Wait("a", 1, 3, "ca2", h.now)
	h.tick(DefaultTiming().Backoff - 250*time.Millisecond)
	if h.car.trip != nil {
		t.Fatal("planned during the back-off")
	}
	h.tick(250 * time.Millisecond)
	if h.car.trip == nil {
		t.Fatal("no trip after the back-off")
	}
}

func TestCarAbortWhenCarNeverOpens(t *testing.T) {
	h := newCarH(t)
	h.state(9, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	arrive := h.car.arriveBy
	// The car keeps publishing (not silent) but stays at 9.
	for h.now.Before(arrive.Add(-250 * time.Millisecond)) {
		h.tick(250 * time.Millisecond)
		h.state(9, DoorClosed, "")
	}
	if h.car.trip == nil {
		t.Fatal("aborted early")
	}
	if n := strings.Count(h.cmdLog(), "call@1"); n < 2 {
		t.Fatalf("call not re-sent: %q", h.cmdLog())
	}
	first := h.link.cmds[0].CmdID
	for _, c := range h.link.cmds {
		if c.Cmd == CmdCall && c.CmdID != first {
			t.Fatalf("re-sent call has a new cmd_id %s (first %s)", c.CmdID, first)
		}
	}
	h.tick(250 * time.Millisecond)
	if h.car.trip != nil || !strings.Contains(h.events[len(h.events)-1], "not open at floor 1") {
		t.Fatalf("events %v", h.events)
	}
}

// One robot at a time: the next turn only after the previous robot's zone
// clear (or failed), or after TurnTimeout. Stale or foreign answers do not
// count.
func TestCarOneAtATime(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.acceptAll()
	h.open(1)
	if h.turnLog() != "b:board@1" {
		t.Fatalf("turns %q", h.turnLog())
	}
	tn := *h.trip().Turn
	// Answers that are not for this turn change nothing.
	h.take(h.car.Zone(Zone{Trip: tn.Trip, N: tn.N - 1, Robot: "b", Action: ActBoard, Event: ZoneClear}, h.now))
	h.take(h.car.Zone(Zone{Trip: tn.Trip, N: tn.N, Robot: "c", Action: ActBoard, Event: ZoneClear}, h.now))
	h.take(h.car.Zone(Zone{Trip: "other", N: tn.N, Robot: "b", Action: ActBoard, Event: ZoneClear}, h.now))
	h.live(14*time.Second, 1, 1) // b in the door zone
	if h.turnLog() != "b:board@1" {
		t.Fatalf("next robot moved before b cleared: %q", h.turnLog())
	}
	if n := len(h.link.turns); n < 3 {
		t.Fatalf("open turn not re-sent (same n) while waiting: %d publishes", n)
	}
	h.answer(ZoneClear)
	if h.turnLog() != "b:board@1 c:board@1" {
		t.Fatalf("turns %q", h.turnLog())
	}
	// c never answers and something stays in the door zone: past its
	// timeout, nobody else moves; c is retried only once the zone is empty.
	h.live(40*time.Second, 1, 1)
	if h.turnLog() != "b:board@1 c:board@1" {
		t.Fatalf("a robot got the door while the zone was occupied: %q", h.turnLog())
	}
	h.live(time.Second, 1, 0)
	if h.turnLog() != "b:board@1 c:board@1 c:board@1" {
		t.Fatalf("turns %q, want c retried", h.turnLog())
	}
	// No answer again, zone empty: c is treated as inside, a boards.
	h.live(DefaultTiming().TurnTimeout, 1, 0)
	if h.turnLog() != "b:board@1 c:board@1 c:board@1 a:board@1" || h.trip().rider("c").Status != RiderUnsureIn {
		t.Fatalf("turns %q, c %s", h.turnLog(), h.trip().rider("c").Status)
	}
	if h.count("overdue") != 2 {
		t.Fatalf("events %v", h.events)
	}
	h.answer(ZoneClear)
	// Boarding done: the car goes to 3 (a), then exits by slot.
	if !strings.HasSuffix(h.cmdLog(), "goto@3") || h.trip().Stage != StageTravel {
		t.Fatalf("cmds %q stage %s", h.cmdLog(), h.trip().Stage)
	}
	h.tick(2 * time.Second)
	if strings.Count(h.turnLog(), ":") != 4 {
		t.Fatal("turn while travelling")
	}
	h.open(3)
	h.answer(ZoneClear)
	h.state(3, DoorClosed, h.trip().ID)
	h.tick(time.Second)
	h.open(5)
	if !strings.HasSuffix(h.turnLog(), "a:exit@3 c:exit@5") {
		t.Fatalf("turns %q", h.turnLog())
	}
	h.answer(ZoneClear) // late-boarded c is out after all
	h.open(7)
	h.answer(ZoneClear)
	if h.car.trip != nil || !strings.HasSuffix(h.cmdLog(), "release@7") {
		t.Fatalf("trip %+v cmds %q", h.car.trip, h.cmdLog())
	}
	if h.turnLog() != "b:board@1 c:board@1 c:board@1 a:board@1 a:exit@3 c:exit@5 b:exit@7" {
		t.Fatalf("turns %q", h.turnLog())
	}
}

// The door is held while robots move: hold re-sent every HoldRefresh during
// a gate, no release until the last robot is out, then exactly one.
func TestCarDoorHoldAndRelease(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.acceptAll()
	h.open(1)
	h.link.cmds = nil
	h.live(12*time.Second, 1, 1) // b moving slowly
	holds := strings.Count(h.cmdLog(), "hold@1")
	if holds != 2 {
		t.Fatalf("%d holds in 12 s of boarding (%q), want 2 (every 5 s)", holds, h.cmdLog())
	}
	for _, step := range []func(){
		func() { h.answer(ZoneClear) }, func() { h.answer(ZoneClear) }, func() { h.answer(ZoneClear) },
		func() { h.open(3) }, func() { h.answer(ZoneClear) },
		func() { h.open(5) }, func() { h.answer(ZoneClear) },
		func() { h.open(7) },
	} {
		step()
		if strings.Contains(h.cmdLog(), "release") {
			t.Fatalf("released while robots still move: %q", h.cmdLog())
		}
	}
	h.answer(ZoneClear)
	if n := strings.Count(h.cmdLog(), "release"); n != 1 {
		t.Fatalf("%d releases: %q", n, h.cmdLog())
	}
	if h.count("released") != 1 || h.count("departed") != 3 || h.count("stop") != 3 {
		t.Fatalf("events %v", h.events)
	}
}

// A failed exit is retried; a second failure carries the robot, and the
// robots behind it at that stop, to the next stop. There they exit first.
func TestCarExitRetryThenCarry(t *testing.T) {
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
	h.answer(ZoneFailed) // a, try 1
	if !strings.HasSuffix(h.turnLog(), "a:exit@3 a:exit@3") {
		t.Fatalf("no retry: %q", h.turnLog())
	}
	h.answer(ZoneFailed) // a, try 2: carried, with x behind it
	if a, x := h.trip().rider("a"), h.trip().rider("x"); a.Exit != 7 || x.Exit != 7 || a.Status != RiderInside || x.Status != RiderInside {
		t.Fatalf("a %+v x %+v, want both carried to 7", a, x)
	}
	if h.trip().Stage != StageTravel || !strings.HasSuffix(h.cmdLog(), "goto@7") {
		t.Fatalf("stage %s cmds %q", h.trip().Stage, h.cmdLog())
	}
	h.open(7)
	h.answer(ZoneClear)
	h.answer(ZoneClear)
	h.answer(ZoneClear)
	if !strings.HasSuffix(h.turnLog(), "a:exit@7 x:exit@7 b:exit@7") {
		t.Fatalf("turns %q: the carried robots are at the door and go first", h.turnLog())
	}
	if h.count("replanned") != 2 || h.count("exit_failed") != 2 || h.car.trip != nil {
		t.Fatalf("events %v", h.events)
	}
}

// At the last stop there is nowhere to carry the robot: it is stuck, its
// task must go elsewhere (notice), the car stays reserved until the robot
// is seen out, and the lease alarm fires meanwhile.
func TestCarStuckAtLastStop(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	h.answer(ZoneClear)
	h.open(3)
	h.answer(ZoneFailed)
	h.answer(ZoneFailed)
	tr := h.trip()
	if tr.Stage != StageBlocked || tr.rider("a").Status != RiderStuck {
		t.Fatalf("trip %+v", tr)
	}
	stuck := false
	for _, n := range h.notices {
		stuck = stuck || n.Kind == NoticeStuck && n.Robot == "a"
	}
	if !stuck || !strings.HasSuffix(h.cmdLog(), "release@3") {
		t.Fatalf("notices %+v cmds %q", h.notices, h.cmdLog())
	}
	// Another robot queues; the car takes no trip while blocked.
	h.car.Wait("z", 1, 4, "cz", h.now)
	h.robots["a"] = fleet.Robot{Name: "a", Conn: fleet.InElevator, Mode: "RIDING", HasPose: true, Floor: 3}
	h.tick(5 * time.Minute)
	if h.trip().ID != tr.ID || h.count("trip_planned") != 1 {
		t.Fatalf("car not kept for the stuck robot")
	}
	if h.count("overdue") < 2 { // blocked + lease
		t.Fatalf("no lease alarm: %v", h.events)
	}
	// Out (a human helped): back on the fleet link at floor 3.
	h.robots["a"] = fleet.Robot{Name: "a", Conn: fleet.Online, Mode: "IDLE", HasPose: true, Floor: 3, LastSeen: h.now}
	h.tick(250 * time.Millisecond)
	if h.count("released") != 1 {
		t.Fatalf("not released after a was seen out: %v", h.events)
	}
	h.tick(DefaultTiming().BatchWindow)
	if h.count("trip_planned") != 2 {
		t.Fatal("waiting robot not served after the car was freed")
	}
}

// Travel never ends: overdue alarm, lease alarm, car kept; it is freed only
// once the robots are accounted out.
func TestCarLeaseFreesOnlyWhenSafe(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.car.Wait("a", 1, 3, "ca", h.now)
	h.tick(DefaultTiming().BatchWindow)
	h.acceptAll()
	h.open(1)
	h.answer(ZoneClear)
	lease := h.trip().Lease
	h.tick(lease.Sub(h.now) + time.Second)
	if h.car.trip == nil {
		t.Fatal("car freed with a robot inside")
	}
	var travelLate, leaseLate bool
	for _, e := range h.events {
		travelLate = travelLate || strings.HasPrefix(e, "overdue") && strings.Contains(e, "not open at floor 3")
		leaseLate = leaseLate || strings.HasPrefix(e, "overdue") && strings.Contains(e, "lease expired")
	}
	if !travelLate || !leaseLate {
		t.Fatalf("events %v", h.events)
	}
	if strings.Contains(h.cmdLog(), "release") {
		t.Fatalf("released with a robot inside: %q", h.cmdLog())
	}
	h.take(h.car.Out("a", 2, h.now)) // came out somehow, reported after reconnect
	if h.car.trip != nil || h.count("released") != 1 {
		t.Fatalf("not freed once safe: %v", h.events)
	}
}

// A restarted fleetd picks a trip up from its retained message: the current
// turn is published again with the same n, so a robot that already
// answered answers again, and the trip goes on.
func TestCarRestoreMidTrip(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	h.acceptAll()
	h.open(1)
	h.answer(ZoneClear)
	h.answer(ZoneClear)
	saved := h.link.trips[len(h.link.trips)-1] // c is boarding (turn 2)
	if saved.Turn == nil || saved.Turn.Robot != "a" {
		t.Fatalf("saved turn %+v", saved.Turn)
	}

	h2 := newCarH(t)
	h2.car = NewCar(testCar, DefaultTiming(), "b1", h2.link, func(kind, robot, trip, detail string) {
		h2.events = append(h2.events, kind+" "+robot+": "+detail)
	}, nil)
	h2.take(h2.car.Restore(saved, h2.now))
	if len(h2.link.turns) != 0 {
		t.Fatalf("turn published before the door was seen open: %+v", h2.link.turns)
	}
	if h2.cmdLog() != "goto@1" {
		t.Fatalf("door not reopened after restore: %q", h2.cmdLog())
	}
	if tr := h2.link.trips[len(h2.link.trips)-1]; tr.Boot != "b1" {
		t.Fatalf("adopted trip keeps boot %q", tr.Boot)
	}
	h2.state(1, DoorClosed, saved.ID)
	if len(h2.link.turns) != 0 {
		t.Fatal("turn published with the door closed")
	}
	h2.state(1, DoorOpen, saved.ID)
	if len(h2.link.turns) != 1 || h2.link.turns[0].N != saved.Turn.N || h2.link.turns[0].Robot != "a" {
		t.Fatalf("turn not republished with the same n once open: %+v", h2.link.turns)
	}
	h2.answer(ZoneClear)
	if h2.trip().Stage != StageTravel || !strings.HasSuffix(h2.cmdLog(), "goto@3") {
		t.Fatalf("stage %s cmds %q", h2.trip().Stage, h2.cmdLog())
	}
	for _, c := range h2.link.cmds {
		if !strings.HasSuffix(c.CmdID, ".b1") {
			t.Fatalf("cmd_id %s does not carry the new boot", c.CmdID)
		}
	}

	// A trip nobody boarded is aborted on restore.
	h3 := newCarH(t)
	h3.state(1, DoorClosed, "")
	h3.plan3()
	planned := h3.link.trips[len(h3.link.trips)-1]
	h4 := newCarH(t)
	h4.take(h4.car.Restore(planned, h4.now))
	if h4.car.trip != nil || len(h4.notices) != 3 || h4.notices[0].Kind != NoticeAborted || h4.notices[0].Fault ||
		!strings.Contains(h4.notices[0].Reason, "restarted before boarding") {
		t.Fatalf("planned trip not aborted on restore: %+v", h4.notices)
	}
}

func TestCarLateAnswersAndDrop(t *testing.T) {
	h := newCarH(t)
	h.state(1, DoorClosed, "")
	h.plan3()
	if ok, _ := h.car.Drop("a", "battery", h.now); !ok {
		t.Fatal("waiting rider could not be dropped")
	}
	h.acceptAll()
	h.open(1)
	tn := *h.trip().Turn                        // b
	h.live(2*DefaultTiming().TurnTimeout, 1, 0) // two unanswered tries
	if h.trip().rider("b").Status != RiderUnsureIn {
		t.Fatalf("b %s", h.trip().rider("b").Status)
	}
	if ok, _ := h.car.Drop("b", "x", h.now); ok {
		t.Fatal("dropped a robot that may be in the car")
	}
	h.take(h.car.Zone(Zone{Trip: tn.Trip, N: tn.N, Robot: "b", Action: ActBoard, Event: ZoneClear}, h.now))
	if h.trip().rider("b").Status != RiderInside {
		t.Fatalf("late clear not taken: %s", h.trip().rider("b").Status)
	}
	h.answer(ZoneClear) // c
	if h.turnLog() != "b:board@1 b:board@1 c:board@1" || h.trip().Stage != StageTravel {
		t.Fatalf("turns %q stage %s; dropped a must get no turn", h.turnLog(), h.trip().Stage)
	}
	// Boarding turn timed out and the robot is seen at the lobby: it did not board.
	h2 := newCarH(t)
	h2.state(1, DoorClosed, "")
	h2.car.Wait("a", 1, 3, "ca", h2.now)
	h2.car.Wait("b", 1, 5, "cb", h2.now)
	h2.tick(DefaultTiming().BatchWindow)
	h2.acceptAll()
	h2.open(1)
	h2.live(2*DefaultTiming().TurnTimeout, 1, 0) // b silent twice
	h2.answer(ZoneClear)                         // a boarded
	h2.robots["b"] = fleet.Robot{Name: "b", Conn: fleet.Online, Mode: "IDLE", HasPose: true, Floor: 1, LastSeen: h2.now}
	h2.tick(250 * time.Millisecond)
	if st := h2.trip().rider("b").Status; st != RiderLeft {
		t.Fatalf("b %s, want left", st)
	}
	left := false
	for _, n := range h2.notices {
		left = left || n.Kind == NoticeLeft && n.Robot == "b" && !n.Fault
	}
	if !left {
		t.Fatalf("notices %+v", h2.notices)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := Parse([]byte(`{"elevators":[{"id":"A","floors":[1,2,3],"lobby":{"x":1,"y":2},"lobbies":{"3":{"x":5,"y":6}}}],
		"timing":{"batch_window_s":1.5,"exit_retries":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Elevators[0]
	if c.Capacity != 3 || c.FloorS != 2 || c.LobbyAt(1) != (Point{1, 2}) || c.LobbyAt(3) != (Point{5, 6}) || !c.Serves(2) || c.Serves(4) {
		t.Fatalf("car %+v", c)
	}
	if cfg.Timing.BatchWindow != 1500*time.Millisecond || cfg.Timing.ExitRetries != 2 || cfg.Timing.TurnTimeout != DefaultTiming().TurnTimeout {
		t.Fatalf("timing %+v", cfg.Timing)
	}
	for _, bad := range []string{
		`{"elevators":[{"floors":[1,2],"lobby":{}}]}`,
		`{"elevators":[{"id":"A","floors":[1],"lobby":{}}]}`,
		`{"elevators":[{"id":"A","floors":[1,2]}]}`,
		`{"elevators":[{"id":"A","floors":[1,2],"lobby":{},"capacity":-1}]}`,
		`{"elevators":[{"id":"A","floors":[1,2],"lobby":{}},{"id":"A","floors":[1,2],"lobby":{}}]}`,
		`{"elevators":[{"id":"A","floors":[1,2],"lobby":{},"lobbies":{"x":{}}}]}`,
		`{"timing":{"turn_timeout_s":-1}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if c, _ := Load(""); len(c.Elevators) != 1 || c.Elevators[0].ID != "E1" {
		t.Fatalf("default %+v", c)
	}
	if c, _ := Load("none"); len(c.Elevators) != 0 {
		t.Fatalf("none %+v", c)
	}
}
