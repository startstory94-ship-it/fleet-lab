// Command mockelevator is a mock elevator controller on MQTT: the elevator
// side of docs/ELEVATOR.md for every car of an -elevators config (the same
// file fleetd takes).
//
//	sub elevator/<id>/cmd     call, goto, hold, release (each cmd_id once)
//	sub elevator/<id>/zone    robots' enter/clear/failed -> door_zone
//	pub elevator/<id>/state   retained, QoS 1, on change and every second
//	sub mockelevator/<id>/fault  {"unresponsive":bool,"door_blocked":bool}
//
// Run: go run ./cmd/mockelevator [-elevators file] [-mqtt mqtt://127.0.0.1:1883]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"time"

	"fleetlab/internal/elevator"
	"fleetlab/internal/mqttx"
)

const faultTopic = "mockelevator/%s/fault"

type options struct {
	mqttURL      string
	lifts        string
	only         string
	startFloor   int
	floorS       float64
	doorS        float64
	unresponsive string
	blocked      string
	logLevel     string
	tick         time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.mqttURL, "mqtt", "mqtt://127.0.0.1:1883", "MQTT broker URL")
	flag.StringVar(&o.lifts, "elevators", "", `elevator config JSON (fleetd's -elevators); "" = default car E1`)
	flag.StringVar(&o.only, "id", "", "comma-separated car ids to run (default: every car of the config)")
	flag.IntVar(&o.startFloor, "start-floor", 0, "floor every car starts at (default: its first floor)")
	flag.Float64Var(&o.floorS, "floor-s", 0, "travel seconds per floor (default: floor_s of the config)")
	flag.Float64Var(&o.doorS, "door-s", 0, "door open/close seconds (default: door_s of the config)")
	flag.StringVar(&o.unresponsive, "unresponsive", "", "comma-separated car ids that start unresponsive (no state, commands ignored)")
	flag.StringVar(&o.blocked, "door-blocked", "", "comma-separated car ids whose door zone starts blocked")
	flag.StringVar(&o.logLevel, "log", "info", "log level: debug, info, warn, error")
	flag.DurationVar(&o.tick, "tick", 50*time.Millisecond, "simulation step")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(o.logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(o, log); err != nil {
		log.Error("mockelevator", "err", err)
		os.Exit(1)
	}
}

func run(o options, log *slog.Logger) error {
	cfg, err := elevator.Load(o.lifts)
	if err != nil {
		return fmt.Errorf("elevator config %q: %w", o.lifts, err)
	}
	only := splitIDs(o.only)
	boot := fmt.Sprintf("mock-%06x", rand.Intn(1<<24))
	cars := map[string]*car{}
	for _, c := range cfg.Elevators {
		if len(only) > 0 && !slices.Contains(only, c.ID) {
			continue
		}
		if o.floorS > 0 {
			c.FloorS = o.floorS
		}
		if o.doorS > 0 {
			c.DoorS = o.doorS
		}
		k := newCar(c, boot, o.startFloor)
		k.setFaults(slices.Contains(splitIDs(o.unresponsive), c.ID), slices.Contains(splitIDs(o.blocked), c.ID))
		cars[c.ID] = k
		log.Info("car", "id", c.ID, "floors", c.Floors, "capacity", c.Capacity, "floor_s", c.FloorS, "door_s", c.DoorS,
			"start", k.floor, "unresponsive", k.unresponsive, "door_blocked", k.doorBlocked)
	}
	if len(cars) == 0 {
		return fmt.Errorf("no elevator to run")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var mu sync.Mutex
	handle := func(topic string, payload []byte) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(topic, "mockelevator/") {
			parts := strings.Split(topic, "/")
			c := cars[parts[1]]
			var f struct {
				Unresponsive bool `json:"unresponsive"`
				DoorBlocked  bool `json:"door_blocked"`
			}
			if c == nil || len(parts) != 3 || json.Unmarshal(payload, &f) != nil {
				return
			}
			c.setFaults(f.Unresponsive, f.DoorBlocked)
			log.Warn("faults", "car", parts[1], "unresponsive", f.Unresponsive, "door_blocked", f.DoorBlocked)
			return
		}
		id, kind, ok := elevator.SplitTopic(topic)
		c := cars[id]
		if !ok || c == nil {
			return
		}
		now := time.Now()
		switch kind {
		case "cmd":
			var cmd elevator.Cmd
			if err := json.Unmarshal(payload, &cmd); err != nil {
				log.Warn("bad command", "car", id, "err", err)
				return
			}
			c.advance(now) // bring the car up to now first
			if c.command(cmd, now) {
				log.Info("command", "car", id, "cmd", cmd.Cmd, "floor", cmd.Floor, "hold_s", cmd.HoldS, "trip", cmd.Trip, "cmd_id", cmd.CmdID)
			} else {
				log.Debug("command not applied", "car", id, "cmd", cmd.Cmd, "cmd_id", cmd.CmdID)
			}
		case "zone":
			var z elevator.Zone
			if err := json.Unmarshal(payload, &z); err != nil {
				log.Warn("bad zone", "car", id, "err", err)
				return
			}
			c.zoneMsg(z)
			log.Info("zone", "car", id, "robot", z.Robot, "action", z.Action, "event", z.Event, "n", z.N,
				"door_zone", c.doorZone(), "in_zone", c.zoneNames())
		}
	}

	var subs []mqttx.Subscription
	for id := range cars {
		subs = append(subs,
			mqttx.Subscription{Topic: fmt.Sprintf(elevator.TopicCmd, id), QoS: 1},
			mqttx.Subscription{Topic: fmt.Sprintf(elevator.TopicZone, id), QoS: 1},
			mqttx.Subscription{Topic: fmt.Sprintf(faultTopic, id), QoS: 1})
	}
	mq, err := mqttx.Connect(ctx, log, o.mqttURL, "mockelevator-"+boot, subs, handle)
	if err != nil {
		return err
	}
	select {
	case <-mq.Up():
	case <-ctx.Done():
		return nil
	}

	type out struct {
		id string
		s  elevator.State
	}
	t := time.NewTicker(o.tick)
	defer t.Stop()
	last := map[string]elevator.State{}
	for {
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			mq.Close(sctx)
			cancel()
			return nil
		case <-t.C:
		}
		var pubs []out
		mu.Lock()
		now := time.Now()
		for id, c := range cars {
			if s := c.step(now); s != nil {
				pubs = append(pubs, out{id, *s})
			}
		}
		mu.Unlock()
		for _, p := range pubs {
			body, _ := json.Marshal(p.s)
			if err := mq.Publish(ctx, fmt.Sprintf(elevator.TopicState, p.id), 1, true, body); err != nil {
				log.Warn("state not published", "car", p.id, "err", err)
				continue
			}
			prev := last[p.id]
			if prev.Floor != p.s.Floor || prev.Door != p.s.Door || prev.Moving != p.s.Moving || prev.Trip != p.s.Trip ||
				prev.DoorZone != p.s.DoorZone || prev.Hold != p.s.Hold {
				log.Info("state", "car", p.id, "floor", p.s.Floor, "dir", p.s.Dir, "door", p.s.Door, "moving", p.s.Moving,
					"hold", p.s.Hold, "trip", p.s.Trip, "load", p.s.Load, "door_zone", p.s.DoorZone)
			}
			last[p.id] = p.s
		}
	}
}

func splitIDs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
