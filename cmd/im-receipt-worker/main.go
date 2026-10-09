package main

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/receiptoutbox"
	"github.com/redis/go-redis/v9"
)

type workerConfig struct {
	DatabaseURL string
	RedisURL    string
	Stream      string
}

func redisOptionsFromURL(raw string) (*redis.Options, error) {
	options, err := redis.ParseURL(raw)
	if err != nil {
		return nil, errors.New("invalid Redis URL")
	}
	// The Worker holds a PostgreSQL row lock while XADD runs. Socket reads
	// must respect its publish context, including during Redis outages.
	options.ContextTimeoutEnabled = true
	return options, nil
}

func configFromEnv(getenv func(string) string) (workerConfig, error) {
	config := workerConfig{DatabaseURL: getenv("IM_DATABASE_URL"),
		RedisURL: getenv("IM_RECEIPT_REDIS_URL"), Stream: getenv("IM_RECEIPT_STREAM")}
	if config.DatabaseURL == "" {
		return workerConfig{}, errors.New("IM_DATABASE_URL is required")
	}
	parsed, err := url.Parse(config.RedisURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "redis" && parsed.Scheme != "rediss") {
		return workerConfig{}, errors.New("IM_RECEIPT_REDIS_URL must be redis:// or rediss:// with a host")
	}
	if _, err := redisOptionsFromURL(config.RedisURL); err != nil {
		return workerConfig{}, errors.New("IM_RECEIPT_REDIS_URL is invalid")
	}
	if config.Stream == "" {
		config.Stream = receiptoutbox.DefaultStream
	}
	if config.Stream == "enterprise-im:message-created:v1" || (getenv("IM_REALTIME_STREAM") != "" && config.Stream == getenv("IM_REALTIME_STREAM")) {
		return workerConfig{}, errors.New("receipt stream must be independent")
	}
	if len(config.Stream) > 128 || strings.IndexFunc(config.Stream, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return workerConfig{}, errors.New("IM_RECEIPT_STREAM must be a nonblank key of at most 128 bytes")
	}
	return config, nil
}

func run(ctx context.Context, config workerConfig, logger *slog.Logger) error {
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()
	redisOptions, err := redisOptionsFromURL(config.RedisURL)
	if err != nil {
		return errors.New("invalid Redis configuration")
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(checkCtx); err != nil {
		return errors.New("database unavailable at startup")
	}
	if e := (policystore.Service{DB: pool}).CheckReceiptRuntime(checkCtx); e != nil {
		return errors.New("receipt schema unavailable at startup")
	}
	if err := redisClient.Ping(checkCtx).Err(); err != nil {
		return errors.New("Redis unavailable at startup")
	}
	if err := receiptoutbox.RefreshPublisherPresence(checkCtx, redisClient, config.Stream); err != nil {
		return errors.New("publisher presence unavailable at startup")
	}
	worker := receiptoutbox.Worker{DB: pool, Publisher: receiptoutbox.RedisPublisher{Client: redisClient, Stream: config.Stream}}
	logger.Info("receipt worker started", "stream", config.Stream)
	lastPresenceRefresh := time.Now()
	lastCleanup := time.Time{}
	for ctx.Err() == nil {
		processCtx, processCancel := context.WithTimeout(ctx, 10*time.Second)
		processed, err := worker.ProcessOne(processCtx)
		processCancel()
		if time.Since(lastCleanup) >= time.Minute {
			cleanupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			count, cleanupErr := (policystore.Service{DB: pool}).CleanupExpiredSyncProofs(cleanupCtx, 1000)
			cancel()
			lastCleanup = time.Now()
			if cleanupErr != nil && ctx.Err() == nil {
				logger.Warn("sync proof cleanup failed", "error_code", "proof_cleanup_unavailable")
			} else if cleanupErr == nil && count > 0 {
				logger.Info("expired sync proofs cleaned", "count", count)
			}
		}
		if err == nil && time.Since(lastPresenceRefresh) >= 3*time.Second {
			presenceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			presenceErr := receiptoutbox.RefreshPublisherPresence(presenceCtx, redisClient, config.Stream)
			cancel()
			lastPresenceRefresh = time.Now()
			if presenceErr != nil && ctx.Err() == nil {
				logger.Warn("receipt publisher presence refresh failed", "error_code", "publisher_presence_unavailable")
			}
		}
		if err != nil && ctx.Err() == nil {
			logger.Warn("receipt event processing failed; event remains retryable", "error_code", "receipt_event_unavailable")
		}
		if processed && err == nil {
			continue
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	logger.Info("receipt worker stopped")
	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := configFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid receipt worker configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config, logger); err != nil {
		logger.Error("receipt worker stopped", "error", err)
		os.Exit(1)
	}
}
