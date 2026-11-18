// Publish policies of the adapter (fleet_logic.pose_due/state_due plus the
// adapter's 5 Hz pose timer). Times are monotonic seconds.
using System;

namespace Fleet.Core
{
    public struct PoseSent
    {
        public double T, X, Y, Yaw;
        public PoseSent(double t, double x, double y, double yaw) { T = t; X = x; Y = y; Yaw = yaw; }
    }

    public struct StateSent
    {
        public double T;
        public string Key;
        public StateSent(double t, string key) { T = t; Key = key; }
    }

    public static class Throttle
    {
        public static double AngleDiff(double a, double b) => Math.Abs(Math.Atan2(Math.Sin(a - b), Math.Cos(a - b)));

        /// <summary>fleet_logic.pose_due: moved 5 cm or turned 2 degrees, else a 1 s heartbeat.</summary>
        public static bool PoseDue(PoseSent? last, double now, double x, double y, double yaw)
        {
            if (!last.HasValue)
                return true;
            var l = last.Value;
            bool moved = Hypot(x - l.X, y - l.Y) >= Protocol.PoseDeadbandM ||
                         AngleDiff(yaw, l.Yaw) >= Protocol.PoseDeadbandRad;
            return moved || now - l.T >= Protocol.PoseIdlePeriod;
        }

        /// <summary>PoseDue capped at PoseMaxHz. The adapter gets the cap from its
        /// 0.2 s timer; a Unity frame loop runs faster, so it is explicit here.
        /// The 1 ms slack keeps frame jitter from skipping a whole period.</summary>
        public static bool PoseDueRated(PoseSent? last, double now, double x, double y, double yaw)
        {
            if (last.HasValue && now - last.Value.T < 1.0 / Protocol.PoseMaxHz - 0.001)
                return false;
            return PoseDue(last, now, x, y, yaw);
        }

        /// <summary>fleet_logic.state_due: at once when the key changes, else every second.</summary>
        public static bool StateDue(StateSent? last, double now, string key)
        {
            if (!last.HasValue)
                return true;
            return key != last.Value.Key || now - last.Value.T >= Protocol.StatePeriod;
        }

        public static double Hypot(double dx, double dy) => Math.Sqrt(dx * dx + dy * dy);
    }
}
