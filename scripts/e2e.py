"""End-to-end scenarios: fleetd + ROS 2 mock robots + adapter + Mosquitto.

Starts everything, runs each scenario against the real processes, prints
PASS/FAIL per step and exits non-zero on any failure. Takes about four
minutes (the elevator rides of stage 6 take most of it).

The mock elevator (cmd/mockelevator) runs from the start with the same
-elevators file as fleetd. `--only elevator` runs just the connection step and
the elevator scenarios (for working on them).

Needs: Mosquitto on 127.0.0.1:1883, Go on PATH, and a shell where ROS 2 is
set up (pixi shell + local_setup.bat). Use scripts\\e2e.bat on Windows.
"""

import base64
import hashlib
import json
import os
import re
import socket
import statistics
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request
import uuid
from collections import defaultdict
from datetime import datetime

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


# The elevator of the e2e run: fleetd and the mock elevator read the same file.
# One car, floors 1-8, lobby (10, 0) on every floor, 1 s per floor, 1 s doors.
# A short turn timeout (8 s) keeps the slow-door scenario short.
ELEVATORS = {"elevators": [{"id": "E1", "capacity": 3, "floors": [1, 2, 3, 4, 5, 6, 7, 8],
                            "lobby": {"x": 10, "y": 0}, "floor_s": 1, "door_s": 1}],
             "timing": {"turn_timeout_s": 8, "turn_estimate_s": 8}}


def main():
    only = sys.argv[sys.argv.index("--only") + 1] if "--only" in sys.argv else ""
    tmp = tempfile.mkdtemp(prefix="fleet-e2e-")
    ext = ".exe" if os.name == "nt" else ""
    exe, car_exe = os.path.join(tmp, "fleetd" + ext), os.path.join(tmp, "mockelevator" + ext)
    subprocess.run(["go", "build", "-o", exe, "./cmd/fleetd"], cwd=ROOT, check=True)
    subprocess.run(["go", "build", "-o", car_exe, "./cmd/mockelevator"], cwd=ROOT, check=True)
    lifts = os.path.join(tmp, "elevators.json")
    with open(lifts, "w") as f:
        json.dump(ELEVATORS, f)
    logs = {n: open(os.path.join(tmp, n + ".log"), "w") for n in ("fleetd", "robots", "adapter", "elevator")}
    print(f"logs: {tmp}")

    fleetd_cmd = [exe, "-http", API.removeprefix("http://"), "-db", os.path.join(tmp, "f.db"), "-elevators", lifts]

    def restart_fleetd():
        """Hard-kill fleetd and start it again on the same database."""
        procs["fleetd"].kill()
        procs["fleetd"].wait()
        logs["fleetd2"] = open(os.path.join(tmp, "fleetd2.log"), "w")
        procs["fleetd"] = start(fleetd_cmd, logs["fleetd2"])

    def start_mock(name, args):
        """Start another mock robot process; the running adapter bridges it."""
        logs[name] = open(os.path.join(tmp, name + ".log"), "w")
        procs[name] = start([sys.executable, "-u", "mock_robots/mock_fleet.py"] + args, logs[name])
        return procs[name]

    clear_retained()
    procs = {
        "elevator": start([car_exe, "-elevators", lifts], logs["elevator"]),
        "fleetd": start(fleetd_cmd, logs["fleetd"]),
        "robots": start([sys.executable, "-u", "mock_robots/mock_fleet.py", "--count", "3", "--seed", "1",
                         "--drop-seconds", str(DROP_SECONDS)], logs["robots"]),
        # tx_1 (elevator scenario) loses its first elevator turn.
        "adapter": start([sys.executable, "-u", "adapter/fleet_adapter.py", "--host-id", "e2e",
                          "--ignore-turns", "tx_1:1"], logs["adapter"]),
    }
    m = Mqtt()
    try:
        run(m, procs, restart_fleetd, start_mock, only)
    finally:
        for p in procs.values():
            if p.poll() is None:
                p.kill()
        for f in logs.values():
            f.close()

    failed = results.count(False)
    print(f"\n{len(results) - failed}/{len(results)} passed")
    sys.exit(1 if failed else 0)


def run(m, procs, restart_fleetd, start_mock, only=""):
    names = ["robot_1", "robot_2", "robot_3"]

    # 1. connection
    ok = wait_for(lambda: all((robot(n) or {}).get("conn") == "ONLINE" for n in names), 30)
    step("3 robots visible and ONLINE", bool(ok))
    if not ok:
        return
    if only == "elevator":
        elevator_steps(m, start_mock)
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

    # 11. battery policy
    battery_steps(start_mock)

    # 12. dashboard and live frames
    live_steps()

    # 13. elevator rides (stage 6): mixed destinations, then an exit failure
    elevator_steps(m, start_mock)

    # 14. adapter crash -> LWT -> all OFFLINE. kill() is a hard kill (no clean
    # "offline" message), so only the broker's LWT can report it.
    procs["adapter"].kill()
    step("adapter crash takes all robots OFFLINE",
         wait_for(lambda: all(robot(n)["conn"] == "OFFLINE" for n in names), 12) is not None)


