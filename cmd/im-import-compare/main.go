package main

import (
	"context"
	c "github.com/leileipei/Enterprise_IM/internal/importcompare"
	"github.com/leileipei/Enterprise_IM/internal/importinput"
	p "github.com/leileipei/Enterprise_IM/internal/importpreflight"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const workerName = "im-import-compare-worker"

func commandMain() int {
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[0] != workerName {
		return Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deadline, err := controlDeadline(ctx)
	if err != nil {
		b := &cleanupBudget{}
		defer b.close()
		return emit(ctx, b, c.Incomplete(p.Report{}, "database", reason(err, "DATABASE_READ_FAILED")), os.Stdout, os.Stderr)
	}
	ctx, done := context.WithDeadline(ctx, deadline)
	defer done()
	return runWorker(ctx, os.Args[1:], os.Stdout, os.Stderr, importinput.Read, os.Getenv, func(cfg c.Config) c.SnapshotReader { return c.PGReader{Config: cfg} })
}
func main() { os.Exit(commandMain()) }
