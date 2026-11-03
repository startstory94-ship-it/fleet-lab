"""Unit tests for RobotRide, the robot side of docs/ELEVATOR.md.
Run: python -m pytest adapter"""

import pytest

from fleet_logic import (LINK_BACK_S, LOBBY_TOL, MOVE_EXTRA_S, TRIP_LOST_S, RobotRide, car_door,
                         car_slot)

LOBBY = {"x": 10.0, "y": 0.0}


def plan(**kw):
    p = {"trip": "E1-k3-1", "elevator": "E1", "origin": 1, "direction": "up", "exit_floor": 3, "board": 2,
         "slot": 0, "boarding": ["robot_2", "robot_3", "robot_1"], "stops": [3, 5, 7], "silence_s": 58,
         "turn_timeout_s": 15, "lobby": dict(LOBBY), "exit_lobby": dict(LOBBY)}
    p.update(kw)
    return p


def robot_state(**kw):
    s = {"mode": "IDLE", "floor": 1, "in_car": False, "error": ""}
    s.update(kw)
    return s


def turn(n, action, floor=1, robot="robot_1", trip="E1-k3-1", lobby=None):
    return {"trip": trip, "n": n, "robot": robot, "action": action, "floor": floor,
            "lobby": lobby or dict(LOBBY), "timeout_s": 15}


def kinds(acts):
    return [a[0] for a in acts]


def only(acts, kind):
    return [a for a in acts if a[0] == kind]


def acks(acts):
    return [(a[1], a[2]) for a in acts if a[0] == "ack"]


def zones(acts):
    return [(a[2]["n"], a[2]["action"], a[2]["event"]) for a in acts if a[0] == "zone"]


def events(acts):
    return [a[1]["event"] for a in acts if a[0] == "event"]


class Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


def accepted(**kw):
    r = RobotRide("robot_1", "boot1", wall=Clock())
    acts = r.accept("R1", plan(**kw), robot_state(), (LOBBY["x"], LOBBY["y"]))
    assert acks(acts) == [("R1", "accepted")]
    return r


def board(r, n=1):
    acts = r.turn(turn(n, "board"))
    move = only(acts, "move")[0][1]
    return acts, move


def boarded(r, n=1):
    _, move = board(r, n)
    return r.move_result(move["id"], "done", "", 0.0)


# ---- accept ----

def test_accept_reports_waiting_with_the_ride_as_task():
    r = accepted()
    assert r.overlay() == {"mode": "WAITING_ELEVATOR", "task_id": "R1", "silence_s": 0}
    assert not r.silent and r.holding and r.elevator == "E1"


def test_accept_sends_state_and_event_while_the_link_is_up():
    r = RobotRide("robot_1", "b", wall=Clock())
    acts = r.accept("R1", plan(), robot_state(), (10.0, 0.0))
    assert kinds(acts) == ["ack", "event", "state"]
    assert events(acts) == ["ride_accepted"] and only(acts, "event")[0][1]["seq"] == 1


@pytest.mark.parametrize("p,state,pos,why", [
    (None, robot_state(), (10, 0), "without a plan"),
    (plan(slot="x"), robot_state(), (10, 0), "bad ride plan"),
    (plan(), robot_state(mode="ERROR", error="boom"), (10, 0), "ERROR"),
    (plan(), robot_state(floor=2), (10, 0), "on floor 2"),
    (plan(), robot_state(), (10 + LOBBY_TOL + 0.1, 0), "not at the lobby"),
    (plan(), robot_state(in_car=True), (10, 0), "in a car"),
    (plan(), None, None, "not known"),
])
def test_accept_refuses(p, state, pos, why):
    r = RobotRide("robot_1", "b")
    acts = r.accept("R1", p, state, pos)
    assert acks(acts) == [("R1", "failed")] and why in acts[0][3]
    assert not r.holding


def test_new_ride_replaces_a_waiting_one():
    r = accepted()
    acts = r.accept("R2", plan(trip="E1-k3-2"), robot_state(), (10, 0))
    assert acks(acts) == [("R1", "cancelled"), ("R2", "accepted")]
    assert events(acts) == ["ride_cancelled", "ride_accepted"]
    assert r.cmd_id == "R2" and r.trip == "E1-k3-2"
    # Turns of the replaced trip are ignored.
    assert r.turn(turn(1, "board", trip="E1-k3-1")) == []