class EventTail:
    """Collects every event after a starting seq by polling /events?since=,
    so a busy event stream cannot push the ones we need out of the server's
    recent-event window. gap is set if the server had already dropped some."""

    def __init__(self):
        self.cursor = max([e["seq"] for e in http_json("/events?n=1")] or [0])
        self.events, self.gap = [], False
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def run(self):
        while not self.stop.wait(0.2):
            try:
                new = http_json(f"/events?since={self.cursor}")
            except Exception:
                continue
            if new and new[0]["seq"] != self.cursor + 1:
                self.gap = True
            if new:
                self.events.extend(new)
                self.cursor = new[-1]["seq"]

    def close(self):
        self.stop.set()
        self.thread.join()


def kind(e):
    return e.get("detail", "").split(":")[0]


def battery_kinds(evs, name):
    return [kind(e) for e in list(evs) if e["type"] == "battery" and e["robot"] == name]


def first_occurrences(kinds):
    out = []
    for k in kinds:
        if k not in out:
            out.append(k)
    return out


def battery_steps(start_mock):
    """bat_1 starts at 30 % and uses 3 % per metre. One 4 m task takes it to
    about 18 %: the server docks it, it charges at 10 %/s, and other robots
    take the tasks next to it meanwhile. Its dock is at (25, 0), far from
    everything the earlier scenarios use (x <= 8), so bat_1 is always the
    nearest robot to its tasks."""
    bat, x0 = "bat_1", 25.0
    start_mock("bat", ["--count", "1", "--prefix", "bat", "--origin-x", str(x0), "--battery", "30",
                       "--drain-per-m", "3", "--charge-rate", "10"])
    ok = wait_for(lambda: (robot(bat) or {}).get("conn") == "ONLINE" and robot(bat)["mode"] == "IDLE"
                  and robot(bat)["has_pose"], 20)  # no pose yet: not assignable (stage 4)
    step("low-battery robot bat_1 ONLINE", bool(ok), robot(bat))
    if not ok:
        return
    tail = EventTail()
    try:
        battery_scenario(bat, x0, tail)
    finally:
        tail.close()


def battery_scenario(bat, x0, tail):
    t1 = post_json("/tasks", {"waypoints": [{"x": x0, "y": -4.0, "floor": 1}]})["id"]
    done = wait_for(lambda: task(t1)["state"] == "DONE", 20)
    t = task(t1) or {}
    step("bat_1 does the task next to it", bool(done) and t.get("robot") == bat,
         (t.get("state"), t.get("robot")))

    # bat_1 is nearest but needs charge: these go to the other robots.
    near = [post_json("/tasks", {"waypoints": [{"x": x, "y": y, "floor": 1}]})["id"]
            for x, y in [(x0, -3.0), (x0 + 1, -4.0)]]
    assigned = wait_for(lambda: all(task(i)["robot"] for i in near), 5)
    who = [(task(i) or {}).get("robot") for i in near]
    step("tasks next to a low robot go to others", bool(assigned) and bat not in who, who)

    # Resends are allowed; the first occurrence of each must come in order.
    want = ["low", "dock_sent", "docking", "charged"]
    got = wait_for(lambda: "charged" in battery_kinds(tail.events, bat) and
                   first_occurrences(battery_kinds(tail.events, bat)), 40)
    step("battery events low, dock_sent, docking, charged", got == want, got)

    wait_for(lambda: all(task(i)["state"] == "DONE" for i in near), 60)
    step("other robots finished them", all((task(i) or {}).get("state") == "DONE" for i in near),
         [((task(i) or {}).get("state"), (task(i) or {}).get("robot")) for i in near])

    back = wait_for(lambda: robot(bat)["mode"] == "IDLE" and robot(bat)["battery"] >= 99, 20)
    t2 = post_json("/tasks", {"waypoints": [{"x": x0, "y": -1.0, "floor": 1}]})["id"]
    took = wait_for(lambda: task(t2)["robot"] == bat, 5)
    step("charged bat_1 takes tasks again", bool(back) and bool(took), (robot(bat), task(t2)))
    wait_for(lambda: task(t2)["state"] == "DONE", 15)
    time.sleep(0.5)  # let the tail catch up

    flagged, bad = False, []
    for e in list(tail.events):
        if e["robot"] != bat:
            continue
        if e["type"] == "battery" and kind(e) in ("low", "charged"):
            flagged = kind(e) == "low"
        elif e["type"] == "task" and kind(e) == "assigned" and flagged:
            bad.append(e.get("task"))
    step("no task assigned to bat_1 while it needed charge", not tail.gap and not bad,
         bad or "events were dropped before they were read")


