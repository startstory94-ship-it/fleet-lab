// -capture <dir>: saves the screen (HUD included) as numbered PNGs every
// -captureMs for -captureSeconds, for turning into a GIF. Encoding runs on
// the main thread at end of frame; the file write does not.
using System.Collections;
using System.IO;
using System.Threading.Tasks;
using UnityEngine;

namespace Fleet.Dashboard3D
{
    public sealed class FrameCapture : MonoBehaviour
    {
        public string Dir;
        public int IntervalMs = 200;
        public double Seconds = 60;
        public bool QuitWhenDone;
        public int Saved { get; private set; }

        IEnumerator Start()
        {
            Directory.CreateDirectory(Dir);
            Debug.Log($"[dash] capturing to {Dir} every {IntervalMs} ms for {Seconds} s");
            var end = Time.realtimeSinceStartupAsDouble + Seconds;
            var next = Time.realtimeSinceStartupAsDouble;
            while (Time.realtimeSinceStartupAsDouble < end)
            {
                yield return new WaitForEndOfFrame();
                if (Time.realtimeSinceStartupAsDouble < next) continue;
                // After a slow frame, do not burst to catch up: the GIF wants even spacing.
                next = System.Math.Max(next + IntervalMs / 1000.0, Time.realtimeSinceStartupAsDouble);
                var tex = ScreenCapture.CaptureScreenshotAsTexture();
                var png = tex.EncodeToPNG();
                Destroy(tex);
                var path = Path.Combine(Dir, $"frame_{++Saved:D5}.png");
                Task.Run(() => File.WriteAllBytes(path, png));
            }
            Debug.Log($"[dash] capture done: {Saved} frames in {Dir}");
            if (QuitWhenDone)
            {
                yield return new WaitForSecondsRealtime(1); // let the last writes finish
                Application.Quit();
            }
        }
    }
}
