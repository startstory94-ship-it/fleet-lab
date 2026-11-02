package task

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fleetlab/internal/fleet"
)

// Config holds the assignment and retry rules.
type Config struct {
	MinBattery   float64       // % a robot needs to take a task
	FloorPenalty float64       // metres added to the distance per floor of difference
	AckTimeout   time.Duration // wait this long for a sign of life before re-sending
	MaxResends   int           // re-sends of the same cmd_id before the attempt fails
	MaxAttempts  int           // failed attempts that are retried; one more fails the task
	CmdTTL       time.Duration // expires_at = send time + CmdTTL
	// RestoreGrace is how long, from the first MQTT connection after a
	// restart, a robot has to confirm the leg it was on. Keep it above the
	// registry's OfflineAfter so a robot that is only slow is not replaced.
	RestoreGrace time.Duration
	WatchMargin  time.Duration // a watch lasts until last send + CmdTTL + WatchMargin
	WatchMax     time.Duration // and never longer than this
	Cooldown     time.Duration // a robot that ignored a goto takes no task for this long
	LegMinSpeed  float64       // m/s; a leg times out after distance/LegMinSpeed + LegSlack
	LegSlack     time.Duration
}

func DefaultConfig() Config {
	return Config{
		MinBattery:   0, // stage 4 raises this
		FloorPenalty: 1000,
		AckTimeout:   3 * time.Second,
		MaxResends:   3,
		MaxAttempts:  3,
		CmdTTL:       10 * time.Second,
		RestoreGrace: 25 * time.Second,
		WatchMargin:  5 * time.Second,
		WatchMax:     10 * time.Minute,
		Cooldown:     time.Minute,
		LegMinSpeed:  0.3,
		LegSlack:     30 * time.Second,
	}
}

// Deps are the manager's connections to the outside world.
type Deps struct {
	Now    func() time.Time
	Robots RobotSource
	Sender Sender
	Store  Store        // optional
	Emit   func(Event)  // optional; called with the lock held, must not block
	Log    *slog.Logger // optional
	// Boot goes into every cmd_id so ids from different server runs never
	// collide. Random when empty.
	Boot string
}

// watch is a goto the server took away from a robot (robot lost, no
// response, timeout, restart, user cancel). While the robot may still be
// executing it, or the goto may still be on its way, the robot is watched and
// cancelled if it reports that cmd_id.
type watch struct {
	robot, cmdID string
	since        time.Time
	lastSend     time.Time // last goto or cancel sent for it; it may land until + CmdTTL
	lastCancel   time.Time
}

// sighting is a robot reporting a cmd_id it should not be executing.
type sighting struct {
	cmdID string
	since time.Time
}

// Manager is safe for concurrent use. Drive it with Tick and HandleAck, and
// run RunWriter (or call Flush) to persist.
type Manager struct {
	mu        sync.Mutex
	cfg       Config
	d         Deps
	tasks     map[string]*Task
	order     []*Task // by Seq, i.e. FIFO
	nextSeq   uint64
	watches   []*watch
	sightings map[string]sighting  // robot -> stray cmd_id it reports
	cooldown  map[string]time.Time // robot -> takes no task until
	linkUp    bool

	// Persistence: the latest copy of each changed task, written by Flush.
	dirty   map[string]Task
	wake    chan struct{}
	flushMu sync.Mutex
}

