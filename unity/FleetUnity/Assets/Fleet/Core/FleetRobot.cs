// One robot's side of the fleet protocol (docs/ROBOT_PROTOCOL.md): what
// fleet_adapter.py does for a ROS robot, done by the robot itself. Pure: the
// caller passes the clocks in and sends the returned messages in order.
//   now  - monotonic seconds, for the publish policies and the ride timers
//   wall - unix seconds, for "stamp" and expires_at
// Elevator rides (docs/ELEVATOR.md) are RobotRide; this class wires it the
// way fleet_adapter.py does (handle_cmd, handle_elevator, tick_rides, run,
// send_fleet): the blackout (no fleet/* while Ride.Silent, received commands
// dropped before they are even parsed), the forced pose/state on link-return,
// and the elevator moves, which go straight to RobotSim instead of a ROS topic.
using System;
using System.Collections.Generic;
using System.Globalization;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;

namespace Fleet.Core
{
    public sealed class FleetRobot
    {
        public readonly string Name;
        public readonly string Host;   // must equal the <host> of fleet/adapter/<host>/status (LWT match)
        public readonly string Boot;   // new per process: fleetd then accepts a seq that starts over
        public readonly RobotSim Sim;
        public readonly RobotRide Ride;
        public string Frame = "map";
        // false sends "battery": null; fleetd's battery policy then skips the robot.
        public bool ReportBattery = true;
        // false: every state says "can_ride": false and a "ride" fails, so fleetd
        // never gives the robot a task with a waypoint on another floor.
        public bool CanRide = true;
        public Action<string> Log = _ => { };

        readonly CommandBook book = new CommandBook();
        string activeGoto;             // cmd_id of the goto being driven, reported as task_id
        ulong poseSeq, stateSeq;
        PoseSent? lastPose;
        StateSent? lastState;
        double lastWall;               // RobotRide's clock for stamps: the wall time of the current call
        bool wasSilent;

        public FleetRobot(string name, string host, RobotSim sim, string boot = null, int ignoreTurns = 0,
                          double moveExtra = RobotRide.MoveExtraS)
        {
            Name = name; Host = host; Sim = sim; Boot = boot ?? Ids.NewBoot();
            lastWall = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() / 1000.0;
            // Move ids unique per run, like the adapter's random move_base.
            Ride = new RobotRide(name, Boot, () => lastWall, ignoreTurns, new Random().Next(1 << 30), moveExtra);
        }

        public string TaskId => activeGoto ?? "";
        public ulong PoseSeq => poseSeq;
        public ulong StateSeq => stateSeq;

        /// <summary>Send pose and state on the next Tick, e.g. after a reconnect.</summary>
        public void PublishSoon()
        {
            lastPose = null;
            lastState = null;
        }

        // ---------------------------------------------------------------- commands

        public void HandleCommand(string payload, double now, double wall, List<Outgoing> outq)
        {
            lastWall = wall;
            if (Ride.Silent)
            {
                // In the car: Wi-Fi does not reach the robot, so it never gets this
                // (fleet_adapter.handle_cmd). Dropped before the CommandBook, or the
                // server's re-send after the link is back would look like a duplicate.
                Log($"{Name}: no link (in the elevator), command dropped: {Trunc(payload, 80)}");
                return;
            }
            JObject cmd;
            string cmdId, kind;
            double? expiresAt;
            try
            {
                cmd = JObject.Parse(payload);
                cmdId = cmd.Value<string>("cmd_id");
                kind = cmd["type"]?.Type == JTokenType.String ? (string)cmd["type"] : null;
                expiresAt = OptNumber(cmd["expires_at"]);
            }
            catch (Exception e) when (e is JsonException || e is FormatException || e is InvalidCastException)
            {
                Log($"{Name}: bad cmd ({e.Message}): {Trunc(payload)}");
                return;
            }
            if (string.IsNullOrEmpty(cmdId) || kind == null)
            {
                Log($"{Name}: bad cmd (no cmd_id or type): {Trunc(payload)}");
                return;
            }

            switch (book.Check(cmdId, expiresAt, wall))
            {
                case Verdict.Duplicate:
                    // Not run again; the last ack is repeated because fleetd never saw it.
                    var last = book.LastAck(cmdId);
                    if (last != null)
                        outq.Add(new Outgoing(Topics.CmdAck(Name), last, 1));
                    return;
                case Verdict.Expired:
                    Ack(outq, cmdId, "failed", "expired");
                    return;
            }

            if (kind == "cancel" && CanRide)
            {
                // A cancel for the ride, or any cancel while holding one, is the ride's business.
                var acts = Ride.Cancel(cmdId, cmd["target"]?.Type == JTokenType.String ? (string)cmd["target"] : null);
                if (acts != null)
                {
                    Run(acts, now, wall, outq);
                    return;
                }
            }
            if ((kind == "goto" || kind == "dock") && Ride.Holding)
            {
                Ack(outq, cmdId, "failed", $"holding ride {Ride.CmdId}");
                return;
            }

            switch (kind)
            {
                case "goto": DoGoto(cmd, cmdId, outq); break;
                case "cancel": DoCancel(cmd, cmdId, outq); break;
                case "dock": DoDock(cmdId, outq); break;
                case "reset_error":
                    var why = Sim.ResetError();
                    Ack(outq, cmdId, why == null ? "accepted" : "failed", why ?? "reset");
                    break;
                case "rescue":
                    // A human takes the robot out of the car (the mock's rescue service).
                    var no = Sim.Rescue();
                    Ack(outq, cmdId, no == null ? "accepted" : "failed", no ?? $"taken out at floor {Sim.Floor}");
                    break;
                case "ride":
                    if (CanRide) DoRide(cmd, cmdId, now, wall, outq);
                    else Ack(outq, cmdId, "failed", "this robot cannot ride elevators (can_ride false)");
                    break;
                default:
                    Ack(outq, cmdId, "failed", $"unknown command type '{kind}'");
                    break;
            }
        }