class WebSocket:
    """Minimal RFC 6455 client, enough for /ws/live: no extensions (so no
    compression), text messages, fragments joined, pings answered. The pixi
    environment has no WebSocket library."""

    GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

    def __init__(self, url, timeout=5.0):
        u = urllib.parse.urlsplit(url)
        self.sock = socket.create_connection((u.hostname, u.port or 80), timeout=timeout)
        self.buf = b""
        self.partial = b""  # fragments of a message not finished yet
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((f"GET {u.path or '/'} HTTP/1.1\r\nHost: {u.netloc}\r\n"
                           "Upgrade: websocket\r\nConnection: Upgrade\r\n"
                           f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
        while b"\r\n\r\n" not in self.buf:
            self._fill()
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        lines = head.decode("latin-1").split("\r\n")
        headers = {k.strip().lower(): v.strip() for k, v in (ln.split(":", 1) for ln in lines[1:] if ":" in ln)}
        accept = base64.b64encode(hashlib.sha1((key + self.GUID).encode()).digest()).decode()
        if lines[0].split()[1:2] != ["101"] or headers.get("sec-websocket-accept") != accept:
            raise ConnectionError(f"handshake failed: {lines[0]}")

    def _fill(self):
        chunk = self.sock.recv(65536)
        if not chunk:
            raise ConnectionError("connection closed")
        self.buf += chunk

    def _send(self, opcode, payload=b""):
        mask = os.urandom(4)
        n = len(payload)
        head = bytes([0x80 | opcode])
        if n < 126:
            head += bytes([0x80 | n])
        elif n < 1 << 16:
            head += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            head += bytes([0x80 | 127]) + struct.pack(">Q", n)
        self.sock.sendall(head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))

    def _frame(self):
        """Takes one whole frame off the buffer, or returns None (nothing
        consumed) if it has not fully arrived. A timeout while waiting for the
        rest of a frame therefore never loses its header."""
        buf = self.buf
        if len(buf) < 2:
            return None
        b0, b1 = buf[0], buf[1]
        n, pos = b1 & 0x7F, 2
        if n == 126:
            if len(buf) < 4:
                return None
            n, pos = struct.unpack(">H", buf[2:4])[0], 4
        elif n == 127:
            if len(buf) < 10:
                return None
            n, pos = struct.unpack(">Q", buf[2:10])[0], 10
        mask = None
        if b1 & 0x80:
            if len(buf) < pos + 4:
                return None
            mask, pos = buf[pos:pos + 4], pos + 4
        if len(buf) < pos + n:
            return None
        data, self.buf = buf[pos:pos + n], buf[pos + n:]
        if mask:
            data = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
        return b0 & 0x80, b0 & 0x0F, data

    def recv(self):
        """Returns the next text message. Raises socket.timeout if none has
        fully arrived; buffered bytes and a partly received fragmented message
        are kept for the next call."""
        while True:
            f = self._frame()
            if f is None:
                self._fill()
                continue
            fin, opcode, data = f
            if opcode == 0x8:
                raise ConnectionError("closed by server")
            if opcode == 0x9:
                self._send(0xA, data)
                continue
            if opcode == 0xA:
                continue
            self.partial += data
            if fin:
                msg, self.partial = self.partial, b""
                return msg.decode()

    def close(self):
        try:
            self._send(0x8, struct.pack(">H", 1000))
        except OSError:
            pass
        self.sock.close()


def server_time(s):
    """Parses Go's RFC 3339 time (nanoseconds, Z or offset) to unix seconds."""
    m = re.match(r"(.*T\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)$", s)
    frac = (m.group(2) or ".0")[:7]
    tz = "+00:00" if m.group(3) == "Z" else m.group(3)
    return datetime.fromisoformat(m.group(1) + frac + tz).timestamp()


def live_steps():
    try:
        with urllib.request.urlopen(API + "/", timeout=2) as r:
            body, ctype, code = r.read(), r.headers.get("Content-Type", ""), r.status
    except Exception as e:
        body, ctype, code = b"", str(e), 0
    step("GET / serves the dashboard", code == 200 and ctype.startswith("text/html") and b"<canvas" in body,
         (code, ctype))

    everyone = sorted(r["name"] for r in http_json("/robots"))
    try:
        ws = WebSocket(API.replace("http://", "ws://") + "/ws/live")
    except Exception as e:
        step("/ws/live first frame is a snapshot of every robot", False, e)
        return
    try:
        live_scenario(ws, everyone)
    finally:
        ws.close()


def live_scenario(ws, everyone):
    first = json.loads(ws.recv())
    got = sorted(r["name"] for r in first["robots"])
    step("/ws/live first frame is a snapshot of every robot", first["type"] == "snapshot" and got == everyone,
         (first["type"], got, everyone))

    # A task far from the docks: whoever takes it drives for several seconds.
    tid = post_json("/tasks", {"waypoints": [{"x": 4.0, "y": 20.0, "floor": 1}]})["id"]
    frames, end = [first], time.monotonic() + 5
    ws.sock.settimeout(0.5)
    while time.monotonic() < end:
        try:
            frames.append(json.loads(ws.recv()))
        except socket.timeout:
            pass
    name = (task(tid) or {}).get("robot")
    samples = [(server_time(f["time"]), r["x"], r["y"]) for f in frames[1:]
               for r in f["robots"] if r["name"] == name]
    moving = [s for s, prev in zip(samples[1:], samples) if (s[1], s[2]) != (prev[1], prev[2])]
    moved = samples and ((samples[-1][1] - samples[0][1]) ** 2 + (samples[-1][2] - samples[0][2]) ** 2) ** 0.5
    step("moving robot shows changing x/y across frames", bool(name) and len(moving) >= 10 and moved > 1.5,
         (name, len(moving), moved))

    # Frames come every ~150 ms while it moves, numbered without gaps.
    gaps = [b[0] - a[0] for a, b in zip(samples, samples[1:])]
    median = statistics.median(gaps) if gaps else 0
    seqs = [f["seq"] for f in frames]
    print(f"      ({name}: {len(moving)} moves in {len(frames) - 1} frames, median gap {median * 1000:.0f} ms)", flush=True)
    # A heartbeat (nothing changed for 2 s) repeats the seq; a delta adds one.
    in_order = all(f["type"] in ("delta", "heartbeat") for f in frames[1:]) and all(
        b["seq"] == a["seq"] + (b["type"] == "delta") for a, b in zip(frames, frames[1:]))
    step("frames ~150 ms apart, seq without gaps", 0.1 <= median <= 0.35 and in_order,
         (f"median {median:.3f}s", seqs[:12]))
    try:
        post_json(f"/tasks/{tid}/cancel", {})
    except Exception:
        pass  # already finished


# ------------------------------------------------------------------ elevator (stage 6)

LINK_BACK_S = 1.0  # adapter: Wi-Fi comes back this long after the exit clear


class LiftWatch:
    """Records, with arrival times, everything on the elevator channel of E1
    and the robots' fleet topics: car states (door_zone), turns and zone
    answers, and what each robot publishes while it should have no Wi-Fi."""

    def __init__(self, car="E1"):
        self.lock = threading.Lock()
        self.states, self.zones, self.turns, self.fleet = [], [], [], []
        self.c = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2, client_id=f"e2e-lift-{uuid.uuid4().hex[:6]}")
        self.c.on_message = self.on_message
        self.c.connect("127.0.0.1", 1883)
        for t in (f"elevator/{car}/state", f"elevator/{car}/zone", f"elevator/{car}/turn",
                  "fleet/+/state", "fleet/+/pose", "fleet/+/event", "fleet/+/cmd_ack"):
            self.c.subscribe(t, qos=1)
        self.c.loop_start()

    def on_message(self, client, userdata, msg):
        now = time.monotonic()
        try:
            body = json.loads(msg.payload)
        except ValueError:
            return
        parts = msg.topic.split("/")
        with self.lock:
            if parts[0] == "elevator":
                {"state": self.states, "zone": self.zones, "turn": self.turns}[parts[2]].append((now, body))
            elif parts[1] != "adapter":
                self.fleet.append((now, parts[1], parts[2], body))

    def close(self):
        self.c.loop_stop()
        self.c.disconnect()

    def snapshot(self, robots=None, since=0.0):
        """States after since; zones, turns and fleet messages of robots after since."""
        with self.lock:
            keep = lambda r: robots is None or r in robots  # noqa: E731
            return ([(t, s) for t, s in self.states if t >= since],
                    [(t, z) for t, z in self.zones if t >= since and keep(z.get("robot"))],
                    [(t, x) for t, x in self.turns if t >= since and keep(x.get("robot"))],
                    [(t, r, k, b) for t, r, k, b in self.fleet if t >= since and keep(r)])


