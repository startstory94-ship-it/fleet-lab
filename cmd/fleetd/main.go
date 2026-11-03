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
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.httpAddr, "http", "127.0.0.1:8080", "HTTP listen address (REST, WebSocket, dashboard)")
	flag.StringVar(&c.mqttURL, "mqtt", "mqtt://127.0.0.1:1883", "MQTT broker URL")
	flag.StringVar(&c.dbPath, "db", "fleetd.db", "SQLite database path")
	flag.StringVar(&c.logLevel, "log", "info", "log level: debug, info, warn, error")
	flag.DurationVar(&c.frame, "frame", live.DefaultConfig().FrameInterval, "dashboard frame interval (/ws/live)")
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

	cmds := newCmdQueue(log)
	taskCfg := task.DefaultConfig()
	// After a restart a robot that is merely slow must not lose its task.
	taskCfg.RestoreGrace = max(taskCfg.RestoreGrace, fleetCfg.OfflineAfter+5*time.Second)
	tasks := task.NewManager(taskCfg, task.Deps{
		Robots: registry, Sender: cmds, Store: db, Log: log,
		Emit: func(e task.Event) {
			detail := e.Kind
			if e.Detail != "" {
				detail += ": " + e.Detail
			}
			publish(fleet.Event{Time: e.Time, Robot: e.Robot, Type: e.Type, Task: e.Task,
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
	}, func(topic string, payload []byte) {
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
func ackTopic(topic string) (string, bool) {
	parts := strings.Split(topic, "/")
	if len(parts) == 3 && parts[0] == "fleet" && parts[2] == "cmd_ack" && parts[1] != "" {
		return parts[1], true
	}
	return "", false
}

// cmdQueue is the task manager's Sender. The manager calls Send with its lock
// held, and a QoS 1 publish waits for the broker, so commands are handed to
// a goroutine instead. A command lost here is re-sent by the manager.
type cmdQueue struct {
	ch  chan outCmd
	log *slog.Logger
}

type outCmd struct {
	robot string
	cmd   task.Command
}

func newCmdQueue(log *slog.Logger) *cmdQueue {
	return &cmdQueue{ch: make(chan outCmd, 256), log: log}
}

func (q *cmdQueue) Send(robot string, c task.Command) error {
	select {
	case q.ch <- outCmd{robot, c}:
		return nil
	default:
		return errors.New("command queue full")
	}
}

func (q *cmdQueue) run(ctx context.Context, mq *mqttx.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-q.ch:
			body, _ := json.Marshal(o.cmd)
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := mq.Publish(pctx, "fleet/"+o.robot+"/cmd", 1, false, body)
			cancel()
			if err != nil {
				q.log.Warn("publish cmd", "robot", o.robot, "cmd_id", o.cmd.CmdID, "err", err)
			} else {
				q.log.Debug("cmd sent", "robot", o.robot, "cmd_id", o.cmd.CmdID, "type", o.cmd.Type)
			}
		}
	}
}