func NewManager(cfg Config, d Deps) *Manager {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Emit == nil {
		d.Emit = func(Event) {}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Boot == "" {
		d.Boot = strings.ToLower(rand.Text()[:6])
	}
	return &Manager{cfg: cfg, d: d, tasks: make(map[string]*Task),
		sightings: make(map[string]sighting), cooldown: make(map[string]time.Time),
		dirty: make(map[string]Task), wake: make(chan struct{}, 1)}
}

// Restore loads tasks from the store. Call it once, before anything else.
// Tasks that were on a robot wait for the robot to report their cmd_id; see
// LinkUp.
func (m *Manager) Restore() error {
	if m.d.Store == nil {
		return nil
	}
	loaded, err := m.d.Store.Load()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.d.Now()
	for i := range loaded {
		t := &loaded[i]
		if t.active() {
			t.restoring, t.waitFrom = true, now
			if m.linkUp {
				t.restoreUntil = now.Add(m.cfg.RestoreGrace)
			}
		}
		m.tasks[t.ID] = t
		m.order = append(m.order, t)
		m.nextSeq = max(m.nextSeq, t.Seq)
	}
	if f, ok := m.d.Store.(interface{ MaxSeq() (uint64, error) }); ok {
		if n, err := f.MaxSeq(); err == nil {
			m.nextSeq = max(m.nextSeq, n)
		}
	}
	m.d.Log.Info("tasks restored", "count", len(loaded), "boot", m.d.Boot)
	return nil
}

// LinkUp starts the restore grace period. Call it on the first MQTT
// connection: before that no robot can report anything.
func (m *Manager) LinkUp() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.linkUp {
		return
	}
	m.linkUp = true
	until := m.d.Now().Add(m.cfg.RestoreGrace)
	for _, t := range m.order {
		if t.restoring {
			t.restoreUntil = until
		}
	}
}

// Submit queues a new task. It is saved before it is queued: if the store
// fails, the task does not exist and the error is returned.
func (m *Manager) Submit(wps []Waypoint) (Task, error) {
	if len(wps) == 0 {
		return Task{}, fmt.Errorf("%w: needs at least one waypoint", ErrInvalid)
	}
	for i, w := range wps {
		if !finite(w.X) || !finite(w.Y) {
			return Task{}, fmt.Errorf("%w: waypoint %d is not a finite number", ErrInvalid, i)
		}
	}
	m.mu.Lock()
	m.nextSeq++ // never handed out again in this run, even if the save fails
	seq, now := m.nextSeq, m.d.Now()
	m.mu.Unlock()

	t := &Task{
		ID: "t-" + strconv.FormatUint(seq, 10), Seq: seq,
		Waypoints: append([]Waypoint(nil), wps...),
		State:     Queued, Created: now, Updated: now,
	}
	if m.d.Store != nil {
		if err := m.d.Store.Save(*t); err != nil {
			return Task{}, fmt.Errorf("save task: %w", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t)
	for i := len(m.order) - 1; i > 0 && m.order[i].Seq < m.order[i-1].Seq; i-- {
		m.order[i], m.order[i-1] = m.order[i-1], m.order[i] // a concurrent Submit got in first
	}
	return copyTask(t), nil
}

// List returns tasks in creation order, optionally only those in state.
func (m *Manager) List(state string) []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Task{}
	for _, t := range m.order {
		if state == "" || t.State == state {
			out = append(out, copyTask(t))
		}
	}
	return out
}

func (m *Manager) Get(id string) (Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, false
	}
	return copyTask(t), true
}

// Cancel stops a task. A robot working on it gets a cancel; the "cancelled"
// ack that follows is for a finished task and is ignored.
func (m *Manager) Cancel(id string) (Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	if !ok {
		return Task{}, ErrNotFound
	}
	if Finished(t.State) {
		return copyTask(t), ErrFinished
	}
	now := m.d.Now()
	if t.active() {
		m.sendCancel(m.addWatch(t, now), now)
	}
	t.Finished, t.restoring = now, false
	m.set(t, now, t.Robot, "cancelled", Cancelled, "cancelled by user")
	return copyTask(t), nil
}

// HandleAck processes a cmd_ack from robot. Acks for anything but a task's
// current cmd_id (late, duplicate, or for a cancel) are ignored.
func (m *Manager) HandleAck(robot string, a Ack) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[taskOf(a.CmdID)]
	if t == nil || !t.active() || t.CmdID != a.CmdID || t.Robot != robot {
		m.d.Log.Debug("ack ignored", "robot", robot, "cmd_id", a.CmdID, "status", a.Status)
		return
	}
	now := m.d.Now()
	switch a.Status {
	case "accepted":
		m.confirm(t, now)
	case "done":
		m.legDone(t, now)
	case "failed", "cancelled":
		reason := a.Status
		if a.Message != "" {
			reason += ": " + a.Message
		}
		m.fail(t, now, reason, false)
	default:
		m.d.Log.Warn("unknown ack status", "robot", robot, "cmd_id", a.CmdID, "status", a.Status)
	}
}