def test_ride_refused_once_boarding_started():
    r = accepted()
    board(r)
    acts = r.accept("R2", plan(trip="E1-k3-2"), robot_state(), (10, 0))
    assert acks(acts) == [("R2", "failed")] and r.cmd_id == "R1"


# ---- cancel ----

def test_cancel_for_the_waiting_ride():
    r = accepted()
    acts = r.cancel("R1.x99", "R1")
    assert acks(acts) == [("R1", "cancelled"), ("R1.x99", "accepted")]
    assert events(acts) == ["ride_cancelled"] and kinds(acts)[-1] == "state"
    assert not r.holding and r.overlay() is None


def test_late_cancel_of_an_old_ride_does_not_drop_the_new_one():
    r = accepted()
    r.accept("R2", plan(trip="E1-k3-2"), robot_state(), (10, 0))
    acts = r.cancel("R1.x5", "R1")
    assert acks(acts) == [("R1.x5", "failed")]
    assert r.cmd_id == "R2" and r.holding


def test_cancel_after_boarding_started_fails():
    r = accepted()
    board(r)
    acts = r.cancel("R1.x5", "R1")
    assert acks(acts) == [("R1.x5", "failed")] and r.holding


def test_cancel_not_about_rides_is_left_to_the_goto_logic():
    r = RobotRide("robot_1", "b")
    assert r.cancel("c9", "go7") is None
    assert r.cancel("c9", None) is None


def test_cancel_with_a_cmd_id_starting_with_x_uses_the_target():
    # The target is taken as is, never cut out of the cancel's cmd_id.
    r = RobotRide("robot_1", "b", wall=Clock())
    r.accept("t-1.0.1.xa1.R2", plan(), robot_state(), (10, 0))
    acts = r.cancel("t-1.0.1.xa1.R2.x1793", "t-1.0.1.xa1.R2")
    assert acks(acts)[0] == ("t-1.0.1.xa1.R2", "cancelled")


# ---- boarding ----

def test_board_turn_announces_silence_then_enters_and_moves():
    r = accepted(slot=2)
    acts, move = board(r)
    assert kinds(acts) == ["event", "state", "zone", "move"]
    assert events(acts) == ["boarding"]  # still on Wi-Fi
    assert acts[1][1] == {"mode": "BOARDING", "task_id": "R1", "silence_s": 58}
    assert zones(acts) == [(1, "board", "enter")]
    assert move["op"] == "board" and move["path"] == [car_door(LOBBY), car_slot(LOBBY, 2)]
    assert r.silent


def test_boarded_answers_clear_and_queues_the_event():
    r = accepted()
    acts = boarded(r)
    assert zones(acts) == [(1, "board", "clear")]
    assert events(acts) == []  # no Wi-Fi: queued
    assert [e["event"] for e in r.queue] == ["boarded"]
    assert r.overlay()["mode"] == "RIDING"


def test_board_failure_brings_the_link_back_at_the_lobby():
    r = accepted()
    _, move = board(r)
    acts = r.move_result(move["id"], "failed", "blocked", 0.0)
    assert zones(acts) == [(1, "board", "failed")]
    assert only(acts, "zone")[0][2]["reason"] == "blocked"
    assert not r.silent and r.overlay()["mode"] == "WAITING_ELEVATOR" and r.overlay()["silence_s"] == 0
    assert "state" in kinds(acts) and events(acts) == ["board_failed"]


def test_turns_for_others_or_other_trips_are_ignored():
    r = accepted()
    assert r.turn(turn(1, "board", robot="robot_2")) == []
    assert r.turn(turn(1, "board", trip="E1-other")) == []
    assert r.turn({"trip": "E1-k3-1", "robot": "robot_1"}) == []  # malformed
    assert RobotRide("robot_1", "b").turn(turn(1, "board")) == []  # no ride


def test_same_n_while_moving_is_ignored_and_answered_n_is_repeated():
    r = accepted()
    _, move = board(r)
    assert r.turn(turn(1, "board")) == []  # moving on it
    clear = r.move_result(move["id"], "done", "", 0.0)
    again = r.turn(turn(1, "board"))
    assert again == only(clear, "zone")  # the same answer, same seq


