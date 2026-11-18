// Builds Assets/Scenes/Dashboard.unity from code, so the 3D dashboard is
// reproducible in batchmode: a camera, a light, flat ambient light and the
// DashboardApp with its material assets (created here too). Referencing the
// materials from the scene is what keeps their shader variants in the build.
//   Unity.exe -batchmode -quit -projectPath unity/FleetUnity
//             -executeMethod Fleet.EditorTools.DashboardScene.Build
using System.IO;
using Fleet.Dashboard3D;
using UnityEditor;
using UnityEditor.SceneManagement;
using UnityEngine;
using UnityEngine.Rendering;

namespace Fleet.EditorTools
{
    public static class DashboardScene
    {
        public const string ScenePath = "Assets/Scenes/Dashboard.unity";
        const string MaterialDir = "Assets/Fleet/Dashboard/Materials";

        [MenuItem("Fleet/Rebuild Dashboard Scene")]
        public static void Build()
        {
            Directory.CreateDirectory(MaterialDir);
            var opaque = MaterialAsset("DashOpaque", "Standard", m => m.SetFloat("_Glossiness", 0.15f));
            var fade = MaterialAsset("DashFade", "Standard", m =>
            {
                // Standard shader "Fade" mode, as its inspector sets it.
                m.SetFloat("_Mode", 2);
                m.SetOverrideTag("RenderType", "Transparent");
                m.SetInt("_SrcBlend", (int)BlendMode.SrcAlpha);
                m.SetInt("_DstBlend", (int)BlendMode.OneMinusSrcAlpha);
                m.SetInt("_ZWrite", 0);
                m.DisableKeyword("_ALPHATEST_ON");
                m.EnableKeyword("_ALPHABLEND_ON");
                m.DisableKeyword("_ALPHAPREMULTIPLY_ON");
                m.SetFloat("_Glossiness", 0.1f);
                m.renderQueue = (int)RenderQueue.Transparent;
            });
            var lines = MaterialAsset("DashLines", "Sprites/Default", null); // vertex colours, alpha blended
            var text = MaterialAsset("DashText", "GUI/Text Shader", null);   // TextMesh labels use the font's own material with this shader

            var scene = EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
            RenderSettings.skybox = null;
            RenderSettings.ambientMode = AmbientMode.Flat;
            RenderSettings.ambientLight = new Color(0.55f, 0.57f, 0.62f);
            RenderSettings.fog = false;

            var cam = new GameObject("Main Camera").AddComponent<Camera>();
            cam.tag = "MainCamera";
            cam.clearFlags = CameraClearFlags.SolidColor;
            cam.backgroundColor = new Color(0.075f, 0.085f, 0.11f);
            cam.nearClipPlane = 0.1f;
            cam.farClipPlane = 1000f;
            cam.fieldOfView = 45f;
            cam.transform.position = new Vector3(0, 20, -30);

            var sun = new GameObject("Sun").AddComponent<Light>();
            sun.type = LightType.Directional;
            sun.intensity = 0.9f;
            sun.shadows = LightShadows.None;
            sun.transform.rotation = Quaternion.Euler(55, -30, 0);

            var app = new GameObject("FleetDashboard").AddComponent<DashboardApp>();
            app.OpaqueMaterial = opaque;
            app.FadeMaterial = fade;
            app.LinesMaterial = lines;
            app.TextMaterial = text;

            Directory.CreateDirectory(Path.GetDirectoryName(ScenePath));
            EditorSceneManager.SaveScene(scene, ScenePath);
            AssetDatabase.SaveAssets();
            AssetDatabase.Refresh();
            Debug.Log($"[build] dashboard scene written: {ScenePath}");
        }

        static Material MaterialAsset(string name, string shader, System.Action<Material> setup)
        {
            var path = $"{MaterialDir}/{name}.mat";
            var m = AssetDatabase.LoadAssetAtPath<Material>(path);
            var s = Shader.Find(shader);
            if (s == null) throw new System.Exception($"shader {shader} not found");
            if (m == null)
            {
                m = new Material(s) { name = name };
                AssetDatabase.CreateAsset(m, path);
            }
            m.shader = s;
            setup?.Invoke(m);
            EditorUtility.SetDirty(m);
            return m;
        }
    }
}
