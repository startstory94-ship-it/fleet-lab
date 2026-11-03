package main

import (
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"time"
)

// clockStep is the smallest step of the wall clock (frame latency compares
// fleetd's wall clock with ours; on Windows it moves in ~0.5 ms steps).
func clockStep() time.Duration {
	step := time.Duration(math.MaxInt64)
	prev := time.Now().Round(0)
	for i := 0; i < 20; {
		if n := time.Now().Round(0); !n.Equal(prev) {
			step = min(step, n.Sub(prev))
			prev = n
			i++
		}
	}
	return step.Round(time.Microsecond)
}

type results struct {
	Config  config `json:"-"`
	Started time.Time
	Took    string
	Frames  []frameRun
	Storm   []stormRun
}

// percentile p (0-100) of ds by nearest rank; 0 when empty.
func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// peak returns the most offsets falling in one bucket of width w (buckets
// start at 0) and how many buckets hold at least one.
func peak(offsets []time.Duration, w time.Duration) (most, busy int) {
	n := map[int64]int{}
	for _, d := range offsets {
		n[int64(d/w)]++
	}
	for _, c := range n {
		most = max(most, c)
	}
	return most, len(n)
}

// histogram counts offsets per bucket of width w from 0 to the last one.
func histogram(offsets []time.Duration, w time.Duration) []int {
	var h []int
	for _, d := range offsets {
		i := int(d / w)
		for len(h) <= i {
			h = append(h, 0)
		}
		h[i]++
	}
	return h
}

// summary is "median [min-max]" of xs (with an en dash).
func summary(xs []float64, prec int) string {
	var ok []float64
	for _, x := range xs {
		if x >= 0 && !math.IsNaN(x) { // negative: not measured
			ok = append(ok, x)
		}
	}
	if len(ok) == 0 {
		return "n/a"
	}
	slices.Sort(ok)
	f := func(x float64) string { return fmt.Sprintf("%.*f", prec, x) }
	med := median(ok)
	if len(ok) == 1 {
		return f(med)
	}
	return fmt.Sprintf("%s [%s\u2013%s]", f(med), f(ok[0]), f(ok[len(ok)-1]))
}

