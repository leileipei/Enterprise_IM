package importcompare

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"os"
	"strings"
	"time"
)

func driverConfig(c Config) (*pgx.ConnConfig, error) {
	// Parsing can itself read credentials/service files: reject contamination first.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "PG") || name == "HOME" || name == "USERPROFILE" {
			return nil, configFailure()
		}
	}
	if len(c.connection) == 0 || !schemaName.MatchString(c.Schema) {
		return nil, configFailure()
	}
	cfg, err := pgx.ParseConfigWithOptions(c.connection.canonical(), pgx.ParseConfigOptions{ParseConfigOptions: pgconn.ParseConfigOptions{ConnStringAllowedKeys: connectionKeys}})
	if err != nil {
		return nil, configFailure()
	}
	cfg.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, CancelRequestDelay: 0, DeadlineDelay: 200 * time.Millisecond}
	}
	cfg.RuntimeParams = map[string]string{}
	cfg.Fallbacks = nil
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.StatementCacheCapacity = 0
	cfg.DescriptionCacheCapacity = 0
	return cfg, nil
}
