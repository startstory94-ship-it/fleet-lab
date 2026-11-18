// The 3D dashboard's feed client against web/app.js: frames (snapshot,
// delta, removed, gap, heartbeat), the clock offset, applyRobot's track rules
// and poseAt. The poseAt cases are scripts/web_test.js 14-58, ported.
using System;
using System.Collections.Generic;
using Fleet.Dashboard;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class LiveModelTests
    {
        const double T0 = 1_800_000_000_000; // ms
        LiveModel m;

        [SetUp]
        public void SetUp() => m = new LiveModel();

        static string Iso(double ms) =>
            DateTimeOffset.FromUnixTimeMilliseconds((long)ms).ToString("yyyy-MM-dd'T'HH:mm:ss.fff'Z'");

        static RobotView R(string name, double x, double y = 0, string mode = "IDLE", double v = 0, int floor = 1, string conn = "ONLINE") =>
            new RobotView { Name = name, Conn = conn, Floor = floor, X = x, Y = y, V = v, HasPose = true, Mode = mode };

        static LiveFrame F(string type, ulong seq, double t, params RobotView[] robots) =>
            new LiveFrame { Type = type, Seq = seq, Time = Iso(t), Robots = new List<RobotView>(robots) };

        // ---- frames ----

        [Test]
        public void SnapshotReplacesWhatIsHeld()
        {
            m.OnFrame(F("snapshot", 5, T0, R("a", 1), R("b", 2)), T0);
            m.OnFrame(F("snapshot", 9, T0 + 100, R("b", 3), R("c", 4)), T0 + 100);
            CollectionAssert.AreEquivalent(new[] { "b", "c" }, m.Robots.Keys);
            Assert.AreEqual(9UL, m.Seq);
            Assert.AreEqual(3, m.Robots["b"].V.X);
            Assert.AreEqual(0, m.Gaps, "a snapshot never counts as a gap");
        }

        [Test]
        public void DeltaAppliesOnlyChangedRobots()
        {
            m.OnFrame(F("snapshot", 5, T0, R("a", 1), R("b", 2)), T0);
            m.OnFrame(F("delta", 6, T0 + 150, R("a", 1.5, mode: "MOVING", v: 1)), T0 + 150);
            Assert.AreEqual(6UL, m.Seq);
            Assert.AreEqual(1.5, m.Robots["a"].V.X);
            Assert.AreEqual("MOVING", m.Robots["a"].V.Mode);
            Assert.AreEqual(2, m.Robots["b"].V.X);
            Assert.AreEqual(2, m.Robots["a"].Track.Count);
        }

        [Test]
        public void RemovedRobotsAreDeleted()
        {
            m.OnFrame(F("snapshot", 1, T0, R("a", 1), R("b", 2)), T0);
            var d = F("delta", 2, T0 + 150);
            d.Removed.Add("a");
            m.OnFrame(d, T0 + 150);
            CollectionAssert.AreEquivalent(new[] { "b" }, m.Robots.Keys);
        }

        [Test]
        public void GapIsCountedButTheDeltaStillApplies()
        {
            // app.js 338-341: the server prevents gaps; the client only warns.
            var logged = new List<string>();
            m.Log = logged.Add;
            m.OnFrame(F("snapshot", 1, T0, R("a", 1)), T0);
            m.OnFrame(F("delta", 4, T0 + 150, R("a", 2)), T0 + 150);
            Assert.AreEqual(1, m.Gaps);
            Assert.AreEqual(4UL, m.Seq);
            Assert.AreEqual(2, m.Robots["a"].V.X);
            Assert.AreEqual(1, logged.Count);
            // The next delta follows the gapped one: no new gap. A snapshot resyncs.
            m.OnFrame(F("delta", 5, T0 + 300, R("a", 3)), T0 + 300);
            m.OnFrame(F("snapshot", 9, T0 + 450, R("a", 4)), T0 + 450);
            m.OnFrame(F("delta", 10, T0 + 600, R("a", 5)), T0 + 600);
            Assert.AreEqual(1, m.Gaps);
        }

        [Test]
        public void HeartbeatOnlyMovesTheClockOffset()
        {
            m.OnFrame(F("snapshot", 7, T0, R("a", 1)), T0 + 40); // server 40 ms behind us
            Assert.AreEqual(-40, m.ClockOffset.Value, 1e-6);
            m.OnFrame(F("heartbeat", 7, T0 + 2000), T0 + 2010);  // smaller delay: offset -10
            Assert.AreEqual(-10, m.ClockOffset.Value, 1e-6);
            Assert.AreEqual(7UL, m.Seq);
            Assert.AreEqual(1, m.Heartbeats);
            Assert.AreEqual(1, m.Robots["a"].Track.Count);
            Assert.AreEqual(0, m.Gaps);
        }

        [Test]
        public void ClockOffsetKeepsTheSmallestDelayAndDriftsOneMsPerFrame()
        {
            m.OnFrame(F("heartbeat", 0, T0), T0 + 10);       // -10
            m.OnFrame(F("heartbeat", 0, T0 + 1000), T0 + 1300); // a late frame: -300 < -11, so drift to -11
            Assert.AreEqual(-11, m.ClockOffset.Value, 1e-6);
            m.OnFrame(F("heartbeat", 0, T0 + 2000), T0 + 1990); // server ahead: +10 wins
            Assert.AreEqual(10, m.ClockOffset.Value, 1e-6);
            Assert.AreEqual(T0 + 5000 + 10 - LiveModel.RenderDelayMs, m.RenderTime(T0 + 5000), 1e-6);
        }

        [Test]
        public void ParsesARealFrame()
        {
            var json = "{\"type\":\"delta\",\"seq\":42,\"time\":\"2026-11-18T17:16:42.123456789+09:00\"," +
                       "\"robots\":[{\"name\":\"robot_1\",\"host\":\"ros-pc\",\"conn\":\"ONLINE\",\"floor\":2,\"x\":1.5,\"y\":-3,\"yaw\":0.5," +
                       "\"v\":0.8,\"has_pose\":true,\"mode\":\"MOVING\",\"battery\":81.5,\"battery_known\":true,\"needs_charge\":false," +
                       "\"task_id\":\"t-1\",\"error\":\"\",\"can_ride\":true}],\"removed\":[\"old\"]}";
            var f = LiveModel.Parse(json);
            Assert.AreEqual("delta", f.Type);
            Assert.AreEqual(42UL, f.Seq);
            Assert.AreEqual("robot_1", f.Robots[0].Name);
            Assert.AreEqual(2, f.Robots[0].Floor);
            Assert.AreEqual(0.8, f.Robots[0].V);
            CollectionAssert.AreEqual(new[] { "old" }, f.Removed);
            var hb = LiveModel.Parse("{\"type\":\"heartbeat\",\"seq\":3,\"time\":\"2026-11-18T08:16:42Z\",\"robots\":[]}");
            Assert.AreEqual(0, hb.Robots.Count);
            Assert.AreEqual(0, hb.Removed.Count, "removed is omitempty");
        }

        [Test]
        public void GoTimesWithNineFractionalDigitsParseToTheMillisecond()
        {
            double want = DateTimeOffset.Parse("2026-11-18T08:16:42Z").ToUnixTimeMilliseconds() + 123.456789;
            Assert.AreEqual(want, LiveModel.UnixMs("2026-11-18T17:16:42.123456789+09:00"), 1e-3);
            Assert.AreEqual(want - 123.456789, LiveModel.UnixMs("2026-11-18T08:16:42Z"), 1e-6);
            Assert.AreEqual(want - 23.456789, LiveModel.UnixMs("2026-11-18T08:16:42.1Z"), 1e-6);
            Assert.IsNaN(LiveModel.UnixMs("not a time"));
            Assert.IsNaN(LiveModel.UnixMs(""));
        }

        // ---- applyRobot's track rules (app.js 352-371) ----

        [Test]
        public void AStandstillAddsAHoldSampleJustBeforeTheNextMove()
        {
            m.OnFrame(F("snapshot", 1, T0, R("a", 0)), T0);
            m.OnFrame(F("delta", 2, T0 + 1000, R("a", 1, mode: "MOVING", v: 1)), T0 + 1000);
            var tr = m.Robots["a"].Track;
            Assert.AreEqual(3, tr.Count);
            Assert.AreEqual(T0 + 850, tr[1].T, 1e-6); // held at x 0 until 150 ms before
            Assert.AreEqual(0, tr[1].X);
            Assert.AreEqual(1, tr[2].X);
        }

        [Test]
        public void FloorChangeStartsANewTrack()
        {
            m.OnFrame(F("snapshot", 1, T0, R("a", 0), R("b", 0)), T0);
            m.OnFrame(F("delta", 2, T0 + 150, R("a", 1, floor: 1)), T0 + 150);
            m.OnFrame(F("delta", 3, T0 + 300, R("a", 5, floor: 3)), T0 + 300);
            var tr = m.Robots["a"].Track;
            Assert.AreEqual(1, tr.Count);
            Assert.AreEqual(3, tr[0].Floor);
        }

        [Test]
        public void SameViewIsNotAMoveButAModeChangeIs()
        {
            m.OnFrame(F("snapshot", 1, T0, R("a", 0)), T0);
            m.OnFrame(F("delta", 2, T0 + 150, R("a", 0)), T0 + 150); // battery-only change, say
            Assert.AreEqual(1, m.Robots["a"].Track.Count);
            m.OnFrame(F("delta", 3, T0 + 300, R("a", 0, mode: "CHARGING")), T0 + 300);
            Assert.AreEqual(2, m.Robots["a"].Track.Count);
        }

        [Test]
        public void TrackIsTrimmedToThreeSecondsBehindRenderTime()
        {
            m.OnFrame(F("snapshot", 1, T0, R("a", 0, mode: "MOVING", v: 1)), T0);
            for (int i = 1; i <= 60; i++)
                m.OnFrame(F("delta", (ulong)(i + 1), T0 + i * 150, R("a", i * 0.15, mode: "MOVING", v: 1)), T0 + i * 150);
            var tr = m.Robots["a"].Track;
            double keepFrom = m.RenderTime(T0 + 60 * 150) - 3000;
            Assert.Less(tr.Count, 30);
            Assert.GreaterOrEqual(tr[1].T, keepFrom);
        }

        [Test]
        public void PoseWithoutPoseKeepsTheViewButNoSample()
        {
            m.OnFrame(F("snapshot", 1, T0, new RobotView { Name = "a", Conn = "ONLINE", HasPose = false }), T0);
            Assert.IsTrue(m.Robots.ContainsKey("a"));
            Assert.AreEqual(0, m.Robots["a"].Track.Count);
            Assert.IsNull(LiveModel.PoseAt(m.Robots["a"].Track, T0, m.Robots["a"].V));
        }

        // ---- poseAt (scripts/web_test.js 14-58) ----

        static readonly List<Sample> Track = new List<Sample>
        {
            new Sample(1000, 0, 0, 0, 1),
            new Sample(1200, 2, 0, 0, 1),
            new Sample(1400, 2, 2, Math.PI / 2, 1),
        };
        static readonly RobotView Moving = new RobotView { V = 1, Mode = "MOVING" };

        [Test]
        public void PoseAtNoSamples() => Assert.IsNull(LiveModel.PoseAt(new List<Sample>(), 1000, Moving));

        [Test]
        public void PoseAtBeforeTheFirstSampleHoldsIt() => Assert.AreEqual(Track[0], LiveModel.PoseAt(Track, 500, Moving).Value);

        [Test]
        public void PoseAtOnASample()
        {
            var p = LiveModel.PoseAt(Track, 1200, Moving).Value;
            Assert.AreEqual(2, p.X, 1e-9);
            Assert.AreEqual(0, p.Y, 1e-9);
        }

        [Test]
        public void PoseAtInterpolatesBetweenSamples()
        {
            var p = LiveModel.PoseAt(Track, 1100, Moving).Value;
            Assert.AreEqual(1, p.X, 1e-9);
            Assert.AreEqual(0, p.Y, 1e-9);
            Assert.AreEqual(0, p.Yaw, 1e-9);
            var q = LiveModel.PoseAt(Track, 1300, Moving).Value;
            Assert.AreEqual(2, q.X, 1e-9);
            Assert.AreEqual(1, q.Y, 1e-9);
            Assert.AreEqual(Math.PI / 4, q.Yaw, 1e-9);
        }

        [Test]
        public void PoseAtTurnsTheShortWayAcrossPi()
        {
            var tr = new List<Sample> { new Sample(0, 0, 0, 3.0, 1), new Sample(100, 0, 0, -3.0, 1) };
            var p = LiveModel.PoseAt(tr, 50, null).Value;
            Assert.AreEqual(Math.PI, Math.Abs(Math.Atan2(Math.Sin(p.Yaw), Math.Cos(p.Yaw))), 1e-6);
        }

        [Test]
        public void PoseAtTwoSamplesAtTheSameTimeGiveNoNaN()
        {
            var tr = new List<Sample> { new Sample(100, 1, 1, 0, 1), new Sample(100, 3, 1, 0, 1) };
            var p = LiveModel.PoseAt(tr, 100, null).Value;
            Assert.IsFalse(double.IsNaN(p.X) || double.IsNaN(p.Y));
        }

        [Test]
        public void PoseAtExtrapolatesAMovingRobotAlongItsHeading()
        {
            var p = LiveModel.PoseAt(Track, 1500, Moving).Value; // 100 ms at 1 m/s, heading +y
            Assert.AreEqual(2, p.X, 1e-9);
            Assert.AreEqual(2.1, p.Y, 1e-9);
        }

        [Test]
        public void PoseAtExtrapolationStopsAfter250Ms() =>
            Assert.AreEqual(2.25, LiveModel.PoseAt(Track, 5000, Moving).Value.Y, 1e-9);

        [Test]
        public void PoseAtDoesNotExtrapolateAStoppedOrIdleRobot()
        {
            Assert.AreEqual(Track[2], LiveModel.PoseAt(Track, 1500, new RobotView { V = 0, Mode = "MOVING" }).Value);
            Assert.AreEqual(Track[2], LiveModel.PoseAt(Track, 1500, new RobotView { V = 1, Mode = "IDLE" }).Value);
            Assert.AreEqual(Track[2], LiveModel.PoseAt(Track, 1500, null).Value);
        }

        [Test]
        public void RenderedPoseRunsRenderDelayBehindTheFeed()
        {
            // Frames 150 ms apart at 1 m/s; local clock = server clock.
            m.OnFrame(F("snapshot", 1, T0, R("a", 0, mode: "MOVING", v: 1)), T0);
            for (int i = 1; i <= 4; i++)
                m.OnFrame(F("delta", (ulong)(i + 1), T0 + i * 150, R("a", i * 0.15, mode: "MOVING", v: 1)), T0 + i * 150);
            var r = m.Robots["a"];
            // At local T0+600 the newest sample is x 0.6; drawn 350 ms behind: x 0.25.
            var p = LiveModel.PoseAt(r.Track, m.RenderTime(T0 + 600), r.V).Value;
            Assert.AreEqual(0.25, p.X, 1e-9);
        }
    }
}
