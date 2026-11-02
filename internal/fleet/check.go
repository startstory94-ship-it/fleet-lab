package fleet

import (
	"fmt"
	"math"
)

// Limits on values from robots. A message outside them is rejected whole and
// the robot keeps its last good values: one bad number must not reach the
// task manager, the dashboard or JSON encoding (NaN and Inf cannot be
// encoded, and rounding a huge value can overflow to Inf).
const (
	MaxCoord    = 1e6  // metres, |x| and |y|
	MaxSpeed    = 100  // m/s, |v|
	MaxSilenceS = 3600 // seconds of announced blackout
)

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Check reports why the pose cannot be used, or nil.
func (m PoseMsg) Check() error {
	switch {
	case !finite(m.X) || !finite(m.Y) || math.Abs(m.X) > MaxCoord || math.Abs(m.Y) > MaxCoord:
		return fmt.Errorf("position (%g, %g) outside ±%g m", m.X, m.Y, float64(MaxCoord))
	case !finite(m.Yaw):
		return fmt.Errorf("yaw %g is not finite", m.Yaw)
	case !finite(m.V) || math.Abs(m.V) > MaxSpeed:
		return fmt.Errorf("speed %g outside ±%d m/s", m.V, MaxSpeed)
	}
	return nil
}

// Check reports why the state cannot be used, or nil.
func (m StateMsg) Check() error {
	switch {
	case m.Battery != nil && (!finite(*m.Battery) || *m.Battery < 0 || *m.Battery > 100):
		return fmt.Errorf("battery %g outside 0..100", *m.Battery)
	case !finite(m.SilenceS) || m.SilenceS < 0 || m.SilenceS > MaxSilenceS:
		return fmt.Errorf("silence_s %g outside 0..%d", m.SilenceS, MaxSilenceS)
	}
	return nil
}
