// Package api serves fleetd's HTTP interface.
package api

import (
	"encoding/json"
	"net/http"

	"fleetlab/internal/fleet"
)

func New(g *fleet.Registry) http.Handler {
	mux := http.NewServeMux()
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
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
