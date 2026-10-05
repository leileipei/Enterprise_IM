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
	DatabaseURL     string
	Enabled         bool
	BatchSize       int
	DigestEnabled   bool
	DigestBatchSize int
}

func configFromEnv(getenv func(string) string) (workerConfig, error) {
	cfg := workerConfig{DatabaseURL: getenv("IM_DATABASE_URL"), BatchSize: retention.DefaultBatchSize, DigestBatchSize: retention.DefaultBatchSize}
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
	switch getenv("IM_DIGEST_CLEANER_ENABLED") {
	case "", "false":
	case "true":
		cfg.DigestEnabled = true
	default:
		return workerConfig{}, errors.New("IM_DIGEST_CLEANER_ENABLED must be true or false")
	}
	if raw := getenv("IM_DIGEST_CLEANER_BATCH_SIZE"); raw != "" {
		size, err := strconv.Atoi(raw)
		if err != nil || size < 1 || size > retention.MaxBatchSize {
			return workerConfig{}, errors.New("IM_DIGEST_CLEANER_BATCH_SIZE must be between 1 and 1000")
		}
		cfg.DigestBatchSize = size
	}
	if (cfg.Enabled || cfg.DigestEnabled) && cfg.DatabaseURL == "" {
		return workerConfig{}, errors.New("IM_DATABASE_URL is required when retention clearing is enabled")
	}
	return cfg, nil
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
	if !cfg.Enabled && !cfg.DigestEnabled {
		logger.Info("retention cleaners disabled")
		return nil
	}
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()
	worker := retention.Worker{DB: pool, BatchSize: cfg.BatchSize}
	var body bodyProcessor
	var digest digestProcessor
	if cfg.Enabled {
		body = worker
	}
	if cfg.DigestEnabled {
		digest = retention.DigestWorker{DB: pool, BatchSize: cfg.DigestBatchSize}
	}
	logger.Info("retention cleaners started", "body_enabled", cfg.Enabled, "digest_enabled", cfg.DigestEnabled, "body_batch_size", cfg.BatchSize, "digest_batch_size", cfg.DigestBatchSize)
	failures := 0
	for ctx.Err() == nil {
		_, err := runSweep(ctx, worker, body, digest, logger)
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
	logger.Info("retention cleaners stopped")
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
