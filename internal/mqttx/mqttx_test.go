package mqttx

import (
	"testing"
	"time"
)

func TestBackoffGrowsCapsAndJitters(t *testing.T) {
	// Without jitter the base is 1, 2, 4, 8, 16, 30, 30, ... seconds;
	// jitter keeps each wait within [0.5, 1.5) of the base.
	bases := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for n, b := range bases {
		base := b * time.Second
		lo, hi := base/2, base*3/2
		for i := 0; i < 200; i++ {
			if d := Backoff(n); d < lo || d >= hi {
				t.Fatalf("Backoff(%d) = %v, want in [%v, %v)", n, d, lo, hi)
			}
		}
	}
}

func TestBackoffIsSpreadOut(t *testing.T) {
	// Many clients retrying at once should not pick the same delay.
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		seen[Backoff(3)] = true
	}
	if len(seen) < 50 {
		t.Errorf("only %d distinct delays out of 100; jitter is too weak", len(seen))
	}
}
