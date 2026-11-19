# Live reconnect check for review finding 1 (stage 8):
# a Unity robot's MQTT link drops right after it leaves the elevator, so its
# link-return messages (pose/state/events/done) are produced while disconnected.
# They must be kept and sent in order after reconnecting.
#
# Only the robot's link goes through a TCP proxy (18831 -> 1883) that we cut;
# fleetd, the mock elevator and the observer stay on the broker.
#
# Needs: Mosquitto on 127.0.0.1:1883, bin/fleetd.exe, bin/mockelevator.exe and
# the Unity robots build (scripts/unity_build.ps1 -Target robots); port 8080 free.
#   py -3 scripts/unity_reconnect_check.py [cut_s] ["exit clear"|"exit enter"] [out_dir]
# Writes run.log-style lines to stdout and sub.out (broker traffic as JSON
# lines), events.json, task.json, marks.json to out_dir.
import json, os, socket, subprocess, sys, tempfile, threading, time, urllib.request

R = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
D = sys.argv[3] if len(sys.argv) > 3 else os.path.join(tempfile.gettempdir(), "fleet_reconnect")
M = os.environ.get("MOSQUITTO_DIR", r"C:\Program Files\mosquitto")
os.makedirs(D, exist_ok=True)
API = "http://127.0.0.1:8080"
CUT_S = float(sys.argv[1]) if len(sys.argv) > 1 else 6.0
TRIGGER = sys.argv[2] if len(sys.argv) > 2 else "exit clear"

marks = []  # (time, label)
def mark(label):
    marks.append((time.time(), label))
    print(f"{time.time():.3f} MARK {label}", flush=True)

# ---- proxy -------------------------------------------------------------
cut = threading.Event()
conns = []
lock = threading.Lock()

def pump(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d:
                break
            b.sendall(d)
    except OSError:
        pass
    for s in (a, b):
        try: s.close()
        except OSError: pass

def proxy():
    ls = socket.socket(); ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    ls.bind(("127.0.0.1", 18831)); ls.listen(8)
    while True:
        c, _ = ls.accept()
        if cut.is_set():
            c.close(); continue
        u = socket.create_connection(("127.0.0.1", 1883))
        with lock: conns.extend([c, u])
        threading.Thread(target=pump, args=(c, u), daemon=True).start()
        threading.Thread(target=pump, args=(u, c), daemon=True).start()

def do_cut():
    cut.set()
    with lock:
        for s in conns:
            try: s.shutdown(socket.SHUT_RDWR)
            except OSError: pass
            try: s.close()
            except OSError: pass
        conns.clear()

def req(m, p, b=None):
    r = urllib.request.Request(API + p, data=json.dumps(b).encode() if b is not None else None,
                               method=m, headers={"Content-Type": "application/json"})
    return json.load(urllib.request.urlopen(r, timeout=5))

# ---- run ---------------------------------------------------------------
for f in os.listdir(D):
    if f.startswith(("f.db", "sub.out", "robots.log")):
        os.remove(os.path.join(D, f))
procs = []
def start(args, out=None, err=None):
    p = subprocess.Popen(args, stdout=open(out, "w") if out else subprocess.DEVNULL,
                         stderr=open(err, "w") if err else subprocess.DEVNULL)
    procs.append(p); return p

threading.Thread(target=proxy, daemon=True).start()
start([rf"{M}\mosquitto_sub.exe", "-p", "1883", "-v", "-W", "100", "-F", "%j", "-t", "fleet/#", "-t", "elevator/#"],
      out=rf"{D}\sub.out")
start([rf"{R}\bin\mockelevator.exe"], rf"{D}\elev.out", rf"{D}\elev.err")
start([rf"{R}\bin\fleetd.exe", "-http", "127.0.0.1:8080", "-db", rf"{D}\f.db"], rf"{D}\fleetd.out", rf"{D}\fleetd.err")
time.sleep(2)
start([rf"{R}\unity\FleetUnity\Build\FleetRobots.exe", "-batchmode", "-nographics", "-logFile", rf"{D}\robots.log",
       "-broker", "127.0.0.1:18831", "-host", "unity-1", "-robots", "1", "-prefix", "unity", "-floor", "1",
       "-origin", "0,-3", "-spacing", "2", "-speed", "0.8", "-canRide", "true"])
try:
    for _ in range(60):
        try:
            if any(r["name"] == "unity_1" for r in req("GET", "/robots")):
                break
        except Exception:
            pass
        time.sleep(0.5)
    time.sleep(2)
    t = req("POST", "/tasks", {"waypoints": [{"x": 6.0, "y": 3.0, "floor": 3}]})
    mark(f"task {t['id']}")

    logpos = 0; done_cut = False; restore_at = None; last = None; end = time.time() + 150
    while time.time() < end:
        try:
            with open(rf"{D}\robots.log", encoding="utf-8", errors="replace") as fh:
                fh.seek(logpos); chunk = fh.read(); logpos = fh.tell()
        except FileNotFoundError:
            chunk = ""
        for line in chunk.splitlines():
            if line.startswith("[fleet]"):
                print(f"{time.time():.3f} LOG {line}", flush=True)
                if not done_cut and TRIGGER in line:
                    do_cut(); done_cut = True; restore_at = time.time() + CUT_S
                    mark(f"link CUT (after '{TRIGGER}')")
        if restore_at and time.time() >= restore_at:
            cut.clear(); restore_at = None; mark("link RESTORED")
        st = req("GET", "/tasks/" + t["id"])
        rb = {x["name"]: x for x in req("GET", "/robots")}.get("unity_1", {})
        cur = (st["state"], rb.get("mode"), rb.get("floor"), rb.get("online"))
        if cur != last:
            print(f"{time.time():.3f} API task={cur[0]} mode={cur[1]} floor={cur[2]} online={cur[3]}", flush=True)
            last = cur
        if st["state"] in ("DONE", "FAILED", "CANCELLED") and not restore_at and done_cut:
            time.sleep(3); break
        time.sleep(0.03)
    json.dump(req("GET", "/tasks/" + t["id"]), open(rf"{D}\task.json", "w"), indent=1)
    json.dump(marks, open(rf"{D}\marks.json", "w"))
    json.dump(req("GET", "/events"), open(rf"{D}\events.json", "w"), indent=1)
    print("waiting for mosquitto_sub -W to end...", flush=True)
    procs[0].wait(timeout=120)
finally:
    for p in reversed(procs):
        try: p.kill()
        except Exception: pass