        /// <summary>A person on site (mockrobot/&lt;robot&gt;/operator, {"action":"reset_error"|"rescue"}),
        /// the mock's local reset_error and rescue services. Works in the blackout too and
        /// never acks: it is not a fleet command. Tick carries on what it changes.</summary>
        public void HandleOperator(string payload, double wall)
        {
            lastWall = wall;
            string action;
            try { action = (JToken.Parse(payload) as JObject)?.Value<string>("action"); }
            catch (Exception e) when (e is JsonException || e is FormatException || e is InvalidCastException)
            {
                Log($"{Name}: bad operator message: {Trunc(payload)}");
                return;
            }
            string no;
            switch (action)
            {
                case "reset_error": no = Sim.ResetError(); break;
                case "rescue": no = Sim.Rescue(); break;
                default:
                    Log($"{Name}: unknown operator action: {Trunc(payload)}");
                    return;
            }
            Log($"{Name}: operator {action}: {no ?? "done"}");
        }

        void DoRide(JObject cmd, string cmdId, double now, double wall, List<Outgoing> outq)
        {
            var acts = Ride.Accept(cmdId, cmd["ride"] as JObject, RideState(), new Spot(Sim.X, Sim.Y), now);
            if (Ride.CmdId == cmdId && activeGoto != null)
            {
                // A goto still running: stop the robot; its ack goes before the ride's "accepted".
                Sim.Stop();
                EndGoto(outq, "cancelled", $"replaced by ride {cmdId}");
            }
            Run(acts, now, wall, outq);
        }

        void DoGoto(JObject cmd, string cmdId, List<Outgoing> outq)
        {
            double x, y, yaw;
            try
            {
                x = Number(cmd["x"]);
                y = Number(cmd["y"]);
                yaw = OptNumber(cmd["yaw"]) ?? 0.0;
            }
            catch (FormatException)
            {
                Ack(outq, cmdId, "failed", "goto needs numeric x and y");
                return;
            }
            var refused = Sim.Goto(x, y, yaw);
            if (refused != null)
            {
                Ack(outq, cmdId, "failed", refused);
                return;
            }
            // A new goal preempts the old goto: it ended "cancelled", not "failed".
            if (activeGoto != null)
                Ack(outq, activeGoto, "cancelled", $"preempted by {cmdId}");
            activeGoto = cmdId;
            Ack(outq, cmdId, "accepted", "");
        }

