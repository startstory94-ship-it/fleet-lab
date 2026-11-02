"""ROS 2 <-> MQTT fleet adapter.

Bridges every robot on this host (found by its /<name>/robot_state topic) to the
fleet server over MQTT. Robots stay plain ROS 2 nodes; only this process talks
to the network outside the robot.

ROS 2 -> MQTT
  /<name>/odom + robot_state  -> fleet/<name>/pose   QoS 0, <= 5 Hz, deadband, 1 Hz when still
  /<name>/robot_state         -> fleet/<name>/state  QoS 1, on change + every 1 s
MQTT -> ROS 2
  fleet/<name>/cmd            -> goal_pose / cancel / dock / reset_error / drop_comms
                              <- fleet/<name>/cmd_ack
Adapter liveness
  fleet/adapter/<host>/status  "online" (retained); LWT sends "offline"

Every pose/state message carries `boot` (random per adapter start) and `seq`
(per robot, per topic, increasing) so the server can drop stale or duplicate
messages and still accept a restarted adapter whose seq starts over.

Run:  python fleet_adapter.py [--mqtt-host 127.0.0.1] [--host-id <name>]
"""

import argparse
import collections
import json
import math
import queue
import random
import socket
import threading
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

POSE_MAX_HZ = 5.0
POSE_IDLE_PERIOD = 1.0     # s, pose heartbeat while standing still
POSE_DEADBAND_M = 0.05
POSE_DEADBAND_RAD = math.radians(2.0)
STATE_PERIOD = 1.0         # s, state heartbeat
DISCOVERY_PERIOD = 2.0     # s
ROBOT_SILENT_AFTER = 1.5   # s without robot_state -> stop forwarding (robot_state is 2 Hz)
KEEPALIVE = 5              # s, MQTT keepalive
SERVICE_CMDS = ("cancel", "dock", "reset_error", "drop_comms")
SEEN_CMD_IDS = 256         # per robot, for duplicate suppression


def quat_to_yaw(q):
    return math.atan2(2.0 * (q.w * q.z + q.x * q.y), 1.0 - 2.0 * (q.y * q.y + q.z * q.z))


def angle_diff(a, b):
    return abs(math.atan2(math.sin(a - b), math.cos(a - b)))


class RobotBridge:
    """Per-robot state: latest ROS data, throttling and seq counters."""

    def __init__(self, node, name):
        self.name = name
        self.pose_seq = 0
        self.state_seq = 0
        self.pose = None            # (x, y, yaw, v, frame)
        self.state = None           # dict from robot_state
        self.last_pose_sent = None  # (time, x, y, yaw)
        self.last_state_sent = None # (time, key)
        self.last_rx = 0.0          # monotonic time of the last robot_state
        self.seen_cmds = collections.OrderedDict()

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
        self.last_rx = time.monotonic()

    def is_live(self, now):
        """Only forward while the robot itself is publishing. Re-sending cached
        data would hide a silent robot from the server's liveness check."""
        return self.state is not None and now - self.last_rx <= ROBOT_SILENT_AFTER

    def remember_cmd(self, cmd_id):
        """Return False if cmd_id was already handled (QoS 1 may redeliver)."""
        if cmd_id in self.seen_cmds:
            return False
        self.seen_cmds[cmd_id] = True
        while len(self.seen_cmds) > SEEN_CMD_IDS:
            self.seen_cmds.popitem(last=False)
        return True


