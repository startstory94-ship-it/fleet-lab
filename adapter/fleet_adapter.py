"""ROS 2 <-> MQTT fleet adapter.

Bridges every robot on this host (found by its /<name>/robot_state topic) to the
fleet server over MQTT. Robots stay plain ROS 2 nodes; only this process talks
to the network outside the robot.

ROS 2 -> MQTT
  /<name>/odom + robot_state  -> fleet/<name>/pose   QoS 0, <= 5 Hz, deadband, 1 Hz when still
  /<name>/robot_state         -> fleet/<name>/state  QoS 1, on change + every 1 s
MQTT -> ROS 2
  fleet/<name>/cmd            -> goal_pose / cancel / dock / reset_error / drop_comms
                              <- fleet/<name>/cmd_ack  accepted, then done/failed/cancelled for goto
  fleet/<name>/cmd  type ride -> elevator ride (docs/ELEVATOR.md), see below
Adapter liveness
  fleet/adapter/<host>/status  "online" (retained); LWT sends "offline"

Elevator rides (stage 6, docs/ELEVATOR.md section 5): a ride is accepted at
the lobby; the adapter then follows elevator/<car>/turn|state|trip, answers on
elevator/<car>/zone and drives the robot through the door with
/<name>/elevator_move (board, floor, exit). From the boarding turn until 1 s
after the robot is out of the car the robot has no Wi-Fi: nothing is
published on fleet/* for it (simulated loss) and commands for it are dropped,
while the elevator channel keeps working. Robot events (fleet/<name>/event)
made in that time are queued and sent when the link is back, followed by the
ride's "done" with the floor the robot is on. An elevator move the robot does
not report within turn_timeout_s + --move-timeout-extra fails; a trip that
ends with the robot still inside ends the blackout, fails the ride and the
robot reports ERROR until a human takes it out (fleet command "rescue" in the
mock). Pure logic: RobotRide.

Every pose/state message carries `boot` (random per adapter start) and `seq`
(per robot, per topic, increasing) so the server can drop stale or duplicate
messages and still accept a restarted adapter whose seq starts over.

The robot itself knows nothing about command ids (like a Nav2 robot). The
adapter tracks the active goto, reports it as `task_id` in state, and sends the
final ack. Pure logic lives in fleet_logic.py.

Run:  python fleet_adapter.py [--mqtt-host 127.0.0.1] [--host-id <name>]
"""

import argparse
import json
import math
import queue
import random
import socket
import time
import uuid

import paho.mqtt.client as mqtt
import rclpy
from rclpy.node import Node
from rclpy.qos import (DurabilityPolicy, QoSProfile, ReliabilityPolicy,
                       qos_profile_sensor_data)

from geometry_msgs.msg import PoseStamped
from nav_msgs.msg import Odometry
from std_msgs.msg import String
from std_srvs.srv import Trigger

from fleet_logic import (POSE_MAX_HZ, CommandBook, GoalTracker, RobotRide,
                         is_live, pose_due, service_outcome, state_due)

DISCOVERY_PERIOD = 2.0     # s
KEEPALIVE = 5              # s, MQTT keepalive
SERVICE_CMDS = ("cancel", "dock", "reset_error", "drop_comms", "rescue")


def quat_to_yaw(q):
    return math.atan2(2.0 * (q.w * q.z + q.x * q.y), 1.0 - 2.0 * (q.y * q.y + q.z * q.z))


