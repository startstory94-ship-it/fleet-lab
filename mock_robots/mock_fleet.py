"""Mock robots for fleet-server development (ROS 2, no Gazebo).

Each robot is an rclpy node that only simulates what a fleet server sees:
pose, battery, mode, goals, docking, faults and communication drops.
Motion is a straight line toward the goal; there is no map or obstacle model.

Interfaces per robot (namespace = robot name):
  pub  /<name>/odom           nav_msgs/Odometry      10 Hz, best effort
  pub  /<name>/battery_state  sensor_msgs/BatteryState 1 Hz
  pub  /<name>/robot_state    std_msgs/String (JSON) 2 Hz, reliable + transient local
  sub  /<name>/goal_pose      geometry_msgs/PoseStamped
  srv  /<name>/cancel         std_srvs/Trigger
  srv  /<name>/dock           std_srvs/Trigger
  srv  /<name>/reset_error    std_srvs/Trigger
  srv  /<name>/drop_comms     std_srvs/Trigger   (stop publishing for --drop-seconds)

Run:  python mock_fleet.py --count 3
"""

import argparse
import json
import math
import random
import time

import rclpy
from rclpy.executors import MultiThreadedExecutor
from rclpy.node import Node
from rclpy.qos import (DurabilityPolicy, QoSProfile, ReliabilityPolicy,
                       qos_profile_sensor_data)

from geometry_msgs.msg import PoseStamped, TransformStamped
from nav_msgs.msg import Odometry
from sensor_msgs.msg import BatteryState
from std_msgs.msg import String
from std_srvs.srv import Trigger

try:
    from tf2_ros import TransformBroadcaster
except ImportError:  # TF is optional; RViz just won't show the robots
    TransformBroadcaster = None

IDLE, MOVING, DOCKING, CHARGING, ERROR = "IDLE", "MOVING", "DOCKING", "CHARGING", "ERROR"

TICK = 0.1              # s, motion update period
ARRIVE_TOL = 0.05       # m
DRAIN_PER_M = 0.05      # % battery per metre driven (default of --drain-per-m)
DRAIN_IDLE = 0.002      # % per second while powered on
CHARGE_RATE = 1.0       # % per second on the dock (default of --charge-rate)


def yaw_to_quat(yaw):
    return 0.0, 0.0, math.sin(yaw / 2.0), math.cos(yaw / 2.0)


def quat_to_yaw(q):
    return math.atan2(2.0 * (q.w * q.z + q.x * q.y), 1.0 - 2.0 * (q.y * q.y + q.z * q.z))


