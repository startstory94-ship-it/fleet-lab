// Package live turns the registry into a stream of batched position frames
// for dashboards.
//
// Every FrameInterval the broadcaster takes one snapshot of the registry,
// builds ONE frame holding only the robots whose displayed fields changed
// since the previous frame (plus removed robots), encodes it once and hands
// the same bytes to every subscriber. A new subscriber first gets a full
// snapshot frame. When nothing changed for Heartbeat, a heartbeat frame
// (no robots, the current seq) is sent so clients can tell a quiet fleet from
// a dead connection.
//
// Each subscriber has a small buffered channel. A subscriber that falls
// behind never stalls the others: when its channel is full, the frames still
// queued (the oldest ones, which it would read next) are dropped and a full
// snapshot of the current state is queued instead. The reader may race the
// drain and take one of those frames after the oldest was dropped, so Next
// also discards any delta that does not follow the previous frame it
// returned; the snapshot queued by the drain comes right after. A client
// therefore never applies a delta over a gap, and after any drop the next
// frame it gets is a snapshot. Correctness never depends on every delta
// arriving.
//
// Locks: the broadcaster's mutex is a leaf. Tick reads the robot source
// (registry lock) and the charge source (task manager lock) one after the
// other, holding no other lock, and only then takes its own mutex; under it,
// it only does non-blocking channel operations. Nothing calls into the
// broadcaster while holding another lock. With the existing order (task
// manager -> registry -> event numbering -> event hub -> hub subscriber) the
// lock graph stays acyclic; see README.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"fleetlab/internal/fleet"
)

// Frame types.
const (
	Snapshot  = "snapshot"  // every robot; replaces what the client holds
	Delta     = "delta"     // only robots that changed, and removed robots
	Heartbeat = "heartbeat" // nothing changed; seq is the current one (no gap)
)

// Frame is one message on /ws/live.
type Frame struct {
	Type string `json:"type"`
	// Seq increases by one per tick that changed something. A snapshot
	// carries the seq of the state it shows and deltas that follow it
	// continue from there. A heartbeat repeats the current seq.
	Seq     uint64      `json:"seq"`
	Time    time.Time   `json:"time"` // server time of the registry snapshot
	Robots  []RobotView `json:"robots"`
	Removed []string    `json:"removed,omitempty"`
}

