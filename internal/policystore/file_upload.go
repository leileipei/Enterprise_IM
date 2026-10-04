package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const attemptSelect = `id::text,lease_token::text,owner_id::text,lease_expires_at,phase,actual_size_bytes,sha256,COALESCE(detected_media_type,''),COALESCE(object_version_id,'')`

func scanAttempt(row pgx.Row, m files.Metadata) (files.UploadTicket, error) {
	u := files.UploadTicket{File: m}
	var size *int64
	var hash []byte
	var detected string
	e := row.Scan(&u.AttemptID, &u.LeaseToken, &u.OwnerID, &u.LeaseExpiresAt, &u.Phase, &size, &hash, &detected, &u.ObjectVersionID)
	if e != nil {
		return u, e
	}
	if size != nil {
		if len(hash) != 32 {
			return u, files.ErrDependencyUnavailable
		}
		v := files.Measurement{SizeBytes: *size, DetectedMediaType: detected}
		copy(v.SHA256[:], hash)
		u.Measurement = &v
	}
	return u, nil
}
func fileNewIDs(ctx context.Context, tx pgx.Tx) (string, string, error) {
	var a, b string
	e := tx.QueryRow(ctx, "SELECT gen_random_uuid()::text,gen_random_uuid()::text").Scan(&a, &b)
	return a, b, e
}
func uploadLeaseEnd(at, expires time.Time) time.Time {
	end := at.Add(180 * time.Second)
	if end.After(expires) {
		end = expires
	}
	return end
}
func uploadVersionValid(s string) bool {
	return s != "" && s != "null" && len(s) <= 1024 && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Locks actor/peers, conversation, policy, configuration, then the owned file.
func uploadFileContext(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, fileID string) (files.Metadata, time.Time, error) {
	var zero files.Metadata
	if !directoryUUIDPattern.MatchString(fileID) {
		return zero, time.Time{}, files.ErrFileNotFound
	}
	fileID = strings.ToLower(fileID)
	at, e := fileClock(ctx, tx)
	if e != nil {
		return zero, at, e
	}
	a, found, e := loadMembershipSnapshot(ctx, tx, id.TenantID, id.ActingMembershipID, id.UserID)
	if e != nil {
		return zero, at, e
	}
	if !found || !memberActiveAt(a, at) {
		return zero, at, files.ErrInvalidIdentity
	}
	m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 AND uploader_user_id=$3 AND uploader_membership_id=$4", id.TenantID, fileID, id.UserID, id.ActingMembershipID))
	if errors.Is(e, pgx.ErrNoRows) {
		return zero, at, files.ErrFileNotFound
	}
	if e != nil {
		return zero, at, e
	}
	at, e = authorizeFileTx(ctx, tx, id, m.ConversationID, true)
	if e != nil {
		return zero, at, e
	}
	var config files.UploadPolicy
	e = tx.QueryRow(ctx, `SELECT enabled,max_size_bytes,allowed_media_types FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID).Scan(&config.Enabled, &config.MaxSizeBytes, &config.AllowedMediaTypes)
	if e != nil {
		return zero, at, e
	}
	m, e = scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 FOR UPDATE", id.TenantID, m.ID))
	if e != nil {
		return zero, at, e
	}
	at, e = authorizeFileTx(ctx, tx, id, m.ConversationID, true)
	if e != nil {
		return zero, at, e
	}
	if m.State != files.StateAllocated {
		return zero, at, files.ErrAlreadyUploaded
	}
	if !at.Before(m.UploadExpiresAt) {
		return zero, at, files.ErrUploadExpired
	}
	if !config.Enabled {
		return zero, at, files.ErrUploadDisabled
	}
	if m.DeclaredSizeBytes > config.MaxSizeBytes {
		return zero, at, files.ErrFileTooLarge
	}
	if !slices.Contains(config.AllowedMediaTypes, m.DeclaredMediaType) {
		return zero, at, files.ErrFileTypeNotAllowed
	}
	return m, at, nil
}
func checkUploadFinal(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, u files.UploadTicket) error {
	at, e := authorizeFileTx(ctx, tx, id, u.File.ConversationID, true)
	if e != nil {
		return e
	}
	if !at.Before(u.File.UploadExpiresAt) {
		return files.ErrUploadExpired
	}
	if !at.Before(u.LeaseExpiresAt) {
		return files.ErrLeaseLost
	}
	return nil
}
func (s Service) AcquireFileUpload(ctx context.Context, id access.TrustedIdentity, fileID, ownerID string) (files.UploadTicket, error) {
	id, e := fileIdentity(id)
	if e != nil {
		return files.UploadTicket{}, e
	}
	if !directoryUUIDPattern.MatchString(ownerID) {
		return files.UploadTicket{}, files.ErrInvalidMetadata
	}
	ownerID = strings.ToLower(ownerID)
	return fileTransaction(ctx, s, func(tx pgx.Tx) (files.UploadTicket, error) {
		m, at, e := uploadFileContext(ctx, tx, id, fileID)
		if e != nil {
			return files.UploadTicket{}, e
		}
		u, e := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptSelect+" FROM file_upload_attempts WHERE tenant_id=$1 AND file_id=$2 AND phase NOT IN ('receive_failed','sealed') FOR UPDATE", id.TenantID, m.ID), m)
		if e != nil && !errors.Is(e, pgx.ErrNoRows) {
			return u, e
		}
		newAttempt := errors.Is(e, pgx.ErrNoRows)
		if !newAttempt {
			at, e = authorizeFileTx(ctx, tx, id, m.ConversationID, true)
			if e != nil {
				return u, e
			}
			if !at.Before(m.UploadExpiresAt) {
				return u, files.ErrUploadExpired
			}
			if u.Phase == files.UploadStoring || u.Phase == files.UploadRecoveryPending {
				return u, files.ErrRecoveryPending
			}
			if at.Before(u.LeaseExpiresAt) {
				return u, files.ErrUploadBusy
			}
			if u.Phase == files.UploadReceiving {
				if _, e = tx.Exec(ctx, "UPDATE file_upload_attempts SET phase='receive_failed',last_reason_code='receive_failed',updated_at=$3 WHERE tenant_id=$1 AND id=$2", id.TenantID, u.AttemptID, at); e != nil {
					return u, e
				}
				newAttempt = true
			}
		}
		attemptID, token, e := fileNewIDs(ctx, tx)
		if e != nil {
			return u, e
		}
		expiry := uploadLeaseEnd(at, m.UploadExpiresAt)
		if newAttempt {
			u, e = scanAttempt(tx.QueryRow(ctx, `INSERT INTO file_upload_attempts(id,tenant_id,file_id,phase,lease_token,owner_id,lease_expires_at,created_at,updated_at) VALUES($1,$2,$3,'receiving',$4,$5,$6,$7,$7) RETURNING `+attemptSelect, attemptID, id.TenantID, m.ID, token, ownerID, expiry, at), m)
		} else {
			u, e = scanAttempt(tx.QueryRow(ctx, `UPDATE file_upload_attempts SET lease_token=$3,owner_id=$4,lease_expires_at=$5,updated_at=$6 WHERE tenant_id=$1 AND id=$2 RETURNING `+attemptSelect, id.TenantID, u.AttemptID, token, ownerID, expiry, at), m)
		}
		if e != nil {
			return u, e
		}
		if e = checkUploadFinal(ctx, tx, id, u); e != nil {
			return u, e
		}
		return u, nil
	})
}
func mutateFileUpload(ctx context.Context, s Service, id access.TrustedIdentity, input files.UploadTicket, fn func(pgx.Tx, files.UploadTicket, time.Time) (files.UploadTicket, error)) (files.UploadTicket, error) {
	id, e := fileIdentity(id)
	if e != nil {
		return files.UploadTicket{}, e
	}
	if !directoryUUIDPattern.MatchString(input.AttemptID) || !directoryUUIDPattern.MatchString(input.LeaseToken) || !directoryUUIDPattern.MatchString(input.OwnerID) {
		return files.UploadTicket{}, files.ErrLeaseLost
	}
	return fileTransaction(ctx, s, func(tx pgx.Tx) (files.UploadTicket, error) {
		m, at, e := uploadFileContext(ctx, tx, id, input.File.ID)
		if e != nil {
			return files.UploadTicket{}, e
		}
		u, e := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptSelect+" FROM file_upload_attempts WHERE tenant_id=$1 AND file_id=$2 AND id=$3 FOR UPDATE", id.TenantID, m.ID, input.AttemptID), m)
		if errors.Is(e, pgx.ErrNoRows) {
			return u, files.ErrLeaseLost
		}
		if e != nil {
			return u, e
		}
		if u.LeaseToken != input.LeaseToken || u.OwnerID != input.OwnerID {
			return u, files.ErrLeaseLost
		}
		at, e = authorizeFileTx(ctx, tx, id, m.ConversationID, true)
		if e != nil {
			return u, e
		}
		if !at.Before(m.UploadExpiresAt) {
			return u, files.ErrUploadExpired
		}
		if !at.Before(u.LeaseExpiresAt) {
			return u, files.ErrLeaseLost
		}
		out, e := fn(tx, u, at)
		if e != nil {
			return out, e
		}
		if e = checkUploadFinal(ctx, tx, id, out); e != nil {
			return out, e
		}
		return out, nil
	})
}
func (s Service) RenewFileUpload(ctx context.Context, id access.TrustedIdentity, t files.UploadTicket) (files.UploadTicket, error) {
	return mutateFileUpload(ctx, s, id, t, func(tx pgx.Tx, u files.UploadTicket, at time.Time) (files.UploadTicket, error) {
		if u.Phase != files.UploadReceiving && u.Phase != files.UploadReceived && u.Phase != files.UploadStoring && u.Phase != files.UploadRecovered {
			return u, files.ErrLeaseLost
		}
		return scanAttempt(tx.QueryRow(ctx, "UPDATE file_upload_attempts SET lease_expires_at=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2 RETURNING "+attemptSelect, u.File.TenantID, u.AttemptID, uploadLeaseEnd(at, u.File.UploadExpiresAt), at), u.File)
	})
}
func (s Service) ReceiveFileUpload(ctx context.Context, id access.TrustedIdentity, t files.UploadTicket, m files.Measurement) (files.UploadTicket, error) {
	return mutateFileUpload(ctx, s, id, t, func(tx pgx.Tx, u files.UploadTicket, at time.Time) (files.UploadTicket, error) {
		if files.ValidateMeasurement(m, u.File) != nil {
			return u, files.ErrInvalidFileSize
		}
		if u.Measurement != nil {
			if *u.Measurement != m {
				return u, files.ErrUploadConflict
			}
			if u.Phase != files.UploadReceived && u.Phase != files.UploadRecovered {
				return u, files.ErrUploadBusy
			}
			return u, nil
		}
		if u.Phase != files.UploadReceiving {
			return u, files.ErrLeaseLost
		}
		return scanAttempt(tx.QueryRow(ctx, `UPDATE file_upload_attempts SET phase='received',actual_size_bytes=$3,sha256=$4,detected_media_type=$5,updated_at=$6 WHERE tenant_id=$1 AND id=$2 RETURNING `+attemptSelect, u.File.TenantID, u.AttemptID, m.SizeBytes, m.SHA256[:], m.DetectedMediaType, at), u.File)
	})
}
func (s Service) BeginFileObjectWrite(ctx context.Context, id access.TrustedIdentity, t files.UploadTicket) (files.UploadTicket, error) {
	return mutateFileUpload(ctx, s, id, t, func(tx pgx.Tx, u files.UploadTicket, at time.Time) (files.UploadTicket, error) {
		if u.Phase != files.UploadReceived {
			return u, files.ErrUploadBusy
		}
		return scanAttempt(tx.QueryRow(ctx, "UPDATE file_upload_attempts SET phase='storing',updated_at=$3 WHERE tenant_id=$1 AND id=$2 RETURNING "+attemptSelect, u.File.TenantID, u.AttemptID, at), u.File)
	})
}
func (s Service) RememberFileObject(ctx context.Context, id access.TrustedIdentity, t files.UploadTicket, versionID string) (files.UploadTicket, error) {
	if !uploadVersionValid(versionID) {
		return files.UploadTicket{}, files.ErrInvalidMetadata
	}
	return mutateFileUpload(ctx, s, id, t, func(tx pgx.Tx, u files.UploadTicket, at time.Time) (files.UploadTicket, error) {
		if u.Phase == files.UploadRecovered {
			if u.ObjectVersionID != versionID {
				return u, files.ErrUploadConflict
			}
			return u, nil
		}
		if u.Phase != files.UploadStoring {
			return u, files.ErrLeaseLost
		}
		return scanAttempt(tx.QueryRow(ctx, "UPDATE file_upload_attempts SET phase='recovered',object_version_id=$3,updated_at=$4 WHERE tenant_id=$1 AND id=$2 RETURNING "+attemptSelect, u.File.TenantID, u.AttemptID, versionID, at), u.File)
	})
}
func (s Service) SealFileUpload(ctx context.Context, id access.TrustedIdentity, t files.UploadTicket) (files.Metadata, error) {
	out, e := mutateFileUpload(ctx, s, id, t, func(tx pgx.Tx, u files.UploadTicket, at time.Time) (files.UploadTicket, error) {
		if u.Phase != files.UploadRecovered || u.Measurement == nil || !uploadVersionValid(u.ObjectVersionID) {
			return u, files.ErrRecoveryPending
		}
		m := u.Measurement
		f, e := scanFile(tx.QueryRow(ctx, `UPDATE file_objects SET state='uploaded',state_version=state_version+1,updated_at=$3,uploaded_at=$3,object_key=$4,object_version_id=$5,actual_size_bytes=$6,sha256=$7,detected_media_type=$8 WHERE tenant_id=$1 AND id=$2 AND state='allocated' RETURNING `+fileSelect, u.File.TenantID, u.File.ID, at, "tenants/"+u.File.TenantID+"/files/"+u.File.ID, u.ObjectVersionID, m.SizeBytes, m.SHA256[:], m.DetectedMediaType))
		if e != nil {
			return u, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO file_lifecycle_events(tenant_id,file_id,state_version,from_state,to_state,reason_code,occurred_at,actor_kind,actor_user_id,acting_membership_id) VALUES($1,$2,$3,'allocated','uploaded','upload_sealed',$4,'user',$5,$6)`, f.TenantID, f.ID, f.StateVersion, at, f.UploaderUserID, f.UploaderMembershipID)
		if e != nil {
			return u, e
		}
		if e = auditFileUser(ctx, tx, access.TrustedIdentity{TenantID: f.TenantID, UserID: f.UploaderUserID, ActingMembershipID: f.UploaderMembershipID}, f.ID, "file_upload_seal", at); e != nil {
			return u, e
		}
		_, e = tx.Exec(ctx, "UPDATE file_upload_attempts SET phase='sealed',updated_at=$3 WHERE tenant_id=$1 AND id=$2", f.TenantID, u.AttemptID, at)
		if e != nil {
			return u, e
		}
		u.File = f
		u.Phase = files.UploadSealed
		return u, nil
	})
	return out.File, e
}
func (s Service) FailFileUpload(ctx context.Context, id access.TrustedIdentity, input files.UploadTicket, reasonCode string) error {
	var e error
	id, e = fileIdentity(id)
	if e != nil {
		return e
	}
	if !directoryUUIDPattern.MatchString(input.AttemptID) || !directoryUUIDPattern.MatchString(input.LeaseToken) || !directoryUUIDPattern.MatchString(input.OwnerID) || !directoryUUIDPattern.MatchString(input.File.ID) {
		return files.ErrLeaseLost
	}
	if !slices.Contains([]string{"receive_failed", "object_write_uncertain", "object_read_failed", "audit_unavailable", "lease_lost", "upload_expired"}, reasonCode) {
		return files.ErrInvalidMetadata
	}
	_, e = fileTransaction(ctx, s, func(tx pgx.Tx) (bool, error) {
		m, e := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND id=$2 AND uploader_user_id=$3 AND uploader_membership_id=$4 FOR UPDATE", id.TenantID, input.File.ID, id.UserID, id.ActingMembershipID))
		if errors.Is(e, pgx.ErrNoRows) {
			return false, files.ErrFileNotFound
		}
		if e != nil {
			return false, e
		}
		u, e := scanAttempt(tx.QueryRow(ctx, "SELECT "+attemptSelect+" FROM file_upload_attempts WHERE tenant_id=$1 AND file_id=$2 AND id=$3 FOR UPDATE", id.TenantID, m.ID, input.AttemptID), m)
		if e != nil || u.LeaseToken != input.LeaseToken || u.OwnerID != input.OwnerID || u.Phase == files.UploadSealed || u.Phase == files.UploadReceiveFailed {
			return false, files.ErrLeaseLost
		}
		at, e := fileClock(ctx, tx)
		if e != nil {
			return false, e
		}
		phase := u.Phase
		if phase == files.UploadReceiving {
			phase = files.UploadReceiveFailed
		} else if phase == files.UploadStoring {
			phase = files.UploadRecoveryPending
		}
		_, e = tx.Exec(ctx, `UPDATE file_upload_attempts SET phase=$3,lease_expires_at=LEAST($4::timestamptz,$6::timestamptz),updated_at=$4::timestamptz,last_reason_code=$5,next_lookup_at=CASE WHEN $3='recovery_pending' THEN $4::timestamptz ELSE next_lookup_at END WHERE tenant_id=$1 AND id=$2`, id.TenantID, u.AttemptID, phase, at, reasonCode, m.UploadExpiresAt)
		if e != nil {
			return false, e
		}
		_, e = tx.Exec(ctx, `INSERT INTO audit_events(tenant_id,actor_user_id,acting_membership_id,action,resource_type,resource_id,outcome,reason,occurred_at) VALUES($1,$2,$3,'file_upload_failed','file',$4,'deny',$5,$6)`, id.TenantID, id.UserID, id.ActingMembershipID, m.ID, reasonCode, at)
		if e != nil {
			return false, errors.Join(ErrAuditUnavailable, e)
		}
		return true, nil
	})
	return e
}
