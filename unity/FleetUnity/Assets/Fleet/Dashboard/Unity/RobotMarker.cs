// One robot in the 3D view, drawn like the 2D map's drawRobot (app.js
// 749-778): a disc in the legend colour with a heading nose, STALE faded with
// a dashed ring, OFFLINE dark with a cross, the name above.
using Fleet.Dashboard;
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public sealed class RobotMarker : MonoBehaviour
    {
        GameObject solid, faded, nose, ring, cross;
        TextMesh label;
        string shown;

        public static RobotMarker Create(Transform parent, string name)
        {
            var root = new GameObject(name);
            root.transform.SetParent(parent, false);
            var m = root.AddComponent<RobotMarker>();
            var t = root.transform;
            var body = new Vector3(0.55f, 0.15f, 0.55f);
            m.solid = Prims.Make(PrimitiveType.Cylinder, t, "body", Color.gray);
            m.solid.transform.localScale = body;
            m.solid.transform.localPosition = new Vector3(0, 0.15f, 0);
            m.faded = Prims.Make(PrimitiveType.Cylinder, t, "body-stale", Color.gray, fade: true);
            m.faded.transform.localScale = body;
            m.faded.transform.localPosition = new Vector3(0, 0.15f, 0);
            m.nose = Prims.Box(t, "nose", new Vector3(0, 0.34f, 0.2f), new Vector3(0.1f, 0.08f, 0.3f), new Color(0.11f, 0.14f, 0.19f));

            // STALE: a dashed ring (drawRobot's setLineDash), as 12 short bars.
            m.ring = new GameObject("stale-ring");
            m.ring.transform.SetParent(t, false);
            for (int i = 0; i < 12; i++)
            {
                float a = i * 30f;
                var d = Prims.Box(m.ring.transform, "dash", Vector3.zero, new Vector3(0.14f, 0.05f, 0.05f), Prims.ColorOf(Look.Idle));
                d.transform.localRotation = Quaternion.Euler(0, a, 0);
                d.transform.localPosition = Quaternion.Euler(0, a, 0) * new Vector3(0, 0.3f, 0.36f);
            }
            // OFFLINE: a white cross on top (app.js 766-771).
            m.cross = new GameObject("offline-cross");
            m.cross.transform.SetParent(t, false);
            foreach (var a in new[] { 45f, -45f })
            {
                var b = Prims.Box(m.cross.transform, "bar", new Vector3(0, 0.31f, 0), new Vector3(0.06f, 0.02f, 0.45f), Color.white);
                b.transform.localRotation = Quaternion.Euler(0, a, 0);
            }
            m.label = Prims.Label(t, "label", name, new Color(0.92f, 0.94f, 0.97f), 40);
            m.label.transform.localPosition = new Vector3(0, 0.5f, 0);
            return m;
        }

        /// <summary>conn is the robot's, or IN_ELEVATOR for one drawn in a car.
        /// labelLevel stacks names, so neighbours in a car do not overlap
        /// (app.js 810 puts them above and below).</summary>
        public void Show(RobotView v, string conn, Vector3 pos, Quaternion rot, string extra, Camera cam, int labelLevel = 0)
        {
            transform.localPosition = pos;
            transform.localRotation = rot;
            var look = RobotStyle.Of(conn, v?.Mode ?? "", v?.Error ?? "");
            bool stale = conn == "STALE", offline = conn == "OFFLINE";
            string key = $"{look}|{stale}|{offline}";
            if (key != shown)
            {
                shown = key;
                solid.SetActive(!stale);
                faded.SetActive(stale);
                ring.SetActive(stale);
                cross.SetActive(offline);
                // drawRobot: STALE at half alpha (app.js 753).
                if (stale) Prims.Recolor(faded, Prims.ColorOf(look, 0.35f));
                else Prims.Recolor(solid, Prims.ColorOf(look));
            }
            string text = name + (v != null && v.NeedsCharge ? " (low)" : "") + extra;
            if (label.text != text) label.text = text;
            label.transform.localPosition = new Vector3(0, 0.5f + 0.45f * labelLevel, 0);
            Prims.Face(label, cam, 15);
        }
    }
}
