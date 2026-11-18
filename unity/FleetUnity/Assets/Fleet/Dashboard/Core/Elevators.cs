// GET /elevators (docs/ELEVATOR.md section 10) and what the map makes of it,
// ported from web/app.js 148-190: who is in which car slot, the car outline
// next to its lobby, plus CarMotion, which the 3D view needs and the 2D map
// does not: a continuous car height between floors.
using System;
using System.Collections.Generic;
using Newtonsoft.Json;

namespace Fleet.Dashboard
{
    public struct Point2
    {
        [JsonProperty("x")] public double X;
        [JsonProperty("y")] public double Y;
        public Point2(double x, double y) { X = x; Y = y; }
    }

    public sealed class CarState
    {
        [JsonProperty("floor")] public int Floor;
        [JsonProperty("dir")] public string Dir = "idle";     // up, down, idle
        [JsonProperty("door")] public string Door = "closed"; // open, closed, opening, closing
        [JsonProperty("moving")] public bool Moving;
        [JsonProperty("hold")] public bool Hold;
        [JsonProperty("trip")] public string Trip = "";
        [JsonProperty("load")] public int Load;
        [JsonProperty("door_zone")] public int DoorZone;
    }

    public sealed class Rider
    {
        [JsonProperty("robot")] public string Robot = "";
        [JsonProperty("dest")] public int Dest;
        [JsonProperty("exit")] public int Exit;
        [JsonProperty("board")] public int Board;
        [JsonProperty("slot")] public int Slot;
        [JsonProperty("status")] public string Status = "";
        [JsonProperty("out_floor")] public int OutFloor;
    }

    public sealed class Trip
    {
        [JsonProperty("trip")] public string Id = "";
        [JsonProperty("stage")] public string Stage = "";
        [JsonProperty("floor")] public int Floor;
        [JsonProperty("stops")] public List<int> Stops = new List<int>();
        [JsonProperty("riders")] public List<Rider> Riders = new List<Rider>();
        [JsonProperty("detail")] public string Detail = "";
    }

    public sealed class WaitView
    {
        [JsonProperty("robot")] public string Robot = "";
        [JsonProperty("floor")] public int Floor;
        [JsonProperty("dest")] public int Dest;
    }

    public sealed class ElevatorView
    {
        [JsonProperty("id")] public string Id = "";
        [JsonProperty("capacity")] public int Capacity;
        [JsonProperty("floors")] public List<int> Floors = new List<int>();
        [JsonProperty("lobby")] public Point2 Lobby;
        [JsonProperty("state")] public CarState State;     // null until the car is heard
        [JsonProperty("responding")] public bool Responding;
        [JsonProperty("trip")] public Trip Trip;           // null without a trip
        [JsonProperty("waiting")] public List<WaitView> Waiting = new List<WaitView>();
    }

    /// <summary>What is in a car slot (carSlots, app.js 157-166).</summary>
    public sealed class SlotInfo
    {
        public string Robot;
        public int Exit, Dest;
        public string Status;
        public bool Carried;
    }

    /// <summary>A robot that may be in a car (ridersInCars, app.js 168-180).</summary>
    public struct InCar
    {
        public string Car;
        public int Slot, Floor;
        public string Status;
    }

    /// <summary>The car outline in map metres and slot centres (carBox, app.js 182-190).</summary>
    public struct CarBox
    {
        public double X0, X1, Y0, Y1;
        public Point2 Lobby;
        public Point2 Slot(int i) => new Point2(Lobby.X + Elevators.CarSlot0Dx + Elevators.CarSlotDx * i, Lobby.Y);
    }

    public static class Elevators
    {
        // Rider statuses that may be in the car (docs/ELEVATOR.md 3.8). A boarding
        // robot is drawn in its slot: it is on its way there. (app.js 150-152)
        public static readonly HashSet<string> InCarStatuses =
            new HashSet<string> { "boarding", "inside", "unsure_in", "exiting", "stuck" };
        // Car geometry, as the adapter drives the mock robots (adapter/fleet_logic.py):
        // the car lies along +x of its lobby, door first, slot 0 at the door. (app.js 153-155)
        public const double CarDoorDx = 1.0, CarSlot0Dx = 1.8, CarSlotDx = 0.8;

