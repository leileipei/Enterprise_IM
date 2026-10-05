package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type repairOperations interface {
	Repair(context.Context) (int, error)
	Close()
}
type auditRepairOps struct {
	pool  *pgxpool.Pool
	repo  policystore.Service
	owner string
}

func (o *auditRepairOps) Repair(ctx context.Context) (int, error) {
	return o.repo.RepairFileDownloadAudit(ctx, o.owner, 20)
}
func (o *auditRepairOps) Close() { o.pool.Close() }
func newAuditRepair(ctx context.Context, getenv func(string) string) (repairOperations, error) {
	fail := errors.New("download audit repair unavailable")
	dsn := getenv("IM_DATABASE_URL")
	if dsn == "" {
		return nil, fail
	}
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, e := pgxpool.New(check, dsn)
	if e != nil {
		return nil, fail
	}
	ok := false
	defer func() {
		if !ok {
			pool.Close()
		}
	}()
	repo := policystore.Service{DB: pool}
	if repo.CheckFileDownloadAuditRuntime(check) != nil {
		return nil, fail
	}
	var b [16]byte
	if _, e = rand.Read(b[:]); e != nil {
		return nil, fail
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	owner := fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	ok = true
	return &auditRepairOps{pool: pool, repo: repo, owner: owner}, nil
}
func runAuditRepair(ctx context.Context, once bool, out io.Writer, ops repairOperations) error {
	return runAuditRepairWithWait(ctx, once, out, ops, waitAuditRepair)
}
func waitAuditRepair(ctx context.Context, delay time.Duration) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func runAuditRepairWithWait(ctx context.Context, once bool, out io.Writer, ops repairOperations, wait func(context.Context, time.Duration) error) error {
	if ops == nil || wait == nil || out == nil {
		return errors.New("download audit repair unavailable")
	}
	defer ops.Close()
	failures := 0
	for ctx.Err() == nil {
		batch, cancel := context.WithTimeout(ctx, 5*time.Second)
		repaired, e := ops.Repair(batch)
		cancel()
		status := "file_download_audit_repair"
		if e != nil {
			status = "file_download_audit_repair_unavailable"
			failures++
		} else {
			failures = 0
		}
		if err := json.NewEncoder(out).Encode(map[string]any{"status": status, "repaired": repaired}); err != nil {
			return errors.New("download audit repair output unavailable")
		}
		if once {
			if e != nil {
				return errors.New("download audit repair unavailable")
			}
			return nil
		}
		delay := time.Second
		if failures > 0 {
			if failures >= 6 {
				delay = 30 * time.Second
			} else {
				delay = time.Duration(1<<uint(failures-1)) * time.Second
			}
		}
		if e = wait(ctx, delay); e != nil {
			return e
		}
	}
	return ctx.Err()
}
