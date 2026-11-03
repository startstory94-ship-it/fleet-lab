package elevator

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// seats renders a plan as "robot:board/slot@dest ..." in boarding order.
func seats(p Plan) string {
	var b []string
	for _, s := range p.Seats {
		b = append(b, fmt.Sprintf("%s:%d/%d@%d", s.Robot, s.Board, s.Slot, s.Dest))
	}
	return strings.Join(b, " ")
}

func deferred(p Plan) string {
	var b []string
	for _, d := range p.Deferred {
		b = append(b, d.Robot+"="+d.Reason)
	}
	return strings.Join(b, " ")
}

func TestPlanTable(t *testing.T) {
	w := func(robot string, dest int) Want { return Want{robot, dest} }
	cases := []struct {
		name     string
		req      Request
		seats    string // boarding order: robot:board/slot@dest
		stops    []int
		dir      int
		deferred string
	}{
		{
			name:  "3,7,5 from 1: board 7,5,3; 3 at the door",
			req:   Request{Origin: 1, Dir: 1, Capacity: 3, Robots: []Want{w("a", 3), w("b", 7), w("c", 5)}},
			seats: "b:0/2@7 c:1/1@5 a:2/0@3", stops: []int{3, 5, 7}, dir: 1,
		},
		{
			name:  "input order does not matter",
			req:   Request{Origin: 1, Dir: 1, Capacity: 3, Robots: []Want{w("c", 5), w("b", 7), w("a", 3)}},
			seats: "b:0/2@7 c:1/1@5 a:2/0@3", stops: []int{3, 5, 7}, dir: 1,
		},
		{
			name:  "same floor pair: one stop, name decides the door",
			req:   Request{Origin: 1, Dir: 1, Capacity: 3, Robots: []Want{w("y", 4), w("x", 4)}},
			seats: "y:0/1@4 x:1/0@4", stops: []int{4}, dir: 1,
		},
		{
			name:  "capacity split: nearest ride, the rest wait",
			req:   Request{Origin: 1, Dir: 1, Capacity: 2, Robots: []Want{w("a", 9), w("b", 2), w("c", 6), w("d", 4)}},
			seats: "d:0/1@4 b:1/0@2", stops: []int{2, 4}, dir: 1, deferred: "a=capacity c=capacity",
		},
		{
			name:  "mixed directions: only the asked direction",
			req:   Request{Origin: 5, Dir: 1, Capacity: 4, Robots: []Want{w("a", 8), w("b", 2), w("c", 6), w("d", 1)}},
			seats: "a:0/1@8 c:1/0@6", stops: []int{6, 8}, dir: 1, deferred: "b=direction d=direction",
		},
		{
			name:  "mixed, no direction given: the majority",
			req:   Request{Origin: 5, Capacity: 4, Robots: []Want{w("a", 8), w("b", 2), w("d", 1)}},
			seats: "d:0/1@1 b:1/0@2", stops: []int{2, 1}, dir: -1, deferred: "a=direction",
		},
		{
			name:  "mixed tie, no direction: the nearest destination",
			req:   Request{Origin: 5, Capacity: 4, Robots: []Want{w("a", 9), w("b", 4)}},
			seats: "b:0/0@4", stops: []int{4}, dir: -1, deferred: "a=direction",
		},
		{
			name:  "going down reverses: 7 exits first",
			req:   Request{Origin: 9, Dir: -1, Capacity: 3, Robots: []Want{w("a", 3), w("b", 7), w("c", 5)}},
			seats: "a:0/2@3 c:1/1@5 b:2/0@7", stops: []int{7, 5, 3}, dir: -1,
		},
		{
			name:  "basement floors",
			req:   Request{Origin: 0, Dir: -1, Capacity: 3, Robots: []Want{w("a", -2), w("b", -1)}},
			seats: "a:0/1@-2 b:1/0@-1", stops: []int{-1, -2}, dir: -1,
		},
		{
			name: "no robots", req: Request{Origin: 1, Dir: 1, Capacity: 3},
			seats: "", stops: nil, dir: 1,
		},
		{
			name:  "one robot",
			req:   Request{Origin: 1, Dir: 1, Capacity: 3, Robots: []Want{w("a", 2)}},
			seats: "a:0/0@2", stops: []int{2}, dir: 1,
		},
		{
			name:  "destination is the origin: never boards",
			req:   Request{Origin: 3, Dir: 1, Capacity: 3, Robots: []Want{w("a", 3), w("b", 4)}},
			seats: "b:0/0@4", stops: []int{4}, dir: 1, deferred: "a=same_floor",
		},
		{
			name:  "only same floor",
			req:   Request{Origin: 3, Capacity: 3, Robots: []Want{w("a", 3)}},
			seats: "", stops: nil, dir: 1, deferred: "a=same_floor",
		},
		{
			name:  "robot listed twice: the first entry counts",
			req:   Request{Origin: 1, Dir: 1, Capacity: 3, Robots: []Want{w("a", 5), w("a", 2), w("b", 3)}},
			seats: "a:0/1@5 b:1/0@3", stops: []int{3, 5}, dir: 1, deferred: "a=duplicate",
		},
		{
			name:  "capacity zero",
			req:   Request{Origin: 1, Dir: 1, Capacity: 0, Robots: []Want{w("a", 5)}},
			seats: "", stops: nil, dir: 1, deferred: "a=capacity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanTrip(tc.req)
			if got := seats(p); got != tc.seats {
				t.Errorf("seats %q, want %q", got, tc.seats)
			}
			if !reflect.DeepEqual(p.Stops, tc.stops) {
				t.Errorf("stops %v, want %v", p.Stops, tc.stops)
			}
			if p.Dir != tc.dir {
				t.Errorf("dir %d, want %d", p.Dir, tc.dir)
			}
			if got := deferred(p); got != tc.deferred {
				t.Errorf("deferred %q, want %q", got, tc.deferred)
			}
		})
	}
}

