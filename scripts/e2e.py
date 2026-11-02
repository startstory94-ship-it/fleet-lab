"""End-to-end scenarios: fleetd + ROS 2 mock robots + adapter + Mosquitto.

Starts everything, runs each scenario against the real processes, prints
PASS/FAIL per step and exits non-zero on any failure. Takes under two
minutes.

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

    def start_mock(name, args):
        """Start another mock robot process; the running adapter bridges it."""
        logs[name] = open(os.path.join(tmp, name + ".log"), "w")
        procs[name] = start([sys.executable, "-u", "mock_robots/mock_fleet.py"] + args, logs[name])

    procs = {
        "fleetd": start(fleetd_cmd, logs["fleetd"]),
        "robots": start([sys.executable, "-u", "mock_robots/mock_fleet.py", "--count", "3", "--seed", "1",
                         "--drop-seconds", str(DROP_SECONDS)], logs["robots"]),
        "adapter": start([sys.executable, "-u", "adapter/fleet_adapter.py", "--host-id", "e2e"], logs["adapter"]),
    }
    m = Mqtt()
    try:
        run(m, procs, restart_fleetd, start_mock)
    finally:
        for p in procs.values():
            if p.poll() is None:
                p.kill()
        for f in logs.values():
            f.close()

    failed = results.count(False)
    print(f"\n{len(results) - failed}/{len(results)} passed")
    sys.exit(1 if failed else 0)


def run(m, procs, restart_fleetd, start_mock):
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

    # 11. battery policy
    battery_steps(start_mock)

    # 12. dashboard and live frames
    live_steps()

    # 13. adapter crash -> LWT -> all OFFLINE. kill() is a hard kill (no clean
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
    ok = wait_for(lambda: (robot(bat) or {}).get("conn") == "ONLINE" and robot(bat)["mode"] == "IDLE", 20)
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