        void DoCancel(JObject cmd, string cmdId, List<Outgoing> outq)
        {
            // fleetd always names the goto it means (target). Unlike the ROS
            // adapter, which stops whatever runs, a cancel for another cmd_id
            // leaves the current goto alone: a late cancel must not stop a newer task.
            var target = cmd["target"]?.Type == JTokenType.String ? (string)cmd["target"] : "";
            if (target != "" && target != activeGoto)
            {
                Ack(outq, cmdId, "failed", activeGoto == null
                    ? $"{target} is not running (nothing to cancel, {Sim.Mode})"
                    : $"{target} is not running (driving {activeGoto})");
                return;
            }
            var why = Sim.Stop();
            if (why != null)
            {
                Ack(outq, cmdId, "failed", why);
                return;
            }
            EndGoto(outq, "cancelled", $"cancel requested by {cmdId}");
            Ack(outq, cmdId, "accepted", "cancelled");
        }

        void DoDock(string cmdId, List<Outgoing> outq)
        {
            var why = Sim.Dock();
            if (why != null)
            {
                Ack(outq, cmdId, "failed", why);
                return;
            }
            // A dock gets one ack; the battery policy follows the mode (DOCKING, CHARGING).
            EndGoto(outq, "cancelled", $"dock requested by {cmdId}");
            Ack(outq, cmdId, "accepted", "docking");
        }

        void EndGoto(List<Outgoing> outq, string status, string message)
        {
            if (activeGoto == null) return;
            var id = activeGoto;
            activeGoto = null;
            Ack(outq, id, status, message);
        }

        void Ack(List<Outgoing> outq, string cmdId, string status, string message, int? floor = null)
        {
            var body = new JObject { ["cmd_id"] = cmdId, ["status"] = status, ["message"] = message };
            if (floor.HasValue) body["floor"] = floor.Value;
            var json = body.ToString(Formatting.None);
            book.Record(cmdId, json);
            Log($"{Name}: {cmdId} {status} {message}{(floor.HasValue ? $" (floor {floor})" : "")}");
            if (Ride.Silent) return; // recorded, but no link to send it (fleet_adapter.send_fleet)
            outq.Add(new Outgoing(Topics.CmdAck(Name), json, 1));
        }

        // ---------------------------------------------------------------- elevator channel

        /// <summary>A message on elevator/&lt;car&gt;/turn|state|trip (fleet_adapter.handle_elevator).
        /// msg is null for a cleared retained trip. Only a robot riding that car reacts.</summary>
        public void HandleElevator(string car, string kind, JObject msg, double now, double wall, List<Outgoing> outq)
        {
            lastWall = wall;
            if (!CanRide || string.IsNullOrEmpty(car) || Ride.Elevator != car) return;
            switch (kind)
            {
                case "turn": if (msg != null) Run(Ride.Turn(msg, now), now, wall, outq); break;
                case "state": if (msg != null) Run(Ride.CarState(msg), now, wall, outq); break;
                // Only its end matters: robots never move on the retained trip's turn.
                case "trip": Run(Ride.TripMsg(msg, now), now, wall, outq); break;
            }
        }

        RideRobotState RideState() => new RideRobotState
        {
            Mode = Sim.Mode, Error = Sim.Error, Floor = Sim.Floor, InCar = Sim.InCar, Move = Sim.LastMove,
        };

        /// <summary>Carries out RobotRide actions, in order (fleet_adapter.run).</summary>
        void Run(List<RideAction> actions, double now, double wall, List<Outgoing> outq)
        {
            foreach (var a in actions)
            {
                switch (a.Kind)
                {
                    case RideKind.Ack:
                        Ack(outq, a.CmdId, a.Status, a.Message, a.Floor);
                        break;
                    case RideKind.State:
                        PublishState(now, wall, outq, true, a.Override);
                        break;
                    case RideKind.Pose:
                        PublishPose(now, wall, outq, true);
                        break;
                    case RideKind.Event:
                        // RobotRide queues events itself while there is no link; the ones it
                        // hands out go now (the "boarding" event goes with the last state).
                        outq.Add(new Outgoing(Topics.Event(Name), a.Body.ToString(Formatting.None), 1));
                        break;
                    case RideKind.Zone:
                        outq.Add(new Outgoing(Topics.Zone(a.Car), a.Body.ToString(Formatting.None), 1));
                        Log($"{Name}: zone n={a.Body["n"]} {a.Body["action"]} {a.Body["event"]} floor {a.Body["floor"]}");
                        break;
                    case RideKind.Move:
                        Sim.StartMove(a.Move);
                        break;
                }
            }
            if (Ride.Silent != wasSilent)
            {
                wasSilent = Ride.Silent;
                Log($"{Name}: Wi-Fi {(Ride.Silent ? "lost (in the elevator)" : "back")}");
            }
        }