func TestPlanTripsSplitsByCapacity(t *testing.T) {
	var robots []Want
	for i, d := range []int{9, 2, 6, 4, 8, 0, 3} {
		robots = append(robots, Want{fmt.Sprintf("r%d", i), d})
	}
	trips := PlanTrips(Request{Origin: 1, Dir: 1, Capacity: 2, Robots: robots})
	var got []string
	for _, p := range trips {
		got = append(got, seats(p))
	}
	want := []string{"r6:0/1@3 r1:1/0@2", "r2:0/1@6 r3:1/0@4", "r0:0/1@9 r4:1/0@8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trips %q, want %q", got, want)
	}
	if d := deferred(trips[len(trips)-1]); d != "r5=direction" {
		t.Fatalf("deferred %q, want r5=direction on the last trip", d)
	}
	for _, p := range trips[:len(trips)-1] {
		if len(p.Deferred) != 0 {
			t.Fatalf("deferred reported before the last trip: %v", p.Deferred)
		}
	}
}

// checkPlan asserts the planner's rules on one request and its plan.
func checkPlan(req Request, p Plan) error {
	if len(p.Seats) > max(req.Capacity, 0) {
		return fmt.Errorf("%d seats for capacity %d", len(p.Seats), req.Capacity)
	}
	if p.Dir != 1 && p.Dir != -1 {
		return fmt.Errorf("dir %d", p.Dir)
	}
	if req.Dir != 0 && p.Dir != sign(req.Dir) {
		return fmt.Errorf("dir %d, asked %d", p.Dir, req.Dir)
	}
	// Every robot entry boards exactly once or is deferred exactly once.
	count := map[string]int{}
	firstDest := map[string]int{}
	for _, w := range req.Robots {
		if _, ok := firstDest[w.Robot]; !ok {
			firstDest[w.Robot] = w.Dest
		}
	}
	entries := map[string]int{}
	for _, w := range req.Robots {
		entries[w.Robot]++
	}
	boards, slots := map[int]bool{}, map[int]bool{}
	for _, s := range p.Seats {
		count[s.Robot]++
		if s.Dest != firstDest[s.Robot] {
			return fmt.Errorf("%s rides to %d, wanted %d", s.Robot, s.Dest, firstDest[s.Robot])
		}
		if sign(s.Dest-req.Origin) != p.Dir {
			return fmt.Errorf("%s goes %d from %d in a trip going %d: mixed directions", s.Robot, s.Dest, req.Origin, p.Dir)
		}
		if boards[s.Board] || slots[s.Slot] || s.Board < 0 || s.Board >= len(p.Seats) || s.Slot < 0 || s.Slot >= len(p.Seats) {
			return fmt.Errorf("board %d / slot %d repeated or out of range", s.Board, s.Slot)
		}
		boards[s.Board], slots[s.Slot] = true, true
	}
	for _, d := range p.Deferred {
		count[d.Robot]++
	}
	for robot, n := range entries {
		if count[robot] != n {
			return fmt.Errorf("%s: %d entries, boards+deferred %d", robot, n, count[robot])
		}
		boarded := 0
		for _, s := range p.Seats {
			if s.Robot == robot {
				boarded++
			}
		}
		if boarded > 1 {
			return fmt.Errorf("%s boards %d times", robot, boarded)
		}
	}
	for _, d := range p.Deferred {
		if d.Reason == DeferSameFloor && d.Dest != req.Origin {
			return fmt.Errorf("%s deferred as same floor but goes to %d", d.Robot, d.Dest)
		}
	}
	for _, s := range p.Seats {
		if s.Dest == req.Origin {
			return fmt.Errorf("%s boards for its own floor", s.Robot)
		}
	}
	// Seats are listed in boarding order.
	for i, s := range p.Seats {
		if s.Board != i {
			return fmt.Errorf("seat %d has board %d", i, s.Board)
		}
	}
	// Stops are in travel order, distinct, and exactly the destinations.
	dests := map[int]bool{}
	for _, s := range p.Seats {
		dests[s.Dest] = true
	}
	if len(p.Stops) != len(dests) {
		return fmt.Errorf("stops %v for destinations %v", p.Stops, dests)
	}
	for i, s := range p.Stops {
		if !dests[s] {
			return fmt.Errorf("stop %d is nobody's destination", s)
		}
		if i > 0 && (s-p.Stops[i-1])*p.Dir <= 0 {
			return fmt.Errorf("stops %v not in travel order", p.Stops)
		}
	}
	// Ride the trip: at every stop, the robots getting out must be the ones
	// nearest the door among those still inside. Boarding must be the
	// reverse: everyone boarding later stands nearer the door.
	inside := map[string]Seat{}
	for _, s := range p.Seats {
		for _, o := range inside {
			if o.Slot < s.Slot {
				return fmt.Errorf("%s boards after %s but stands behind it", s.Robot, o.Robot)
			}
		}
		inside[s.Robot] = s
	}
	for _, stop := range p.Stops {
		maxOut, minStay := -1, len(p.Seats)
		for _, s := range inside {
			if s.Dest == stop {
				maxOut = max(maxOut, s.Slot)
			} else {
				minStay = min(minStay, s.Slot)
			}
		}
		if maxOut > minStay {
			return fmt.Errorf("at floor %d a robot in slot %d gets out past a robot staying in slot %d", stop, maxOut, minStay)
		}
		for n, s := range inside {
			if s.Dest == stop {
				delete(inside, n)
			}
		}
	}
	if len(inside) != 0 {
		return fmt.Errorf("robots never got out: %v", inside)
	}
	return nil
}

