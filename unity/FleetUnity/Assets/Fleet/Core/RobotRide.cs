// The robot side of docs/ELEVATOR.md (section 5): a behaviour-for-behaviour
// port of adapter/fleet_logic.py RobotRide (fleet_logic.py:174-546). Pure:
// it is fed the ride command, turns, the car's state, the retained trip, the
// robot's elevator-move results and the clock, and answers with actions for
// FleetRobot to carry out, in order (fleet_logic.py:178-193):
//
//   Ack    fleet/<robot>/cmd_ack (Floor: the "floor" extra, or null)
//   State  publish fleet/<robot>/state now. Override (mode, task_id,
//          silence_s) is sent although the link is about to go; null means
//          the usual state
//   Pose   publish fleet/<robot>/pose now (the floor changed)
//   Event  fleet/<robot>/event (sent even in the blackout: RobotRide queues
//          events itself while there is no link)
//   Zone   elevator/<car>/zone
//   Move   an elevator move for RobotSim (the adapter's /<robot>/elevator_move)
//
// While Silent is set (from the boarding turn until LinkBackS after the robot
// is out of the car) the robot has no Wi-Fi: FleetRobot publishes nothing on
// fleet/* for it (except the two above) and drops commands for it. The
// elevator channel keeps working. Robot events made during that time are
// queued and sent, in seq order, when the link is back.
//
// How a ride can end (fleet_logic.py:195-208, docs/ELEVATOR.md 5 and 6):
//   - normally: exit turn -> out of the car -> LinkBackS later Wi-Fi is back
//     -> pose, state, queued events, "done" with the floor;
//   - taken out by a human while the trip still runs (in_car false with no
//     move under way): event "rescued", then as above;
//   - the trip ends with the robot still inside (operator release, trip
//     done/aborted, retained trip cleared, or nothing heard of the trip for
//     TripLostS): Wi-Fi back at once, event "exit_failed", the ride is acked
//     "failed" and the robot reports ERROR ("needs a human") until it is
//     taken out; then event "rescued" and its own state again.
// An elevator move the robot does not report within turn_timeout_s +
// MoveExtra ends as "failed"; a robot that still reports the move as running
// is waited for, because it is still in the door zone.
using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using Newtonsoft.Json;
using Newtonsoft.Json.Linq;

namespace Fleet.Core
{
    public enum RideKind { Ack, State, Pose, Event, Zone, Move }

    /// <summary>mode/task_id/silence_s (and error) reported instead of the robot's own.</summary>
    public sealed class RideOverlay
    {
        public string Mode;
        public string TaskId;
        public double SilenceS;
        public string Error;    // null: the robot's own error

        public override bool Equals(object o) =>
            o is RideOverlay v && v.Mode == Mode && v.TaskId == TaskId && v.SilenceS == SilenceS && v.Error == Error;
        public override int GetHashCode() => (Mode ?? "").GetHashCode() ^ (TaskId ?? "").GetHashCode();
        public override string ToString() => $"{Mode} task={TaskId} silence={SilenceS} error={Error}";
    }

    /// <summary>An elevator move (mock_fleet.py's elevator_move JSON).</summary>
    public sealed class ElevatorMove
    {
        public long Id;
        public string Op;              // board | floor | exit
        public List<double[]> Path;    // board/exit: [[x, y], ...]
        public int? Floor;             // floor, exit

        public JObject ToJson()
        {
            var o = new JObject { ["id"] = Id, ["op"] = Op };
            if (Path != null) o["path"] = new JArray(Path.Select(p => new JArray(p[0], p[1])));
            if (Floor.HasValue) o["floor"] = Floor.Value;
            return o;
        }
    }

    public sealed class RideAction
    {
        public RideKind Kind;
        public string CmdId, Status, Message;  // Ack
        public int? Floor;                     // Ack extra {"floor": n}
        public RideOverlay Override;           // State
        public JObject Body;                   // Event, Zone
        public string Car;                     // Zone
        public ElevatorMove Move;              // Move

