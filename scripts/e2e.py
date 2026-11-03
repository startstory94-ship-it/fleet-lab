"""End-to-end scenarios: fleetd + ROS 2 mock robots + adapter + Mosquitto.

Starts everything, runs each scenario against the real processes, prints
PASS/FAIL per step and exits non-zero on any failure. Takes about two minutes.

Needs: Mosquitto on 127.0.0.1:1883, Go on PATH, and a shell where ROS 2 is
set up (pixi shell + local_setup.bat). Use scripts\\e2e.bat on Windows.
"""

import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
import uuid

import paho.mqtt.client as mqtt

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
API = "http://127.0.0.1:18080"
DROP_SECONDS = 23  # long enough to cross the 20 s OFFLINE threshold

results = []


def step(name, ok, detail=""):
    results.append(ok)
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  ({detail})" if detail and not ok else ""), flush=True)


def http_json(path):
    with urllib.request.urlopen(API + path, timeout=2) as r:
        return json.load(r)


def post_json(path, body):
    req = urllib.request.Request(API + path, data=json.dumps(body).encode(), method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=2) as r:
        return json.load(r)


def task(task_id):
    try:
        return http_json("/tasks/" + task_id)
    except Exception:
        return None


def robot(name):
    try:
        return http_json("/robots/" + name)
    except Exception:
        return None


def wait_for(pred, timeout, interval=0.2):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        try:
            v = pred()
            if v:
                return v
        except Exception:
            pass
        time.sleep(interval)
    return None


class Mqtt:
    def __init__(self):
        self.acks = []
        self.lock = threading.Lock()
        self.c = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2, client_id=f"e2e-{uuid.uuid4().hex[:6]}")
        self.c.on_message = self.on_message
        self.c.connect("127.0.0.1", 1883)
        self.c.subscribe("fleet/+/cmd_ack", qos=1)
        self.c.loop_start()

    def on_message(self, client, userdata, msg):
        with self.lock:
            self.acks.append(json.loads(msg.payload))

    def acks_for(self, cmd_id):
        with self.lock:
            return [a["status"] for a in self.acks if a["cmd_id"] == cmd_id]

    def cmd(self, robot_name, body):
        self.c.publish(f"fleet/{robot_name}/cmd", json.dumps(body), qos=1).wait_for_publish(2)

    def raw(self, topic, body):
        self.c.publish(topic, json.dumps(body), qos=0).wait_for_publish(2)


def start(cmd, log):
    return subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, cwd=ROOT,
                            creationflags=getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0))


def main():
    tmp = tempfile.mkdtemp(prefix="fleet-e2e-")
    exe = os.path.join(tmp, "fleetd.exe" if os.name == "nt" else "fleetd")
    subprocess.run(["go", "build", "-o", exe, "./cmd/fleetd"], cwd=ROOT, check=True)
    logs = {n: open(os.path.join(tmp, n + ".log"), "w") for n in ("fleetd", "robots", "adapter")}
    print(f"logs: {tmp}")

    fleetd_cmd = [exe, "-http", API.removeprefix("http://"), "-db", os.path.join(tmp, "f.db")]

    def restart_fleetd():
        """Hard-kill fleetd and start it again on the same database."""
        procs["fleetd"].kill()
        procs["fleetd"].wait()
        logs["fleetd2"] = open(os.path.join(tmp, "fleetd2.log"), "w")
        procs["fleetd"] = start(fleetd_cmd, logs["fleetd2"])

    procs = {
        "fleetd": start(fleetd_cmd, logs["fleetd"]),
        "robots": start([sys.executable, "-u", "mock_robots/mock_fleet.py", "--count", "3", "--seed", "1",
                         "--drop-seconds", str(DROP_SECONDS)], logs["robots"]),
        "adapter": start([sys.executable, "-u", "adapter/fleet_adapter.py", "--host-id", "e2e"], logs["adapter"]),
    }
    m = Mqtt()
    try:
        run(m, procs, restart_fleetd)
    finally:
        for p in procs.values():
            if p.poll() is None:
                p.kill()
        for f in logs.values():
            f.close()

    failed = results.count(False)
    print(f"\n{len(results) - failed}/{len(results)} passed")
    sys.exit(1 if failed else 0)