class MockRobot(Node):
    def __init__(self, name, x, y, speed, battery, fault_rate, drop_seconds,
                 roam_rate=0.0, roam_max=4.0, drain_per_m=DRAIN_PER_M, charge_rate=CHARGE_RATE):
        super().__init__("mock_robot", namespace=name)
        self.name = name
        self.x, self.y, self.yaw = x, y, 0.0
        self.dock = (x, y)
        self.speed = speed
        self.battery = battery
        self.drain_per_m = drain_per_m
        self.charge_rate = charge_rate
        self.fault_rate = fault_rate          # faults per minute (Poisson)
        self.drop_seconds = drop_seconds
        self.roam_rate = roam_rate            # Wi-Fi roaming gaps per minute (Poisson)
        self.roam_max = roam_max              # longest roaming gap, s
        self.mode = IDLE
        self.goal = None                      # (x, y, yaw) or None
        self.error = ""
        self.comms_down_until = 0.0
        self.frame = "map"

        state_qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE,
                               durability=DurabilityPolicy.TRANSIENT_LOCAL)
        self.odom_pub = self.create_publisher(Odometry, "odom", qos_profile_sensor_data)
        self.batt_pub = self.create_publisher(BatteryState, "battery_state", 10)
        self.state_pub = self.create_publisher(String, "robot_state", state_qos)
        self.tf = TransformBroadcaster(self) if TransformBroadcaster else None

        self.create_subscription(PoseStamped, "goal_pose", self.on_goal, 10)
        self.create_service(Trigger, "cancel", self.on_cancel)
        self.create_service(Trigger, "dock", self.on_dock)
        self.create_service(Trigger, "reset_error", self.on_reset_error)
        self.create_service(Trigger, "drop_comms", self.on_drop_comms)

        self.create_timer(TICK, self.step)
        self.create_timer(1.0, self.publish_battery)
        self.create_timer(0.5, self.publish_state)
        self.get_logger().info(f"ready at ({x:.1f}, {y:.1f}), battery {battery:.0f}%")

    # ---- commands ----
    def on_goal(self, msg):
        if self.mode == ERROR:
            self.get_logger().warn(f"goal ignored: in ERROR ({self.error})")
            return
        if self.battery <= 0.0:
            self.get_logger().warn("goal ignored: battery empty")
            return
        p = msg.pose.position
        self.goal = (p.x, p.y, quat_to_yaw(msg.pose.orientation))
        self.frame = msg.header.frame_id or "map"
        self.mode = MOVING
        self.get_logger().info(f"goal ({p.x:.2f}, {p.y:.2f})")

    def on_cancel(self, req, res):
        if self.mode in (MOVING, DOCKING):
            self.goal, self.mode = None, IDLE
            res.success, res.message = True, "cancelled"
        else:
            res.success, res.message = False, f"nothing to cancel ({self.mode})"
        return res

    def on_dock(self, req, res):
        if self.mode == ERROR:
            res.success, res.message = False, f"in ERROR ({self.error})"
            return res
        self.goal, self.mode = (self.dock[0], self.dock[1], 0.0), DOCKING
        res.success, res.message = True, "docking"
        return res

    def on_reset_error(self, req, res):
        if self.mode != ERROR:
            res.success, res.message = False, "not in ERROR"
            return res
        self.mode, self.error, self.goal = IDLE, "", None
        res.success, res.message = True, "reset"
        return res

    def on_drop_comms(self, req, res):
        self.comms_down_until = time.monotonic() + self.drop_seconds
        res.success, res.message = True, f"silent for {self.drop_seconds:.0f}s"
        self.get_logger().warn(res.message)
        return res

    # ---- simulation ----
    def step(self):
        self.battery = max(0.0, self.battery - DRAIN_IDLE * TICK)

        if self.mode == CHARGING:
            self.battery = min(100.0, self.battery + self.charge_rate * TICK)
            if self.battery >= 100.0:
                self.mode = IDLE
        elif self.mode in (MOVING, DOCKING) and self.goal:
            self.drive()

        if self.mode != ERROR and self.fault_rate > 0.0 and \
                random.random() < self.fault_rate * TICK / 60.0:
            self.fail("simulated_fault")
        if self.battery <= 0.0 and self.mode in (MOVING, DOCKING):
            self.fail("battery_empty")

        if self.roam_rate > 0.0 and self.comms_up() and \
                random.random() < self.roam_rate * TICK / 60.0:
            gap = random.uniform(0.5, self.roam_max)
            self.comms_down_until = time.monotonic() + gap
            self.get_logger().info(f"roaming gap {gap:.1f}s")

        self.publish_odom()

    def drive(self):
        gx, gy, gyaw = self.goal
        dx, dy = gx - self.x, gy - self.y
        dist = math.hypot(dx, dy)
        if dist <= ARRIVE_TOL:
            self.x, self.y, self.yaw = gx, gy, gyaw
            self.goal = None
            self.mode = CHARGING if self.mode == DOCKING else IDLE
            self.get_logger().info(f"arrived ({gx:.2f}, {gy:.2f}) -> {self.mode}")
            return
        move = min(dist, self.speed * TICK)
        self.yaw = math.atan2(dy, dx)
        self.x += move * dx / dist
        self.y += move * dy / dist
        self.battery = max(0.0, self.battery - self.drain_per_m * move)

    def fail(self, reason):
        self.mode, self.error, self.goal = ERROR, reason, None
        self.get_logger().error(f"ERROR: {reason}")

    # ---- publishing ----
    def comms_up(self):
        return time.monotonic() >= self.comms_down_until

    def publish_odom(self):
        if not self.comms_up():
            return
        now = self.get_clock().now().to_msg()
        qx, qy, qz, qw = yaw_to_quat(self.yaw)
        moving = self.mode in (MOVING, DOCKING)

        odom = Odometry()
        odom.header.stamp, odom.header.frame_id = now, self.frame
        odom.child_frame_id = f"{self.name}/base_link"
        odom.pose.pose.position.x, odom.pose.pose.position.y = self.x, self.y
        o = odom.pose.pose.orientation
        o.x, o.y, o.z, o.w = qx, qy, qz, qw
        odom.twist.twist.linear.x = self.speed if moving else 0.0
        self.odom_pub.publish(odom)

        if self.tf:
            t = TransformStamped()
            t.header.stamp, t.header.frame_id = now, self.frame
            t.child_frame_id = odom.child_frame_id
            t.transform.translation.x, t.transform.translation.y = self.x, self.y
            r = t.transform.rotation
            r.x, r.y, r.z, r.w = qx, qy, qz, qw
            self.tf.sendTransform(t)

    def publish_battery(self):
        if not self.comms_up():
            return
        b = BatteryState()
        b.header.stamp = self.get_clock().now().to_msg()
        b.percentage = self.battery / 100.0
        b.voltage = 22.0 + 4.0 * b.percentage
        b.present = True
        b.power_supply_status = (BatteryState.POWER_SUPPLY_STATUS_CHARGING
                                 if self.mode == CHARGING
                                 else BatteryState.POWER_SUPPLY_STATUS_DISCHARGING)
        self.batt_pub.publish(b)

    def publish_state(self):
        if not self.comms_up():
            return
        state = {
            "name": self.name,
            "mode": self.mode,
            "x": round(self.x, 3), "y": round(self.y, 3), "yaw": round(self.yaw, 3),
            "frame": self.frame,
            "battery": round(self.battery, 1),
            "goal": None if self.goal is None else [round(v, 3) for v in self.goal],
            "error": self.error,
            "stamp": time.time(),
        }
        self.state_pub.publish(String(data=json.dumps(state)))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--count", type=int, default=3, help="number of robots")
    ap.add_argument("--prefix", default="robot", help="names are <prefix>_1, <prefix>_2, ...")
    ap.add_argument("--speed", type=float, default=0.8, help="m/s")
    ap.add_argument("--spacing", type=float, default=2.0, help="m between docks on the x axis")
    ap.add_argument("--origin-x", type=float, default=0.0, help="x of the first dock")
    ap.add_argument("--fault-rate", type=float, default=0.0, help="random faults per robot per minute")
    ap.add_argument("--drop-seconds", type=float, default=10.0, help="silence length for drop_comms")
    ap.add_argument("--roam-rate", type=float, default=0.0,
                    help="random Wi-Fi roaming gaps per robot per minute")
    ap.add_argument("--roam-max", type=float, default=4.0, help="longest roaming gap in seconds (min 0.5)")
    ap.add_argument("--battery", type=float, default=None,
                    help="start battery %% for every robot (default: random 60-100)")
    ap.add_argument("--drain-per-m", type=float, default=DRAIN_PER_M, help="battery %% used per metre driven")
    ap.add_argument("--charge-rate", type=float, default=CHARGE_RATE, help="battery %% gained per second on the dock")
    ap.add_argument("--seed", type=int, default=None)
    args, ros_args = ap.parse_known_args()
    random.seed(args.seed)

    rclpy.init(args=ros_args)
    robots = []
    for i in range(args.count):
        battery = random.uniform(60.0, 100.0)  # drawn even when fixed, so --seed gives the same run
        robots.append(MockRobot(f"{args.prefix}_{i + 1}", x=args.origin_x + i * args.spacing, y=0.0,
                                speed=args.speed, battery=battery if args.battery is None else args.battery,
                                fault_rate=args.fault_rate, drop_seconds=args.drop_seconds,
                                roam_rate=args.roam_rate, roam_max=args.roam_max,
                                drain_per_m=args.drain_per_m, charge_rate=args.charge_rate))
    executor = MultiThreadedExecutor()
    for r in robots:
        executor.add_node(r)
    try:
        executor.spin()
    except KeyboardInterrupt:
        pass
    finally:
        for r in robots:
            r.destroy_node()
        rclpy.try_shutdown()


if __name__ == "__main__":
    main()
