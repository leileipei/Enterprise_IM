package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func runWithSignals(args []string, stdout, stderr io.Writer, reader inputReader) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWithReader(ctx, args, stdout, stderr, reader)
}
func main() { os.Exit(runWithSignals(os.Args[1:], os.Stdout, os.Stderr, readInput)) }