def test_new_n_for_a_board_already_done_is_cleared_without_moving():
    r = accepted()
    boarded(r)
    acts = r.turn(turn(2, "board"))
    assert zones(acts) == [(2, "board", "clear")] and only(acts, "move") == []


def test_injected_lost_turn():
    r = RobotRide("robot_1", "b", ignore_turns=1, wall=Clock())
    r.accept("R1", plan(), robot_state(), (10, 0))
    assert r.turn(turn(1, "board")) == []
    assert zones(r.turn(turn(1, "board"))) == [(1, "board", "enter")]  # the re-send gets through


# ---- riding and exiting ----

def test_robot_follows_the_car_floor_while_riding():
    r = accepted()
    boarded(r)
    assert r.car_state({"floor": 2, "moving": True}) == []
    acts = r.car_state({"floor": 3, "moving": False, "door": "opening"})
    assert [a[1]["op"] for a in acts] == ["floor"] and acts[0][1]["floor"] == 3
    assert r.car_state({"floor": 3, "moving": False}) == []


def test_exit_to_the_turns_lobby_then_link_back_then_done():
    r = accepted()
    boarded(r)
    lobby3 = {"x": 11.0, "y": 1.0}
    acts = r.turn(turn(4, "exit", floor=3, lobby=lobby3))
    assert zones(acts) == [(4, "exit", "enter")] and events(acts) == []
    move = only(acts, "move")[0][1]
    assert move["op"] == "exit" and move["floor"] == 3 and move["path"] == [car_door(lobby3), [11.0, 1.0]]
    acts = r.move_result(move["id"], "done", "", 100.0)
    assert zones(acts) == [(4, "exit", "clear")] and only(acts, "zone")[0][2]["floor"] == 3
    assert r.silent and r.tick(100.0 + LINK_BACK_S - 0.1, 3) == []
    acts = r.tick(100.0 + LINK_BACK_S, 3)
    # pose first (new floor), then the state without silence, the queued events in order, then done.
    assert kinds(acts) == ["pose", "state", "event", "event", "ack"]
    assert events(acts) == ["boarded", "exited"]
    seqs = [a[1]["seq"] for a in only(acts, "event")]
    assert seqs == sorted(seqs)
    assert acts[-1] == ("ack", "R1", "done", "", {"floor": 3})
    assert not r.silent and not r.holding and r.overlay() is None


def test_exit_failure_goes_back_to_the_slot_and_waits():
    r = accepted()
    boarded(r)
    move = only(r.turn(turn(4, "exit", floor=3)), "move")[0][1]
    acts = r.move_result(move["id"], "failed", "door zone blocked", 0.0)
    assert zones(acts) == [(4, "exit", "failed")]
    assert r.overlay()["mode"] == "RIDING" and r.silent
    assert r.tick(TRIP_LOST_S - 1, 3) == []  # never leaves by itself while the trip goes on
    # The failed answer is repeated for the same n; the retry (new n) moves again.
    assert zones(r.turn(turn(4, "exit", floor=3))) == [(4, "exit", "failed")]
    acts = r.turn(turn(5, "exit", floor=3))
    assert zones(acts) == [(5, "exit", "enter")]
    # Carried: exit at 5 although the plan said 3.
    r.move_result(only(acts, "move")[0][1]["id"], "failed", "door zone blocked", 0.0)
    r.car_state({"floor": 5, "moving": False})
    move = only(r.turn(turn(6, "exit", floor=5)), "move")[0][1]
    assert move["floor"] == 5
    r.move_result(move["id"], "done", "", 10.0)
    done = r.tick(10.0 + LINK_BACK_S, 5)
    assert done[-1] == ("ack", "R1", "done", "", {"floor": 5})
    assert events(done) == ["boarded", "exit_failed", "exit_failed", "exited"]


def test_turns_after_done_are_ignored_but_exit_again_before_done_is_cleared():
    r = accepted()
    boarded(r)
    move = only(r.turn(turn(4, "exit", floor=3)), "move")[0][1]
    r.move_result(move["id"], "done", "", 0.0)
    # Out but Wi-Fi not back yet: a new n for the same exit is answered clear.
    assert zones(r.turn(turn(5, "exit", floor=3))) == [(5, "exit", "clear")]
    r.tick(LINK_BACK_S, 3)
    assert r.turn(turn(6, "exit", floor=3)) == []  # done sent: not ours any more


