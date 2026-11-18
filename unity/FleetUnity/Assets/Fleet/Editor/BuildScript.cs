// Batch build of the robot player:
//   Unity.exe -batchmode -quit -projectPath unity/FleetUnity
//             -executeMethod Fleet.EditorTools.BuildScript.BuildWindows [-buildOutput <exe>]
// Output defaults to unity/FleetUnity/Build/FleetRobots.exe.
using System;
using System.IO;
using UnityEditor;
using UnityEditor.Build;
using UnityEditor.Build.Reporting;
using UnityEditor.SceneManagement;
using UnityEngine;

namespace Fleet.EditorTools
{
    public static class BuildScript
    {
        const string ScenePath = "Assets/Scenes/Main.unity";

        [MenuItem("Fleet/Build Windows Player")]
        public static void BuildWindows()
        {
            var output = Arg("-buildOutput") ??
                         Path.GetFullPath(Path.Combine(Application.dataPath, "..", "Build", "FleetRobots.exe"));
            EnsureScene();

            // Mono, nothing stripped: MQTTnet and Newtonsoft use reflection.
            var target = NamedBuildTarget.Standalone;
            PlayerSettings.SetScriptingBackend(target, ScriptingImplementation.Mono2x);
            PlayerSettings.SetManagedStrippingLevel(target, ManagedStrippingLevel.Disabled);
            PlayerSettings.SetApiCompatibilityLevel(target, ApiCompatibilityLevel.NET_Standard);
            PlayerSettings.runInBackground = true;
            PlayerSettings.fullScreenMode = FullScreenMode.Windowed;
            PlayerSettings.defaultScreenWidth = 1280;
            PlayerSettings.defaultScreenHeight = 720;
            PlayerSettings.resizableWindow = true;
            PlayerSettings.productName = "FleetRobots";
            PlayerSettings.companyName = "fleet-lab";
            PlayerSettings.usePlayerLog = true;

            var report = BuildPipeline.BuildPlayer(new BuildPlayerOptions
            {
                scenes = new[] { ScenePath },
                locationPathName = output,
                target = BuildTarget.StandaloneWindows64,
                options = BuildOptions.None,
            });
            var s = report.summary;
            Debug.Log($"[build] {s.result}: {output} ({s.totalSize / (1024 * 1024)} MB, {s.totalErrors} errors, {s.totalTime.TotalSeconds:F0} s)");
            if (Application.isBatchMode && s.result != BuildResult.Succeeded)
                EditorApplication.Exit(1);
        }

        /// <summary>The bootstrap creates itself at start, so the scene only has to exist.</summary>
        static void EnsureScene()
        {
            if (!File.Exists(ScenePath))
            {
                Directory.CreateDirectory(Path.GetDirectoryName(ScenePath));
                var scene = EditorSceneManager.NewScene(NewSceneSetup.EmptyScene, NewSceneMode.Single);
                EditorSceneManager.SaveScene(scene, ScenePath);
                AssetDatabase.Refresh();
            }
            EditorBuildSettings.scenes = new[] { new EditorBuildSettingsScene(ScenePath, true) };
        }

        static string Arg(string name)
        {
            var a = Environment.GetCommandLineArgs();
            for (int i = 0; i < a.Length - 1; i++)
                if (string.Equals(a[i], name, StringComparison.OrdinalIgnoreCase))
                    return a[i + 1];
            return null;
        }
    }
}
