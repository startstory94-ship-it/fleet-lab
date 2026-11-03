// Command loadtest drives a real fleetd over MQTT and WebSocket and measures
// where load concentrates (PLAN.md stage 7, results in docs/LOAD_TEST.md):
//
//   - frames: N fake robots publish pose (5 Hz with the adapter's deadband)
//     and state (1 Hz) through Mosquitto; M dashboards read /ws/live, K of
//     them deliberately slow. Measured over a window after warm-up: fleetd's
//     CPU (process CPU time delta), frames/s and bytes/s per client, frame
//     latency (server time in the frame -> received), drops and catch-up
//     snapshots (/live/stats). Scenarios "default" (encode once, drop old
//     frames for slow clients), "per-client" (fleetd -bench-encode-per-client)
//     and "block" (fleetd -bench-block-slow).
//   - storm: every robot's connection is cut at once (a TCP proxy between
//     the robots and the broker; fleetd stays connected) and refused for
//     -outage. The robots reconnect with autopaho, the library fleetd uses,
//     with mqttx.Backoff ("storm-jitter") or the same backoff without jitter
//     ("storm-nojitter"). Measured: connection attempts per 100 ms, time
//     until all are connected again and until fleetd shows all ONLINE.
//
// Each scenario runs -repeat times on a fresh fleetd (spawned from -fleetd,
// with its own database and -elevators none) and the report gives the median
// and the range. Needs Mosquitto on -broker.
//
//	go build -o bin/fleetd.exe ./cmd/fleetd
//	go run ./cmd/loadtest -fleetd bin/fleetd.exe -md out.md -json out.json
//
// -attach URL measures a fleetd that is already running instead (its flags
// cannot be switched, so pick the scenarios that match; -pid for CPU).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"time"

	"fleetlab/internal/mqttx"
)

type config struct {
	fleetd    string
	attach    string
	pid       int
	broker    string
	robots    int
	clients   int
	slow      int
	slowDelay time.Duration
	moving    float64
	warmup    time.Duration
	window    time.Duration
	repeat    int
	outage    time.Duration
	scenarios string
	md        string
	jsonOut   string
	machine   string
	keepLogs  bool
	slowRcv   int
	check     bool
}

type scenario struct {
	name    string
	label   string
	storm   bool
	flags   []string
	backoff func(int) time.Duration
}

var allScenarios = []scenario{
	{name: "default", label: "encode once + drop (fleetd default)", backoff: mqttx.Backoff},
	{name: "per-client", label: "encode per client", flags: []string{"-bench-encode-per-client"}, backoff: mqttx.Backoff},
	{name: "block", label: "block on slow clients", flags: []string{"-bench-block-slow"}, backoff: mqttx.Backoff},
	{name: "storm-jitter", label: "mqttx.Backoff (\u00b150% jitter)", storm: true, backoff: mqttx.Backoff},
	{name: "storm-nojitter", label: "same backoff, no jitter", storm: true, backoff: backoffNoJitter},
}

