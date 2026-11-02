"""fleet_adapter.py wiring, with fake ROS 2 and MQTT modules (no rclpy or
broker needed): what goes out on which topic, in which order, and what is
held back while the robot is in the car. Run: python -m pytest adapter"""

import json
import sys
import types
from types import SimpleNamespace as NS

import pytest


# ---------------------------------------------------------------- fakes

class FakeLogger:
    def __getattr__(self, name):
        return lambda *a, **k: None


class FakePub:
    def __init__(self, topic):
        self.topic, self.msgs = topic, []

    def publish(self, msg):
        self.msgs.append(msg)


class FakeFuture:
    def __init__(self, res):
        self.res = res

    def add_done_callback(self, cb):
        cb(self)

    def exception(self):
        return None

    def result(self):
        return self.res


class FakeClient:
    def __init__(self, name):
        self.name, self.calls = name, 0

    def service_is_ready(self):
        return True

    def call_async(self, req):
        self.calls += 1
        return FakeFuture(NS(success=True, message="ok"))


class FakeNode:
    topics = []

    def __init__(self, name, namespace=None):
        self.pubs = {}

    def create_subscription(self, *a, **k):
        return object()

    def create_publisher(self, typ, topic, qos):
        self.pubs[topic] = FakePub(topic)
        return self.pubs[topic]

    def create_client(self, typ, name):
        return FakeClient(name)

    def create_timer(self, *a):
        return object()

    def get_logger(self):
        return FakeLogger()

    def get_topic_names_and_types(self):
        return FakeNode.topics

    def get_clock(self):
        return NS(now=lambda: NS(to_msg=lambda: None))


class FakeString:
    def __init__(self, data=""):
        self.data = data


class FakeMqtt:
    def __init__(self, *a, **k):
        self.out, self.subs = [], []

    def __getattr__(self, name):  # will_set, reconnect_delay_set, connect_async, loop_start ...
        return lambda *a, **k: None

    def subscribe(self, topic, qos=0):
        self.subs.append(topic)

    def publish(self, topic, payload, qos=0, retain=False):
        self.out.append((topic, json.loads(payload) if payload else None))
        return NS(wait_for_publish=lambda timeout=None: True)


def _pose_stamped():
    pos, ori = NS(x=0.0, y=0.0, z=0.0), NS(x=0.0, y=0.0, z=0.0, w=1.0)
    return NS(header=NS(frame_id="", stamp=None), pose=NS(position=pos, orientation=ori))


def _install_fakes():
    mods = {
        "rclpy": NS(init=lambda **k: None, spin=lambda n: None, try_shutdown=lambda: None),
        "rclpy.node": NS(Node=FakeNode),
        "rclpy.qos": NS(DurabilityPolicy=NS(TRANSIENT_LOCAL=1), QoSProfile=lambda **k: None,
                        ReliabilityPolicy=NS(RELIABLE=1), qos_profile_sensor_data=None),
        "geometry_msgs": NS(), "geometry_msgs.msg": NS(PoseStamped=_pose_stamped),
        "nav_msgs": NS(), "nav_msgs.msg": NS(Odometry=object),
        "std_msgs": NS(), "std_msgs.msg": NS(String=FakeString),
        "std_srvs": NS(), "std_srvs.srv": NS(Trigger=NS(Request=lambda: None)),
        "paho": NS(), "paho.mqtt": NS(),
        "paho.mqtt.client": NS(Client=FakeMqtt, CallbackAPIVersion=NS(VERSION2=2)),
    }
    saved = {k: sys.modules.get(k) for k in mods}
    for k, v in mods.items():
        sys.modules[k] = types.SimpleNamespace(**vars(v)) if isinstance(v, NS) else v
    sys.modules["paho"].mqtt = sys.modules["paho.mqtt"]
    sys.modules["paho.mqtt"].client = sys.modules["paho.mqtt.client"]
    try:
        sys.modules.pop("fleet_adapter", None)
        import fleet_adapter
        return fleet_adapter
    finally:
        for k, v in saved.items():
            if v is None:
                sys.modules.pop(k, None)
            else:
                sys.modules[k] = v


fa = _install_fakes()

