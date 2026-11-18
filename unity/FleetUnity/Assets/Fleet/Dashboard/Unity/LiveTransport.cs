// /ws/live transport. The interface is all DashboardApp uses, so a WebGL
// build can add a browser-WebSocket (jslib) implementation next to this one.
// The standalone one runs ClientWebSocket on a background task and hands
// frames to the main thread through a queue, like FleetMqtt's inbox.
using System;
using System.Collections.Concurrent;
using System.IO;
using System.Net.Http;
using System.Net.WebSockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Fleet.Dashboard;

namespace Fleet.Dashboard3D
{
    public enum LinkState { Connecting, Up, Down, Full }

    public interface ILiveTransport : IDisposable
    {
        LinkState State { get; }
        string Detail { get; }        // why it is down, or the next retry
        int Connects { get; }
        void Start();
        bool TryReceive(out string frame);
    }

    public sealed class ClientWebSocketTransport : ILiveTransport
    {
        readonly Uri live;
        readonly string httpBase;
        readonly ConcurrentQueue<string> inbox = new ConcurrentQueue<string>();
        readonly CancellationTokenSource stop = new CancellationTokenSource();
        readonly Backoff backoff = new Backoff();
        readonly Random rand = new Random();
        volatile LinkState state = LinkState.Connecting;
        volatile string detail = "";
        int connects;
        public Action<string> Log = _ => { };

        public ClientWebSocketTransport(string liveUrl, string httpBase)
        {
            live = new Uri(liveUrl);
            this.httpBase = httpBase;
        }

        public LinkState State => state;
        public string Detail => detail;
        public int Connects => Volatile.Read(ref connects);

        public void Start() => Task.Run(Run);

        public bool TryReceive(out string frame) => inbox.TryDequeue(out frame);

        async Task Run()
        {
            var buf = new byte[64 * 1024];
            while (!stop.IsCancellationRequested)
            {
                state = LinkState.Connecting;
                detail = "";
                string why;
                using (var ws = new ClientWebSocket())
                {
                    try
                    {
                        using (var t = CancellationTokenSource.CreateLinkedTokenSource(stop.Token))
                        {
                            t.CancelAfter(5000);
                            await ws.ConnectAsync(live, t.Token);
                        }
                        backoff.Reset();
                        Interlocked.Increment(ref connects);
                        state = LinkState.Up;
                        Log($"live connected {live}");
                        why = await Receive(ws, buf);
                    }
                    catch (Exception e) when (!stop.IsCancellationRequested)
                    {
                        why = await Explain(e);
                    }
                    catch (Exception)
                    {
                        return;
                    }
                    try { ws.Abort(); } catch (Exception) { }
                }
                if (stop.IsCancellationRequested) return;
                double wait = backoff.Next(rand.NextDouble());
                if (state != LinkState.Full) state = LinkState.Down;
                detail = $"{why}; retry in {wait / 1000:F1} s";
                Log($"live down: {detail}");
                try { await Task.Delay(TimeSpan.FromMilliseconds(wait), stop.Token); }
                catch (TaskCanceledException) { return; }
            }
        }

        /// <summary>Reads whole messages until the socket closes or goes silent
        /// for LiveSilenceMs (app.js 286-293: heartbeats every 2 s keep a healthy
        /// one busy). Returns why it ended.</summary>
        async Task<string> Receive(ClientWebSocket ws, byte[] buf)
        {
            var msg = new MemoryStream();
            while (true)
            {
                WebSocketReceiveResult r;
                using (var t = CancellationTokenSource.CreateLinkedTokenSource(stop.Token))
                {
                    t.CancelAfter(TimeSpan.FromMilliseconds(LiveModel.LiveSilenceMs));
                    try { r = await ws.ReceiveAsync(new ArraySegment<byte>(buf), t.Token); }
                    catch (OperationCanceledException) when (!stop.IsCancellationRequested) { return "no frames for 5 s"; }
                }
                if (r.MessageType == WebSocketMessageType.Close) return $"closed by server ({ws.CloseStatus})";
                msg.Write(buf, 0, r.Count);
                if (!r.EndOfMessage) continue; // a snapshot can be larger than one buffer
                inbox.Enqueue(Encoding.UTF8.GetString(msg.GetBuffer(), 0, (int)msg.Length));
                msg.SetLength(0);
            }
        }

        /// <summary>A failed connect. Past the subscriber cap fleetd answers 503
        /// before upgrading (api.go serveLive); Mono may not put the status in
        /// the exception, so a plain GET tells (it is 503 only at the cap).</summary>
        async Task<string> Explain(Exception e)
        {
            var text = (e.InnerException?.Message ?? e.Message).Trim(); // Windows messages end in a newline
            bool full = text.Contains("503");
            if (!full && !(e is OperationCanceledException))
            {
                try
                {
                    using (var http = new HttpClient { Timeout = TimeSpan.FromSeconds(3) })
                    using (var res = await http.GetAsync(httpBase + "/ws/live", stop.Token))
                        full = (int)res.StatusCode == 503;
                }
                catch (Exception) { } // server down: the connect error says enough
            }
            if (full)
            {
                state = LinkState.Full;
                return "fleetd has too many live clients (503)";
            }
            return e is OperationCanceledException ? "connect timed out" : text;
        }

        public void Dispose()
        {
            stop.Cancel();
        }
    }
}