def zone_counts(zones):
    """Most robots in the door zone at once, counted from the robots' own
    enter/clear/failed messages (independent of the car's door_zone). An
    answer only ends the robot's newest enter (like the mock car)."""
    inside, most = {}, 0
    for _, z in zones:
        if z["event"] == "enter":
            inside[z["robot"]] = max(inside.get(z["robot"], 0), z["n"])
        elif z["n"] >= inside.get(z["robot"], 1 << 30):
            inside.pop(z["robot"])
        most = max(most, len(inside))
    return most


def answers(zones):
    """The first answer (clear/failed) for each turn (trip, n), in arrival
    order; repeats of an answer are left out."""
    seen, out = set(), []
    for t, z in zones:
        k = (z["trip"], z["n"])
        if z["event"] != "enter" and k not in seen:
            seen.add(k)
            out.append((t, z))
    return out


def enters_once(zones):
    """(trip, n, robot) entered more than once. Must be empty."""
    c = defaultdict(int)
    for _, z in zones:
        if z["event"] == "enter":
            c[(z["trip"], z["n"], z["robot"])] += 1
    return {k: v for k, v in c.items() if v != 1}


def door_rule_breaks(states, zones):
    """Against the car's own states: every enter must follow a car state with
    the door open at that floor, door_zone 0 and not moving, received after
    the previous turn's answer; no state may show the car moving with
    something in the door zone."""
    ans = sorted(t for t, _ in answers(zones))
    bad = []
    for te, z in zones:
        if z["event"] != "enter":
            continue
        prev = max([t for t in ans if t < te] or [te - 30])
        # ±0.25 s: the car's state and the robot's messages come from different
        # clients; the watcher can read them in either order when the
        # conductor answers within a few milliseconds.
        if not any(prev - 0.25 < ts < te + 0.25 and s.get("door") == "open" and s.get("door_zone") == 0 and not s.get("moving")
                   and s.get("floor") == z["floor"] for ts, s in states):
            near = [(round(ts - te, 2), s.get("floor"), s.get("door"), s.get("door_zone"), s.get("moving"))
                    for ts, s in states if te - 4 < ts < te + 0.5]
            bad.append(("enter without an empty open door", z["robot"], z["n"], round(prev - te, 2), near[-6:]))
    bad += [("moving with door_zone", s.get("floor"), s.get("door_zone")) for _, s in states
            if s.get("moving") and s.get("door_zone", 0) > 0]
    return bad


