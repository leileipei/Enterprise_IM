BEGIN;

ALTER TABLE tenants
    ADD COLUMN message_body_retention_days integer NOT NULL DEFAULT 365
        CHECK (message_body_retention_days BETWEEN 1 AND 3650),
    ADD COLUMN retention_version bigint NOT NULL DEFAULT 0 CHECK (retention_version >= 0),
    ADD COLUMN retention_approval_reference text,
    ADD COLUMN retention_approved_by_user_id uuid,
    ADD COLUMN retention_approved_at timestamptz,
    ADD CONSTRAINT tenants_retention_approval_complete CHECK (
        (retention_version = 0 AND message_body_retention_days = 365
            AND retention_approval_reference IS NULL
            AND retention_approved_by_user_id IS NULL AND retention_approved_at IS NULL)
        OR
        (retention_version > 0 AND retention_approval_reference IS NOT NULL
            AND length(btrim(retention_approval_reference)) BETWEEN 1 AND 128
            AND retention_approved_by_user_id IS NOT NULL AND retention_approved_at IS NOT NULL)
    ),
    ADD CONSTRAINT tenants_retention_approver_fk
        FOREIGN KEY (id, retention_approved_by_user_id)
        REFERENCES users (tenant_id, id);

CREATE TABLE tenant_retention_policy_history (
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    version bigint NOT NULL CHECK (version > 0),
    message_body_retention_days integer NOT NULL
        CHECK (message_body_retention_days BETWEEN 1 AND 3650),
    approval_reference text NOT NULL
        CHECK (length(btrim(approval_reference)) BETWEEN 1 AND 128),
    approved_by_user_id uuid NOT NULL,
    approved_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, version),
    FOREIGN KEY (tenant_id, approved_by_user_id)
        REFERENCES users (tenant_id, id)
);

CREATE FUNCTION reject_retention_history_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'retention approval history is immutable' USING ERRCODE = '23514';
END $$;

CREATE TRIGGER tenant_retention_history_immutable
BEFORE UPDATE OR DELETE ON tenant_retention_policy_history
FOR EACH ROW EXECUTE FUNCTION reject_retention_history_change();

COMMIT;
