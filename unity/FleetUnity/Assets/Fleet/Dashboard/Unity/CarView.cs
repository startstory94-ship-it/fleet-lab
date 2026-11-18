// One elevator: the shaft over every floor it serves, at the car outline
// next to its lobby (carBox, app.js 182-190), and the car moving in it with
// its door on the lobby side (drawCars, app.js 780-813: green when open).
using Fleet.Dashboard;
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public sealed class CarView : MonoBehaviour
    {
        public const float CarHeight = 2.2f;
        static readonly Color Purple = Prims.ColorOf(Look.Elevator);
        static readonly Color OpenGreen = Prims.ColorOf(Look.Charging);

        public readonly CarMotion Motion = new CarMotion();
        public ElevatorView View;
        CarBox box;
        Transform car, shaft;
        GameObject doorL, doorR;
        TextMesh label;
        float y = float.NaN, open;
        string shaftKey;
        System.Func<double, float> floorY;

        public static CarView Create(Transform parent, ElevatorView v, System.Func<double, float> floorY)
        {
            var root = new GameObject("car " + v.Id);
            root.transform.SetParent(parent, false);
            var c = root.AddComponent<CarView>();
            c.floorY = floorY;
            c.View = v;
            c.box = Elevators.Box(v.Lobby, v.Capacity);
            c.car = new GameObject("car").transform;
            c.car.SetParent(root.transform, false);

            var size = new Vector3((float)(c.box.X1 - c.box.X0), CarHeight, (float)(c.box.Y1 - c.box.Y0));
            var mid = new Vector3(size.x / 2, CarHeight / 2, 0); // car origin: door side, floor level, centre line
            Prims.Box(c.car, "cabin", mid, size, new Color(Purple.r, Purple.g, Purple.b, 0.16f), fade: true);
            Prims.Box(c.car, "floor", new Vector3(size.x / 2, 0.02f, 0), new Vector3(size.x, 0.04f, size.z), new Color(0.24f, 0.18f, 0.33f));
            Prims.Edges(c.car, mid, size, 0.05f, Purple);
            float doorH = CarHeight * 0.92f, half = size.z / 2;
            c.doorL = Prims.Box(c.car, "door-l", new Vector3(0, doorH / 2, -half / 2), new Vector3(0.06f, doorH, half), Purple);
            c.doorR = Prims.Box(c.car, "door-r", new Vector3(0, doorH / 2, half / 2), new Vector3(0.06f, doorH, half), Purple);
            c.label = Prims.Label(c.car, "label", v.Id, new Color(0.85f, 0.75f, 1f), 44);
            c.label.transform.localPosition = new Vector3(size.x / 2, CarHeight + 0.3f, 0);
            return c;
        }

        /// <summary>Where slot i's robot stands, relative to the world root.</summary>
        public Vector3 SlotPosition(int i)
        {
            var p = box.Slot(i);
            return Prims.W(p.X, p.Y, car.localPosition.y + 0.04f);
        }

        public Vector3 CarCentre => car.localPosition + new Vector3((float)(box.X1 - box.X0) / 2, CarHeight / 2, 0);

        /// <summary>The shaft: four posts from the lowest floor to the top car
        /// roof, and a door frame on every served floor.</summary>
        void BuildShaft(int lo, int hi)
        {
            if (shaft != null) Destroy(shaft.gameObject);
            shaft = new GameObject("shaft").transform;
            shaft.SetParent(transform, false);
            float y0 = floorY(lo), y1 = floorY(hi) + CarHeight + 0.4f;
            var dim = new Color(0.45f, 0.36f, 0.6f);
            foreach (var x in new[] { box.X0, box.X1 })
                foreach (var z in new[] { box.Y0, box.Y1 })
                    Prims.Box(shaft, "post", Prims.W(x, z, (y0 + y1) / 2), new Vector3(0.06f, y1 - y0, 0.06f), dim);
            foreach (var f in View.Floors)
            {
                float fy = floorY(f);
                Prims.Box(shaft, "sill", Prims.W(box.X0, (box.Y0 + box.Y1) / 2, fy + 0.01f), new Vector3(0.1f, 0.02f, (float)(box.Y1 - box.Y0)), dim);
                Prims.Box(shaft, "lintel", Prims.W(box.X0, (box.Y0 + box.Y1) / 2, fy + CarHeight), new Vector3(0.06f, 0.06f, (float)(box.Y1 - box.Y0)), dim);
            }
        }

        public void Observe(ElevatorView v, double nowMs)
        {
            View = v;
            Motion.Observe(v.State, nowMs);
        }

        public void Tick(double nowMs, float dt, Camera cam, bool rebuild)
        {
            var key = View.Floors.Count == 0 ? "" : $"{View.Floors[0]}..{View.Floors[View.Floors.Count - 1]}";
            if (rebuild || key != shaftKey)
            {
                shaftKey = key;
                int lo = int.MaxValue, hi = int.MinValue;
                foreach (var f in View.Floors) { lo = Mathf.Min(lo, f); hi = Mathf.Max(hi, f); }
                if (lo <= hi) BuildShaft(lo, hi);
            }
            double pos = Motion.Position(nowMs);
            car.gameObject.SetActive(!double.IsNaN(pos));
            if (double.IsNaN(pos)) return;
            // Positions come from 1 Hz polls; ease toward them instead of stepping.
            float target = floorY(pos);
            y = float.IsNaN(y) || Mathf.Abs(target - y) > 30 ? target : Mathf.Lerp(y, target, 1 - Mathf.Exp(-dt * 6));
            car.localPosition = Prims.W(box.X0, (box.Y0 + box.Y1) / 2, y);

            var s = View.State;
            float want = s == null ? 0 : s.Door == "open" ? 1 : s.Door == "opening" ? 0.7f : s.Door == "closing" ? 0.3f : 0;
            open = Mathf.MoveTowards(open, want, dt * 1.5f);
            float half = (float)(box.Y1 - box.Y0) / 2;
            doorL.transform.localPosition = new Vector3(0, doorL.transform.localPosition.y, -half / 2 - open * half * 0.9f);
            doorR.transform.localPosition = new Vector3(0, doorR.transform.localPosition.y, half / 2 + open * half * 0.9f);
            var doorColor = s != null && s.Door != "closed" ? OpenGreen : Purple;
            Prims.Recolor(doorL, doorColor);
            Prims.Recolor(doorR, doorColor);

            string dir = s == null ? "" : s.Dir == "up" ? " up" : s.Dir == "down" ? " down" : "";
            string text = s == null ? $"{View.Id} ?" : $"{View.Id} {s.Floor}F{dir} · door {s.Door}" + (s.DoorZone > 0 ? $" · zone {s.DoorZone}" : "");
            if (!View.Responding) text += " · not responding";
            if (label.text != text) label.text = text;
            Prims.Face(label, cam, 15);
        }
    }
}
