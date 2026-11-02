// Command fleetd is the fleet management server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"fleetlab/internal/api"
	"fleetlab/internal/elevator"
	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/live"
	"fleetlab/internal/mqttx"
	"fleetlab/internal/store"
	"fleetlab/internal/task"
)

const topicCmdAck = "fleet/+/cmd_ack"

type config struct {
	httpAddr string
	mqttURL  string
	dbPath   string
	logLevel string
	frame    time.Duration
	lifts    string
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.httpAddr, "http", "127.0.0.1:8080", "HTTP listen address (REST, WebSocket, dashboard)")
	flag.StringVar(&c.mqttURL, "mqtt", "mqtt://127.0.0.1:1883", "MQTT broker URL")
	flag.StringVar(&c.dbPath, "db", "fleetd.db", "SQLite database path")
	flag.StringVar(&c.logLevel, "log", "info", "log level: debug, info, warn, error")
	flag.DurationVar(&c.frame, "frame", live.DefaultConfig().FrameInterval, "dashboard frame interval (/ws/live)")
	flag.StringVar(&c.lifts, "elevators", "", `elevator config JSON file; "" = one default car E1 (floors 1-10, lobby (10,0)), "none" = no elevators`)
	flag.Parse()
	return c
}

func main() {
	cfg := parseFlags()

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.logLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	db, err := store.Open(cfg.dbPath, log)
	if err != nil {
		log.Error("open db", "path", cfg.dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()

	hub := events.NewHub()
	// Robot and task events share one numbering. Each source calls publish
	// with its own lock held, so numbers follow the order of the changes.
	var seqMu sync.Mutex
	var seq uint64
	publish := func(e fleet.Event) {
		seqMu.Lock()
		seq++
		e.Seq = seq
		hub.Publish(e)
		seqMu.Unlock()
	}

	fleetCfg := fleet.DefaultConfig()
	registry := fleet.NewRegistry(fleetCfg, func(e fleet.Event) {
		log.Info("event", "robot", e.Robot, "type", e.Type, "from", e.From, "to", e.To, "detail", e.Detail)
		publish(e)
	})

	lifts, err := elevator.Load(cfg.lifts)
	if err != nil {
		log.Error("elevator config", "path", cfg.lifts, "err", err)
		os.Exit(1)
	}
	for _, c := range lifts.Elevators {
		log.Info("elevator", "id", c.ID, "floors", c.Floors, "capacity", c.Capacity, "lobby", c.Lobby)
	}

	cmds := newOutbox(log)
	taskCfg := task.DefaultConfig()
	// After a restart a robot that is merely slow must not lose its task.
	taskCfg.RestoreGrace = max(taskCfg.RestoreGrace, fleetCfg.OfflineAfter+5*time.Second)
	tasks := task.NewManager(taskCfg, task.Deps{
		Robots: registry, Sender: cmds, Store: db, Log: log, Elevators: lifts, Lift: cmds,
		Emit: func(e task.Event) {
			detail := e.Kind
			if e.Detail != "" {
				detail += ": " + e.Detail
			}
			publish(fleet.Event{Time: e.Time, Robot: e.Robot, Type: e.Type, Task: e.Task, Elevator: e.Elevator,
				From: e.From, To: e.To, Detail: detail})
		},
	})
	if err := tasks.Restore(); err != nil {
		log.Error("restore tasks", "err", err)
		os.Exit(1)
	}
	writerDone := make(chan struct{})
	go func() {
		tasks.RunWriter(ctx)
		close(writerDone)
	}()

	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				registry.Tick()
				tasks.Tick()
			}
		}
	}()

	ingest := fleet.Ingest(registry, log)
	mq, err := mqttx.Connect(ctx, log, cfg.mqttURL, "fleetd", []mqttx.Subscription{
		{Topic: fleet.TopicPose, QoS: 0},
		{Topic: fleet.TopicState, QoS: 1},
		{Topic: fleet.TopicAdapterStatus, QoS: 1},
		{Topic: topicCmdAck, QoS: 1},
		{Topic: elevator.SubState, QoS: 1},
		{Topic: elevator.SubZone, QoS: 1},
		{Topic: elevator.SubTrip, QoS: 1},
		{Topic: elevator.SubRobotEvent, QoS: 1},
	}, func(topic string, payload []byte) {
		if car, kind, ok := elevator.SplitTopic(topic); ok {
			handleElevator(log, tasks, car, kind, payload)
			return
		}
		if robot, ok := robotTopic(topic, "event"); ok {
			var e elevator.RobotEvent
			if err := json.Unmarshal(payload, &e); err != nil || e.Event == "" {
				log.Warn("bad robot event", "topic", topic, "err", err)
				return
			}
			tasks.HandleRobotEvent(robot, e)
			return
		}
		if robot, ok := ackTopic(topic); ok {
			var a task.Ack
			if err := json.Unmarshal(payload, &a); err != nil || a.CmdID == "" {
				log.Warn("bad cmd_ack", "topic", topic, "err", err)
				return
			}
			tasks.HandleAck(robot, a)
			return
		}
		ingest(topic, payload)
	})
	if err != nil {
		log.Error("mqtt", "err", err)
		os.Exit(1)
	}
	go cmds.run(ctx, mq)
	go func() {
		select {
		case <-mq.Up():
			tasks.LinkUp() // restored tasks get their grace from now
		case <-ctx.Done():
			return
		}
		// A retained trip published while the broker was away may be lost:
		// publish every current trip again on each (re)connect.
		for {
			select {
			case <-mq.Connected():
				tasks.RepublishTrips()
			case <-ctx.Done():
				return
			}
		}
	}()

	// Dashboard frames. The broadcaster reads the registry and the manager's
	// needs-charge flags one at a time and holds no lock while doing so; see
	// internal/live for the lock order.
	liveCfg := live.DefaultConfig()
	liveCfg.FrameInterval = cfg.frame
	liveCfg.Log = log
	frames := live.New(liveCfg, registry, tasks)
	go frames.Run(ctx)

	srv := &http.Server{Addr: cfg.httpAddr, Handler: api.New(registry, hub, tasks, frames, log), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Info("http listening", "addr", cfg.httpAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	mq.Close(shutdownCtx)
	<-writerDone // last flush of task changes
}

// ackTopic returns the robot of a fleet/<robot>/cmd_ack topic.
func ackTopic(topic string) (string, bool) { return robotTopic(topic, "cmd_ack") }

// robotTopic returns the robot of a fleet/<robot>/<kind> topic.
func robotTopic(topic, kind string) (string, bool) {
	parts := strings.Split(topic, "/")
	if len(parts) == 3 && parts[0] == "fleet" && parts[2] == kind && parts[1] != "" && parts[1] != "adapter" {
		return parts[1], true
	}
	return "", false
}

// handleElevator feeds elevator/<id>/state, zone and trip to the manager.
func handleElevator(log *slog.Logger, tasks *task.Manager, car, kind string, payload []byte) {
	var err error
	switch kind {
	case "state":
		var s elevator.State
		if err = json.Unmarshal(payload, &s); err == nil {
			tasks.HandleElevatorState(car, s)
		}
	case "zone":
		var z elevator.Zone
		if err = json.Unmarshal(payload, &z); err == nil {
			tasks.HandleZone(car, z)
		}
	case "trip":
		if len(payload) == 0 {
			return // retained message cleared
		}
		var t elevator.Trip
		if err = json.Unmarshal(payload, &t); err == nil {
			tasks.RestoreTrip(car, t) // ignored unless an earlier run published it
		}
	}
	if err != nil {
		log.Warn("bad elevator message", "elevator", car, "kind", kind, "err", err)
	}
}