class FleetAdapter(Node):
    def __init__(self, args):
        super().__init__("fleet_adapter")
        self.host_id = args.host_id
        self.boot = uuid.uuid4().hex[:8]
        self.robots = {}
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
        self.get_logger().info(f"adapter {self.host_id} boot={self.boot} -> {args.mqtt_host}:{args.mqtt_port}")

    def status_payload(self, status):
        return json.dumps({"host": self.host_id, "boot": self.boot, "status": status})

    # ---- discovery ----
    def discover(self):
        for topic, _ in self.get_topic_names_and_types():
            parts = topic.strip("/").split("/")
            if len(parts) == 2 and parts[1] == "robot_state" and parts[0] not in self.robots:
                name = parts[0]
                self.robots[name] = RobotBridge(self, name)
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

    def on_mqtt_disconnect(self, client, userdata, flags, reason_code, properties):
        self.get_logger().warn(f"mqtt disconnected: {reason_code}")

    def on_mqtt_message(self, client, userdata, msg):
        self.inbox.put((msg.topic, msg.payload))

    def send(self, topic, body, qos):
        self.mqtt.publish(topic, json.dumps(body, separators=(",", ":")), qos=qos)

    # ---- ROS 2 -> MQTT ----
    def publish_poses(self):
        now = time.monotonic()
        for r in self.robots.values():
            if r.pose is None or not r.is_live(now):
                continue
            x, y, yaw, v, frame = r.pose
            if r.last_pose_sent is not None:
                t0, x0, y0, yaw0 = r.last_pose_sent
                moved = math.hypot(x - x0, y - y0) >= POSE_DEADBAND_M or \
                    angle_diff(yaw, yaw0) >= POSE_DEADBAND_RAD
                if not moved and now - t0 < POSE_IDLE_PERIOD:
                    continue
            r.pose_seq += 1
            r.last_pose_sent = (now, x, y, yaw)
            self.send(f"fleet/{r.name}/pose", {
                "boot": self.boot, "seq": r.pose_seq, "stamp": time.time(),
                "frame": frame, "floor": r.state.get("floor", 1),
                "x": round(x, 3), "y": round(y, 3), "yaw": round(yaw, 3), "v": round(v, 3),
            }, qos=0)

    def publish_states(self):
        now = time.monotonic()
        for r in self.robots.values():
            if not r.is_live(now):
                continue
            s = r.state
            key = (s.get("mode"), s.get("error"), s.get("task_id", ""), s.get("silence_s", 0))
            if r.last_state_sent is not None:
                t0, key0 = r.last_state_sent
                if key == key0 and now - t0 < STATE_PERIOD:
                    continue
            r.state_seq += 1
            r.last_state_sent = (now, key)
            self.send(f"fleet/{r.name}/state", {
                "boot": self.boot, "seq": r.state_seq, "stamp": time.time(),
                "host": self.host_id,
                "mode": s.get("mode"), "battery": s.get("battery"),
                "task_id": s.get("task_id", ""), "error": s.get("error", ""),
                "silence_s": s.get("silence_s", 0),
            }, qos=1)

    # ---- MQTT -> ROS 2 ----
    def drain_inbox(self):
        while True:
            try:
                topic, payload = self.inbox.get_nowait()
            except queue.Empty:
                return
            self.handle_cmd(topic, payload)

    def handle_cmd(self, topic, payload):
        parts = topic.split("/")
        r = self.robots.get(parts[1]) if len(parts) == 3 and parts[2] == "cmd" else None
        if r is None:
            return
        try:
            cmd = json.loads(payload)
            cmd_id, kind = cmd["cmd_id"], cmd["type"]
        except (ValueError, KeyError, TypeError):
            self.get_logger().warn(f"bad cmd on {topic}: {payload[:120]!r}")
            return

        def ack(status, message=""):
            self.send(f"fleet/{r.name}/cmd_ack",
                      {"cmd_id": cmd_id, "status": status, "message": message}, qos=1)

        if not r.remember_cmd(cmd_id):
            return  # duplicate delivery; the first copy was already acked
        expires = cmd.get("expires_at")
        if expires is not None and time.time() > expires:
            ack("failed", "expired")
            return

        if kind == "goto":
            try:
                goal = PoseStamped()
                goal.header.frame_id = cmd.get("frame", "map")
                goal.header.stamp = self.get_clock().now().to_msg()
                goal.pose.position.x = float(cmd["x"])
                goal.pose.position.y = float(cmd["y"])
                yaw = float(cmd.get("yaw", 0.0))
                goal.pose.orientation.z = math.sin(yaw / 2.0)
                goal.pose.orientation.w = math.cos(yaw / 2.0)
            except (KeyError, TypeError, ValueError):
                ack("failed", "goto needs numeric x and y")
                return
            r.goal_pub.publish(goal)
            ack("accepted")
        elif kind in SERVICE_CMDS:
            client = r.clients[kind]
            if not client.service_is_ready():
                ack("failed", f"{kind} service not available")
                return
            future = client.call_async(Trigger.Request())
            future.add_done_callback(
                lambda f: ack("accepted" if f.result() and f.result().success else "failed",
                              f.result().message if f.result() else "no response"))
        else:
            ack("failed", f"unknown command type {kind!r}")

    def close(self):
        # A clean shutdown says offline itself; the LWT only covers crashes.
        info = self.mqtt.publish(self.status_topic, self.status_payload("offline"), qos=1, retain=True)
        info.wait_for_publish(timeout=2)
        self.mqtt.loop_stop()
        self.mqtt.disconnect()


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--mqtt-host", default="127.0.0.1")
    ap.add_argument("--mqtt-port", type=int, default=1883)
    ap.add_argument("--host-id", default=socket.gethostname().lower())
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