// Tick follows running tasks, cancels stray gotos and assigns queued tasks.
// Call it periodically (every 250 ms in fleetd).
func (m *Manager) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.d.Now()
	robots := m.d.Robots.Snapshot()
	byName := make(map[string]*fleet.Robot, len(robots))
	for i := range robots {
		byName[robots[i].Name] = &robots[i]
	}
	for _, t := range m.order {
		if t.active() {
			m.follow(t, byName[t.Robot], now)
		}
	}
	m.spotStrays(robots, now)
	m.chaseWatches(byName, now)
	m.assign(robots, now)
}

// follow checks one active task against its robot's latest state.
func (m *Manager) follow(t *Task, r *fleet.Robot, now time.Time) {
	if t.restoring {
		m.followRestored(t, r, now)
		return
	}
	paused := now.Sub(t.lastFollow)
	if t.lastFollow.IsZero() {
		paused = 0
	}
	t.lastFollow = now
	switch {
	case r == nil || r.Conn == fleet.Stale || r.Conn == fleet.InElevator:
		// Keep the task and pause the clocks: commands and acks cannot get
		// through right now.
		t.waitFrom = now
		if !t.legDeadline.IsZero() {
			t.legDeadline = t.legDeadline.Add(paused)
		}
		return
	case r.Conn == fleet.Offline:
		m.fail(t, now, "robot "+r.Name+" went OFFLINE", true)
		return
	case r.Mode == "ERROR":
		m.fail(t, now, "robot "+r.Name+" in ERROR: "+r.Error, true)
		return
	}
	w := t.Waypoints[t.Leg]
	if t.legDeadline.IsZero() {
		d := math.Hypot(r.X-w.X, r.Y-w.Y) / m.cfg.LegMinSpeed
		t.legDeadline = now.Add(m.cfg.LegSlack + time.Duration(d*float64(time.Second)))
	}
	if now.After(t.legDeadline) {
		m.sendCancel(m.addWatch(t, now), now)
		m.fail(t, now, fmt.Sprintf("leg %d timed out", t.Leg), false)
		return
	}
	if r.TaskID == t.CmdID {
		m.confirm(t, now) // the robot's state is as good as an "accepted"
		return
	}
	// The robot does not report the leg: the goto or its ack was lost, or the
	// robot finished and the final ack was lost. Re-sending the same cmd_id
	// is safe: the adapter does not run it twice and repeats its last ack.
	if now.Sub(t.waitFrom) < m.cfg.AckTimeout {
		return
	}
	if t.resends >= m.cfg.MaxResends {
		m.cooldown[t.Robot] = now.Add(m.cfg.Cooldown)
		m.fail(t, now, fmt.Sprintf("no response to %s after %d resends", t.CmdID, t.resends), true)
		return
	}
	t.resends++
	t.waitFrom = now
	m.d.Log.Info("resending", "task", t.ID, "robot", t.Robot, "cmd_id", t.CmdID, "resend", t.resends)
	m.sendGoto(t, now)
}

