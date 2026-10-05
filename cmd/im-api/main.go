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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/httpserver"
	"github.com/leileipei/Enterprise_IM/internal/objectstore"
	"github.com/leileipei/Enterprise_IM/internal/oidcauth"
	"github.com/leileipei/Enterprise_IM/internal/outbox"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"github.com/leileipei/Enterprise_IM/internal/realtime"
	"github.com/leileipei/Enterprise_IM/internal/webclient"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runAPI(ctx, os.Getenv, logger); err != nil {
		logger.Error("im api unavailable")
		os.Exit(1)
	}
}

func runAPI(parent context.Context, getenv func(string) string, logger *slog.Logger) (err error) {
	ctx, cancelRuntime := context.WithCancel(parent)
	var closers []func() error
	closing := false
	defer func() {
		cancelRuntime()
		if !closing {
			err = errors.Join(err, shutdownAPI(nil, closers))
		}
	}()

	dsn := getenv("IM_DATABASE_URL")
	if dsn == "" {
		logger.Error("IM_DATABASE_URL is required")
		return errors.New("api startup unavailable")
	}
	address := getenv("IM_HTTP_ADDR")
	if address == "" {
		address = ":8080"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		logger.Error("invalid database configuration")
		return errors.New("api startup unavailable")
	}
	closers = append(closers, func() error { pool.Close(); return nil })
	handler := httpserver.Handler(pool)
	enabled, authConfig, err := adminConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid OIDC configuration")
		return errors.New("api startup unavailable")
	}
	fileEnabled, fileConfig, fileSpool, err := fileUploadConfigFromEnv(getenv, enabled)
	if err != nil {
		logger.Error("invalid file upload configuration")
		return errors.New("api startup unavailable")
	}
	fileBusiness, err := fileBusinessConfigFromEnv(getenv, enabled)
	if err != nil {
		logger.Error("invalid file business configuration")
		return errors.New("api startup unavailable")
	}
	fileRuntime, err := startFileRuntime(ctx, pool, getenv, fileEnabled, fileConfig, fileSpool, fileBusiness)
	if err != nil {
		logger.Error("file runtime unavailable")
		return errors.New("api startup unavailable")
	}
	closers = append(closers, fileRuntime.Close)
	readyChecks := []httpserver.Pinger{pool, apiPinger(fileRuntime.CheckHealth)}
	webEnabled, webConfig, err := webConfigFromEnv(getenv, enabled, authConfig)
	if err != nil {
		logger.Error("invalid Web login configuration")
		return errors.New("api startup unavailable")
	}
	realtimeOptions, err := realtimeRedisOptionsFromEnv(getenv, enabled)
	if err != nil {
		logger.Error("invalid realtime configuration")
		return errors.New("api startup unavailable")
	}
	realtimeStream, err := realtimeStreamFromEnv(getenv, realtimeOptions != nil)
	if err != nil {
		logger.Error("invalid realtime Stream configuration")
		return errors.New("api startup unavailable")
	}
	if enabled {
		messageRate, err := messageRateFromEnv(os.Getenv)
		if err != nil {
			logger.Error("invalid message rate configuration")
			return errors.New("api startup unavailable")
		}
		authenticator, err := oidcauth.New(ctx, authConfig, oidcauth.Store{DB: pool})
		if err != nil {
			logger.Error("OIDC authentication unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithAdmin(pool, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("admin API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithRetentionPolicy(handler, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("retention policy API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithAuditQuery(handler, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("audit query API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithRetentionHistory(handler, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("retention history API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithLegalHolds(handler, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("legal hold API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithRetentionBatches(handler, authenticator, access.Service{DB: pool})
		if err != nil {
			logger.Error("retention batch API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithSelfContext(handler, authenticator, policystore.Service{DB: pool})
		if err != nil {
			logger.Error("self context API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithDirectory(handler, authenticator, policystore.Service{DB: pool})
		if err != nil {
			logger.Error("directory API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = assembleFileRoutes(handler, authenticator, policystore.Service{DB: pool, MessageRatePerSecond: messageRate}, access.Service{DB: pool}, fileRuntime)
		if err != nil {
			logger.Error("file API assembly unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithMessageSearch(handler, authenticator, policystore.Service{DB: pool})
		if err != nil {
			logger.Error("message search API unavailable")
			return errors.New("api startup unavailable")
		}
		handler, err = httpserver.HandlerWithCrossMessageSearch(handler, authenticator, policystore.Service{DB: pool})
		if err != nil {
			logger.Error("cross conversation search API unavailable")
			return errors.New("api startup unavailable")
		}
		if realtimeOptions != nil {
			redisClient := redis.NewClient(realtimeOptions)
			closers = append(closers, redisClient.Close)
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			pingErr := redisClient.Ping(checkCtx).Err()
			cancel()
			if pingErr != nil {
				logger.Error("realtime Redis unavailable at startup")
				return errors.New("api startup unavailable")
			}
			fanout, startErr := realtime.StartStreamFanout(ctx, redisClient, realtimeStream,
				policystore.Service{DB: pool})
			if startErr != nil {
				logger.Error("realtime Stream unavailable at startup")
				return errors.New("api startup unavailable")
			}
			readyChecks = append(readyChecks, apiPinger(func(checkCtx context.Context) error {
				if redisClient.Ping(checkCtx).Err() != nil {
					return errors.New("realtime unavailable")
				}
				select {
				case <-fanout.Done():
					return errors.New("realtime unavailable")
				default:
					return checkCtx.Err()
				}
			}))
			handler, err = httpserver.HandlerWithRealtimeNotifications(handler, authenticator,
				policystore.Service{DB: pool}, realtime.RedisTickets{Client: redisClient}, ctx, fanout)
			if err != nil {
				logger.Error("realtime API unavailable")
				return errors.New("api startup unavailable")
			}
			logger.Info("realtime notifications enabled", "stream", realtimeStream)
		}
		if webEnabled {
			handler, err = webclient.NewHandler(handler, authenticator, webConfig, nil)
			if err != nil {
				logger.Error("Web client unavailable")
				return errors.New("api startup unavailable")
			}
			logger.Info("Web client enabled", "path", "/web/")
		}
	}

	if !enabled {
		handler = httpserver.HandlerWithFileContentModes(handler, false, false)
	}
	handler, err = httpserver.HandlerWithRuntimeReady(handler, readyChecks...)
	if err != nil {
		logger.Error("runtime readiness unavailable")
		return errors.New("api startup unavailable")
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
		logger.Error("http listener unavailable")
		return errors.New("api startup unavailable")
	}
	defer listener.Close()
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.Serve(listener) }()
	logger.Info("im api listening", "address", listener.Addr().String())
	var serveErr error
	select {
	case e := <-errorsCh:
		if !errors.Is(e, http.ErrServerClosed) {
			serveErr = errors.New("http server unavailable")
		}
	case <-ctx.Done():
	}
	cancelRuntime()
	closing = true
	return errors.Join(serveErr, shutdownAPI(server, closers))
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

func fileUploadConfigFromEnv(getenv func(string) string, oidcEnabled bool) (bool, objectstore.Config, string, error) {
	var c objectstore.Config
	switch getenv("IM_FILE_UPLOAD_ENABLED") {
	case "", "false":
		return false, c, "", nil
	case "true":
	default:
		return false, c, "", errors.New("invalid file upload enable flag")
	}
	c = objectstore.Config{Endpoint: getenv("IM_FILE_S3_ENDPOINT"), Region: getenv("IM_FILE_S3_REGION"), Bucket: getenv("IM_FILE_S3_BUCKET"), CredentialSource: getenv("IM_FILE_S3_CREDENTIAL_SOURCE")}
	if c.CredentialSource == "" {
		c.CredentialSource = "environment"
	}
	switch getenv("IM_FILE_S3_PATH_STYLE") {
	case "", "false":
	case "true":
		c.PathStyle = true
	default:
		return false, c, "", errors.New("invalid S3 path style")
	}
	dir := getenv("IM_FILE_SPOOL_DIR")
	if !oidcEnabled || getenv("IM_DATABASE_URL") == "" || c.Endpoint == "" || c.Region == "" || c.Bucket == "" || c.CredentialSource != "environment" || getenv("IM_FILE_S3_ACCESS_KEY") == "" || getenv("IM_FILE_S3_SECRET_KEY") == "" || !filepath.IsAbs(dir) {
		return false, c, "", errors.New("incomplete file upload dependencies")
	}
	return true, c, dir, nil
}

func productionFileDownloadHandler(next http.Handler, uploadEnabled bool) http.Handler {
	return httpserver.HandlerWithClosedFileDownload(next, uploadEnabled)
}

func productionFileCapabilities(uploadEnabled, businessEnabled bool) httpserver.FileCapabilities {
	return httpserver.FileCapabilities{UploadEnabled: uploadEnabled, MessageSendEnabled: businessEnabled, DownloadEnabled: businessEnabled, FilenameSearchEnabled: businessEnabled}
}

func productionFileSearchHandler(next http.Handler, auth httpserver.Authenticator) http.Handler {
	return httpserver.HandlerWithClosedFileSearch(next, auth)
}
