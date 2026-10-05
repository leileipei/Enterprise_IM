package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Runtime readiness owns one budget for all configured dependencies. It never
// delegates the readiness request to inner routers with independent budgets.
func HandlerWithRuntimeReady(next http.Handler, checks ...Pinger) (http.Handler, error) {
	if next == nil {
		return nil, errors.New("runtime readiness requires a handler")
	}
	owned := append([]Pinger(nil), checks...)
	for _, c := range owned {
		if c == nil {
			return nil, errors.New("runtime readiness requires complete checks")
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health/ready" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		for _, c := range owned {
			if ctx.Err() != nil || c.Ping(ctx) != nil {
				respond(w, 503, "unavailable")
				return
			}
		}
		if ctx.Err() != nil {
			respond(w, 503, "unavailable")
			return
		}
		respond(w, 200, "ready")
	}), nil
}
