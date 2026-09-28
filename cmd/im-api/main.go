package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/oidcauth"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	dsn := os.Getenv("IM_DATABASE_URL")
	if dsn == "" {
		logger.Error("IM_DATABASE_URL is required")
		os.Exit(1)
	}
	address := os.Getenv("IM_HTTP_ADDR")
	if address == "" {
		address = ":8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		logger.Error("invalid database configuration")
		os.Exit(1)
	}
	defer pool.Close()
	handler := httpserver.Handler(pool)
	enabled, authConfig, err := adminConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid OIDC configuration", "error", err)
		os.Exit(1)
	}
	if enabled {
		authenticator, err := oidcauth.New(ctx, authConfig, oidcauth.Store{DB: pool})
		if err != nil {
			logger.Error("OIDC authentication unavailable", "error", err)
			os.Exit(1)
		}
		handler, err = httpserver.HandlerWithAdmin(pool, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("admin API unavailable", "error", err)
			os.Exit(1)
		}
	}

	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.ListenAndServe() }()
	logger.Info("im api listening", "address", address)
	select {
	case err := <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown failed", "error", err)
			os.Exit(1)
		}
	}
}

func adminConfigFromEnv(getenv func(string) string) (bool, oidcauth.Config, error) {
	switch getenv("IM_OIDC_ENABLED") {
	case "", "false":
		return false, oidcauth.Config{}, nil
	case "true":
		config := oidcauth.Config{
			Issuer:           getenv("IM_OIDC_ISSUER"),
			Audience:         getenv("IM_OIDC_AUDIENCE"),
			JWKSURL:          getenv("IM_OIDC_JWKS_URL"),
			AllowedClientIDs: strings.Split(getenv("IM_OIDC_ALLOWED_CLIENT_IDS"), ","),
		}
		if err := config.Validate(); err != nil {
			return false, oidcauth.Config{}, err
		}
		return true, config, nil
	default:
		return false, oidcauth.Config{}, errors.New("IM_OIDC_ENABLED must be true or false")
	}
}
