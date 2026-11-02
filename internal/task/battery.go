package task

import (
	"fmt"
	"strings"
	"time"

	"fleetlab/internal/fleet"
)

// Battery policy.
//
// A robot whose battery drops below LowBattery needs charge: it takes no new
// task until its battery is back at ResumeBattery, so it does not flap around
// the low mark. A task it already holds is finished first. Once it is ONLINE,
// IDLE, error-free, holds no task and is not being watched for an old goto,
// it is sent a dock command.
//
// Below CriticalBattery a robot that is driving a task (reports its cmd_id)
// hands it back: it is cancelled and watched like any robot a task is taken
// from, and the task is requeued, without using an attempt, once the robot
// has stopped (its final ack, or no report of the leg and no answer to the
// same cmd_id). Until then the task stays on the robot, so two robots never
// drive it. A robot that does not report the leg may have just finished it
// with the "done" still on its way; follow handles it as usual.
//
// A charger that stops below ResumeBattery twice in a row is accepted: at
// LowBattery or more the flag clears; below it the robot backs off like a
// robot that would not dock.
//
// A robot whose state has no battery is never flagged or docked and may take
// tasks: there is nothing to judge.
//
// Flags live in memory. After a restart they are recomputed from each robot's
// first state report: below LowBattery, or below ResumeBattery while DOCKING
// or CHARGING, means it needs charge.

// charge is the battery policy's view of one robot.
type charge struct {
	flagged  bool      // needs charge
	docking  bool      // last report was DOCKING or CHARGING
	charging bool      // last report was CHARGING
	short    int       // charges that ended below ResumeBattery in this flagged period
	dockID   string    // last dock sent while waiting for DOCKING
	dockSent time.Time // when dockID was sent
	dockLand time.Time // the last dock sent may still reach the robot until then
	tries    int       // docks sent without the robot docking
	backoff  time.Time // no dock before this after MaxDockTries
}

// reset starts a new flagged (or unflagged) period. What the robot was last
// seen doing, and a dock still on its way, carry over.
func (c *charge) reset(flagged bool) {
	*c = charge{flagged: flagged, docking: c.docking, charging: c.charging, dockLand: c.dockLand}
}

func docking(r fleet.Robot) bool { return r.Mode == "DOCKING" || r.Mode == "CHARGING" }

// needsCharge reports whether r is flagged. Robots not seen by the policy
// yet are judged by their battery alone.
func (m *Manager) needsCharge(r fleet.Robot) bool {
	if !r.BatteryKnown {
		return false
	}
	if c := m.charge[r.Name]; c != nil {
		return c.flagged
	}
	return r.Mode != "" && r.Battery < m.cfg.LowBattery
}

// dockMayLand reports whether a dock sent to the robot may still arrive. A
// goto sent now could be ended by it ("cancelled"), costing an attempt.
func (m *Manager) dockMayLand(robot string, now time.Time) bool {
	c := m.charge[robot]
	return c != nil && now.Before(c.dockLand)
}

func (m *Manager) watching(robot string) bool {
	for _, w := range m.watches {
		if w.robot == robot {
			return true
		}
	}
	return false
}

// checkBattery updates the flags, hands tasks back from robots at the
// critical level and docks robots that need charge.
func (m *Manager) checkBattery(robots []fleet.Robot, now time.Time) {
	held := make(map[string]*Task)
	for _, t := range m.order {
		if t.active() {
			held[t.Robot] = t
		}
	}
	for _, r := range robots {
		if r.Mode == "" {
			continue // only poses so far; the battery is not known
		}
		if !r.BatteryKnown {
			if !m.noBattery[r.Name] {
				m.noBattery[r.Name] = true
				m.d.Log.Warn("robot reports no battery; battery policy skips it", "robot", r.Name)
			}
			continue
		}
		delete(m.noBattery, r.Name)
		c := m.charge[r.Name]
		if c == nil {
			c = &charge{docking: docking(r), charging: r.Mode == "CHARGING",
				flagged: r.Battery < m.cfg.LowBattery || docking(r) && r.Battery < m.cfg.ResumeBattery}
			m.charge[r.Name] = c
			if c.flagged {
				m.batteryEvent(now, r.Name, "low", fmt.Sprintf("%.1f%% at first report (%s)", r.Battery, r.Mode))
			}
		}
		switch {
		case !c.flagged && r.Battery < m.cfg.LowBattery:
			c.reset(true)
			m.batteryEvent(now, r.Name, "low", fmt.Sprintf("%.1f%%", r.Battery))
		case c.flagged && r.Battery >= m.cfg.ResumeBattery:
			c.reset(false)
			m.batteryEvent(now, r.Name, "charged", fmt.Sprintf("%.1f%%", r.Battery))
		case c.flagged && c.charging && r.Mode == "IDLE":
			m.chargeStopped(r, c, now)
		}
		if c.flagged && docking(r) && !c.docking {
			c.dockID, c.tries = "", 0
			m.batteryEvent(now, r.Name, "docking", fmt.Sprintf("%s at %.1f%%", r.Mode, r.Battery))
		}
		c.docking, c.charging = docking(r), r.Mode == "CHARGING"
		if !c.flagged {
			continue
		}
		if t := held[r.Name]; t != nil {
			// A restored task the robot reports was confirmed by follow this tick;
			// one it does not report is left to the restore probe.
			if !t.handBack && r.Conn == fleet.Online && r.TaskID == t.CmdID &&
				r.Battery < m.cfg.CriticalBattery {
				m.handBack(t, r, now)
			}
			continue
		}
		m.dock(r, c, now)
	}
}

