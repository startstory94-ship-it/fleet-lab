// What a fleet server can see of a robot: kinematic straight-line motion,
// battery, mode, dock and faults. Mirrors mock_robots/mock_fleet.py so the two
// robot kinds behave alike; no map, no obstacles. Coordinates are the fleet's
// map frame (x, y in metres, yaw in radians, counter-clockwise from +x).
using System;

namespace Fleet.Core
{
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

        public RobotSim(double x, double y, double battery)
        {
            X = DockX = x;
            Y = DockY = y;
            Battery = battery;
        }

        /// <summary>Starts driving to (x, y). Returns null, or why the goal is refused.</summary>
        public string Goto(double x, double y, double yaw)
        {
            if (Mode == Modes.Error) return $"in ERROR ({Error})";
            if (battery <= 0.0) return "battery empty";
            SetGoal(x, y, yaw);
            Mode = Modes.Moving;
            return null;
        }

        /// <summary>Stops a drive or a dock. Returns null, or why there was nothing to stop.</summary>
        public string Stop()
        {
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
            SetGoal(DockX, DockY, 0.0);
            Mode = Modes.Docking;
            return null;
        }

        public string ResetError()
        {
            if (Mode != Modes.Error) return "not in ERROR";
            Mode = Modes.Idle;
            Error = "";
            HasGoal = false;
            return null;
        }

        public void Fail(string reason)
        {
            Mode = Modes.Error;
            Error = reason;
            HasGoal = false;
            V = 0;
        }

        public void Step(double dt)
        {
            if (dt <= 0) return;
            Battery = battery - DrainIdle * dt;
            V = 0;
            if (Mode == Modes.Charging)
            {
                Battery = battery + ChargeRate * dt;
                if (battery >= FullAt)
                    Mode = Modes.Idle;
            }
            else if ((Mode == Modes.Moving || Mode == Modes.Docking) && HasGoal)
            {
                Drive(dt);
            }
            if (battery <= 0.0 && (Mode == Modes.Moving || Mode == Modes.Docking))
                Fail("battery_empty");
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
