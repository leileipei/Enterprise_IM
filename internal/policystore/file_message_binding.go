package policystore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/files"
	"slices"
)

type preparedFileBinding struct {
	FileID    string
	SealedSHA [32]byte
}

func prepareFileBindingTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, conversationID, fileID string) (preparedFileBinding, error) {
	var zero preparedFileBinding
	// Check ownership before reporting any state or configuration to this sender.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM file_objects WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 AND uploader_user_id=$4 AND uploader_membership_id=$5)`, id.TenantID, conversationID, fileID, id.UserID, id.ActingMembershipID).Scan(&exists); err != nil {
		return zero, err
	}
	if !exists {
		return zero, ErrMessageNotAvailable
	}
	var config files.UploadPolicy
	err := tx.QueryRow(ctx, `SELECT enabled,max_size_bytes,allowed_media_types FROM tenant_file_upload_policy WHERE tenant_id=$1 FOR SHARE`, id.TenantID).Scan(&config.Enabled, &config.MaxSizeBytes, &config.AllowedMediaTypes)
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrFileNotBindable
	}
	if err != nil {
		return zero, err
	}
	m, err := scanFile(tx.QueryRow(ctx, "SELECT "+fileSelect+" FROM file_objects WHERE tenant_id=$1 AND conversation_id=$2 AND id=$3 AND uploader_user_id=$4 AND uploader_membership_id=$5 FOR UPDATE", id.TenantID, conversationID, fileID, id.UserID, id.ActingMembershipID))
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, ErrMessageNotAvailable
	}
	if err != nil {
		return zero, err
	}
	if m.State != files.StateReady || files.ValidateMetadata(m) != nil || !config.Enabled || m.ActualSizeBytes == nil || m.DeclaredSizeBytes > config.MaxSizeBytes || *m.ActualSizeBytes > config.MaxSizeBytes || !slices.Contains(config.AllowedMediaTypes, m.DeclaredMediaType) || !slices.Contains(config.AllowedMediaTypes, m.DetectedMediaType) {
		return zero, ErrFileNotBindable
	}
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM message_attachments WHERE tenant_id=$1 AND file_id=$2)`, id.TenantID, m.ID).Scan(&exists); err != nil {
		return zero, err
	}
	if exists {
		return zero, ErrFileNotBindable
	}
	binding := preparedFileBinding{FileID: m.ID}
	copy(binding.SealedSHA[:], m.SHA256)
	return binding, nil
}
func insertFileBindingTx(ctx context.Context, tx pgx.Tx, id access.TrustedIdentity, ack MessageACK, binding preparedFileBinding) error {
	_, err := tx.Exec(ctx, `INSERT INTO message_attachments(tenant_id,conversation_id,message_id,sender_user_id,sender_membership_id,file_id,sealed_sha256) VALUES($1,$2,$3,$4,$5,$6,$7)`, id.TenantID, ack.ConversationID, ack.MessageID, id.UserID, id.ActingMembershipID, binding.FileID, binding.SealedSHA[:])
	return err
}
func lockFileMessagePolicy(ctx context.Context, tx pgx.Tx, tenantID string) error {
	// Membership loading already holds the tenant SHARE lock, which also covers
	// the initial publication when policy_current has no row yet.
	_, err := tx.Exec(ctx, "SELECT tenant_id FROM policy_current WHERE tenant_id=$1 FOR SHARE", tenantID)
	return err
}
