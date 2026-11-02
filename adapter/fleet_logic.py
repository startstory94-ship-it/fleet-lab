"""Pure adapter logic, kept free of ROS and MQTT so it can be unit tested.

fleet_adapter.py wires these pieces to rclpy and paho-mqtt.
"""

import collections
import math

POSE_MAX_HZ = 5.0
POSE_IDLE_PERIOD = 1.0      # s, pose heartbeat while standing still
POSE_DEADBAND_M = 0.05
POSE_DEADBAND_RAD = math.radians(2.0)
STATE_PERIOD = 1.0          # s, state heartbeat
ROBOT_SILENT_AFTER = 1.5    # s without robot_state -> stop forwarding (robot_state is 2 Hz)
GOAL_START_TIMEOUT = 3.0    # s for the robot to show the commanded goal
GOAL_MATCH_TOL = 0.01       # m, commanded goal vs goal reported by the robot
ARRIVE_TOL = 0.10           # m, final position vs goal for "done"


def angle_diff(a, b):
    return abs(math.atan2(math.sin(a - b), math.cos(a - b)))


def pose_due(last, now, x, y, yaw):
    """Should a pose be sent now? last is (time, x, y, yaw) of the last one sent, or None.
    Send when moved 5 cm or turned 2 degrees, else once a second as a heartbeat."""
    if last is None:
        return True
    t0, x0, y0, yaw0 = last
    moved = math.hypot(x - x0, y - y0) >= POSE_DEADBAND_M or angle_diff(yaw, yaw0) >= POSE_DEADBAND_RAD
    return moved or now - t0 >= POSE_IDLE_PERIOD


def state_due(last, now, key):
    """Should a state be sent now? last is (time, key) of the last one sent, or None.
    Send immediately when key changes, else once a second as a heartbeat."""
    if last is None:
        return True
    t0, key0 = last
    return key != key0 or now - t0 >= STATE_PERIOD


def is_live(last_rx, now):
    """The robot is publishing if its last robot_state is recent. Forwarding
    cached data from a silent robot would hide the silence from the server."""
    return last_rx is not None and now - last_rx <= ROBOT_SILENT_AFTER


class CommandBook:
    """Remembers handled cmd_ids and the last ack sent for each.

    MQTT QoS 1 may deliver a command twice, and the server re-sends when an ack
    is lost. A duplicate must not run again, but it must get the ack again, or
    the server would time out on a command the robot is already executing.
    """

    NEW, DUPLICATE, EXPIRED = "new", "duplicate", "expired"

    def __init__(self, size=256):
        self.size = size
        self._acks = collections.OrderedDict()  # cmd_id -> last ack dict or None

    def check(self, cmd_id, expires_at, now_wall):
        if cmd_id in self._acks:
            return self.DUPLICATE
        self._acks[cmd_id] = None
        while len(self._acks) > self.size:
            self._acks.popitem(last=False)
        if expires_at is not None and now_wall > expires_at:
            return self.EXPIRED
        return self.NEW

    def record(self, cmd_id, ack):
        if cmd_id in self._acks:
            self._acks[cmd_id] = ack

    def last_ack(self, cmd_id):
        """The ack to repeat for a duplicate, or None if none was sent yet
        (it will be sent when the first copy finishes)."""
        return self._acks.get(cmd_id)


def service_outcome(future):
    """(status, message) for a finished rclpy service future, never raising."""
    exc = future.exception()
    if exc is not None:
        return "failed", f"service call raised {type(exc).__name__}: {exc}"
    res = future.result()
    if res is None:
        return "failed", "no response"
    return ("accepted" if res.success else "failed"), res.message


def _goal_matches(goal, target):
    return goal is not None and len(goal) >= 2 and \
        math.hypot(goal[0] - target[0], goal[1] - target[1]) <= GOAL_MATCH_TOL


class GoalTracker:
    """Follows one goto command from accepted to done, failed or cancelled.

    The robot's state stream is asynchronous: right after a goal is published,
    a state message from before the goal (still IDLE, no goal) can arrive.
    Completion is therefore only judged after the robot has reported the
    commanded goal as its own ("started").
    """

    def __init__(self):
        self.cmd_id = None
        self.target = None
        self.started = False
        self.sent_at = 0.0

    @property
    def active_id(self):
        return self.cmd_id or ""

    def start(self, cmd_id, x, y, now):
        """Track a new goal. Returns the cmd_id it preempts, if any."""
        preempted = self.cmd_id
        self.cmd_id, self.target, self.started, self.sent_at = cmd_id, (x, y), False, now
        return preempted

    def cancel(self, reason):
        """End the tracked goal because a cancel or dock was accepted. Without
        this, a cancel that lands before the robot's next 2 Hz state (so the
        goal was never seen as started) would end as "did not take the goal".
        Returns (cmd_id, "cancelled", reason), or None if nothing is tracked."""
        if self.cmd_id is None:
            return None
        return self._finish("cancelled", reason)

    def _finish(self, status, message):
        out = (self.cmd_id, status, message)
        self.cmd_id, self.target, self.started = None, None, False
        return out

    def update(self, state, pos, now):
        """Feed a robot_state dict and current (x, y). Returns (cmd_id, status,
        message) when the tracked goal ends, else None."""
        if self.cmd_id is None:
            return None
        mode, goal = state.get("mode"), state.get("goal")

        at_target = pos is not None and \
            math.hypot(pos[0] - self.target[0], pos[1] - self.target[1]) <= ARRIVE_TOL

        if not self.started:
            if _goal_matches(goal, self.target):
                self.started = True
            elif goal is None and mode == "IDLE" and at_target:
                # A goal at (or very near) the current spot can finish between
                # two 2 Hz state messages, so "started" is never observed.
                return self._finish("done", "")
            elif mode == "ERROR":
                return self._finish("failed", f"robot in ERROR: {state.get('error', '')}")
            elif now - self.sent_at > GOAL_START_TIMEOUT:
                return self._finish("failed", "robot did not take the goal")
            else:
                return None  # state from before the goal; ignore

        if mode == "ERROR":
            return self._finish("failed", f"robot in ERROR: {state.get('error', '')}")
        if goal is not None and not _goal_matches(goal, self.target):
            return self._finish("cancelled", "preempted by another goal")
        if goal is None:
            if mode == "IDLE" and at_target:
                return self._finish("done", "")
            return self._finish("cancelled", f"stopped before the goal ({mode})")
        return None