// chargeStopped handles a flagged robot that went from CHARGING to IDLE
// below ResumeBattery: sent back the first time, accepted the second.
func (m *Manager) chargeStopped(r fleet.Robot, c *charge, now time.Time) {
	c.short++
	if c.short < 2 {
		return // dock below sends it back
	}
	if r.Battery >= m.cfg.LowBattery {
		c.reset(false)
		m.batteryEvent(now, r.Name, "charged",
			fmt.Sprintf("%.1f%%, charging stopped below %.0f%% twice; taking tasks again", r.Battery, m.cfg.ResumeBattery))
		return
	}
	c.short, c.backoff, c.tries, c.dockID = 0, now.Add(m.cfg.DockCooldown), 0, ""
	m.batteryEvent(now, r.Name, "dock_failed",
		fmt.Sprintf("charging stopped at %.1f%% twice, next try in %s", r.Battery, m.cfg.DockCooldown))
}

// handBack starts taking t away from a robot at the critical level. The
// task stays on the robot until it has stopped; see followHandBack.
func (m *Manager) handBack(t *Task, r fleet.Robot, now time.Time) {
	t.handBack, t.Error = true, fmt.Sprintf("robot %s battery critical (%.1f%%)", r.Name, r.Battery)
	t.waitFrom, t.resends = now, 0
	m.d.Log.Info("battery critical; cancelling", "task", t.ID, "robot", r.Name, "cmd_id", t.CmdID, "battery", r.Battery)
	m.sendCancel(m.addWatch(t, now), now)
}

// followHandBack requeues a handed-back task once its robot has stopped. Its
// final ack (cancelled, failed or done) settles it in HandleAck; if that ack
// is lost, the same cmd_id is sent again so the adapter repeats it (the
// adapter never runs a cmd_id twice, and this robot did receive it).
func (m *Manager) followHandBack(t *Task, r *fleet.Robot, now time.Time) {
	switch {
	case r == nil || r.Conn == fleet.Stale || r.Conn == fleet.InElevator:
		t.waitFrom = now
	case r.Conn == fleet.Offline || r.Mode == "ERROR":
		m.requeue(t, now, t.Error) // still watched
	case r.TaskID == t.CmdID:
		t.waitFrom = now // still driving; chaseWatches repeats the cancel
	case now.Sub(t.waitFrom) < m.cfg.AckTimeout:
	case t.resends >= m.cfg.MaxResends:
		m.requeue(t, now, t.Error)
	default:
		t.resends++
		t.waitFrom = now
		m.sendGoto(t, now)
	}
}

// dock sends r to its dock if it can go now, re-sending while it does not
// show DOCKING and backing off after MaxDockTries.
func (m *Manager) dock(r fleet.Robot, c *charge, now time.Time) {
	if c.docking || r.Conn != fleet.Online || r.Mode != "IDLE" || r.Error != "" || r.TaskID != "" ||
		now.Before(c.backoff) || m.watching(r.Name) {
		return
	}
	if c.dockID != "" && now.Sub(c.dockSent) < m.cfg.DockTimeout {
		return // wait for DOCKING
	}
	if c.tries >= m.cfg.MaxDockTries {
		c.backoff, c.tries, c.dockID = now.Add(m.cfg.DockCooldown), 0, ""
		m.batteryEvent(now, r.Name, "dock_failed",
			fmt.Sprintf("not DOCKING after %d docks, next try in %s", m.cfg.MaxDockTries, m.cfg.DockCooldown))
		return
	}
	m.docks++
	c.tries++
	c.dockID, c.dockSent = fmt.Sprintf("dock.%s.%d.%s", r.Name, m.docks, m.d.Boot), now
	c.dockLand = now.Add(m.cfg.CmdTTL + m.cfg.WatchMargin)
	m.send(r.Name, Command{CmdID: c.dockID, Type: "dock", ExpiresAt: m.expires(now)})
	m.batteryEvent(now, r.Name, "dock_sent", fmt.Sprintf("%s try %d at %.1f%%", c.dockID, c.tries, r.Battery))
}

func isDock(cmdID string) bool { return strings.HasPrefix(cmdID, "dock.") }

// dockAck logs the answer to a dock. A refused dock is re-sent after
// DockTimeout like one that got no answer.
func (m *Manager) dockAck(robot string, a Ack) {
	if a.Status == "failed" {
		m.d.Log.Warn("dock refused", "robot", robot, "cmd_id", a.CmdID, "message", a.Message)
		return
	}
	m.d.Log.Debug("dock ack", "robot", robot, "cmd_id", a.CmdID, "status", a.Status)
}

func (m *Manager) batteryEvent(now time.Time, robot, kind, detail string) {
	m.d.Log.Info("battery", "robot", robot, "event", kind, "detail", detail)
	m.d.Emit(Event{Type: "battery", Time: now, Robot: robot, Kind: kind, Detail: detail})
}
