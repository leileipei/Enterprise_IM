package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var errAPIShutdown = errors.New("api shutdown unavailable")
var errAPICleanup = errors.New("api cleanup unavailable")

func shutdownAPI(server *http.Server, closers []func() error) error {
	return shutdownAPIWithBudgets(server, closers, 10*time.Second, 10*time.Second)
}
func shutdownAPIWithBudgets(server *http.Server, closers []func() error, httpGrace, closeGrace time.Duration) error {
	var result error
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), httpGrace)
		e := server.Shutdown(ctx)
		cancel()
		if e != nil {
			// Closing active connections causes request cancellation before file Close
			// waits for accepted uploads and downloads to release their resources.
			_ = server.Close()
			result = errAPIShutdown
		}
	}
	owned := append([]func() error(nil), closers...)
	done := make(chan error, 1)
	go func() {
		var failure error
		for i := len(owned) - 1; i >= 0; i-- {
			if owned[i] != nil && owned[i]() != nil {
				failure = errAPICleanup
			}
		}
		done <- failure
	}()
	timer := time.NewTimer(closeGrace)
	defer timer.Stop()
	select {
	case e := <-done:
		return errors.Join(result, e)
	case <-timer.C:
		return errors.Join(result, errAPICleanup)
	}
}