def blackout_leaks(zones, fleet, robot):
    """Fleet messages the robot sent while it had no Wi-Fi: after its boarding
    enter (+0.3 s for messages on their way) until LINK_BACK_S after its exit
    clear (−0.1 s), and the gap from that clear to its first fleet message."""
    enter = [t for t, z in zones if z["robot"] == robot and z["action"] == "board" and z["event"] == "enter"]
    out = [t for t, z in zones if z["robot"] == robot and z["action"] == "exit" and z["event"] == "clear"]
    if not enter or not out:
        return None, None
    lo, hi = enter[0] + 0.3, out[0] + LINK_BACK_S - 0.1
    leaks = [(k, b.get("mode", b.get("event", b.get("status", "")))) for t, r, k, b in fleet if r == robot and lo < t < hi]
    after = [t for t, r, k, b in fleet if r == robot and t > out[0]]
    return leaks, (min(after) - out[0]) if after else None


def lift_events(tail, robots, kinds_wanted):
    return [e for e in list(tail.events) if e["type"] == "elevator" and kind(e) in kinds_wanted
            and (e.get("robot") in robots or any(r in e.get("detail", "") for r in robots))]


def planned_boarding(e):
    """['lift_2', 'lift_3', 'lift_1'] and '[3 5 7]' from a trip_planned detail."""
    m = re.search(r"board ([\w,]+); stops (\[[\d ]*\])", e.get("detail", ""))
    return (m.group(1).split(","), m.group(2)) if m else ([], "")


def trip_id(e):
    m = re.search(r"(E1-[\w-]+?): ", e.get("detail", ""))
    return m.group(1) if m else None


def wait_robots(names, floor, timeout):
    def ready():
        rs = [robot(n) or {} for n in names]
        return all(r.get("conn") == "ONLINE" and r.get("mode") == "IDLE" and r.get("has_pose") and r.get("floor") == floor
                   for r in rs)
    return wait_for(ready, timeout)


def retire(proc, names, label):
    """Robots idle on upper floors would be the nearest to the next tasks
    (assignment adds 1000 m per floor): take them away."""
    if proc is None:
        return
    proc.kill()
    gone = wait_for(lambda: all((robot(n) or {}).get("conn") != "ONLINE" for n in names), 10)
    step(f"{label} robots taken away (not ONLINE)", bool(gone))


def common_checks(label, states, zones, fleet, robots):
    """door_zone, door rules, one enter per turn, Wi-Fi blackout."""
    zmax = max([s.get("door_zone", 0) for _, s in states] or [99])
    ones = sum(1 for _, s in states if s.get("door_zone") == 1)
    step(f"{label}: door_zone never above 1 (car states and robots' own enter/answer)",
         zmax <= 1 and ones > 0 and zone_counts(zones) == 1, (zmax, ones, zone_counts(zones)))
    breaks, twice = door_rule_breaks(states, zones), enters_once(zones)
    step(f"{label}: every enter after an open, empty, standing car; never moving with door_zone > 0; one enter per turn",
         not breaks and not twice and any(z["event"] == "enter" for _, z in zones), (breaks[:5], twice))
    gaps = {r: blackout_leaks(zones, fleet, r) for r in robots}
    step(f"{label}: no fleet/* message from a robot in the car, first one >= 0.9 s after its exit clear",
         all(lk == [] and gap is not None and gap >= LINK_BACK_S - 0.1 for lk, gap in gaps.values()), gaps)


def elevator_steps(m, start_mock):
    try:
        ok = wait_for(lambda: http_json("/elevators")[0]["responding"], 10)
    except Exception:
        ok = False
    step("mock elevator E1 heard by fleetd", bool(ok))
    if not ok:
        return
    watch, tail = LiftWatch(), EventTail()
    try:
        retire(mixed_ride(m, start_mock, watch, tail), LIFT, "lift")
        retire(exit_failure(m, start_mock, watch, tail), FX, "fx")
        retire(lost_turn_slow_door(m, start_mock, watch, tail), TX, "tx")
        stuck_release_rescue(m, start_mock, watch, tail)
    finally:
        tail.close()
        watch.close()


