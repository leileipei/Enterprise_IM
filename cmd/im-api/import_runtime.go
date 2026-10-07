package main

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/importapply"
)

func importEnabledFromEnv(getenv func(string) string, oidcEnabled bool) (bool, error) {
	switch getenv("IM_IMPORT_ENABLED") {
	case "", "false":
		return false, nil
	case "true":
		if !oidcEnabled {
			return false, errors.New("controlled import requires OIDC")
		}
		return true, nil
	default:
		return false, errors.New("invalid controlled import configuration")
	}
}
func startImportService(ctx context.Context, pool *pgxpool.Pool, enabled bool) (*importapply.Service, error) {
	if !enabled {
		return nil, nil
	}
	s, e := importapply.NewService(pool, "public")
	if e != nil {
		return nil, e
	}
	if e = s.CheckReady(ctx); e != nil {
		return nil, e
	}
	return s, nil
}