        public override string ToString()
        {
            switch (Kind)
            {
                case RideKind.Ack: return $"ack {CmdId} {Status} {Message} {(Floor.HasValue ? "floor " + Floor : "")}";
                case RideKind.State: return $"state {(Override == null ? "-" : Override.ToString())}";
                case RideKind.Event: case RideKind.Zone: return $"{Kind} {Body?.ToString(Formatting.None)}";
                case RideKind.Move: return $"move {Move?.ToJson().ToString(Formatting.None)}";
                default: return Kind.ToString();
            }
        }
    }

    /// <summary>What RobotRide needs of the robot's robot_state (null when not known / not live).</summary>
    public sealed class RideRobotState
    {
        public string Mode;
        public string Error = "";
        public int Floor = 1;
        public bool? InCar;            // null: not reported
        public MoveReport Move;
    }

    public struct Spot
    {
        public double X, Y;
        public Spot(double x, double y) { X = x; Y = y; }
    }

    public sealed class RobotRide
    {
        // fleet_logic.py:210-221
        public const double LobbyTol = 0.5;      // m, "at the lobby" for accepting a ride
        public const double LinkBackS = 1.0;     // s after leaving the car until Wi-Fi is back
        public const double MoveExtraS = 30.0;   // s added to turn_timeout_s before an unreported move fails
        public const double TripLostS = 60.0;    // s without the trip (republished every 10 s) or a turn
        public const double DoorDx = 1.0;        // car door, from the lobby (the car is along +x of its lobby)
        public const double Slot0Dx = 1.8;       // slot 0 (door side), from the lobby
        public const double SlotDx = 0.8;        // between slots
        public const int Remember = 256;         // ride cmd_ids and finished trips remembered

        public const string Waiting = "waiting", Boarding = "boarding", Riding = "riding", Exiting = "exiting", Out = "out";

        public static double[] CarDoor(Spot lobby) => new[] { lobby.X + DoorDx, lobby.Y };
        public static double[] CarSlot(Spot lobby, int slot) => new[] { lobby.X + Slot0Dx + SlotDx * slot, lobby.Y };

        sealed class Plan
        {
            public string Trip, Elevator;
            public int Origin, Slot;
            public double SilenceS, TurnTimeoutS;
            public Spot Lobby;
            public Spot? ExitLobby;
            public string ExitFloor;
        }

        sealed class Move
        {
            public long Id;
            public int N, Floor;
            public string Action;
            public double Deadline;
        }

        public readonly string Robot, Boot;
        readonly Func<double> wall;
        readonly double moveExtra;

        public string CmdId { get; private set; }
        Plan plan;
        public string Stage { get; private set; }
        readonly Dictionary<int, JObject> results = new Dictionary<int, JObject>(); // turn n -> zone body sent for it
        Move move;                         // the move under way
        public bool Silent { get; private set; }
        double? linkAt;
        int? carFloor;
        double tripSeen;                   // last time the trip or one of its turns was heard
        string tripOver = "";              // the trip ended while a move was under way
        public string Stranded { get; private set; } = "";  // car the robot was left in when its trip ended
        readonly List<JObject> queue = new List<JObject>();  // robot events made without a link
        long evSeq, zoneSeq, moveSeq;
        readonly Recent rideIds = new Recent(Remember);
        readonly Recent ended = new Recent(Remember);
        int ignoreTurns;                   // failure injection: this many own turns are "lost"

        public RobotRide(string robot, string boot, Func<double> wall = null, int ignoreTurns = 0,
                         long moveBase = 0, double moveExtra = MoveExtraS)
        {
            Robot = robot; Boot = boot;
            this.wall = wall ?? (() => DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() / 1000.0);
            this.ignoreTurns = ignoreTurns;
            moveSeq = moveBase;
            this.moveExtra = moveExtra;
        }

        public bool Holding => Stage != null;
        public string Trip => plan?.Trip ?? "";
        public string Elevator => plan?.Elevator ?? "";
        public bool Moving => move != null;
        public long? MoveId => move?.Id;
        public IReadOnlyList<JObject> Queue => queue;

