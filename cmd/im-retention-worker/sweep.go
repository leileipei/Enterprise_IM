package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/retention"
)

type tenantLister interface {
	ListActiveTenants(context.Context) ([]string, error)
}
type bodyProcessor interface {
	ProcessTenant(context.Context, string) (retention.BatchResult, error)
}
type digestProcessor interface {
	ProcessTenant(context.Context, string) (retention.DigestBatchResult, error)
}
type sweepCounts struct{ BodiesCleared, DigestsRetired int }

var errSweepFailed = errors.New("retention sweep unavailable or partially failed")

func runSweep(ctx context.Context, lister tenantLister, body bodyProcessor, digest digestProcessor, logger *slog.Logger) (sweepCounts, error) {
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	tenants, err := lister.ListActiveTenants(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return sweepCounts{}, ctx.Err()
		}
		logger.Warn("retention tenant listing unavailable")
		return sweepCounts{}, errSweepFailed
	}
	counts := sweepCounts{}
	failed := false
	for _, tenantID := range tenants {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if body != nil {
			batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			batch, err := body.ProcessTenant(batchCtx, tenantID)
			cancel()
			if ctx.Err() != nil {
				return counts, ctx.Err()
			}
			if err != nil {
				failed = true
				logger.Warn("retention body batch failed; will retry", "tenant_id", tenantID)
			} else {
				counts.BodiesCleared += batch.ClearedCount
				if batch.ClearedCount > 0 {
					logger.Info("message bodies cleared", "tenant_id", batch.TenantID, "conversation_id", batch.ConversationID, "batch_id", batch.BatchID, "cleared_count", batch.ClearedCount)
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if digest != nil {
			batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			batch, err := digest.ProcessTenant(batchCtx, tenantID)
			cancel()
			if ctx.Err() != nil {
				return counts, ctx.Err()
			}
			if err != nil {
				failed = true
				logger.Warn("retention digest batch failed; will retry", "tenant_id", tenantID)
			} else {
				counts.DigestsRetired += batch.RetiredCount
				if batch.RetiredCount > 0 {
					logger.Info("message digests retired", "tenant_id", batch.TenantID, "conversation_id", batch.ConversationID, "batch_id", batch.BatchID, "retired_count", batch.RetiredCount)
				}
			}
		}
	}
	if failed {
		return counts, errSweepFailed
	}
	return counts, nil
}
