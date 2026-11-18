using Fleet.Core;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class BootArgsTests
    {
        [Test]
        public void DefaultsWhenNoArgs()
        {
            var a = BootArgs.Parse(new[] { "FleetRobots.exe", "-batchmode", "-nographics" });
            Assert.AreEqual("127.0.0.1", a.BrokerHost);
            Assert.AreEqual(1883, a.BrokerPort);
            Assert.AreEqual("unity-1", a.Host);
            Assert.AreEqual(2, a.Robots);
            Assert.AreEqual("unity_1", a.RobotName(0));
            Assert.AreEqual(1, a.Floor);
            Assert.IsEmpty(a.Warnings);
        }

        [Test]
        public void ParsesEveryOption()
        {
            var a = BootArgs.Parse(new[] { "x", "-broker", "10.0.0.5:1884", "-host", "unity-7", "-robots", "4",
                "-prefix", "u7", "-floor", "3", "-origin", "1.5,-2", "-spacing", "3", "-speed", "1.2",
                "-battery", "40", "-logFile", "p.log" });
            Assert.AreEqual("10.0.0.5", a.BrokerHost);
            Assert.AreEqual(1884, a.BrokerPort);
            Assert.AreEqual("unity-7", a.Host);
            Assert.AreEqual(4, a.Robots);
            Assert.AreEqual("u7_4", a.RobotName(3));
            Assert.AreEqual(3, a.Floor);
            Assert.AreEqual(1.5, a.OriginX);
            Assert.AreEqual(-2, a.OriginY);
            Assert.AreEqual(3, a.Spacing);
            Assert.AreEqual(1.2, a.Speed);
            Assert.AreEqual(40, a.Battery);
        }

        [Test]
        public void BadValueKeepsDefaultAndWarns()
        {
            var a = BootArgs.Parse(new[] { "-robots", "many", "-speed" });
            Assert.AreEqual(2, a.Robots);
            Assert.AreEqual(0.8, a.Speed);
            Assert.AreEqual(2, a.Warnings.Count);
        }
    }
}
