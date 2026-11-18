// Ports of adapter/test_fleet_logic.py: throttling and the command book.
using System;
using Fleet.Core;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class ThrottleTests
    {
        static double Rad(double deg) => deg * Math.PI / 180.0;

        [Test]
        public void PoseFirstAlwaysSent() => Assert.IsTrue(Throttle.PoseDue(null, 0, 0, 0, 0));

        [TestCase(0.00, 0.0, 0.2, false)] // still, inside heartbeat period
        [TestCase(0.04, 0.0, 0.2, false)] // below 5 cm
        [TestCase(0.05, 0.0, 0.2, true)]  // 5 cm
        [TestCase(0.00, 1.9, 0.2, false)] // below 2 degrees
        [TestCase(0.00, 2.1, 0.2, true)]  // turned
        [TestCase(0.00, 0.0, 1.0, true)]  // heartbeat while still
        public void PoseDeadband(double dx, double dyawDeg, double dt, bool want)
        {
            var last = new PoseSent(10.0, 1.0, 1.0, 0.0);
            Assert.AreEqual(want, Throttle.PoseDue(last, 10.0 + dt, 1.0 + dx, 1.0, Rad(dyawDeg)));
        }

        [Test]
        public void PoseDeadbandHandlesYawWraparound()
        {
            var last = new PoseSent(0.0, 0.0, 0.0, Math.PI - 0.01);
            Assert.IsFalse(Throttle.PoseDue(last, 0.2, 0.0, 0.0, -Math.PI + 0.01)); // 0.02 rad apart, not 2 pi
        }

        [Test]
        public void PoseRateIsCappedAtFiveHz()
        {
            var last = new PoseSent(10.0, 0.0, 0.0, 0.0);
            Assert.IsFalse(Throttle.PoseDueRated(last, 10.1, 1.0, 0.0, 0.0), "moved 1 m but only 0.1 s later");
            Assert.IsTrue(Throttle.PoseDueRated(last, 10.2, 1.0, 0.0, 0.0));
            Assert.IsTrue(Throttle.PoseDueRated(null, 0.0, 0.0, 0.0, 0.0));
        }

        [Test]
        public void StateSentOnChangeOrHeartbeat()
        {
            var last = new StateSent(5.0, "IDLE||0");
            Assert.IsFalse(Throttle.StateDue(last, 5.5, "IDLE||0"));
            Assert.IsTrue(Throttle.StateDue(last, 5.5, "MOVING|c1|0"));
            Assert.IsTrue(Throttle.StateDue(last, 6.0, "IDLE||0"));
        }
    }

    public class CommandBookTests
    {
        [Test]
        public void DuplicateIsNotRunButAckIsRepeated()
        {
            var book = new CommandBook();
            Assert.AreEqual(Verdict.New, book.Check("c1", null, 0));
            Assert.IsNull(book.LastAck("c1")); // still running: nothing to repeat yet
            book.Record("c1", "accepted");
            Assert.AreEqual(Verdict.Duplicate, book.Check("c1", null, 0));
            Assert.AreEqual("accepted", book.LastAck("c1"));
            book.Record("c1", "done");
            Assert.AreEqual("done", book.LastAck("c1")); // the latest outcome is repeated
        }

        [Test]
        public void ExpiredCommand()
        {
            var book = new CommandBook();
            Assert.AreEqual(Verdict.Expired, book.Check("c1", 100.0, 101.0));
            // A re-send of the expired command is a duplicate and gets the same answer.
            book.Record("c1", "failed expired");
            Assert.AreEqual(Verdict.Duplicate, book.Check("c1", 100.0, 101.0));
            Assert.AreEqual(Verdict.New, book.Check("c2", 200.0, 101.0));
            Assert.AreEqual(Verdict.New, book.Check("c3", null, 101.0), "no expires_at never expires");
        }

        [Test]
        public void BookForgetsOldest()
        {
            var book = new CommandBook(2);
            foreach (var c in new[] { "a", "b", "c" })
                book.Check(c, null, 0);
            Assert.AreEqual(Verdict.New, book.Check("a", null, 0));
            Assert.AreEqual(Verdict.Duplicate, book.Check("c", null, 0));
        }

        [Test]
        public void RecordForUnknownIdIsIgnored()
        {
            var book = new CommandBook();
            book.Record("x", "done");
            Assert.IsNull(book.LastAck("x"));
        }
    }
}