PLAN = {"trip": "E1-k3-1", "elevator": "E1", "origin": 1, "direction": "up", "exit_floor": 3, "board": 0,
        "slot": 0, "boarding": ["r1"], "stops": [3], "silence_s": 40, "turn_timeout_s": 15,
        "lobby": {"x": 10, "y": 0}, "exit_lobby": {"x": 10, "y": 0}}


@pytest.fixture
def ad():
    FakeNode.topics = [("/r1/robot_state", ["std_msgs/msg/String"])]
    a = fa.FleetAdapter(NS(host_id="t", mqtt_host="h", mqtt_port=1, ignore_turns=None, move_timeout_extra=None))
    a.discover()
    r = a.robots["r1"]
    r.pose = (10.0, 0.0, 0.0, 0.0, "map")
    robot_state(r, {})
    a.mqtt.out.clear()
    return a


def robot_state(r, extra):
    s = {"mode": "IDLE", "floor": 1, "in_car": False, "error": "", "battery": 90, "goal": None, "move": None}
    s.update(extra)
    r.on_state(FakeString(json.dumps(s)))


def cmd(a, body):
    a.handle_cmd("fleet/r1/cmd", json.dumps(body).encode())


def out(a, prefix=""):
    return [(t, b) for t, b in a.mqtt.out if t.startswith(prefix)]


def ride_and_board(a):
    cmd(a, {"cmd_id": "R1", "type": "ride", "ride": PLAN})
    a.handle_elevator("elevator/E1/turn", json.dumps({"trip": "E1-k3-1", "n": 1, "robot": "r1", "action": "board",
                                                      "floor": 1, "lobby": {"x": 10, "y": 0}}).encode())
    return a.robots["r1"]


def test_ride_is_accepted_and_the_car_followed(ad):
    cmd(ad, {"cmd_id": "R1", "type": "ride", "ride": PLAN})
    acks = [b["status"] for t, b in out(ad, "fleet/r1/cmd_ack")]
    assert acks == ["accepted"]
    assert {"elevator/E1/turn", "elevator/E1/state", "elevator/E1/trip"} <= set(ad.mqtt.subs)
    st = [b for t, b in out(ad, "fleet/r1/state")]
    assert st[-1]["mode"] == "WAITING_ELEVATOR" and st[-1]["task_id"] == "R1"


def test_board_turn_order_then_silence(ad):
    r = ride_and_board(ad)
    topics = [t for t, _ in ad.mqtt.out]
    i_ev = topics.index("fleet/r1/event", topics.index("fleet/r1/cmd_ack"))
    boarding = [i for i, (t, b) in enumerate(ad.mqtt.out) if t == "fleet/r1/state" and b["mode"] == "BOARDING"]
    enter = topics.index("elevator/E1/zone")
    assert i_ev < boarding[0] < enter  # event, last state (with silence_s), then into the zone
    assert ad.mqtt.out[boarding[0]][1]["silence_s"] == 40
    assert r.ride.silent and json.loads(r.move_pub.msgs[-1].data)["op"] == "board"


def test_nothing_on_fleet_while_silent_but_the_zone_goes_out(ad):
    r = ride_and_board(ad)
    ad.mqtt.out.clear()
    r.last_rx = __import__("time").monotonic()
    ad.publish_states()
    ad.publish_poses()
    ad.send_fleet(r, "event", {"x": 1}, 1)
    ad.ack(r, "zzz", "accepted")
    assert out(ad, "fleet/") == []
    # The robot reports the board move done: the zone answer still goes out.
    robot_state(r, {"in_car": True, "move": {"id": r.ride.move["id"], "op": "board", "status": "done"}})
    assert [b["event"] for t, b in out(ad, "elevator/E1/zone")] == ["clear"]
    assert out(ad, "fleet/") == []


def test_commands_are_dropped_while_silent(ad):
    r = ride_and_board(ad)
    ad.mqtt.out.clear()
    cmd(ad, {"cmd_id": "G1", "type": "goto", "x": 1, "y": 1})
    cmd(ad, {"cmd_id": "R1.x1", "type": "cancel", "target": "R1"})
    assert ad.mqtt.out == [] and r.book.last_ack("G1") is None
    assert r.book.check("G1", None, 0) == r.book.NEW  # never seen: it did not reach the robot