// TestPlanProperties checks the rules on random requests: exiting robots
// always at the front, capacity never exceeded, one direction, every robot
// boards once or is deferred, deterministic, and independent of input order.
func TestPlanProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 5000 {
		req := Request{Origin: rng.IntN(12) - 2, Dir: rng.IntN(3) - 1, Capacity: rng.IntN(6)}
		n := rng.IntN(9)
		for range n {
			req.Robots = append(req.Robots, Want{Robot: fmt.Sprintf("r%d", rng.IntN(10)), Dest: rng.IntN(12) - 2})
		}
		p := PlanTrip(req)
		if err := checkPlan(req, p); err != nil {
			t.Fatalf("case %d %+v: %v\nplan %+v", i, req, err, p)
		}
		if again := PlanTrip(req); !reflect.DeepEqual(p, again) {
			t.Fatalf("case %d: not deterministic:\n%+v\n%+v", i, p, again)
		}
		// Shuffling keeps the result when no robot is listed twice (with
		// duplicates, "the first entry counts" depends on order).
		if !hasDup(req.Robots) {
			shuf := req
			shuf.Robots = append([]Want(nil), req.Robots...)
			rng.Shuffle(len(shuf.Robots), func(a, b int) { shuf.Robots[a], shuf.Robots[b] = shuf.Robots[b], shuf.Robots[a] })
			if q := PlanTrip(shuf); !reflect.DeepEqual(p, q) {
				t.Fatalf("case %d: order changed the plan:\n%+v\n%+v", i, p, q)
			}
		}
		// The split into trips boards every robot of the direction exactly once.
		if req.Capacity > 0 {
			seen := map[string]bool{}
			for _, tp := range PlanTrips(req) {
				seatsOnly := tp
				seatsOnly.Deferred = nil
				if err := checkPlan(Request{Origin: req.Origin, Dir: tp.Dir, Capacity: req.Capacity, Robots: wantsOf(tp)}, seatsOnly); err != nil {
					t.Fatalf("case %d split: %v", i, err)
				}
				for _, s := range tp.Seats {
					if seen[s.Robot] {
						t.Fatalf("case %d: %s boards two trips", i, s.Robot)
					}
					seen[s.Robot] = true
				}
				for _, d := range tp.Deferred {
					if d.Reason == DeferCapacity {
						t.Fatalf("case %d: %s left for capacity after the split", i, d.Robot)
					}
				}
			}
		}
	}
}

func wantsOf(p Plan) []Want {
	var out []Want
	for _, s := range p.Seats {
		out = append(out, Want{s.Robot, s.Dest})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Robot < out[j].Robot })
	return out
}

func hasDup(ws []Want) bool {
	seen := map[string]bool{}
	for _, w := range ws {
		if seen[w.Robot] {
			return true
		}
		seen[w.Robot] = true
	}
	return false
}
