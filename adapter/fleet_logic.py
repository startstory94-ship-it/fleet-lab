"""Pure adapter logic, kept free of ROS and MQTT so it can be unit tested.

fleet_adapter.py wires these pieces to rclpy and paho-mqtt.
"""

import collections
import time
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


# ---------------------------------------------------------------- elevator rides
#
# The robot side of docs/ELEVATOR.md (section 5). RobotRide is fed the ride
# command, turns, the car's state, the robot's elevator-move results and the
# clock, and answers with actions for the adapter to carry out, in order:
#
#   ("ack", cmd_id, status, message, extra)  fleet/<robot>/cmd_ack (extra: {"floor": n} or None)
#   ("state", override)   publish fleet/<robot>/state now. override (mode,
#                         task_id, silence_s) is sent although the link is
#                         about to go; None means the usual state
#   ("pose",)             publish fleet/<robot>/pose now (the floor changed)
#   ("event", body)       fleet/<robot>/event
#   ("zone", car, body)   elevator/<car>/zone
#   ("move", body)        /<robot>/elevator_move to the robot
#
# While `silent` is set (from the boarding turn until LINK_BACK_S after the
# robot is out of the car) the robot has no Wi-Fi: the adapter publishes
# nothing on fleet/* for it and drops commands for it. The elevator channel
# keeps working. Robot events made during that time are queued and sent,
# in seq order, when the link is back.

# How a ride can end (docs/ELEVATOR.md 5 and 6):
#   - normally: exit turn -> out of the car -> LINK_BACK_S later Wi-Fi is
#     back -> pose, state, queued events, "done" with the floor;
#   - taken out by a human while the trip still runs (the robot reports
#     in_car false with no move under way): event "rescued", then as above;
#   - the trip ends with the robot still inside (operator release, trip
#     done/aborted, retained trip cleared, or nothing heard of the trip for
#     TRIP_LOST_S): Wi-Fi back at once, event "exit_failed", the ride is
#     acked "failed" and the robot reports ERROR ("in the car, needs a human")
#     until it is taken out; then event "rescued" and its own state again.
# An elevator move the robot does not report within turn_timeout_s +
# move_extra (lost move, restarted robot) ends as "failed"; a robot that
# still reports the move as running (slow, or stopped by a fault in the
# door zone) is waited for, because it is still in the door zone.

LOBBY_TOL = 0.5      # m, "at the lobby" for accepting a ride
LINK_BACK_S = 1.0    # s after leaving the car until Wi-Fi is back
MOVE_EXTRA_S = 30.0  # s added to turn_timeout_s before an unreported move fails
TRIP_LOST_S = 60.0   # s without the trip (republished every 10 s) or a turn: the trip is gone
DOOR_DX = 1.0        # car door, from the lobby (the car is along +x of its lobby)
SLOT0_DX = 1.8       # slot 0 (door side), from the lobby
SLOT_DX = 0.8        # between slots
REMEMBER = 256       # ride cmd_ids and finished trips remembered
ENDED_STAGES = ("done", "aborted")

WAITING, BOARDING, RIDING, EXITING, OUT = "waiting", "boarding", "riding", "exiting", "out"


def car_door(lobby):
    return [lobby["x"] + DOOR_DX, lobby["y"]]


def car_slot(lobby, slot):
    return [lobby["x"] + SLOT0_DX + SLOT_DX * slot, lobby["y"]]


def _remember(od, key):
    od[key] = True
    od.move_to_end(key)
    while len(od) > REMEMBER:
        od.popitem(last=False)