        static readonly JsonSerializerSettings Json = new JsonSerializerSettings
        {
            DateParseHandling = DateParseHandling.None,
            MissingMemberHandling = MissingMemberHandling.Ignore,
        };

        public static List<ElevatorView> Parse(string json) =>
            JsonConvert.DeserializeObject<List<ElevatorView>>(json, Json) ?? new List<ElevatorView>();

        public static SlotInfo[] CarSlots(ElevatorView view)
        {
            var slots = new SlotInfo[view == null ? 0 : Math.Max(0, view.Capacity)];
            if (view?.Trip?.Riders == null) return slots;
            foreach (var r in view.Trip.Riders)
            {
                if (!InCarStatuses.Contains(r.Status) || !(r.Slot >= 0 && r.Slot < slots.Length)) continue;
                slots[r.Slot] = new SlotInfo { Robot = r.Robot, Exit = r.Exit, Dest = r.Dest, Status = r.Status, Carried = r.Exit != r.Dest };
            }
            return slots;
        }

        /// <summary>Robot -> car for every robot that may be in a car. Their last
        /// pose is from before the blackout, so the map draws them in the car.</summary>
        public static Dictionary<string, InCar> RidersInCars(IEnumerable<ElevatorView> views)
        {
            var o = new Dictionary<string, InCar>();
            if (views == null) return o;
            foreach (var v in views)
            {
                int floor = v.State != null ? v.State.Floor : v.Trip?.Floor ?? 0;
                if (v.Trip?.Riders == null) continue;
                foreach (var r in v.Trip.Riders)
                    if (InCarStatuses.Contains(r.Status))
                        o[r.Robot] = new InCar { Car = v.Id, Slot = r.Slot, Floor = floor, Status = r.Status };
            }
            return o;
        }

        public static CarBox Box(Point2 lobby, int capacity)
        {
            int cap = Math.Max(1, capacity);
            return new CarBox
            {
                Lobby = lobby,
                X0 = lobby.X + CarDoorDx,
                X1 = lobby.X + CarSlot0Dx + CarSlotDx * (cap - 1) + 0.6,
                Y0 = lobby.Y - 0.7,
                Y1 = lobby.Y + 0.7,
            };
        }

        /// <summary>The floors to stack: every floor a car serves, every floor a
        /// robot is on, and floors of active task waypoints (floorsSeen, app.js 607-614).</summary>
        public static List<int> FloorsToDraw(IEnumerable<ElevatorView> cars, IEnumerable<RobotView> robots, IEnumerable<TaskView> tasks)
        {
            var set = new SortedSet<int>();
            if (cars != null) foreach (var c in cars) foreach (var f in c.Floors) set.Add(f);
            if (robots != null) foreach (var r in robots) if (r.HasPose) set.Add(r.Floor);
            if (tasks != null)
                foreach (var t in tasks)
                    if (TaskView.Active.Contains(t.State))
                        foreach (var w in t.Waypoints) set.Add(w.Floor);
            return new List<int>(set);
        }
    }

    /// <summary>
    /// A continuous car position (in floors) from polled state. state.floor is
    /// the last floor passed and arrives about once a second, so while the car
    /// moves this advances from the last floor change at the observed time per
    /// floor (mockelevator's floor_s, 2 s by default) and stops short of the
    /// next floor until the state says it got there.
    /// </summary>
    public sealed class CarMotion
    {
        public double FloorMs = 2000;   // estimate of travel time per floor
        int floor;
        bool known, moving;
        int dir;
        double changedAt;               // local ms of the last floor change or start

        public void Observe(CarState s, double nowMs)
        {
            if (s == null) return;
            int d = s.Dir == "up" ? 1 : s.Dir == "down" ? -1 : 0;
            if (!known)
            {
                known = true;
                floor = s.Floor;
                moving = s.Moving;
                dir = d;
                changedAt = nowMs;
                return;
            }
            if (s.Floor != floor)
            {
                // Two floor changes one floor apart while moving: a travel-time sample.
                if (moving && s.Moving && Math.Abs(s.Floor - floor) == 1)
                {
                    double dt = nowMs - changedAt;
                    if (dt > 300 && dt < 15000) FloorMs = 0.5 * FloorMs + 0.5 * dt;
                }
                floor = s.Floor;
                changedAt = nowMs;
            }
            else if (s.Moving && !moving)
            {
                changedAt = nowMs; // started from a standstill
            }
            moving = s.Moving;
            dir = d;
        }

