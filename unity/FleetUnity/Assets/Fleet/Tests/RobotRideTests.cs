// RobotRide against docs/ELEVATOR.md 5: a port of the 34 cases of
// adapter/test_ride_logic.py (same names, same behaviour; parametrized
// Python cases are TestCases here).
using System.Collections.Generic;
using System.Linq;
using Fleet.Core;
using Newtonsoft.Json.Linq;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class RobotRideTests
    {
        static readonly Spot Lobby = new Spot(10.0, 0.0);
        const double Wall = 1000.0;

        // ---- helpers (test_ride_logic.py:1-75) ----

        static JObject Plan(System.Action<JObject> edit = null)
        {
            var p = JObject.Parse(@"{""trip"":""E1-k3-1"",""elevator"":""E1"",""origin"":1,""direction"":""up"",""exit_floor"":3,
                ""board"":2,""slot"":0,""boarding"":[""robot_2"",""robot_3"",""robot_1""],""stops"":[3,5,7],""silence_s"":58,
                ""turn_timeout_s"":15,""lobby"":{""x"":10.0,""y"":0.0},""exit_lobby"":{""x"":10.0,""y"":0.0}}");
            edit?.Invoke(p);
            return p;
        }

        static RideRobotState State(string mode = "IDLE", int floor = 1, bool inCar = false, string error = "") =>
            new RideRobotState { Mode = mode, Floor = floor, InCar = inCar, Error = error };

        static JObject TurnMsg(int n, string action, int floor = 1, string robot = "robot_1", string trip = "E1-k3-1", Spot? lobby = null)
        {
            var l = lobby ?? Lobby;
            return new JObject
            {
                ["trip"] = trip, ["n"] = n, ["robot"] = robot, ["action"] = action, ["floor"] = floor,
                ["lobby"] = new JObject { ["x"] = l.X, ["y"] = l.Y }, ["timeout_s"] = 15,
            };
        }

        static List<RideKind> Kinds(List<RideAction> a) => a.Select(x => x.Kind).ToList();
        static List<RideAction> Only(List<RideAction> a, RideKind k) => a.Where(x => x.Kind == k).ToList();
        static List<string> Acks(List<RideAction> a) => a.Where(x => x.Kind == RideKind.Ack).Select(x => $"{x.CmdId} {x.Status}").ToList();
        static List<string> Zones(List<RideAction> a) =>
            a.Where(x => x.Kind == RideKind.Zone).Select(x => $"{x.Body["n"]} {x.Body["action"]} {x.Body["event"]}").ToList();
        static List<string> Events(List<RideAction> a) => a.Where(x => x.Kind == RideKind.Event).Select(x => (string)x.Body["event"]).ToList();
        static ElevatorMove MoveOf(List<RideAction> a) => Only(a, RideKind.Move)[0].Move;

        static RobotRide NewRide(int ignoreTurns = 0, string boot = "boot1") => new RobotRide("robot_1", boot, () => Wall, ignoreTurns);

        static RobotRide Accepted(System.Action<JObject> edit = null)
        {
            var r = NewRide();
            var acts = r.Accept("R1", Plan(edit), State(), Lobby);
            CollectionAssert.AreEqual(new[] { "R1 accepted" }, Acks(acts));
            return r;
        }

        static ElevatorMove Board(RobotRide r, out List<RideAction> acts, int n = 1)
        {
            acts = r.Turn(TurnMsg(n, "board"));
            return MoveOf(acts);
        }

        static List<RideAction> Boarded(RobotRide r, int n = 1)
        {
            var move = Board(r, out _, n);
            return r.MoveResult(move.Id, "done", "", 0.0);
        }

        static void AssertPath(List<double[]> path, params double[][] want)
        {
            Assert.AreEqual(want.Length, path.Count);
            for (int i = 0; i < want.Length; i++)
            {
                Assert.AreEqual(want[i][0], path[i][0], 1e-9);
                Assert.AreEqual(want[i][1], path[i][1], 1e-9);
            }
        }

        // ---- accept ----

        [Test]
        public void AcceptReportsWaitingWithTheRideAsTask()
        {
            var r = Accepted();
            Assert.AreEqual(new RideOverlay { Mode = "WAITING_ELEVATOR", TaskId = "R1", SilenceS = 0 }, r.Overlay());
            Assert.IsFalse(r.Silent);
            Assert.IsTrue(r.Holding);
            Assert.AreEqual("E1", r.Elevator);
        }

        [Test]
        public void AcceptSendsStateAndEventWhileTheLinkIsUp()
        {
            var r = new RobotRide("robot_1", "b", () => Wall);
            var acts = r.Accept("R1", Plan(), State(), new Spot(10.0, 0.0));
            CollectionAssert.AreEqual(new[] { RideKind.Ack, RideKind.Event, RideKind.State }, Kinds(acts));
            CollectionAssert.AreEqual(new[] { "ride_accepted" }, Events(acts));
            Assert.AreEqual(1, (int)Only(acts, RideKind.Event)[0].Body["seq"]);
        }

        [TestCase("noplan", "without a plan")]
        [TestCase("emptyplan", "without a plan")]
        [TestCase("badslot", "bad ride plan")]
        [TestCase("error", "ERROR")]
        [TestCase("floor2", "on floor 2")]
        [TestCase("far", "not at the lobby")]
        [TestCase("incar", "in a car")]
        [TestCase("unknown", "not known")]
        public void AcceptRefuses(string c, string why)
        {
            JObject p = Plan();
            RideRobotState s = State();
            Spot? pos = new Spot(10, 0);
            switch (c)
            {
                case "noplan": p = null; break;
                case "emptyplan": p = new JObject(); break; // Python: an empty dict is "no plan" too
                case "badslot": p["slot"] = "x"; break;
                case "error": s = State("ERROR", error: "boom"); break;
                case "floor2": s = State(floor: 2); break;
                case "far": pos = new Spot(10 + RobotRide.LobbyTol + 0.1, 0); break;
                case "incar": s = State(inCar: true); break;
                case "unknown": s = null; pos = null; break;
            }
            var r = new RobotRide("robot_1", "b");
            var acts = r.Accept("R1", p, s, pos);
            CollectionAssert.AreEqual(new[] { "R1 failed" }, Acks(acts));
            StringAssert.Contains(why, acts[0].Message);
            Assert.IsFalse(r.Holding);
        }

        [Test]
        public void NewRideReplacesAWaitingOne()
        {
            var r = Accepted();
            var acts = r.Accept("R2", Plan(p => p["trip"] = "E1-k3-2"), State(), new Spot(10, 0));
            CollectionAssert.AreEqual(new[] { "R1 cancelled", "R2 accepted" }, Acks(acts));
            CollectionAssert.AreEqual(new[] { "ride_cancelled", "ride_accepted" }, Events(acts));
            Assert.AreEqual("R2", r.CmdId);
            Assert.AreEqual("E1-k3-2", r.Trip);
            // Turns of the replaced trip are ignored.
            Assert.IsEmpty(r.Turn(TurnMsg(1, "board", trip: "E1-k3-1")));
        }

        [Test]
        public void RideRefusedOnceBoardingStarted()
        {
            var r = Accepted();
            Board(r, out _);
            var acts = r.Accept("R2", Plan(p => p["trip"] = "E1-k3-2"), State(), new Spot(10, 0));
            CollectionAssert.AreEqual(new[] { "R2 failed" }, Acks(acts));
            Assert.AreEqual("R1", r.CmdId);
        }

        // ---- cancel ----

        [Test]
        public void CancelForTheWaitingRide()
        {
            var r = Accepted();
            var acts = r.Cancel("R1.x99", "R1");
            CollectionAssert.AreEqual(new[] { "R1 cancelled", "R1.x99 accepted" }, Acks(acts));
            CollectionAssert.AreEqual(new[] { "ride_cancelled" }, Events(acts));
            Assert.AreEqual(RideKind.State, acts.Last().Kind);
            Assert.IsFalse(r.Holding);
            Assert.IsNull(r.Overlay());
        }

        [Test]
        public void LateCancelOfAnOldRideDoesNotDropTheNewOne()
        {
            var r = Accepted();
            r.Accept("R2", Plan(p => p["trip"] = "E1-k3-2"), State(), new Spot(10, 0));
            var acts = r.Cancel("R1.x5", "R1");
            CollectionAssert.AreEqual(new[] { "R1.x5 failed" }, Acks(acts));
            Assert.AreEqual("R2", r.CmdId);
            Assert.IsTrue(r.Holding);
        }

        [Test]
        public void CancelAfterBoardingStartedFails()
        {
            var r = Accepted();
            Board(r, out _);
            var acts = r.Cancel("R1.x5", "R1");
            CollectionAssert.AreEqual(new[] { "R1.x5 failed" }, Acks(acts));
            Assert.IsTrue(r.Holding);
        }

        [Test]
        public void CancelNotAboutRidesIsLeftToTheGotoLogic()
        {
            var r = new RobotRide("robot_1", "b");
            Assert.IsNull(r.Cancel("c9", "go7"));
            Assert.IsNull(r.Cancel("c9", null));
        }

        [Test]
        public void CancelWithACmdIdStartingWithXUsesTheTarget()
        {
            // The target is taken as is, never cut out of the cancel's cmd_id.
            var r = new RobotRide("robot_1", "b", () => Wall);
            r.Accept("t-1.0.1.xa1.R2", Plan(), State(), new Spot(10, 0));
            var acts = r.Cancel("t-1.0.1.xa1.R2.x1793", "t-1.0.1.xa1.R2");
            Assert.AreEqual("t-1.0.1.xa1.R2 cancelled", Acks(acts)[0]);
        }

        // ---- boarding ----

        [Test]
        public void BoardTurnAnnouncesSilenceThenEntersAndMoves()
        {
            var r = Accepted(p => p["slot"] = 2);
            var move = Board(r, out var acts);
            CollectionAssert.AreEqual(new[] { RideKind.Event, RideKind.State, RideKind.Zone, RideKind.Move }, Kinds(acts));
            CollectionAssert.AreEqual(new[] { "boarding" }, Events(acts)); // still on Wi-Fi
            Assert.AreEqual(new RideOverlay { Mode = "BOARDING", TaskId = "R1", SilenceS = 58 }, acts[1].Override);
            CollectionAssert.AreEqual(new[] { "1 board enter" }, Zones(acts));
            Assert.AreEqual("board", move.Op);
            AssertPath(move.Path, RobotRide.CarDoor(Lobby), RobotRide.CarSlot(Lobby, 2));
            Assert.IsTrue(r.Silent);
        }

        [Test]
        public void BoardedAnswersClearAndQueuesTheEvent()
        {
            var r = Accepted();
            var acts = Boarded(r);
            CollectionAssert.AreEqual(new[] { "1 board clear" }, Zones(acts));
            Assert.IsEmpty(Events(acts)); // no Wi-Fi: queued
            CollectionAssert.AreEqual(new[] { "boarded" }, r.Queue.Select(e => (string)e["event"]));
            Assert.AreEqual("RIDING", r.Overlay().Mode);
        }

        [Test]
        public void BoardFailureBringsTheLinkBackAtTheLobby()
        {
            var r = Accepted();
            var move = Board(r, out _);
            var acts = r.MoveResult(move.Id, "failed", "blocked", 0.0);
            CollectionAssert.AreEqual(new[] { "1 board failed" }, Zones(acts));
            Assert.AreEqual("blocked", (string)Only(acts, RideKind.Zone)[0].Body["reason"]);
            Assert.IsFalse(r.Silent);
            Assert.AreEqual("WAITING_ELEVATOR", r.Overlay().Mode);
            Assert.AreEqual(0, r.Overlay().SilenceS);
            CollectionAssert.Contains(Kinds(acts), RideKind.State);
            CollectionAssert.AreEqual(new[] { "board_failed" }, Events(acts));
        }

        [Test]
        public void TurnsForOthersOrOtherTripsAreIgnored()
        {
            var r = Accepted();
            Assert.IsEmpty(r.Turn(TurnMsg(1, "board", robot: "robot_2")));
            Assert.IsEmpty(r.Turn(TurnMsg(1, "board", trip: "E1-other")));
            Assert.IsEmpty(r.Turn(new JObject { ["trip"] = "E1-k3-1", ["robot"] = "robot_1" })); // malformed
            Assert.IsEmpty(new RobotRide("robot_1", "b").Turn(TurnMsg(1, "board"))); // no ride
        }

        [Test]
        public void SameNWhileMovingIsIgnoredAndAnsweredNIsRepeated()
        {
            var r = Accepted();
            var move = Board(r, out _);
            Assert.IsEmpty(r.Turn(TurnMsg(1, "board"))); // moving on it
            var clear = r.MoveResult(move.Id, "done", "", 0.0);
            var again = r.Turn(TurnMsg(1, "board"));
            Assert.AreEqual(1, again.Count);
            Assert.AreSame(Only(clear, RideKind.Zone)[0].Body, again[0].Body); // the same answer, same seq
        }

        [Test]
        public void NewNForABoardAlreadyDoneIsClearedWithoutMoving()
        {
            var r = Accepted();
            Boarded(r);
            var acts = r.Turn(TurnMsg(2, "board"));
            CollectionAssert.AreEqual(new[] { "2 board clear" }, Zones(acts));
            Assert.IsEmpty(Only(acts, RideKind.Move));
        }

        [Test]
        public void InjectedLostTurn()
        {
            var r = NewRide(ignoreTurns: 1, boot: "b");
            r.Accept("R1", Plan(), State(), new Spot(10, 0));
            Assert.IsEmpty(r.Turn(TurnMsg(1, "board")));
            CollectionAssert.AreEqual(new[] { "1 board enter" }, Zones(r.Turn(TurnMsg(1, "board")))); // the re-send gets through
        }

        // ---- riding and exiting ----

        [Test]
        public void RobotFollowsTheCarFloorWhileRiding()
        {
            var r = Accepted();
            Boarded(r);
            Assert.IsEmpty(r.CarState(JObject.Parse("{\"floor\":2,\"moving\":true}")));
            var acts = r.CarState(JObject.Parse("{\"floor\":3,\"moving\":false,\"door\":\"opening\"}"));
            Assert.AreEqual(1, acts.Count);
            Assert.AreEqual("floor", acts[0].Move.Op);
            Assert.AreEqual(3, acts[0].Move.Floor);
            Assert.IsEmpty(r.CarState(JObject.Parse("{\"floor\":3,\"moving\":false}")));
        }

        [Test]
        public void ExitToTheTurnsLobbyThenLinkBackThenDone()
        {
            var r = Accepted();
            Boarded(r);
            var lobby3 = new Spot(11.0, 1.0);
            var acts = r.Turn(TurnMsg(4, "exit", floor: 3, lobby: lobby3));
            CollectionAssert.AreEqual(new[] { "4 exit enter" }, Zones(acts));
            Assert.IsEmpty(Events(acts));
            var move = MoveOf(acts);
            Assert.AreEqual("exit", move.Op);
            Assert.AreEqual(3, move.Floor);
            AssertPath(move.Path, RobotRide.CarDoor(lobby3), new[] { 11.0, 1.0 });
            acts = r.MoveResult(move.Id, "done", "", 100.0);
            CollectionAssert.AreEqual(new[] { "4 exit clear" }, Zones(acts));
            Assert.AreEqual(3, (int)Only(acts, RideKind.Zone)[0].Body["floor"]);
            Assert.IsTrue(r.Silent);
            Assert.IsEmpty(r.Tick(100.0 + RobotRide.LinkBackS - 0.1, 3));
            acts = r.Tick(100.0 + RobotRide.LinkBackS, 3);
            // pose first (new floor), then the state without silence, the queued events in order, then done.
            CollectionAssert.AreEqual(new[] { RideKind.Pose, RideKind.State, RideKind.Event, RideKind.Event, RideKind.Ack }, Kinds(acts));
            CollectionAssert.AreEqual(new[] { "boarded", "exited" }, Events(acts));
            var seqs = Only(acts, RideKind.Event).Select(a => (long)a.Body["seq"]).ToList();
            CollectionAssert.IsOrdered(seqs);
            var ack = acts.Last();
            Assert.AreEqual(("R1", "done", "", (int?)3), (ack.CmdId, ack.Status, ack.Message, ack.Floor));
            Assert.IsFalse(r.Silent);
            Assert.IsFalse(r.Holding);
            Assert.IsNull(r.Overlay());
        }

        [Test]
        public void ExitFailureGoesBackToTheSlotAndWaits()
        {
            var r = Accepted();
            Boarded(r);
            var move = MoveOf(r.Turn(TurnMsg(4, "exit", floor: 3)));
            var acts = r.MoveResult(move.Id, "failed", "door zone blocked", 0.0);
            CollectionAssert.AreEqual(new[] { "4 exit failed" }, Zones(acts));
            Assert.AreEqual("RIDING", r.Overlay().Mode);
            Assert.IsTrue(r.Silent);
            Assert.IsEmpty(r.Tick(RobotRide.TripLostS - 1, 3)); // never leaves by itself while the trip goes on
            // The failed answer is repeated for the same n; the retry (new n) moves again.
            CollectionAssert.AreEqual(new[] { "4 exit failed" }, Zones(r.Turn(TurnMsg(4, "exit", floor: 3))));
            acts = r.Turn(TurnMsg(5, "exit", floor: 3));
            CollectionAssert.AreEqual(new[] { "5 exit enter" }, Zones(acts));
            // Carried: exit at 5 although the plan said 3.
            r.MoveResult(MoveOf(acts).Id, "failed", "door zone blocked", 0.0);
            r.CarState(JObject.Parse("{\"floor\":5,\"moving\":false}"));
            move = MoveOf(r.Turn(TurnMsg(6, "exit", floor: 5)));
            Assert.AreEqual(5, move.Floor);
            r.MoveResult(move.Id, "done", "", 10.0);
            var done = r.Tick(10.0 + RobotRide.LinkBackS, 5);
            Assert.AreEqual("R1 done 5", $"{done.Last().CmdId} {done.Last().Status} {done.Last().Floor}");
            CollectionAssert.AreEqual(new[] { "boarded", "exit_failed", "exit_failed", "exited" }, Events(done));
        }

        [Test]
        public void TurnsAfterDoneAreIgnoredButExitAgainBeforeDoneIsCleared()
        {
            var r = Accepted();
            Boarded(r);
            var move = MoveOf(r.Turn(TurnMsg(4, "exit", floor: 3)));
            r.MoveResult(move.Id, "done", "", 0.0);
            // Out but Wi-Fi not back yet: a new n for the same exit is answered clear.
            CollectionAssert.AreEqual(new[] { "5 exit clear" }, Zones(r.Turn(TurnMsg(5, "exit", floor: 3))));
            r.Tick(RobotRide.LinkBackS, 3);
            Assert.IsEmpty(r.Turn(TurnMsg(6, "exit", floor: 3))); // done sent: not ours any more
        }

        [Test]
        public void StaleMoveReportsAreIgnored()
        {
            var r = Accepted();
            var move = Board(r, out _);
            Assert.IsEmpty(r.MoveResult(move.Id - 1, "done", "", 0.0));
            Assert.IsEmpty(r.MoveResult(move.Id, "running", "", 0.0));
            CollectionAssert.AreEqual(new[] { "1 board clear" }, Zones(r.MoveResult(move.Id, "done", "", 0.0)));
            Assert.IsEmpty(r.MoveResult(move.Id, "done", "", 0.0)); // reported again
        }

        [Test]
        public void EventSeqIncreasesAcrossRides()
        {
            var r = Accepted();
            Boarded(r);
            var move = MoveOf(r.Turn(TurnMsg(2, "exit", floor: 3)));
            r.MoveResult(move.Id, "done", "", 0.0);
            var first = Only(r.Tick(RobotRide.LinkBackS, 3), RideKind.Event).Select(a => (long)a.Body["seq"]).ToList();
            var acts = r.Accept("R2", Plan(p => { p["trip"] = "E1-k3-9"; p["origin"] = 3; }), State(floor: 3), new Spot(10, 0));
            var second = Only(acts, RideKind.Event).Select(a => (long)a.Body["seq"]).ToList();
            Assert.AreEqual(first.Max() + 1, second[0]);
            Assert.That(Only(acts, RideKind.Event).Select(a => (string)a.Body["boot"]), Is.All.EqualTo("boot1"));
        }

        // ---- review 6b ----

        static void ExitFailedOnce(RobotRide r, int n = 4, int floor = 3)
        {
            var move = MoveOf(r.Turn(TurnMsg(n, "exit", floor: floor)));
            r.MoveResult(move.Id, "failed", "blocked", 0.0);
        }

        [Test]
        public void ReviewOldNIsNotAnsweredWhileMovingOnANewN()
        {
            // Review 6b-3 (was: a late copy of turn 4 re-sent "failed" for 4 while the
            // robot was in the door zone for 5; the car then dropped it from door_zone).
            var r = Accepted();
            Boarded(r);
            ExitFailedOnce(r);
            var acts = r.Turn(TurnMsg(5, "exit", floor: 3));
            CollectionAssert.AreEqual(new[] { "5 exit enter" }, Zones(acts));
            Assert.IsTrue(r.Moving);
            Assert.IsEmpty(r.Turn(TurnMsg(4, "exit", floor: 3)));
            Assert.IsEmpty(r.Turn(TurnMsg(5, "exit", floor: 3)));
            r.MoveResult(MoveOf(acts).Id, "done", "", 0.0);
            // Once out of the zone the old answer may be repeated again.
            CollectionAssert.AreEqual(new[] { "4 exit failed" }, Zones(r.Turn(TurnMsg(4, "exit", floor: 3))));
        }

        [Test]
        public void ReviewTripReleasedWithTheRobotInside()
        {
            // Review 6b-1 (was: silent forever without an exit turn).
            var r = Accepted();
            Boarded(r);
            r.CarState(JObject.Parse("{\"floor\":5,\"moving\":false}"));
            Assert.IsEmpty(r.TripMsg(JObject.Parse("{\"trip\":\"E1-k3-1\",\"stage\":\"blocked\"}"), 5.0)); // still running
            var acts = r.TripMsg(JObject.Parse("{\"trip\":\"E1-k3-1\",\"stage\":\"done\",\"detail\":\"released by operator\"}"), 6.0);
            Assert.IsFalse(r.Silent);
            Assert.IsFalse(r.Holding);
            Assert.AreEqual("E1", r.Stranded);
            CollectionAssert.AreEqual(new[] { RideKind.Pose, RideKind.State, RideKind.Event, RideKind.Event, RideKind.Ack }, Kinds(acts));
            CollectionAssert.AreEqual(new[] { "boarded", "exit_failed" }, Events(acts));
            StringAssert.Contains("released by operator", (string)Only(acts, RideKind.Event).Last().Body["detail"]);
            var ack = acts.Last();
            Assert.AreEqual("R1 failed 5", $"{ack.CmdId} {ack.Status} {ack.Floor}");
            // Until a human takes it out it reports ERROR, then "rescued" and its own state.
            var ov = r.Overlay(new RideRobotState { InCar = true });
            Assert.AreEqual("ERROR", ov.Mode);
            StringAssert.Contains("needs a human", ov.Error);
            Assert.AreEqual("", ov.TaskId);
            Assert.IsEmpty(r.Tick(10.0, 5, new RideRobotState { InCar = true }));
            acts = r.Tick(11.0, 5, new RideRobotState { InCar = false });
            CollectionAssert.AreEqual(new[] { "rescued" }, Events(acts));
            CollectionAssert.AreEqual(new[] { RideKind.Event, RideKind.State }, Kinds(acts));
            Assert.IsNull(r.Overlay(new RideRobotState { InCar = false }));
            Assert.AreEqual("", r.Stranded);
            // A new ride is refused while the robot says it is in a car.
            Assert.AreEqual("R9 failed", Acks(r.Accept("R9", Plan(), State(inCar: true), new Spot(10, 0)))[0]);
        }

        [TestCase(null)]
        [TestCase("{\"trip\":\"E1-k3-2\",\"stage\":\"planned\"}")]
        [TestCase("{\"trip\":\"E1-k3-1\",\"stage\":\"aborted\"}")]
        public void TripClearedOrReplacedLeavesTheRobotStranded(string msg)
        {
            var r = Accepted();
            Boarded(r);
            var acts = r.TripMsg(msg == null ? null : JObject.Parse(msg), 1.0);
            CollectionAssert.AreEqual(new[] { "R1 failed" }, Acks(acts));
            Assert.AreEqual("E1", r.Stranded);
            Assert.IsFalse(r.Silent);
        }

        [Test]
        public void TripEndBeforeBoardingIsLeftToTheCancel()
        {
            var r = Accepted();
            Assert.IsEmpty(r.TripMsg(null, 1.0));
            Assert.AreEqual("waiting", r.Stage);
        }

        [Test]
        public void TripEndDuringAMoveWaitsForTheMove()
        {
            var r = Accepted();
            Boarded(r);
            var move = MoveOf(r.Turn(TurnMsg(4, "exit", floor: 3)));
            Assert.IsEmpty(r.TripMsg(null, 1.0)); // in the door zone: finish the move first
            var acts = r.MoveResult(move.Id, "failed", "blocked", 2.0);
            CollectionAssert.AreEqual(new[] { "4 exit failed" }, Zones(acts));
            CollectionAssert.AreEqual(new[] { "R1 failed" }, Acks(acts));
            // ... but a successful exit is a normal end.
            var r2 = Accepted();
            Boarded(r2);
            move = MoveOf(r2.Turn(TurnMsg(4, "exit", floor: 3)));
            r2.TripMsg(null, 1.0);
            r2.MoveResult(move.Id, "done", "", 2.0);
            var last = r2.Tick(2.0 + RobotRide.LinkBackS, 3).Last();
            Assert.AreEqual("R1 done 3", $"{last.CmdId} {last.Status} {last.Floor}");
            Assert.AreEqual("", r2.Stranded);
        }

        [Test]
        public void TripNotHeardForALongTimeIsGone()
        {
            var r = Accepted();
            Boarded(r);
            r.TripMsg(JObject.Parse("{\"trip\":\"E1-k3-1\",\"stage\":\"travel\"}"), 50.0); // republished: still there
            Assert.IsEmpty(r.Tick(50.0 + RobotRide.TripLostS - 1, 3, new RideRobotState { InCar = true }));
            var acts = r.Tick(50.0 + RobotRide.TripLostS + 1, 3, new RideRobotState { InCar = true });
            CollectionAssert.AreEqual(new[] { "R1 failed" }, Acks(acts));
            StringAssert.Contains("nothing heard", acts.Last().Message);
        }

        [Test]
        public void RescuedWhileTheTripRunsComesOutDone()
        {
            var r = Accepted();
            Boarded(r);
            var acts = r.Tick(5.0, 4, new RideRobotState { InCar = false }); // a human took it out on floor 4
            Assert.AreEqual("R1 done 4", $"{acts.Last().CmdId} {acts.Last().Status} {acts.Last().Floor}");
            CollectionAssert.AreEqual(new[] { "boarded", "rescued" }, Events(acts));
            CollectionAssert.AreEqual(new[] { RideKind.Pose, RideKind.State }, Kinds(acts).Take(2));
            Assert.IsFalse(r.Silent);
            Assert.AreEqual("", r.Stranded);
        }

        [Test]
        public void ReviewBoardMoveNeverReportedFails()
        {
            // Review 6b-2 (was: BOARDING + silent forever).
            var r = Accepted();
            var acts = r.Turn(TurnMsg(1, "board"), 100.0);
            var move = MoveOf(acts);
            double deadline = 100.0 + 15 + RobotRide.MoveExtraS;
            Assert.IsEmpty(r.Tick(deadline - 0.1, 1, new RideRobotState { Move = null }));
            acts = r.Tick(deadline, 1, new RideRobotState { Move = new MoveReport { Id = move.Id - 5, Status = "done" } }); // robot restarted: other move
            CollectionAssert.AreEqual(new[] { "1 board failed" }, Zones(acts));
            Assert.AreEqual("robot did not finish the move", (string)Only(acts, RideKind.Zone)[0].Body["reason"]);
            Assert.IsFalse(r.Silent);
            Assert.AreEqual("WAITING_ELEVATOR", r.Overlay().Mode);
            CollectionAssert.AreEqual(new[] { "board_failed" }, Events(acts));
            // The re-sent turn gets the same answer; a late report of the move is ignored.
            CollectionAssert.AreEqual(new[] { "1 board failed" }, Zones(r.Turn(TurnMsg(1, "board"), deadline + 1)));
            Assert.IsEmpty(r.MoveResult(move.Id, "done", "", deadline + 2));
        }

        [Test]
        public void SlowMoveStillRunningIsWaitedFor()
        {
            var r = Accepted();
            var move = MoveOf(r.Turn(TurnMsg(1, "board"), 0.0));
            var running = new RideRobotState { Move = new MoveReport { Id = move.Id, Status = "running" } };
            for (int t = 45; t < 200; t += 5)
                Assert.IsEmpty(r.Tick(t, 1, running)); // in the door zone: no answer
            CollectionAssert.AreEqual(new[] { "1 board clear" }, Zones(r.MoveResult(move.Id, "done", "", 200.0)));
        }

        [Test]
        public void ExitMoveTimeoutAnswersFailedAndWaitsInTheSlot()
        {
            var r = Accepted();
            Boarded(r);
            r.TripMsg(JObject.Parse("{\"trip\":\"E1-k3-1\",\"stage\":\"exiting\"}"), 0.0);
            r.Turn(TurnMsg(4, "exit", floor: 3), 0.0);
            var acts = r.Tick(15 + RobotRide.MoveExtraS, 3, null); // robot silent on ROS
            CollectionAssert.AreEqual(new[] { "4 exit failed" }, Zones(acts));
            Assert.AreEqual("riding", r.Stage);
            Assert.IsTrue(r.Silent);
        }

        [Test]
        public void ReviewExitTurnAtTheLobbyIsAnsweredFailed()
        {
            var r = Accepted();
            var acts = r.Turn(TurnMsg(3, "exit", floor: 1));
            CollectionAssert.AreEqual(new[] { "3 exit failed" }, Zones(acts));
            Assert.AreEqual("not in the car", (string)Only(acts, RideKind.Zone)[0].Body["reason"]);
            Assert.IsEmpty(Only(acts, RideKind.Move));
            Assert.IsFalse(r.Silent);
        }
    }
}