        // ---------------------------------------------------------------- simulation + publishing

        public void Tick(double dt, double now, double wall, List<Outgoing> outq)
        {
            lastWall = wall;
            Sim.Step(dt);
            // The robot's report of its elevator move (fleet_adapter RobotBridge.on_state).
            var mv = Sim.LastMove;
            if (mv != null && Ride.Moving)
                Run(Ride.MoveResult(mv.Id, mv.Status, mv.Detail, now), now, wall, outq);
            if (activeGoto != null)
            {
                if (Sim.Mode == Modes.Error)
                    EndGoto(outq, "failed", $"robot in ERROR: {Sim.Error}");
                else if (!Sim.HasGoal && Sim.Mode == Modes.Idle)
                    EndGoto(outq, "done", "");
            }
            // fleet_adapter.tick_rides; the robot is always live here (same process).
            if (Ride.Holding || Ride.Stranded != "")
                Run(Ride.Tick(now, Sim.Floor, RideState()), now, wall, outq);
            PublishState(now, wall, outq);
            PublishPose(now, wall, outq);
        }

        /// <summary>fleet_adapter.publish_state. over is sent even in the blackout:
        /// it is the last state before the link goes (BOARDING with silence_s).
        /// force skips the 1 s policy (the state goes now) but still resets it.</summary>
        void PublishState(double now, double wall, List<Outgoing> outq, bool force = false, RideOverlay over = null)
        {
            if (Ride.Silent && over == null)
                return;
            var ov = over ?? Ride.Overlay(RideState());
            string mode = ov?.Mode ?? Sim.Mode;
            string taskId = ov != null ? ov.TaskId ?? "" : TaskId;
            double silence = ov?.SilenceS ?? 0;
            string error = ov?.Error ?? Sim.Error;
            // Same key as the adapter: a change of mode, error, task_id or silence_s goes out at once.
            var key = $"{mode}|{error}|{taskId}|{silence.ToString(CultureInfo.InvariantCulture)}";
            if (!force && !Throttle.StateDue(lastState, now, key))
                return;
            stateSeq++;
            lastState = new StateSent(now, key);
            var s = new JObject
            {
                ["boot"] = Boot, ["seq"] = stateSeq, ["stamp"] = Math.Round(wall, 3), ["host"] = Host,
                ["mode"] = mode,
                ["battery"] = ReportBattery ? (JToken)Math.Round(Sim.Battery, 1) : JValue.CreateNull(),
                ["task_id"] = taskId, ["error"] = error, ["silence_s"] = silence,
                ["can_ride"] = CanRide,
            };
            outq.Add(new Outgoing(Topics.State(Name), s.ToString(Formatting.None), 1));
        }

        void PublishPose(double now, double wall, List<Outgoing> outq, bool force = false)
        {
            if (Ride.Silent)
                return;
            if (!force && !Throttle.PoseDueRated(lastPose, now, Sim.X, Sim.Y, Sim.Yaw))
                return;
            poseSeq++;
            lastPose = new PoseSent(now, Sim.X, Sim.Y, Sim.Yaw);
            var p = new JObject
            {
                ["boot"] = Boot, ["seq"] = poseSeq, ["stamp"] = Math.Round(wall, 3), ["host"] = Host,
                ["frame"] = Frame, ["floor"] = Sim.Floor,
                ["x"] = Math.Round(Sim.X, 3), ["y"] = Math.Round(Sim.Y, 3),
                ["yaw"] = Math.Round(Sim.Yaw, 3), ["v"] = Math.Round(Sim.V, 3),
            };
            outq.Add(new Outgoing(Topics.Pose(Name), p.ToString(Formatting.None), 0));
        }

        // ---------------------------------------------------------------- helpers

        // Like Python's float(): numbers and numeric strings.
        static double Number(JToken t)
        {
            if (t == null) throw new FormatException("missing");
            if (t.Type == JTokenType.Float || t.Type == JTokenType.Integer) return t.Value<double>();
            if (t.Type == JTokenType.String &&
                double.TryParse((string)t, NumberStyles.Float, CultureInfo.InvariantCulture, out var v)) return v;
            throw new FormatException($"not a number: {t}");
        }

        static double? OptNumber(JToken t) =>
            t == null || t.Type == JTokenType.Null ? (double?)null : Number(t);

        static string Trunc(string s, int n = 120) => s.Length <= n ? s : s.Substring(0, n);
    }
}
