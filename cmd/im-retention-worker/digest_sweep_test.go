package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/retention"
)

func TestDigestCleanerConfigAndDisabled(t *testing.T) {
	cfg, err := configFromEnv(testEnv(nil))
	if err != nil || cfg.DigestEnabled || cfg.DigestBatchSize != 100 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	for _, body := range []string{"false", "true"} {
		for _, digest := range []string{"false", "true"} {
			cfg, err := configFromEnv(testEnv(map[string]string{"IM_DATABASE_URL": "postgres://localhost/db", "IM_BODY_CLEANER_ENABLED": body, "IM_DIGEST_CLEANER_ENABLED": digest}))
			if err != nil || cfg.Enabled != (body == "true") || cfg.DigestEnabled != (digest == "true") {
				t.Fatalf("switches %s/%s: %+v %v", body, digest, cfg, err)
			}
		}
	}
	for _, size := range []string{"1", "1000"} {
		if _, err := configFromEnv(testEnv(map[string]string{"IM_DIGEST_CLEANER_BATCH_SIZE": size})); err != nil {
			t.Fatal(err)
		}
	}
	for _, values := range []map[string]string{
		{"IM_DIGEST_CLEANER_ENABLED": "true"}, {"IM_DIGEST_CLEANER_ENABLED": "1"}, {"IM_DIGEST_CLEANER_ENABLED": " true "},
		{"IM_DIGEST_CLEANER_BATCH_SIZE": "0"}, {"IM_DIGEST_CLEANER_BATCH_SIZE": "-1"}, {"IM_DIGEST_CLEANER_BATCH_SIZE": "1001"}, {"IM_DIGEST_CLEANER_BATCH_SIZE": "x"},
	} {
		if _, err := configFromEnv(testEnv(values)); err == nil {
			t.Fatalf("invalid accepted: %v", values)
		}
	}
	cfg, err = configFromEnv(testEnv(map[string]string{"IM_DATABASE_URL": "invalid url"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), cfg, testLogger()); err != nil {
		t.Fatalf("disabled connected: %v", err)
	}
}

type digestLister func(context.Context) ([]string, error)

func (f digestLister) ListActiveTenants(ctx context.Context) ([]string, error) { return f(ctx) }

type digestBodyProcessor func(context.Context, string) (retention.BatchResult, error)

func (f digestBodyProcessor) ProcessTenant(ctx context.Context, id string) (retention.BatchResult, error) {
	return f(ctx, id)
}

type digestOnlyProcessor func(context.Context, string) (retention.DigestBatchResult, error)

func (f digestOnlyProcessor) ProcessTenant(ctx context.Context, id string) (retention.DigestBatchResult, error) {
	return f(ctx, id)
}

func TestDualCleanerSweepFairnessAndCancellation(t *testing.T) {
	for _, failure := range []string{"none", "body", "digest", "body timeout"} {
		t.Run(failure, func(t *testing.T) {
			calls := []string{}
			lists := 0
			var logs bytes.Buffer
			lister := digestLister(func(ctx context.Context) ([]string, error) {
				lists++
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded list")
				}
				return []string{"a", "b"}, nil
			})
			check := func(ctx context.Context) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("unbounded batch")
				}
			}
			body := digestBodyProcessor(func(ctx context.Context, id string) (retention.BatchResult, error) {
				calls = append(calls, id+"-body")
				check(ctx)
				if id == "a" && (failure == "body" || failure == "body timeout") {
					if failure == "body timeout" {
						return retention.BatchResult{}, context.DeadlineExceeded
					}
					return retention.BatchResult{}, errors.New("sensitive-body-digest-URL")
				}
				return retention.BatchResult{TenantID: id, ClearedCount: 1}, nil
			})
			digest := digestOnlyProcessor(func(ctx context.Context, id string) (retention.DigestBatchResult, error) {
				calls = append(calls, id+"-digest")
				check(ctx)
				if ctx.Err() != nil {
					t.Fatal("reused cancelled body context")
				}
				if id == "a" && failure == "digest" {
					return retention.DigestBatchResult{}, errors.New("sensitive-body-digest-URL")
				}
				return retention.DigestBatchResult{TenantID: id, RetiredCount: 2}, nil
			})
			counts, err := runSweep(context.Background(), lister, body, digest, slog.New(slog.NewTextHandler(&logs, nil)))
			wantBodies, wantDigests := 2, 4
			if failure == "body" || failure == "body timeout" {
				wantBodies = 1
			}
			if failure == "digest" {
				wantDigests = 2
			}
			if lists != 1 || strings.Join(calls, ",") != "a-body,a-digest,b-body,b-digest" || counts.BodiesCleared != wantBodies || counts.DigestsRetired != wantDigests || (failure == "none") != (err == nil) {
				t.Fatalf("sweep: %v %+v %v lists %d", calls, counts, err, lists)
			}
			if strings.Contains(logs.String(), "sensitive-body-digest-URL") {
				t.Fatal("leaked error")
			}
		})
	}
	t.Run("parent cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		lister := digestLister(func(context.Context) ([]string, error) { return []string{"a", "b"}, nil })
		body := digestBodyProcessor(func(context.Context, string) (retention.BatchResult, error) {
			calls++
			cancel()
			return retention.BatchResult{}, context.Canceled
		})
		digest := digestOnlyProcessor(func(context.Context, string) (retention.DigestBatchResult, error) {
			t.Fatal("digest ran after parent cancel")
			return retention.DigestBatchResult{}, nil
		})
		if _, err := runSweep(ctx, lister, body, digest, testLogger()); !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("cancel: %d %v", calls, err)
		}
	})
	t.Run("digest only", func(t *testing.T) {
		calls := 0
		lister := digestLister(func(context.Context) ([]string, error) { return []string{"a"}, nil })
		digest := digestOnlyProcessor(func(context.Context, string) (retention.DigestBatchResult, error) {
			calls++
			return retention.DigestBatchResult{RetiredCount: 1}, nil
		})
		counts, err := runSweep(context.Background(), lister, nil, digest, testLogger())
		if err != nil || counts.DigestsRetired != 1 || counts.BodiesCleared != 0 || calls != 1 {
			t.Fatalf("digest only: %+v %d %v", counts, calls, err)
		}
	})
}
func TestDigestCleanerProductionProcess(t *testing.T) {
	for _, mode := range []string{"digest", "both"} {
		t.Run(mode, func(t *testing.T) { testCleanerProductionProcess(t, mode) })
	}
}