        /// <summary>fleet_logic.py:275-290. mode/task_id/silence_s (and error)
        /// to report instead of the robot's own, or null.</summary>
        public RideOverlay Overlay(RideRobotState rs = null)
        {
            if (Stage == null)
            {
                if (Stranded != "" || rs?.InCar == true)
                {
                    var car = Stranded != "" ? Stranded : "a car";
                    return new RideOverlay { Mode = "ERROR", TaskId = "", SilenceS = 0, Error = $"in elevator {car} without a ride: needs a human" };
                }
                return null;
            }
            if (Stage == Out) return null;
            string mode = Stage == Waiting ? "WAITING_ELEVATOR" : Stage == Boarding ? "BOARDING" : Stage == Riding ? "RIDING" : "EXITING";
            return new RideOverlay { Mode = mode, TaskId = CmdId, SilenceS = Stage != Waiting ? plan.SilenceS : 0 };
        }

        // ---------------------------------------------------------------- commands (fleet link)

        /// <summary>fleet_logic.py:292-336. A new ride command (the caller already
        /// checked cmd_id for repeats and expiry). rs is the robot's state, pos its (x, y).</summary>
        public List<RideAction> Accept(string cmdId, JObject ride, RideRobotState rs, Spot? pos, double now = 0.0)
        {
            List<RideAction> Fail(string msg) => new List<RideAction> { Ack(cmdId, "failed", msg) };
            if (ride == null || !ride.HasValues)
                return Fail("ride without a plan");
            Plan p;
            try
            {
                p = new Plan
                {
                    Trip = Str(Need(ride, "trip")), Elevator = Str(Need(ride, "elevator")),
                    Origin = Int(Need(ride, "origin")), Slot = Int(Need(ride, "slot")),
                    SilenceS = Num(Need(ride, "silence_s")),
                    TurnTimeoutS = Truthy(ride["turn_timeout_s"]) ? Num(ride["turn_timeout_s"]) : 15,
                    Lobby = ParseSpot(Need(ride, "lobby")),
                    ExitFloor = ride["exit_floor"] == null || ride["exit_floor"].Type == JTokenType.Null ? "None" : ride["exit_floor"].ToString(),
                };
                try { p.ExitLobby = Truthy(ride["exit_lobby"]) ? ParseSpot(ride["exit_lobby"]) : (Spot?)null; }
                catch (FormatException) { p.ExitLobby = null; }
            }
            catch (FormatException e)
            {
                return Fail($"bad ride plan ({e.Message})");
            }
            if (p.Trip == "" || p.Elevator == "" || p.Slot < 0 || p.SilenceS <= 0)
                return Fail("bad ride plan");
            if (rs == null || pos == null)
                return Fail("robot state not known yet");
            if (rs.Mode == "ERROR")
                return Fail($"in ERROR ({rs.Error})");
            if (Stage != null && Stage != Waiting)
                return Fail($"already riding {Trip}");
            int floor = rs.Floor;
            if (floor != p.Origin)
                return Fail($"on floor {floor}, the ride starts on {p.Origin}");
            if (rs.InCar == true || Stranded != "")
                return Fail("in a car");
            double d = Throttle.Hypot(pos.Value.X - p.Lobby.X, pos.Value.Y - p.Lobby.Y);
            if (d > LobbyTol)
                return Fail($"not at the lobby ({d.ToString("F1", CultureInfo.InvariantCulture)} m away)");
            var outq = new List<RideAction>();
            if (Stage == Waiting)
            {
                outq.Add(Ack(CmdId, "cancelled", $"replaced by {cmdId}"));
                outq.AddRange(Event("ride_cancelled", floor, $"replaced by {cmdId}"));
                Clear();
            }
            CmdId = cmdId;
            Stage = Waiting;
            plan = p;
            results.Clear();
            move = null; Silent = false; linkAt = null; carFloor = null;
            tripSeen = now; tripOver = "";
            rideIds.Add(cmdId);
            outq.Add(Ack(cmdId, "accepted", ""));
            outq.AddRange(Event("ride_accepted", floor, $"exit {p.ExitFloor}, slot {p.Slot}"));
            outq.Add(new RideAction { Kind = RideKind.State });
            return outq;
        }