// followRestored handles a task that was on a robot when the server stopped.
func (m *Manager) followRestored(t *Task, r *fleet.Robot, now time.Time) {
	if r != nil && r.Mode == "" {
		r = nil // only poses so far; its task_id is not known yet
	}
	if r != nil && r.TaskID == t.CmdID {
		m.confirm(t, now)
		return
	}
	if t.restoreUntil.IsZero() {
		return // no MQTT connection yet
	}
	if r != nil && (r.Conn == fleet.Stale || r.Conn == fleet.InElevator) {
		return // like any STALE robot, it keeps its task until OFFLINE
	}
	if !now.Before(t.restoreUntil) {
		// Its last goto may have been sent just before the restart.
		t.lastSent = now
		m.addWatch(t, now)
		m.requeue(t, now, "not confirmed by "+t.Robot+" after restart")
		return
	}
	// The robot may have finished while we were down and its ack was lost.
	// Asking again with the same cmd_id makes the adapter repeat that ack.
	if r != nil && r.Conn == fleet.Online && !t.probed {
		t.probed = true
		m.sendGoto(t, now)
	}
}

// confirm records a sign that the robot has the current leg.
func (m *Manager) confirm(t *Task, now time.Time) {
	if t.restoring {
		m.d.Log.Info("task confirmed after restart", "task", t.ID, "robot", t.Robot, "cmd_id", t.CmdID)
	}
	t.waitFrom, t.resends, t.restoring = now, 0, false
	if t.State == Assigned {
		if t.Started.IsZero() {
			t.Started = now
		}
		m.set(t, now, t.Robot, "running", Running, t.CmdID)
	}
}

func (m *Manager) legDone(t *Task, now time.Time) {
	t.restoring = false
	if t.Started.IsZero() {
		t.Started = now
	}
	t.Leg++
	if t.Leg >= len(t.Waypoints) {
		t.Finished, t.Error = now, ""
		m.set(t, now, t.Robot, "done", Done, "")
		return
	}
	m.newCmd(t, now)
	m.set(t, now, t.Robot, "leg_done", Assigned, fmt.Sprintf("leg %d/%d, next %s", t.Leg, len(t.Waypoints), t.CmdID))
	m.sendGoto(t, now)
}

// fail ends the current attempt. watch says the robot may still be driving
// the leg (it did not report the failure itself).
func (m *Manager) fail(t *Task, now time.Time, reason string, watch bool) {
	if watch {
		m.addWatch(t, now)
	}
	if !slices.Contains(t.FailedOn, t.Robot) {
		t.FailedOn = append(t.FailedOn, t.Robot)
	}
	t.Attempts++
	if t.Attempts > m.cfg.MaxAttempts {
		t.Error, t.Finished, t.restoring = reason, now, false
		m.set(t, now, t.Robot, "failed", Failed, reason)
		return
	}
	m.requeue(t, now, reason)
}

func (m *Manager) requeue(t *Task, now time.Time, reason string) {
	robot := t.Robot
	t.Robot, t.CmdID, t.Error, t.restoring = "", "", reason, false
	m.set(t, now, robot, "requeued", Queued, reason)
}

func (m *Manager) assign(robots []fleet.Robot, now time.Time) {
	busy := make(map[string]bool)
	for _, t := range m.order {
		if t.active() {
			busy[t.Robot] = true
		}
	}
	var free []fleet.Robot
	for _, r := range robots {
		if m.ineligible(r, busy, now) == "" {
			free = append(free, r)
		}
	}
	for _, t := range m.order {
		if len(free) == 0 {
			return
		}
		if t.State != Queued {
			continue
		}
		i := m.pick(free, t)
		t.Robot = free[i].Name
		free = append(free[:i], free[i+1:]...)
		t.Dispatches++
		m.newCmd(t, now)
		m.set(t, now, t.Robot, "assigned", Assigned, t.CmdID)
		m.sendGoto(t, now)
	}
}

// ineligible says why r cannot take a task, or "" if it can.
func (m *Manager) ineligible(r fleet.Robot, busy map[string]bool, now time.Time) string {
	switch {
	case r.Conn != fleet.Online:
		return "conn " + r.Conn
	case r.Mode != "IDLE":
		return "mode " + r.Mode
	case r.Error != "":
		return "error " + r.Error
	case r.Battery < m.cfg.MinBattery:
		return "battery low"
	case r.TaskID != "":
		return "reports task " + r.TaskID
	case busy[r.Name]:
		return "holds a task"
	case now.Before(m.cooldown[r.Name]):
		return "cooling down after ignoring a command"
	}
	return ""
}