        /// <summary>Car position in floors (e.g. 3.4 = 40 % from 3 to 4), NaN if never heard.</summary>
        public double Position(double nowMs)
        {
            if (!known) return double.NaN;
            if (!moving || dir == 0) return floor;
            double k = Math.Min(Math.Max(0, (nowMs - changedAt) / FloorMs), 0.95);
            return floor + dir * k;
        }
    }

    /// <summary>A task as GET /tasks shows it, the fields the 3D view uses.</summary>
    public sealed class TaskView
    {
        // app.js 24.
        public static readonly HashSet<string> Active = new HashSet<string> { "QUEUED", "ASSIGNED", "RUNNING" };
        [JsonProperty("id")] public string Id = "";
        [JsonProperty("state")] public string State = "";
        [JsonProperty("robot")] public string Robot = "";
        [JsonProperty("leg")] public int Leg;
        [JsonProperty("waypoints")] public List<Waypoint> Waypoints = new List<Waypoint>();

        public sealed class Waypoint
        {
            [JsonProperty("x")] public double X;
            [JsonProperty("y")] public double Y;
            [JsonProperty("floor")] public int Floor;
        }

        static readonly JsonSerializerSettings Json = new JsonSerializerSettings
        {
            DateParseHandling = DateParseHandling.None,
            MissingMemberHandling = MissingMemberHandling.Ignore,
        };

        public static List<TaskView> Parse(string json) =>
            JsonConvert.DeserializeObject<List<TaskView>>(json, Json) ?? new List<TaskView>();

        public static int CountActive(IEnumerable<TaskView> tasks)
        {
            int n = 0;
            if (tasks != null) foreach (var t in tasks) if (Active.Contains(t.State)) n++;
            return n;
        }
    }

    /// <summary>An event from GET /events (fleet.Event), with describe() (app.js 379-393) in English.</summary>
    public sealed class FleetEvent
    {
        [JsonProperty("seq")] public ulong Seq;
        [JsonProperty("time")] public string Time = "";
        [JsonProperty("robot")] public string Robot = "";
        [JsonProperty("type")] public string Type = "";
        [JsonProperty("task")] public string Task = "";
        [JsonProperty("elevator")] public string Elevator = "";
        [JsonProperty("from")] public string From = "";
        [JsonProperty("to")] public string To = "";
        [JsonProperty("detail")] public string Detail = "";

        static readonly JsonSerializerSettings Json = new JsonSerializerSettings
        {
            DateParseHandling = DateParseHandling.None,
            MissingMemberHandling = MissingMemberHandling.Ignore,
        };

        public static List<FleetEvent> Parse(string json) =>
            JsonConvert.DeserializeObject<List<FleetEvent>>(json, Json) ?? new List<FleetEvent>();

        public string Describe()
        {
            string Arrow(string a, string b) => $"{(string.IsNullOrEmpty(a) ? "-" : a)} -> {(string.IsNullOrEmpty(b) ? "-" : b)}";
            string Opt(string s) => string.IsNullOrEmpty(s) ? "" : s;
            switch (Type)
            {
                case "seen": return $"{Robot} first seen";
                case "conn": return $"{Robot} conn {Arrow(From, To)}{(string.IsNullOrEmpty(Detail) ? "" : $" ({Detail})")}";
                case "mode": return $"{Robot} mode {Arrow(From, To)}";
                case "error": return $"{Robot} error {(string.IsNullOrEmpty(To) ? "cleared" : To)}";
                case "task": return $"{Task} {(string.IsNullOrEmpty(Robot) ? "" : $"[{Robot}] ")}{Opt(Detail)}";
                case "battery": return $"{Robot} battery {Opt(Detail)}";
                case "adapter": return $"adapter {Opt(Detail)}";
                case "elevator_overdue": return $"{Robot} elevator overdue";
                case "elevator": return $"{Opt(Elevator)} {(string.IsNullOrEmpty(Robot) ? "" : $"[{Robot}] ")}{Opt(Detail)}";
                default: return $"{Opt(Robot)} {Type} {Opt(Detail)}";
            }
        }
    }
}
