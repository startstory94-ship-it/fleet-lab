// Protocol constants for robots managed by fleetd (docs/ROBOT_PROTOCOL.md).
// Values follow adapter/fleet_logic.py and mock_robots/mock_fleet.py so a
// Unity robot looks to fleetd exactly like a bridged ROS robot.
using System;

namespace Fleet.Core
{
    public static class Protocol
    {
        public const double PoseMaxHz = 5.0;          // fleet_logic.POSE_MAX_HZ (adapter's pose timer)
        public const double PoseIdlePeriod = 1.0;     // s, pose heartbeat while still
        public const double PoseDeadbandM = 0.05;
        public static readonly double PoseDeadbandRad = 2.0 * Math.PI / 180.0;
        public const double StatePeriod = 1.0;        // s, state heartbeat
        public const int KeepAliveS = 5;              // fleet_adapter.KEEPALIVE
        public const int CommandMemory = 256;         // CommandBook size
    }

    public static class Modes
    {
        public const string Idle = "IDLE";
        public const string Moving = "MOVING";
        public const string Docking = "DOCKING";
        public const string Charging = "CHARGING";
        public const string Error = "ERROR";
        // Ride modes are reported by RobotRide's overlay, not by RobotSim.
        public const string WaitingElevator = "WAITING_ELEVATOR";
        public const string Boarding = "BOARDING";
        public const string Riding = "RIDING";
        public const string Exiting = "EXITING";
    }

    public static class Topics
    {
        public static string Pose(string robot) => $"fleet/{robot}/pose";
        public static string State(string robot) => $"fleet/{robot}/state";
        public static string Cmd(string robot) => $"fleet/{robot}/cmd";
        public static string CmdAck(string robot) => $"fleet/{robot}/cmd_ack";
        public static string AdapterStatus(string host) => $"fleet/adapter/{host}/status";
        public static string Event(string robot) => $"fleet/{robot}/event";
        public static string Zone(string car) => $"elevator/{car}/zone";

        /// <summary>What a riding robot follows (docs/ELEVATOR.md 2), subscribed with
        /// a wildcard so one connection serves every robot and every car.</summary>
        public static readonly string[] ElevatorFollow = { "elevator/+/turn", "elevator/+/state", "elevator/+/trip" };

        /// <summary>Splits elevator/&lt;car&gt;/&lt;kind&gt; (kind turn, state or trip). False for anything else.</summary>
        public static bool ParseElevator(string topic, out string car, out string kind)
        {
            car = kind = null;
            var p = topic.Split('/');
            if (p.Length != 3 || p[0] != "elevator" || p[1] == "") return false;
            if (p[2] != "turn" && p[2] != "state" && p[2] != "trip") return false;
            car = p[1];
            kind = p[2];
            return true;
        }

        /// <summary>The body of an elevator message as fleet_adapter.handle_elevator reads it:
        /// an empty trip payload is a cleared retained trip (ok, msg null); a payload
        /// that is not a JSON object is ignored (false).</summary>
        public static bool ElevatorBody(string kind, string payload, out Newtonsoft.Json.Linq.JObject msg)
        {
            msg = null;
            if (kind == "trip" && string.IsNullOrEmpty(payload)) return true;
            try
            {
                msg = Newtonsoft.Json.Linq.JToken.Parse(payload) as Newtonsoft.Json.Linq.JObject;
            }
            catch (Newtonsoft.Json.JsonException) { return false; }
            return msg != null;
        }

        /// <summary>The robot of a fleet/&lt;robot&gt;/cmd topic, or null.</summary>
        public static string RobotOfCmd(string topic)
        {
            var p = topic.Split('/');
            return p.Length == 3 && p[0] == "fleet" && p[2] == "cmd" && p[1] != "adapter" ? p[1] : null;
        }
    }

    /// <summary>One MQTT publish the Unity layer must send, in order.</summary>
    public readonly struct Outgoing
    {
        public readonly string Topic;
        public readonly string Payload;
        public readonly int Qos;
        public readonly bool Retain;

        public Outgoing(string topic, string payload, int qos, bool retain = false)
        {
            Topic = topic; Payload = payload; Qos = qos; Retain = retain;
        }

        public override string ToString() => $"{Topic} q{Qos}{(Retain ? " r" : "")} {Payload}";
    }

    public static class Ids
    {
        /// <summary>8 hex chars, like the adapter's uuid4().hex[:8]: new every process start.</summary>
        public static string NewBoot() => Guid.NewGuid().ToString("N").Substring(0, 8);
    }
}
