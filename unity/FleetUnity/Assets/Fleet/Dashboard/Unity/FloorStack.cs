// The floors, stacked: one translucent slab per floor over the shared map
// bounds (so robots on lower floors stay visible through the ones above),
// a 1 m grid (gridLines, app.js 54-70), an outline, the floor number, the
// lobby spot of each car and a hole where the shaft passes.
using System.Collections.Generic;
using Fleet.Dashboard;
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public sealed class FloorStack : MonoBehaviour
    {
        const float Thick = 0.08f;
        static readonly Color SlabColor = new Color(0.36f, 0.42f, 0.52f, 0.26f);
        static readonly Color EdgeColor = new Color(0.55f, 0.6f, 0.68f);
        static readonly Color GridMinor = new Color(0.6f, 0.66f, 0.75f, 0.16f);
        static readonly Color GridMajor = new Color(0.7f, 0.76f, 0.85f, 0.38f);

        readonly Dictionary<int, TextMesh> labels = new Dictionary<int, TextMesh>();
        string key;
        public System.Func<double, float> FloorY;
        public List<int> Floors = new List<int>();
        public (double minX, double minY, double maxX, double maxY) Box;

        /// <summary>Rebuilds when the floors, the bounds or the cars change.</summary>
        public void Sync(List<int> floors, (double minX, double minY, double maxX, double maxY) box, List<ElevatorView> cars, float floorHeight)
        {
            var k = $"{string.Join(",", floors)}|{box}|{floorHeight}|";
            foreach (var c in cars) k += $"{c.Id}:{c.Lobby.X},{c.Lobby.Y},{c.Capacity};";
            if (k == key) return;
            key = k;
            Floors = floors;
            Box = box;
            foreach (Transform child in transform) Destroy(child.gameObject);
            labels.Clear();
            foreach (var f in floors) BuildFloor(f, box, cars);
        }

        void BuildFloor(int f, (double minX, double minY, double maxX, double maxY) b, List<ElevatorView> cars)
        {
            var root = new GameObject($"floor {f}").transform;
            root.SetParent(transform, false);
            float y = FloorY(f);

            // Slab minus the first car's shaft (more cars would just overlap the slab).
            var rects = new List<(double x0, double y0, double x1, double y1)> { (b.minX, b.minY, b.maxX, b.maxY) };
            if (cars.Count > 0)
            {
                var cb = Elevators.Box(cars[0].Lobby, cars[0].Capacity);
                rects = Subtract(rects[0], (cb.X0, cb.Y0, cb.X1, cb.Y1));
            }
            foreach (var r in rects)
            {
                if (r.x1 - r.x0 < 0.01 || r.y1 - r.y0 < 0.01) continue;
                Prims.Box(root, "slab", Prims.W((r.x0 + r.x1) / 2, (r.y0 + r.y1) / 2, y - Thick / 2),
                    new Vector3((float)(r.x1 - r.x0), Thick, (float)(r.y1 - r.y0)), SlabColor, fade: true);
            }

            // Outline.
            float w = 0.05f, sx = (float)(b.maxX - b.minX), sz = (float)(b.maxY - b.minY);
            double cx = (b.minX + b.maxX) / 2, cz = (b.minY + b.maxY) / 2;
            Prims.Box(root, "edge", Prims.W(cx, b.minY, y), new Vector3(sx, w, w), EdgeColor);
            Prims.Box(root, "edge", Prims.W(cx, b.maxY, y), new Vector3(sx, w, w), EdgeColor);
            Prims.Box(root, "edge", Prims.W(b.minX, cz, y), new Vector3(w, w, sz), EdgeColor);
            Prims.Box(root, "edge", Prims.W(b.maxX, cz, y), new Vector3(w, w, sz), EdgeColor);

            BuildGrid(root, b, y + 0.005f);

            foreach (var c in cars)
            {
                if (!c.Floors.Contains(f)) continue;
                // The lobby spot where robots wait for the car.
                var lobby = Prims.Box(root, "lobby", Prims.W(c.Lobby.X, c.Lobby.Y, y + 0.01f), new Vector3(0.7f, 0.02f, 0.7f),
                    Prims.ColorOf(Look.Elevator, 0.35f), fade: true);
                lobby.name = $"lobby {c.Id}";
            }

            var label = Prims.Label(root, "label", $"{f}F", new Color(0.85f, 0.88f, 0.93f), 64);
            label.anchor = TextAnchor.MiddleRight;
            label.transform.localPosition = Prims.W(b.minX - 0.4, b.minY, y + 0.2f);
            labels[f] = label;
        }

        void BuildGrid(Transform parent, (double minX, double minY, double maxX, double maxY) b, float y)
        {
            var verts = new List<Vector3>();
            var colors = new List<Color>();
            foreach (var (v, major) in MapGrid.Lines(b.minX, b.maxX, 1))
            {
                verts.Add(Prims.W(v, b.minY, y));
                verts.Add(Prims.W(v, b.maxY, y));
                colors.Add(major ? GridMajor : GridMinor);
                colors.Add(major ? GridMajor : GridMinor);
            }
            foreach (var (v, major) in MapGrid.Lines(b.minY, b.maxY, 1))
            {
                verts.Add(Prims.W(b.minX, v, y));
                verts.Add(Prims.W(b.maxX, v, y));
                colors.Add(major ? GridMajor : GridMinor);
                colors.Add(major ? GridMajor : GridMinor);
            }
            var idx = new int[verts.Count];
            for (int i = 0; i < idx.Length; i++) idx[i] = i;
            var mesh = new Mesh { name = "grid" };
            mesh.SetVertices(verts);
            mesh.SetColors(colors);
            mesh.SetIndices(idx, MeshTopology.Lines, 0);
            var g = new GameObject("grid");
            g.transform.SetParent(parent, false);
            g.AddComponent<MeshFilter>().sharedMesh = mesh;
            g.AddComponent<MeshRenderer>().sharedMaterial = Prims.Lines;
        }

        /// <summary>r minus hole as up to four rectangles.</summary>
        static List<(double x0, double y0, double x1, double y1)> Subtract((double x0, double y0, double x1, double y1) r,
            (double x0, double y0, double x1, double y1) h)
        {
            double hx0 = System.Math.Max(r.x0, h.x0), hx1 = System.Math.Min(r.x1, h.x1);
            double hy0 = System.Math.Max(r.y0, h.y0), hy1 = System.Math.Min(r.y1, h.y1);
            if (hx0 >= hx1 || hy0 >= hy1) return new List<(double, double, double, double)> { r };
            return new List<(double, double, double, double)>
            {
                (r.x0, r.y0, hx0, r.y1),      // left
                (hx1, r.y0, r.x1, r.y1),      // right
                (hx0, r.y0, hx1, hy0),        // below the hole
                (hx0, hy1, hx1, r.y1),        // above the hole
            };
        }

        public void FaceLabels(Camera cam)
        {
            foreach (var l in labels.Values) Prims.Face(l, cam, 22);
        }
    }
}
