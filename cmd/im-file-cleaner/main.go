package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/filecleanup"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type cleanerOperations interface {
	Repair(context.Context) (int, error)
	Step(context.Context) (bool, error)
	Close()
}
type cleanerOps struct {
	pool   *pgxpool.Pool
	repo   policystore.Service
	worker *filecleanup.Worker
	owner  string
}

func (o *cleanerOps) Repair(ctx context.Context) (int, error) {
	return o.repo.RepairFileDownloadAudit(ctx, o.owner, 20)
}
func (o *cleanerOps) Step(ctx context.Context) (bool, error) { return o.worker.Step(ctx) }
func (o *cleanerOps) Close()                                 { o.pool.Close() }
func newCleaner(ctx context.Context, getenv func(string) string) (cleanerOperations, error) {
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, e := pgxpool.New(check, getenv("IM_DATABASE_URL"))
	if e != nil {
		return nil, errors.New("cleaner database unavailable")
	}
	ok := false
	defer func() {
		if !ok {
			pool.Close()
		}
	}()
	if e = pool.Ping(check); e != nil {
		return nil, errors.New("cleaner database unavailable")
	}
	objects, e := objectstore.NewS3VersionDeleter(objectstore.Config{Endpoint: getenv("IM_FILE_S3_ENDPOINT"), Region: getenv("IM_FILE_S3_REGION"), Bucket: getenv("IM_FILE_S3_BUCKET"), PathStyle: getenv("IM_FILE_S3_PATH_STYLE") == "true", CredentialSource: "cleanup_environment"})
	if e != nil {
		return nil, errors.New("cleaner object configuration unavailable")
	}
	var b [16]byte
	if _, e = rand.Read(b[:]); e != nil {
		return nil, errors.New("cleaner identity unavailable")
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	owner := fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	repo := policystore.Service{DB: pool}
	worker, e := filecleanup.NewWorker(repo, objects, owner)
	if e != nil {
		return nil, e
	}
	ok = true
	return &cleanerOps{pool: pool, repo: repo, worker: worker, owner: owner}, nil
}

type cleanerFactories struct {
	cleanup func(context.Context, func(string) string) (cleanerOperations, error)
	repair  func(context.Context, func(string) string) (repairOperations, error)
}

func runCleaner(ctx context.Context, args []string, getenv func(string) string, out io.Writer, factories cleanerFactories) error {
	argsConfig, e := parseCleanerArguments(args)
	if e != nil {
		return e
	}
	if !argsConfig.Execute {
		return json.NewEncoder(out).Encode(map[string]any{"status": "file_cleaner_disabled"})
	}
	if argsConfig.RepairOnly {
		if factories.repair == nil {
			return errors.New("download audit repair unavailable")
		}
		ops, e := factories.repair(ctx, getenv)
		if e != nil {
			return e
		}
		return runAuditRepair(ctx, argsConfig.Once, out, ops)
	}
	if factories.cleanup == nil {
		return errors.New("cleaner unavailable")
	}
	ops, e := factories.cleanup(ctx, getenv)
	if e != nil {
		return e
	}
	defer ops.Close()
	for ctx.Err() == nil {
		sweep, cancel := context.WithTimeout(ctx, 120*time.Second)
		repaired, repairErr := ops.Repair(sweep)
		var stepErr error
		processed, blocked := 0, 0
		if repairErr == nil {
			for processed < 20 {
				found, e := ops.Step(sweep)
				if found && errors.Is(e, filecleanup.ErrBlocked) {
					blocked++
					processed++
					continue
				}
				if e != nil {
					stepErr = e
					break
				}
				if !found {
					break
				}
				processed++
			}
		}
		cancel()
		status := "file_cleaner_sweep"
		if repairErr != nil || stepErr != nil {
			status = "file_cleaner_unavailable"
		}
		if e = json.NewEncoder(out).Encode(map[string]any{"status": status, "processed": processed, "blocked": blocked, "repaired": repaired}); e != nil {
			return e
		}
		if argsConfig.Once {
			if repairErr != nil || stepErr != nil {
				return errors.New("cleaner sweep unavailable")
			}
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if e := runCleaner(ctx, os.Args[1:], os.Getenv, os.Stdout, cleanerFactories{cleanup: newCleaner, repair: newAuditRepair}); e != nil && !errors.Is(e, context.Canceled) {
		fmt.Fprintln(os.Stderr, "file cleaner unavailable")
		os.Exit(1)
	}
}