def test_stale_move_reports_are_ignored():
    r = accepted()
    _, move = board(r)
    assert r.move_result(move["id"] - 1, "done", "", 0.0) == []
    assert r.move_result(move["id"], "running", "", 0.0) == []
    assert zones(r.move_result(move["id"], "done", "", 0.0)) == [(1, "board", "clear")]
    assert r.move_result(move["id"], "done", "", 0.0) == []  # reported again


def test_event_seq_increases_across_rides():
    r = accepted()
    boarded(r)
    move = only(r.turn(turn(2, "exit", floor=3)), "move")[0][1]
    r.move_result(move["id"], "done", "", 0.0)
    first = [a[1]["seq"] for a in only(r.tick(LINK_BACK_S, 3), "event")]
    acts = r.accept("R2", plan(trip="E1-k3-9", origin=3), robot_state(floor=3), (10, 0))
    second = [a[1]["seq"] for a in only(acts, "event")]
    assert second[0] == max(first) + 1
    assert all(a[1]["boot"] == "boot1" for a in only(acts, "event"))


# ---- review 6b ----

def exit_failed_once(r, n=4, floor=3):
    move = only(r.turn(turn(n, "exit", floor=floor)), "move")[0][1]
    r.move_result(move["id"], "failed", "blocked", 0.0)


def test_review_old_n_is_not_answered_while_moving_on_a_new_n():
    """Review 6b-3 (was: a late copy of turn 4 re-sent 'failed' for 4 while the
    robot was in the door zone for 5; the car then dropped it from door_zone)."""
    r = accepted()
    boarded(r)
    exit_failed_once(r)
    acts = r.turn(turn(5, "exit", floor=3))
    assert zones(acts) == [(5, "exit", "enter")] and r.move is not None
    assert r.turn(turn(4, "exit", floor=3)) == []
    assert r.turn(turn(5, "exit", floor=3)) == []
    r.move_result(only(acts, "move")[0][1]["id"], "done", "", 0.0)
    # Once out of the zone the old answer may be repeated again.
    assert zones(r.turn(turn(4, "exit", floor=3))) == [(4, "exit", "failed")]


def test_review_trip_released_with_the_robot_inside():
    """Review 6b-1 (was: silent forever without an exit turn)."""
    r = accepted()
    boarded(r)
    r.car_state({"floor": 5, "moving": False})
    assert r.trip_msg({"trip": "E1-k3-1", "stage": "blocked"}, 5.0) == []  # still running
    acts = r.trip_msg({"trip": "E1-k3-1", "stage": "done", "detail": "released by operator"}, 6.0)
    assert not r.silent and not r.holding and r.stranded == "E1"
    assert kinds(acts) == ["pose", "state", "event", "event", "ack"]
    assert events(acts) == ["boarded", "exit_failed"]
    assert "released by operator" in only(acts, "event")[-1][1]["detail"]
    ack = acts[-1]
    assert ack[1:3] == ("R1", "failed") and ack[4] == {"floor": 5}
    # Until a human takes it out it reports ERROR, then "rescued" and its own state.
    ov = r.overlay({"in_car": True})
    assert ov["mode"] == "ERROR" and "needs a human" in ov["error"] and ov["task_id"] == ""
    assert r.tick(10.0, 5, {"in_car": True}) == []
    acts = r.tick(11.0, 5, {"in_car": False})
    assert events(acts) == ["rescued"] and kinds(acts) == ["event", "state"]
    assert r.overlay({"in_car": False}) is None and not r.stranded
    # A new ride is refused while the robot says it is in a car.
    assert acks(r.accept("R9", plan(), robot_state(in_car=True), (10, 0)))[0][1] == "failed"


@pytest.mark.parametrize("msg", [None, {"trip": "E1-k3-2", "stage": "planned"}, {"trip": "E1-k3-1", "stage": "aborted"}])
def test_trip_cleared_or_replaced_leaves_the_robot_stranded(msg):
    r = accepted()
    boarded(r)
    acts = r.trip_msg(msg, 1.0)
    assert acks(acts) == [("R1", "failed")] and r.stranded == "E1" and not r.silent


