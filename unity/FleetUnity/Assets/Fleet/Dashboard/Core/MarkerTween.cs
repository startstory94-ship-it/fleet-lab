// Display smoothing for one robot marker in the 3D dashboard. A marker is
// drawn from one source at a time: the robot's interpolated track on a floor,
// or a slot of the car it rides. When the source changes (it boards, it
// leaves the car, or its track starts over on another floor) the target
// jumps; instead of snapping, the marker eases from where it was last drawn
// to the (still moving) new target over DurationMs. A robot drawn for the
// first time snaps. This changes only what is drawn, never what the robot
// reported.
using System;

namespace Fleet.Dashboard
{
    public struct V3
    {
        public double X, Y, Z;
        public V3(double x, double y, double z) { X = x; Y = y; Z = z; }
        public static V3 Lerp(V3 a, V3 b, double k) => new V3(a.X + (b.X - a.X) * k, a.Y + (b.Y - a.Y) * k, a.Z + (b.Z - a.Z) * k);
        public override string ToString() => $"({X:F3}, {Y:F3}, {Z:F3})";
    }

    public sealed class MarkerTween
    {
        public const double DurationMs = 600;

        bool drawn;
        string key;            // the current source, e.g. "t3" (track on floor 3) or "cE1" (in car E1)
        V3 last;               // last drawn position
        V3 from;               // where the running tween started
        double start = double.NaN;

        public bool Tweening { get; private set; }

        /// <summary>Ease-out cubic: fast start, gentle arrival.</summary>
        public static double EaseOut(double k)
        {
            k = Math.Max(0, Math.Min(1, k));
            double u = 1 - k;
            return 1 - u * u * u;
        }

        /// <summary>The position to draw now. sourceKey names where target comes
        /// from; when it differs from the last call's, a tween starts from the
        /// last drawn position (also mid-tween). Otherwise target is drawn as is.</summary>
        public V3 Step(double nowMs, string sourceKey, V3 target)
        {
            if (!drawn)
            {
                drawn = true;
                key = sourceKey;
                last = target;
                return target;
            }
            if (sourceKey != key)
            {
                key = sourceKey;
                from = last;
                start = nowMs;
                Tweening = true;
            }
            V3 p = target;
            if (Tweening)
            {
                double k = (nowMs - start) / DurationMs;
                if (k >= 1) Tweening = false;
                else p = V3.Lerp(from, target, EaseOut(k));
            }
            last = p;
            return p;
        }
    }
}
