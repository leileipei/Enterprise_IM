BEGIN;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM device_delivery_cursors)
       OR EXISTS (SELECT 1 FROM conversation_user_read_cursors)
       OR EXISTS (SELECT 1 FROM device_sync_proofs)
       OR EXISTS (SELECT 1 FROM device_sync_proof_items)
       OR EXISTS (SELECT 1 FROM message_device_receipts)
       OR EXISTS (SELECT 1 FROM message_user_receipts)
       OR EXISTS (SELECT 1 FROM conversation_receipt_versions)
       OR EXISTS (SELECT 1 FROM receipt_outbox_events)
       OR EXISTS (SELECT 1 FROM receipt_outbox_targets) THEN
        RAISE EXCEPTION 'receipt migration rollback requires empty business tables' USING ERRCODE='23514';
    END IF;
END $$;
DROP TABLE receipt_outbox_targets;
DROP TABLE receipt_outbox_events;
DROP TABLE conversation_receipt_versions;
DROP TABLE message_user_receipts;
DROP TABLE message_device_receipts;
DROP TABLE device_sync_proof_items;
DROP TABLE device_sync_proofs;
DROP TABLE conversation_user_read_cursors;
DROP TABLE device_delivery_cursors;
DROP FUNCTION protect_receipt_outbox();
DROP FUNCTION protect_sync_proof_item();
DROP FUNCTION protect_sync_proof();
DROP FUNCTION protect_receipt_progress();
DROP FUNCTION protect_receipt_fact();
ALTER TABLE conversation_membership_intervals DROP CONSTRAINT receipt_interval_owner_unique;
COMMIT;