LIFT = ["lift_1", "lift_2", "lift_3"]
FX = ["fx_1", "fx_2"]
TX = ["tx_1", "tx_2"]


def mixed_ride(m, start_mock, watch, tail):
    """lift_1..3 on floor 1 get tasks to floors 3, 7 and 5 at the same time.
    They start 3-4.2 m from the lobby, so they arrive within 1 s of each
    other (the batch window is 3 s). Returns the mock process."""
    lift = LIFT
    proc = start_mock("lift", ["--count", "3", "--prefix", "lift", "--origin-x", "13", "--spacing", "0.6",
                               "--speed", "1.5", "--battery", "100", "--floor", "1"])
    ok = wait_robots(lift, 1, 20)
    step("elevator: lift_1..3 ONLINE and IDLE on floor 1", bool(ok), [robot(n) for n in lift])
    if not ok:
        return proc
    t0 = time.monotonic()
    want = {3: (13.0, 3.0), 7: (13.6, 3.0), 5: (14.2, 3.0)}
    ids = {f: post_json("/tasks", {"waypoints": [{"x": x, "y": y, "floor": f}]})["id"] for f, (x, y) in want.items()}
    done = wait_for(lambda: all(task(i)["state"] == "DONE" for i in ids.values()), 150, interval=0.5)
    time.sleep(1.5)  # let the tail and the watcher catch up
    print(f"      (mixed ride: tasks done in {time.monotonic() - t0:.0f} s)", flush=True)
    tasks = {f: task(i) or {} for f, i in ids.items()}
    dest = {t.get("robot"): f for f, t in tasks.items()}
    states, zones, turns, fleet = watch.snapshot(lift, t0)

    planned = lift_events(tail, lift, {"trip_planned"})
    full = [e for e in planned if sorted(planned_boarding(e)[0]) == lift]
    order, stops = planned_boarding(full[0]) if full else ([], "")
    trip = trip_id(full[0]) if full else None
    step("one trip carried all three: boarding 7,5,3, stops [3 5 7]",
         len(full) == 1 and [dest.get(r) for r in order] == [7, 5, 3] and stops == "[3 5 7]",
         [(e.get("detail"), dest) for e in planned])

    ans = [z for _, z in answers(zones) if z["trip"] == trip]
    boards = [z["robot"] for z in ans if z["action"] == "board" and z["event"] == "clear"]
    exits = [(z["robot"], z["floor"]) for z in ans if z["action"] == "exit" and z["event"] == "clear"]
    step("boarded one by one in order 7,5,3 (door-side robot last)", [dest.get(r) for r in boards] == [7, 5, 3], boards)
    step("exited at 3, 5, 7 in that order, each at its own floor",
         [f for _, f in exits] == [3, 5, 7] and all(dest.get(r) == f for r, f in exits), exits)
    common_checks("mixed", states, zones, fleet, lift)

    conn = defaultdict(list)
    for e in list(tail.events):
        if e["type"] == "conn" and e["robot"] in lift:
            conn[e["robot"]].append(e["to"])
    announced = {r: max([b.get("silence_s", 0) for _, rr, k, b in fleet if rr == r and k == "state"] or [0]) for r in lift}
    step("each robot announced silence_s and was IN_ELEVATOR, never OFFLINE",
         all("IN_ELEVATOR" in conn[r] and "OFFLINE" not in conn[r] and announced[r] > 0 for r in lift),
         (dict(conn), announced))

    evs = defaultdict(list)
    for t, r, k, b in fleet:
        if k == "event":
            evs[r].append((t, b))
    exit_t = {z["robot"]: t for t, z in answers(zones) if z["action"] == "exit" and z["event"] == "clear"}
    names_ok = all([b["event"] for _, b in evs[r]] == ["ride_accepted", "boarding", "boarded", "exited"] for r in lift)
    queued = all(t >= exit_t.get(r, 1e18) for r in lift for t, b in evs[r] if b["event"] in ("boarded", "exited"))
    recorded = all(sum(1 for e in list(tail.events) if e["type"] == "elevator" and e.get("robot") == r
                       and kind(e) == "robot_event") >= 4 for r in lift)
    step("robot events (seq + robot time) queued in the car, sent after the exit, recorded by fleetd",
         names_ok and queued and recorded, {r: [(b["seq"], b["event"]) for _, b in evs[r]] for r in lift})

    acks = [(r, b.get("floor")) for t, r, k, b in fleet if k == "cmd_ack" and b["status"] == "done" and ".R" in b["cmd_id"]]
    step("ride done acks carry the exit floor", sorted(acks) == sorted((r, f) for r, f in dest.items()), acks)

    final = {f: (t.get("state"), t.get("robot"), (robot(t.get("robot")) or {}).get("floor"), t.get("dispatches"))
             for f, t in tasks.items()}
    step("3 ride tasks DONE, robots on floors 3, 5, 7, no reassignment",
         bool(done) and all(s == "DONE" and fl == f and d == 1 for f, (s, _, fl, d) in final.items()), final)
    lift_now = http_json("/elevators")[0]
    released = [e for e in list(tail.events) if e["type"] == "elevator" and kind(e) == "released" and trip_id(e) == trip]
    step("trip released and cleared", trip and lift_now.get("trip") is None and len(released) == 1,
         (trip, lift_now.get("trip"), released))
    return proc


