// FleetRobot + RobotSim riding the elevator end to end, the way
// fleet_adapter.py drives RobotRide: blackout (per robot), dropped commands,
// link-return order, door-zone timing, exit failure, stranded robot, can_ride
// false, plus the elevator topic parsing used by FleetBootstrap.
using System.Collections.Generic;
using System.Linq;
using Fleet.Core;
using Newtonsoft.Json.Linq;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class FleetRobotRideTests
    {
        const double Wall0 = 1_800_000_000.0;
        const string Trip = "E1-u1-1";
        FleetRobot a, b;
        List<Outgoing> outq;
        double now, wall;

        [SetUp]
        public void SetUp()
        {
            // Both at the lobby (10, 0); speed 1 m/s.
            a = new FleetRobot("unity_1", "unity-1", new RobotSim(10, 0, 80) { Speed = 1.0 });
            b = new FleetRobot("unity_2", "unity-1", new RobotSim(10, 0, 80) { Speed = 1.0 });
            outq = new List<Outgoing>();
            now = 100.0;
            wall = Wall0;
        }

        // ---- helpers ----

        static string RideCmd(string id, string robot, int slot = 0, int exit = 3, string trip = Trip, double? expires = null) =>
            new JObject
            {
                ["cmd_id"] = id, ["type"] = "ride", ["x"] = 10, ["y"] = 0, ["expires_at"] = expires ?? Wall0 + 1000,
                ["ride"] = new JObject
                {
                    ["trip"] = trip, ["elevator"] = "E1", ["origin"] = 1, ["direction"] = "up", ["exit_floor"] = exit,
                    ["board"] = 0, ["slot"] = slot, ["boarding"] = new JArray(robot), ["stops"] = new JArray(exit),
                    ["silence_s"] = 40, ["turn_timeout_s"] = 15,
                    ["lobby"] = new JObject { ["x"] = 10, ["y"] = 0 }, ["exit_lobby"] = new JObject { ["x"] = 10, ["y"] = 0 },
                },
            }.ToString();

        static JObject Turn(int n, string robot, string action, int floor, string trip = Trip) => new JObject
        {
            ["trip"] = trip, ["n"] = n, ["robot"] = robot, ["action"] = action, ["floor"] = floor,
            ["lobby"] = new JObject { ["x"] = 10, ["y"] = 0 }, ["timeout_s"] = 15,
        };

        static JObject CarState(int floor, bool moving = false) =>
            new JObject { ["floor"] = floor, ["moving"] = moving, ["door"] = moving ? "closed" : "open", ["trip"] = Trip };

        void Cmd(FleetRobot r, string json) => r.HandleCommand(json, now, wall, outq);
        void Elev(string kind, JObject msg) { foreach (var r in new[] { a, b }) r.HandleElevator("E1", kind, msg, now, wall, outq); }

        void Run(double seconds, double dt = 0.05)
        {
            for (double t = 0; t < seconds - 1e-9; t += dt)
            {
                now += dt;
                wall += dt;
                a.Tick(dt, now, wall, outq);
                b.Tick(dt, now, wall, outq);
            }
        }

        /// <summary>Runs until a message on the topic shows up (or the time runs out); returns it.</summary>
        JObject RunUntil(string topic, System.Func<JObject, bool> match = null, double max = 20, double dt = 0.05)
        {
            for (double t = 0; t < max; t += dt)
            {
                int from = outq.Count;
                Run(dt, dt);
                for (int i = from; i < outq.Count; i++)
                    if (outq[i].Topic == topic && (match == null || match(JObject.Parse(outq[i].Payload))))
                        return JObject.Parse(outq[i].Payload);
            }
            Assert.Fail($"no {topic} within {max} s");
            return null;
        }

        List<JObject> Take(string topic)
        {
            var got = outq.Where(o => o.Topic == topic).Select(o => JObject.Parse(o.Payload)).ToList();
            outq.RemoveAll(o => o.Topic == topic);
            return got;
        }

        List<string> Acks(string robot) => Take($"fleet/{robot}/cmd_ack").Select(x => $"{x["cmd_id"]} {x["status"]}").ToList();

        int FleetMsgs(string robot) => outq.Count(o => o.Topic.StartsWith($"fleet/{robot}/"));

        /// <summary>Accept, board on turn 1 and wait for the board clear. Leaves outq empty.</summary>
        void AcceptAndBoard(FleetRobot r, int slot = 0, int n = 1)
        {
            Cmd(r, RideCmd("R-" + r.Name, r.Name, slot));
            Elev("turn", Turn(n, r.Name, "board", 1));
            RunUntil("elevator/E1/zone", z => (string)z["event"] == "clear" && (string)z["robot"] == r.Name);
            Assert.IsTrue(r.Sim.InCar);
            outq.Clear();
        }

        // ---- a whole ride ----

        [Test]
        public void FullRideBlackoutAndLinkReturnOrder()
        {
            Run(0.2);
            outq.Clear();
            Cmd(a, RideCmd("R1", "unity_1", slot: 1));
            CollectionAssert.AreEqual(new[] { "R1 accepted" }, Acks("unity_1"));
            var waiting = Take("fleet/unity_1/state").Single();
            Assert.AreEqual("WAITING_ELEVATOR", (string)waiting["mode"]);
            Assert.AreEqual("R1", (string)waiting["task_id"]);
            Assert.AreEqual(0.0, (double)waiting["silence_s"]);
            Assert.AreEqual("ride_accepted", (string)Take("fleet/unity_1/event").Single()["event"]);

            // Board turn: event "boarding", the last state (BOARDING + silence_s), then zone enter, in this order.
            Elev("turn", Turn(1, "unity_1", "board", 1));
            CollectionAssert.AreEqual(new[] { "fleet/unity_1/event", "fleet/unity_1/state", "elevator/E1/zone" }, outq.Select(o => o.Topic));
            var boarding = JObject.Parse(outq[1].Payload);
            Assert.AreEqual("BOARDING", (string)boarding["mode"]);
            Assert.AreEqual(40.0, (double)boarding["silence_s"]);
            Assert.AreEqual("enter", (string)JObject.Parse(outq[2].Payload)["event"]);
            Assert.IsTrue(a.Ride.Silent);
            Assert.AreEqual(10.0, a.Sim.X, 1e-9, "enter goes out before the robot moves");
            outq.Clear();

            var clear = RunUntil("elevator/E1/zone", z => (string)z["event"] == "clear");
            Assert.AreEqual("board", (string)clear["action"]);
            Assert.AreEqual(RobotRide.CarSlot(new Spot(10, 0), 1)[0], a.Sim.X, 1e-9, "clear only once in the slot");
            Assert.IsTrue(a.Sim.InCar);
            Assert.AreEqual(0, FleetMsgs("unity_1"), "nothing on fleet/* while boarding");
            outq.Clear();

            // In the car: the car goes to 3 and opens; the robot follows the car's floor.
            Elev("state", CarState(2, moving: true));
            Run(0.2);
            Assert.AreEqual(1, a.Sim.Floor);
            Elev("state", CarState(3));
            Run(0.2);
            Assert.AreEqual(3, a.Sim.Floor);

            Elev("turn", Turn(4, "unity_1", "exit", 3));
            var exitEnter = Take("elevator/E1/zone").Single();
            Assert.AreEqual("exit enter", $"{exitEnter["action"]} {exitEnter["event"]}");
            clear = RunUntil("elevator/E1/zone", z => (string)z["event"] == "clear");
            Assert.AreEqual(3, (int)clear["floor"]);
            Assert.AreEqual(10.0, a.Sim.X, 1e-9, "clear once out at the lobby");
            Assert.IsFalse(a.Sim.InCar);
            Assert.AreEqual(0, FleetMsgs("unity_1"), "nothing on fleet/* from inside the car");
            Assert.IsTrue(a.Ride.Silent, "the link is back only 1 s after the exit clear");

            Run(0.9);
            Assert.AreEqual(0, FleetMsgs("unity_1"));
            RunUntil("fleet/unity_1/cmd_ack");
            var fleet = outq.Where(o => o.Topic.StartsWith("fleet/unity_1/")).ToList();
            // pose (new floor) -> state IDLE silence 0 -> queued events in seq order -> done with floor.
            CollectionAssert.AreEqual(new[] { "pose", "state", "event", "event", "cmd_ack" },
                fleet.Select(o => o.Topic.Split('/')[2]).Take(5));
            var pose = JObject.Parse(fleet[0].Payload);
            Assert.AreEqual(3, (int)pose["floor"]);
            var st = JObject.Parse(fleet[1].Payload);
            Assert.AreEqual("IDLE", (string)st["mode"]);
            Assert.AreEqual("", (string)st["task_id"]);
            Assert.AreEqual(0.0, (double)st["silence_s"]);
            var evs = fleet.Skip(2).Take(2).Select(o => JObject.Parse(o.Payload)).ToList();
            CollectionAssert.AreEqual(new[] { "boarded", "exited" }, evs.Select(e => (string)e["event"]));
            Assert.Less((long)evs[0]["seq"], (long)evs[1]["seq"]);
            var done = JObject.Parse(fleet[4].Payload);
            Assert.AreEqual("R1 done 3", $"{done["cmd_id"]} {done["status"]} {done["floor"]}");
            Assert.IsFalse(a.Ride.Silent);
            Assert.IsFalse(a.Ride.Holding);

            // fleetd asks again with the same cmd_id and no plan: the same "done".
            outq.Clear();
            Cmd(a, "{\"cmd_id\":\"R1\",\"type\":\"ride\"}");
            var again = Take("fleet/unity_1/cmd_ack").Single();
            Assert.AreEqual("done", (string)again["status"]);
            Assert.AreEqual(3, (int)again["floor"]);
        }

        // ---- per-robot blackout ----

        [Test]
        public void BlackoutIsPerRobotTheOtherRobotKeepsTalking()
        {
            AcceptAndBoard(a);
            Run(3.0);
            Assert.AreEqual(0, FleetMsgs("unity_1"), "a is in the car: silent");
            Assert.GreaterOrEqual(Take("fleet/unity_2/state").Count, 2, "b keeps its state heartbeat");
            Assert.GreaterOrEqual(Take("fleet/unity_2/pose").Count, 2, "b keeps its pose heartbeat");

            // A command for a is dropped (no ack); b's command is handled.
            Cmd(a, "{\"cmd_id\":\"g1\",\"type\":\"goto\",\"x\":0,\"y\":0}");
            Cmd(b, "{\"cmd_id\":\"g2\",\"type\":\"goto\",\"x\":11,\"y\":0}");
            Assert.IsEmpty(Acks("unity_1"));
            CollectionAssert.AreEqual(new[] { "g2 accepted" }, Acks("unity_2"));
            Assert.IsTrue(a.Sim.InCar);
        }

        [Test]
        public void ADroppedCommandIsNewAgainOnceTheLinkIsBack()
        {
            AcceptAndBoard(a);
            Cmd(a, "{\"cmd_id\":\"g1\",\"type\":\"goto\",\"x\":12,\"y\":0}"); // dropped in the car
            Elev("state", CarState(3));
            Elev("turn", Turn(2, "unity_1", "exit", 3));
            RunUntil("fleet/unity_1/cmd_ack");
            outq.Clear();
            Cmd(a, "{\"cmd_id\":\"g1\",\"type\":\"goto\",\"x\":12,\"y\":0}"); // fleetd's re-send
            CollectionAssert.AreEqual(new[] { "g1 accepted" }, Acks("unity_1"), "not taken for a duplicate");
            Assert.AreEqual("MOVING", a.Sim.Mode);
        }

        // ---- commands while holding a ride ----

        [Test]
        public void RideReplacesARunningGotoAndGotoDockAreRefusedWhileHolding()
        {
            a.Sim.X = 9.8;
            Cmd(a, "{\"cmd_id\":\"g1\",\"type\":\"goto\",\"x\":20,\"y\":0}");
            Run(0.1);
            outq.Clear();
            Cmd(a, RideCmd("R1", "unity_1"));
            CollectionAssert.AreEqual(new[] { "g1 cancelled", "R1 accepted" }, Acks("unity_1"));
            Assert.AreEqual("IDLE", a.Sim.Mode, "the goto is stopped");
            Cmd(a, "{\"cmd_id\":\"g2\",\"type\":\"goto\",\"x\":1,\"y\":1}");
            Cmd(a, "{\"cmd_id\":\"d1\",\"type\":\"dock\"}");
            var acks = Take("fleet/unity_1/cmd_ack");
            Assert.That(acks.Select(x => (string)x["status"]), Is.All.EqualTo("failed"));
            StringAssert.Contains("holding ride R1", (string)acks[0]["message"]);
            // A cancel for the ride at the lobby ends it.
            Cmd(a, "{\"cmd_id\":\"R1.x1\",\"type\":\"cancel\",\"target\":\"R1\"}");
            CollectionAssert.AreEqual(new[] { "R1 cancelled", "R1.x1 accepted" }, Acks("unity_1"));
            Assert.IsFalse(a.Ride.Holding);
        }

        [Test]
        public void RideRefusedAwayFromTheLobby()
        {
            a.Sim.X = 5;
            Cmd(a, RideCmd("R1", "unity_1"));
            var ack = Take("fleet/unity_1/cmd_ack").Single();
            Assert.AreEqual("failed", (string)ack["status"]);
            StringAssert.Contains("not at the lobby", (string)ack["message"]);
        }

        // ---- failures ----

        [Test]
        public void ExitBlockedGoesBackToTheSlotStaysSilentThenExitsOnTheRetry()
        {
            a.Sim.ExitFailFloor = 3;
            a.Sim.ExitFailCount = 1;
            AcceptAndBoard(a, slot: 1);
            double slotX = a.Sim.X;
            Elev("state", CarState(3));
            Elev("turn", Turn(2, "unity_1", "exit", 3));
            var failed = RunUntil("elevator/E1/zone", z => (string)z["event"] == "failed");
            Assert.AreEqual("door zone blocked", (string)failed["reason"]);
            Assert.AreEqual(slotX, a.Sim.X, 1e-9, "back in its own slot");
            Assert.IsTrue(a.Sim.InCar);
            Assert.IsTrue(a.Ride.Silent);
            Assert.AreEqual(0, FleetMsgs("unity_1"));
            outq.Clear();
            Elev("turn", Turn(2, "unity_1", "exit", 3)); // re-sent n: same answer, no move
            Assert.AreEqual("failed", (string)Take("elevator/E1/zone").Single()["event"]);
            Elev("turn", Turn(3, "unity_1", "exit", 3)); // the retry
            RunUntil("elevator/E1/zone", z => (string)z["event"] == "clear");
            RunUntil("fleet/unity_1/cmd_ack");
            var evs = Take("fleet/unity_1/event").Select(e => (string)e["event"]).ToList();
            CollectionAssert.AreEqual(new[] { "boarded", "exit_failed", "exited" }, evs);
        }

        [Test]
        public void TripReleasedWithTheRobotInsideThenRescued()
        {
            AcceptAndBoard(a);
            Elev("state", CarState(4));
            Run(0.1);
            Elev("trip", JObject.Parse($"{{\"trip\":\"{Trip}\",\"stage\":\"travel\",\"turn\":{{\"robot\":\"unity_1\",\"n\":9,\"action\":\"exit\",\"floor\":4}}}}"));
            Run(0.2);
            Assert.IsTrue(a.Sim.InCar, "never moves on the retained trip's turn");
            Assert.AreEqual(0, outq.Count(o => o.Topic == "elevator/E1/zone"));
            Elev("trip", null); // cleared retained trip: the trip ended with the robot inside
            var ack = Take("fleet/unity_1/cmd_ack").Single();
            Assert.AreEqual("failed", (string)ack["status"]);
            Assert.AreEqual(4, (int)ack["floor"]);
            Run(1.2);
            var st = Take("fleet/unity_1/state").Last();
            Assert.AreEqual("ERROR", (string)st["mode"]);
            StringAssert.Contains("needs a human", (string)st["error"]);
            outq.Clear();
            Cmd(a, "{\"cmd_id\":\"res1\",\"type\":\"rescue\"}");
            CollectionAssert.AreEqual(new[] { "res1 accepted" }, Acks("unity_1"));
            Run(0.1);
            Assert.AreEqual("rescued", (string)Take("fleet/unity_1/event").Single()["event"]);
            Assert.AreEqual("IDLE", (string)Take("fleet/unity_1/state").Last()["mode"]);
            Assert.AreEqual(4, a.Sim.Floor);
        }

        [Test]
        public void FaultInTheDoorZoneKeepsTheMoveRunningUntilResetError()
        {
            Cmd(a, RideCmd("R1", "unity_1"));
            Elev("turn", Turn(1, "unity_1", "board", 1));
            Run(0.5);
            outq.Clear();
            a.Sim.Fail("simulated_fault");
            Run(RobotRide.MoveExtraS + 20); // past turn_timeout_s + extra: still "running", so no answer
            Assert.IsEmpty(outq.Where(o => o.Topic == "elevator/E1/zone").ToList());
            Assert.AreEqual("running", a.Sim.LastMove.Status);
            Assert.IsTrue(a.Ride.Silent);
            // The robot cannot get the reset over Wi-Fi in the door zone; a human resets it on the spot.
            Assert.IsNull(a.Sim.ResetError());
            var failed = RunUntil("elevator/E1/zone", z => (string)z["event"] == "failed");
            StringAssert.Contains("fault in the door zone", (string)failed["reason"]);
            Assert.AreEqual(10.0, a.Sim.X, 1e-9, "pushed back to the lobby");
            Assert.IsFalse(a.Ride.Silent);
            Assert.AreEqual("WAITING_ELEVATOR", (string)Take("fleet/unity_1/state").Last()["mode"]);
        }

        // ---- can_ride false ----

        [Test]
        public void CanRideFalseReportsItAndFailsRides()
        {
            a.CanRide = false;
            Run(0.1);
            Assert.IsFalse((bool)Take("fleet/unity_1/state").Single()["can_ride"]);
            Cmd(a, RideCmd("R1", "unity_1"));
            var ack = Take("fleet/unity_1/cmd_ack").Single();
            Assert.AreEqual("failed", (string)ack["status"]);
            StringAssert.Contains("can_ride false", (string)ack["message"]);
            outq.Clear();
            a.HandleElevator("E1", "turn", Turn(1, "unity_1", "board", 1), now, wall, outq);
            Assert.IsEmpty(outq);
        }

        // ---- MQTT helpers ----

        [Test]
        public void ElevatorTopicsAndBodies()
        {
            Assert.IsTrue(Topics.ParseElevator("elevator/E1/turn", out var car, out var kind));
            Assert.AreEqual(("E1", "turn"), (car, kind));
            Assert.IsFalse(Topics.ParseElevator("elevator/E1/zone", out _, out _), "our own zone messages are not followed");
            Assert.IsFalse(Topics.ParseElevator("elevator/E1/cmd", out _, out _));
            Assert.IsFalse(Topics.ParseElevator("fleet/unity_1/cmd", out _, out _));
            Assert.IsTrue(Topics.ElevatorBody("trip", "", out var msg));
            Assert.IsNull(msg, "an empty retained trip is a cleared trip");
            Assert.IsFalse(Topics.ElevatorBody("turn", "", out _));
            Assert.IsFalse(Topics.ElevatorBody("state", "[1,2]", out _));
            Assert.IsFalse(Topics.ElevatorBody("state", "{bad", out _));
            Assert.IsTrue(Topics.ElevatorBody("state", "{\"floor\":2}", out msg));
            Assert.AreEqual(2, (int)msg["floor"]);
        }

        [Test]
        public void OnlyTheRobotOnThatCarReacts()
        {
            Cmd(a, RideCmd("R1", "unity_1"));
            outq.Clear();
            b.HandleElevator("E1", "turn", Turn(1, "unity_1", "board", 1), now, wall, outq);
            a.HandleElevator("E2", "turn", Turn(1, "unity_1", "board", 1), now, wall, outq);
            Assert.IsEmpty(outq);
            a.HandleElevator("E1", "turn", Turn(1, "unity_1", "board", 1), now, wall, outq);
            Assert.AreEqual(1, outq.Count(o => o.Topic == "elevator/E1/zone"));
        }

        // ---- RobotSim moves (mock_fleet.py on_elevator_move) ----

        [Test]
        public void SimFloorMoveOnlyInTheCarAndGotoRefusedInTheCar()
        {
            var s = new RobotSim(10, 0, 80);
            s.StartMove(new ElevatorMove { Id = 1, Op = "floor", Floor = 5 });
            Assert.AreEqual(1, s.Floor, "not in a car: the floor stays");
            Assert.AreEqual("done", s.LastMove.Status);
            s.StartMove(new ElevatorMove { Id = 2, Op = "exit", Floor = 3, Path = new List<double[]> { new[] { 11.0, 0 } } });
            Assert.AreEqual("failed", s.LastMove.Status, "cannot exit when not in the car");
            s.StartMove(new ElevatorMove { Id = 3, Op = "board", Path = new List<double[]> { new[] { 11.0, 0 }, new[] { 11.8, 0 } } });
            Assert.AreEqual("running", s.LastMove.Status);
            Assert.IsFalse(s.InCar, "in_car changes only when the move ends");
            Assert.AreEqual("in the elevator", s.Goto(0, 0, 0));
            for (int i = 0; i < 100 && s.InMove; i++) s.Step(0.05);
            Assert.IsTrue(s.InCar);
            Assert.AreEqual("done", s.LastMove.Status);
            Assert.AreEqual("in the elevator", s.Dock());
            s.StartMove(new ElevatorMove { Id = 4, Op = "floor", Floor = 5 });
            Assert.AreEqual(5, s.Floor);
            s.StartMove(new ElevatorMove { Id = 4, Op = "floor", Floor = 6 });
            Assert.AreEqual(5, s.Floor, "the same move id again is ignored");
        }

        [Test]
        public void SimDoorMoveSecondsSlowsTheMove()
        {
            var s = new RobotSim(10, 0, 80) { DoorMoveS = 4 };
            s.StartMove(new ElevatorMove { Id = 1, Op = "board", Path = new List<double[]> { new[] { 11.0, 0 }, new[] { 11.8, 0 } } });
            double t = 0;
            while (s.InMove && t < 20) { s.Step(0.05); t += 0.05; }
            Assert.AreEqual(4.0, t, 0.2);
        }
    }
}
