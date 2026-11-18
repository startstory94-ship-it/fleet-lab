// Smaller pure parts of web/app.js the 3D view shares: robot colour by
// mode/connection, grid lines, map bounds that only grow, and the reconnect
// back-off of connect(). (Its 5 s silence watch is in the transport: a
// receive cancelled after LiveModel.LiveSilenceMs.)
using System;
using System.Collections.Generic;

namespace Fleet.Dashboard
{
    /// <summary>The 2D legend's colours (web/style.css), as 0xRRGGBB.</summary>
    public enum Look { Idle, Moving, Charging, Error, Elevator, Offline }

    public static class RobotStyle
    {
        public const int IdleRgb = 0x8a919e, MovingRgb = 0x2f6fde, ChargingRgb = 0x23a05a,
            ErrorRgb = 0xd93b3b, ElevatorRgb = 0x8a4fd6, OfflineRgb = 0x3a3f4a, TaskRgb = 0xe08a1e;

        public static int Rgb(Look l)
        {
            switch (l)
            {
                case Look.Moving: return MovingRgb;
                case Look.Charging: return ChargingRgb;
                case Look.Error: return ErrorRgb;
                case Look.Elevator: return ElevatorRgb;
                case Look.Offline: return OfflineRgb;
                default: return IdleRgb;
            }
        }

        /// <summary>robotColor (app.js 574-583), same precedence. STALE is drawn
        /// idle-coloured, faded and dashed (drawRobot, app.js 753-759).</summary>
        public static Look Of(string conn, string mode, string error)
        {
            if (conn == "OFFLINE") return Look.Offline;
            if (conn == "IN_ELEVATOR") return Look.Elevator;
            if (conn == "STALE") return Look.Idle;
            if (mode == "ERROR" || !string.IsNullOrEmpty(error)) return Look.Error;
            if (mode == "MOVING") return Look.Moving;
            if (mode == "DOCKING" || mode == "CHARGING") return Look.Charging;
            if (mode == "WAITING_ELEVATOR" || mode == "BOARDING" || mode == "RIDING" || mode == "EXITING") return Look.Elevator;
            return Look.Idle;
        }

        public static Look Of(RobotView v) => Of(v.Conn, v.Mode, v.Error);
    }

    public static class MapGrid
    {
        public const int MaxLines = 200; // app.js 20

        /// <summary>gridLines (app.js 54-70): lines between lo and hi, the step
        /// growing 1-5-10-50... until there are at most maxLines.</summary>
        public static List<(double v, bool major)> Lines(double lo, double hi, double minStep, int maxLines = MaxLines)
        {
            var lines = new List<(double, bool)>();
            if (double.IsNaN(lo) || double.IsNaN(hi) || double.IsInfinity(lo) || double.IsInfinity(hi) || hi < lo) return lines;
            double step = minStep;
            while ((hi - lo) / step > maxLines) step *= step.ToString(System.Globalization.CultureInfo.InvariantCulture)[0] == '1' ? 5 : 2;
            // Count with k: far from the origin i + 1 can equal i in floating point.
            double first = Math.Ceiling(lo / step);
            double n = Math.Min(Math.Floor(hi / step) - first, maxLines + 1);
            for (int k = 0; k <= n; k++)
            {
                double i = first + k;
                if (lines.Count > 0 && lines[lines.Count - 1].Item1 == i * step) continue;
                lines.Add((i * step, i % 5 == 0));
            }
            return lines;
        }
    }

    /// <summary>
    /// Map bounds over every floor (updateBounds, app.js 654-671): they only
    /// grow, so slabs do not jump around as robots move, padded by 2 m and at
    /// least 10 m wide. One box for all floors keeps the stack aligned.
    /// </summary>
    public sealed class Bounds2
    {
        const double Pad = 2, MinSpan = 10, MaxCoord = 1e6; // app.js 19, 657
        public double MinX = double.PositiveInfinity, MinY = double.PositiveInfinity,
            MaxX = double.NegativeInfinity, MaxY = double.NegativeInfinity;
        public int Version; // bumped when the box grows

        public void Add(double x, double y)
        {
            // inRange (app.js 72): one bad value must not stretch the map.
            if (double.IsNaN(x) || double.IsNaN(y) || Math.Abs(x) > MaxCoord || Math.Abs(y) > MaxCoord) return;
            bool grew = false;
            if (x - Pad < MinX) { MinX = x - Pad; grew = true; }
            if (x + Pad > MaxX) { MaxX = x + Pad; grew = true; }
            if (y - Pad < MinY) { MinY = y - Pad; grew = true; }
            if (y + Pad > MaxY) { MaxY = y + Pad; grew = true; }
            if (grew) Version++;
        }

        /// <summary>The box to draw: at least MinSpan per axis, centred.</summary>
        public (double minX, double minY, double maxX, double maxY) Box()
        {
            if (double.IsInfinity(MinX)) return (-MinSpan / 2, -MinSpan / 2, MinSpan / 2, MinSpan / 2);
            double ex = Math.Max(0, MinSpan - (MaxX - MinX)) / 2, ey = Math.Max(0, MinSpan - (MaxY - MinY)) / 2;
            return (MinX - ex, MinY - ey, MaxX + ex, MaxY + ey);
        }
    }

    /// <summary>connect()'s reconnect delay (app.js 279, 284, 298-299): 0.5 s
    /// doubling to 10 s, with +-25 % jitter, reset once a connection opens.</summary>
    public sealed class Backoff
    {
        public const double FirstMs = 500, MaxMs = 10000;
        double delay = FirstMs;

        public void Reset() => delay = FirstMs;

        /// <summary>The wait before the next attempt; rand01 in [0, 1).</summary>
        public double Next(double rand01)
        {
            double d = delay * (0.75 + rand01 * 0.5);
            delay = Math.Min(delay * 2, MaxMs);
            return d;
        }
    }
}