// pick chooses a robot for t: the nearest one that has not failed t, or the
// nearest of all if every free robot has.
func (m *Manager) pick(free []fleet.Robot, t *Task) int {
	w := t.Waypoints[t.Leg]
	best, bestCost := -1, 0.0
	for _, fresh := range []bool{true, false} {
		for i, r := range free {
			if fresh && slices.Contains(t.FailedOn, r.Name) {
				continue
			}
			c := math.Hypot(r.X-w.X, r.Y-w.Y) + m.cfg.FloorPenalty*math.Abs(float64(r.Floor-w.Floor))
			if best < 0 || c < bestCost || c == bestCost && r.Name < free[best].Name {
				best, bestCost = i, c
			}
		}
		if best >= 0 {
			return best
		}
	}
	return best
}

// addWatch starts watching t's current goto on t's robot.
func (m *Manager) addWatch(t *Task, now time.Time) *watch {
	return m.watchCmd(t.Robot, t.CmdID, t.lastSent, now)
}

func (m *Manager) watchCmd(robot, cmdID string, lastSend, now time.Time) *watch {
	for _, w := range m.watches {
		if w.robot == robot && w.cmdID == cmdID {
			return w
		}
	}
	w := &watch{robot: robot, cmdID: cmdID, since: now, lastSend: lastSend}
	if robot != "" && cmdID != "" {
		m.watches = append(m.watches, w)
	}
	return w
}

// chaseWatches cancels watched gotos that robots are executing.
func (m *Manager) chaseWatches(byName map[string]*fleet.Robot, now time.Time) {
	keep := m.watches[:0]
	for _, w := range m.watches {
		r := byName[w.robot]
		switch {
		case now.Sub(w.since) > m.cfg.WatchMax:
			continue // spotStrays still catches it if the robot shows up with it
		case r == nil || r.Mode == "" || r.Conn != fleet.Online:
			keep = append(keep, w) // cannot tell (or reach it) yet
			continue
		case r.TaskID != w.cmdID:
			// Not on it now, but the goto (or a cancel that overtook it) may
			// still be on its way until it expires.
			if now.Before(w.lastSend.Add(m.cfg.CmdTTL + m.cfg.WatchMargin)) {
				keep = append(keep, w)
			}
			continue
		}
		if w.lastCancel.IsZero() || now.Sub(w.lastCancel) >= m.cfg.AckTimeout {
			m.d.Log.Warn("robot runs a task it does not have; cancelling", "robot", w.robot, "cmd_id", w.cmdID)
			m.sendCancel(w, now)
		}
		keep = append(keep, w)
	}
	clear(m.watches[len(keep):])
	m.watches = keep
}

// spotStrays is the safety net behind watches (which live in memory only):
// an ONLINE robot that keeps reporting the cmd_id of a known task it does not
// hold gets watched, and so cancelled. Waiting AckTimeout ignores the moment
// right after a "done", when its state still shows the finished leg.
func (m *Manager) spotStrays(robots []fleet.Robot, now time.Time) {
	for _, r := range robots {
		t := m.tasks[taskOf(r.TaskID)]
		if r.Conn != fleet.Online || r.Mode == "" || t == nil ||
			t.active() && t.Robot == r.Name && t.CmdID == r.TaskID {
			delete(m.sightings, r.Name)
			continue
		}
		s, ok := m.sightings[r.Name]
		if !ok || s.cmdID != r.TaskID {
			m.sightings[r.Name] = sighting{cmdID: r.TaskID, since: now}
			continue
		}
		if now.Sub(s.since) >= m.cfg.AckTimeout {
			m.watchCmd(r.Name, r.TaskID, time.Time{}, now)
		}
	}
}

func (m *Manager) newCmd(t *Task, now time.Time) {
	t.CmdID = fmt.Sprintf("%s.%d.%d.%s", t.ID, t.Leg, t.Dispatches, m.d.Boot)
	t.waitFrom, t.resends, t.probed = now, 0, false
	t.legDeadline, t.lastFollow = time.Time{}, time.Time{}
}