def exit_failure(m, start_mock, watch, tail):
    """fx_1, fx_2 go to floors 4 and 6; every robot's first two exits at
    floor 4 fail (blocked door zone). The robot for 4 is retried, carried to
    6 and exits there first (door side); it comes back down on a new trip and
    its task still ends DONE on floor 4."""
    fx = FX
    proc = start_mock("fx", ["--count", "2", "--prefix", "fx", "--origin-x", "30", "--speed", "2", "--battery", "100",
                             "--floor", "1", "--exit-fail-floor", "4", "--exit-fail-count", "2"])
    ok = wait_robots(fx, 1, 20)
    step("exit failure: fx_1, fx_2 ONLINE on floor 1", bool(ok), [robot(n) for n in fx])
    if not ok:
        return proc
    t0 = time.monotonic()
    t4 = post_json("/tasks", {"waypoints": [{"x": 30.0, "y": 4.0, "floor": 4}]})["id"]
    t6 = post_json("/tasks", {"waypoints": [{"x": 32.0, "y": 4.0, "floor": 6}]})["id"]
    done = wait_for(lambda: task(t4)["state"] == "DONE" and task(t6)["state"] == "DONE", 180, interval=0.5)
    time.sleep(1.5)
    print(f"      (exit failure: tasks done in {time.monotonic() - t0:.0f} s)", flush=True)
    a, b = (task(t4) or {}).get("robot"), (task(t6) or {}).get("robot")
    states, zones, turns, fleet = watch.snapshot(fx, t0)
    ex = [(z["robot"], z["floor"], z["event"]) for _, z in answers(zones) if z["action"] == "exit"]
    step("exit at 4 failed, retried once, then carried: both exit at 6, the carried robot first",
         ex[:4] == [(a, 4, "failed"), (a, 4, "failed"), (a, 6, "clear"), (b, 6, "clear")], ex)
    re_ev = [e["detail"] for e in lift_events(tail, [a], {"replanned"})]
    failed_ev = [e["detail"] for e in lift_events(tail, [a], {"exit_failed"})]
    step("fleetd reported exit_failed (retry, gave up) and replanned the robot to floor 6",
         len(failed_ev) >= 2 and any("exits at floor 6" in d for d in re_ev), (failed_ev, re_ev))
    acks = [x.get("floor") for t, r, k, x in fleet if r == a and k == "cmd_ack" and x["status"] == "done" and ".R" in x["cmd_id"]]
    step("carried robot acked done at floor 6, then rode back down and acked done at 4", acks == [6, 4], acks)
    common_checks("exit failure", states, zones, fleet, fx)
    fin = [(task(t) or {}) for t in (t4, t6)]
    floors = [(robot(t.get("robot")) or {}).get("floor") for t in fin]
    step("both tasks DONE on floors 4 and 6, no reassignment",
         bool(done) and [t.get("state") for t in fin] == ["DONE", "DONE"] and floors == [4, 6]
         and all(t.get("dispatches") == 1 for t in fin), [(t.get("state"), t.get("robot"), t.get("dispatches")) for t in fin] + floors)
    return proc


def lost_turn_slow_door(m, start_mock, watch, tail):
    """tx_1, tx_2 on floor 2 go to floors 4 and 5. Every pass through the
    door takes 10 s (--door-move-s), longer than the 8 s turn timeout of this
    run, and the adapter loses tx_1's first turn (--ignore-turns tx_1:1).
    The conductor must re-send the turn, never give the door to the other
    robot while one is in the zone, and accept the late clears."""
    tx = TX
    proc = start_mock("tx", ["--count", "2", "--prefix", "tx", "--origin-x", "11", "--spacing", "0.5", "--speed", "2",
                             "--battery", "100", "--floor", "2", "--door-move-s", "10"])
    ok = wait_robots(tx, 2, 20)
    step("lost turn + slow door: tx_1, tx_2 ONLINE on floor 2", bool(ok), [robot(n) for n in tx])
    if not ok:
        return proc
    t0 = time.monotonic()
    ids = [post_json("/tasks", {"waypoints": [{"x": 11.0, "y": 3.0, "floor": f}]})["id"] for f in (4, 5)]
    done = wait_for(lambda: all(task(i)["state"] == "DONE" for i in ids), 200, interval=0.5)
    time.sleep(1.5)
    print(f"      (lost turn + slow door: tasks done in {time.monotonic() - t0:.0f} s)", flush=True)
    states, zones, turns, fleet = watch.snapshot(tx, t0)
    first = [(x["n"], x["action"]) for _, x in turns if x["robot"] == "tx_1"]
    resent = len(first) >= 2 and first[0] == first[1]
    entered_after_resend = [t for t, z in zones if z["robot"] == "tx_1" and z["event"] == "enter"]
    step("the lost turn was re-sent with the same n and the robot moved on the re-send",
         resent and bool(entered_after_resend) and entered_after_resend[0] > [t for t, x in turns if x["robot"] == "tx_1"][1] - 0.01,
         first[:4])
    od = [e["detail"] for e in list(tail.events) if e["type"] == "elevator" and kind(e) == "overdue" and t0 > 0
          and any(r in e.get("detail", "") for r in tx)]
    step("slow door: turns timed out (overdue) but nobody else moved", bool(od), od[:3])
    common_checks("slow door", states, zones, fleet, tx)
    fin = [(task(i) or {}) for i in ids]
    step("both tasks DONE on floors 4 and 5",
         bool(done) and [(robot(t.get("robot")) or {}).get("floor") for t in fin] == [4, 5],
         [(t.get("state"), t.get("robot"), t.get("error")) for t in fin])
    return proc


