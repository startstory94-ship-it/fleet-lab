// The 3D fleet dashboard: a viewer of fleetd (never of MQTT), so ROS and
// Unity robots look the same. /ws/live feeds LiveModel (the port of
// web/app.js); GET /elevators, /tasks and /events are polled. Lives only in
// Assets/Scenes/Dashboard.unity, which DashboardScene.cs builds.
//   keys: 1-9 / 0 focus floor 1-9 / 10, F follow the car, H overview
using System;
using System.Collections.Generic;
using System.Linq;
using System.Text;
using Fleet.Dashboard;
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public sealed class DashboardApp : MonoBehaviour
    {
        // Assigned by DashboardScene.cs so the build keeps these shaders.
        public Material OpaqueMaterial, FadeMaterial, LinesMaterial, TextMaterial;

        DashArgs args;
        ILiveTransport live;
        FleetPoller poll;
        readonly LiveModel model = new LiveModel();
        readonly Bounds2 bounds = new Bounds2();
        Transform world;
        FloorStack floors;
        readonly Dictionary<string, RobotMarker> markers = new Dictionary<string, RobotMarker>();
        readonly Dictionary<string, CarView> cars = new Dictionary<string, CarView>();
        readonly List<GameObject> pins = new List<GameObject>();
        Dictionary<string, InCar> inCar = new Dictionary<string, InCar>();
        Camera cam;
        OrbitCamera orbit;
        bool follow, framed;
        int? focus;
        FrameCapture capture;
        static readonly DateTime Epoch = new DateTime(1970, 1, 1, 0, 0, 0, DateTimeKind.Utc);

        static double NowMs() => (DateTime.UtcNow - Epoch).TotalMilliseconds;

        float FloorY(double floor) => (float)((floor - 1) * args.FloorHeight);

        void Awake()
        {
            // fleetd closes a /ws/live client that takes no frame for 5 s
            // (api.go writeTimeout): a paused, unfocused player would be dropped.
            Application.runInBackground = true;
            QualitySettings.vSyncCount = 0;
            Application.targetFrameRate = 60;

            args = DashArgs.Parse(Environment.GetCommandLineArgs());
            foreach (var w in args.Warnings) Debug.LogWarning($"[dash] {w}");
            follow = args.Follow;
            focus = args.Focus;

            Prims.Opaque = OpaqueMaterial;
            Prims.Fade = FadeMaterial;
            Prims.Lines = LinesMaterial;
            Prims.Font = Resources.GetBuiltinResource<Font>("LegacyRuntime.ttf");

            world = new GameObject("world").transform;
            floors = new GameObject("floors").AddComponent<FloorStack>();
            floors.transform.SetParent(world, false);
            floors.FloorY = f => FloorY(f);

            cam = Camera.main;
            orbit = cam.gameObject.AddComponent<OrbitCamera>();
            if (args.Yaw.HasValue) orbit.Yaw = (float)args.Yaw.Value;
            if (args.Pitch.HasValue) orbit.Pitch = (float)args.Pitch.Value;

            poll = gameObject.AddComponent<FleetPoller>();
            poll.Api = args.Api;
            poll.ElevatorsChanged += OnElevators;

            model.Log = s => Debug.LogWarning($"[dash] {s}");
            var t = new ClientWebSocketTransport(args.LiveUrl, args.Api) { Log = s => Debug.Log($"[dash] {s}") };
            live = t;
            live.Start();

            if (!string.IsNullOrEmpty(args.CaptureDir))
            {
                capture = gameObject.AddComponent<FrameCapture>();
                capture.Dir = args.CaptureDir;
                capture.IntervalMs = args.CaptureMs;
                capture.Seconds = args.CaptureSeconds;
                capture.QuitWhenDone = args.CaptureQuit;
            }
            Debug.Log($"[dash] api {args.Api}, live {args.LiveUrl}, floor height {args.FloorHeight} m");
        }

        void OnElevators()
        {
            double now = NowMs();
            foreach (var v in poll.Elevators)
            {
                if (!cars.TryGetValue(v.Id, out var c))
                    cars[v.Id] = c = CarView.Create(world, v, f => FloorY(f));
                c.Observe(v, now);
            }
            inCar = Elevators.RidersInCars(poll.Elevators);
        }

        void Update()
        {
            double now = NowMs();
            while (live.TryReceive(out var json))
            {
                try { model.OnFrame(LiveModel.Parse(json), now); }
                catch (Exception e) { Debug.LogWarning($"[dash] bad frame: {e.Message}"); }
            }
            Keys();

            // interestingPoints (app.js 636-652), over all floors.
            foreach (var r in model.Robots.Values)
                if (r.Track.Count > 0) bounds.Add(r.Track[r.Track.Count - 1].X, r.Track[r.Track.Count - 1].Y);
            foreach (var v in poll.Elevators)
            {
                var b = Elevators.Box(v.Lobby, v.Capacity);
                bounds.Add(v.Lobby.X, v.Lobby.Y);
                bounds.Add(b.X1, b.Y1);
            }
            foreach (var t in poll.Tasks)
                if (TaskView.Active.Contains(t.State))
                    foreach (var w in t.Waypoints) bounds.Add(w.X, w.Y);

            var floorList = Elevators.FloorsToDraw(poll.Elevators, model.Robots.Values.Select(r => r.V), poll.Tasks);
            floors.Sync(floorList, bounds.Box(), poll.Elevators, (float)args.FloorHeight);
            floors.FaceLabels(cam);

            foreach (var c in cars.Values) c.Tick(now, Time.unscaledDeltaTime, cam, false);
            DrawRobots(now);
            DrawPins();
            Frame();
        }

        void DrawRobots(double now)
        {
            double t = model.RenderTime(now);
            var seen = new HashSet<string>();
            foreach (var kv in model.Robots)
            {
                var name = kv.Key;
                var r = kv.Value;
                if (inCar.ContainsKey(name)) continue; // drawn in its car below
                var p = LiveModel.PoseAt(r.Track, t, r.V);
                if (p == null) continue;
                var s = p.Value;
                Marker(name).Show(r.V, r.V.Conn, Prims.W(s.X, s.Y, FloorY(s.Floor)), Prims.Yaw(s.Yaw), "", cam);
                seen.Add(name);
            }
            // Robots that may be in a car, by slot, in the elevator colour (app.js 806-811).
            foreach (var kv in inCar)
            {
                if (!cars.TryGetValue(kv.Value.Car, out var car)) continue;
                model.Robots.TryGetValue(kv.Key, out var r);
                var slot = car.View.Trip?.Riders.FirstOrDefault(x => x.Robot == kv.Key);
                string extra = slot != null ? $" > {slot.Exit}F" : "";
                Marker(kv.Key).Show(r?.V, "IN_ELEVATOR", car.SlotPosition(kv.Value.Slot), Prims.Yaw(Math.PI), extra, cam, Math.Max(0, kv.Value.Slot));
                seen.Add(kv.Key);
            }
            foreach (var name in markers.Keys.Where(n => !seen.Contains(n)).ToList())
            {
                Destroy(markers[name].gameObject);
                markers.Remove(name);
            }
        }

        RobotMarker Marker(string name)
        {
            if (!markers.TryGetValue(name, out var m))
                markers[name] = m = RobotMarker.Create(world, name);
            return m;
        }

        /// <summary>Waypoints of active tasks still ahead, as orange pins.</summary>
        void DrawPins()
        {
            int n = 0;
            foreach (var t in poll.Tasks)
            {
                if (!TaskView.Active.Contains(t.State)) continue;
                for (int i = t.Leg; i < t.Waypoints.Count; i++)
                {
                    var w = t.Waypoints[i];
                    if (n == pins.Count)
                    {
                        var g = Prims.Make(PrimitiveType.Cylinder, world, "pin", Prims.Rgb(RobotStyle.TaskRgb));
                        g.transform.localScale = new Vector3(0.18f, 0.25f, 0.18f);
                        pins.Add(g);
                    }
                    pins[n].SetActive(true);
                    pins[n].transform.localPosition = Prims.W(w.X, w.Y, FloorY(w.Floor) + 0.25f);
                    n++;
                }
            }
            for (int i = n; i < pins.Count; i++) pins[i].SetActive(false);
        }

        void Keys()
        {
            for (int k = 0; k <= 9; k++)
                if (Input.GetKeyDown(KeyCode.Alpha0 + k) || Input.GetKeyDown(KeyCode.Keypad0 + k))
                {
                    focus = k == 0 ? 10 : k;
                    follow = false;
                }
            if (Input.GetKeyDown(KeyCode.F)) { follow = !follow; if (follow) focus = null; }
            if (Input.GetKeyDown(KeyCode.H) || Input.GetKeyDown(KeyCode.Home)) { follow = false; focus = null; framed = false; }
        }

        /// <summary>Points the camera: the followed car, a focused floor, or the whole stack.</summary>
        void Frame()
        {
            // Size the first view on the fleet, not on the empty map before the first snapshot.
            if (!framed && (model.Snapshots == 0 || poll.Elevators.Count == 0) && Time.realtimeSinceStartup < 5) return;
            var box = bounds.Box();
            var centre = Prims.W((box.minX + box.maxX) / 2, (box.minY + box.maxY) / 2, 0);
            float span = (float)Math.Max(box.maxX - box.minX, box.maxY - box.minY);
            var car = cars.Values.FirstOrDefault();
            if (follow && car != null)
            {
                var c = car.CarCentre;
                // Look at the building near the car's height, not at the car alone.
                orbit.Target = new Vector3(centre.x, c.y, centre.z);
                if (!framed) { orbit.Distance = (float)(args.Distance ?? span * 1.5f); orbit.JumpTo(orbit.Target); framed = true; }
            }
            else if (focus.HasValue)
            {
                orbit.Target = new Vector3(centre.x, FloorY(focus.Value), centre.z);
                if (!framed) { orbit.Distance = (float)(args.Distance ?? span * 1.6f); orbit.JumpTo(orbit.Target); framed = true; }
            }
            else if (!framed && floors.Floors.Count > 0)
            {
                float lo = FloorY(floors.Floors[0]), hi = FloorY(floors.Floors[floors.Floors.Count - 1]);
                orbit.Distance = (float)(args.Distance ?? Math.Max(span * 1.4f, (hi - lo) * 1.6f));
                orbit.JumpTo(new Vector3(centre.x, (lo + hi) / 2, centre.z));
                framed = true;
            }
        }

        // ---------------------------------------------------------------- HUD

        GUIStyle text, small, head;
        Texture2D white;

        void OnGUI()
        {
            if (text == null)
            {
                white = Texture2D.whiteTexture;
                text = new GUIStyle(GUI.skin.label) { fontSize = 14, richText = true };
                text.normal.textColor = new Color(0.9f, 0.92f, 0.96f);
                small = new GUIStyle(text) { fontSize = 12 };
                head = new GUIStyle(text) { fontSize = 16, fontStyle = FontStyle.Bold };
            }
            var sb = new StringBuilder();
            string liveText;
            switch (live.State)
            {
                case LinkState.Up: liveText = $"<color=#5fd38d>connected</color>  seq {model.Seq}" + (model.Gaps > 0 ? $", {model.Gaps} gaps" : ""); break;
                case LinkState.Full: liveText = $"<color=#ff7b7b>server full</color>  {live.Detail}"; break;
                case LinkState.Connecting: liveText = "<color=#f0c060>connecting</color>"; break;
                default: liveText = $"<color=#ff7b7b>down</color>  {live.Detail}"; break;
            }
            sb.AppendLine($"live  {liveText}");
            sb.AppendLine(poll.ApiUp ? "REST  <color=#5fd38d>ok</color>" : $"REST  <color=#ff7b7b>{poll.ApiError}</color>");
            var byConn = model.Robots.Values.GroupBy(r => r.V.Conn).ToDictionary(g => g.Key, g => g.Count());
            int C(string k) => byConn.TryGetValue(k, out var n) ? n : 0;
            sb.AppendLine($"robots {model.Robots.Count}   online {C("ONLINE")} · stale {C("STALE")} · offline {C("OFFLINE")} · in elevator {C("IN_ELEVATOR")}");
            sb.AppendLine($"tasks  {TaskView.CountActive(poll.Tasks)} active / {poll.Tasks.Count}");
            foreach (var v in poll.Elevators)
            {
                var s = v.State;
                string st = s == null ? "no state" : $"{s.Floor}F {(s.Dir == "idle" ? "" : s.Dir)} · door {s.Door}{(s.Moving ? " · moving" : "")}";
                sb.Append($"{v.Id}  {st}");
                if (v.Trip != null)
                    sb.Append($"\n     trip {v.Trip.Id} · {v.Trip.Stage}" + (v.Trip.Stage == "travel" ? $" to {v.Trip.Floor}F" : "") +
                              $" · stops {string.Join(">", v.Trip.Stops)}");
                var slots = Elevators.CarSlots(v);
                if (slots.Any(x => x != null))
                    sb.Append("\n     " + string.Join("  ", slots.Select((o, i) => $"[{i}] " + (o == null ? "-" : $"{o.Robot}>{o.Exit}F {o.Status}"))));
                sb.AppendLine();
            }
            var rect = new Rect(12, 12, 560, 30 + 19 * CountLines(sb));
            Panel(rect);
            GUI.Label(new Rect(rect.x + 10, rect.y + 6, rect.width, 22), $"fleetd 3D  <size=12>{args.Api}</size>", head);
            GUI.Label(new Rect(rect.x + 10, rect.y + 28, rect.width - 20, rect.height), sb.ToString(), text);

            // Legend, as on the 2D map.
            var legend = new (string, Color)[]
            {
                ("IDLE", Prims.ColorOf(Look.Idle)), ("MOVING", Prims.ColorOf(Look.Moving)), ("CHARGING", Prims.ColorOf(Look.Charging)),
                ("ERROR", Prims.ColorOf(Look.Error)), ("STALE", Prims.ColorOf(Look.Idle, 0.35f)), ("OFFLINE", Prims.ColorOf(Look.Offline)),
                ("ELEVATOR", Prims.ColorOf(Look.Elevator)),
            };
            var lr = new Rect(12, rect.yMax + 6, 560, 26);
            Panel(lr);
            float x = lr.x + 10;
            foreach (var (name, color) in legend)
            {
                var old = GUI.color;
                GUI.color = color;
                GUI.DrawTexture(new Rect(x, lr.y + 8, 11, 11), white);
                GUI.color = old;
                GUI.Label(new Rect(x + 15, lr.y + 4, 80, 20), name, small);
                x += 22 + small.CalcSize(new GUIContent(name)).x;
            }

            // Last events.
            if (poll.Events.Count > 0)
            {
                var ev = new StringBuilder();
                foreach (var e in poll.Events.OrderByDescending(e => e.Seq).Take(6))
                {
                    double ms = LiveModel.UnixMs(e.Time);
                    string hhmmss = double.IsNaN(ms) ? "" : Epoch.AddMilliseconds(ms).ToLocalTime().ToString("HH:mm:ss");
                    ev.AppendLine($"<color=#9aa3b2>{hhmmss}</color>  {Trim(e.Describe(), 70)}");
                }
                var er = new Rect(Screen.width - 472, 12, 460, 12 + 15 * CountLines(ev));
                Panel(er);
                GUI.Label(new Rect(er.x + 10, er.y + 4, er.width - 20, er.height), ev.ToString(), small);
            }

            var help = "drag orbit · right/middle drag pan · wheel zoom · 1-9/0 floor · F follow car" + (follow ? " (on)" : "") + " · H overview";
            var hr = new Rect(12, Screen.height - 32, small.CalcSize(new GUIContent(help)).x + 20, 22);
            Panel(hr);
            GUI.Label(new Rect(hr.x + 10, hr.y + 3, hr.width, 20), help, small);
        }

        void Panel(Rect r)
        {
            var old = GUI.color;
            GUI.color = new Color(0.06f, 0.07f, 0.1f, 0.78f);
            GUI.DrawTexture(r, white);
            GUI.color = old;
        }

        static int CountLines(StringBuilder sb)
        {
            int n = 0;
            for (int i = 0; i < sb.Length; i++) if (sb[i] == '\n') n++;
            return n;
        }

        static string Trim(string s, int n) => s.Length <= n ? s : s.Substring(0, n - 1) + "…";

        void OnDestroy()
        {
            live?.Dispose();
        }
    }
}