class RobotBridge:
    """Per-robot state: latest ROS data, throttling, seq counters, commands."""

    def __init__(self, node, name, ignore_turns=0, move_extra=None):
        self.node = node
        self.name = name
        self.pose_seq = 0
        self.state_seq = 0
        self.pose = None            # (x, y, yaw, v, frame)
        self.state = None           # dict from robot_state
        self.last_pose_sent = None  # (time, x, y, yaw)
        self.last_state_sent = None # (time, key)
        self.last_rx = None         # monotonic time of the last robot_state
        self.book = CommandBook()
        self.goal = GoalTracker()
        self.ride = RobotRide(name, node.boot, ignore_turns=ignore_turns, move_base=random.randrange(1 << 30),
                              **({} if move_extra is None else {"move_extra": move_extra}))
        self.was_silent = False
        self.move_pub = node.create_publisher(String, f"/{name}/elevator_move", 10)

        state_qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE,
                               durability=DurabilityPolicy.TRANSIENT_LOCAL)
        self.subs = [
            node.create_subscription(Odometry, f"/{name}/odom", self.on_odom, qos_profile_sensor_data),
            node.create_subscription(String, f"/{name}/robot_state", self.on_state, state_qos),
        ]
        self.goal_pub = node.create_publisher(PoseStamped, f"/{name}/goal_pose", 10)
        self.clients = {c: node.create_client(Trigger, f"/{name}/{c}") for c in SERVICE_CMDS}

    def on_odom(self, msg):
        p = msg.pose.pose
        self.pose = (p.position.x, p.position.y, quat_to_yaw(p.orientation),
                     msg.twist.twist.linear.x, msg.header.frame_id or "map")

    def on_state(self, msg):
        try:
            self.state = json.loads(msg.data)
        except ValueError:
            return
        now = time.monotonic()
        self.last_rx = now
        pos = self.pose[:2] if self.pose else None
        ended = self.goal.update(self.state, pos, now)
        if ended:
            self.node.ack(self, *ended)
        mv = self.state.get("move")
        if isinstance(mv, dict) and self.ride.move is not None:
            self.node.run(self, self.ride.move_result(mv.get("id"), mv.get("status"), mv.get("detail", ""), now))

    def ack(self, cmd_id, status, message="", extra=None):
        body = {"cmd_id": cmd_id, "status": status, "message": message}
        if extra:
            body.update(extra)
        self.book.record(cmd_id, body)
        return body