        /// <summary>fleet_logic.py:338-353. A cancel. Returns the actions, or null
        /// when it is not about rides (then it stops a goto as before).</summary>
        public List<RideAction> Cancel(string cmdId, string target)
        {
            if (!string.IsNullOrEmpty(target) && target == CmdId && Stage != null)
            {
                if (Stage != Waiting)
                    return new List<RideAction> { Ack(cmdId, "failed", $"ride {target} already started ({Stage})") };
                int floor = plan.Origin;
                var outq = new List<RideAction> { Ack(CmdId, "cancelled", "ride cancelled at the lobby") };
                outq.AddRange(Event("ride_cancelled", floor, $"by {cmdId}"));
                Clear();
                outq.Add(Ack(cmdId, "accepted", ""));
                outq.Add(new RideAction { Kind = RideKind.State });
                return outq;
            }
            if (!string.IsNullOrEmpty(target) && rideIds.Contains(target))
                return new List<RideAction> { Ack(cmdId, "failed", $"{target} is not the current ride") };
            if (Stage != null)
                return new List<RideAction> { Ack(cmdId, "failed", $"holding ride {CmdId}; the cancel is for {(string.IsNullOrEmpty(target) ? "nothing" : target)}") };
            return null;
        }

        void Clear()   // fleet_logic.py:355-359
        {
            if (plan != null) ended.Add(plan.Trip);
            CmdId = null; plan = null; Stage = null;
            results.Clear();
            move = null; linkAt = null; carFloor = null; tripOver = "";
        }

        // ---------------------------------------------------------------- elevator channel

        /// <summary>fleet_logic.py:363-410. A turn from elevator/&lt;car&gt;/turn.</summary>
        public List<RideAction> Turn(JObject t, double now = 0.0)
        {
            var none = new List<RideAction>();
            if (Stage == null || t == null || StrOrNull(t["trip"]) != Trip || StrOrNull(t["robot"]) != Robot)
                return none; // not ours: another trip, another robot, or the ride is over
            int n, floor;
            string action;
            try
            {
                n = Int(Need(t, "n"));
                action = Str(Need(t, "action"));
                floor = Int(Need(t, "floor"));
            }
            catch (FormatException) { return none; }
            tripSeen = now;
            if (ignoreTurns > 0)
            {
                ignoreTurns--;
                return none;
            }
            if (move != null)
                // In the door zone on move.N: answer nothing now, not even an old
                // n again (its answer would take the robot out of the car's count).
                return none;
            if (results.TryGetValue(n, out var answered))
                return new List<RideAction> { ZoneAction(answered) }; // answered: answer again
            if (action == "board" && (Stage == Riding || Stage == Exiting || Stage == Out) || action == "exit" && Stage == Out)
                // Already done (the answer was lost or late): clear for this n.
                return Answer(n, action, floor, "clear");
            if (action == "exit" && Stage == Waiting)
                return Answer(n, action, floor, "failed", "not in the car");
            var lobby = plan.Lobby;
            List<RideAction> outq;
            List<double[]> path;
            if (action == "board" && Stage == Waiting)
            {
                outq = Event("boarding", floor, $"turn {n}");
                outq.Add(new RideAction { Kind = RideKind.State, Override = new RideOverlay { Mode = "BOARDING", TaskId = CmdId, SilenceS = plan.SilenceS } });
                Stage = Boarding; Silent = true; carFloor = floor;
                path = new List<double[]> { CarDoor(lobby), CarSlot(lobby, plan.Slot) };
            }
            else if (action == "exit" && Stage == Riding)
            {
                Spot exitLobby;
                try { exitLobby = ParseSpot(t["lobby"]); }
                catch (FormatException) { exitLobby = plan.ExitLobby ?? lobby; }
                outq = new List<RideAction>();
                Stage = Exiting;
                path = new List<double[]> { CarDoor(exitLobby), new[] { exitLobby.X, exitLobby.Y } };
            }
            else
            {
                return none;
            }
            moveSeq++;
            move = new Move { Id = moveSeq, N = n, Action = action, Floor = floor, Deadline = now + plan.TurnTimeoutS + moveExtra };
            outq.Add(ZoneAction(Zone(n, action, "enter", floor)));
            outq.Add(new RideAction
            {
                Kind = RideKind.Move,
                Move = new ElevatorMove { Id = moveSeq, Op = action, Path = path, Floor = action == "exit" ? floor : (int?)null },
            });
            return outq;
        }

