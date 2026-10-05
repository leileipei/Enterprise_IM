package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Pinger interface {
	Ping(context.Context) error
}

// Handler exposes only operational probes until authenticated business APIs exist.
func Handler(database Pinger) http.Handler {
	return newMux(database)
}

func newMux(database Pinger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if database == nil {
			respond(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := database.Ping(ctx); err != nil {
			respond(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		respond(w, http.StatusOK, "ready")
	})
	return mux
}

func respond(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(struct {
		Status string `json:"status"`
	}{Status: status})
}