// median of sorted xs.
func median(xs []float64) float64 {
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

type metric[R any] struct {
	name string
	prec int
	get  func(R) float64
}

var frameMetrics = []metric[frameRun]{
	{"fleetd CPU (% of one core)", 0, func(r frameRun) float64 { return r.ServerCPU }},
	{"loadtest CPU (% of one core, same machine)", 0, func(r frameRun) float64 { return r.LoadgenCPU }},
	{"pose messages sent by the fake robots (/s)", 0, func(r frameRun) float64 { return r.PosesSent }},
	{"delta frames built (/s)", 1, func(r frameRun) float64 { return r.Deltas }},
	{"JSON encodes per frame", 1, func(r frameRun) float64 { return r.EncodesPerTick }},
	{"fast client: frames received (/s)", 1, func(r frameRun) float64 { return r.FastFPS }},
	{"fast client: KB/s", 0, func(r frameRun) float64 { return r.FastKBps }},
	{"fast client: latency p50 (ms)", 1, func(r frameRun) float64 { return r.FastP50ms }},
	{"fast client: latency p99 (ms)", 1, func(r frameRun) float64 { return r.FastP99ms }},
	{"fast client: latency max (ms)", 0, func(r frameRun) float64 { return r.FastMaxms }},
	{"slow client: frames received (/s)", 1, func(r frameRun) float64 { return r.SlowFPS }},
	{"slow client: KB/s", 0, func(r frameRun) float64 { return r.SlowKBps }},
	{"slow client: age of the state shown, p50 (ms)", 0, func(r frameRun) float64 { return r.SlowAgeP50ms }},
	{"slow client: age of the state shown, p99 (ms)", 0, func(r frameRun) float64 { return r.SlowAgeP99ms }},
	{"slow client: age of the state shown, max (ms)", 0, func(r frameRun) float64 { return r.SlowAgeMaxms }},
	{"slow client: catch-up snapshots received (in the window)", 0, func(r frameRun) float64 { return r.SlowSnapshots }},
	{"dropped frames (/s, all clients)", 1, func(r frameRun) float64 { return r.Dropped }},
	{"catch-up snapshots (/s, all clients)", 1, func(r frameRun) float64 { return r.Resyncs }},
	{"time Tick spent waiting (%)", 0, func(r frameRun) float64 { return r.BlockedPct }},
}

var stormMetrics = []metric[stormRun]{
	{"connection attempts after the cut", 0, func(r stormRun) float64 { return float64(r.Attempts) }},
	{"peak attempts in one 100 ms", 0, func(r stormRun) float64 { return float64(r.PeakPer100ms) }},
	{"100 ms buckets with attempts", 0, func(r stormRun) float64 { return float64(r.BusyBuckets) }},
	{"cut \u2192 all reconnected (s)", 2, func(r stormRun) float64 { return r.AllConnectedS }},
	{"outage end \u2192 all reconnected (s)", 2, func(r stormRun) float64 { return r.AfterOutageS }},
	{"cut \u2192 fleetd shows all ONLINE (s)", 2, func(r stormRun) float64 { return r.AllOnlineS }},
	{"fleetd CPU, cut \u2192 all ONLINE (% of one core)", 0, func(r stormRun) float64 { return r.ServerCPU }},
	{"attempts failed after the outage ended", 0, func(r stormRun) float64 { return float64(r.FailsAfterOutage) }},
	{"of all failures: proxy could not reach the broker", 0, func(r stormRun) float64 { return float64(r.ProxyDialFails) }},
}

func table[R any](b *strings.Builder, ms []metric[R], cols []scenario, runs func(string) []R) {
	b.WriteString("| |")
	for _, s := range cols {
		fmt.Fprintf(b, " %s<br>`%s` |", s.label, s.name)
	}
	b.WriteString("\n|---|")
	for range cols {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, m := range ms {
		fmt.Fprintf(b, "| %s |", m.name)
		for _, s := range cols {
			var xs []float64
			for _, r := range runs(s.name) {
				xs = append(xs, m.get(r))
			}
			fmt.Fprintf(b, " %s |", summary(xs, m.prec))
		}
		b.WriteString("\n")
	}
}

func (res results) markdown(chosen []scenario) string {
	c := res.Config
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- generated by cmd/loadtest on %s, took %s -->\n\n", res.Started.Format("2006-01-02 15:04"), res.Took)
	fmt.Fprintf(&b, "Machine: %s/%s, %d logical CPUs", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	if c.machine != "" {
		fmt.Fprintf(&b, ", %s", c.machine)
	}
	fmt.Fprintf(&b, ". fleetd, Mosquitto and the load generator share it. Wall clock step %v: latencies below that are noise.\n\n", clockStep())
	fmt.Fprintf(&b, "Each cell: median [min\u2013max] of %d runs, each on a fresh fleetd.\n\n", c.repeat)

	var frames, storms []scenario
	for _, s := range chosen {
		if s.storm {
			storms = append(storms, s)
		} else {
			frames = append(frames, s)
		}
	}
	if len(frames) > 0 {
		fmt.Fprintf(&b, "### Frames: %d robots (%.0f%% moving), %d /ws/live clients (%d slow: %v after every frame), %v window after %v warm-up\n\n",
			c.robots, c.moving*100, c.clients, c.slow, c.slowDelay, c.window, c.warmup)
		ms := frameMetrics
		if c.slow == 0 {
			ms = slices.DeleteFunc(slices.Clone(ms), func(m metric[frameRun]) bool { return strings.HasPrefix(m.name, "slow client") })
		}
		table(&b, ms, frames, func(name string) []frameRun {
			var out []frameRun
			for _, r := range res.Frames {
				if r.Scenario == name {
					out = append(out, r)
				}
			}
			return out
		})
		b.WriteString("\n")
	}
	if len(storms) > 0 {
		fmt.Fprintf(&b, "### Reconnect storm: %d robots cut at once, broker refused for %v\n\n", c.robots, c.outage)
		get := func(name string) []stormRun {
			var out []stormRun
			for _, r := range res.Storm {
				if r.Scenario == name {
					out = append(out, r)
				}
			}
			return out
		}
		table(&b, stormMetrics, storms, get)
		b.WriteString("\nConnection attempts per 250 ms after the cut (the run with the median peak):\n\n```\n")
		for _, s := range storms {
			runs := get(s.name)
			if len(runs) == 0 {
				continue
			}
			slices.SortFunc(runs, func(a, b stormRun) int { return a.PeakPer100ms - b.PeakPer100ms })
			h := runs[len(runs)/2].Histogram
			fmt.Fprintf(&b, "%s", s.name)
			if causes := runs[len(runs)/2].FailCauses; len(causes) > 0 {
				fmt.Fprintf(&b, " (failed after the outage: %v)", causes)
			}
			b.WriteString("\n")
			top := slices.Max(append([]int{1}, h...))
			for i, n := range h {
				fmt.Fprintf(&b, "  %5.2fs %4d %s\n", float64(i)*0.25, n, strings.Repeat("#", (n*50+top-1)/top))
			}
		}
		b.WriteString("```\n")
	}
	return b.String()
}
