package policystore_test

import (
	"context"
	"errors"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"testing"
	"time"
)

func TestFileScanTwoNodes(t *testing.T) {
	c, s, m := scanFixture(t)
	ctx := context.Background()
	peer := filePeer(t, c)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT id FROM file_objects WHERE id=$1 FOR UPDATE", m.ID); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := (policystore.Service{DB: peer}).ClaimFileScan(ctx, clientB); e != nil || ok {
		t.Fatal("skip locked", ok, e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	type result struct {
		ok bool
		e  error
	}
	ch := make(chan result, 2)
	go func() { _, ok, e := s.ClaimFileScan(ctx, uploadOwner); ch <- result{ok, e} }()
	go func() { _, ok, e := (policystore.Service{DB: peer}).ClaimFileScan(ctx, clientB); ch <- result{ok, e} }()
	a, b := <-ch, <-ch
	if a.e != nil || b.e != nil || a.ok == b.ok {
		t.Fatal(a, b)
	}
	var n int
	c.QueryRow(ctx, "SELECT count(*) FROM file_scan_jobs WHERE file_id=$1", m.ID).Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
}
func TestFileScanPolicyChangeDuringWait(t *testing.T) {
	for _, withdrawType := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "type withdrawn"}[withdrawType], func(t *testing.T) {
			c, s, m := scanFixture(t)
			ctx := context.Background()
			job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
			if e != nil || !ok {
				t.Fatal(e)
			}
			peer := filePeer(t, c)
			tx, e := c.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(ctx)
			if _, e = tx.Exec(ctx, "SELECT tenant_id FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE", tenantA); e != nil {
				t.Fatal(e)
			}
			ch := make(chan error, 1)
			go func() { ch <- (policystore.Service{DB: peer}).CompleteFileScan(ctx, job, cleanScan(job)) }()
			waitFileLock(t, c, peer, "", func() { tx.Rollback(ctx) })
			query := "UPDATE tenant_file_upload_policy SET enabled=false WHERE tenant_id=$1"
			reason := "upload_disabled"
			if withdrawType {
				query = "UPDATE tenant_file_upload_policy SET allowed_media_types=ARRAY['application/pdf']::text[] WHERE tenant_id=$1"
				reason = "type_not_allowed"
			}
			if _, e = tx.Exec(ctx, query, tenantA); e != nil {
				t.Fatal(e)
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			if e = <-ch; e != nil {
				t.Fatal(e)
			}
			scanState(t, c, m.ID, files.StateRejected, 3)
			var got, engine string
			if e = c.QueryRow(ctx, "SELECT j.reason_code,f.scan_engine FROM file_scan_jobs j JOIN file_objects f ON f.id=j.file_id WHERE j.id=$1", job.JobID).Scan(&got, &engine); e != nil || got != reason || engine != "policy/tenant-file-upload" {
				t.Fatal(got, engine, e)
			}
		})
	}
}
func TestFileScanLateCASAndAuditLease(t *testing.T) {
	c, s, m := scanFixture(t)
	ctx := context.Background()
	job, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok {
		t.Fatal(e)
	}
	run(t, c, "UPDATE file_scan_jobs SET lease_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1", job.JobID)
	peer := filePeer(t, c)
	tx, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "LOCK TABLE file_worker_audit_events IN ACCESS EXCLUSIVE MODE"); e != nil {
		t.Fatal(e)
	}
	ch := make(chan error, 1)
	go func() { ch <- (policystore.Service{DB: peer}).CompleteFileScan(ctx, job, cleanScan(job)) }()
	waitFileLock(t, c, peer, "RowExclusiveLock", func() { tx.Rollback(ctx) })
	time.Sleep(2300 * time.Millisecond)
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if e = <-ch; !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal("late clean", e)
	}
	scanState(t, c, m.ID, files.StateScanning, 2)
	if _, e = s.RenewFileScan(ctx, job); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal("late renew", e)
	}
	n, e := s.RecoverExpiredFileScans(ctx, clientB, 100)
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	scanState(t, c, m.ID, files.StateScanFailed, 3)
	if e = s.CompleteFileScan(ctx, job, cleanScan(job)); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
	if _, ok, e = s.ClaimFileScan(ctx, uploadOwner); e != nil || ok {
		t.Fatal("retry before10s", ok, e)
	}
	time.Sleep(10 * time.Second)
	next, ok, e := s.ClaimFileScan(ctx, uploadOwner)
	if e != nil || !ok || next.JobID == job.JobID || next.LeaseToken == job.LeaseToken || next.Attempt != 2 {
		t.Fatal(next, ok, e)
	}
	scanState(t, c, m.ID, files.StateScanning, 4)
	if e = s.CompleteFileScan(ctx, job, cleanScan(job)); !errors.Is(e, files.ErrLeaseLost) {
		t.Fatal(e)
	}
}
