// The process's one MQTT connection, acting as adapter host <host>
// (fleet/adapter/<host>/status), like fleet_adapter.py's paho client:
// retained "online" on every connect, retained "offline" as LWT and on a clean
// quit, 5 s keepalive, reconnect with jittered back-off, re-subscribe after
// each reconnect. Robots that can ride also follow elevator/+/turn|state|trip
// (wildcards: one subscription serves every robot of the process and every
// car; FleetBootstrap routes each message to the robots riding that car).
// MQTTnet runs on thread-pool threads; incoming messages go
// to an inbox the main thread drains, and outgoing ones leave through one
// sender task so acks and seq keep their order. While the link is down QoS 1
// messages wait in a PendingQueue (like paho's) and go out in order once it is
// back and subscribed; QoS 0 (pose) is dropped.
using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Fleet.Core;
using MQTTnet;
using MQTTnet.Client;
using MQTTnet.Protocol;
using Newtonsoft.Json.Linq;

namespace Fleet.UnityApp
{
    public sealed class FleetMqtt : IDisposable
    {
        public readonly string Host;
        public readonly string Boot = Ids.NewBoot();
        public Action<string> Log = _ => { };

        readonly string brokerHost;
        readonly int brokerPort;
        readonly string[] cmdTopics;
        readonly IMqttClient client;
        readonly MqttClientOptions options;
        readonly ConcurrentQueue<KeyValuePair<string, string>> inbox = new ConcurrentQueue<KeyValuePair<string, string>>();
        readonly BlockingCollection<Outgoing> outbox = new BlockingCollection<Outgoing>(4096);
        readonly PendingQueue pending = new PendingQueue();
        readonly CancellationTokenSource stop = new CancellationTokenSource();
        readonly System.Random rng = new System.Random();
        int connects;        // bumped on each successful connect; the main thread re-publishes on change
        int dropped;
        // True from "online" + subscribe done until the link drops. IsConnected is true
        // earlier, and sending then would let new messages pass the status and the resubscribe.
        volatile bool ready;

        public FleetMqtt(string brokerHost, int brokerPort, string host, IEnumerable<string> robots,
                         IEnumerable<string> extraTopics = null)
        {
            this.brokerHost = brokerHost;
            this.brokerPort = brokerPort;
            Host = host;
            var topics = new List<string>();
            foreach (var r in robots) topics.Add(Topics.Cmd(r));
            if (extraTopics != null) topics.AddRange(extraTopics);
            cmdTopics = topics.ToArray();

            client = new MqttFactory().CreateMqttClient();
            options = new MqttClientOptionsBuilder()
                .WithTcpServer(brokerHost, brokerPort)
                .WithClientId($"adapter-{host}")
                .WithCleanSession(true)
                .WithKeepAlivePeriod(TimeSpan.FromSeconds(Protocol.KeepAliveS))
                .WithTimeout(TimeSpan.FromSeconds(5))
                .WithWillTopic(Topics.AdapterStatus(host))
                .WithWillPayload(StatusPayload("offline"))
                .WithWillQualityOfServiceLevel(MqttQualityOfServiceLevel.AtLeastOnce)
                .WithWillRetain(true)
                .Build();
            client.ApplicationMessageReceivedAsync += e =>
            {
                // Copy now: MQTTnet may reuse the buffer after the handler returns.
                var m = e.ApplicationMessage;
                var seg = m.PayloadSegment;
                var text = seg.Array == null ? "" : Encoding.UTF8.GetString(seg.Array, seg.Offset, seg.Count);
                inbox.Enqueue(new KeyValuePair<string, string>(m.Topic, text));
                return Task.CompletedTask;
            };
            client.DisconnectedAsync += e =>
            {
                ready = false;
                if (!stop.IsCancellationRequested)
                    Log($"mqtt disconnected: {e.Reason} {e.Exception?.Message}");
                return Task.CompletedTask;
            };
        }

        public bool IsConnected => client.IsConnected;
        public int Connects => Volatile.Read(ref connects);
        public int Dropped => Volatile.Read(ref dropped) + Volatile.Read(ref pendingDropped);
        int pendingDropped;

        public void Start()
        {
            Task.Run(ConnectLoop);
            Task.Run(SendLoop);
        }

        public bool TryReceive(out string topic, out string payload)
        {
            if (inbox.TryDequeue(out var kv))
            {
                topic = kv.Key;
                payload = kv.Value;
                return true;
            }
            topic = payload = null;
            return false;
        }

        /// <summary>Queue for sending. While disconnected QoS 1 messages are kept (bounded,
        /// oldest dropped) and sent in order after reconnecting; QoS 0 is dropped.</summary>
        public void Send(Outgoing o)
        {
            if (stop.IsCancellationRequested || !outbox.TryAdd(o))
                Interlocked.Increment(ref dropped);
        }