func (m *Manager) sendGoto(t *Task, now time.Time) {
	w := t.Waypoints[t.Leg]
	t.lastSent = now
	m.send(t.Robot, Command{CmdID: t.CmdID, Type: "goto", X: w.X, Y: w.Y, ExpiresAt: m.expires(now)})
}

// sendCancel stops whatever the robot is driving. Its cmd_id embeds the time
// so it never collides with an earlier cancel the adapter remembers.
func (m *Manager) sendCancel(w *watch, now time.Time) {
	w.lastCancel, w.lastSend = now, now
	id := fmt.Sprintf("%s.x%d", w.cmdID, now.UnixMilli())
	m.send(w.robot, Command{CmdID: id, Type: "cancel", ExpiresAt: m.expires(now)})
}

func (m *Manager) send(robot string, c Command) {
	if err := m.d.Sender.Send(robot, c); err != nil {
		m.d.Log.Warn("send failed", "robot", robot, "cmd_id", c.CmdID, "err", err)
	}
}

func (m *Manager) expires(now time.Time) float64 {
	return float64(now.Add(m.cfg.CmdTTL).UnixMilli()) / 1000
}

// set changes state, queues the task for saving and emits. robot is who the
// event is about (requeue has already cleared t.Robot).
func (m *Manager) set(t *Task, now time.Time, robot, kind, to, detail string) {
	from := t.State
	t.State, t.Updated = to, now
	m.save(t)
	m.d.Log.Info("task", "id", t.ID, "event", kind, "robot", robot, "from", from, "to", to, "detail", detail)
	m.d.Emit(Event{Time: now, Task: t.ID, Robot: robot, Kind: kind, From: from, To: to, Detail: detail})
}

// save marks the task for the writer. The lock is never held across a
// database write: HandleAck runs on the MQTT receive goroutine.
func (m *Manager) save(t *Task) {
	if m.d.Store == nil {
		return
	}
	m.dirty[t.ID] = copyTask(t)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Flush writes every pending change, oldest task first. A write that fails
// stays pending (unless a newer copy arrived) and is retried by the next
// Flush: the in-memory state stays authoritative, and a crash can lose only
// changes not yet flushed, which the restart rules recover from.
func (m *Manager) Flush() error {
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	batch := make([]Task, 0, len(m.dirty))
	for _, t := range m.dirty {
		batch = append(batch, t)
	}
	clear(m.dirty)
	m.mu.Unlock()

	sort.Slice(batch, func(i, j int) bool { return batch[i].Seq < batch[j].Seq })
	var firstErr error
	for _, t := range batch {
		if err := m.d.Store.Save(t); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("save %s: %w", t.ID, err)
			}
			m.mu.Lock()
			if _, newer := m.dirty[t.ID]; !newer {
				m.dirty[t.ID] = t
			}
			m.mu.Unlock()
		}
	}
	return firstErr
}

// RunWriter flushes changes as they happen, retrying failures every second,
// until ctx ends; then it flushes once more.
func (m *Manager) RunWriter(ctx context.Context) {
	if m.d.Store == nil {
		return
	}
	retry := time.NewTicker(time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := m.Flush(); err != nil {
				m.d.Log.Error("final task flush", "err", err)
			}
			return
		case <-m.wake:
		case <-retry.C:
		}
		if err := m.Flush(); err != nil {
			m.d.Log.Error("task flush (will retry)", "err", err)
		}
	}
}

// taskOf returns the task id part of a cmd_id ("t-3.0.1.boot" -> "t-3").
func taskOf(cmdID string) string {
	id, _, _ := strings.Cut(cmdID, ".")
	return id
}

func copyTask(t *Task) Task {
	c := *t
	c.Waypoints = append([]Waypoint(nil), t.Waypoints...)
	c.FailedOn = append([]string(nil), t.FailedOn...)
	return c
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
