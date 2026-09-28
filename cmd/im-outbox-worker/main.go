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
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/redis/go-redis/v9"
)

const defaultStream = "enterprise-im:message-created:v1"

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
		RedisURL: getenv("IM_OUTBOX_REDIS_URL"), Stream: getenv("IM_OUTBOX_STREAM")}
	if config.DatabaseURL == "" {
		return workerConfig{}, errors.New("IM_DATABASE_URL is required")
	}
	parsed, err := url.Parse(config.RedisURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "redis" && parsed.Scheme != "rediss") {
		return workerConfig{}, errors.New("IM_OUTBOX_REDIS_URL must be redis:// or rediss:// with a host")
	}
	if _, err := redisOptionsFromURL(config.RedisURL); err != nil {
		return workerConfig{}, errors.New("IM_OUTBOX_REDIS_URL is invalid")
	}
	if config.Stream == "" {
		config.Stream = defaultStream
	}
	if len(config.Stream) > 128 || strings.IndexFunc(config.Stream, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return workerConfig{}, errors.New("IM_OUTBOX_STREAM must be a nonblank key of at most 128 bytes")
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
	if err := redisClient.Ping(checkCtx).Err(); err != nil {
		return errors.New("Redis unavailable at startup")
	}
	worker := outbox.Worker{DB: pool, Publisher: outbox.RedisPublisher{Client: redisClient, Stream: config.Stream}}
	logger.Info("outbox worker started", "stream", config.Stream)
	for ctx.Err() == nil {
		processed, err := worker.ProcessOne(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Warn("outbox event processing failed; event remains retryable", "error", err)
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
	logger.Info("outbox worker stopped")
	return nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := configFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid outbox worker configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, config, logger); err != nil {
		logger.Error("outbox worker stopped", "error", err)
		os.Exit(1)
	}
}
