package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// Pose and state behave like adapter/fleet_logic.py: a pose is due when the
// robot moved 5 cm or turned 2 degrees (checked at 5 Hz), else once a second;
// a state once a second.
const (
	poseHz         = 5
	poseIdle       = time.Second
	deadbandM      = 0.05
	deadbandRad    = 2 * math.Pi / 180
	statePeriod    = time.Second
	robotHost      = "loadtest"
	robotPrefix    = "lt_"
	publishTimeout = 2 * time.Second
)

// backoffNoJitter is mqttx.Backoff without the +/-50 % jitter: the baseline
// for the reconnect storm.
func backoffNoJitter(n int) time.Duration {
	return min(time.Second<<min(n, 5), 30*time.Second)
}

// connLog records connection attempts and successful connections of all
// robots (for the reconnect storm).
type connLog struct {
	mu       sync.Mutex
	attempts []time.Time
	ups      []upEvent
	fails    []connFail
}

// connFail is a failed connection attempt (outside an outage these are the
// storm's own cost: a broker or listen queue that could not keep up).
type connFail struct {
	at  time.Time
	err string
}

func (l *connLog) fail(err error) {
	l.mu.Lock()
	l.fails = append(l.fails, connFail{time.Now(), errClass(err)})
	l.mu.Unlock()
}

// errClass shortens an error to its last part ("...: connection reset")
// so the same cause from different robots counts together.
func errClass(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}

type upEvent struct {
	at    time.Time
	robot int
}

func (l *connLog) attempt() {
	l.mu.Lock()
	l.attempts = append(l.attempts, time.Now())
	l.mu.Unlock()
}

func (l *connLog) up(i int) {
	l.mu.Lock()
	l.ups = append(l.ups, upEvent{time.Now(), i})
	l.mu.Unlock()
}

func (l *connLog) copy() ([]time.Time, []upEvent, []connFail) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.attempts...), append([]upEvent(nil), l.ups...), append([]connFail(nil), l.fails...)
}

// fakeFleet is N robots, each with its own MQTT connection (autopaho, the
// library fleetd uses, with the reconnect backoff under test).
type fakeFleet struct {
	log       connLog
	posesSent atomic.Int64
	statesOK  atomic.Int64
	wg        sync.WaitGroup
}

type poseMsg struct {
	Boot  string  `json:"boot"`
	Seq   uint64  `json:"seq"`
	Stamp float64 `json:"stamp"`
	Host  string  `json:"host"`
	Frame string  `json:"frame"`
	Floor int     `json:"floor"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Yaw   float64 `json:"yaw"`
	V     float64 `json:"v"`
}

type stateMsg struct {
	Boot    string  `json:"boot"`
	Seq     uint64  `json:"seq"`
	Stamp   float64 `json:"stamp"`
	Host    string  `json:"host"`
	Mode    string  `json:"mode"`
	Battery float64 `json:"battery"`
	TaskID  string  `json:"task_id"`
	Error   string  `json:"error"`
}

func robotName(i int) string { return fmt.Sprintf("%s%04d", robotPrefix, i) }

// startFleet connects n robots to broker (host:port) and keeps them
// publishing until ctx ends. A fraction moving of them drive in circles; the
// rest stand still (pose once a second).
func startFleet(ctx context.Context, broker string, n int, moving float64, backoff func(int) time.Duration) (*fakeFleet, error) {
	u, err := url.Parse("mqtt://" + broker)
	if err != nil {
		return nil, err
	}
	f := &fakeFleet{}
	boot := fmt.Sprintf("lt%06d", rand.IntN(1_000_000))
	for i := range n {
		cm, err := autopaho.NewConnection(ctx, autopaho.ClientConfig{
			ServerUrls:                    []*url.URL{u},
			KeepAlive:                     5,
			CleanStartOnInitialConnection: true,
			ReconnectBackoff:              backoff,
			ConnectTimeout:                5 * time.Second,
			AttemptConnection: func(ctx context.Context, _ autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
				f.log.attempt()
				var d net.Dialer
				return d.DialContext(ctx, "tcp", u.Host)
			},
			OnConnectionUp: func(*autopaho.ConnectionManager, *paho.Connack) { f.log.up(i) },
			OnConnectError: f.log.fail,
			ClientConfig: paho.ClientConfig{
				ClientID:      robotName(i),
				OnClientError: func(error) {},
			},
		})
		if err != nil {
			return nil, err
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.drive(ctx, cm, i, boot, float64(i)/float64(n) < moving)
			dctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = cm.Disconnect(dctx)
			cancel()
		}()
	}
	return f, nil
}

// drive publishes one robot's pose and state until ctx ends.
func (f *fakeFleet) drive(ctx context.Context, cm *autopaho.ConnectionManager, i int, boot string, moving bool) {
	name := robotName(i)
	// Robots stand on a grid 6 m apart and drive circles of 1.5 m radius at
	// 0.8 m/s (0.16 m per pose tick, always past the deadband).
	cx, cy := float64(i%25)*6, float64(i/25)*6
	const r, v = 1.5, 0.8
	phase := rand.Float64() * 2 * math.Pi
	var poseSeq, stateSeq uint64
	var lastPose time.Time
	var lx, ly, lyaw float64
	var lastState time.Time
	battery := 60 + 40*rand.Float64()

	// Spread the robots over the 200 ms tick.
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(time.Second / poseHz)))):
	case <-ctx.Done():
		return
	}
	start := time.Now()
	t := time.NewTicker(time.Second / poseHz)
	defer t.Stop()
	for {
		now := time.Now()
		x, y, yaw, speed := cx+r, cy, math.Pi/2, 0.0
		if moving {
			a := phase + v/r*now.Sub(start).Seconds()
			x, y, yaw, speed = cx+r*math.Cos(a), cy+r*math.Sin(a), a+math.Pi/2, v
		}
		dyaw := math.Abs(math.Atan2(math.Sin(yaw-lyaw), math.Cos(yaw-lyaw)))
		if lastPose.IsZero() || math.Hypot(x-lx, y-ly) >= deadbandM || dyaw >= deadbandRad || now.Sub(lastPose) >= poseIdle {
			poseSeq++
			body, _ := json.Marshal(poseMsg{Boot: boot, Seq: poseSeq, Stamp: unix(now), Host: robotHost, Frame: "map",
				Floor: 1, X: x, Y: y, Yaw: math.Atan2(math.Sin(yaw), math.Cos(yaw)), V: speed})
			pctx, cancel := context.WithTimeout(ctx, publishTimeout)
			if _, err := cm.Publish(pctx, &paho.Publish{Topic: "fleet/" + name + "/pose", QoS: 0, Payload: body}); err == nil {
				f.posesSent.Add(1)
			}
			cancel()
			lastPose, lx, ly, lyaw = now, x, y, yaw
		}
		if now.Sub(lastState) >= statePeriod {
			stateSeq++
			mode := "IDLE"
			if moving {
				mode = "MOVING"
			}
			body, _ := json.Marshal(stateMsg{Boot: boot, Seq: stateSeq, Stamp: unix(now), Host: robotHost,
				Mode: mode, Battery: battery})
			pctx, cancel := context.WithTimeout(ctx, publishTimeout)
			if _, err := cm.Publish(pctx, &paho.Publish{Topic: "fleet/" + name + "/state", QoS: 1, Payload: body}); err == nil {
				f.statesOK.Add(1)
			}
			cancel()
			lastState = now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
