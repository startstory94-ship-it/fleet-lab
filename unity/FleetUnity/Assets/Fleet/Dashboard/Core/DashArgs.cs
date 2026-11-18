// Command-line options of the 3D dashboard:
//   -api http://127.0.0.1:8080  -floorHeight m
//   -capture <dir> -captureMs n -captureSeconds n [-captureQuit]
//   -follow (follow the car)  -focus F (start on floor F)
//   -yaw deg -pitch deg -distance m (camera)
// Window size: Unity's own -screen-width / -screen-height. Unknown arguments
// are skipped, as in BootArgs.
using System;
using System.Collections.Generic;
using System.Globalization;

namespace Fleet.Dashboard
{
    public sealed class DashArgs
    {
        public string Api = "http://127.0.0.1:8080";
        public double FloorHeight = 5.0;   // taller than a real storey so floors read apart at a low camera
        public string CaptureDir;            // null: no capture
        public int CaptureMs = 200;
        public double CaptureSeconds = 60;
        public bool CaptureQuit;
        public bool Follow;
        public int? Focus;
        public double? Yaw, Pitch, Distance;
        public readonly List<string> Warnings = new List<string>();

        /// <summary>ws://host/ws/live for the API base (wss for https).</summary>
        public string LiveUrl
        {
            get
            {
                var u = new Uri(Api);
                var b = new UriBuilder(u) { Scheme = u.Scheme == "https" ? "wss" : "ws", Path = "/ws/live" };
                if (u.IsDefaultPort) b.Port = -1;
                return b.Uri.ToString();
            }
        }

        public static DashArgs Parse(IList<string> args)
        {
            var a = new DashArgs();
            for (int i = 0; i < args.Count; i++)
            {
                string key = args[i].ToLowerInvariant();
                string val = i + 1 < args.Count ? args[i + 1] : null;
                bool used = true;
                try
                {
                    switch (key)
                    {
                        case "-api":
                            var api = Need(val).TrimEnd('/');
                            if (!api.Contains("://")) api = "http://" + api;
                            new Uri(api); // throws UriFormatException
                            a.Api = api;
                            break;
                        case "-floorheight": a.FloorHeight = Math.Max(0.5, D(Need(val))); break;
                        case "-capture": a.CaptureDir = Need(val); break;
                        case "-capturems": a.CaptureMs = Math.Max(20, int.Parse(Need(val), CultureInfo.InvariantCulture)); break;
                        case "-captureseconds": a.CaptureSeconds = Math.Max(0, D(Need(val))); break;
                        case "-focus": a.Focus = int.Parse(Need(val), CultureInfo.InvariantCulture); break;
                        case "-yaw": a.Yaw = D(Need(val)); break;
                        case "-pitch": a.Pitch = D(Need(val)); break;
                        case "-distance": a.Distance = Math.Max(1, D(Need(val))); break;
                        case "-capturequit": a.CaptureQuit = true; used = false; break;
                        case "-follow": a.Follow = true; used = false; break;
                        default: used = false; break;
                    }
                }
                catch (Exception e) when (e is FormatException || e is OverflowException || e is UriFormatException)
                {
                    a.Warnings.Add($"bad value for {args[i]}: '{val}', default kept");
                }
                if (used) i++;
            }
            return a;
        }

        static string Need(string v) => v ?? throw new FormatException("missing value");
        static double D(string s) => double.Parse(s, NumberStyles.Float, CultureInfo.InvariantCulture);
    }
}
