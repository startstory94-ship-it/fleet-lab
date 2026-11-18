// Display smoothing for one robot marker in the 3D dashboard. A marker is
// drawn from one source at a time: the robot's interpolated track on a floor,
// or a slot of the car it rides. When the source changes (it boards, it
// leaves the car, or its track starts over on another floor) the target
// jumps; instead of snapping, the marker eases from where it was last drawn
// to the (still moving) new target over DurationMs. A robot drawn for the
// first time snaps. This changes only what is drawn, never what the robot
// reported.
// Leaving a car has one more catch: the REST poll says "out" before the robot's
// first pose after the ride (up to a pose period later), and until then its track
// still ends on the boarding floor. HoldAfterCar keeps the marker where it was
// drawn until that pose arrives (or a timeout), then the normal tween takes over.
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
        public const double HoldTimeoutMs = 5000;   // after this the track is used even without a new pose

        bool drawn;
        string key;            // the current source, e.g. "t3" (track on floor 3) or "cE1" (in car E1)
        V3 last;               // last drawn position
        V3 from;               // where the running tween started
        double start = double.NaN;
        bool holding, released;   // released: this car exit's hold is over, until the next Step
        double holdSince, leftT;

        public bool Tweening { get; private set; }
        public bool Holding => holding;
        /// <summary>The last drawn position (what a held marker keeps showing).</summary>
        public V3 Last => last;

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
            holding = released = false;
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

        /// <summary>Call before drawing a robot from its track (not from a car). True: draw
        /// Last instead and do not call Step. A robot whose last drawn source was a car
        /// ("c...") is held from the moment it is drawn from its track until the track's
        /// newest sample is newer than serverNowMs at that moment (same clock as the samples),
        /// or HoldTimeoutMs of nowMs have passed. newestSampleT is NaN without samples.</summary>
        public bool HoldAfterCar(double nowMs, double serverNowMs, double newestSampleT)
        {
            if (!drawn || released) return false;
            if (!holding)
            {
                if (key == null || key.Length == 0 || key[0] != 'c') return false;
                holding = true;
                holdSince = nowMs;
                leftT = serverNowMs;
            }
            if (newestSampleT > leftT || nowMs - holdSince >= HoldTimeoutMs)
            {
                holding = false;
                released = true;
                return false;
            }
            return true;
        }
    }
}
