// PendingQueue: what FleetMqtt keeps while the link is down (paho behaviour:
// QoS 1 kept in order and sent after reconnecting, QoS 0 not).
using System;
using System.Collections.Generic;
using System.Threading.Tasks;
using Fleet.Core;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class PendingQueueTests
    {
        static Outgoing Q1(int i) => new Outgoing("fleet/r/event", i.ToString(), 1);
        static Outgoing Q0(int i) => new Outgoing("fleet/r/pose", i.ToString(), 0);

        static int Flush(PendingQueue q, List<string> sent, Func<bool> ready = null, Func<Outgoing, bool> fail = null)
        {
            return q.FlushAsync(ready ?? (() => true), o =>
            {
                if (fail != null && fail(o)) throw new InvalidOperationException("boom");
                sent.Add(o.Payload);
                return Task.CompletedTask;
            }).GetAwaiter().GetResult();
        }

        [Test]
        public void QosOneWaitsWhileNotReadyAndGoesOutInOrderBeforeNewerOnes()
        {
            var q = new PendingQueue();
            var sent = new List<string>();
            q.Enqueue(Q1(1), false);
            q.Enqueue(Q1(2), false);
            Assert.AreEqual(0, Flush(q, sent, () => false));
            Assert.AreEqual(2, q.Count);
            q.Enqueue(Q1(3), true);                 // newer, after the link is back
            Assert.AreEqual(3, Flush(q, sent));
            CollectionAssert.AreEqual(new[] { "1", "2", "3" }, sent);
            Assert.AreEqual(0, q.Count);
            Assert.AreEqual(0, q.Dropped);
        }

        [Test]
        public void QosZeroIsDroppedWhileNotReady()
        {
            var q = new PendingQueue();
            var sent = new List<string>();
            q.Enqueue(Q0(1), false);
            q.Enqueue(Q1(2), false);
            q.Enqueue(Q0(3), false);
            Assert.AreEqual(1, q.Count);
            Assert.AreEqual(2, q.Dropped);
            Flush(q, sent);
            CollectionAssert.AreEqual(new[] { "2" }, sent);
        }

        [Test]
        public void BoundDropsTheOldestAndCountsIt()
        {
            var q = new PendingQueue(3);
            for (int i = 1; i <= 5; i++) q.Enqueue(Q1(i), false);
            Assert.AreEqual(3, q.Count);
            Assert.AreEqual(2, q.Dropped);
            var sent = new List<string>();
            Flush(q, sent);
            CollectionAssert.AreEqual(new[] { "3", "4", "5" }, sent);
        }

        [Test]
        public void FailedPublishKeepsQosOneAtTheHeadAndStopsThePass()
        {
            var q = new PendingQueue();
            var sent = new List<string>();
            q.Enqueue(Q1(1), true);
            q.Enqueue(Q1(2), true);
            q.Enqueue(Q1(3), true);
            Assert.AreEqual(1, Flush(q, sent, fail: o => o.Payload == "2"));
            Assert.AreEqual(2, q.Count, "2 and 3 wait; 3 must not pass 2");
            Assert.AreEqual(0, q.Dropped);
            Assert.AreEqual(2, Flush(q, sent));
            CollectionAssert.AreEqual(new[] { "1", "2", "3" }, sent);
        }

        [Test]
        public void FailedQosZeroIsDroppedAndTheOnesBehindItWait()
        {
            var q = new PendingQueue();
            var sent = new List<string>();
            q.Enqueue(Q0(1), true);
            q.Enqueue(Q1(2), true);
            Assert.AreEqual(0, Flush(q, sent, fail: o => o.Qos == 0));
            Assert.AreEqual(1, q.Count);
            Assert.AreEqual(1, q.Dropped);
            Flush(q, sent);
            CollectionAssert.AreEqual(new[] { "2" }, sent);
        }

        [Test]
        public void FlushStopsWhenTheLinkDropsMidway()
        {
            var q = new PendingQueue();
            var sent = new List<string>();
            for (int i = 1; i <= 3; i++) q.Enqueue(Q1(i), true);
            bool up = true;
            q.FlushAsync(() => up, o => { sent.Add(o.Payload); up = false; return Task.CompletedTask; }).GetAwaiter().GetResult();
            Assert.AreEqual(1, sent.Count);
            Assert.AreEqual(2, q.Count);
        }
    }
}
