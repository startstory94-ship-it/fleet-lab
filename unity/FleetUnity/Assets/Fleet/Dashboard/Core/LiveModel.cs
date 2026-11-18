// The /ws/live client state, ported from web/app.js (the 2D dashboard is the
// reference): frames (snapshot / delta / heartbeat), the server clock offset,
// per-robot tracks and the interpolated pose drawn RenderDelayMs behind the
// server clock. Frame format: internal/live/live.go. No UnityEngine here, so
// it runs under EditMode tests; times are milliseconds on the local wall clock
// (Date.now() in app.js), passed in by the caller.
using System;
using System.Collections.Generic;
using System.Globalization;
using Newtonsoft.Json;

namespace Fleet.Dashboard
{
    /// <summary>One robot as /ws/live shows it (live.RobotView).</summary>
    public sealed class RobotView
    {
        [JsonProperty("name")] public string Name = "";
        [JsonProperty("host")] public string Host = "";
        [JsonProperty("conn")] public string Conn = "";
        [JsonProperty("floor")] public int Floor;
        [JsonProperty("x")] public double X;
        [JsonProperty("y")] public double Y;
        [JsonProperty("yaw")] public double Yaw;
        [JsonProperty("v")] public double V;
        [JsonProperty("has_pose")] public bool HasPose;
        [JsonProperty("mode")] public string Mode = "";
        [JsonProperty("battery")] public double Battery;
        [JsonProperty("battery_known")] public bool BatteryKnown;
        [JsonProperty("needs_charge")] public bool NeedsCharge;
        [JsonProperty("task_id")] public string TaskId = "";
        [JsonProperty("error")] public string Error = "";
    }

    /// <summary>One message on /ws/live (live.Frame).</summary>
    public sealed class LiveFrame
    {
        public const string Snapshot = "snapshot", Delta = "delta", Heartbeat = "heartbeat";
        [JsonProperty("type")] public string Type = "";
        [JsonProperty("seq")] public ulong Seq;
        [JsonProperty("time")] public string Time = "";
        [JsonProperty("robots")] public List<RobotView> Robots = new List<RobotView>();
        [JsonProperty("removed")] public List<string> Removed = new List<string>();
    }

    /// <summary>A position sample {t, x, y, yaw, floor} of a track (app.js 29-32).</summary>
    public struct Sample
    {
        public double T, X, Y, Yaw;
        public int Floor;
        public Sample(double t, double x, double y, double yaw, int floor) { T = t; X = x; Y = y; Yaw = yaw; Floor = floor; }
    }

    public sealed class TrackedRobot
    {
        public RobotView V;
        public readonly List<Sample> Track = new List<Sample>();
    }

    public sealed class LiveModel
    {
        // app.js 16-18.
        public const double RenderDelayMs = 350;   // two frames and a bit: there is almost always a sample ahead
        public const double ExtrapolateMs = 250;   // past the newest sample, keep moving with v/yaw this long at most
        public const double LiveSilenceMs = 5000;  // the server sends a heartbeat every 2 s; silence longer = dead
        // applyRobot's constants (app.js 363-370).
        const double HoldGapMs = 400, HoldLeadMs = 150, KeepMs = 3000;

        // app.js 25.
        public static readonly HashSet<string> MovingModes = new HashSet<string> { "MOVING", "DOCKING", "BOARDING", "EXITING" };

        public readonly Dictionary<string, TrackedRobot> Robots = new Dictionary<string, TrackedRobot>();
        public double? ClockOffset;   // server clock - local clock, ms
        public ulong Seq;             // state.liveSeq
        public int Gaps;              // deltas that did not follow the previous seq (app.js only warns)
        public int Snapshots, Deltas, Heartbeats;
        public Action<string> Log = _ => { };

        static readonly JsonSerializerSettings Json = new JsonSerializerSettings
        {
            DateParseHandling = DateParseHandling.None, // Go sends 9 fractional digits; parsed by UnixMs
            MissingMemberHandling = MissingMemberHandling.Ignore,
        };

        public static LiveFrame Parse(string json)
        {
            var f = JsonConvert.DeserializeObject<LiveFrame>(json, Json);
            if (f.Robots == null) f.Robots = new List<RobotView>();
            if (f.Removed == null) f.Removed = new List<string>();
            return f;
        }

