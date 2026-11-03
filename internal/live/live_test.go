package live

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"fleetlab/internal/fleet"
)

// fakeRobots is a RobotSource and ChargeSource the test edits between ticks.
type fakeRobots struct {
	mu     sync.Mutex
	robots map[string]fleet.Robot
	flags  map[string]bool
}

func newFake(names ...string) *fakeRobots {
	f := &fakeRobots{robots: map[string]fleet.Robot{}, flags: map[string]bool{}}
	for i, n := range names {
		f.robots[n] = fleet.Robot{Name: n, Conn: fleet.Online, Mode: "IDLE", Floor: 1, X: float64(2 * i),
			HasPose: true, Battery: 80, BatteryKnown: true}
	}
	return f
}

func (f *fakeRobots) Snapshot() []fleet.Robot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.Robot, 0, len(f.robots))
	for _, r := range f.robots {
		out = append(out, r)
	}
	return out
}

func (f *fakeRobots) NeedsCharge() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, v := range f.flags {
		out[k] = v
	}
	return out
}

func (f *fakeRobots) edit(name string, fn func(r *fleet.Robot)) {
	f.mu.Lock()
	r := f.robots[name]
	fn(&r)
	f.robots[name] = r
	f.mu.Unlock()
}

func (f *fakeRobots) want() map[string]RobotView {
	out := map[string]RobotView{}
	for _, r := range f.Snapshot() {
		out[r.Name] = view(r, f.NeedsCharge()[r.Name])
	}
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, buffer int, names ...string) (*Broadcaster, *fakeRobots, *clock) {
	t.Helper()
	f := newFake(names...)
	c := &clock{t: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
	b := New(Config{FrameInterval: 150 * time.Millisecond, Buffer: buffer}, f, f)
	b.SetClock(c.now)
	return b, f, c
}

// subscribe is Subscribe for tests that stay under MaxSubscribers.
func subscribe(b *Broadcaster) (*Sub, func()) {
	s, unsub, err := b.Subscribe()
	if err != nil {
		panic(err)
	}
	return s, unsub
}

func decode(t *testing.T, data []byte) Frame {
	t.Helper()
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return f
}

// next reads one frame without waiting; it fails if none is queued.
func next(t *testing.T, s *Sub) ([]byte, Frame) {
	t.Helper()
	data, ok, err := s.poll()
	if err != nil || !ok {
		t.Fatalf("no frame: ok=%v err=%v", ok, err)
	}
	return data, decode(t, data)
}

func none(t *testing.T, s *Sub) {
	t.Helper()
	if data, ok, _ := s.poll(); ok {
		t.Fatalf("unexpected frame %s", data)
	}
}

func names(f Frame) []string {
	var out []string
	for _, r := range f.Robots {
		out = append(out, r.Name)
	}
	return out
}

// client is what a dashboard does with frames: a snapshot replaces the
// state, a delta is merged into it.
type client struct {
	robots map[string]RobotView
	seq    uint64
}

func (c *client) apply(t *testing.T, f Frame) {
	t.Helper()
	switch f.Type {
	case Snapshot:
		c.robots = map[string]RobotView{}
	case Delta:
		if c.robots == nil {
			t.Fatal("delta before any snapshot")
		}
		if f.Seq != c.seq+1 {
			t.Fatalf("delta seq %d applied after %d: a gap", f.Seq, c.seq)
		}
	case Heartbeat:
		if c.robots == nil || f.Seq != c.seq || len(f.Robots) != 0 {
			t.Fatalf("heartbeat seq %d with %d robots after seq %d", f.Seq, len(f.Robots), c.seq)
		}
	default:
		t.Fatalf("frame type %q", f.Type)
	}
	for _, r := range f.Robots {
		c.robots[r.Name] = r
	}
	for _, n := range f.Removed {
		delete(c.robots, n)
	}
	c.seq = f.Seq
}

func TestDeltaHasOnlyChangedRobots(t *testing.T) {
	b, f, _ := setup(t, 8, "r1", "r2", "r3")
	s, unsub := subscribe(b)
	defer unsub()
	next(t, s) // the (empty) snapshot

	b.Tick()
	if _, fr := next(t, s); fr.Type != Delta || !reflect.DeepEqual(names(fr), []string{"r1", "r2", "r3"}) {
		t.Fatalf("first delta = %s %v, want every robot (all new)", fr.Type, names(fr))
	}

	f.edit("r2", func(r *fleet.Robot) { r.X += 0.5 })
	b.Tick()
	_, fr := next(t, s)
	if !reflect.DeepEqual(names(fr), []string{"r2"}) || fr.Robots[0].X != f.want()["r2"].X {
		t.Fatalf("delta = %+v, want only r2 at its new x", fr.Robots)
	}

	// Nothing changed (noise under a millimetre does not count): no frame.
	f.edit("r1", func(r *fleet.Robot) { r.X += 0.0001 })
	b.Tick()
	none(t, s)

	// Every displayed field counts, one robot each.
	edits := map[string]func(r *fleet.Robot){
		"r1": func(r *fleet.Robot) { r.Conn = fleet.Stale },
		"r2": func(r *fleet.Robot) { r.TaskID = "t-1.0.1.x" },
		"r3": func(r *fleet.Robot) { r.Battery = 41 },
	}
	for name, fn := range edits {
		f.edit(name, fn)
		b.Tick()
		if _, fr := next(t, s); !reflect.DeepEqual(names(fr), []string{name}) {
			t.Fatalf("after editing %s delta has %v", name, names(fr))
		}
	}
	for name, fn := range map[string]func(r *fleet.Robot){
		"r1": func(r *fleet.Robot) { r.Floor = 3 },
		"r2": func(r *fleet.Robot) { r.Yaw = 1 },
		"r3": func(r *fleet.Robot) { r.Error = "bump" },
	} {
		f.edit(name, fn)
		b.Tick()
		if _, fr := next(t, s); !reflect.DeepEqual(names(fr), []string{name}) {
			t.Fatalf("after editing %s delta has %v", name, names(fr))
		}
	}

	// A robot that disappears is listed as removed.
	f.mu.Lock()
	delete(f.robots, "r3")
	f.mu.Unlock()
	b.Tick()
	if _, fr := next(t, s); len(fr.Robots) != 0 || !reflect.DeepEqual(fr.Removed, []string{"r3"}) {
		t.Fatalf("delta = %+v, want only r3 removed", fr)
	}
}

func TestFirstFrameIsSnapshot(t *testing.T) {
	b, f, _ := setup(t, 8, "r1", "r2")
	b.Tick()
	f.edit("r1", func(r *fleet.Robot) { r.Y = 4 })
	b.Tick() // seq 2, changes only r1

	s, unsub := subscribe(b)
	defer unsub()
	_, fr := next(t, s)
	if fr.Type != Snapshot || fr.Seq != 2 || !reflect.DeepEqual(names(fr), []string{"r1", "r2"}) {
		t.Fatalf("first frame = %s seq %d %v, want a snapshot of seq 2 with r1, r2", fr.Type, fr.Seq, names(fr))
	}
	c := &client{}
	c.apply(t, fr)
	if !reflect.DeepEqual(c.robots, f.want()) {
		t.Fatalf("snapshot state %+v, want %+v", c.robots, f.want())
	}
	f.edit("r2", func(r *fleet.Robot) { r.Mode = "MOVING" })
	b.Tick()
	_, fr = next(t, s)
	c.apply(t, fr) // fails on a seq gap
	if !reflect.DeepEqual(c.robots, f.want()) {
		t.Fatalf("state after delta %+v, want %+v", c.robots, f.want())
	}
}

func TestFrameSeqAndTime(t *testing.T) {
	b, f, clk := setup(t, 8, "r1")
	s, unsub := subscribe(b)
	defer unsub()
	next(t, s)
	var last uint64
	for i := 0; i < 5; i++ {
		clk.t = clk.t.Add(150 * time.Millisecond)
		f.edit("r1", func(r *fleet.Robot) { r.X++ })
		b.Tick()
		_, fr := next(t, s)
		if fr.Seq != last+1 || !fr.Time.Equal(clk.t) {
			t.Fatalf("frame %d: seq %d time %v, want seq %d time %v", i, fr.Seq, fr.Time, last+1, clk.t)
		}
		last = fr.Seq
	}
	b.Tick() // no change: no frame, seq unchanged
	if st := b.Stats(); st.Seq != last {
		t.Fatalf("seq %d after an empty tick, want %d", st.Seq, last)
	}
}

// One encode per frame, and every subscriber gets the very same bytes.
func TestEncodeOncePerFrame(t *testing.T) {
	b, f, _ := setup(t, 8, "r1", "r2")
	b.Tick()
	var subs []*Sub
	for i := 0; i < 20; i++ {
		s, unsub := subscribe(b)
		defer unsub()
		next(t, s)
		subs = append(subs, s)
	}
	// 1 delta + 1 snapshot shared by the 20 subscribers.
	if got := b.Stats().Encodes; got != 2 {
		t.Fatalf("encodes after 20 subscribes = %d, want 2 (one delta, one cached snapshot)", got)
	}
	for i := 0; i < 3; i++ {
		f.edit("r1", func(r *fleet.Robot) { r.X++ })
		before := b.Stats().Encodes
		b.Tick()
		if got := b.Stats().Encodes - before; got != 1 {
			t.Fatalf("tick encoded %d times for 20 subscribers, want 1", got)
		}
		first, _ := next(t, subs[0])
		for _, s := range subs[1:] {
			data, _ := next(t, s)
			if &data[0] != &first[0] {
				t.Fatal("subscribers got different byte slices for one frame")
			}
		}
	}
}

// A subscriber that does not read loses its oldest frames, then gets a
// snapshot; a subscriber that reads gets every delta.
func TestSlowSubscriberResyncs(t *testing.T) {
	const buffer = 4
	b, f, _ := setup(t, buffer, "r1", "r2")
	fast, unsubFast := subscribe(b)
	defer unsubFast()
	slow, unsubSlow := subscribe(b)
	defer unsubSlow()

	fc := &client{}
	_, fr := next(t, fast)
	fc.apply(t, fr)
	for i := 1; i <= 10; i++ {
		// Alternate robots, so a lost delta loses a change of the other one.
		name := []string{"r1", "r2"}[i%2]
		f.edit(name, func(r *fleet.Robot) { r.X += float64(i) })
		b.Tick()
		_, fr := next(t, fast)
		if fr.Type != Delta {
			t.Fatalf("fast subscriber got a %s at tick %d", fr.Type, i)
		}
		fc.apply(t, fr) // fails on a gap
	}
	if fast.Dropped() != 0 || fast.Resyncs() != 0 {
		t.Fatalf("fast dropped %d resynced %d, want 0", fast.Dropped(), fast.Resyncs())
	}
	if !reflect.DeepEqual(fc.robots, f.want()) {
		t.Fatalf("fast state %+v, want %+v", fc.robots, f.want())
	}

	// slow: [snap0 d1 d2 d3] full at tick 4 -> all 4 dropped, [snap4];
	// d5..d7 fill it, tick 8 drops 4, [snap8]; then d9, d10.
	if got := slow.Dropped(); got != 8 {
		t.Errorf("slow dropped %d, want 8", got)
	}
	if got := slow.Resyncs(); got != 2 {
		t.Errorf("slow resynced %d, want 2", got)
	}
	var got []string
	sc := &client{}
	for slow.Pending() > 0 {
		_, fr := next(t, slow)
		got = append(got, fr.Type)
		sc.apply(t, fr)
	}
	if want := []string{Snapshot, Delta, Delta}; !reflect.DeepEqual(got, want) {
		t.Fatalf("slow then reads %v, want %v (old frames dropped, newest kept)", got, want)
	}
	if !reflect.DeepEqual(sc.robots, f.want()) || sc.seq != 10 {
		t.Fatalf("slow state seq %d %+v, want seq 10 %+v", sc.seq, sc.robots, f.want())
	}
	if st := b.Stats(); st.Dropped != 8 || st.Resyncs != 2 || st.Frames != 10 {
		t.Fatalf("stats %+v", st)
	}
}

// A reader that keeps up partly (reads now and then) still never sees a gap.
func TestPartialReaderNeverSeesGap(t *testing.T) {
	b, f, _ := setup(t, 3, "r1", "r2", "r3")
	s, unsub := subscribe(b)
	defer unsub()
	c := &client{}
	for i := 1; i <= 200; i++ {
		name := []string{"r1", "r2", "r3"}[i%3]
		f.edit(name, func(r *fleet.Robot) { r.Y += 1 })
		b.Tick()
		if i%(1+i%5) == 0 { // irregular reading
			_, fr := next(t, s)
			c.apply(t, fr)
		}
	}
	for s.Pending() > 0 {
		_, fr := next(t, s)
		c.apply(t, fr)
	}
	if s.Dropped() == 0 {
		t.Fatal("test does not exercise drops")
	}
	if !reflect.DeepEqual(c.robots, f.want()) {
		t.Fatalf("state %+v, want %+v", c.robots, f.want())
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	b, f, _ := setup(t, 8, "r1")
	s, unsub := subscribe(b)
	next(t, s)
	unsub()
	unsub() // twice is fine
	f.edit("r1", func(r *fleet.Robot) { r.X++ })
	b.Tick()
	if data, ok, err := s.poll(); ok || err != ErrClosed {
		t.Fatalf("after unsubscribe poll = %s, %v, %v; want ErrClosed", data, ok, err)
	}
	if st := b.Stats(); st.Subscribers != 0 {
		t.Fatalf("subscribers = %d after unsubscribe", st.Subscribers)
	}
}

func TestNeedsChargeAppearsAndClears(t *testing.T) {
	b, f, _ := setup(t, 8, "r1", "r2")
	s, unsub := subscribe(b)
	defer unsub()
	next(t, s)
	b.Tick()
	next(t, s)

	for _, flagged := range []bool{true, false} {
		f.mu.Lock()
		f.flags["r2"] = flagged
		f.mu.Unlock()
		b.Tick()
		_, fr := next(t, s)
		if len(fr.Robots) != 1 || fr.Robots[0].Name != "r2" || fr.Robots[0].NeedsCharge != flagged {
			t.Fatalf("delta %+v, want r2 needs_charge=%v", fr.Robots, flagged)
		}
	}
}

// Ticks, subscribes and unsubscribes at once: no deadlock, no panic on a
// closed channel, and every reader sees a gap-free stream.
func TestConcurrentSubscribers(t *testing.T) {
	b, f, _ := setup(t, 2, "r1", "r2")
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f.edit([]string{"r1", "r2"}[i%2], func(r *fleet.Robot) { r.X++ })
			b.Tick()
		}
	}()
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, unsub := subscribe(b)
			defer unsub()
			var seq uint64
			snapshot := false
			for {
				data, err := s.Next(ctx)
				if err != nil {
					return
				}
				var fr Frame
				if err := json.Unmarshal(data, &fr); err != nil {
					errs <- err.Error()
					return
				}
				if fr.Type == Delta && (!snapshot || fr.Seq != seq+1) {
					errs <- "gap"
					return
				}
				snapshot = snapshot || fr.Type == Snapshot
				seq = fr.Seq
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock")
	}
	close(errs)
	var all []string
	for e := range errs {
		all = append(all, e)
	}
	sort.Strings(all)
	if len(all) > 0 {
		t.Fatalf("readers saw: %v", all)
	}
	if b.Stats().Dropped == 0 {
		t.Fatal("test does not exercise drops")
	}
}

