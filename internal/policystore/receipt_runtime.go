package policystore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrReceiptRuntimeUnavailable = errors.New("receipt runtime schema unavailable")

// Only objects in the selected application schema can satisfy this gate.
// Presence alone is insufficient: types, NOT NULL, keys and enabled protection
// triggers are checked before enabling the public receipt/device routes.
func (s Service) CheckReceiptRuntime(ctx context.Context) error {
	if s.DB == nil {
		return ErrReceiptRuntimeUnavailable
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return ErrReceiptRuntimeUnavailable
	}
	defer tx.Rollback(ctx)
	columns := map[string]string{
		"tenant_client_policies":         "tenant_id:uuid version:bigint device_lease_days:integer max_active_devices:integer offline_cache_seconds:integer group_receipt_visibility:text updated_at:timestamptz",
		"user_devices":                   "id:uuid tenant_id:uuid user_id:uuid registration_id:uuid platform:text name:text credential_digest:bytea created_at:timestamptz expires_at:timestamptz revoked_at:timestamptz? last_seen_at:timestamptz?",
		"device_registration_windows":    "tenant_id:uuid user_id:uuid window_start:timestamptz created_count:integer",
		"device_delivery_cursors":        "tenant_id:uuid user_id:uuid device_id:uuid conversation_id:uuid acting_membership_id:uuid scanned_seq:bigint delivered_seq:bigint updated_at:timestamptz",
		"conversation_user_read_cursors": "tenant_id:uuid user_id:uuid conversation_id:uuid read_seq:bigint updated_at:timestamptz",
		"device_sync_proofs":             "id:uuid tenant_id:uuid user_id:uuid device_id:uuid acting_membership_id:uuid conversation_id:uuid token_digest:bytea after_seq:bigint next_after_seq:bigint issued_at:timestamptz expires_at:timestamptz confirmed_at:timestamptz?",
		"device_sync_proof_items":        "tenant_id:uuid proof_id:uuid conversation_id:uuid user_id:uuid device_id:uuid acting_membership_id:uuid seq:bigint message_id:uuid recipient_interval_id:uuid?",
		"message_device_receipts":        "tenant_id:uuid conversation_id:uuid seq:bigint user_id:uuid device_id:uuid delivered_at:timestamptz read_at:timestamptz?",
		"message_user_receipts":          "tenant_id:uuid conversation_id:uuid seq:bigint user_id:uuid delivered_at:timestamptz read_at:timestamptz?",
		"conversation_receipt_versions":  "tenant_id:uuid conversation_id:uuid revision:bigint",
		"receipt_outbox_events":          "id:uuid tenant_id:uuid conversation_id:uuid revision:bigint actor_user_id:uuid event_type:text status:text attempt_count:integer next_retry_at:timestamptz created_at:timestamptz published_at:timestamptz?",
		"receipt_outbox_targets":         "tenant_id:uuid event_id:uuid target_user_id:uuid",
		"receipt_write_windows":          "tenant_id:uuid user_id:uuid window_start:timestamptz write_count:integer",
	}
	// Counts are a completeness check, not a claim of DDL attestation. Existing
	// migration privileges remain responsible for installing trusted definitions.
	keys := map[string][3]int{
		"tenant_client_policies": {1, 1, 5}, "user_devices": {1, 1, 6}, "device_registration_windows": {1, 1, 1},
		"device_delivery_cursors": {1, 3, 2}, "conversation_user_read_cursors": {1, 2, 1}, "device_sync_proofs": {1, 3, 5},
		"device_sync_proof_items": {1, 3, 1}, "message_device_receipts": {1, 2, 1}, "message_user_receipts": {1, 2, 1},
		"conversation_receipt_versions": {1, 1, 1}, "receipt_outbox_events": {1, 2, 6}, "receipt_outbox_targets": {1, 2, 0}, "receipt_write_windows": {1, 1, 1},
	}
	for table, list := range columns {
		for _, field := range strings.Fields(list) {
			name, kind, _ := strings.Cut(field, ":")
			nullable := strings.HasSuffix(kind, "?")
			kind = strings.TrimSuffix(kind, "?")
			if kind == "integer" {
				kind = "int4"
			}
			if kind == "bigint" {
				kind = "int8"
			}
			var valid bool
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid JOIN pg_type t ON t.oid=a.atttypid
    WHERE n.nspname=current_schema() AND c.relname=$1 AND c.relkind='r' AND a.attname=$2 AND NOT a.attisdropped AND t.typname=$3 AND a.attnotnull=$4)`, table, name, kind, !nullable).Scan(&valid)
			if e != nil || !valid {
				return fmt.Errorf("%w: column %s.%s", ErrReceiptRuntimeUnavailable, table, name)
			}
		}
		var primary, foreign, checks, unique int
		e = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE k.contype='p'),count(*) FILTER(WHERE k.contype='f'),count(*) FILTER(WHERE k.contype='c'),count(*) FILTER(WHERE k.contype='u') FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname=$1 AND k.convalidated`, table).Scan(&primary, &foreign, &checks, &unique)
		minimum := keys[table]
		uniqueMinimum := 0
		switch table {
		case "user_devices", "device_sync_proofs", "receipt_outbox_events":
			uniqueMinimum = 2
		}
		if e != nil || primary != minimum[0] || foreign < minimum[1] || checks < minimum[2] || unique < uniqueMinimum {
			return fmt.Errorf("%w: constraints %s (%d,%d,%d)", ErrReceiptRuntimeUnavailable, table, primary, foreign, checks)
		}
	}
	triggers := map[string]string{"user_devices": "user_devices_identity_immutable", "device_delivery_cursors": "device_delivery_progress_protected", "conversation_user_read_cursors": "conversation_user_read_progress_protected", "conversation_receipt_versions": "conversation_receipt_revision_protected", "device_sync_proofs": "device_sync_proof_immutable", "device_sync_proof_items": "device_sync_proof_item_protected", "message_device_receipts": "message_device_receipts_protected", "message_user_receipts": "message_user_receipts_protected", "receipt_outbox_events": "receipt_outbox_event_protected", "receipt_outbox_targets": "receipt_outbox_target_protected"}
	for table, name := range triggers {
		var valid bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname=$1 AND t.tgname=$2 AND t.tgenabled IN ('O','A') AND NOT t.tgisinternal)`, table, name).Scan(&valid)
		if e != nil || !valid {
			return ErrReceiptRuntimeUnavailable
		}
	}
	var valid bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() AND c.relname='audit_events' AND k.conname='audit_events_device_owner_context' AND k.convalidated)`).Scan(&valid)
	if e != nil || !valid {
		return ErrReceiptRuntimeUnavailable
	}
	return tx.Commit(ctx)
}