def test_trip_end_before_boarding_is_left_to_the_cancel():
    r = accepted()
    assert r.trip_msg(None, 1.0) == [] and r.stage == "waiting"


def test_trip_end_during_a_move_waits_for_the_move():
    r = accepted()
    boarded(r)
    move = only(r.turn(turn(4, "exit", floor=3)), "move")[0][1]
    assert r.trip_msg(None, 1.0) == []  # in the door zone: finish the move first
    acts = r.move_result(move["id"], "failed", "blocked", 2.0)
    assert zones(acts) == [(4, "exit", "failed")] and acks(acts) == [("R1", "failed")]
    # ... but a successful exit is a normal end.
    r2 = accepted()
    boarded(r2)
    move = only(r2.turn(turn(4, "exit", floor=3)), "move")[0][1]
    r2.trip_msg(None, 1.0)
    r2.move_result(move["id"], "done", "", 2.0)
    assert r2.tick(2.0 + LINK_BACK_S, 3)[-1] == ("ack", "R1", "done", "", {"floor": 3}) and not r2.stranded


def test_trip_not_heard_for_a_long_time_is_gone():
    r = accepted()
    boarded(r)
    r.trip_msg({"trip": "E1-k3-1", "stage": "travel"}, 50.0)  # republished: still there
    assert r.tick(50.0 + TRIP_LOST_S - 1, 3, {"in_car": True}) == []
    acts = r.tick(50.0 + TRIP_LOST_S + 1, 3, {"in_car": True})
    assert acks(acts) == [("R1", "failed")] and "nothing heard" in acts[-1][3]


def test_rescued_while_the_trip_runs_comes_out_done():
    r = accepted()
    boarded(r)
    acts = r.tick(5.0, 4, {"in_car": False})  # a human took it out on floor 4
    assert acts[-1] == ("ack", "R1", "done", "", {"floor": 4})
    assert events(acts) == ["boarded", "rescued"] and kinds(acts)[:2] == ["pose", "state"]
    assert not r.silent and not r.stranded


def test_review_board_move_never_reported_fails():
    """Review 6b-2 (was: BOARDING + silent forever)."""
    r = accepted()
    acts = r.turn(turn(1, "board"), 100.0)
    move = only(acts, "move")[0][1]
    deadline = 100.0 + 15 + MOVE_EXTRA_S
    assert r.tick(deadline - 0.1, 1, {"move": None}) == []
    acts = r.tick(deadline, 1, {"move": {"id": move["id"] - 5, "status": "done"}})  # robot restarted: other move
    assert zones(acts) == [(1, "board", "failed")] and only(acts, "zone")[0][2]["reason"] == "robot did not finish the move"
    assert not r.silent and r.overlay()["mode"] == "WAITING_ELEVATOR"
    assert events(acts) == ["board_failed"]
    # The re-sent turn gets the same answer; a late report of the move is ignored.
    assert zones(r.turn(turn(1, "board"), deadline + 1)) == [(1, "board", "failed")]
    assert r.move_result(move["id"], "done", "", deadline + 2) == []


def test_slow_move_still_running_is_waited_for():
    r = accepted()
    move = only(r.turn(turn(1, "board"), 0.0), "move")[0][1]
    running = {"move": {"id": move["id"], "status": "running"}}
    for t in range(45, 200, 5):
        assert r.tick(float(t), 1, running) == []  # in the door zone: no answer
    assert zones(r.move_result(move["id"], "done", "", 200.0)) == [(1, "board", "clear")]


def test_exit_move_timeout_answers_failed_and_waits_in_the_slot():
    r = accepted()
    boarded(r)
    r.trip_msg({"trip": "E1-k3-1", "stage": "exiting"}, 0.0)
    r.turn(turn(4, "exit", floor=3), 0.0)
    acts = r.tick(15 + MOVE_EXTRA_S, 3, None)  # robot silent on ROS
    assert zones(acts) == [(4, "exit", "failed")] and r.stage == "riding" and r.silent


def test_review_exit_turn_at_the_lobby_is_answered_failed():
    r = accepted()
    acts = r.turn(turn(3, "exit", floor=1))
    assert zones(acts) == [(3, "exit", "failed")] and only(acts, "zone")[0][2]["reason"] == "not in the car"
    assert only(acts, "move") == [] and not r.silent
