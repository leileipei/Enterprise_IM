package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/retention"
)

type workerConfig struct {
	DatabaseURL string
	Enabled     bool
	BatchSize   int
}

func configFromEnv(getenv func(string) string) (workerConfig, error) {
	cfg := workerConfig{DatabaseURL: getenv("IM_DATABASE_URL"), BatchSize: retention.DefaultBatchSize}
	switch getenv("IM_BODY_CLEANER_ENABLED") {
	case "", "false":
	case "true":
		cfg.Enabled = true
	default:
		return workerConfig{}, errors.New("IM_BODY_CLEANER_ENABLED must be true or false")
	}
	if raw := getenv("IM_BODY_CLEANER_BATCH_SIZE"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > retention.MaxBatchSize {
			return workerConfig{}, errors.New("IM_BODY_CLEANER_BATCH_SIZE must be between 1 and 1000")
		}
		cfg.BatchSize = size
	}
	if cfg.Enabled && cfg.DatabaseURL == "" {
		return workerConfig{}, errors.New("IM_DATABASE_URL is required when body clearing is enabled")
	}
	return cfg, nil
}

type sweepWorker interface {
	ListActiveTenants(context.Context) ([]string, error)
	ProcessTenant(context.Context, string) (retention.BatchResult, error)
}

var errSweepFailed = errors.New("retention sweep unavailable or partially failed")

func runSweep(ctx context.Context, worker sweepWorker, logger *slog.Logger) (int, error) {
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	tenants, err := worker.ListActiveTenants(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		logger.Warn("retention tenant listing unavailable")
		return 0, errSweepFailed
	}
	count := 0
	failed := false
	for _, tenantID := range tenants {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		batch, err := worker.ProcessTenant(batchCtx, tenantID)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return count, ctx.Err()
			}
			failed = true
			logger.Warn("retention batch failed; will retry", "tenant_id", tenantID)
			continue
		}
		count += batch.ClearedCount
		if batch.ClearedCount > 0 {
			logger.Info("message bodies cleared", "tenant_id", batch.TenantID, "conversation_id", batch.ConversationID, "batch_id", batch.BatchID, "cleared_count", batch.ClearedCount)
		}
	}
	if failed {
		return count, errSweepFailed
	}
	return count, nil
}

func sweepDelay(failures int) time.Duration {
	if failures <= 1 {
		return time.Second
	}
	if failures >= 6 {
		return 30 * time.Second
	}
	return time.Second << uint(failures-1)
}

func run(ctx context.Context, cfg workerConfig, logger *slog.Logger) error {
	if !cfg.Enabled {
		logger.Info("retention body cleaner disabled")
		return nil
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()
	worker := retention.Worker{DB: pool, BatchSize: cfg.BatchSize}
	logger.Info("retention body cleaner started", "batch_size", cfg.BatchSize)
	failures := 0
	for ctx.Err() == nil {
		_, err := runSweep(ctx, worker, logger)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			if failures < 6 {
				failures++
			}
		} else {
			failures = 0
		}
		timer := time.NewTimer(sweepDelay(failures))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	logger.Info("retention body cleaner stopped")
	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid retention worker configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("retention worker stopped", "error", err)
		os.Exit(1)
	}
}
