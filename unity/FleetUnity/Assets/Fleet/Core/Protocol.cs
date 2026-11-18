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
    }

    public static class Topics
    {
        public static string Pose(string robot) => $"fleet/{robot}/pose";
        public static string State(string robot) => $"fleet/{robot}/state";
        public static string Cmd(string robot) => $"fleet/{robot}/cmd";
        public static string CmdAck(string robot) => $"fleet/{robot}/cmd_ack";
        public static string AdapterStatus(string host) => $"fleet/adapter/{host}/status";

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