def run(m, procs, restart_fleetd):
    names = ["robot_1", "robot_2", "robot_3"]

    # 1. connection
    ok = wait_for(lambda: all((robot(n) or {}).get("conn") == "ONLINE" for n in names), 30)
    step("3 robots visible and ONLINE", bool(ok))
    if not ok:
        return

    # 2. goto: accepted -> done, robot ends at the goal, task_id shown while moving
    m.cmd("robot_1", {"cmd_id": "go1", "type": "goto", "x": 3.0, "y": 2.0})
    busy = wait_for(lambda: robot("robot_1")["task_id"] == "go1", 5)
    step("task_id reported while moving", bool(busy))
    done = wait_for(lambda: m.acks_for("go1") == ["accepted", "done"], 15)
    r1 = robot("robot_1")
    step("goto acked accepted then done", bool(done), m.acks_for("go1"))
    step("robot_1 ended at goal", abs(r1["x"] - 3.0) < 0.1 and abs(r1["y"] - 2.0) < 0.1, r1)
    step("task_id cleared after done", wait_for(lambda: robot("robot_1")["task_id"] == "", 3) is not None)

    # 3. duplicate: not re-run, last ack repeated
    m.cmd("robot_1", {"cmd_id": "go1", "type": "goto", "x": 0.0, "y": 0.0})
    rep = wait_for(lambda: m.acks_for("go1") == ["accepted", "done", "done"], 3)
    time.sleep(1.0)
    r1 = robot("robot_1")
    step("duplicate gets last ack again", bool(rep), m.acks_for("go1"))
    step("duplicate does not move the robot", abs(r1["x"] - 3.0) < 0.1, r1)

    # 4. expired and unknown commands
    m.cmd("robot_2", {"cmd_id": "old1", "type": "goto", "x": 1, "y": 1, "expires_at": 1})
    m.cmd("robot_2", {"cmd_id": "bad1", "type": "fly"})
    step("expired command failed", wait_for(lambda: m.acks_for("old1") == ["failed"], 3) is not None)
    step("unknown command failed", wait_for(lambda: m.acks_for("bad1") == ["failed"], 3) is not None)

    # 5. goto then cancel -> goto ends as cancelled
    m.cmd("robot_2", {"cmd_id": "go2", "type": "goto", "x": 2.0, "y": 8.0})
    wait_for(lambda: robot("robot_2")["task_id"] == "go2", 5)
    m.cmd("robot_2", {"cmd_id": "cx2", "type": "cancel"})
    step("cancel acked", wait_for(lambda: m.acks_for("cx2") == ["accepted"], 3) is not None)
    step("cancelled goto reported", wait_for(lambda: m.acks_for("go2") == ["accepted", "cancelled"], 5) is not None,
         m.acks_for("go2"))

    # 6. a stale (old seq) pose is ignored
    # Same boot as the adapter with seq 1: older than anything the server has.
    x_before = robot("robot_2")["x"]
    m.raw("fleet/robot_2/pose", {"boot": adapter_boot(), "seq": 1, "x": 99, "y": 99, "floor": 9})
    time.sleep(0.5)
    step("old-seq pose ignored", abs(robot("robot_2")["x"] - x_before) < 0.5 and robot("robot_2")["floor"] == 1)

    # 7. silent robot: STALE -> OFFLINE -> ONLINE
    m.cmd("robot_3", {"cmd_id": "drop3", "type": "drop_comms"})
    t0 = time.monotonic()
    step("silent robot STALE within 4 s", wait_for(lambda: robot("robot_3")["conn"] == "STALE", 4) is not None)
    off = wait_for(lambda: robot("robot_3")["conn"] == "OFFLINE", 25)
    step("silent robot OFFLINE after ~20 s", bool(off) and 19 <= time.monotonic() - t0 <= 25,
         f"{time.monotonic() - t0:.1f}s")
    step("robot ONLINE again after silence",
         wait_for(lambda: robot("robot_3")["conn"] == "ONLINE", DROP_SECONDS + 10) is not None)

    # 8. events are numbered in order
    seqs = [e["seq"] for e in http_json("/events?n=200")]
    step("event seq strictly increasing", all(b == a + 1 for a, b in zip(seqs, seqs[1:])), seqs[:10])

    # 9. tasks over REST: 6 single-waypoint tasks shared by the 3 robots
    spots = [(1.0, 3.0), (5.0, 3.0), (3.0, 5.0), (0.0, 4.0), (6.0, 1.0), (2.0, 6.0)]
    ids = [post_json("/tasks", {"waypoints": [{"x": x, "y": y, "floor": 1}]})["id"] for x, y in spots]
    step("6 tasks submitted", len(set(ids)) == 6, ids)
    wait_for(lambda: all(task(i)["state"] == "DONE" for i in ids), 60)
    final = [task(i) or {} for i in ids]
    step("6 tasks DONE", all(t.get("state") == "DONE" for t in final),
         [(t.get("id"), t.get("state"), t.get("robot")) for t in final])
    who = {t.get("robot") for t in final}
    step("every robot did at least one task", set(names) <= who, sorted(map(str, who)))

    # 10. server restart while tasks run: they continue and finish
    spots = [(0.0, 9.0), (4.0, 10.0), (8.0, 7.0)]
    ids2 = [post_json("/tasks", {"waypoints": [{"x": x, "y": y, "floor": 1}]})["id"] for x, y in spots]
    running = wait_for(lambda: all(task(i)["state"] == "RUNNING" for i in ids2), 10)
    step("3 tasks RUNNING before restart", bool(running), [(task(i) or {}).get("state") for i in ids2])
    restart_fleetd()
    back = wait_for(lambda: all(task(i) for i in ids + ids2), 20)
    step("fleetd restarted with tasks restored", bool(back) and all(task(i)["state"] == "DONE" for i in ids))
    wait_for(lambda: all(task(i)["state"] == "DONE" for i in ids2), 60)
    final = [task(i) or {} for i in ids2]
    step("3 tasks DONE after restart", all(t.get("state") == "DONE" for t in final),
         [(t.get("id"), t.get("state"), t.get("robot"), t.get("error")) for t in final])
    step("restart did not reassign running tasks", all(t.get("dispatches") == 1 for t in final),
         [(t.get("id"), t.get("dispatches"), t.get("error")) for t in final])

    # 11. adapter crash -> LWT -> all OFFLINE. kill() is a hard kill (no clean
    # "offline" message), so only the broker's LWT can report it.
    procs["adapter"].kill()
    step("adapter crash takes all robots OFFLINE",
         wait_for(lambda: all(robot(n)["conn"] == "OFFLINE" for n in names), 12) is not None)


def adapter_boot():
    """The adapter's boot id, read from its retained status message."""
    got, box = threading.Event(), {}
    c = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2)

    def on_msg(cl, u, msg):
        box["boot"] = json.loads(msg.payload)["boot"]
        got.set()
    c.on_message = on_msg
    c.connect("127.0.0.1", 1883)
    c.subscribe("fleet/adapter/e2e/status")
    c.loop_start()
    got.wait(3)
    c.loop_stop()
    c.disconnect()
    return box.get("boot", "")


if __name__ == "__main__":
    main()
