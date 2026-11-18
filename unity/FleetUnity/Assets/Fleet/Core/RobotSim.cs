// What a fleet server can see of a robot: kinematic straight-line motion,
// battery, mode, dock, faults and elevator moves. Mirrors
// mock_robots/mock_fleet.py so the two robot kinds behave alike; no map, no
// obstacles. Coordinates are the fleet's map frame (x, y in metres, yaw in
// radians, counter-clockwise from +x).
//
// Elevator moves (docs/ELEVATOR.md 5, mock_fleet.py on_elevator_move) come
// from RobotRide, like the adapter's /<robot>/elevator_move:
//   board  lobby -> door -> slot, then in the car
//   floor  the car stands at another floor: the robot (in the car) is there
//   exit   slot -> door -> the turn's lobby on floor F (the floor is set when the move starts)
// LastMove is what the mock reports as robot_state "move".
using System;
using System.Collections.Generic;

namespace Fleet.Core
{
    /// <summary>The robot's report of its last elevator move (mock robot_state "move").</summary>
    public sealed class MoveReport
    {
        public long Id;
        public string Op;
        public string Status;   // running | done | failed
        public string Detail = "";
    }

    public sealed class RobotSim
    {
        public const double ArriveTol = 0.05;   // m, mock_fleet.ARRIVE_TOL

        public double X, Y, Yaw;
        public double V;                         // m/s actually driven in the last step
        public double Speed = 0.8;               // m/s
        public int Floor = 1;
        public string Mode = Modes.Idle;
        public string Error = "";
        public double DockX, DockY;              // where dock drives to (the spawn point)

        double battery = 100.0;
        public double Battery { get => battery; set => battery = Clamp(value); }
        public double DrainPerM = 0.05;          // % per metre driven (mock default)
        public double DrainIdle = 0.002;         // % per second while powered on
        public double ChargeRate = 1.0;          // % per second on the dock
        // Charging stops here and the robot goes IDLE. It must be at least
        // fleetd's ResumeBattery (90) or the battery policy sends it back.
        public double FullAt = 100.0;

        public bool HasGoal { get; private set; }
        public double GoalX { get; private set; }
        public double GoalY { get; private set; }
        double goalYaw;

        // ---- elevator (mock_fleet.py stage 6) ----
        public bool InCar { get; private set; }
        public MoveReport LastMove { get; private set; }
        public bool InMove => emove != null;
        // Failure injection, as mock_fleet.py --exit-fail-floor/--exit-fail-count/--door-move-s.
        public int? ExitFailFloor;
        public int ExitFailCount;                // exits that still fail there (-1: always)
        public double DoorMoveS;                 // every board/exit takes at least this long
        double clock;                            // s of simulated time (the mock uses its monotonic clock)
        double carLobbyX, carLobbyY;             // where a rescue puts it (the lobby it boarded from)

        sealed class Emove
        {
            public long Id;
            public string Op;
            public List<double[]> Path;
            public bool Fail;
            public double Speed, Until, StartX, StartY;
        }
        Emove emove;

        public RobotSim(double x, double y, double battery)
        {
            X = DockX = carLobbyX = x;
            Y = DockY = carLobbyY = y;
            Battery = battery;
        }

        /// <summary>Starts driving to (x, y). Returns null, or why the goal is refused.</summary>
        public string Goto(double x, double y, double yaw)
        {
            if (Mode == Modes.Error) return $"in ERROR ({Error})";
            if (battery <= 0.0) return "battery empty";
            if (InCar || emove != null) return "in the elevator";
            SetGoal(x, y, yaw);
            Mode = Modes.Moving;
            return null;
        }

        /// <summary>Stops a drive or a dock. Returns null, or why there was nothing to stop.</summary>
        public string Stop()
        {
            if (emove != null || InCar) return "in the elevator";
            if (Mode != Modes.Moving && Mode != Modes.Docking)
                return $"nothing to cancel ({Mode})";
            HasGoal = false;
            Mode = Modes.Idle;
            V = 0;
            return null;
        }

        /// <summary>Drives to the dock point, then charges. Returns null, or why not.</summary>
        public string Dock()
        {
            if (Mode == Modes.Error) return $"in ERROR ({Error})";
            if (emove != null || InCar) return "in the elevator";
            SetGoal(DockX, DockY, 0.0);
            Mode = Modes.Docking;
            return null;
        }

        public string ResetError()
        {
            if (Mode != Modes.Error) return "not in ERROR";
            if (emove != null)
            {
                // Stopped by the fault in the door zone: a human pushes it back to
                // where the move started, and only then is the move over (mock on_reset_error).
                var e = emove;
                X = e.StartX; Y = e.StartY;
                emove = null;
                Report(e.Id, e.Op, "failed", $"fault in the door zone: {Error}");
            }
            Mode = Modes.Idle;
            Error = "";
            HasGoal = false;
            return null;
        }

        /// <summary>A human takes the robot out of the car to the lobby it boarded
        /// from, on the floor the car is at (mock on_rescue). Null, or why not.</summary>
        public string Rescue()
        {
            if (!InCar) return "not in a car";
            if (emove != null)
            {
                var e = emove;
                emove = null;
                Report(e.Id, e.Op, "failed", "taken out by a human");
            }
            X = carLobbyX; Y = carLobbyY;
            InCar = false;
            HasGoal = false;
            if (Mode == Modes.Moving) Mode = Modes.Idle;
            V = 0;
            return null;
        }

        /// <summary>A fault. During an elevator move the robot stops where it is,
        /// maybe in the door zone: the move stays "running" until ResetError.</summary>
        public void Fail(string reason)
        {
            Mode = Modes.Error;
            Error = reason;
            HasGoal = false;
            V = 0;
        }

