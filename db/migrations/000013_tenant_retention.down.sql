BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM tenant_retention_policy_history)
        OR EXISTS (SELECT 1 FROM tenants WHERE retention_version <> 0
        OR message_body_retention_days <> 365
        OR retention_approval_reference IS NOT NULL
        OR retention_approved_by_user_id IS NOT NULL
        OR retention_approved_at IS NOT NULL) THEN
        RAISE EXCEPTION 'approved tenant retention policies cannot be discarded'
            USING ERRCODE = '23514';
    END IF;
END $$;

DROP TABLE tenant_retention_policy_history;
DROP FUNCTION reject_retention_history_change();

ALTER TABLE tenants
    DROP CONSTRAINT tenants_retention_approver_fk,
    DROP CONSTRAINT tenants_retention_approval_complete,
    DROP COLUMN retention_approved_at,
    DROP COLUMN retention_approved_by_user_id,
    DROP COLUMN retention_approval_reference,
    DROP COLUMN retention_version,
    DROP COLUMN message_body_retention_days;

COMMIT;
