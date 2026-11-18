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
