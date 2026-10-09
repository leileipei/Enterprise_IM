package policystore

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/nativeauth"
	"net/netip"
	"time"
)

func (s Service) CheckNativeAuthRuntime(ctx context.Context) error {
	if s.DB == nil {
		return nativeauth.ErrUnavailable
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return nativeauth.ErrUnavailable
	}
	defer tx.Rollback(ctx)
	var valid bool
	e = tx.QueryRow(ctx, `WITH relation AS (SELECT to_regclass(current_schema()||'.native_oauth_request_windows') AS rel_oid)
 SELECT rel_oid IS NOT NULL
 AND (SELECT count(*) FROM pg_attribute WHERE attrelid=relation.rel_oid AND attnum>0 AND NOT attisdropped)=3
 AND (SELECT count(*) FROM pg_attribute WHERE attrelid=relation.rel_oid AND attnum>0 AND NOT attisdropped AND attnotnull AND
 (attname,format_type(atttypid,atttypmod)) IN (('ip_digest','bytea'),('window_start','timestamp with time zone'),('request_count','integer')))=3
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=relation.rel_oid AND contype='p' AND convalidated AND NOT condeferrable AND pg_get_constraintdef(pg_constraint.oid)='PRIMARY KEY (ip_digest)')
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=relation.rel_oid AND conname='native_oauth_ip_digest_guard' AND contype='c' AND convalidated AND pg_get_constraintdef(pg_constraint.oid)='CHECK ((octet_length(ip_digest) = 32))')
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=relation.rel_oid AND conname='native_oauth_request_count_guard' AND contype='c' AND convalidated AND pg_get_constraintdef(pg_constraint.oid)='CHECK (((request_count >= 1) AND (request_count <= 20)))')
 AND has_table_privilege(relation.rel_oid,'SELECT')
 AND has_table_privilege(relation.rel_oid,'INSERT')
 AND has_table_privilege(relation.rel_oid,'UPDATE')
 AND has_table_privilege(relation.rel_oid,'DELETE') FROM relation`).Scan(&valid)
	if e != nil || !valid {
		return nativeauth.ErrUnavailable
	}
	return nil
}

// AllowNativeOAuthRequest uses the database clock and an atomic keyed UPSERT.
// Fixed windows and counts survive process restarts and cannot multiply across
// API nodes. Only the canonical peer address digest is persisted.
func (s Service) AllowNativeOAuthRequest(ctx context.Context, peer string) (time.Duration, error) {
	ip, e := netip.ParseAddr(peer)
	if e != nil || ip.IsUnspecified() || ip.Zone() != "" || s.DB == nil {
		return 0, nativeauth.ErrUnavailable
	}
	key := sha256.Sum256([]byte(ip.Unmap().String()))
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return 0, nativeauth.ErrUnavailable
	}
	defer tx.Rollback(ctx)
	// Bound cleanup work. The index and SKIP LOCKED prevent unrelated stale
	// windows from serializing concurrent public authentication requests.
	_, e = tx.Exec(ctx, `WITH expired AS (SELECT ip_digest FROM native_oauth_request_windows WHERE window_start<=clock_timestamp()-interval '1 minute' ORDER BY window_start,ip_digest LIMIT 256 FOR UPDATE SKIP LOCKED) DELETE FROM native_oauth_request_windows WHERE ip_digest IN (SELECT ip_digest FROM expired)`)
	if e != nil {
		return 0, nativeauth.ErrUnavailable
	}
	var count int
	e = tx.QueryRow(ctx, `WITH clock AS MATERIALIZED (SELECT clock_timestamp() AS moment)
 INSERT INTO native_oauth_request_windows(ip_digest,window_start,request_count) VALUES($1,(SELECT moment FROM clock),1)
 ON CONFLICT(ip_digest) DO UPDATE SET
 window_start=CASE WHEN native_oauth_request_windows.window_start<=((SELECT moment FROM clock)-interval '1 minute') THEN (SELECT moment FROM clock) ELSE native_oauth_request_windows.window_start END,
 request_count=CASE WHEN native_oauth_request_windows.window_start<=((SELECT moment FROM clock)-interval '1 minute') THEN 1 ELSE native_oauth_request_windows.request_count+1 END
 WHERE native_oauth_request_windows.window_start<=((SELECT moment FROM clock)-interval '1 minute') OR native_oauth_request_windows.request_count<20
 RETURNING request_count`, key[:]).Scan(&count)
	if errors.Is(e, pgx.ErrNoRows) {
		return time.Minute, nativeauth.ErrRateLimited
	}
	if e != nil {
		return 0, nativeauth.ErrUnavailable
	}
	if e = tx.Commit(ctx); e != nil {
		return 0, nativeauth.ErrUnavailable
	}
	return 0, nil
}
