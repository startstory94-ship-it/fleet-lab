// MarkerTween: the 3D dashboard eases a robot marker when its drawn position
// changes source (track <-> car slot, or the track starts over on a new floor).
using Fleet.Dashboard;
using NUnit.Framework;

namespace Fleet.Tests
{
    public class MarkerTweenTests
    {
        static void Near(V3 want, V3 got, double tol = 1e-9)
        {
            Assert.AreEqual(want.X, got.X, tol, $"x of {got}");
            Assert.AreEqual(want.Y, got.Y, tol, $"y of {got}");
            Assert.AreEqual(want.Z, got.Z, tol, $"z of {got}");
        }

        [Test]
        public void FirstAppearanceSnapsAndSameSourcePassesThrough()
        {
            var m = new MarkerTween();
            Near(new V3(5, 0, 2), m.Step(0, "t1", new V3(5, 0, 2)));
            Assert.IsFalse(m.Tweening);
            // Normal interpolation of the track is left alone, however far it moves.
            Near(new V3(9, 0, 2), m.Step(16, "t1", new V3(9, 0, 2)));
            Near(new V3(9.1, 0, 2), m.Step(32, "t1", new V3(9.1, 0, 2)));
        }

        [Test]
        public void SourceSwitchEasesOutFromTheLastDrawnPosition()
        {
            var m = new MarkerTween();
            m.Step(0, "t1", new V3(10, 0, 0));                  // at the lobby
            var slot = new V3(12.6, 0.04, 0);
            Near(new V3(10, 0, 0), m.Step(100, "cE1", slot));    // boarded: starts where it was
            Assert.IsTrue(m.Tweening);
            var half = m.Step(100 + MarkerTween.DurationMs / 2, "cE1", slot);
            Near(V3.Lerp(new V3(10, 0, 0), slot, 0.875), half);  // ease-out: 87.5 % at half time
            Near(slot, m.Step(100 + MarkerTween.DurationMs, "cE1", slot));
            Assert.IsFalse(m.Tweening);
            Near(slot, m.Step(2000, "cE1", slot));
        }

        [Test]
        public void TrackStartingOverOnAnotherFloorEases()
        {
            var m = new MarkerTween();
            m.Step(0, "t1", new V3(12.6, 0, 0));
            var f3 = new V3(10, 6, 0);                           // floor 3, 3 m per floor
            var p = m.Step(10, "t3", f3);
            Near(new V3(12.6, 0, 0), p);
            var q = m.Step(310, "t3", f3);
            Assert.That(q.Y, Is.GreaterThan(0).And.LessThan(6));
            Near(f3, m.Step(10 + MarkerTween.DurationMs + 1, "t3", f3));
        }

        [Test]
        public void TweenFollowsAMovingTarget()
        {
            var m = new MarkerTween();
            m.Step(0, "cE1", new V3(0, 0, 0));
            m.Step(0, "t2", new V3(10, 0, 0));
            // The robot walks away from the lobby while the marker eases in.
            var a = m.Step(300, "t2", new V3(11, 0, 0));
            Assert.AreEqual(11 * 0.875, a.X, 1e-9);
            Near(new V3(12, 0, 0), m.Step(600, "t2", new V3(12, 0, 0)));
        }

        [Test]
        public void SwitchMidTweenRestartsFromWhereTheMarkerIs()
        {
            var m = new MarkerTween();
            m.Step(0, "t1", new V3(0, 0, 0));
            m.Step(0, "cE1", new V3(10, 0, 0));
            var mid = m.Step(300, "cE1", new V3(10, 0, 0));      // 8.75
            Near(mid, m.Step(300, "t1", new V3(0, 0, 0)), 1e-9);  // back to the track: no jump
            var later = m.Step(600, "t1", new V3(0, 0, 0));
            Assert.AreEqual(mid.X * (1 - 0.875), later.X, 1e-9);
        }