        string StatusPayload(string status) =>
            new JObject { ["host"] = Host, ["boot"] = Boot, ["status"] = status }.ToString(Newtonsoft.Json.Formatting.None);

        async Task ConnectLoop()
        {
            double delay = 0.5 + rng.NextDouble(); // spread reconnects like the adapter (0.5-1.5 s)
            while (!stop.IsCancellationRequested)
            {
                if (!client.IsConnected)
                {
                    try
                    {
                        await client.ConnectAsync(options, stop.Token);
                        await Publish(Topics.AdapterStatus(Host), StatusPayload("online"), 1, true);
                        var sub = new MqttClientSubscribeOptionsBuilder();
                        foreach (var t in cmdTopics)
                            sub.WithTopicFilter(t, MqttQualityOfServiceLevel.AtLeastOnce);
                        await client.SubscribeAsync(sub.Build(), stop.Token);
                        ready = true;
                        Interlocked.Increment(ref connects);
                        Log($"mqtt connected to {brokerHost}:{brokerPort} as host {Host} (boot {Boot}), {string.Join(", ", cmdTopics)}");
                        delay = 0.5 + rng.NextDouble();
                    }
                    catch (OperationCanceledException) when (stop.IsCancellationRequested)
                    {
                        return;
                    }
                    catch (Exception e)
                    {
                        ready = false;
                        // Connected but "online" or the subscribe failed: drop the link so the
                        // next pass redoes the whole sequence instead of seeing IsConnected.
                        if (client.IsConnected)
                        {
                            try { await client.DisconnectAsync(new MqttClientDisconnectOptionsBuilder().Build()); }
                            catch (Exception) { }
                        }
                        Log($"mqtt connect failed: {e.Message}; retry in {delay:F1} s");
                        try { await Task.Delay(TimeSpan.FromSeconds(delay), stop.Token); } catch (OperationCanceledException) { return; }
                        delay = Math.Min(30.0, delay * 2);
                        continue;
                    }
                }
                try { await Task.Delay(500, stop.Token); } catch (OperationCanceledException) { return; }
            }
        }

        async Task SendLoop()
        {
            bool failed = false;
            try
            {
                while (!stop.IsCancellationRequested)
                {
                    // Wait for news only when there is nothing to flush right now; a failed
                    // publish waits too, so a dead link is not retried in a hot loop.
                    int wait = pending.Count > 0 && ready && !failed ? 0 : 100;
                    if (outbox.TryTake(out var o, wait, stop.Token))
                    {
                        pending.Enqueue(o, ready);
                        while (outbox.TryTake(out o)) pending.Enqueue(o, ready);
                    }
                    failed = false;
                    await pending.FlushAsync(() => ready, m => Publish(m.Topic, m.Payload, m.Qos, m.Retain),
                        (m, e) => { failed = true; Log($"publish {m.Topic} failed: {e.Message}"); }).ConfigureAwait(false);
                    Volatile.Write(ref pendingDropped, pending.Dropped);
                }
            }
            catch (OperationCanceledException) { }
            catch (InvalidOperationException) { } // outbox completed by Dispose
        }

        Task Publish(string topic, string payload, int qos, bool retain)
        {
            var msg = new MqttApplicationMessageBuilder()
                .WithTopic(topic)
                .WithPayload(payload)
                .WithQualityOfServiceLevel(qos == 0 ? MqttQualityOfServiceLevel.AtMostOnce : MqttQualityOfServiceLevel.AtLeastOnce)
                .WithRetainFlag(retain)
                .Build();
            return client.PublishAsync(msg, stop.Token);
        }

        /// <summary>Clean quit: say "offline" ourselves (the LWT only covers crashes), then disconnect.</summary>
        public void Dispose()
        {
            if (stop.IsCancellationRequested) return;
            try
            {
                if (client.IsConnected)
                {
                    var off = new MqttApplicationMessageBuilder()
                        .WithTopic(Topics.AdapterStatus(Host)).WithPayload(StatusPayload("offline"))
                        .WithQualityOfServiceLevel(MqttQualityOfServiceLevel.AtLeastOnce).WithRetainFlag(true).Build();
                    client.PublishAsync(off).Wait(2000);
                    client.DisconnectAsync(new MqttClientDisconnectOptionsBuilder().Build()).Wait(2000);
                }
            }
            catch (Exception e)
            {
                Log($"mqtt close: {e.Message}");
            }
            stop.Cancel();
            outbox.CompleteAdding();
            client.Dispose();
        }
    }
}