func main() {
	var c config
	defBin := "bin/fleetd"
	if runtime.GOOS == "windows" {
		defBin += ".exe"
	}
	flag.StringVar(&c.fleetd, "fleetd", defBin, "fleetd binary to spawn for every run")
	flag.StringVar(&c.attach, "attach", "", "measure an already running fleetd at this URL instead of spawning one")
	flag.IntVar(&c.pid, "pid", 0, "with -attach: fleetd's process id, for CPU numbers")
	flag.StringVar(&c.broker, "broker", "127.0.0.1:1883", "MQTT broker host:port")
	flag.IntVar(&c.robots, "robots", 500, "fake robots")
	flag.IntVar(&c.clients, "clients", 20, "WebSocket /ws/live clients")
	flag.IntVar(&c.slow, "slow", 2, "of the clients, how many are slow")
	flag.DurationVar(&c.slowDelay, "slow-delay", 500*time.Millisecond, "a slow client sleeps this long after every frame")
	flag.Float64Var(&c.moving, "moving", 1, "fraction of robots driving (the rest stand still: pose at 1 Hz)")
	flag.DurationVar(&c.warmup, "warmup", 5*time.Second, "frames: time after all robots are ONLINE and clients connected, before measuring")
	flag.DurationVar(&c.window, "window", 15*time.Second, "frames: measuring window")
	flag.IntVar(&c.repeat, "repeat", 3, "runs per scenario")
	flag.DurationVar(&c.outage, "outage", 2500*time.Millisecond, "storm: how long the robots' broker connection is refused")
	flag.StringVar(&c.scenarios, "scenarios", "default,per-client,block,storm-jitter,storm-nojitter", "comma-separated scenarios")
	flag.StringVar(&c.md, "md", "", "write the markdown report here too")
	flag.StringVar(&c.jsonOut, "json", "", "write every run's numbers here (JSON)")
	flag.StringVar(&c.machine, "machine", "", "machine description for the report (CPU model etc.)")
	flag.BoolVar(&c.keepLogs, "keep-logs", false, "keep each spawned fleetd's log and database")
	flag.IntVar(&c.slowRcv, "slow-rcvbuf", 0, "slow clients' socket receive buffer in bytes (0: OS default); a small one makes the server's drop path show up sooner where socket buffers are big (Linux)")
	flag.BoolVar(&c.check, "check", false, "exit 1 unless every frames run had no client errors and fast clients got frames, and, with slow clients, the server dropped frames and the slow clients got catch-up snapshots (CI)")
	flag.Parse()
	if c.slow > c.clients {
		c.slow = c.clients
	}

	var chosen []scenario
	for _, name := range strings.Split(c.scenarios, ",") {
		i := slices.IndexFunc(allScenarios, func(s scenario) bool { return s.name == strings.TrimSpace(name) })
		if i < 0 {
			fatalf("unknown scenario %q", name)
		}
		chosen = append(chosen, allScenarios[i])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Runs are interleaved (A B C A B C ...) so drift of the machine over the
	// minutes of a run does not line up with one scenario.
	res := results{Config: c, Started: time.Now()}
	for run := 1; run <= c.repeat; run++ {
		for _, sc := range chosen {
			if ctx.Err() != nil {
				fatalf("interrupted")
			}
			fmt.Fprintf(os.Stderr, "== %s run %d/%d\n", sc.name, run, c.repeat)
			if sc.storm {
				r, err := runStorm(ctx, c, sc)
				if err != nil {
					fatalf("%s run %d: %v", sc.name, run, err)
				}
				fmt.Fprintf(os.Stderr, "   %s\n", r)
				res.Storm = append(res.Storm, r)
			} else {
				r, err := runFrames(ctx, c, sc)
				if err != nil {
					fatalf("%s run %d: %v", sc.name, run, err)
				}
				fmt.Fprintf(os.Stderr, "   %s\n", r)
				res.Frames = append(res.Frames, r)
				if c.check {
					if err := r.check(c); err != nil {
						fatalf("%s run %d: check: %v", sc.name, run, err)
					}
				}
			}
		}
	}
	res.Took = time.Since(res.Started).Round(time.Second).String()

	md := res.markdown(chosen)
	fmt.Print(md)
	if c.md != "" {
		if err := os.WriteFile(c.md, []byte(md), 0o644); err != nil {
			fatalf("%v", err)
		}
	}
	if c.jsonOut != "" {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fatalf("json: %v", err)
		}
		if err := os.WriteFile(c.jsonOut, b, 0o644); err != nil {
			fatalf("%v", err)
		}
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "loadtest: "+format+"\n", a...)
	os.Exit(1)
}

func startServer(c config, sc scenario) (*server, error) {
	if c.attach != "" {
		return &server{base: strings.TrimRight(c.attach, "/"), pid: c.pid}, nil
	}
	return spawnFleetd(c.fleetd, c.broker, sc.flags)
}