        // ---- leaving a car: hold until the robot's first new pose ----

        // Drawn in its car; REST then says "out" while its track still ends on the boarding floor.
        static MarkerTween InCarAt(V3 slot)
        {
            var m = new MarkerTween();
            m.Step(0, "t1", new V3(10, 0, 0));
            m.Step(100, "cE1", slot);
            m.Step(1000, "cE1", slot);
            return m;
        }

        [Test]
        public void HeldAfterTheCarUntilANewerPoseThenTweensFromTheHeldPosition()
        {
            var slot = new V3(12.6, 6, 0);
            var m = InCarAt(slot);
            // Out per REST at 5000 ms (server clock 105000); the track's newest sample is old.
            Assert.IsTrue(m.HoldAfterCar(5000, 105000, 4000));
            Near(slot, m.Last);
            Assert.IsTrue(m.HoldAfterCar(5500, 105500, 4000), "still the pre-ride pose");
            Assert.IsTrue(m.Holding);
            // A pose newer than the moment it left arrives (T 105700): released.
            Assert.IsFalse(m.HoldAfterCar(5800, 105800, 105700));
            var lobby = new V3(10, 6, 0);                       // new floor, lobby
            Near(slot, m.Step(5800, "t3", lobby));               // tween starts where it was held
            Assert.IsTrue(m.Tweening);
            Near(lobby, m.Step(5800 + MarkerTween.DurationMs, "t3", lobby));
            Assert.IsFalse(m.HoldAfterCar(9000, 109000, 105700), "no second hold after the exit");
        }

        [Test]
        public void HoldTimesOutAndFallsBackToTheTrack()
        {
            var slot = new V3(12.6, 6, 0);
            var m = InCarAt(slot);
            Assert.IsTrue(m.HoldAfterCar(5000, 105000, 4000));
            Assert.IsTrue(m.HoldAfterCar(5000 + MarkerTween.HoldTimeoutMs - 1, 109999, 4000));
            Assert.IsFalse(m.HoldAfterCar(5000 + MarkerTween.HoldTimeoutMs, 110000, 4000));
            Assert.IsFalse(m.HoldAfterCar(11000, 111000, 4000), "stays released, even with no newer pose");
            Near(slot, m.Step(11000, "t1", new V3(10, 0, 0)));    // tween from the held spot
            Assert.IsTrue(m.Tweening);
        }

        [Test]
        public void RobotThatWasNeverInACarIsNotHeld()
        {
            var m = new MarkerTween();
            Assert.IsFalse(m.HoldAfterCar(0, 100000, double.NaN), "not drawn yet");
            m.Step(0, "t1", new V3(1, 0, 0));
            Assert.IsFalse(m.HoldAfterCar(100, 100100, 50));
            m.Step(100, "t3", new V3(1, 6, 0));                  // a floor change on its own: no hold
            Assert.IsFalse(m.HoldAfterCar(200, 100200, 50));
            Assert.IsFalse(m.Holding);
        }

        [Test]
        public void BackInTheCarEndsTheHold()
        {
            var slot = new V3(12.6, 6, 0);
            var m = InCarAt(slot);
            Assert.IsTrue(m.HoldAfterCar(5000, 105000, 4000));
            m.Step(5100, "cE1", slot);                           // REST flickers back to "in the car"
            Assert.IsFalse(m.Holding);
            Assert.IsTrue(m.HoldAfterCar(6000, 106000, 4000), "a new exit holds again");
        }

        [Test]
        public void EaseOutShape()
        {
            Assert.AreEqual(0, MarkerTween.EaseOut(0));
            Assert.AreEqual(1, MarkerTween.EaseOut(1));
            Assert.AreEqual(1, MarkerTween.EaseOut(5));
            Assert.AreEqual(0, MarkerTween.EaseOut(-1));
            Assert.Greater(MarkerTween.EaseOut(0.25), 0.25, "faster than linear at the start");
        }
    }
}
