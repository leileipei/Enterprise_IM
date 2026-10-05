package policystore

import (
	"bytes"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"slices"
	"strings"
	"time"
)

const fileSelect = `id::text,tenant_id::text,conversation_id::text,uploader_user_id::text,uploader_membership_id::text,upload_request_id::text,request_digest,COALESCE(original_filename,''),COALESCE(declared_media_type,''),declared_size_bytes,state,state_version,created_at,updated_at,upload_expires_at,COALESCE(object_key,''),COALESCE(object_version_id,''),COALESCE(detected_media_type,''),actual_size_bytes,sha256,uploaded_at,COALESCE(scan_job_id::text,''),COALESCE(scan_engine,''),COALESCE(scan_definition_version,''),scanned_at,scan_sha256,deletion_requested_at,deleted_at`

func scanFile(row pgx.Row) (files.Metadata, error) {
	var m files.Metadata
	e := row.Scan(&m.ID, &m.TenantID, &m.ConversationID, &m.UploaderUserID, &m.UploaderMembershipID, &m.UploadRequestID, &m.RequestDigest, &m.OriginalFilename, &m.DeclaredMediaType, &m.DeclaredSizeBytes, &m.State, &m.StateVersion, &m.CreatedAt, &m.UpdatedAt, &m.UploadExpiresAt, &m.ObjectKey, &m.ObjectVersionID, &m.DetectedMediaType, &m.ActualSizeBytes, &m.SHA256, &m.UploadedAt, &m.ScanJobID, &m.ScanEngine, &m.ScanDefinitionVersion, &m.ScannedAt, &m.ScanSHA256, &m.DeletionRequestedAt, &m.DeletedAt)
	return m, e
}
func auditFileUser(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, fileID, action string, at time.Time) error {
	_, e := tx.Exec(ctx, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,$4,'file',$5,'allow',$4,$6)`, id.TenantID, id.UserID, id.ActingMembershipID, action, fileID, at)
	if e != nil {
		return errors.Join(ErrAuditUnavailable, e)
	}
	return nil
}
func (s Service) ReserveFile(ctx context.Context, id access.TrustedIdentity, p files.CreateParams) (files.Reservation, error) {
	id, e := fileIdentity(id)
	if e != nil {
		return files.Reservation{}, e
	}
	p, e = files.NormalizeCreate(p)
	if e != nil {
		return files.Reservation{}, e
	}
	if p.TenantID != id.TenantID || p.UploaderUserID != id.UserID || p.UploaderMembershipID != id.ActingMembershipID {
		return files.Reservation{}, files.ErrInvalidIdentity
	}
	digest, e := files.CreationDigest(p)
	if e != nil {
		return files.Reservation{}, e
	}
	return fileTransaction(ctx, s, func(tx pgx.Tx) (files.Reservation, error) {
		var zero files.Reservation
		if _, e := authorizeFileTx(ctx, tx, id, p.ConversationID, true); e != nil {
			return zero, e
		}
		var config files.UploadPolicy
		e := tx.QueryRow(ctx, `SELECT enabled,max_size_bytes,allowed_media_types,upload_ttl_seconds,tenant_storage_budget_bytes,version FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR UPDATE`, id.TenantID).Scan(&config.Enabled, &config.MaxSizeBytes, &config.AllowedMediaTypes, &config.UploadTTLSeconds, &config.TenantStorageBudgetBytes, &config.Version)
		if e != nil {
			return zero, e
		}
		at, e := authorizeFileTx(ctx, tx, id, p.ConversationID, true)
		if e != nil {
			return zero, e
		}
		old, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND uploader_user_id=$2 AND upload_request_id=$3", id.TenantID, id.UserID, p.UploadRequestID))
		if e == nil {
			if !bytes.Equal(digest[:], old.RequestDigest) {
				return zero, files.ErrUploadConflict
			}
			return files.Reservation{File: old, Duplicate: true}, nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return zero, e
		}
		if !config.Enabled {
			return zero, files.ErrUploadDisabled
		}
		if p.DeclaredSizeBytes > config.MaxSizeBytes {
			return zero, files.ErrFileTooLarge
		}
		if !slices.Contains(config.AllowedMediaTypes, p.DeclaredMediaType) {
			return zero, files.ErrFileTypeNotAllowed
		}
		var used int64
		e = tx.QueryRow(ctx, "SELECT COALESCE(sum(declared_size_bytes),0) FROM file_objects WHERE tenant_id=$1 AND state<>'deleted'", id.TenantID).Scan(&used)
		if e != nil {
			return zero, e
		}
		if used > config.TenantStorageBudgetBytes-p.DeclaredSizeBytes {
			return zero, files.ErrStorageBudgetExceeded
		}
		var fileID string
		if e = tx.QueryRow(ctx, "SELECT gen_random_uuid()::text").Scan(&fileID); e != nil {
			return zero, e
		}
		m, e := scanFile(tx.QueryRow(ctx, `INSERT INTO file_objects(id,tenant_id,conversation_id,uploader_user_id,uploader_membership_id,upload_request_id,request_digest,original_filename,declared_media_type,declared_size_bytes,state,state_version,created_at,updated_at,upload_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'allocated',0,$11,$11,$12) RETURNING `+fileSelect, fileID, id.TenantID, p.ConversationID, id.UserID, id.ActingMembershipID, p.UploadRequestID, digest[:], p.OriginalFilename, p.DeclaredMediaType, p.DeclaredSizeBytes, at, at.Add(time.Duration(config.UploadTTLSeconds)*time.Second)))
		if e != nil {
			return zero, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO file_lifecycle_events(tenant_id,file_id,from_state,to_state,reason_code,state_version,actor_kind,actor_user_id,acting_membership_id,occurred_at) VALUES($1,$2,NULL,'allocated','allocated',0,'user',$3,$4,$5)`, id.TenantID, m.ID, id.UserID, id.ActingMembershipID, at)
		if e != nil {
			return zero, e
		}
		if e = auditFileUser(ctx, tx, id, m.ID, "file_reserve", at); e != nil {
			return zero, e
		}
		if _, e = authorizeFileTx(ctx, tx, id, p.ConversationID, true); e != nil {
			return zero, e
		}
		return files.Reservation{File: m}, nil
	})
}
func (s Service) GetOwnFile(ctx context.Context, id access.TrustedIdentity, fileID string) (files.Metadata, error) {
	id, e := fileIdentity(id)
	if e != nil {
		return files.Metadata{}, e
	}
	if !directoryUUIDPattern.MatchString(fileID) {
		return files.Metadata{}, files.ErrFileNotFound
	}
	fileID = strings.ToLower(fileID)
	return fileTransaction(ctx, s, func(tx pgx.Tx) (files.Metadata, error) {
		var zero files.Metadata
		at, e := fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		actor, found, e := loadMembership(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
		if e != nil {
			return zero, e
		}
		at, e = fileClock(ctx, tx)
		if e != nil {
			return zero, e
		}
		if !found || !memberActiveAt(actor, at) {
			return zero, files.ErrInvalidIdentity
		}
		m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 AND uploader_user_id=$3 AND uploader_membership_id=$4", id.TenantID, fileID, id.UserID, id.ActingMembershipID))
		if errors.Is(e, pgx.ErrNoRows) {
			return zero, files.ErrFileNotFound
		}
		if e != nil {
			return zero, e
		}
		if _, e = authorizeFileTx(ctx, tx, id, m.ConversationID, false); e != nil {
			return zero, e
		}
		return m, nil
	})
}