        /// <summary>fleet_logic.py:412-419. The car's state: while riding, the
        /// robot is on the car's floor once the car stands there.</summary>
        public List<RideAction> CarState(JObject s)
        {
            var none = new List<RideAction>();
            if (Stage != Riding || s == null || Truthy(s["moving"]) || s["floor"] == null || s["floor"].Type == JTokenType.Null)
                return none;
            int f;
            try { f = Int(s["floor"]); } catch (FormatException) { return none; }
            if (f == carFloor) return none;
            carFloor = f;
            moveSeq++;
            return new List<RideAction> { new RideAction { Kind = RideKind.Move, Move = new ElevatorMove { Id = moveSeq, Op = "floor", Floor = f } } };
        }

        /// <summary>fleet_logic.py:421-440. The retained trip (null: cleared). A
        /// trip that ended, was cleared or was replaced while the robot is still
        /// in the car leaves it there.</summary>
        public List<RideAction> TripMsg(JObject msg, double now = 0.0)
        {
            var none = new List<RideAction>();
            if (Stage == null) return none;
            var stage = msg == null ? null : StrOrNull(msg["stage"]);
            if (msg != null && StrOrNull(msg["trip"]) == Trip && stage != "done" && stage != "aborted")
            {
                tripSeen = now;
                return none;
            }
            if (Stage == Waiting) return none; // before boarding: the server's cancel ends the ride
            string why;
            if (msg != null && StrOrNull(msg["trip"]) == Trip)
            {
                var detail = StrOrNull(msg["detail"]);
                why = $"trip {Trip} {PyStr(msg["stage"])}" + (string.IsNullOrEmpty(detail) ? "" : $" ({detail})");
            }
            else if (msg == null)
                why = $"trip {Trip} cleared";
            else
                why = $"trip {Trip} replaced by {PyStr(msg["trip"])}";
            if (tripOver == "") tripOver = why;
            return Settle(now);
        }

        // ---------------------------------------------------------------- robot

        /// <summary>fleet_logic.py:442-447. The robot's report of an elevator move.</summary>
        public List<RideAction> MoveResult(long moveId, string status, string detail, double now)
        {
            if (move == null || moveId != move.Id || (status != "done" && status != "failed"))
                return new List<RideAction>();
            var m = move;
            move = null;
            var outq = Moved(m, status, detail, now);
            outq.AddRange(Settle(now));
            return outq;
        }

        List<RideAction> Moved(Move m, string status, string detail, double now)   // fleet_logic.py:449-466
        {
            int n = m.N, floor = m.Floor;
            string action = m.Action;
            if (action == "board")
            {
                if (status == "done")
                {
                    Stage = Riding;
                    var o = Answer(n, action, floor, "clear");
                    o.AddRange(Event("boarded", floor, $"slot {plan.Slot}"));
                    return o;
                }
                // Not in: back at the lobby, the link is back. The server drops it from the trip.
                Stage = Waiting; Silent = false;
                var f = Answer(n, action, floor, "failed", string.IsNullOrEmpty(detail) ? "could not board" : detail);
                f.AddRange(Event("board_failed", floor, detail));
                f.Add(new RideAction { Kind = RideKind.State });
                f.AddRange(Flush());
                return f;
            }
            if (status == "done")
            {
                Stage = Out; linkAt = now + LinkBackS;
                var o = Answer(n, action, floor, "clear");
                o.AddRange(Event("exited", floor, ""));
                return o;
            }
            Stage = Riding; // back in its slot; waits for the next exit turn
            var x = Answer(n, action, floor, "failed", string.IsNullOrEmpty(detail) ? "could not exit" : detail);
            x.AddRange(Event("exit_failed", floor, detail));
            return x;
        }

