package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/oidcauth"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/leileipei/Enterprise_IM/internal/webclient"
	"github.com/redis/go-redis/v9"
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
	webEnabled, webConfig, err := webConfigFromEnv(os.Getenv, enabled, authConfig)
	if err != nil {
		logger.Error("invalid Web login configuration", "error", err)
		os.Exit(1)
	}
	realtimeOptions, err := realtimeRedisOptionsFromEnv(os.Getenv, enabled)
	if err != nil {
		logger.Error("invalid realtime configuration", "error", err)
		os.Exit(1)
	}
	realtimeStream, err := realtimeStreamFromEnv(os.Getenv, realtimeOptions != nil)
	if err != nil {
		logger.Error("invalid realtime Stream configuration", "error", err)
		os.Exit(1)
	}
	if enabled {
		messageRate, err := messageRateFromEnv(os.Getenv)
		if err != nil {
			logger.Error("invalid message rate configuration", "error", err)
			os.Exit(1)
		}
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
		handler, err = httpserver.HandlerWithRetentionPolicy(handler, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("retention policy API unavailable", "error", err)
			os.Exit(1)
		}
		handler, err = httpserver.HandlerWithSelfContext(handler, authenticator, policystore.Service{DB: pool})
		if err != nil {
			logger.Error("self context API unavailable", "error", err)
			os.Exit(1)
		}
		handler, err = httpserver.HandlerWithDirectory(handler, authenticator, policystore.Service{DB: pool})
		if err != nil {
			logger.Error("directory API unavailable", "error", err)
			os.Exit(1)
		}
		handler, err = httpserver.HandlerWithConversations(handler, authenticator, policystore.Service{DB: pool, MessageRatePerSecond: messageRate})
		if err != nil {
			logger.Error("conversation API unavailable", "error", err)
			os.Exit(1)
		}
		if realtimeOptions != nil {
			redisClient := redis.NewClient(realtimeOptions)
			defer redisClient.Close()
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pingErr := redisClient.Ping(checkCtx).Err()
			cancel()
			if pingErr != nil {
				logger.Error("realtime Redis unavailable at startup")
				os.Exit(1)
			}
			fanout, startErr := realtime.StartStreamFanout(ctx, redisClient, realtimeStream,
				policystore.Service{DB: pool})
			if startErr != nil {
				logger.Error("realtime Stream unavailable at startup", "error", startErr)
				os.Exit(1)
			}
			handler, err = httpserver.HandlerWithRealtimeNotifications(handler, authenticator,
				policystore.Service{DB: pool}, realtime.RedisTickets{Client: redisClient}, ctx, fanout)
			if err != nil {
				logger.Error("realtime API unavailable", "error", err)
				os.Exit(1)
			}
			logger.Info("realtime notifications enabled", "stream", realtimeStream)
		}
		if webEnabled {
			handler, err = webclient.NewHandler(handler, authenticator, webConfig, nil)
			if err != nil {
				logger.Error("Web client unavailable", "error", err)
				os.Exit(1)
			}
			logger.Info("Web client enabled", "path", "/web/")
		}
	}

	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		logger.Error("http listener unavailable", "error", err)
		os.Exit(1)
	}
	defer listener.Close()
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.Serve(listener) }()
	logger.Info("im api listening", "address", listener.Addr().String())
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

func realtimeStreamFromEnv(getenv func(string) string, enabled bool) (string, error) {
	stream := getenv("IM_REALTIME_STREAM")
	if !enabled {
		if stream != "" {
			return "", errors.New("IM_REALTIME_STREAM requires IM_REALTIME_REDIS_URL")
		}
		return "", nil
	}
	if stream == "" {
		stream = outbox.DefaultStream
	}
	if len(stream) > 128 || strings.IndexFunc(stream, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", errors.New("IM_REALTIME_STREAM must be a nonblank key of at most 128 bytes")
	}
	return stream, nil
}

func realtimeRedisOptionsFromEnv(getenv func(string) string, oidcEnabled bool) (*redis.Options, error) {
	raw := getenv("IM_REALTIME_REDIS_URL")
	if raw == "" {
		return nil, nil
	}
	if !oidcEnabled {
		return nil, errors.New("IM_REALTIME_REDIS_URL requires IM_OIDC_ENABLED=true")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "redis" && parsed.Scheme != "rediss") {
		return nil, errors.New("IM_REALTIME_REDIS_URL must be redis:// or rediss:// with a host")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		return nil, errors.New("IM_REALTIME_REDIS_URL is invalid")
	}
	options.ContextTimeoutEnabled = true
	return options, nil
}

func messageRateFromEnv(getenv func(string) string) (int, error) {
	raw := getenv("IM_MESSAGE_RATE_PER_SECOND")
	if raw == "" {
		return 10, nil
	}
	rate, err := strconv.Atoi(raw)
	if err != nil || rate <= 0 || rate > 10000 {
		return 0, errors.New("IM_MESSAGE_RATE_PER_SECOND must be an integer from 1 to 10000")
	}
	return rate, nil
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

func webConfigFromEnv(getenv func(string) string, oidcEnabled bool, auth oidcauth.Config) (bool, webclient.Config, error) {
	switch getenv("IM_WEB_ENABLED") {
	case "", "false":
		return false, webclient.Config{}, nil
	case "true":
		if !oidcEnabled {
			return false, webclient.Config{}, errors.New("IM_WEB_ENABLED requires IM_OIDC_ENABLED=true")
		}
		scope := getenv("IM_WEB_SCOPE")
		if scope == "" {
			scope = "openid profile"
		}
		config := webclient.Config{Issuer: auth.Issuer, AuthorizationURL: getenv("IM_WEB_AUTHORIZATION_URL"),
			TokenURL: getenv("IM_WEB_TOKEN_URL"), ClientID: getenv("IM_WEB_CLIENT_ID"),
			RedirectURL: getenv("IM_WEB_REDIRECT_URL"), Scope: scope}
		if err := config.Validate(); err != nil {
			return false, webclient.Config{}, err
		}
		for _, allowed := range auth.AllowedClientIDs {
			if allowed == config.ClientID {
				return true, config, nil
			}
		}
		return false, webclient.Config{}, errors.New("web client ID is not in OIDC allowed clients")
	default:
		return false, webclient.Config{}, errors.New("IM_WEB_ENABLED must be true or false")
	}
}
