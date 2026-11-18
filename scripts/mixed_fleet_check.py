"""Mixed-fleet check: ROS 2 mock robots (through the adapter) and Unity robots
(MQTT direct, can_ride false) under one fleetd with the mock elevator.

Start first: Mosquitto, bin/fleetd.exe -http 127.0.0.1:8080 (fresh -db), bin/mockelevator.exe,
mock_robots/mock_fleet.py --count 3 --origin-x 4 --spacing 1.5 and the adapter (scripts/demo.bat
starts those), and scripts/unity_robots.ps1 -Robots 2 -Origin "0,-3" -Background.
Then: py -3 scripts/mixed_fleet_check.py
"""
import json, subprocess, sys, time, urllib.request

API = "http://127.0.0.1:8080"


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(API + path, data=data, method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=5) as f:
        return json.load(f)


def robots():
    return {r["name"]: r for r in req("GET", "/robots")}


def wait(cond, timeout, what):
    end = time.time() + timeout
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(0.5)
    raise SystemExit(f"FAIL: timed out waiting for {what}")


def task_state(tid):
    return req("GET", f"/tasks/{tid}")


wait(lambda: sum(r["conn"] == "ONLINE" and r["mode"] == "IDLE" and r["has_pose"] for r in robots().values()) >= 5, 90, "5 robots ONLINE+IDLE")
for n, r in sorted(robots().items()):
    print(f"{n:9} host={r['host']:8} conn={r['conn']:7} floor={r['floor']} can_ride={r['can_ride']} battery={r['battery']:.0f}")

# 1. same-floor task next to the Unity robots -> a Unity robot takes it
a = req("POST", "/tasks", {"waypoints": [{"x": 1.0, "y": -5.0, "floor": 1}]})
# 2. cross-floor task -> only a ROS robot may take it (Unity robots cannot ride)
b = req("POST", "/tasks", {"waypoints": [{"x": 6.0, "y": 3.0, "floor": 3}]})
time.sleep(2)
ta, tb = task_state(a["id"]), task_state(b["id"])
print("assigned:", a["id"], "->", ta["robot"], "|", b["id"], "->", tb["robot"])
assert ta["robot"].startswith("unity_"), ta
assert tb["robot"].startswith("robot_"), tb

# 3. a long same-floor task for the other Unity robot, cancelled while running
c = req("POST", "/tasks", {"waypoints": [{"x": -8.0, "y": -12.0, "floor": 1}]})
wait(lambda: task_state(c["id"])["state"] == "RUNNING", 20, "task c RUNNING")
print("c running on", task_state(c["id"])["robot"])
req("POST", f"/tasks/{c['id']}/cancel")
wait(lambda: task_state(c["id"])["state"] == "CANCELLED", 15, "task c CANCELLED")
print("c CANCELLED")

wait(lambda: task_state(a["id"])["state"] == "DONE", 30, "task a DONE")
print("a DONE on", task_state(a["id"])["robot"])
wait(lambda: task_state(b["id"])["state"] == "DONE", 150, "task b DONE (elevator ride)")
print("b DONE on", task_state(b["id"])["robot"], "floor now", robots()[task_state(b["id"])["robot"]]["floor"])

# 4. kill the Unity process: its robots go OFFLINE (LWT), ROS robots stay ONLINE
subprocess.run(["taskkill", "/F", "/IM", "FleetRobots.exe"], capture_output=True)
t0 = time.time()
wait(lambda: all(r["conn"] == "OFFLINE" for n, r in robots().items() if n.startswith("unity_")), 10, "unity OFFLINE")
print(f"unity robots OFFLINE after {time.time() - t0:.2f}s")
rs = robots()
assert all(r["conn"] == "ONLINE" for n, r in rs.items() if n.startswith("robot_")), rs
print("ROS robots still ONLINE")
print("PASS")
