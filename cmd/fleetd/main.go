// Command fleetd is the fleet management server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"fleetlab/internal/api"
	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/mqttx"
)

type config struct {
	httpAddr string
	mqttURL  string
	dbPath   string
	logLevel string
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.httpAddr, "http", "127.0.0.1:8080", "HTTP listen address (REST, WebSocket, dashboard)")
	flag.StringVar(&c.mqttURL, "mqtt", "mqtt://127.0.0.1:1883", "MQTT broker URL")
	flag.StringVar(&c.dbPath, "db", "fleetd.db", "SQLite database path")
	flag.StringVar(&c.logLevel, "log", "info", "log level: debug, info, warn, error")
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

	hub := events.NewHub()
	registry := fleet.NewRegistry(fleet.DefaultConfig(), func(e fleet.Event) {
		log.Info("event", "robot", e.Robot, "type", e.Type, "from", e.From, "to", e.To, "detail", e.Detail)
		hub.Publish(e)
	})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				registry.Tick()
			}
		}
	}()

	mq, err := mqttx.Connect(ctx, log, cfg.mqttURL, "fleetd", []mqttx.Subscription{
		{Topic: fleet.TopicPose, QoS: 0},
		{Topic: fleet.TopicState, QoS: 1},
		{Topic: fleet.TopicAdapterStatus, QoS: 1},
	}, fleet.Ingest(registry, log))
	if err != nil {
		log.Error("mqtt", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{Addr: cfg.httpAddr, Handler: api.New(registry, hub, log), ReadHeaderTimeout: 5 * time.Second}

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
}
