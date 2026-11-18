// FleetRobot against the protocol: seq/boot, publishing, the ack lifecycle,
// duplicates, expiry, cancel semantics, dock and battery. The cases follow
// adapter/test_fleet_logic.py (GoalTracker, CommandBook) and the adapter's
// command handling in fleet_adapter.py.
using System.Collections.Generic;
using System.Linq;
using Fleet.Core;
using Newtonsoft.Json.Linq;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class FleetRobotTests
    {
        const double Wall0 = 1_800_000_000.0;
        FleetRobot r;
        List<Outgoing> outq;
        double now, wall;

        [SetUp]
        public void SetUp()
        {
            r = new FleetRobot("unity_1", "unity-1", new RobotSim(0, 0, 80) { Speed = 1.0 });
            outq = new List<Outgoing>();
            now = 100.0;
            wall = Wall0;
        }

        // ---- helpers ----

        void Cmd(string json) => r.HandleCommand(json, now, wall, outq);

        string Goto(string id, double x, double y, double? expires = null) =>
            $"{{\"cmd_id\":\"{id}\",\"type\":\"goto\",\"x\":{x},\"y\":{y},\"yaw\":0,\"expires_at\":{expires ?? wall + 10}}}";

        void Run(double seconds, double dt = 0.05)
        {
            for (double t = 0; t < seconds - 1e-9; t += dt)
            {
                now += dt;
                wall += dt;
                r.Tick(dt, now, wall, outq);
            }
        }

        List<JObject> Take(string kind)
        {
            var topic = $"fleet/unity_1/{kind}";
            var got = outq.Where(o => o.Topic == topic).Select(o => JObject.Parse(o.Payload)).ToList();
            outq.RemoveAll(o => o.Topic == topic);
            return got;
        }

        List<string> Acks() => Take("cmd_ack").Select(a => $"{a["cmd_id"]} {a["status"]}").ToList();

        // ---- boot and seq ----

        [Test]
        public void BootIsFreshPerRobotAndProcess()
        {
            var other = new FleetRobot("unity_2", "unity-1", new RobotSim(0, 0, 80));
            Assert.AreEqual(8, r.Boot.Length);
            Assert.AreNotEqual(r.Boot, other.Boot);
        }

        [Test]
        public void SeqIncreasesPerTopicAndCarriesBootHostStamp()
        {
            Run(3.0);
            var states = Take("state");
            var poses = Take("pose");
            Assert.GreaterOrEqual(states.Count, 3, "1 s state heartbeat");
            Assert.GreaterOrEqual(poses.Count, 3, "1 s pose heartbeat while still");
            foreach (var list in new[] { states, poses })
            {
                for (int i = 0; i < list.Count; i++)
                {
                    Assert.AreEqual((ulong)(i + 1), (ulong)list[i]["seq"], "per-topic seq from 1, no gaps");
                    Assert.AreEqual(r.Boot, (string)list[i]["boot"]);
                    Assert.AreEqual("unity-1", (string)list[i]["host"]);
                    Assert.That((double)list[i]["stamp"], Is.GreaterThan(Wall0), "wall-clock unix seconds");
                }
            }
        }

        [Test]
        public void StateFieldsAndCanRideFalseEverywhere()
        {
            Cmd(Goto("c1", 2, 0));
            Run(4.0);
            Cmd("{\"cmd_id\":\"d1\",\"type\":\"dock\"}");
            Run(3.0);
            var states = Take("state");
            Assert.IsNotEmpty(states);
            foreach (var s in states)
            {
                Assert.AreEqual(JTokenType.Boolean, s["can_ride"].Type);
                Assert.IsFalse((bool)s["can_ride"]);
                Assert.AreEqual(0, (int)s["silence_s"]);
                Assert.That((double)s["battery"], Is.InRange(0.0, 100.0));
                Assert.IsNotNull(s["task_id"]);
                Assert.IsNotNull(s["error"]);
            }
            var pose = Take("pose").Last();
            Assert.AreEqual(1, (int)pose["floor"]);
            Assert.AreEqual("map", (string)pose["frame"]);
        }

        [Test]
        public void BatteryNullWhenNotReported()
        {
            r.ReportBattery = false;
            Run(0.1);
            var s = Take("state").Single();
            Assert.AreEqual(JTokenType.Null, s["battery"].Type);
        }

        [Test]
        public void PoseRateAndDeadbandWhileMoving()
        {
            Run(0.1);
            Take("pose");
            Cmd(Goto("c1", 10, 0));
            Run(2.0, 0.01); // 100 frames per second, 1 m/s
            var poses = Take("pose");
            Assert.That(poses.Count, Is.InRange(9, 11), "about 5 Hz while moving, not once per frame");
        }

        [Test]
        public void TaskIdChangeIsPublishedAtOnce()
        {
            Run(0.3);
            Take("state");
            Cmd(Goto("c1", 5, 0));
            Run(0.05);
            var s = Take("state").Single();
            Assert.AreEqual("c1", (string)s["task_id"]);
            Assert.AreEqual("MOVING", (string)s["mode"]);
        }

        // ---- goto lifecycle ----

        [Test]
        public void GotoAcceptedThenDoneAndTaskIdCleared()
        {
            Cmd(Goto("c1", 2, 0));
            CollectionAssert.AreEqual(new[] { "c1 accepted" }, Acks());
            Assert.AreEqual("c1", r.TaskId);
            Run(1.0);
            Assert.IsEmpty(Acks(), "still driving");
            Run(2.0);
            CollectionAssert.AreEqual(new[] { "c1 done" }, Acks());
            Assert.AreEqual("", r.TaskId);
            Assert.AreEqual("IDLE", r.Sim.Mode);
            Assert.AreEqual(2.0, r.Sim.X, 1e-9);
            Assert.AreEqual("", (string)Take("state").Last()["task_id"]);
        }

        [Test]
        public void GotoAtCurrentPositionFinishes()
        {
            Cmd(Goto("c1", 0, 0));
            Run(0.05);
            CollectionAssert.AreEqual(new[] { "c1 accepted", "c1 done" }, Acks());
        }

        [Test]
        public void NewGotoPreemptsOldOneAsCancelled()
        {
            Cmd(Goto("c1", 5, 0));
            Run(0.5);
            Cmd(Goto("c2", 0, 3));
            var acks = Take("cmd_ack");
            CollectionAssert.AreEqual(new[] { "c1 accepted", "c1 cancelled", "c2 accepted" },
                acks.Select(a => $"{a["cmd_id"]} {a["status"]}"));
            StringAssert.Contains("preempted by c2", (string)acks[1]["message"]);
            Assert.AreEqual("c2", r.TaskId);
        }

        [Test]
        public void BadGotoFails()
        {
            Cmd("{\"cmd_id\":\"c1\",\"type\":\"goto\",\"x\":\"abc\",\"y\":1}");
            var a = Take("cmd_ack").Single();
            Assert.AreEqual("failed", (string)a["status"]);
            Assert.AreEqual("goto needs numeric x and y", (string)a["message"]);
            Assert.AreEqual("IDLE", r.Sim.Mode);
        }

        [Test]
        public void GotoFailsWhenRobotFaults()
        {
            Cmd(Goto("c1", 5, 0));
            Run(0.5);
            r.Sim.Fail("simulated_fault");
            Run(0.05);
            var a = Take("cmd_ack").Last();
            Assert.AreEqual("failed", (string)a["status"]);
            StringAssert.Contains("simulated_fault", (string)a["message"]);
            // A goto to a robot in ERROR is refused.
            Cmd(Goto("c2", 1, 1));
            Assert.AreEqual("failed", (string)Take("cmd_ack").Single()["status"]);
            Cmd("{\"cmd_id\":\"r1\",\"type\":\"reset_error\"}");
            CollectionAssert.AreEqual(new[] { "r1 accepted" }, Acks());
            Assert.AreEqual("IDLE", r.Sim.Mode);
        }

        // ---- cancel ----

        [Test]
        public void CancelWithTargetEndsGotoAsCancelledNotFailed()
        {
            Cmd(Goto("c1", 5, 0));
            Run(0.5);
            Acks();
            Cmd("{\"cmd_id\":\"c1.x1\",\"type\":\"cancel\",\"target\":\"c1\"}");
            CollectionAssert.AreEqual(new[] { "c1 cancelled", "c1.x1 accepted" }, Acks());
            Assert.AreEqual("", r.TaskId);
            Run(5.0);
            Assert.IsEmpty(Acks(), "a cancelled goto is never reported again");
            Assert.AreEqual("IDLE", r.Sim.Mode);
            Assert.Less(r.Sim.X, 1.0, "stopped short of the goal");
        }

        [Test]
        public void CancelForAnotherCmdLeavesCurrentGotoAlone()
        {
            Cmd(Goto("c2", 5, 0));
            Acks();
            Cmd("{\"cmd_id\":\"c1.x1\",\"type\":\"cancel\",\"target\":\"c1\"}");
            var a = Take("cmd_ack").Single();
            Assert.AreEqual("c1.x1", (string)a["cmd_id"]);
            Assert.AreEqual("failed", (string)a["status"]);
            Assert.AreEqual("c2", r.TaskId);
            Assert.AreEqual("MOVING", r.Sim.Mode);
        }

        [Test]
        public void CancelWithNothingRunningFails()
        {
            Cmd("{\"cmd_id\":\"x1\",\"type\":\"cancel\"}");
            var a = Take("cmd_ack").Single();
            Assert.AreEqual("failed", (string)a["status"]);
            StringAssert.Contains("nothing to cancel", (string)a["message"]);
        }

        [Test]
        public void CancelWithoutTargetStopsDocking()
        {
            r.Sim.X = 3;
            Cmd("{\"cmd_id\":\"d1\",\"type\":\"dock\"}");
            Cmd("{\"cmd_id\":\"x1\",\"type\":\"cancel\"}");
            CollectionAssert.AreEqual(new[] { "d1 accepted", "x1 accepted" }, Acks());
            Assert.AreEqual("IDLE", r.Sim.Mode);
        }

        // ---- duplicates and expiry ----

        [Test]
        public void DuplicateIsNotRunAgainButLastAckIsRepeated()
        {
            Cmd(Goto("c1", 1, 0));
            Assert.AreEqual(1, Take("cmd_ack").Count);
            Cmd(Goto("c1", 1, 0));
            var again = Take("cmd_ack").Single();
            Assert.AreEqual("accepted", (string)again["status"]);
            Run(2.0);
            CollectionAssert.AreEqual(new[] { "c1 done" }, Acks());
            Cmd(Goto("c1", 1, 0)); // fleetd re-sends after losing the done
            CollectionAssert.AreEqual(new[] { "c1 done" }, Acks());
            Assert.AreEqual("IDLE", r.Sim.Mode, "not run again");
            Assert.AreEqual("", r.TaskId);
        }

        [Test]
        public void ExpiredCommandIsRejectedAndResendGetsSameAnswer()
        {
            Cmd(Goto("c1", 5, 0, wall - 1));
            var a = Take("cmd_ack").Single();
            Assert.AreEqual("failed", (string)a["status"]);
            Assert.AreEqual("expired", (string)a["message"]);
            Assert.AreEqual("IDLE", r.Sim.Mode);
            Cmd(Goto("c1", 5, 0, wall + 100)); // same id: a duplicate, not a new goto
            Assert.AreEqual("expired", (string)Take("cmd_ack").Single()["message"]);
            Assert.AreEqual("IDLE", r.Sim.Mode);
        }

        [Test]
        public void NullOrMissingExpiresAtNeverExpires()
        {
            Cmd("{\"cmd_id\":\"c1\",\"type\":\"goto\",\"x\":1,\"y\":0,\"expires_at\":null}");
            Cmd("{\"cmd_id\":\"c2\",\"type\":\"goto\",\"x\":2,\"y\":0}");
            CollectionAssert.AreEqual(new[] { "c1 accepted", "c1 cancelled", "c2 accepted" }, Acks());
        }

        [Test]
        public void UnknownTypesAndRideFail()
        {
            Cmd("{\"cmd_id\":\"k1\",\"type\":\"dance\"}");
            Cmd("{\"cmd_id\":\"k2\",\"type\":\"ride\",\"ride\":{}}");
            var acks = Take("cmd_ack");
            Assert.AreEqual(2, acks.Count);
            Assert.That(acks.Select(x => (string)x["status"]), Is.All.EqualTo("failed"));
            StringAssert.Contains("unknown command type 'ride'", (string)acks[1]["message"]);
        }

        [Test]
        public void MalformedCommandsAreIgnored()
        {
            Cmd("not json");
            Cmd("{\"type\":\"goto\",\"x\":1,\"y\":1}");
            Cmd("{\"cmd_id\":\"c1\"}");
            Cmd("{\"cmd_id\":\"c2\",\"type\":\"goto\",\"x\":1,\"y\":1,\"expires_at\":\"soon\"}");
            Assert.IsEmpty(outq);
        }

        // ---- dock and battery ----

        [Test]
        public void DockPreemptsGotoThenChargesUntilFullThenIdle()
        {
            r.Sim.Battery = 85;
            r.Sim.ChargeRate = 5.0;
            Cmd(Goto("c1", 4, 0));
            Run(2.0);
            Cmd("{\"cmd_id\":\"dock.unity_1.1.b\",\"type\":\"dock\",\"expires_at\":" + (wall + 10) + "}");
            CollectionAssert.AreEqual(new[] { "c1 accepted", "c1 cancelled", "dock.unity_1.1.b accepted" }, Acks());
            Assert.AreEqual("DOCKING", r.Sim.Mode);
            Assert.AreEqual("", r.TaskId, "a dock is never reported as task_id");
            Run(2.5);
            Assert.AreEqual("CHARGING", r.Sim.Mode);
            Assert.AreEqual(0.0, r.Sim.X, 1e-9, "the dock is the spawn point");
            Run(5.0);
            Assert.AreEqual("IDLE", r.Sim.Mode);
            Assert.That(r.Sim.Battery, Is.InRange(99.9, 100.0), "charged to full (clamped), then the idle drain");
            Assert.IsEmpty(Acks(), "a dock gets only accepted");
            var modes = Take("state").Select(s => (string)s["mode"]).Distinct().ToList();
            CollectionAssert.IsSubsetOf(new[] { "DOCKING", "CHARGING", "IDLE" }, modes);
        }

        [Test]
        public void BatteryDrainsWhileMovingAndIsClamped()
        {
            r.Sim.DrainPerM = 1.0;
            r.Sim.DrainIdle = 0;
            Cmd(Goto("c1", 10, 0));
            Run(5.0);
            Assert.AreEqual(75.0, r.Sim.Battery, 0.2, "5 m at 1 %/m");
            r.Sim.Battery = 150;
            Assert.AreEqual(100.0, r.Sim.Battery);
            r.Sim.Battery = -3;
            Assert.AreEqual(0.0, r.Sim.Battery);
        }

        [Test]
        public void EmptyBatteryWhileDrivingIsAnError()
        {
            r.Sim.Battery = 0.5;
            r.Sim.DrainPerM = 1.0;
            Cmd(Goto("c1", 10, 0));
            Run(2.0);
            Assert.AreEqual("ERROR", r.Sim.Mode);
            Assert.AreEqual("battery_empty", r.Sim.Error);
            Assert.AreEqual(0.0, r.Sim.Battery);
            CollectionAssert.AreEqual(new[] { "c1 accepted", "c1 failed" }, Acks());
        }

        [Test]
        public void DockRefusedInError()
        {
            r.Sim.Fail("x");
            Cmd("{\"cmd_id\":\"d1\",\"type\":\"dock\"}");
            Assert.AreEqual("failed", (string)Take("cmd_ack").Single()["status"]);
        }
    }
}
