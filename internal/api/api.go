// Package api serves fleetd's HTTP interface.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"fleetlab/internal/elevator"
	"fleetlab/internal/events"
	"fleetlab/internal/fleet"
	"fleetlab/internal/live"
	"fleetlab/internal/task"
	"fleetlab/web"
)

// Tasks is the task manager as the API sees it (task.Manager).
type Tasks interface {
	Submit(wps []task.Waypoint) (task.Task, error)
	List(state string) []task.Task
	Get(id string) (task.Task, bool)
	Cancel(id string) (task.Task, error)
}

// Elevators is implemented by task.Manager: the cars, their state, current
// trip and queue, for GET /elevators, and the operator's force-release.
type Elevators interface {
	Elevators() []elevator.View
	ReleaseElevator(id string) (elevator.View, error)
}

// New builds the HTTP handler. frames may be nil; /ws/live then answers 503.
func New(g *fleet.Registry, hub *events.Hub, tasks Tasks, frames *live.Broadcaster, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	routeTasks(mux, tasks)
	routeDashboard(mux)
	mux.HandleFunc("GET /ws/live", func(w http.ResponseWriter, r *http.Request) {
		if frames == nil {
			writeError(w, http.StatusServiceUnavailable, "live frames are off")
			return
		}
		serveLive(w, r, frames, log)
	})
	mux.HandleFunc("GET /live/stats", func(w http.ResponseWriter, r *http.Request) {
		if frames == nil {
			writeError(w, http.StatusServiceUnavailable, "live frames are off")
			return
		}
		writeJSON(w, http.StatusOK, frames.Stats())
	})
	mux.HandleFunc("GET /elevators", func(w http.ResponseWriter, r *http.Request) {
		if e, ok := tasks.(Elevators); ok {
			writeJSON(w, http.StatusOK, e.Elevators())
			return
		}
		writeJSON(w, http.StatusOK, []elevator.View{})
	})
	mux.HandleFunc("POST /elevators/{id}/release", sameSiteJSON(func(w http.ResponseWriter, r *http.Request) {
		e, ok := tasks.(Elevators)
		if !ok {
			writeError(w, http.StatusNotFound, "unknown elevator")
			return
		}
		v, err := e.ReleaseElevator(r.PathValue("id"))
		switch {
		case errors.Is(err, task.ErrNoElevator):
			writeError(w, http.StatusNotFound, "unknown elevator")
		case errors.Is(err, task.ErrNoTrip):
			writeError(w, http.StatusConflict, "elevator has no trip to release")
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, http.StatusOK, v)
		}
	}))
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
	// Event numbers start again at 1 when fleetd restarts. Every response
	// carries this run's id so a client can tell a restart from more events.
	boot := rand.Text()[:10]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(BootHeader, boot)
		mux.ServeHTTP(w, r)
	})
}

// BootHeader names the response header holding the server run's id.
const BootHeader = "X-Fleetd-Boot"

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
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := wsjson.Write(wctx, c, e)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// writeTimeout bounds one WebSocket write. A client that cannot take a frame
// in this time is closed; until then the broadcaster drops frames for it
// without waiting. A variable so tests can shorten it.
var writeTimeout = 5 * time.Second

// serveLive streams position frames to one WebSocket client. The bytes are
// the broadcaster's, shared with every other client; they are only written.
// Beyond the broadcaster's subscriber limit it answers 503 before upgrading.
// (websocket.Accept also refuses a browser page from another origin.)
func serveLive(w http.ResponseWriter, r *http.Request, frames *live.Broadcaster, log *slog.Logger) {
	sub, unsubscribe, err := frames.Subscribe()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer unsubscribe()
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()

	ctx := c.CloseRead(r.Context())
	for {
		data, err := sub.Next(ctx)
		if err != nil {
			if d := sub.Dropped(); d > 0 {
				log.Info("live client gone", "dropped_frames", d, "resyncs", sub.Resyncs())
			}
			return
		}
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		err = c.Write(wctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			log.Info("live client write failed; closing", "err", err, "dropped_frames", sub.Dropped())
			return
		}
	}
}

// routeDashboard serves web/ at /. Content types are set from the file
// extension here rather than by the OS table (on Windows the registry can map
// .js to text/plain, which browsers refuse to run as a module or script).
func routeDashboard(mux *http.ServeMux) {
	files := http.FileServerFS(web.FS)
	types := map[string]string{
		".html": "text/html; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", types[".html"])
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	for _, name := range []string{"app.js", "style.css"} {
		ct := types[path.Ext(name)]
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