// frameRun is one run of a frames scenario. Rates are per second over the
// window; CPU is in % of one core.
type frameRun struct {
	Scenario       string
	ServerCPU      float64
	LoadgenCPU     float64
	PosesSent      float64 // pose messages the fake robots published (not what fleetd received)
	Deltas         float64 // delta frames fleetd built
	EncodesPerTick float64 // encodes / (deltas + heartbeats)
	FastFPS        float64 // frames received, mean over fast clients
	FastKBps       float64
	FastP50ms      float64 // delta latency, all fast clients pooled
	FastP99ms      float64
	FastMaxms      float64
	SlowFPS        float64
	SlowKBps       float64
	SlowSnapshots  float64 // catch-up snapshots received by slow clients
	SlowAgeP50ms   float64 // slow clients: age of the state in each frame they read (deltas and snapshots), pooled
	SlowAgeP99ms   float64
	SlowAgeMaxms   float64
	Dropped        float64 // /live/stats
	DroppedTotal   uint64  // /live/stats, in the window
	Resyncs        float64
	BlockedPct     float64 // share of the window Tick spent waiting (block only)
	Online         int
	ClientErrors   int
}

func (r frameRun) String() string {
	return fmt.Sprintf("cpu %.0f%% (loadtest %.0f%%), poses sent %.0f/s, deltas %.1f/s, encodes/tick %.1f, fast %.1f fps %.0f KB/s p50 %.1f ms p99 %.1f ms, slow %.1f fps age p50 %.0f ms max %.0f ms snapshots %.0f, dropped %.1f/s resyncs %.1f/s, blocked %.0f%%, client errors %d",
		r.ServerCPU, r.LoadgenCPU, r.PosesSent, r.Deltas, r.EncodesPerTick, r.FastFPS, r.FastKBps, r.FastP50ms, r.FastP99ms, r.SlowFPS, r.SlowAgeP50ms, r.SlowAgeMaxms, r.SlowSnapshots, r.Dropped, r.Resyncs, r.BlockedPct, r.ClientErrors)
}

// check is -check: the run must show the paths it is meant to exercise.
func (r frameRun) check(c config) error {
	switch {
	case r.ClientErrors > 0:
		return fmt.Errorf("%d WebSocket clients failed", r.ClientErrors)
	case r.FastFPS == 0 && c.clients > c.slow:
		return fmt.Errorf("fast clients received no frames")
	case c.slow > 0 && r.Scenario != "block" && r.DroppedTotal == 0:
		return fmt.Errorf("no frame was dropped for the slow clients: make them slower (-slow-delay, -slow-rcvbuf) or the run longer")
	case c.slow > 0 && r.Scenario != "block" && r.SlowSnapshots == 0:
		return fmt.Errorf("slow clients got no catch-up snapshot")
	}
	return nil
}

