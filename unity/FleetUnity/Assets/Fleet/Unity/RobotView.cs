// Primitives for one robot plus the stage (floor, camera, light). The fleet
// map (x, y) is Unity (x, z): seen from above, x points right and y up, as on
// the dashboard.
using Fleet.Core;
using UnityEngine;

namespace Fleet.UnityApp
{
    public sealed class RobotView : MonoBehaviour
    {
        FleetRobot robot;
        Renderer body;
        TextMesh label;
        string shownMode;

        public static RobotView Create(FleetRobot r, Transform parent)
        {
            var root = new GameObject(r.Name);
            root.transform.SetParent(parent, false);
            var v = root.AddComponent<RobotView>();
            v.robot = r;

            var b = GameObject.CreatePrimitive(PrimitiveType.Cylinder);
            b.name = "body";
            Object.Destroy(b.GetComponent<Collider>()); // kinematic only, no physics
            b.transform.SetParent(root.transform, false);
            b.transform.localScale = new Vector3(0.5f, 0.15f, 0.5f);
            b.transform.localPosition = new Vector3(0, 0.15f, 0);
            v.body = b.GetComponent<Renderer>();

            var nose = GameObject.CreatePrimitive(PrimitiveType.Cube); // direction marker
            nose.name = "nose";
            Object.Destroy(nose.GetComponent<Collider>());
            nose.transform.SetParent(root.transform, false);
            nose.transform.localScale = new Vector3(0.1f, 0.1f, 0.25f);
            nose.transform.localPosition = new Vector3(0, 0.32f, 0.2f);

            if (!Application.isBatchMode)
            {
                var l = new GameObject("label");
                l.transform.SetParent(root.transform, false);
                l.transform.localPosition = new Vector3(0, 0.6f, 0);
                l.transform.rotation = Quaternion.Euler(90, 0, 0);
                v.label = l.AddComponent<TextMesh>();
                v.label.font = Resources.GetBuiltinResource<Font>("LegacyRuntime.ttf");
                v.label.GetComponent<MeshRenderer>().sharedMaterial = v.label.font.material;
                v.label.characterSize = 0.08f;
                v.label.fontSize = 48;
                v.label.anchor = TextAnchor.LowerCenter;
            }
            v.Sync();
            return v;
        }

        public void Sync()
        {
            var s = robot.Sim;
            var mode = robot.Ride.Overlay()?.Mode ?? s.Mode;
            transform.localPosition = new Vector3((float)s.X, 0, (float)s.Y);
            // Fleet yaw is counter-clockwise from +x; Unity's Y rotation is clockwise from +z.
            transform.localRotation = Quaternion.Euler(0, 90f - (float)(s.Yaw * Mathf.Rad2Deg), 0);
            if (shownMode != mode)
            {
                shownMode = mode;
                body.material.color = ColorOf(mode);
            }
            if (label != null)
            {
                label.transform.rotation = Quaternion.Euler(90, 0, 0);
                label.text = $"{robot.Name} F{s.Floor}\n{mode} {s.Battery:F0}%";
            }
        }

        static Color ColorOf(string mode)
        {
            switch (mode)
            {
                case Modes.Moving: return new Color(0.2f, 0.5f, 0.9f);
                case Modes.Docking: return new Color(0.9f, 0.7f, 0.2f);
                case Modes.Charging: return new Color(0.3f, 0.8f, 0.3f);
                case Modes.Error: return new Color(0.9f, 0.2f, 0.2f);
                case Modes.WaitingElevator:
                case Modes.Boarding:
                case Modes.Riding:
                case Modes.Exiting: return new Color(0.6f, 0.35f, 0.85f); // purple, as the dashboards draw riders
                default: return new Color(0.75f, 0.75f, 0.75f);
            }
        }

        public static void BuildStage(BootArgs a, Transform parent)
        {
            float cx = (float)(a.OriginX + a.Spacing * (a.Robots - 1) / 2.0), cz = (float)a.OriginY;
            var floor = GameObject.CreatePrimitive(PrimitiveType.Plane);
            floor.name = "floor";
            floor.transform.SetParent(parent, false);
            floor.transform.localPosition = new Vector3(cx, 0, cz);
            floor.transform.localScale = new Vector3(4, 1, 4); // 40 x 40 m
            floor.GetComponent<Renderer>().material.color = new Color(0.25f, 0.27f, 0.3f);

            if (Camera.main == null)
            {
                var cam = new GameObject("camera").AddComponent<Camera>();
                cam.tag = "MainCamera";
                cam.transform.position = new Vector3(cx, 18, cz);
                cam.transform.rotation = Quaternion.Euler(90, 0, 0); // top-down, +z (map y) up
                cam.backgroundColor = new Color(0.1f, 0.1f, 0.12f);
                cam.clearFlags = CameraClearFlags.SolidColor;
            }
            if (Object.FindAnyObjectByType<Light>() == null)
            {
                var light = new GameObject("sun").AddComponent<Light>();
                light.type = LightType.Directional;
                light.transform.rotation = Quaternion.Euler(60, 30, 0);
            }
        }
    }
}