        /// <summary>fleet_logic.py:468-502. Clocks: an unreported move, the link
        /// back after the exit, a robot taken out by a human, a trip that is gone.
        /// rs is the robot's live state (null if it is not publishing).</summary>
        public List<RideAction> Tick(double now, int robotFloor, RideRobotState rs = null)
        {
            var outq = new List<RideAction>();
            if (Stranded != "" && rs != null && rs.InCar == false)
            {
                var car = Stranded;
                Stranded = "";
                outq.AddRange(Event("rescued", robotFloor, $"taken out of {car}"));
                outq.Add(new RideAction { Kind = RideKind.State });
            }
            if (Stage == null) return outq;
            if (move != null && now >= move.Deadline)
            {
                var rep = rs?.Move;
                if (rep != null && rep.Id == move.Id && rep.Status == "running")
                    // Alive and still on it (slow, or stopped in the door zone): it is in the zone.
                    move.Deadline = now + 1.0;
                else
                {
                    var m = move;
                    move = null;
                    outq.AddRange(Moved(m, "failed", "robot did not finish the move", now));
                }
            }
            if (Stage == Riding && move == null && rs != null && rs.InCar == false)
            {
                // Taken out by a human while the trip runs: out, on this floor.
                outq.AddRange(Event("rescued", robotFloor, "taken out of the car"));
                Stage = Out; linkAt = now;
            }
            if (Stage == Riding && tripOver == "" && now - tripSeen > TripLostS)
                tripOver = $"nothing heard of trip {Trip} for {TripLostS:F0} s";
            outq.AddRange(Settle(now, robotFloor));
            if (Stage != Out || now < linkAt) return outq;
            var cmdId = CmdId;
            Silent = false;
            Clear();
            outq.Add(new RideAction { Kind = RideKind.Pose });
            outq.Add(new RideAction { Kind = RideKind.State });
            outq.AddRange(Flush());
            outq.Add(Ack(cmdId, "done", "", robotFloor));
            return outq;
        }

        /// <summary>fleet_logic.py:504-519. The trip is over and the robot still
        /// in the car (no move under way): link back, the ride fails, the robot
        /// waits for a human.</summary>
        List<RideAction> Settle(double now, int? robotFloor = null)
        {
            var outq = new List<RideAction>();
            if (tripOver == "" || Stage != Riding || move != null) return outq;
            int? floor = robotFloor ?? carFloor;
            string why = tripOver, cmdId = CmdId, car = Elevator;
            outq.AddRange(Event("exit_failed", floor, $"{why}; still in the car, needs a human"));
            Silent = false;
            Clear();
            Stranded = car;
            outq.Add(new RideAction { Kind = RideKind.Pose });
            outq.Add(new RideAction { Kind = RideKind.State });
            outq.AddRange(Flush());
            outq.Add(Ack(cmdId, "failed", $"{why}; robot still in the car", floor));
            return outq;
        }

        // ---------------------------------------------------------------- helpers (fleet_logic.py:521-546)

        List<RideAction> Answer(int n, string action, int floor, string ev, string reason = "")
        {
            var z = Zone(n, action, ev, floor, reason);
            results[n] = z;
            return new List<RideAction> { ZoneAction(z) };
        }

        RideAction ZoneAction(JObject z) => new RideAction { Kind = RideKind.Zone, Car = Elevator, Body = z };

        JObject Zone(int n, string action, string ev, int floor, string reason = "")
        {
            zoneSeq++;
            var z = new JObject
            {
                ["trip"] = Trip, ["n"] = n, ["robot"] = Robot, ["action"] = action, ["event"] = ev,
                ["floor"] = floor, ["seq"] = zoneSeq, ["stamp"] = Math.Round(wall(), 3),
            };
            if (!string.IsNullOrEmpty(reason)) z["reason"] = reason;
            return z;
        }