func runFrames(ctx context.Context, c config, sc scenario) (frameRun, error) {
	r := frameRun{Scenario: sc.name}
	srv, err := startServer(c, sc)
	if err != nil {
		return r, err
	}
	defer srv.stop(c.keepLogs)
	px, err := newProxy(c.broker)
	if err != nil {
		return r, err
	}
	defer px.close()

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fl, err := startFleet(rctx, px.addr(), c.robots, c.moving, sc.backoff)
	if err != nil {
		return r, err
	}
	defer fl.wg.Wait()
	defer cancel()
	if _, _, err := srv.waitOnline(rctx, c.robots, 60*time.Second, 250*time.Millisecond); err != nil {
		return r, err
	}

	clients := make([]*wsClient, c.clients)
	url := "ws" + strings.TrimPrefix(srv.base, "http") + "/ws/live"
	for i := range clients {
		clients[i] = &wsClient{slow: i < c.slow, delay: c.slowDelay}
		if clients[i].slow {
			clients[i].rcvBuf = c.slowRcv
		}
		go clients[i].run(rctx, url)
	}
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if st, err := srv.liveStats(); err == nil && st.Subscribers >= c.clients {
			break
		}
		if time.Now().After(end) {
			return r, fmt.Errorf("not all %d WebSocket clients connected", c.clients)
		}
	}
	sleep(rctx, c.warmup)

	// Take the cheap local readings first: /live/stats waits for the
	// broadcaster's mutex, which -bench-block-slow holds for up to a second.
	t0 := time.Now()
	cpu0, cpuOK := srv.cpu()
	own0, _ := cpuTime(os.Getpid())
	poses0 := fl.posesSent.Load()
	before := make([]counts, len(clients))
	for i, cl := range clients {
		before[i] = cl.snapshot()
	}
	st0, err := srv.liveStats()
	if err != nil {
		return r, err
	}

	sleep(rctx, c.window)

	t1 := time.Now()
	cpu1, _ := srv.cpu()
	own1, _ := cpuTime(os.Getpid())
	poses1 := fl.posesSent.Load()
	after := make([]counts, len(clients))
	for i, cl := range clients {
		after[i] = cl.snapshot()
	}
	st1, err := srv.liveStats()
	if err != nil {
		return r, err
	}
	win := t1.Sub(t0).Seconds()
	if ctx.Err() != nil {
		return r, ctx.Err()
	}
	r.Online, _ = srv.online()

	if cpuOK {
		r.ServerCPU = (cpu1 - cpu0).Seconds() / win * 100
	} else {
		r.ServerCPU = -1 // unknown: n/a in the report
	}
	r.LoadgenCPU = (own1 - own0).Seconds() / win * 100
	r.PosesSent = float64(poses1-poses0) / win
	r.DroppedTotal = st1.Dropped - st0.Dropped
	r.Deltas = float64(st1.Frames-st0.Frames) / win
	if ticks := float64(st1.Frames - st0.Frames + st1.Heartbeats - st0.Heartbeats); ticks > 0 {
		r.EncodesPerTick = float64(st1.Encodes-st0.Encodes) / ticks
	}
	r.Dropped = float64(st1.Dropped-st0.Dropped) / win
	r.Resyncs = float64(st1.Resyncs-st0.Resyncs) / win
	r.BlockedPct = float64(st1.BlockedMS-st0.BlockedMS) / 1000 / win * 100

	var lats, slowAges []time.Duration
	var nFast, nSlow float64
	for i, cl := range clients {
		d := after[i].sub(before[i])
		cl.mu.Lock()
		if cl.err != nil {
			r.ClientErrors++
		}
		cl.mu.Unlock()
		if cl.slow {
			nSlow++
			r.SlowFPS += float64(d.Frames) / win
			r.SlowKBps += float64(d.Bytes) / 1024 / win
			r.SlowSnapshots += float64(d.Snapshots)
			slowAges = append(slowAges, cl.latencies(t0, t1)...)
			continue
		}
		nFast++
		r.FastFPS += float64(d.Frames) / win
		r.FastKBps += float64(d.Bytes) / 1024 / win
		lats = append(lats, cl.latencies(t0, t1)...)
	}
	if nFast > 0 {
		r.FastFPS /= nFast
		r.FastKBps /= nFast
	}
	if nSlow > 0 {
		r.SlowFPS /= nSlow
		r.SlowKBps /= nSlow
	}
	r.FastP50ms, r.FastP99ms, r.FastMaxms = ms(percentile(lats, 50)), ms(percentile(lats, 99)), ms(percentile(lats, 100))
	r.SlowAgeP50ms, r.SlowAgeP99ms, r.SlowAgeMaxms = ms(percentile(slowAges, 50)), ms(percentile(slowAges, 99)), ms(percentile(slowAges, 100))
	return r, nil
}

