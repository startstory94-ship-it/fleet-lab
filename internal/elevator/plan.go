// Package elevator plans and conducts shared elevator rides: robots with
// different destination floors board one car together, in an order that keeps
// the robots that exit first nearest the door, and move through the door one
// at a time.
//
// plan.go is the pure planner. car.go conducts one car (batching, trips,
// turns, lease). proto.go holds the MQTT messages; docs/ELEVATOR.md is the
// protocol.
package elevator

import (
	"sort"
)

// Want is one robot that wants to ride from the request's origin.
type Want struct {
	Robot string
	Dest  int // destination floor
}

// Request is the input of Plan.
type Request struct {
	Origin   int
	Dir      int // +1 up, -1 down, 0: choose (see Plan)
	Capacity int // slots in the car
	Robots   []Want
}

// Seat is one robot's part of a plan.
type Seat struct {
	Robot string
	Dest  int
	// Board is the boarding order, 0 first. The robot that exits last
	// boards first.
	Board int
	// Slot is the position in the car, 0 at the door. Slot equals the exit
	// order: the robot that exits first stands at the door.
	Slot int
}

// Deferral reasons.
const (
	DeferSameFloor = "same_floor" // destination is the origin: never boards
	DeferDirection = "direction"  // goes the other way: next trip
	DeferCapacity  = "capacity"   // did not fit: next trip
	DeferDuplicate = "duplicate"  // robot listed twice; the first entry counts
)

type Deferred struct {
	Robot  string
	Dest   int
	Reason string
}

// Plan is one trip: who rides, in which order they board, where they stand
// and where the car stops.
type Plan struct {
	Origin   int
	Dir      int
	Stops    []int  // in travel order
	Seats    []Seat // in boarding order
	Deferred []Deferred
}

// Riders returns the robots that ride, in boarding order.
func (p Plan) Riders() []string {
	out := make([]string, len(p.Seats))
	for i, s := range p.Seats {
		out[i] = s.Robot
	}
	return out
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// PlanTrip makes one trip. It is a pure function: the same request gives the
// same plan whatever the order of req.Robots, except that of two entries for
// one robot the first counts.
//
// Rules:
//   - A destination equal to the origin never boards (DeferSameFloor).
//   - Only one direction rides. With Dir 0 it is the direction more robots
//     want; on a tie, the direction of the nearest destination; then up.
//   - When more robots want to go than there are slots, the nearest
//     destinations ride (a short trip frees the car soonest); ties by name.
//   - Exit order is by distance from the origin, ties by name. The robot that
//     exits first gets slot 0 (the door) and boards last; boarding order is
//     the exact reverse of exit order. So at every stop the robots getting
//     out stand in front of everyone who stays.
func PlanTrip(req Request) Plan {
	p := Plan{Origin: req.Origin}
	seen := map[string]bool{}
	var wants []Want
	for _, w := range req.Robots {
		if seen[w.Robot] {
			p.Deferred = append(p.Deferred, Deferred{w.Robot, w.Dest, DeferDuplicate})
			continue
		}
		seen[w.Robot] = true
		if w.Dest == req.Origin {
			p.Deferred = append(p.Deferred, Deferred{w.Robot, w.Dest, DeferSameFloor})
			continue
		}
		wants = append(wants, w)
	}
	byDistance := func(ws []Want) {
		sort.Slice(ws, func(i, j int) bool {
			di, dj := abs(ws[i].Dest-req.Origin), abs(ws[j].Dest-req.Origin)
			if di != dj {
				return di < dj
			}
			return ws[i].Robot < ws[j].Robot
		})
	}
	byDistance(wants)

	dir := sign(req.Dir)
	if dir == 0 {
		up, down := 0, 0
		for _, w := range wants {
			if w.Dest > req.Origin {
				up++
			} else {
				down++
			}
		}
		switch {
		case up > down:
			dir = 1
		case down > up:
			dir = -1
		case len(wants) > 0:
			dir = sign(wants[0].Dest - req.Origin) // nearest first after sorting
		default:
			dir = 1
		}
	}
	p.Dir = dir

	var go_ []Want
	for _, w := range wants {
		if sign(w.Dest-req.Origin) != dir {
			p.Deferred = append(p.Deferred, Deferred{w.Robot, w.Dest, DeferDirection})
			continue
		}
		go_ = append(go_, w)
	}
	capacity := max(req.Capacity, 0)
	if len(go_) > capacity {
		for _, w := range go_[capacity:] {
			p.Deferred = append(p.Deferred, Deferred{w.Robot, w.Dest, DeferCapacity})
		}
		go_ = go_[:capacity]
	}
	// go_ is in exit order now.
	n := len(go_)
	p.Seats = make([]Seat, n)
	for i, w := range go_ {
		p.Seats[n-1-i] = Seat{Robot: w.Robot, Dest: w.Dest, Board: n - 1 - i, Slot: i}
		if len(p.Stops) == 0 || p.Stops[len(p.Stops)-1] != w.Dest {
			p.Stops = append(p.Stops, w.Dest)
		}
	}
	sort.Slice(p.Deferred, func(i, j int) bool { return p.Deferred[i].Robot < p.Deferred[j].Robot })
	return p
}

// PlanTrips splits a request into as many trips as the capacity needs, each
// planned with PlanTrip. Robots deferred for direction, same floor or duplicate
// are returned once, in Deferred of the last plan. The car runs one trip at
// a time and plans again before the next one (new robots may have come), so
// fleetd uses Plan; PlanTrips shows the whole split.
func PlanTrips(req Request) []Plan {
	var out []Plan
	var other []Deferred
	for {
		p := PlanTrip(req)
		req.Dir = p.Dir
		var next []Want
		for _, d := range p.Deferred {
			if d.Reason == DeferCapacity {
				next = append(next, Want{d.Robot, d.Dest})
			} else {
				other = append(other, d)
			}
		}
		p.Deferred = nil
		if len(p.Seats) > 0 || len(out) == 0 {
			out = append(out, p)
		}
		if len(p.Seats) == 0 || len(next) == 0 {
			for _, w := range next { // capacity 0: nobody can ever ride
				other = append(other, Deferred{w.Robot, w.Dest, DeferCapacity})
			}
			sort.Slice(other, func(i, j int) bool { return other[i].Robot < other[j].Robot })
			out[len(out)-1].Deferred = other
			return out
		}
		req.Robots = next
	}
}
