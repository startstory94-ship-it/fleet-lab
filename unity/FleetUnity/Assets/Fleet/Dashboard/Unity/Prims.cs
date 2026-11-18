// Primitive building blocks of the 3D view. Materials come from assets the
// scene references (DashboardScene.cs creates them), so the player build
// keeps their shader variants; runtime-only Shader.Find materials can come
// out magenta or opaque in a build.
using Fleet.Dashboard;
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public static class Prims
    {
        public static Material Opaque, Fade, Lines;
        public static Font Font;

        /// <summary>Map (x, y) at height h: map x is Unity x, map y is Unity z
        /// (as in the robot player's RobotView), so seen from above +y is up.</summary>
        public static Vector3 W(double x, double y, float h) => new Vector3((float)x, h, (float)y);

        /// <summary>Fleet yaw is counter-clockwise from +x; Unity's Y rotation is clockwise from +z.</summary>
        public static Quaternion Yaw(double rad) => Quaternion.Euler(0, 90f - (float)(rad * Mathf.Rad2Deg), 0);

        public static Color Rgb(int rgb, float a = 1) =>
            new Color(((rgb >> 16) & 0xff) / 255f, ((rgb >> 8) & 0xff) / 255f, (rgb & 0xff) / 255f, a);

        public static Color ColorOf(Look l, float a = 1) => Rgb(RobotStyle.Rgb(l), a);

        public static GameObject Box(Transform parent, string name, Vector3 center, Vector3 size, Color color, bool fade = false)
        {
            var g = Make(PrimitiveType.Cube, parent, name, color, fade);
            g.transform.localPosition = center;
            g.transform.localScale = size;
            return g;
        }

        public static GameObject Make(PrimitiveType type, Transform parent, string name, Color color, bool fade = false)
        {
            var g = GameObject.CreatePrimitive(type);
            g.name = name;
            Object.Destroy(g.GetComponent<Collider>()); // a viewer: no physics
            g.transform.SetParent(parent, false);
            var r = g.GetComponent<Renderer>();
            r.sharedMaterial = new Material(fade ? Fade : Opaque) { color = color };
            r.shadowCastingMode = UnityEngine.Rendering.ShadowCastingMode.Off;
            r.receiveShadows = false;
            return g;
        }

        public static void Recolor(GameObject g, Color c) => g.GetComponent<Renderer>().sharedMaterial.color = c;

        public static TextMesh Label(Transform parent, string name, string text, Color color, int size = 48)
        {
            var g = new GameObject(name);
            g.transform.SetParent(parent, false);
            var t = g.AddComponent<TextMesh>();
            t.font = Font;
            g.GetComponent<MeshRenderer>().sharedMaterial = Font.material;
            t.fontSize = size;
            t.characterSize = 0.05f;
            t.anchor = TextAnchor.LowerCenter;
            t.alignment = TextAlignment.Center;
            t.color = color;
            t.text = text;
            return t;
        }

        // World height of a TextMesh line per font size unit at scale 1 and
        // characterSize 0.05 (measured from a capture of the built player).
        const float LineHeightPerSize = 0.0045f;

        /// <summary>Turns a label to the camera and sizes it to about `pixels`
        /// tall on screen, so names stay readable at any zoom.</summary>
        public static void Face(TextMesh t, Camera cam, float pixels = 14)
        {
            if (cam == null) return;
            var tr = t.transform;
            tr.rotation = cam.transform.rotation;
            float d = Vector3.Dot(tr.position - cam.transform.position, cam.transform.forward);
            if (d <= 0.01f) return;
            float worldPerPixel = 2 * d * Mathf.Tan(cam.fieldOfView * 0.5f * Mathf.Deg2Rad) / Mathf.Max(1, Screen.height);
            float s = pixels * worldPerPixel / (LineHeightPerSize * t.fontSize);
            tr.localScale = Vector3.one * s / Mathf.Max(1e-4f, tr.parent != null ? tr.parent.lossyScale.x : 1);
        }

        /// <summary>The 12 edges of a box as thin bars (GL lines are one pixel wide).</summary>
        public static void Edges(Transform parent, Vector3 center, Vector3 size, float w, Color color)
        {
            var h = size * 0.5f;
            for (int a = 0; a < 3; a++)
            {
                for (int i = 0; i < 4; i++)
                {
                    var c = center;
                    var s = new Vector3(w, w, w);
                    float s1 = (i & 1) == 0 ? -1 : 1, s2 = (i & 2) == 0 ? -1 : 1;
                    if (a == 0) { s.x = size.x + w; c.y += s1 * h.y; c.z += s2 * h.z; }
                    if (a == 1) { s.y = size.y + w; c.x += s1 * h.x; c.z += s2 * h.z; }
                    if (a == 2) { s.z = size.z + w; c.x += s1 * h.x; c.y += s2 * h.y; }
                    Box(parent, "edge", c, s, color);
                }
            }
        }
    }
}
