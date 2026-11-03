"""Unit tests for fleet_logic. Run: python -m pytest adapter"""

import math

import pytest

from fleet_logic import (GOAL_START_TIMEOUT, CommandBook, GoalTracker, is_live,
                         pose_due, service_outcome, state_due)


# ---- throttling ----

def test_pose_first_always_sent():
    assert pose_due(None, 0.0, 0, 0, 0)


@pytest.mark.parametrize("dx,dyaw_deg,dt,want", [
    (0.00, 0.0, 0.2, False),  # still, inside heartbeat period
    (0.04, 0.0, 0.2, False),  # below 5 cm
    (0.05, 0.0, 0.2, True),   # 5 cm
    (0.00, 1.9, 0.2, False),  # below 2 degrees
    (0.00, 2.1, 0.2, True),   # turned
    (0.00, 0.0, 1.0, True),   # heartbeat while still
])
def test_pose_deadband(dx, dyaw_deg, dt, want):
    last = (10.0, 1.0, 1.0, 0.0)
    assert pose_due(last, 10.0 + dt, 1.0 + dx, 1.0, math.radians(dyaw_deg)) is want


def test_pose_deadband_handles_yaw_wraparound():
    last = (0.0, 0.0, 0.0, math.pi - 0.01)
    assert not pose_due(last, 0.2, 0.0, 0.0, -math.pi + 0.01)  # 0.02 rad apart, not 2pi


def test_state_sent_on_change_or_heartbeat():
    last = (5.0, ("IDLE", "", "", 0))
    assert not state_due(last, 5.5, ("IDLE", "", "", 0))
    assert state_due(last, 5.5, ("MOVING", "", "c1", 0))
    assert state_due(last, 6.0, ("IDLE", "", "", 0))


def test_is_live():
    assert not is_live(None, 100.0)
    assert is_live(99.0, 100.0)
    assert not is_live(98.0, 100.0)  # silent for 2 s


# ---- duplicate and expired commands ----

def test_duplicate_is_not_run_but_ack_is_repeated():
    book = CommandBook()
    assert book.check("c1", None, 0) == CommandBook.NEW
    assert book.last_ack("c1") is None  # still running: nothing to repeat yet
    book.record("c1", {"cmd_id": "c1", "status": "accepted"})
    assert book.check("c1", None, 0) == CommandBook.DUPLICATE
    assert book.last_ack("c1") == {"cmd_id": "c1", "status": "accepted"}
    book.record("c1", {"cmd_id": "c1", "status": "done"})
    assert book.last_ack("c1")["status"] == "done"  # the latest outcome is repeated


def test_expired_command():
    book = CommandBook()
    assert book.check("c1", expires_at=100.0, now_wall=101.0) == CommandBook.EXPIRED
    # A re-send of the expired command is a duplicate and gets the same answer.
    book.record("c1", {"cmd_id": "c1", "status": "failed", "message": "expired"})
    assert book.check("c1", 100.0, 101.0) == CommandBook.DUPLICATE
    assert book.check("c2", expires_at=200.0, now_wall=101.0) == CommandBook.NEW


def test_book_forgets_oldest():
    book = CommandBook(size=2)
    for c in ("a", "b", "c"):
        book.check(c, None, 0)
    assert book.check("a", None, 0) == CommandBook.NEW
    assert book.check("c", None, 0) == CommandBook.DUPLICATE


# ---- service futures ----

class FakeFuture:
    def __init__(self, result=None, exc=None):
        self._result, self._exc = result, exc

    def exception(self):
        return self._exc

    def result(self):
        if self._exc:
            raise self._exc
        return self._result


class Resp:
    def __init__(self, success, message):
        self.success, self.message = success, message


def test_service_outcome():
    assert service_outcome(FakeFuture(Resp(True, "docking"))) == ("accepted", "docking")
    assert service_outcome(FakeFuture(Resp(False, "in ERROR"))) == ("failed", "in ERROR")
    assert service_outcome(FakeFuture(None)) == ("failed", "no response")
    status, msg = service_outcome(FakeFuture(exc=RuntimeError("boom")))
    assert status == "failed" and "boom" in msg  # must not raise


# ---- goal tracking ----

def st(mode, goal=None, error=""):
    return {"mode": mode, "goal": goal, "error": error}


def test_goal_done_only_after_started():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    # A state from before the goal (IDLE, no goal) must not count as done.
    assert g.update(st("IDLE"), (0.0, 0.0), 0.1) is None
    assert g.active_id == "c1"
    assert g.update(st("MOVING", [3.0, 2.0, 0.0]), (0.5, 0.3), 0.5) is None
    assert g.update(st("IDLE"), (3.0, 2.0), 4.0) == ("c1", "done", "")
    assert g.active_id == ""


def test_goal_at_current_position_finishes_without_seeing_start():
    g = GoalTracker()
    g.start("c1", 1.0, 1.0, now=0.0)
    assert g.update(st("IDLE"), (1.0, 1.0), 0.5) == ("c1", "done", "")


def test_goal_not_taken_times_out():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    assert g.update(st("IDLE"), (0.0, 0.0), 1.0) is None
    ended = g.update(st("IDLE"), (0.0, 0.0), GOAL_START_TIMEOUT + 0.1)
    assert ended[:2] == ("c1", "failed")


def test_goal_fails_on_error():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    g.update(st("MOVING", [3.0, 2.0, 0.0]), (0.5, 0.3), 0.5)
    ended = g.update(st("ERROR", error="simulated_fault"), (1.0, 0.6), 1.0)
    assert ended[:2] == ("c1", "failed") and "simulated_fault" in ended[2]


def test_goal_cancelled_when_robot_stops_short():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    g.update(st("MOVING", [3.0, 2.0, 0.0]), (0.5, 0.3), 0.5)
    assert g.update(st("IDLE"), (1.0, 0.6), 1.0)[:2] == ("c1", "cancelled")


def test_goal_preempted_by_dock():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    g.update(st("MOVING", [3.0, 2.0, 0.0]), (0.5, 0.3), 0.5)
    assert g.update(st("DOCKING", [0.0, 0.0, 0.0]), (0.6, 0.4), 1.0)[:2] == ("c1", "cancelled")


def test_new_goal_reports_preempted_one():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    assert g.start("c2", 1.0, 1.0, now=1.0) == "c1"
    assert g.active_id == "c2"


def test_cancel_before_robot_shows_goal_is_cancelled_not_failed():
    g = GoalTracker()
    g.start("c1", 3.0, 2.0, now=0.0)
    # The cancel lands before the robot's next state, so "started" was never seen.
    assert g.cancel("cancel requested by x") == ("c1", "cancelled", "cancel requested by x")
    assert g.active_id == ""
    # Later states and the start timeout must not report c1 again.
    assert g.update(st("IDLE"), (0.0, 0.0), GOAL_START_TIMEOUT + 1) is None


def test_cancel_without_goal_is_noop():
    assert GoalTracker().cancel("x") is None


def test_no_goal_no_output():
    assert GoalTracker().update(st("IDLE"), (0, 0), 1.0) is None
