package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// server is the fleetd under test: spawned per run, or one already running
// (-attach).
type server struct {
	base string // http://host:port
	pid  int    // 0 if unknown (attached without -pid): no CPU numbers
	cmd  *exec.Cmd
	dir  string // temp dir with the database and log of a spawned fleetd
}

// spawnFleetd starts bin and waits until it answers /healthz.
func spawnFleetd(bin, broker string, extra []string) (*server, error) {
	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "loadtest-fleetd-")
	if err != nil {
		return nil, err
	}
	logf, err := os.Create(filepath.Join(dir, "fleetd.log"))
	if err != nil {
		return nil, err
	}
	args := append([]string{
		"-http", addr, "-mqtt", "mqtt://" + broker, "-db", filepath.Join(dir, "fleetd.db"),
		"-elevators", "none", "-log", "warn", "-mqtt-id", "fleetd-loadtest",
	}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	go func() { cmd.Wait(); logf.Close() }()
	s := &server{base: "http://" + addr, pid: cmd.Process.Pid, cmd: cmd, dir: dir}
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if resp, e := http.Get(s.base + "/healthz"); e == nil {
			resp.Body.Close()
			return s, nil
		}
	}
	s.stop(true)
	return nil, fmt.Errorf("fleetd did not answer on %s (log: %s)", addr, filepath.Join(dir, "fleetd.log"))
}

func (s *server) stop(keepLogs bool) {
	if s.cmd == nil {
		return
	}
	s.cmd.Process.Kill()
	time.Sleep(200 * time.Millisecond) // let Wait close the log
	if keepLogs {
		fmt.Fprintf(os.Stderr, "fleetd log kept: %s\n", filepath.Join(s.dir, "fleetd.log"))
		return
	}
	os.RemoveAll(s.dir)
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

// liveStats is GET /live/stats.
type liveStats struct {
	Frames      uint64 `json:"frames"`
	Heartbeats  uint64 `json:"heartbeats"`
	Encodes     uint64 `json:"encodes"`
	Dropped     uint64 `json:"dropped"`
	Resyncs     uint64 `json:"resyncs"`
	Blocked     uint64 `json:"blocked"`
	BlockedMS   uint64 `json:"blocked_ms"`
	Subscribers int    `json:"subscribers"`
}

func (s *server) liveStats() (liveStats, error) {
	var st liveStats
	return st, getJSON(s.base+"/live/stats", &st)
}

// online counts the load test's robots that fleetd shows ONLINE.
func (s *server) online() (int, error) {
	var robots []struct {
		Name string `json:"name"`
		Conn string `json:"conn"`
	}
	if err := getJSON(s.base+"/robots", &robots); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range robots {
		if strings.HasPrefix(r.Name, robotPrefix) && r.Conn == "ONLINE" {
			n++
		}
	}
	return n, nil
}

// waitOnline waits until want robots are ONLINE; it returns how long it took.
func (s *server) waitOnline(ctx context.Context, want int, timeout time.Duration, poll time.Duration) (time.Duration, int, error) {
	start := time.Now()
	n := 0
	for time.Since(start) < timeout {
		var err error
		if n, err = s.online(); err == nil && n >= want {
			return time.Since(start), n, nil
		}
		select {
		case <-ctx.Done():
			return time.Since(start), n, ctx.Err()
		case <-time.After(poll):
		}
	}
	return time.Since(start), n, fmt.Errorf("%d of %d robots ONLINE after %v", n, want, timeout)
}

func (s *server) cpu() (time.Duration, bool) {
	if s.pid == 0 {
		return 0, false
	}
	d, err := cpuTime(s.pid)
	return d, err == nil
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func getJSON(url string, v any) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
