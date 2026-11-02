package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"fleetlab/internal/task"
)

const maxBody = 1 << 20

func routeTasks(mux *http.ServeMux, tasks Tasks) {
	mux.HandleFunc("POST /tasks", func(w http.ResponseWriter, r *http.Request) {
		wps, err := decodeWaypoints(http.MaxBytesReader(w, r.Body, maxBody))
		if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("body over %d bytes", tooBig.Limit))
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		t, err := tasks.Submit(wps)
		switch {
		case errors.Is(err, task.ErrInvalid):
			writeError(w, http.StatusBadRequest, err.Error())
			return
		case err != nil: // not saved, so not created
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, t)
	})
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		if state != "" && !slices.Contains(task.States, state) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown state %q", state))
			return
		}
		writeJSON(w, http.StatusOK, tasks.List(state))
	})
	mux.HandleFunc("GET /tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := tasks.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "unknown task")
			return
		}
		writeJSON(w, http.StatusOK, t)
	})
	mux.HandleFunc("POST /tasks/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		t, err := tasks.Cancel(r.PathValue("id"))
		switch {
		case errors.Is(err, task.ErrNotFound):
			writeError(w, http.StatusNotFound, "unknown task")
		case errors.Is(err, task.ErrFinished):
			writeError(w, http.StatusConflict, "task already "+t.State)
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, http.StatusOK, t)
		}
	})
}

// decodeWaypoints reads {"waypoints":[{"x":..,"y":..,"floor":..}]}. Every
// field is required so a typo cannot silently send a robot to (0, 0).
func decodeWaypoints(body io.Reader) ([]task.Waypoint, error) {
	var req struct {
		Waypoints []struct {
			X     *float64 `json:"x"`
			Y     *float64 `json:"y"`
			Floor *int     `json:"floor"`
		} `json:"waypoints"`
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("bad JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errors.New("bad JSON: data after the object")
		}
		return nil, fmt.Errorf("bad JSON after the object: %w", err)
	}
	if len(req.Waypoints) == 0 {
		return nil, errors.New("needs at least one waypoint")
	}
	out := make([]task.Waypoint, len(req.Waypoints))
	for i, w := range req.Waypoints {
		if w.X == nil || w.Y == nil || w.Floor == nil {
			return nil, fmt.Errorf("waypoint %d needs numeric x, y and floor", i)
		}
		out[i] = task.Waypoint{X: *w.X, Y: *w.Y, Floor: *w.Floor}
	}
	return out, nil
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