// RobotView is what the dashboard shows of one robot. It is comparable, so
// "changed" is plain inequality. Positions are rounded to a millimetre,
// angles to a milliradian and the battery to 0.1 %, so noise below that does
// not count as a change.
type RobotView struct {
	Name         string  `json:"name"`
	Host         string  `json:"host"`
	Conn         string  `json:"conn"`
	Floor        int     `json:"floor"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Yaw          float64 `json:"yaw"`
	V            float64 `json:"v"`
	HasPose      bool    `json:"has_pose"`
	Mode         string  `json:"mode"`
	Battery      float64 `json:"battery"`
	BatteryKnown bool    `json:"battery_known"`
	NeedsCharge  bool    `json:"needs_charge"` // flagged by the battery policy
	TaskID       string  `json:"task_id"`
	Error        string  `json:"error"`
}

func view(r fleet.Robot, needsCharge bool) RobotView {
	return RobotView{
		Name: r.Name, Host: r.Host, Conn: r.Conn, Floor: r.Floor,
		X: round(r.X, 1000), Y: round(r.Y, 1000), Yaw: round(r.Yaw, 1000), V: round(r.V, 1000),
		HasPose: r.HasPose, Mode: r.Mode, Battery: round(r.Battery, 10), BatteryKnown: r.BatteryKnown,
		NeedsCharge: needsCharge, TaskID: r.TaskID, Error: r.Error,
	}
}

// encodable reports whether JSON can carry every number (no NaN or Inf).
func (v RobotView) encodable() bool {
	for _, f := range []float64{v.X, v.Y, v.Yaw, v.V, v.Battery} {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return false
		}
	}
	return true
}

// round rounds to 1/scale. A value so large that v*scale overflows is kept
// as it is (Ingest refuses such values; this is the second line).
func round(v, scale float64) float64 {
	r := math.Round(v*scale) / scale
	if math.IsInf(r, 0) || math.IsNaN(r) {
		return v
	}
	return r
}

// RobotSource gives the current view of every robot (fleet.Registry).
type RobotSource interface {
	Snapshot() []fleet.Robot
}

// ChargeSource reports which robots the battery policy has flagged as
// needing charge (task.Manager). It must not call into the broadcaster.
type ChargeSource interface {
	NeedsCharge() map[string]bool
}

type Config struct {
	FrameInterval  time.Duration // how often Run ticks
	Heartbeat      time.Duration // with no change for this long, send a heartbeat; 0: never
	Buffer         int           // frames queued per subscriber
	MaxSubscribers int           // Subscribe fails beyond this
	Log            *slog.Logger  // optional
}

func DefaultConfig() Config {
	return Config{FrameInterval: 150 * time.Millisecond, Heartbeat: 2 * time.Second, Buffer: 8, MaxSubscribers: 64}
}

// queued is one frame in a subscriber's channel.
type queued struct {
	data []byte
	seq  uint64
	kind string
}

var (
	// ErrClosed is returned by Next after unsubscribing.
	ErrClosed = errors.New("live: unsubscribed")
	// ErrTooMany is returned by Subscribe at MaxSubscribers.
	ErrTooMany = errors.New("live: too many subscribers")
)

// Sub is one subscriber. Read frames with Next from a single goroutine.
type Sub struct {
	c        chan queued
	dropped  atomic.Uint64
	resyncs  atomic.Uint64
	needSnap bool // under the broadcaster's mutex: a snapshot is owed

	// Reader state, touched only by Next.
	synced bool   // a snapshot was returned
	last   uint64 // seq of the last frame returned
}

// Dropped counts frames thrown away because the subscriber fell behind.
func (s *Sub) Dropped() uint64 { return s.dropped.Load() }

// Resyncs counts snapshots queued for it in place of dropped frames.
func (s *Sub) Resyncs() uint64 { return s.resyncs.Load() }

// Pending is the number of frames queued (some may still be discarded).
func (s *Sub) Pending() int { return len(s.c) }

// Next returns the next encoded frame (JSON), waiting until there is one, ctx
// ends or the subscriber is unsubscribed.
func (s *Sub) Next(ctx context.Context) ([]byte, error) {
	for {
		var q queued
		var ok bool
		select {
		case q, ok = <-s.c:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !ok {
			return nil, ErrClosed
		}
		if data, use := s.accept(q); use {
			return data, nil
		}
	}
}

// poll is Next without waiting, for tests.
func (s *Sub) poll() ([]byte, bool, error) {
	for {
		select {
		case q, ok := <-s.c:
			if !ok {
				return nil, false, ErrClosed
			}
			if data, use := s.accept(q); use {
				return data, true, nil
			}
		default:
			return nil, false, nil
		}
	}
}

// accept drops a delta that does not follow the last frame returned (the
// frame before it was dropped and a snapshot is on its way), and a heartbeat
// that is not for the last frame returned.
func (s *Sub) accept(q queued) ([]byte, bool) {
	switch {
	case q.kind == Snapshot:
	case !s.synced:
		return nil, false
	case q.kind == Delta && q.seq != s.last+1:
		s.dropped.Add(1)
		return nil, false
	case q.kind == Heartbeat && q.seq != s.last:
		return nil, false
	}
	s.synced, s.last = true, q.seq
	return q.data, true
}

// Stats are totals over the broadcaster's lifetime.
type Stats struct {
	Frames       uint64 `json:"frames"`        // delta frames built
	Heartbeats   uint64 `json:"heartbeats"`    // heartbeat frames built
	Encodes      uint64 `json:"encodes"`       // frames encoded (deltas, snapshots, heartbeats)
	EncodeErrors uint64 `json:"encode_errors"` // frames that could not be encoded and were skipped
	Unencodable  uint64 `json:"unencodable"`   // robot views skipped for NaN/Inf values (per tick)
	Dropped      uint64 `json:"dropped"`       // frames dropped from full subscriber queues
	Resyncs      uint64 `json:"resyncs"`       // snapshots queued after drops
	Refused      uint64 `json:"refused"`       // subscribers refused at MaxSubscribers
	Subscribers  int    `json:"subscribers"`
	Seq          uint64 `json:"seq"`
}

// Broadcaster is safe for concurrent use.
type Broadcaster struct {
	cfg    Config
	robots RobotSource
	charge ChargeSource // may be nil
	now    func() time.Time

	mu       sync.Mutex
	subs     map[*Sub]struct{}
	prev     map[string]RobotView // state as of the last frame
	seq      uint64
	time     time.Time // of the last delta
	lastSent time.Time // of the last delta or heartbeat
	snap     []byte    // encoded snapshot of snapSeq; nil until needed
	snapSeq  uint64
	stats    Stats
}

// New creates a broadcaster. charge may be nil.
func New(cfg Config, robots RobotSource, charge ChargeSource) *Broadcaster {
	def := DefaultConfig()
	if cfg.FrameInterval <= 0 {
		cfg.FrameInterval = def.FrameInterval
	}
	if cfg.Buffer < 1 {
		cfg.Buffer = 1
	}
	if cfg.MaxSubscribers < 1 {
		cfg.MaxSubscribers = def.MaxSubscribers
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Broadcaster{cfg: cfg, robots: robots, charge: charge, now: time.Now,
		subs: make(map[*Sub]struct{}), prev: make(map[string]RobotView)}
}

// SetClock replaces the clock stamped on frames, for tests. Call it before
// the broadcaster is used.
func (b *Broadcaster) SetClock(now func() time.Time) { b.now = now }

// Run ticks every FrameInterval until ctx ends.
func (b *Broadcaster) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.FrameInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Tick()
		}
	}
}

// encode is called with b.mu held. A frame that cannot be encoded (it would
// take a NaN or Inf to get here) is logged and skipped: the server keeps
// running and the frame's changes are sent with the next good frame.
func (b *Broadcaster) encode(f Frame) ([]byte, bool) {
	if f.Robots == nil {
		f.Robots = []RobotView{}
	}
	data, err := json.Marshal(f)
	if err != nil {
		b.stats.EncodeErrors++
		if n := b.stats.EncodeErrors; n&(n-1) == 0 { // 1st, 2nd, 4th, ...: no log flood
			b.cfg.Log.Error("live frame not encoded; skipped", "type", f.Type, "seq", f.Seq, "errors", n, "err", err)
		}
		return nil, false
	}
	b.stats.Encodes++
	return data, true
}

// snapshot returns the full frame of the current state, encoding it at most
// once per seq. Call with b.mu held.
func (b *Broadcaster) snapshot() (queued, bool) {
	if b.snap == nil || b.snapSeq != b.seq {
		all := make([]RobotView, 0, len(b.prev))
		for _, v := range b.prev {
			all = append(all, v)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
		data, ok := b.encode(Frame{Type: Snapshot, Seq: b.seq, Time: b.time, Robots: all})
		if !ok {
			return queued{}, false
		}
		b.snap, b.snapSeq = data, b.seq
	}
	return queued{data: b.snap, seq: b.seq, kind: Snapshot}, true
}

// resync drops whatever s has queued and queues a snapshot instead. If the
// snapshot cannot be built the debt is remembered and paid on a later tick;
// meanwhile Next discards deltas, since s is not in sync. Call with b.mu held.
func (b *Broadcaster) resync(s *Sub) {
	n := uint64(0)
drain:
	for {
		select {
		case <-s.c:
			n++
		default:
			break drain
		}
	}
	s.dropped.Add(n)
	b.stats.Dropped += n
	q, ok := b.snapshot()
	if !ok {
		s.needSnap = true
		return
	}
	s.c <- q // only Tick and Subscribe send, under b.mu: there is room
	s.needSnap = false
	if n > 0 {
		s.resyncs.Add(1)
		b.stats.Resyncs++
	}
}

// Tick builds and sends one frame if anything changed since the last one, or
// a heartbeat if nothing did for a while.
func (b *Broadcaster) Tick() {
	// Read the sources before taking b.mu, so b.mu never nests with their
	// locks.
	robots := b.robots.Snapshot()
	var flags map[string]bool
	if b.charge != nil {
		flags = b.charge.NeedsCharge()
	}
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if s.needSnap {
			b.resync(s)
		}
	}
	cur := make(map[string]RobotView, len(robots))
	var changed []RobotView
	for _, r := range robots {
		v := view(r, flags[r.Name])
		if !v.encodable() {
			// Only a value that bypassed Ingest can get here. Keep showing
			// the robot as it was, so one bad robot cannot stop the frames.
			b.stats.Unencodable++
			if old, ok := b.prev[r.Name]; ok {
				cur[r.Name] = old
			}
			continue
		}
		cur[r.Name] = v
		if old, ok := b.prev[r.Name]; !ok || old != v {
			changed = append(changed, v)
		}
	}
	var removed []string
	for name := range b.prev {
		if _, ok := cur[name]; !ok {
			removed = append(removed, name)
		}
	}
	if len(changed) == 0 && len(removed) == 0 {
		b.heartbeat(now)
		return
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Name < changed[j].Name })
	sort.Strings(removed)
	data, ok := b.encode(Frame{Type: Delta, Seq: b.seq + 1, Time: now, Robots: changed, Removed: removed})
	if !ok {
		return // nothing committed: the change is still a change next tick
	}
	b.prev, b.seq, b.time, b.lastSent = cur, b.seq+1, now, now
	b.stats.Frames++
	q := queued{data: data, seq: b.seq, kind: Delta}
	for s := range b.subs {
		if s.needSnap {
			continue // a snapshot (which includes this change) is owed
		}
		select {
		case s.c <- q:
		default:
			b.resync(s)
		}
	}
}

// heartbeat sends a heartbeat if nothing was sent for cfg.Heartbeat. A
// subscriber whose queue is full skips it: it has frames to read anyway.
// Call with b.mu held.
func (b *Broadcaster) heartbeat(now time.Time) {
	if b.cfg.Heartbeat <= 0 || now.Sub(b.lastSent) < b.cfg.Heartbeat {
		return
	}
	data, ok := b.encode(Frame{Type: Heartbeat, Seq: b.seq, Time: now})
	if !ok {
		return
	}
	b.lastSent = now
	b.stats.Heartbeats++
	q := queued{data: data, seq: b.seq, kind: Heartbeat}
	for s := range b.subs {
		select {
		case s.c <- q:
		default:
		}
	}
}

// Subscribe registers a subscriber. Its first frame is a full snapshot of
// the state as of the last frame. Call the returned func to unsubscribe;
// Next then returns ErrClosed. At MaxSubscribers it returns ErrTooMany.
func (b *Broadcaster) Subscribe() (*Sub, func(), error) {
	s := &Sub{c: make(chan queued, b.cfg.Buffer)}
	b.mu.Lock()
	if len(b.subs) >= b.cfg.MaxSubscribers {
		b.stats.Refused++
		b.mu.Unlock()
		return nil, nil, ErrTooMany
	}
	b.resync(s)
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return s, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, s)
			close(s.c)
			b.mu.Unlock()
		})
	}, nil
}

// Stats returns the totals so far.
func (b *Broadcaster) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.stats
	st.Subscribers, st.Seq = len(b.subs), b.seq
	return st
}
