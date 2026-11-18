// Command-line options of the robot player:
//   -broker host:port -host id -robots N -prefix name -floor F
//   -origin x,y -spacing m -speed m/s -battery %
//   -canRide true|false       elevator rides (docs/ELEVATOR.md); false sends can_ride:false
//   -exitFailFloor F [-exitFailCount N]   failure injection like mock_fleet.py:
//                             exits on floor F fail (door zone blocked), N per robot (-1: every one)
//   -doorMoveS s              every board/exit takes at least s seconds
//   -ignoreTurns robot:n[,robot:n]  the robot misses its first n own turns (adapter --ignore-turns)
//   -moveTimeoutExtra s       an unreported elevator move fails after turn_timeout_s + s (default 30)
// Unknown arguments (Unity's own, e.g. -batchmode, -logFile) are skipped.
using System;
using System.Collections.Generic;
using System.Globalization;

namespace Fleet.Core
{
    public sealed class BootArgs
    {
        public string BrokerHost = "127.0.0.1";
        public int BrokerPort = 1883;
        public string Host = "unity-1";
        public int Robots = 2;
        public string Prefix = "unity";      // robot names are <prefix>_1, <prefix>_2, ...
        public int Floor = 1;
        // Default docks sit at y = -3 so they do not overlap the ROS mocks'
        // default docks at (0, 0), (2, 0), ... when both fleets run.
        public double OriginX = 0, OriginY = -3;
        public double Spacing = 2.0;          // m between docks along x
        public double Speed = 0.8;            // m/s, the mock's default
        public double Battery = 100;          // start %, kept above fleetd's LowBattery by default
        public bool CanRide = true;
        public int? ExitFailFloor;
        public int ExitFailCount = -1;
        public double DoorMoveS;
        public double MoveTimeoutExtra = RobotRide.MoveExtraS;
        public readonly Dictionary<string, int> IgnoreTurns = new Dictionary<string, int>();
        public readonly List<string> Warnings = new List<string>();

        public string RobotName(int i) => $"{Prefix}_{i + 1}";

        public static BootArgs Parse(IList<string> args)
        {
            var a = new BootArgs();
            for (int i = 0; i < args.Count; i++)
            {
                string key = args[i].ToLowerInvariant();
                string val = i + 1 < args.Count ? args[i + 1] : null;
                bool used = true;
                try
                {
                    switch (key)
                    {
                        case "-broker":
                            var hp = Need(val).Split(':');
                            a.BrokerHost = hp[0];
                            if (hp.Length > 1) a.BrokerPort = int.Parse(hp[1], CultureInfo.InvariantCulture);
                            break;
                        case "-host": a.Host = Need(val); break;
                        case "-robots": a.Robots = Math.Max(0, int.Parse(Need(val), CultureInfo.InvariantCulture)); break;
                        case "-prefix": a.Prefix = Need(val); break;
                        case "-floor": a.Floor = int.Parse(Need(val), CultureInfo.InvariantCulture); break;
                        case "-origin":
                            var xy = Need(val).Split(',');
                            a.OriginX = D(xy[0]);
                            a.OriginY = D(xy[1]);
                            break;
                        case "-spacing": a.Spacing = D(Need(val)); break;
                        case "-speed": a.Speed = D(Need(val)); break;
                        case "-battery": a.Battery = Math.Max(0, Math.Min(100, D(Need(val)))); break;
                        case "-canride":
                            var v = Need(val).ToLowerInvariant();
                            if (v == "true" || v == "1" || v == "yes") a.CanRide = true;
                            else if (v == "false" || v == "0" || v == "no") a.CanRide = false;
                            else throw new FormatException("not a bool");
                            break;
                        case "-exitfailfloor": a.ExitFailFloor = int.Parse(Need(val), CultureInfo.InvariantCulture); break;
                        case "-exitfailcount": a.ExitFailCount = int.Parse(Need(val), CultureInfo.InvariantCulture); break;
                        case "-doormoves": a.DoorMoveS = Math.Max(0, D(Need(val))); break;
                        case "-movetimeoutextra": a.MoveTimeoutExtra = Math.Max(0, D(Need(val))); break;
                        case "-ignoreturns":
                            foreach (var spec in Need(val).Split(','))
                            {
                                var kv = spec.Split(':');
                                a.IgnoreTurns[kv[0].Trim()] = kv.Length > 1 ? int.Parse(kv[1], CultureInfo.InvariantCulture) : 1;
                            }
                            break;
                        default: used = false; break;
                    }
                }
                catch (Exception e) when (e is FormatException || e is IndexOutOfRangeException || e is OverflowException)
                {
                    a.Warnings.Add($"bad value for {args[i]}: '{val}', default kept");
                }
                if (used) i++;
            }
            if (a.Host.Contains("/") || a.Host.Contains("+") || a.Host.Contains("#"))
                a.Warnings.Add($"host '{a.Host}' contains an MQTT topic character");
            return a;
        }

        static string Need(string v) => v ?? throw new FormatException("missing value");
        static double D(string s) => double.Parse(s, NumberStyles.Float, CultureInfo.InvariantCulture);
    }
}