def stuck_release_rescue(m, start_mock, watch, tail):
    """sx_1 rides alone from floor 3 to 6, where every exit fails: it is
    stuck at the last stop and the car stays blocked. Its task is cancelled
    (another robot would take it), the operator releases the car, the adapter
    ends the robot's blackout (ERROR: needs a human), a human takes it out
    (the robot's rescue service, here through the fleet command "rescue") and
    it is IDLE on floor 6 again."""
    sx = "sx_1"
    start_mock("sx", ["--count", "1", "--prefix", "sx", "--origin-x", "11", "--speed", "2", "--battery", "100",
                      "--floor", "3", "--exit-fail-floor", "6", "--exit-fail-count", "-1"])
    ok = wait_robots([sx], 3, 20)
    step("stuck robot: sx_1 ONLINE on floor 3", bool(ok), robot(sx))
    if not ok:
        return
    t0 = time.monotonic()
    tid = post_json("/tasks", {"waypoints": [{"x": 11.0, "y": 3.0, "floor": 6}]})["id"]
    blocked = wait_for(lambda: (http_json("/elevators")[0].get("trip") or {}).get("stage") == "blocked", 90, interval=0.5)
    step("exit fails at the last stop: robot stuck, car blocked", bool(blocked),
         (http_json("/elevators")[0].get("trip") or {}).get("riders"))
    if not blocked:
        return
    try:
        post_json(f"/tasks/{tid}/cancel", {})  # handed to another robot when sx_1 got stuck: stop it
    except Exception:
        pass
    released = post_json("/elevators/E1/release", {})
    back = wait_for(lambda: (robot(sx) or {}).get("conn") == "ONLINE" and robot(sx)["mode"] == "ERROR", 10)
    r = robot(sx) or {}
    step("after the operator release the robot is back ONLINE, ERROR 'needs a human', floor 6",
         bool(back) and "needs a human" in r.get("error", "") and r.get("floor") == 6 and released.get("trip") is None,
         (r.get("conn"), r.get("mode"), r.get("error"), r.get("floor")))
    m.cmd(sx, {"cmd_id": f"rescue-{uuid.uuid4().hex[:6]}", "type": "rescue"})
    idle = wait_for(lambda: robot(sx)["mode"] == "IDLE" and robot(sx)["error"] == "", 10)
    time.sleep(1.0)
    states, zones, turns, fleet = watch.snapshot([sx], t0)
    evs = [b["event"] for t, r_, k, b in fleet if k == "event"]
    acks = [(b["status"], b.get("floor")) for t, r_, k, b in fleet if k == "cmd_ack" and ".R" in b["cmd_id"]]
    step("rescued: IDLE on floor 6, events exit_failed .. rescued, ride acked failed with floor 6",
         bool(idle) and robot(sx)["floor"] == 6 and evs[-2:] == ["exit_failed", "rescued"] and acks[-1] == ("failed", 6),
         (robot(sx), evs, acks))
    zmax = max([s.get("door_zone", 0) for _, s in states] or [99])
    step("car free again, door_zone never above 1",
         http_json("/elevators")[0].get("trip") is None and zmax <= 1 and not door_rule_breaks(states, zones),
         (zmax, door_rule_breaks(states, zones)[:3]))


def clear_retained():
    """An earlier run (killed, or ended with a blocked car) can leave a
    retained trip and car state on the broker; a new fleetd would take that
    trip over (docs/ELEVATOR.md 8). Start clean."""
    c = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2, client_id=f"e2e-clean-{uuid.uuid4().hex[:6]}")
    c.connect("127.0.0.1", 1883)
    c.loop_start()
    for car in [e["id"] for e in ELEVATORS["elevators"]]:
        for kind in ("trip", "state"):
            c.publish(f"elevator/{car}/{kind}", b"", qos=1, retain=True).wait_for_publish(2)
    c.loop_stop()
    c.disconnect()


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