        /// <summary>An elevator move from RobotRide (mock on_elevator_move).</summary>
        public void StartMove(ElevatorMove m)
        {
            if (m == null) return;
            if (LastMove != null && LastMove.Id == m.Id) return; // same move again
            if (m.Op == "floor")
            {
                if (InCar && m.Floor.HasValue) Floor = m.Floor.Value;
                LastMove = new MoveReport { Id = m.Id, Op = m.Op, Status = "done" };
                return;
            }
            if (emove != null) { Report(m.Id, m.Op, "failed", $"busy with move {emove.Id}"); return; }
            if (Mode == Modes.Error) { Report(m.Id, m.Op, "failed", $"in ERROR ({Error})"); return; }
            if (m.Op == "board" && InCar || m.Op == "exit" && !InCar || (m.Op != "board" && m.Op != "exit"))
            {
                Report(m.Id, m.Op, "failed", $"cannot {m.Op} (in_car={(InCar ? "True" : "False")})");
                return;
            }
            if (m.Path == null || m.Path.Count == 0) { Report(m.Id, m.Op, "failed", "bad path"); return; }
            var path = new List<double[]>();
            foreach (var p in m.Path) path.Add(new[] { p[0], p[1] });
            if (m.Op == "exit" && m.Floor.HasValue)
                Floor = m.Floor.Value; // the car is at that floor
            double sx = X, sy = Y;
            bool fail = m.Op == "exit" && ExitFailFloor.HasValue && ExitFailFloor.Value == Floor && ExitFailCount != 0;
            if (fail)
            {
                // Something blocks the door: get into the door zone, stop, back off to the slot.
                var door = path[0];
                path = new List<double[]> { new[] { sx + 0.6 * (door[0] - sx), sy + 0.6 * (door[1] - sy) }, new[] { sx, sy } };
                if (ExitFailCount > 0) ExitFailCount--;
            }
            double length = 0, px = sx, py = sy;
            foreach (var p in path) { length += Throttle.Hypot(p[0] - px, p[1] - py); px = p[0]; py = p[1]; }
            double speed = Speed;
            if (DoorMoveS > 0 && length > 0) speed = Math.Min(speed, length / DoorMoveS);
            if (m.Op == "board") { carLobbyX = sx; carLobbyY = sy; }
            emove = new Emove
            {
                Id = m.Id, Op = m.Op, Path = path, Fail = fail, Speed = Math.Max(speed, 1e-3),
                Until = clock + DoorMoveS, StartX = sx, StartY = sy,
            };
            HasGoal = false;
            Mode = Modes.Moving;
            Report(m.Id, m.Op, "running", "");
        }

        void Report(long id, string op, string status, string detail) =>
            LastMove = new MoveReport { Id = id, Op = op, Status = status, Detail = detail ?? "" };

        public void Step(double dt)
        {
            if (dt <= 0) return;
            clock += dt;
            Battery = battery - DrainIdle * dt;
            V = 0;
            if (Mode == Modes.Charging)
            {
                Battery = battery + ChargeRate * dt;
                if (battery >= FullAt)
                    Mode = Modes.Idle;
            }
            else if (emove != null)
            {
                if (Mode != Modes.Error)
                    DriveMove(dt);
            }
            else if ((Mode == Modes.Moving || Mode == Modes.Docking) && HasGoal)
            {
                Drive(dt);
            }
            if (battery <= 0.0 && (Mode == Modes.Moving || Mode == Modes.Docking))
                Fail("battery_empty");
        }

        void DriveMove(double dt)
        {
            var e = emove;
            if (e.Path.Count > 0)
            {
                double gx = e.Path[0][0], gy = e.Path[0][1];
                double dx = gx - X, dy = gy - Y;
                double dist = Throttle.Hypot(dx, dy);
                if (dist <= ArriveTol)
                {
                    X = gx; Y = gy;
                    e.Path.RemoveAt(0);
                }
                else
                {
                    double move = Math.Min(dist, e.Speed * dt);
                    if (!e.Fail || e.Path.Count > 1)
                        Yaw = Math.Atan2(dy, dx); // backing out keeps the heading
                    X += move * dx / dist;
                    Y += move * dy / dist;
                    V = move / dt;
                    Battery = battery - DrainPerM * move;
                }
                return;
            }
            if (clock < e.Until)
                return; // slow door move: not through yet
            emove = null;
            Mode = Modes.Idle;
            if (e.Fail)
            {
                Report(e.Id, e.Op, "failed", "door zone blocked");
                return;
            }
            InCar = e.Op == "board";
            Report(e.Id, e.Op, "done", "");
        }

        void Drive(double dt)
        {
            double dx = GoalX - X, dy = GoalY - Y;
            double dist = Math.Sqrt(dx * dx + dy * dy);
            if (dist <= ArriveTol)
            {
                X = GoalX; Y = GoalY; Yaw = goalYaw;
                HasGoal = false;
                Mode = Mode == Modes.Docking ? Modes.Charging : Modes.Idle;
                return;
            }
            double move = Math.Min(dist, Speed * dt);
            Yaw = Math.Atan2(dy, dx);
            X += move * dx / dist;
            Y += move * dy / dist;
            V = move / dt;
            Battery = battery - DrainPerM * move;
        }

        void SetGoal(double x, double y, double yaw)
        {
            HasGoal = true;
            GoalX = x; GoalY = y; goalYaw = yaw;
        }

        static double Clamp(double b) => double.IsNaN(b) ? 0.0 : Math.Max(0.0, Math.Min(100.0, b));
    }
}
