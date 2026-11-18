// The send side's ordered buffer. paho (the ROS adapter) keeps QoS 1 publishes
// made while disconnected and sends them, in order, once the link is back;
// this does the same, so events and acks are not lost in a Wi-Fi blip.
// QoS 0 (pose) is only sent live: dropped while not ready or when a publish
// fails. Bounded: when full, the oldest message goes and is counted.
using System;
using System.Collections.Generic;
using System.Threading.Tasks;

namespace Fleet.Core
{
    public sealed class PendingQueue
    {
        public const int DefaultCapacity = 1000;

        readonly Queue<Outgoing> q = new Queue<Outgoing>();
        readonly int capacity;

        public PendingQueue(int capacity = DefaultCapacity) { this.capacity = Math.Max(1, capacity); }

        public int Count => q.Count;
        public int Dropped { get; private set; }

        /// <summary>Adds at the tail. ready: the link is fully up (connected and subscribed);
        /// without it a QoS 0 message is dropped, a QoS 1 one waits.</summary>
        public void Enqueue(Outgoing o, bool ready)
        {
            if (o.Qos == 0 && !ready) { Dropped++; return; }
            if (q.Count >= capacity) { q.Dequeue(); Dropped++; }
            q.Enqueue(o);
        }

        /// <summary>Publishes from the head, in order, while ready() holds. A publish that
        /// throws stops the pass: a QoS 1 head stays at the head, a QoS 0 one is dropped.
        /// OperationCanceledException is not caught. Returns the number sent.</summary>
        public async Task<int> FlushAsync(Func<bool> ready, Func<Outgoing, Task> publish, Action<Outgoing, Exception> onError = null)
        {
            int sent = 0;
            while (q.Count > 0 && ready())
            {
                var o = q.Peek();
                try { await publish(o).ConfigureAwait(false); }
                catch (Exception e) when (!(e is OperationCanceledException))
                {
                    if (o.Qos == 0) { q.Dequeue(); Dropped++; }
                    onError?.Invoke(o, e);
                    break;
                }
                q.Dequeue();
                sent++;
            }
            return sent;
        }
    }
}