        /// <summary>onFrame (app.js 327-350) without the DOM parts.</summary>
        public void OnFrame(LiveFrame f, double localNowMs)
        {
            double t = UnixMs(f.Time);
            if (double.IsNaN(t)) t = localNowMs + (ClockOffset ?? 0); // a NaN would stick in Math.Max forever
            // The smallest delay seen is the best guess of the clock offset; let it
            // drift 1 ms per frame so a changed offset is picked up eventually.
            double off = t - localNowMs;
            ClockOffset = ClockOffset == null ? off : Math.Max(off, ClockOffset.Value - 1);
            if (f.Type == LiveFrame.Heartbeat)
            {
                Heartbeats++;
                return; // only says the feed is alive
            }

            if (f.Type == LiveFrame.Snapshot)
            {
                Snapshots++;
                var names = new HashSet<string>();
                foreach (var r in f.Robots) names.Add(r.Name);
                var gone = new List<string>();
                foreach (var name in Robots.Keys) if (!names.Contains(name)) gone.Add(name);
                foreach (var name in gone) Robots.Remove(name);
            }
            else
            {
                Deltas++;
                if (f.Seq != Seq + 1)
                {
                    // app.js 338-340 warns and applies anyway: live.go's Sub.accept
                    // already drops a delta over a gap and queues a snapshot.
                    Gaps++;
                    Log($"live frame gap {Seq} -> {f.Seq}");
                }
            }
            Seq = f.Seq;
            foreach (var v in f.Robots) ApplyRobot(v, t, localNowMs);
            foreach (var name in f.Removed) Robots.Remove(name);
        }

        /// <summary>applyRobot (app.js 352-371).</summary>
        void ApplyRobot(RobotView v, double t, double localNowMs)
        {
            if (!Robots.TryGetValue(v.Name, out var r))
                Robots[v.Name] = r = new TrackedRobot();
            var prev = r.V;
            r.V = v;
            if (!v.HasPose) return;
            var track = r.Track;
            if (track.Count > 0)
            {
                var last = track[track.Count - 1];
                if (last.Floor != v.Floor)
                    track.Clear(); // changed floors: jump
                else if (last.X == v.X && last.Y == v.Y && last.Yaw == v.Yaw &&
                         prev != null && prev.V == v.V && prev.Mode == v.Mode)
                    return; // not a move
                else if (t - last.T > HoldGapMs)
                {
                    // It stood still since `last`: hold there until just before this
                    // sample, instead of crawling across the whole pause.
                    last.T = t - HoldLeadMs;
                    track.Add(last);
                }
            }
            track.Add(new Sample(t, v.X, v.Y, v.Yaw, v.Floor));
            double keepFrom = RenderTime(localNowMs) - KeepMs;
            while (track.Count > 2 && track[1].T < keepFrom) track.RemoveAt(0);
        }

        /// <summary>renderTime (app.js 373-375).</summary>
        public double RenderTime(double localNowMs) => localNowMs + (ClockOffset ?? 0) - RenderDelayMs;

        /// <summary>
        /// poseAt (app.js 29-52): position at time t (server clock) interpolated
        /// between samples; past the newest one a moving robot goes on along its
        /// heading for at most ExtrapolateMs, then holds. null without samples.
        /// </summary>
        public static Sample? PoseAt(IReadOnlyList<Sample> track, double t, RobotView motion)
        {
            if (track.Count == 0) return null;
            if (t <= track[0].T) return track[0];
            var last = track[track.Count - 1];
            if (t >= last.T)
            {
                if (motion == null || !MovingModes.Contains(motion.Mode) || !(Math.Abs(motion.V) > 0.01)) return last;
                double dt = Math.Min(t - last.T, ExtrapolateMs) / 1000;
                return new Sample(t, last.X + motion.V * Math.Cos(last.Yaw) * dt, last.Y + motion.V * Math.Sin(last.Yaw) * dt, last.Yaw, last.Floor);
            }
            for (int i = track.Count - 1; i > 0; i--)
            {
                var a = track[i - 1];
                var b = track[i];
                if (t >= a.T && t <= b.T)
                {
                    double k = b.T > a.T ? (t - a.T) / (b.T - a.T) : 1;
                    double dyaw = b.Yaw - a.Yaw;
                    dyaw = Math.Atan2(Math.Sin(dyaw), Math.Cos(dyaw)); // shortest turn
                    return new Sample(t, a.X + (b.X - a.X) * k, a.Y + (b.Y - a.Y) * k, a.Yaw + dyaw * k, b.Floor);
                }
            }
            return last;
        }

        /// <summary>
        /// Date.parse for Go's RFC 3339 times (time.Time's JSON): up to nine
        /// fractional digits, which DateTimeOffset.Parse refuses beyond seven.
        /// Returns unix milliseconds, NaN if unparsable.
        /// </summary>
        public static double UnixMs(string s)
        {
            if (string.IsNullOrEmpty(s)) return double.NaN;
            double frac = 0;
            int dot = s.IndexOf('.', Math.Max(0, s.IndexOf('T')));
            if (dot > 0)
            {
                int end = dot + 1;
                while (end < s.Length && char.IsDigit(s[end])) end++;
                if (end > dot + 1)
                    frac = double.Parse("0" + s.Substring(dot, end - dot), CultureInfo.InvariantCulture);
                s = s.Substring(0, dot) + s.Substring(end);
            }
            if (!DateTimeOffset.TryParse(s, CultureInfo.InvariantCulture, DateTimeStyles.AssumeUniversal, out var d))
                return double.NaN;
            return d.ToUnixTimeMilliseconds() + frac * 1000;
        }
    }
}