class FleetAdapter(Node):
    def __init__(self, args):
        super().__init__("fleet_adapter")
        self.host_id = args.host_id
        self.boot = uuid.uuid4().hex[:8]
        self.robots = {}
        self.ignore_turns = dict(args.ignore_turns or [])
        self.move_extra = args.move_timeout_extra
        self.cars = set()           # elevators whose channel we follow
        self.inbox = queue.Queue()  # MQTT thread -> ROS thread
        self.status_topic = f"fleet/adapter/{self.host_id}/status"

        self.mqtt = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2,
                                client_id=f"adapter-{self.host_id}")
        self.mqtt.will_set(self.status_topic, self.status_payload("offline"), qos=1, retain=True)
        # Spread reconnects so many adapters don't hit the broker in lockstep.
        self.mqtt.reconnect_delay_set(min_delay=random.uniform(0.5, 1.5), max_delay=30)
        self.mqtt.on_connect = self.on_mqtt_connect
        self.mqtt.on_disconnect = self.on_mqtt_disconnect
        self.mqtt.on_message = self.on_mqtt_message
        self.mqtt.connect_async(args.mqtt_host, args.mqtt_port, keepalive=KEEPALIVE)
        self.mqtt.loop_start()

        self.create_timer(DISCOVERY_PERIOD, self.discover)
        self.create_timer(1.0 / POSE_MAX_HZ, self.publish_poses)
        self.create_timer(0.1, self.publish_states)
        self.create_timer(0.05, self.drain_inbox)
        self.create_timer(0.1, self.tick_rides)
        self.get_logger().info(f"adapter {self.host_id} boot={self.boot} -> {args.mqtt_host}:{args.mqtt_port}")

    def status_payload(self, status):
        return json.dumps({"host": self.host_id, "boot": self.boot, "status": status})

    # ---- discovery ----
    def discover(self):
        for topic, _ in self.get_topic_names_and_types():
            parts = topic.strip("/").split("/")
            if len(parts) == 2 and parts[1] == "robot_state" and parts[0] not in self.robots:
                name = parts[0]
                self.robots[name] = RobotBridge(self, name, self.ignore_turns.get(name, 0), self.move_extra)
                self.mqtt.subscribe(f"fleet/{name}/cmd", qos=1)
                self.get_logger().info(f"bridging {name}")

    # ---- MQTT ----
    def on_mqtt_connect(self, client, userdata, flags, reason_code, properties):
        if reason_code.is_failure:
            self.get_logger().warn(f"mqtt connect failed: {reason_code}")
            return
        self.get_logger().info("mqtt connected")
        client.publish(self.status_topic, self.status_payload("online"), qos=1, retain=True)
        for name in list(self.robots):
            client.subscribe(f"fleet/{name}/cmd", qos=1)
        for car in list(self.cars):
            self.subscribe_car(car)

    def on_mqtt_disconnect(self, client, userdata, flags, reason_code, properties):
        self.get_logger().warn(f"mqtt disconnected: {reason_code}")

    def on_mqtt_message(self, client, userdata, msg):
        self.inbox.put((msg.topic, msg.payload))

    def send(self, topic, body, qos):
        self.mqtt.publish(topic, json.dumps(body, separators=(",", ":")), qos=qos)

    def send_fleet(self, r, kind, body, qos):
        """Publishes on fleet/<robot>/<kind>, unless the robot is in its
        elevator blackout (no Wi-Fi in the car)."""
        if r.ride.silent:
            self.get_logger().debug(f"{r.name}: no link, {kind} not sent")
            return
        self.send(f"fleet/{r.name}/{kind}", body, qos)

    def ack(self, r, cmd_id, status, message="", extra=None):
        self.send_fleet(r, "cmd_ack", r.ack(cmd_id, status, message, extra), qos=1)

    # ---- elevator rides ----
    def subscribe_car(self, car):
        for kind in ("turn", "state", "trip"):
            self.mqtt.subscribe(f"elevator/{car}/{kind}", qos=1)

    def follow_car(self, car):
        if car and car not in self.cars:
            self.cars.add(car)
            self.subscribe_car(car)
            self.get_logger().info(f"following elevator {car}")

    def run(self, r, actions):
        """Carries out RobotRide actions, in order."""
        for a in actions:
            kind = a[0]
            if kind == "ack":
                _, cmd_id, status, message, extra = a
                self.ack(r, cmd_id, status, message, extra)
                self.get_logger().info(f"{r.name}: {cmd_id} {status} {message} {extra or ''}")
            elif kind == "state":
                self.publish_state(r, time.monotonic(), force=True, override=a[1])
            elif kind == "pose":
                self.publish_pose(r, time.monotonic(), force=True)
            elif kind == "event":
                # RobotRide queues events itself while there is no link; the
                # ones it hands out are sent now (the "boarding" event goes
                # out together with the last state before the blackout).
                self.send(f"fleet/{r.name}/event", a[1], qos=1)
            elif kind == "zone":
                z = a[2]
                self.send(f"elevator/{a[1]}/zone", z, qos=1)
                self.get_logger().info(f"{r.name}: zone n={z['n']} {z['action']} {z['event']} floor {z['floor']}")
            elif kind == "move":
                r.move_pub.publish(String(data=json.dumps(a[1])))
        if r.ride.silent != r.was_silent:
            r.was_silent = r.ride.silent
            self.get_logger().info(f"{r.name}: Wi-Fi {'lost (in the elevator)' if r.ride.silent else 'back'}")

    def tick_rides(self):
        now = time.monotonic()
        for r in self.robots.values():
            if r.ride.holding or r.ride.stranded:
                live = r.state if is_live(r.last_rx, now) else None
                self.run(r, r.ride.tick(now, (r.state or {}).get("floor", 1), live))

    def handle_elevator(self, topic, payload):
        parts = topic.split("/")
        if len(parts) != 3:
            return
        car, kind = parts[1], parts[2]
        now = time.monotonic()
        if kind == "trip" and not payload:
            msg = None  # the retained trip was cleared: it ended
        else:
            try:
                msg = json.loads(payload)
            except ValueError:
                self.get_logger().warn(f"bad message on {topic}")
                return
            if not isinstance(msg, dict):
                return
        for r in self.robots.values():
            if r.ride.elevator != car:
                continue
            if kind == "turn":
                self.run(r, r.ride.turn(msg, now))
            elif kind == "state":
                self.run(r, r.ride.car_state(msg))
            elif kind == "trip":
                # Only its end matters: robots never move on the retained trip's turn.
                self.run(r, r.ride.trip_msg(msg, now))

    # ---- ROS 2 -> MQTT ----
    def publish_poses(self):
        now = time.monotonic()
        for r in self.robots.values():
            self.publish_pose(r, now)

    def publish_pose(self, r, now, force=False):
        if r.pose is None or r.state is None or r.ride.silent or not (force or is_live(r.last_rx, now)):
            return
        x, y, yaw, v, frame = r.pose
        if not force and not pose_due(r.last_pose_sent, now, x, y, yaw):
            return
        r.pose_seq += 1
        r.last_pose_sent = (now, x, y, yaw)
        self.send(f"fleet/{r.name}/pose", {
            "boot": self.boot, "seq": r.pose_seq, "stamp": time.time(), "host": self.host_id,
            "frame": frame, "floor": r.state.get("floor", 1),
            "x": round(x, 3), "y": round(y, 3), "yaw": round(yaw, 3), "v": round(v, 3),
        }, qos=0)

    def publish_states(self):
        now = time.monotonic()
        for r in self.robots.values():
            self.publish_state(r, now)

    def publish_state(self, r, now, force=False, override=None):
        """override is sent even in the blackout: it is the last state before
        the link goes (BOARDING with silence_s)."""
        if r.state is None or (r.ride.silent and override is None) or not (force or is_live(r.last_rx, now)):
            return
        s = r.state
        ov = override or r.ride.overlay(s) or {}
        mode = ov.get("mode", s.get("mode"))
        task_id = ov.get("task_id", r.goal.active_id)
        silence = ov.get("silence_s", s.get("silence_s", 0))
        error = ov.get("error", s.get("error", ""))
        key = (mode, error, task_id, silence)
        if not force and not state_due(r.last_state_sent, now, key):
            return
        r.state_seq += 1
        r.last_state_sent = (now, key)
        self.send(f"fleet/{r.name}/state", {
            "boot": self.boot, "seq": r.state_seq, "stamp": time.time(),
            "host": self.host_id,
            "mode": mode, "battery": s.get("battery"),
            "task_id": task_id, "error": error,
            "silence_s": silence,
        }, qos=1)

    # ---- MQTT -> ROS 2 ----
    def drain_inbox(self):
        while True:
            try:
                topic, payload = self.inbox.get_nowait()
            except queue.Empty:
                return
            if topic.startswith("elevator/"):
                self.handle_elevator(topic, payload)
            else:
                self.handle_cmd(topic, payload)

    def handle_cmd(self, topic, payload):
        parts = topic.split("/")
        r = self.robots.get(parts[1]) if len(parts) == 3 and parts[2] == "cmd" else None
        if r is None:
            return
        if r.ride.silent:
            # In the car: Wi-Fi does not reach the robot, so it never gets this.
            self.get_logger().info(f"{r.name}: no link (in the elevator), command dropped: {payload[:80]!r}")
            return
        try:
            cmd = json.loads(payload)
            cmd_id, kind = cmd["cmd_id"], cmd["type"]
        except (ValueError, KeyError, TypeError):
            self.get_logger().warn(f"bad cmd on {topic}: {payload[:120]!r}")
            return

        verdict = r.book.check(cmd_id, cmd.get("expires_at"), time.time())
        if verdict == CommandBook.DUPLICATE:
            # Do not run it again, but repeat the last ack: the server re-sends
            # because it never saw that ack.
            last = r.book.last_ack(cmd_id)
            if last is not None:
                self.send(f"fleet/{r.name}/cmd_ack", last, qos=1)
            return
        if verdict == CommandBook.EXPIRED:
            self.ack(r, cmd_id, "failed", "expired")
            return

        if kind == "ride":
            pos = r.pose[:2] if r.pose else None
            acts = r.ride.accept(cmd_id, cmd.get("ride"), r.state, pos, time.monotonic())
            if r.ride.cmd_id == cmd_id:
                self.follow_car(r.ride.elevator)
                ended = r.goal.cancel(f"replaced by ride {cmd_id}")
                if ended:  # a goto still running: stop the robot
                    self.ack(r, *ended)
                    client = r.clients["cancel"]
                    if client.service_is_ready():
                        client.call_async(Trigger.Request())
            self.run(r, acts)
            return
        if kind == "cancel":
            acts = r.ride.cancel(cmd_id, cmd.get("target"))
            if acts is not None:
                self.run(r, acts)
                return
        if kind in ("goto", "dock") and r.ride.holding:
            self.ack(r, cmd_id, "failed", f"holding ride {r.ride.cmd_id}")
            return

        if kind == "goto":
            try:
                x, y = float(cmd["x"]), float(cmd["y"])
                yaw = float(cmd.get("yaw", 0.0))
            except (KeyError, TypeError, ValueError):
                self.ack(r, cmd_id, "failed", "goto needs numeric x and y")
                return
            goal = PoseStamped()
            goal.header.frame_id = cmd.get("frame", "map")
            goal.header.stamp = self.get_clock().now().to_msg()
            goal.pose.position.x, goal.pose.position.y = x, y
            goal.pose.orientation.z = math.sin(yaw / 2.0)
            goal.pose.orientation.w = math.cos(yaw / 2.0)
            preempted = r.goal.start(cmd_id, x, y, time.monotonic())
            if preempted:
                self.ack(r, preempted, "cancelled", f"preempted by {cmd_id}")
            r.goal_pub.publish(goal)
            self.ack(r, cmd_id, "accepted")
        elif kind in SERVICE_CMDS:
            client = r.clients[kind]
            if not client.service_is_ready():
                self.ack(r, cmd_id, "failed", f"{kind} service not available")
                return
            future = client.call_async(Trigger.Request())
            future.add_done_callback(lambda f: self.on_service_done(r, cmd_id, kind, f))
        else:
            self.ack(r, cmd_id, "failed", f"unknown command type {kind!r}")

    def on_service_done(self, r, cmd_id, kind, future):
        status, message = service_outcome(future)
        if status == "accepted" and kind in ("cancel", "dock"):
            ended = r.goal.cancel(f"{kind} requested by {cmd_id}")
            if ended:
                self.ack(r, *ended)
        self.ack(r, cmd_id, status, message)

    def close(self):
        # A clean shutdown says offline itself; the LWT only covers crashes.
        info = self.mqtt.publish(self.status_topic, self.status_payload("offline"), qos=1, retain=True)
        info.wait_for_publish(timeout=2)
        self.mqtt.loop_stop()
        self.mqtt.disconnect()


def ignore_spec(s):
    name, _, n = s.partition(":")
    return name, int(n or 1)


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--mqtt-host", default="127.0.0.1")
    ap.add_argument("--mqtt-port", type=int, default=1883)
    ap.add_argument("--host-id", default=socket.gethostname().lower())
    ap.add_argument("--ignore-turns", type=ignore_spec, action="append", metavar="ROBOT:N",
                    help="failure injection: the robot misses its first N elevator turns (repeatable)")
    ap.add_argument("--move-timeout-extra", type=float, default=None, metavar="S",
                    help="an elevator move the robot does not report within turn_timeout_s + S fails (default 30)")
    args, ros_args = ap.parse_known_args()

    rclpy.init(args=ros_args)
    node = FleetAdapter(args)
    try:
        rclpy.spin(node)
    except KeyboardInterrupt:
        pass
    finally:
        node.close()
        node.destroy_node()
        rclpy.try_shutdown()


if __name__ == "__main__":
    main()
