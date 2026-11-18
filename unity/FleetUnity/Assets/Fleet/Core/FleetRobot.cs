// One robot's side of the fleet protocol (docs/ROBOT_PROTOCOL.md): what
// fleet_adapter.py does for a ROS robot, done by the robot itself. Pure: the
// caller passes the clocks in and sends the returned messages in order.
//   now  - monotonic seconds, for the publish policies
//   wall - unix seconds, for "stamp" and expires_at
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
        public string Frame = "map";
        // false sends "battery": null; fleetd's battery policy then skips the robot.
        public bool ReportBattery = true;
        public Action<string> Log = _ => { };

        readonly CommandBook book = new CommandBook();
        string activeGoto;             // cmd_id of the goto being driven, reported as task_id
        ulong poseSeq, stateSeq;
        PoseSent? lastPose;
        StateSent? lastState;

        public FleetRobot(string name, string host, RobotSim sim, string boot = null)
        {
            Name = name; Host = host; Sim = sim; Boot = boot ?? Ids.NewBoot();
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

            switch (kind)
            {
                case "goto": DoGoto(cmd, cmdId, outq); break;
                case "cancel": DoCancel(cmd, cmdId, outq); break;
                case "dock": DoDock(cmdId, outq); break;
                case "reset_error":
                    var why = Sim.ResetError();
                    Ack(outq, cmdId, why == null ? "accepted" : "failed", why ?? "reset");
                    break;
                default:
                    // Includes "ride": this robot sends can_ride:false and cannot ride.
                    Ack(outq, cmdId, "failed", $"unknown command type '{kind}'");
                    break;
            }
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

        void Ack(List<Outgoing> outq, string cmdId, string status, string message)
        {
            var body = new JObject { ["cmd_id"] = cmdId, ["status"] = status, ["message"] = message };
            var json = body.ToString(Formatting.None);
            book.Record(cmdId, json);
            outq.Add(new Outgoing(Topics.CmdAck(Name), json, 1));
            Log($"{Name}: {cmdId} {status} {message}");
        }

        // ---------------------------------------------------------------- simulation + publishing

        public void Tick(double dt, double now, double wall, List<Outgoing> outq)
        {
            Sim.Step(dt);
            if (activeGoto != null)
            {
                if (Sim.Mode == Modes.Error)
                    EndGoto(outq, "failed", $"robot in ERROR: {Sim.Error}");
                else if (!Sim.HasGoal && Sim.Mode == Modes.Idle)
                    EndGoto(outq, "done", "");
            }
            PublishState(now, wall, outq);
            PublishPose(now, wall, outq);
        }

        void PublishState(double now, double wall, List<Outgoing> outq)
        {
            // Same key as the adapter: a change of mode, error or task_id goes out at once.
            var key = $"{Sim.Mode}|{Sim.Error}|{TaskId}|0";
            if (!Throttle.StateDue(lastState, now, key))
                return;
            stateSeq++;
            lastState = new StateSent(now, key);
            var s = new JObject
            {
                ["boot"] = Boot, ["seq"] = stateSeq, ["stamp"] = Math.Round(wall, 3), ["host"] = Host,
                ["mode"] = Sim.Mode,
                ["battery"] = ReportBattery ? (JToken)Math.Round(Sim.Battery, 1) : JValue.CreateNull(),
                ["task_id"] = TaskId, ["error"] = Sim.Error, ["silence_s"] = 0,
                ["can_ride"] = false,
            };
            outq.Add(new Outgoing(Topics.State(Name), s.ToString(Formatting.None), 1));
        }

        void PublishPose(double now, double wall, List<Outgoing> outq)
        {
            if (!Throttle.PoseDueRated(lastPose, now, Sim.X, Sim.Y, Sim.Yaw))
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

        static string Trunc(string s) => s.Length <= 120 ? s : s.Substring(0, 120);
    }
}
