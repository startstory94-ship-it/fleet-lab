"""Mixed-fleet check: ROS 2 mock robots (through the adapter) and Unity robots
(MQTT direct) under one fleetd with the mock elevator.

Start first: Mosquitto, bin/fleetd.exe -http 127.0.0.1:8080 (fresh -db), bin/mockelevator.exe,
mock_robots/mock_fleet.py --count 3 --origin-x 4 --spacing 1.5 --speed 1.2 and the adapter (scripts/demo.bat
starts those), and scripts/unity_robots.ps1 -Robots 2 -Origin "6,-2" -Spacing 1.5 -Speed 1.2 -Background.
Then: py -3 scripts/mixed_fleet_check.py

Unity robots ride elevators like the ROS robots (docs/ELEVATOR.md): cross-floor
tasks go to either kind, and one car carries both. With the Unity robots started
with -CanRide false, run with --unity-no-ride: then only ROS robots may take
cross-floor tasks (docs/ELEVATOR.md 12).
"""
import argparse, json, subprocess, threading, time, urllib.request

API = "http://127.0.0.1:8080"
ap = argparse.ArgumentParser()
ap.add_argument("--unity-no-ride", action="store_true", help="the Unity robots run with -CanRide false")
ap.add_argument("--keep-unity", action="store_true", help="do not kill FleetRobots.exe at the end")
args = ap.parse_args()


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


def idle_all():
    return all(r["conn"] == "ONLINE" and r["mode"] == "IDLE" and not r["task_id"] for r in robots().values())


# Trips are only visible while they run: record their riders as they go.
trips, stop = {}, threading.Event()


def watch_trips():
    while not stop.is_set():
        try:
            for e in req("GET", "/elevators"):
                t = e.get("trip")
                if t:
                    trips.setdefault(t["trip"], {})
                    for r in t["riders"]:
                        trips[t["trip"]][r["robot"]] = r["board"]
        except Exception:
            pass
        stop.wait(0.5)


threading.Thread(target=watch_trips, daemon=True).start()

wait(lambda: sum(r["conn"] == "ONLINE" and r["mode"] == "IDLE" and r["has_pose"] for r in robots().values()) >= 5, 90, "5 robots ONLINE+IDLE")
for n, r in sorted(robots().items()):
    print(f"{n:9} host={r['host']:8} conn={r['conn']:7} floor={r['floor']} can_ride={r['can_ride']} battery={r['battery']:.0f}")
for n, r in robots().items():
    if n.startswith("unity_"):
        assert r["can_ride"] is (not args.unity_no_ride), (n, r["can_ride"])

# 1. same-floor task next to the Unity robots -> a Unity robot takes it
a = req("POST", "/tasks", {"waypoints": [{"x": 6.0, "y": -5.0, "floor": 1}]})
time.sleep(2)
ta = task_state(a["id"])
print("assigned:", a["id"], "->", ta["robot"])
assert ta["robot"].startswith("unity_"), ta

# 2. a long same-floor task, cancelled while running
c = req("POST", "/tasks", {"waypoints": [{"x": -8.0, "y": -12.0, "floor": 1}]})
wait(lambda: task_state(c["id"])["state"] == "RUNNING", 20, "task c RUNNING")
print("c running on", task_state(c["id"])["robot"])
req("POST", f"/tasks/{c['id']}/cancel")
wait(lambda: task_state(c["id"])["state"] == "CANCELLED", 15, "task c CANCELLED")
print("c CANCELLED")
wait(lambda: task_state(a["id"])["state"] == "DONE", 30, "task a DONE")
print("a DONE on", task_state(a["id"])["robot"])
wait(idle_all, 30, "every robot IDLE")

# 3. cross-floor tasks submitted together
floors = [2, 3, 4, 5]
cross = [(req("POST", "/tasks", {"waypoints": [{"x": 6.0, "y": 3.0, "floor": f}]})["id"], f) for f in floors]
time.sleep(2)
for tid, f in cross:
    print("assigned:", tid, f"(floor {f}) ->", task_state(tid).get("robot") or "(queued)")
# The robot's floor is read as soon as its task ends: a robot may take a queued task right after.
at_end, end = {}, time.time() + 300
while len(at_end) < len(cross) and time.time() < end:
    for tid, f in cross:
        if tid not in at_end:
            t = task_state(tid)
            if t["state"] in ("DONE", "FAILED", "CANCELLED"):
                at_end[tid] = (t, robots()[t["robot"]]["floor"])
    time.sleep(0.5)
assert len(at_end) == len(cross), f"FAIL: cross-floor tasks not finished: {[t for t, _ in cross if t not in at_end]}"
riders = []
for tid, f in cross:
    t, floor = at_end[tid]
    print(f"{tid}: {t['state']} on {t['robot']}, task floor {f}, robot floor when done {floor}")
    assert t["state"] == "DONE", t
    assert floor == f, (t, floor)
    riders.append(t["robot"])
for trip, rid in sorted(trips.items()):
    print(f"trip {trip}: boarding order {sorted(rid, key=rid.get)}")
if args.unity_no_ride:
    assert all(r.startswith("robot_") for r in riders), riders
    print("cross-floor tasks only on ROS robots (Unity robots: can_ride false)")
else:
    assert any(r.startswith("unity_") for r in riders), f"no Unity robot rode: {riders}"
    mixed = [t for t, rid in trips.items() if any(n.startswith("unity_") for n in rid) and any(n.startswith("robot_") for n in rid)]
    print(f"Unity robots rode; trips carrying both kinds: {mixed or 'none this run'}")
stop.set()

# 4. kill the Unity process: its robots go OFFLINE (LWT), ROS robots stay ONLINE
if not args.keep_unity:
    subprocess.run(["taskkill", "/F", "/IM", "FleetRobots.exe"], capture_output=True)
    t0 = time.time()
    wait(lambda: all(r["conn"] == "OFFLINE" for n, r in robots().items() if n.startswith("unity_")), 10, "unity OFFLINE")
    print(f"unity robots OFFLINE after {time.time() - t0:.2f}s")
    rs = robots()
    assert all(r["conn"] == "ONLINE" for n, r in rs.items() if n.startswith("robot_")), rs
    print("ROS robots still ONLINE")
print("PASS")