// The snapshot carries the time of the state it shows, not zero or now.
func TestSnapshotCarriesFrameTime(t *testing.T) {
	b, f, clk := setup(t, 8, "r1")
	clk.t = clk.t.Add(time.Second)
	b.Tick()
	frameTime := clk.t
	f.edit("r1", func(r *fleet.Robot) { r.X = 0.0001 }) // below a millimetre: no frame
	clk.t = clk.t.Add(time.Second)
	b.Tick()
	s, unsub := subscribe(b)
	defer unsub()
	if _, fr := next(t, s); fr.Type != Snapshot || !fr.Time.Equal(frameTime) {
		t.Fatalf("snapshot time %v, want %v (the last frame's)", fr.Time, frameTime)
	}
}

func TestRunTicks(t *testing.T) {
	f := newFake("r1")
	b := New(Config{FrameInterval: 5 * time.Millisecond, Buffer: 8}, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	s, unsub := subscribe(b)
	defer unsub()
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	for {
		data, err := s.Next(wctx)
		if err != nil {
			t.Fatal("Run sent no delta:", err)
		}
		if fr := decode(t, data); fr.Type == Delta && len(fr.Robots) == 1 {
			return
		}
	}
}

// With nothing changing a heartbeat repeats the current seq every
// Heartbeat; it never opens a gap for the deltas that follow.
func TestHeartbeat(t *testing.T) {
	f := newFake("r1")
	clk := &clock{t: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
	b := New(Config{Buffer: 8, Heartbeat: 2 * time.Second}, f, nil)
	b.SetClock(clk.now)
	b.Tick() // seq 1
	s, unsub := subscribe(b)
	defer unsub()
	c := &client{}
	_, fr := next(t, s)
	c.apply(t, fr)

	step := func(d time.Duration) {
		clk.t = clk.t.Add(d)
		b.Tick()
	}
	step(1900 * time.Millisecond)
	none(t, s)
	step(100 * time.Millisecond)
	_, fr = next(t, s)
	if fr.Type != Heartbeat || fr.Seq != 1 || !fr.Time.Equal(clk.t) {
		t.Fatalf("got %s seq %d at %v, want a heartbeat for seq 1 at %v", fr.Type, fr.Seq, fr.Time, clk.t)
	}
	c.apply(t, fr)
	step(time.Second)
	none(t, s) // next one is 2 s after the last
	f.edit("r1", func(r *fleet.Robot) { r.X = 5 })
	step(500 * time.Millisecond)
	_, fr = next(t, s)
	c.apply(t, fr) // delta seq 2: no gap after the heartbeat
	// A change resets the heartbeat clock.
	step(1500 * time.Millisecond)
	none(t, s)
	step(500 * time.Millisecond)
	if _, fr = next(t, s); fr.Type != Heartbeat || fr.Seq != 2 {
		t.Fatalf("got %s seq %d, want a heartbeat for seq 2", fr.Type, fr.Seq)
	}
	if st := b.Stats(); st.Heartbeats != 2 || st.Seq != 2 {
		t.Fatalf("stats %+v", st)
	}
}

// A queued heartbeat for an older seq (the reader was resynced past it) is
// not returned.
func TestStaleHeartbeatDiscarded(t *testing.T) {
	s := &Sub{c: make(chan queued, 4)}
	s.c <- queued{data: []byte("s5"), seq: 5, kind: Snapshot}
	s.c <- queued{data: []byte("h4"), seq: 4, kind: Heartbeat}
	s.c <- queued{data: []byte("h5"), seq: 5, kind: Heartbeat}
	var got []string
	for {
		data, ok, _ := s.poll()
		if !ok {
			break
		}
		got = append(got, string(data))
	}
	if !reflect.DeepEqual(got, []string{"s5", "h5"}) {
		t.Fatalf("got %v", got)
	}
}

// REVIEW 5-1: a huge but finite coordinate used to overflow round() to Inf,
// fail json.Marshal and panic inside Run's goroutine, killing fleetd.
func TestReviewHugePoseNoPanic(t *testing.T) {
	g := fleet.NewRegistry(fleet.DefaultConfig(), nil)
	g.ApplyPose("evil", fleet.PoseMsg{Boot: "b", Seq: 1, Floor: 1, X: 1e306})
	b := New(DefaultConfig(), g, nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Tick panicked: %v", r)
		}
	}()
	b.Tick()
	s, unsub := subscribe(b)
	defer unsub()
	if _, fr := next(t, s); len(fr.Robots) != 1 || fr.Robots[0].X != 1e306 {
		t.Fatalf("snapshot %+v, want evil at its raw x", fr.Robots)
	}
}

// A NaN or Inf that bypassed Ingest skips that robot only; the others keep
// streaming and the robot reappears once its values are good.
func TestNonFiniteRobotSkipped(t *testing.T) {
	b, f, _ := setup(t, 8, "good", "bad")
	f.edit("bad", func(r *fleet.Robot) { r.X = math.NaN() })
	s, unsub := subscribe(b)
	defer unsub()
	next(t, s)
	b.Tick()
	if _, fr := next(t, s); !reflect.DeepEqual(names(fr), []string{"good"}) {
		t.Fatalf("delta %v, want only good", names(fr))
	}
	f.edit("good", func(r *fleet.Robot) { r.X = 7 })
	f.edit("bad", func(r *fleet.Robot) { r.Battery = math.Inf(1) })
	b.Tick()
	if _, fr := next(t, s); !reflect.DeepEqual(names(fr), []string{"good"}) {
		t.Fatalf("delta %v, want only good", names(fr))
	}
	f.edit("bad", func(r *fleet.Robot) { r.X, r.Battery = 3, 50 })
	b.Tick()
	if _, fr := next(t, s); !reflect.DeepEqual(names(fr), []string{"bad"}) || fr.Robots[0].X != 3 {
		t.Fatalf("delta %+v, want bad back at x=3", fr.Robots)
	}
	if st := b.Stats(); st.Unencodable != 2 || st.EncodeErrors != 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestMaxSubscribers(t *testing.T) {
	f := newFake("r1")
	b := New(Config{Buffer: 2, MaxSubscribers: 2}, f, nil)
	_, u1 := subscribe(b)
	_, u2 := subscribe(b)
	defer u2()
	if _, _, err := b.Subscribe(); err != ErrTooMany {
		t.Fatalf("third Subscribe err = %v, want ErrTooMany", err)
	}
	u1()
	_, u3, err := b.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe after a slot freed: %v", err)
	}
	u3()
	if st := b.Stats(); st.Refused != 1 || st.Subscribers != 1 {
		t.Fatalf("stats %+v", st)
	}
}
