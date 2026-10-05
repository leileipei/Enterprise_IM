BEGIN;

ALTER TABLE messages
    ALTER COLUMN text_body DROP NOT NULL,
    ADD COLUMN body_cleared_at timestamptz,
    ADD CONSTRAINT messages_body_clear_state CHECK
        ((text_body IS NOT NULL AND body_cleared_at IS NULL)
         OR (text_body IS NULL AND body_cleared_at IS NOT NULL));

CREATE FUNCTION guard_message_body_clear() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.body_cleared_at IS NOT NULL AND
        (NEW.text_body IS DISTINCT FROM OLD.text_body
         OR NEW.body_cleared_at IS DISTINCT FROM OLD.body_cleared_at) THEN
        RAISE EXCEPTION 'cleared message body cannot be restored or rewritten'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER messages_body_clear_one_way
BEFORE UPDATE ON messages
FOR EACH ROW EXECUTE FUNCTION guard_message_body_clear();

CREATE INDEX messages_body_clear_candidates
ON messages (tenant_id, conversation_id, accepted_at, seq)
WHERE body_cleared_at IS NULL;

CREATE TABLE message_body_clear_batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    retention_days integer NOT NULL CHECK (retention_days BETWEEN 1 AND 3650),
    cutoff_at timestamptz NOT NULL,
    cleared_at timestamptz NOT NULL,
    first_seq bigint NOT NULL CHECK (first_seq > 0),
    last_seq bigint NOT NULL CHECK (last_seq >= first_seq),
    cleared_count integer NOT NULL CHECK (cleared_count BETWEEN 1 AND 1000),
    CHECK (cutoff_at = cleared_at - retention_days * INTERVAL '24 hours'),
    FOREIGN KEY (tenant_id, conversation_id) REFERENCES conversations (tenant_id, id)
);

CREATE INDEX message_body_clear_batches_conversation
ON message_body_clear_batches (tenant_id, conversation_id, cleared_at, id);

CREATE FUNCTION reject_message_body_clear_batch_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'message body clear batches are immutable' USING ERRCODE = '23514';
END $$;

CREATE TRIGGER message_body_clear_batches_immutable
BEFORE UPDATE OR DELETE ON message_body_clear_batches
FOR EACH ROW EXECUTE FUNCTION reject_message_body_clear_batch_change();

COMMIT;
