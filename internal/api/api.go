// Package api serves fleetd's HTTP interface.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/task"
)

// Tasks is the task manager as the API sees it (task.Manager).
type Tasks interface {
	Submit(wps []task.Waypoint) (task.Task, error)
	List(state string) []task.Task
	Get(id string) (task.Task, bool)
	Cancel(id string) (task.Task, error)
}

func New(g *fleet.Registry, hub *events.Hub, tasks Tasks, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	routeTasks(mux, tasks)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /robots", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, g.Snapshot())
	})
	mux.HandleFunc("GET /robots/{name}", func(w http.ResponseWriter, r *http.Request) {
		robot, ok := g.Get(r.PathValue("name"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown robot"})
			return
		}
		writeJSON(w, http.StatusOK, robot)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		if s := r.URL.Query().Get("since"); s != "" {
			seq, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be an event seq"})
				return
			}
			writeJSON(w, http.StatusOK, hub.Since(seq))
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		writeJSON(w, http.StatusOK, hub.Recent(n))
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		serveEvents(w, r, hub, log)
	})
	return mux
}

// serveEvents streams live events to one WebSocket client.
func serveEvents(w http.ResponseWriter, r *http.Request, hub *events.Hub, log *slog.Logger) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	sub, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	// We never read client messages, but CloseRead handles pings and closes.
	ctx := c.CloseRead(r.Context())
	for {
		select {
		case <-ctx.Done():
			if d := sub.Dropped(); d > 0 {
				log.Info("ws client gone", "dropped_events", d)
			}
			return
		case e := <-sub.C:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(wctx, c, e)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
