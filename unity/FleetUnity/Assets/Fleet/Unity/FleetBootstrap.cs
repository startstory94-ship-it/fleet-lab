// Spawns the robots, owns the MQTT link and drives the protocol core every
// frame on the main thread. Created automatically at start (no scene wiring),
// so the build's scene can stay empty and a -batchmode -nographics player works.
using System;
using System.Collections.Generic;
using System.Diagnostics;
using Fleet.Core;
using UnityEngine;
using Debug = UnityEngine.Debug;

namespace Fleet.UnityApp
{
    public sealed class FleetBootstrap : MonoBehaviour
    {
        BootArgs args;
        FleetMqtt mqtt;
        readonly List<FleetRobot> robots = new List<FleetRobot>();
        readonly Dictionary<string, FleetRobot> byName = new Dictionary<string, FleetRobot>();
        readonly List<RobotView> views = new List<RobotView>();
        readonly List<Outgoing> outq = new List<Outgoing>();
        readonly Stopwatch mono = Stopwatch.StartNew();   // policies use a monotonic clock, never Time.time
        double lastNow;
        int seenConnects;

        [RuntimeInitializeOnLoadMethod(RuntimeInitializeLoadType.AfterSceneLoad)]
        static void AutoCreate()
        {
            // This assembly ships in the dashboard player too; robots there would
            // connect to MQTT as a second host "unity-1".
            if (UnityEngine.SceneManagement.SceneManager.GetActiveScene().name == "Dashboard") return;
            if (FindAnyObjectByType<FleetBootstrap>() == null)
                new GameObject("FleetBootstrap").AddComponent<FleetBootstrap>();
        }

        static double Wall() => DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() / 1000.0;

        void Awake()
        {
            // MQTT keepalive and fleetd's 2 s STALE timer need us to keep
            // running when the window is not focused or there is no window.
            Application.runInBackground = true;
            QualitySettings.vSyncCount = 0;
            Application.targetFrameRate = Application.isBatchMode ? 30 : 60; // batchmode would spin a core otherwise

            args = BootArgs.Parse(Environment.GetCommandLineArgs());
            foreach (var w in args.Warnings) Debug.LogWarning($"[fleet] {w}");

            var names = new List<string>();
            for (int i = 0; i < args.Robots; i++)
            {
                var name = args.RobotName(i);
                var sim = new RobotSim(args.OriginX + i * args.Spacing, args.OriginY, args.Battery)
                {
                    Speed = args.Speed,
                    Floor = args.Floor,
                    ExitFailFloor = args.ExitFailFloor,
                    ExitFailCount = args.ExitFailFloor.HasValue ? args.ExitFailCount : 0,
                    DoorMoveS = args.DoorMoveS,
                };
                args.IgnoreTurns.TryGetValue(name, out var ignore);
                var r = new FleetRobot(name, args.Host, sim, ignoreTurns: ignore, moveExtra: args.MoveTimeoutExtra)
                {
                    CanRide = args.CanRide,
                    Log = s => Debug.Log($"[fleet] {s}"),
                };
                robots.Add(r);
                byName[name] = r;
                names.Add(name);
                views.Add(RobotView.Create(r, transform));
            }
            if (!Application.isBatchMode)
                RobotView.BuildStage(args, transform);

            mqtt = new FleetMqtt(args.BrokerHost, args.BrokerPort, args.Host, names,
                                 args.CanRide ? Topics.ElevatorFollow : null) { Log = s => Debug.Log($"[fleet] {s}") };
            mqtt.Start();
            lastNow = mono.Elapsed.TotalSeconds;
            Debug.Log($"[fleet] {args.Robots} robots {args.Prefix}_1..{args.Robots} on floor {args.Floor}, host {args.Host}, " +
                      $"broker {args.BrokerHost}:{args.BrokerPort}, origin ({args.OriginX}, {args.OriginY}), speed {args.Speed} m/s, " +
                      $"can_ride {args.CanRide}" + (args.ExitFailFloor.HasValue ? $", exits fail on floor {args.ExitFailFloor} ({args.ExitFailCount})" : "") +
                      (args.DoorMoveS > 0 ? $", door moves {args.DoorMoveS} s" : ""));
        }

        void Update()
        {
            double now = mono.Elapsed.TotalSeconds, wall = Wall();
            double dt = Math.Min(now - lastNow, 0.5); // a stalled frame must not teleport robots
            lastNow = now;

            if (mqtt.Connects != seenConnects)
            {
                // Fresh connection: state and pose at once, so fleetd sees us without waiting a heartbeat.
                seenConnects = mqtt.Connects;
                foreach (var r in robots) r.PublishSoon();
            }
            while (mqtt.TryReceive(out var topic, out var payload))
            {
                if (Topics.ParseElevator(topic, out var car, out var kind))
                {
                    // Parsed once; every robot decides itself whether it rides that car.
                    if (Topics.ElevatorBody(kind, payload, out var msg))
                        foreach (var rr in robots)
                            rr.HandleElevator(car, kind, msg, now, wall, outq);
                    continue;
                }
                var name = Topics.RobotOfCmd(topic);
                if (name != null && byName.TryGetValue(name, out var r))
                    r.HandleCommand(payload, now, wall, outq);
            }
            foreach (var r in robots)
                r.Tick(dt, now, wall, outq);
            foreach (var o in outq)
                mqtt.Send(o);
            outq.Clear();
            foreach (var v in views)
                v.Sync();
        }

        void OnApplicationQuit()
        {
            mqtt?.Dispose();
            Debug.Log("[fleet] offline (clean quit)");
        }
    }
}
