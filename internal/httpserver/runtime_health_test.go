package httpserver

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

type runtimePingFunc func(context.Context) error

func (f runtimePingFunc) Ping(ctx context.Context) error { return f(ctx) }
func TestRuntimeReadySharedBudget(t *testing.T) {
	var first context.Context
	calls := 0
	h, e := HandlerWithRuntimeReady(Handler(nil), runtimePingFunc(func(ctx context.Context) error {
		first = ctx
		d, ok := ctx.Deadline()
		if !ok || time.Until(d) > 2*time.Second {
			t.Error("missing shared budget")
		}
		calls++
		return nil
	}), runtimePingFunc(func(ctx context.Context) error {
		calls++
		if ctx != first {
			t.Error("renewed readiness context")
		}
		return nil
	}))
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 200 || calls != 2 {
		t.Fatal(w.Code, calls)
	}
	failed := true
	h, e = HandlerWithRuntimeReady(Handler(nil), runtimePingFunc(func(context.Context) error {
		if failed {
			return errors.New("private dependency detail")
		}
		return nil
	}))
	if e != nil {
		t.Fatal(e)
	}
	for _, code := range []int{503, 200} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
		if w.Code != code {
			t.Fatal(w.Code)
		}
		failed = false
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/live", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	h, e = HandlerWithRuntimeReady(Handler(nil), runtimePingFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }))
	if e != nil {
		t.Fatal(e)
	}
	started := time.Now()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/health/ready", nil))
	if w.Code != 503 || time.Since(started) > 2500*time.Millisecond {
		t.Fatal("readiness budget exceeded")
	}
	if _, e = HandlerWithRuntimeReady(nil); e == nil {
		t.Fatal("nil next accepted")
	}
	if _, e = HandlerWithRuntimeReady(Handler(nil), nil); e == nil {
		t.Fatal("nil check accepted")
	}
}