        List<RideAction> Event(string name, int? floor, string detail)
        {
            evSeq++;
            var e = new JObject
            {
                ["boot"] = Boot, ["seq"] = evSeq, ["stamp"] = Math.Round(wall(), 3), ["event"] = name,
                ["trip"] = Trip, ["cmd_id"] = CmdId ?? "",
                ["floor"] = floor.HasValue ? (JToken)floor.Value : JValue.CreateNull(), ["detail"] = detail ?? "",
            };
            if (Silent)
            {
                queue.Add(e);
                return new List<RideAction>();
            }
            return new List<RideAction> { new RideAction { Kind = RideKind.Event, Body = e } };
        }

        List<RideAction> Flush()
        {
            var outq = queue.OrderBy(e => (long)e["seq"]).Select(e => new RideAction { Kind = RideKind.Event, Body = e }).ToList();
            queue.Clear();
            return outq;
        }

        static RideAction Ack(string cmdId, string status, string message, int? floor = null) =>
            new RideAction { Kind = RideKind.Ack, CmdId = cmdId, Status = status, Message = message, Floor = floor };

        // Python's KeyError / int() / float() / str() / truthiness, as FormatException.
        static JToken Need(JObject o, string key) =>
            o.TryGetValue(key, out var v) ? v : throw new FormatException($"KeyError('{key}')");

        static string Str(JToken t) =>
            t == null || t.Type == JTokenType.Null ? "" : t.Type == JTokenType.String ? (string)t : t.ToString(Formatting.None);

        static string StrOrNull(JToken t) => t != null && t.Type == JTokenType.String ? (string)t : null;

        static string PyStr(JToken t) => t == null || t.Type == JTokenType.Null ? "None" : Str(t);

        static int Int(JToken t)
        {
            if (t == null) throw new FormatException("missing");
            switch (t.Type)
            {
                case JTokenType.Integer: return t.Value<int>();
                case JTokenType.Float: return (int)Math.Truncate(t.Value<double>());
                case JTokenType.Boolean: return t.Value<bool>() ? 1 : 0;
                case JTokenType.String:
                    if (int.TryParse(((string)t).Trim(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var v)) return v;
                    break;
            }
            throw new FormatException($"invalid literal for int(): {t.ToString(Formatting.None)}");
        }

        static double Num(JToken t)
        {
            if (t == null) throw new FormatException("missing");
            switch (t.Type)
            {
                case JTokenType.Integer: case JTokenType.Float: return t.Value<double>();
                case JTokenType.Boolean: return t.Value<bool>() ? 1 : 0;
                case JTokenType.String:
                    if (double.TryParse(((string)t).Trim(), NumberStyles.Float, CultureInfo.InvariantCulture, out var v)) return v;
                    break;
            }
            throw new FormatException($"could not convert to float: {t.ToString(Formatting.None)}");
        }

        static bool Truthy(JToken t)
        {
            if (t == null) return false;
            switch (t.Type)
            {
                case JTokenType.Null: case JTokenType.Undefined: return false;
                case JTokenType.Boolean: return t.Value<bool>();
                case JTokenType.Integer: case JTokenType.Float: return t.Value<double>() != 0;
                case JTokenType.String: return ((string)t).Length > 0;
                case JTokenType.Object: case JTokenType.Array: return t.HasValues;
                default: return true;
            }
        }

        static Spot ParseSpot(JToken t)
        {
            if (!(t is JObject o)) throw new FormatException("lobby is not an object");
            return new Spot(Num(Need(o, "x")), Num(Need(o, "y")));
        }

        /// <summary>An insertion-ordered set of the last N keys (fleet_logic._remember).</summary>
        sealed class Recent
        {
            readonly int size;
            readonly LinkedList<string> order = new LinkedList<string>();
            readonly Dictionary<string, LinkedListNode<string>> index = new Dictionary<string, LinkedListNode<string>>();
            public Recent(int size) { this.size = size; }
            public bool Contains(string k) => k != null && index.ContainsKey(k);
            public void Add(string k)
            {
                if (index.TryGetValue(k, out var node)) order.Remove(node);
                index[k] = order.AddLast(k);
                while (order.Count > size)
                {
                    index.Remove(order.First.Value);
                    order.RemoveFirst();
                }
            }
        }
    }
}
