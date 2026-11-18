// The rest of the 3D dashboard's pure logic: car slots, riders and the car
// box (cases from scripts/web_test.js 192-238), the car's continuous height,
// robot colours, grid lines, bounds, back-off and the command line.
using System.Collections.Generic;
using System.Linq;
using Fleet.Dashboard;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class DashboardMapTests
    {
        static Rider Rd(string robot, string status, int slot, int exit, int? dest = null) =>
            new Rider { Robot = robot, Status = status, Slot = slot, Exit = exit, Dest = dest ?? exit, Board = 2 - slot };

        static ElevatorView Lift() => new ElevatorView
        {
            Id = "E1", Capacity = 3, Floors = new List<int> { 1, 2, 3, 4, 5, 6, 7 }, Lobby = new Point2(10, 0), Responding = true,
            State = new CarState { Floor = 5, Dir = "idle", Door = "open", Moving = false, Hold = true, Trip = "E1-k3-1", Load = 2 },
            Trip = new Trip
            {
                Id = "E1-k3-1", Stage = "exiting", Floor = 5, Stops = new List<int> { 3, 5, 7 },
                Riders = new List<Rider> { Rd("robot_2", "inside", 2, 7), Rd("robot_3", "exiting", 1, 5), Rd("robot_1", "out", 0, 3) },
            },
        };

        [Test]
        public void CarSlotsByDoorWithOutRobotsLeftOut()
        {
            var s = Elevators.CarSlots(Lift());
            Assert.AreEqual(3, s.Length);
            Assert.IsNull(s[0]); // robot_1 is out
            Assert.AreEqual("robot_3", s[1].Robot);
            Assert.AreEqual("exiting", s[1].Status);
            Assert.IsFalse(s[1].Carried);
            Assert.AreEqual("robot_2", s[2].Robot);
        }

        [Test]
        public void CarSlotsCarriedBadSlotAndNoTrip()
        {
            var v = Lift();
            v.Trip.Riders = new List<Rider> { Rd("robot_1", "inside", 0, 5, 3), Rd("robot_9", "inside", 7, 5), Rd("robot_8", "waiting", 1, 5) };
            var s = Elevators.CarSlots(v);
            Assert.IsTrue(s[0].Carried && s[0].Exit == 5 && s[0].Dest == 3);
            Assert.IsNull(s[1]);
            Assert.IsNull(s[2]);
            var empty = Lift();
            empty.Trip = null;
            Assert.AreEqual(3, Elevators.CarSlots(empty).Count(x => x == null));
            Assert.AreEqual(0, Elevators.CarSlots(null).Length);
        }

        [Test]
        public void RidersInCarsAtTheCarsFloor()
        {
            var e2 = Lift();
            e2.Id = "E2";
            e2.State = null;
            e2.Trip = new Trip { Id = "E2-x-1", Floor = 2, Riders = new List<Rider> { Rd("r9", "boarding", 0, 4) } };
            var m = Elevators.RidersInCars(new[] { Lift(), e2 });
            CollectionAssert.AreEquivalent(new[] { "r9", "robot_2", "robot_3" }, m.Keys);
            Assert.AreEqual("E1", m["robot_2"].Car);
            Assert.AreEqual(2, m["robot_2"].Slot);
            Assert.AreEqual(5, m["robot_2"].Floor);
            Assert.AreEqual(2, m["r9"].Floor); // no state yet: the trip's floor
            foreach (var st in new[] { "unsure_in", "stuck" })
            {
                var v = Lift();
                v.Trip.Riders = new List<Rider> { Rd("robot_1", st, 0, 3) };
                Assert.IsTrue(Elevators.RidersInCars(new[] { v }).ContainsKey("robot_1"), st);
            }
        }

        [Test]
        public void CarBoxDoorNextToTheLobbySlotZeroAtTheDoor()
        {
            var b = Elevators.Box(new Point2(10, 0), 3);
            Assert.AreEqual(11, b.X0, 1e-9);
            var s = Enumerable.Range(0, 3).Select(b.Slot).ToList();
            Assert.Less(s[0].X, s[1].X);
            Assert.Less(s[1].X, s[2].X);
            foreach (var p in s) Assert.IsTrue(p.X > b.X0 && p.X < b.X1 && p.Y > b.Y0 && p.Y < b.Y1);
            // same numbers as the adapter drives the robots to (fleet_logic.car_slot)
            Assert.AreEqual(11.8, s[0].X, 1e-9);
            Assert.AreEqual(13.4, s[2].X, 1e-9);
        }

        [Test]
        public void ParsesGetElevators()
        {
            var json = "[{\"id\":\"E1\",\"capacity\":3,\"floors\":[1,2,3],\"lobby\":{\"x\":10,\"y\":0}," +
                       "\"state\":{\"boot\":\"car-7\",\"seq\":812,\"floor\":2,\"dir\":\"up\",\"door\":\"closed\",\"moving\":true,\"hold\":false," +
                       "\"trip\":\"E1-a-1\",\"load\":1,\"cmd_id\":\"x\",\"door_zone\":0},\"heard\":\"2026-11-03T09:00:40.123456789+09:00\"," +
                       "\"responding\":true,\"trip\":{\"trip\":\"E1-a-1\",\"stage\":\"travel\",\"floor\":3,\"stops\":[3]," +
                       "\"riders\":[{\"robot\":\"robot_1\",\"dest\":3,\"exit\":3,\"board\":0,\"slot\":0,\"status\":\"inside\",\"accepted\":true}]," +
                       "\"planned\":\"2026-11-03T09:00:03Z\",\"lease\":\"2026-11-03T09:02:30Z\"},\"waiting\":[]}," +
                       "{\"id\":\"E2\",\"capacity\":2,\"floors\":[1,2],\"lobby\":{\"x\":0,\"y\":5},\"state\":null,\"responding\":false,\"trip\":null,\"waiting\":[]}]";
            var v = Elevators.Parse(json);
            Assert.AreEqual(2, v.Count);
            Assert.AreEqual(10, v[0].Lobby.X);
            Assert.IsTrue(v[0].State.Moving);
            Assert.AreEqual("travel", v[0].Trip.Stage);
            Assert.AreEqual("robot_1", v[0].Trip.Riders[0].Robot);
            Assert.IsNull(v[1].State);
            Assert.IsNull(v[1].Trip);
        }

        [Test]
        public void FloorsToDrawJoinsCarsRobotsAndActiveTasks()
        {
            var robots = new[] { new RobotView { HasPose = true, Floor = 12 }, new RobotView { HasPose = false, Floor = 40 } };
            var tasks = new[]
            {
                new TaskView { State = "QUEUED", Waypoints = { new TaskView.Waypoint { Floor = -1 } } },
                new TaskView { State = "DONE", Waypoints = { new TaskView.Waypoint { Floor = 30 } } },
            };
            var f = Elevators.FloorsToDraw(new[] { Lift() }, robots, tasks);
            CollectionAssert.AreEqual(new[] { -1, 1, 2, 3, 4, 5, 6, 7, 12 }, f);
            Assert.AreEqual(1, TaskView.CountActive(tasks));
        }

        // ---- CarMotion ----

        static CarState S(int floor, string dir, bool moving) => new CarState { Floor = floor, Dir = dir, Moving = moving };

        [Test]
        public void CarMotionHoldsAStoppedCarOnItsFloor()
        {
            var c = new CarMotion();
            Assert.IsNaN(c.Position(0));
            c.Observe(S(3, "idle", false), 0);
            Assert.AreEqual(3, c.Position(5000));
        }

        [Test]
        public void CarMotionMovesBetweenFloorsAndStopsShortOfTheNext()
        {
            var c = new CarMotion { FloorMs = 2000 };
            c.Observe(S(1, "idle", false), 0);
            c.Observe(S(1, "up", true), 1000);              // starts
            Assert.AreEqual(1.5, c.Position(2000), 1e-9);
            Assert.AreEqual(1.95, c.Position(9000), 1e-9);  // never past the next floor unheard
            c.Observe(S(2, "up", true), 3000);              // passed 2: one 2 s sample
            Assert.AreEqual(2.25, c.Position(3500), 1e-9);
            c.Observe(S(3, "up", true), 4600);              // faster: 1.6 s
            Assert.AreEqual(1800, c.FloorMs, 1e-9);
            c.Observe(S(3, "idle", false), 5000);
            Assert.AreEqual(3, c.Position(5100));
        }

        [Test]
        public void CarMotionGoesDownToo()
        {
            var c = new CarMotion { FloorMs = 2000 };
            c.Observe(S(4, "down", true), 0);
            Assert.AreEqual(3.5, c.Position(1000), 1e-9);
        }

        // ---- colours, grid, bounds, back-off, args ----

        [Test]
        public void RobotColourFollowsTheLegendPrecedence()
        {
            Assert.AreEqual(Look.Offline, RobotStyle.Of("OFFLINE", "MOVING", "e"));
            Assert.AreEqual(Look.Elevator, RobotStyle.Of("IN_ELEVATOR", "IDLE", ""));
            Assert.AreEqual(Look.Idle, RobotStyle.Of("STALE", "MOVING", ""));
            Assert.AreEqual(Look.Error, RobotStyle.Of("ONLINE", "MOVING", "bumper"));
            Assert.AreEqual(Look.Error, RobotStyle.Of("ONLINE", "ERROR", ""));
            Assert.AreEqual(Look.Moving, RobotStyle.Of("ONLINE", "MOVING", ""));
            Assert.AreEqual(Look.Charging, RobotStyle.Of("ONLINE", "DOCKING", ""));
            Assert.AreEqual(Look.Charging, RobotStyle.Of("ONLINE", "CHARGING", ""));
            Assert.AreEqual(Look.Elevator, RobotStyle.Of("ONLINE", "WAITING_ELEVATOR", ""));
            Assert.AreEqual(Look.Idle, RobotStyle.Of("ONLINE", "IDLE", ""));
            Assert.AreEqual(0x8a4fd6, RobotStyle.Rgb(Look.Elevator));
        }

        [Test]
        public void GridLinesLikeTheTwoDMap()
        {
            // scripts/web_test.js 62-83
            var g = MapGrid.Lines(-2.5, 7.2, 1);
            CollectionAssert.AreEqual(new double[] { -2, -1, 0, 1, 2, 3, 4, 5, 6, 7 }, g.Select(l => l.v));
            CollectionAssert.AreEqual(new[] { 0.0, 5.0 }, g.Where(l => l.major).Select(l => l.v));
            Assert.LessOrEqual(MapGrid.Lines(0, 100000, 1).Count, MapGrid.MaxLines + 2);
            Assert.LessOrEqual(MapGrid.Lines(1e15, 1e15 + 1000, 1).Count, MapGrid.MaxLines + 2);
            Assert.AreEqual(0, MapGrid.Lines(double.NaN, 1, 1).Count);
            Assert.AreEqual(0, MapGrid.Lines(5, 1, 1).Count);
        }

        [Test]
        public void BoundsGrowOnlyAndKeepAMinimumSpan()
        {
            var b = new Bounds2();
            Assert.AreEqual((-5.0, -5.0, 5.0, 5.0), b.Box());
            b.Add(0, 0);
            Assert.AreEqual((-5.0, -5.0, 5.0, 5.0), b.Box());
            b.Add(20, 1);
            var box = b.Box();
            Assert.AreEqual(-2, box.minX, 1e-9);
            Assert.AreEqual(22, box.maxX, 1e-9);
            int v = b.Version;
            b.Add(10, 0);                 // inside: no growth
            Assert.AreEqual(v, b.Version);
            b.Add(1e9, 0);                // beyond MAX_COORD: ignored
            Assert.AreEqual(22, b.Box().maxX, 1e-9);
        }

        [Test]
        public void BackoffDoublesToTenSecondsWithJitterAndResets()
        {
            var b = new Backoff();
            Assert.AreEqual(500 * 0.75, b.Next(0), 1e-9);
            Assert.AreEqual(1000 * 1.25, b.Next(1), 1e-9);
            Assert.AreEqual(2000, b.Next(0.5), 1e-9);
            for (int i = 0; i < 10; i++) b.Next(0.5);
            Assert.AreEqual(10000, b.Next(0.5), 1e-9);
            b.Reset();
            Assert.AreEqual(500, b.Next(0.5), 1e-9);
        }

        [Test]
        public void DashArgsParse()
        {
            var a = DashArgs.Parse(new[] { "-batchmode", "-api", "127.0.0.1:9090/", "-floorHeight", "4", "-capture", "C:\\cap",
                "-captureMs", "250", "-captureSeconds", "70", "-captureQuit", "-follow", "-focus", "2", "-screen-width", "1280" });
            Assert.AreEqual("http://127.0.0.1:9090", a.Api);
            Assert.AreEqual("ws://127.0.0.1:9090/ws/live", a.LiveUrl);
            Assert.AreEqual(4, a.FloorHeight);
            Assert.AreEqual("C:\\cap", a.CaptureDir);
            Assert.AreEqual(250, a.CaptureMs);
            Assert.AreEqual(70, a.CaptureSeconds);
            Assert.IsTrue(a.CaptureQuit && a.Follow);
            Assert.AreEqual(2, a.Focus);
            Assert.AreEqual(0, a.Warnings.Count);
            var d = DashArgs.Parse(new[] { "-floorHeight", "x" });
            Assert.AreEqual(5, d.FloorHeight);
            Assert.AreEqual(1, d.Warnings.Count);
            Assert.AreEqual("ws://127.0.0.1:8080/ws/live", d.LiveUrl);
            Assert.AreEqual("wss://fleet.example/ws/live", DashArgs.Parse(new[] { "-api", "https://fleet.example" }).LiveUrl);
        }

        [Test]
        public void EventsDescribeInEnglish()
        {
            var e = FleetEvent.Parse("[{\"seq\":3,\"time\":\"2026-11-18T08:16:42.5Z\",\"robot\":\"robot_1\",\"type\":\"conn\",\"from\":\"ONLINE\",\"to\":\"IN_ELEVATOR\"}]");
            Assert.AreEqual("robot_1 conn ONLINE -> IN_ELEVATOR", e[0].Describe());
        }
    }
}