// stormRun is one reconnect storm.
type stormRun struct {
	Scenario      string
	Attempts      int     // connection attempts after the cut
	PeakPer100ms  int     // most attempts in one 100 ms bucket
	BusyBuckets   int     // 100 ms buckets with at least one attempt
	AllConnectedS float64 // cut -> every robot connected to the broker again
	AfterOutageS  float64 // the same, counted from the end of the outage
	AllOnlineS    float64 // cut -> fleetd shows every robot ONLINE again
	ServerCPU     float64 // % of one core from the cut until all ONLINE
	Histogram     []int   // attempts per 250 ms after the cut
	// Attempts that failed after the outage was over: the storm's own cost.
	FailsAfterOutage int
	FailCauses       map[string]int // by error, after the outage
	ProxyDialFails   int            // of those, the proxy could not reach the broker
}

func (r stormRun) String() string {
	return fmt.Sprintf("attempts %d, peak %d/100ms in %d busy buckets, all connected %.2fs (%.2fs after outage), all ONLINE %.2fs, cpu %.0f%%, failed after outage %d %v (proxy dial %d)",
		r.Attempts, r.PeakPer100ms, r.BusyBuckets, r.AllConnectedS, r.AfterOutageS, r.AllOnlineS, r.ServerCPU, r.FailsAfterOutage, r.FailCauses, r.ProxyDialFails)
}

func runStorm(ctx context.Context, c config, sc scenario) (stormRun, error) {
	r := stormRun{Scenario: sc.name}
	srv, err := startServer(c, sc)
	if err != nil {
		return r, err
	}
	defer srv.stop(c.keepLogs)
	px, err := newProxy(c.broker)
	if err != nil {
		return r, err
	}
	defer px.close()
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fl, err := startFleet(rctx, px.addr(), c.robots, c.moving, sc.backoff)
	if err != nil {
		return r, err
	}
	defer fl.wg.Wait()
	defer cancel()
	if _, _, err := srv.waitOnline(rctx, c.robots, 60*time.Second, 250*time.Millisecond); err != nil {
		return r, err
	}
	sleep(rctx, 3*time.Second)

	cpu0, cpuOK := srv.cpu()
	cut := time.Now()
	px.cut(c.outage)
	// fleetd sees the robots go STALE after 2 s of silence (the outage is
	// longer), then ONLINE again after a few messages.
	for end := cut.Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if n, err := srv.online(); err == nil && n < c.robots {
			break
		}
		if time.Now().After(end) {
			return r, fmt.Errorf("fleetd never noticed the outage")
		}
	}
	if _, _, err := srv.waitOnline(rctx, c.robots, 120*time.Second, 100*time.Millisecond); err != nil {
		return r, err
	}
	allOnline := time.Since(cut)
	r.AllOnlineS = allOnline.Seconds()
	cpu1, _ := srv.cpu()
	if cpuOK {
		r.ServerCPU = (cpu1 - cpu0).Seconds() / allOnline.Seconds() * 100
	} else {
		r.ServerCPU = -1 // unknown: n/a in the report
	}

	attempts, ups, fails := fl.log.copy()
	r.FailCauses = map[string]int{}
	for _, f := range fails {
		if f.at.After(cut.Add(c.outage)) {
			r.FailsAfterOutage++
			r.FailCauses[f.err]++
		}
	}
	r.ProxyDialFails = px.dialFailures()
	var rel []time.Duration
	for _, a := range attempts {
		if a.After(cut) {
			rel = append(rel, a.Sub(cut))
		}
	}
	r.Attempts = len(rel)
	r.PeakPer100ms, r.BusyBuckets = peak(rel, 100*time.Millisecond)
	r.Histogram = histogram(rel, 250*time.Millisecond)
	first := map[int]time.Duration{}
	for _, u := range ups {
		if u.at.After(cut) {
			if _, ok := first[u.robot]; !ok {
				first[u.robot] = u.at.Sub(cut)
			}
		}
	}
	if len(first) < c.robots {
		return r, fmt.Errorf("only %d of %d robots reconnected", len(first), c.robots)
	}
	var last time.Duration
	for _, d := range first {
		last = max(last, d)
	}
	r.AllConnectedS = last.Seconds()
	r.AfterOutageS = (last - c.outage).Seconds()
	return r, nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