class RobotRide:
    """One robot's elevator rides (pure; see the comment above)."""

    def __init__(self, robot, boot, ignore_turns=0, move_base=0, wall=time.time, move_extra=MOVE_EXTRA_S):
        self.robot, self.boot, self.wall = robot, boot, wall
        self.move_extra = move_extra
        self.cmd_id = None
        self.plan = None
        self.stage = None
        self.results = {}          # turn n -> zone body sent for it
        self.move = None           # {"id", "n", "action", "floor", "deadline"} of the move under way
        self.silent = False
        self.link_at = None
        self.car_floor = None
        self.trip_seen = 0.0       # last time the trip or one of its turns was heard
        self.trip_over = ""        # the trip ended while a move was under way
        self.stranded = ""         # car the robot was left in when its trip ended
        self.queue = []            # robot events made without a link
        self.ev_seq = 0
        self.zone_seq = 0
        self.move_seq = move_base  # move ids, unique per adapter run
        self.ride_ids = collections.OrderedDict()
        self.ended = collections.OrderedDict()
        self.ignore_turns = ignore_turns  # failure injection: this many own turns are "lost"

    @property
    def holding(self):
        return self.stage is not None

    @property
    def trip(self):
        return self.plan["trip"] if self.plan else ""

    @property
    def elevator(self):
        return self.plan["elevator"] if self.plan else ""

    def overlay(self, robot_state=None):
        """mode/task_id/silence_s (and error) to report instead of the
        robot's own, or None."""
        if self.stage is None:
            if self.stranded or (robot_state or {}).get("in_car"):
                car = self.stranded or "a car"
                return {"mode": "ERROR", "task_id": "", "silence_s": 0,
                        "error": f"in elevator {car} without a ride: needs a human"}
            return None
        if self.stage == OUT:
            return None
        mode = {WAITING: "WAITING_ELEVATOR", BOARDING: "BOARDING", RIDING: "RIDING", EXITING: "EXITING"}[self.stage]
        silence = self.plan["silence_s"] if self.stage != WAITING else 0
        return {"mode": mode, "task_id": self.cmd_id, "silence_s": silence}

    # ---- commands (fleet link) ----

    def accept(self, cmd_id, plan, robot_state, pos, now=0.0):
        """A new ride command (the caller already checked cmd_id for repeats
        and expiry). robot_state is the robot's last robot_state dict, pos its
        (x, y)."""
        def fail(msg):
            return [("ack", cmd_id, "failed", msg, None)]
        if not plan:
            return fail("ride without a plan")
        try:
            trip, car, origin = str(plan["trip"]), str(plan["elevator"]), int(plan["origin"])
            slot, silence = int(plan["slot"]), float(plan["silence_s"])
            timeout = float(plan.get("turn_timeout_s") or 15)
            lobby = {"x": float(plan["lobby"]["x"]), "y": float(plan["lobby"]["y"])}
        except (KeyError, TypeError, ValueError) as e:
            return fail(f"bad ride plan ({e!r})")
        if not trip or not car or slot < 0 or silence <= 0:
            return fail("bad ride plan")
        if robot_state is None or pos is None:
            return fail("robot state not known yet")
        if robot_state.get("mode") == "ERROR":
            return fail(f"in ERROR ({robot_state.get('error', '')})")
        if self.stage not in (None, WAITING):
            return fail(f"already riding {self.trip}")
        floor = robot_state.get("floor", 1)
        if floor != origin:
            return fail(f"on floor {floor}, the ride starts on {origin}")
        if robot_state.get("in_car") or self.stranded:
            return fail("in a car")
        d = math.hypot(pos[0] - lobby["x"], pos[1] - lobby["y"])
        if d > LOBBY_TOL:
            return fail(f"not at the lobby ({d:.1f} m away)")
        out = []
        if self.stage == WAITING:
            out.append(("ack", self.cmd_id, "cancelled", f"replaced by {cmd_id}", None))
            out += self._event("ride_cancelled", floor, f"replaced by {cmd_id}")
            self._clear()
        self.cmd_id, self.stage = cmd_id, WAITING
        self.plan = dict(plan, lobby=lobby, silence_s=silence, turn_timeout_s=timeout)
        self.results, self.move, self.silent, self.link_at, self.car_floor = {}, None, False, None, None
        self.trip_seen, self.trip_over = now, ""
        _remember(self.ride_ids, cmd_id)
        out.append(("ack", cmd_id, "accepted", "", None))
        out += self._event("ride_accepted", floor, f"exit {plan.get('exit_floor')}, slot {slot}")
        out.append(("state", None))
        return out

    def cancel(self, cmd_id, target):
        """A cancel. Returns the actions, or None when it is not about rides
        (then it stops a goto as before)."""
        if target and target == self.cmd_id and self.stage is not None:
            if self.stage != WAITING:
                return [("ack", cmd_id, "failed", f"ride {target} already started ({self.stage})", None)]
            floor = self.plan["origin"]
            out = [("ack", self.cmd_id, "cancelled", "ride cancelled at the lobby", None)]
            out += self._event("ride_cancelled", floor, f"by {cmd_id}")
            self._clear()
            return out + [("ack", cmd_id, "accepted", "", None), ("state", None)]
        if target and target in self.ride_ids:
            return [("ack", cmd_id, "failed", f"{target} is not the current ride", None)]
        if self.stage is not None:
            return [("ack", cmd_id, "failed", f"holding ride {self.cmd_id}; the cancel is for {target or 'nothing'}", None)]
        return None

    def _clear(self):
        if self.plan:
            _remember(self.ended, self.plan["trip"])
        self.cmd_id, self.plan, self.stage = None, None, None
        self.results, self.move, self.link_at, self.car_floor, self.trip_over = {}, None, None, None, ""

    # ---- elevator channel ----

    def turn(self, t, now=0.0):
        """A turn from elevator/<car>/turn."""
        if self.stage is None or t.get("trip") != self.trip or t.get("robot") != self.robot:
            return []  # not ours: another trip, another robot, or the ride is over
        try:
            n, action, floor = int(t["n"]), t["action"], int(t["floor"])
        except (KeyError, TypeError, ValueError):
            return []
        self.trip_seen = now
        if self.ignore_turns > 0:
            self.ignore_turns -= 1
            return []
        if self.move is not None:
            # In the door zone on move.n: answer nothing now, not even an old
            # n again (its answer would take the robot out of the car's count).
            return []
        if n in self.results:
            return [("zone", self.elevator, self.results[n])]  # answered: answer again
        if action == "board" and self.stage in (RIDING, EXITING, OUT) or action == "exit" and self.stage == OUT:
            # Already done (the answer was lost or late): clear for this n.
            return self._answer(n, action, floor, "clear")
        if action == "exit" and self.stage == WAITING:
            return self._answer(n, action, floor, "failed", "not in the car")
        lobby = self.plan["lobby"]
        if action == "board" and self.stage == WAITING:
            out = self._event("boarding", floor, f"turn {n}")
            out.append(("state", {"mode": "BOARDING", "task_id": self.cmd_id, "silence_s": self.plan["silence_s"]}))
            self.stage, self.silent, self.car_floor = BOARDING, True, floor
            path = [car_door(lobby), car_slot(lobby, int(self.plan["slot"]))]
        elif action == "exit" and self.stage == RIDING:
            try:
                exit_lobby = {"x": float(t["lobby"]["x"]), "y": float(t["lobby"]["y"])}
            except (KeyError, TypeError, ValueError):
                exit_lobby = self.plan.get("exit_lobby") or lobby
            out = []
            self.stage = EXITING
            path = [car_door(exit_lobby), [exit_lobby["x"], exit_lobby["y"]]]
        else:
            return []
        self.move_seq += 1
        self.move = {"id": self.move_seq, "n": n, "action": action, "floor": floor,
                     "deadline": now + self.plan["turn_timeout_s"] + self.move_extra}
        out.append(("zone", self.elevator, self._zone(n, action, "enter", floor)))
        body = {"id": self.move_seq, "op": action, "path": path}
        if action == "exit":
            body["floor"] = floor
        out.append(("move", body))
        return out

    def car_state(self, s):
        """The car's state: while riding, the robot is on the car's floor once
        the car stands there."""
        if self.stage != RIDING or s.get("moving") or s.get("floor") is None or s.get("floor") == self.car_floor:
            return []
        self.car_floor = s["floor"]
        self.move_seq += 1
        return [("move", {"id": self.move_seq, "op": "floor", "floor": self.car_floor})]

    def trip_msg(self, msg, now=0.0):
        """The retained trip (None: cleared). A trip that ended, was cleared
        or was replaced while the robot is still in the car leaves it there."""
        if self.stage is None:
            return []
        if isinstance(msg, dict) and msg.get("trip") == self.trip and msg.get("stage") not in ENDED_STAGES:
            self.trip_seen = now
            return []
        if self.stage == WAITING:
            return []  # before boarding: the server's cancel ends the ride
        if isinstance(msg, dict) and msg.get("trip") == self.trip:
            why = f"trip {self.trip} {msg.get('stage')}" + (f" ({msg['detail']})" if msg.get("detail") else "")
        elif msg is None:
            why = f"trip {self.trip} cleared"
        else:
            why = f"trip {self.trip} replaced by {msg.get('trip')}"
        self.trip_over = self.trip_over or why
        return self._settle(now)

    # ---- robot ----

    def move_result(self, move_id, status, detail, now):
        """The robot's report of an elevator move (robot_state "move")."""
        if self.move is None or move_id != self.move["id"] or status not in ("done", "failed"):
            return []
        m, self.move = self.move, None
        return self._moved(m, status, detail, now) + self._settle(now)

    def _moved(self, m, status, detail, now):
        n, action, floor = m["n"], m["action"], m["floor"]
        if action == "board":
            if status == "done":
                self.stage = RIDING
                return self._answer(n, action, floor, "clear") + \
                    self._event("boarded", floor, f"slot {self.plan['slot']}")
            # Not in: back at the lobby, the link is back. The server drops it from the trip.
            self.stage, self.silent = WAITING, False
            out = self._answer(n, action, floor, "failed", detail or "could not board")
            out += self._event("board_failed", floor, detail)
            return out + [("state", None)] + self._flush()
        if status == "done":
            self.stage, self.link_at = OUT, now + LINK_BACK_S
            return self._answer(n, action, floor, "clear") + self._event("exited", floor, "")
        self.stage = RIDING  # back in its slot; waits for the next exit turn
        return self._answer(n, action, floor, "failed", detail or "could not exit") + \
            self._event("exit_failed", floor, detail)

    def tick(self, now, robot_floor, robot_state=None):
        """Clocks: an unreported move, the link back after the exit, a robot
        taken out by a human, a trip that is gone. robot_state is the robot's
        live robot_state (None if it is not publishing)."""
        out = []
        rs = robot_state or {}
        if self.stranded and robot_state is not None and rs.get("in_car") is False:
            car, self.stranded = self.stranded, ""
            out += self._event("rescued", robot_floor, f"taken out of {car}")
            out.append(("state", None))
        if self.stage is None:
            return out
        if self.move is not None and now >= self.move["deadline"]:
            rep = rs.get("move") if isinstance(rs.get("move"), dict) else {}
            if rep.get("id") == self.move["id"] and rep.get("status") == "running":
                # Alive and still on it (slow, or stopped in the door zone): it is in the zone.
                self.move["deadline"] = now + 1.0
            else:
                m, self.move = self.move, None
                out += self._moved(m, "failed", "robot did not finish the move", now)
        if self.stage == RIDING and self.move is None and robot_state is not None and rs.get("in_car") is False:
            # Taken out by a human while the trip runs: out, on this floor.
            out += self._event("rescued", robot_floor, "taken out of the car")
            self.stage, self.link_at = OUT, now
        if self.stage == RIDING and not self.trip_over and now - self.trip_seen > TRIP_LOST_S:
            self.trip_over = f"nothing heard of trip {self.trip} for {TRIP_LOST_S:.0f} s"
        out += self._settle(now, robot_floor)
        if self.stage != OUT or now < self.link_at:
            return out
        cmd_id = self.cmd_id
        self.silent = False
        self._clear()
        out += [("pose",), ("state", None)] + self._flush()
        out.append(("ack", cmd_id, "done", "", {"floor": robot_floor}))
        return out

    def _settle(self, now, robot_floor=None):
        """The trip is over and the robot still in the car (no move under
        way): link back, the ride fails, the robot waits for a human."""
        if not self.trip_over or self.stage != RIDING or self.move is not None:
            return []
        floor = robot_floor if robot_floor is not None else self.car_floor
        why, cmd_id, car = self.trip_over, self.cmd_id, self.elevator
        out = self._event("exit_failed", floor, f"{why}; still in the car, needs a human")
        self.silent = False
        self._clear()
        self.stranded = car
        out += [("pose",), ("state", None)] + self._flush()
        out.append(("ack", cmd_id, "failed", f"{why}; robot still in the car", {"floor": floor}))
        return out

    # ---- helpers ----

    def _answer(self, n, action, floor, event, reason=""):
        z = self._zone(n, action, event, floor, reason)
        self.results[n] = z
        return [("zone", self.elevator, z)]

    def _zone(self, n, action, event, floor, reason=""):
        self.zone_seq += 1
        z = {"trip": self.trip, "n": n, "robot": self.robot, "action": action, "event": event,
             "floor": floor, "seq": self.zone_seq, "stamp": round(self.wall(), 3)}
        if reason:
            z["reason"] = reason
        return z

    def _event(self, name, floor, detail):
        self.ev_seq += 1
        e = {"boot": self.boot, "seq": self.ev_seq, "stamp": round(self.wall(), 3), "event": name,
             "trip": self.trip, "cmd_id": self.cmd_id or "", "floor": floor, "detail": detail or ""}
        if self.silent:
            self.queue.append(e)
            return []
        return [("event", e)]

    def _flush(self):
        out = [("event", e) for e in sorted(self.queue, key=lambda e: e["seq"])]
        self.queue = []
        return out