def test_run_keeps_the_action_order(ad):
    r = ad.robots["r1"]
    ad.run(r, [("event", {"seq": 1}), ("zone", "E1", {"n": 1, "action": "board", "event": "clear", "floor": 1}),
               ("ack", "c9", "done", "", {"floor": 3}), ("move", {"id": 5, "op": "floor", "floor": 3})])
    assert [t for t, _ in ad.mqtt.out] == ["fleet/r1/event", "elevator/E1/zone", "fleet/r1/cmd_ack"]
    assert ad.mqtt.out[-1][1] == {"cmd_id": "c9", "status": "done", "message": "", "floor": 3}
    assert json.loads(r.move_pub.msgs[-1].data) == {"id": 5, "op": "floor", "floor": 3}


def test_done_then_duplicate_ride_without_plan_gets_done_again(ad):
    r = ride_and_board(ad)
    robot_state(r, {"in_car": True, "move": {"id": r.ride.move["id"], "op": "board", "status": "done"}})
    ad.handle_elevator("elevator/E1/turn", json.dumps({"trip": "E1-k3-1", "n": 2, "robot": "r1", "action": "exit",
                                                      "floor": 3, "lobby": {"x": 10, "y": 0}}).encode())
    robot_state(r, {"in_car": False, "floor": 3, "move": {"id": r.ride.move["id"], "op": "exit", "status": "done"}})
    import time as _t
    _t_now = _t.monotonic() + 2
    ad.run(r, r.ride.tick(_t_now, 3, r.state))
    kinds = [t.split("/")[-1] for t, _ in out(ad, "fleet/r1/")]
    i = kinds.index("pose")
    assert kinds[i:i + 2] == ["pose", "state"] and kinds[-1] == "cmd_ack"
    assert out(ad, "fleet/r1/pose")[-1][1]["floor"] == 3
    done = out(ad, "fleet/r1/cmd_ack")[-1][1]
    assert done == {"cmd_id": "R1", "status": "done", "message": "", "floor": 3}
    ad.mqtt.out.clear()
    cmd(ad, {"cmd_id": "R1", "type": "ride"})  # the server asks again, without the plan
    assert [b for t, b in out(ad, "fleet/r1/cmd_ack")] == [done]
    cmd(ad, {"cmd_id": "R2", "type": "ride"})  # new id without a plan
    assert out(ad, "fleet/r1/cmd_ack")[-1][1]["status"] == "failed"


def test_mqtt_reconnect_while_riding(ad):
    r = ride_and_board(ad)
    ad.mqtt.out.clear()
    ad.mqtt.subs.clear()
    ad.on_mqtt_connect(ad.mqtt, None, None, NS(is_failure=False), None)
    assert {"fleet/r1/cmd", "elevator/E1/turn", "elevator/E1/state", "elevator/E1/trip"} <= set(ad.mqtt.subs)
    assert [t for t, _ in ad.mqtt.out] == ["fleet/adapter/t/status"]  # nothing for the robot in the car
    assert r.ride.silent


def test_cleared_trip_ends_the_blackout_and_rescue_command(ad):
    r = ride_and_board(ad)
    robot_state(r, {"in_car": True, "move": {"id": r.ride.move["id"], "op": "board", "status": "done"}})
    ad.mqtt.out.clear()
    ad.handle_elevator("elevator/E1/trip", b"")  # retained trip cleared (operator release)
    assert not r.ride.silent and r.ride.stranded == "E1"
    st = out(ad, "fleet/r1/state")[-1][1]
    assert st["mode"] == "ERROR" and "needs a human" in st["error"] and st["silence_s"] == 0
    assert out(ad, "fleet/r1/cmd_ack")[-1][1]["status"] == "failed"
    cmd(ad, {"cmd_id": "X1", "type": "rescue"})  # reaches the robot's rescue service
    assert out(ad, "fleet/r1/cmd_ack")[-1][1] == {"cmd_id": "X1", "status": "accepted", "message": "ok"}
