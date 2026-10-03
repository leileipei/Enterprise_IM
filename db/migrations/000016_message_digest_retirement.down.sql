BEGIN;
LOCK TABLE messages, message_idempotency, message_digest_retirement_batches IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS(SELECT 1 FROM messages WHERE digest_retired_at IS NOT NULL)
       OR EXISTS(SELECT 1 FROM message_idempotency WHERE digest_retired_at IS NOT NULL)
       OR EXISTS(SELECT 1 FROM message_digest_retirement_batches) THEN
        RAISE EXCEPTION 'cannot roll back digest retirement with existing history' USING ERRCODE='23514';
    END IF;
END $$;
DROP TABLE message_digest_retirement_batches;
DROP INDEX message_digest_retirement_candidates;
DROP TRIGGER messages_digest_pair ON messages;
DROP TRIGGER idempotency_digest_pair ON message_idempotency;
DROP FUNCTION check_message_digest_pair();
DROP TRIGGER messages_digest_guard ON messages;
DROP TRIGGER idempotency_digest_guard ON message_idempotency;
DROP FUNCTION guard_message_digest_retirement();
ALTER TABLE messages DROP CONSTRAINT messages_digest_state,
    DROP COLUMN digest_retired_at, ALTER COLUMN content_digest SET NOT NULL;
ALTER TABLE message_idempotency DROP CONSTRAINT idempotency_digest_state,
    DROP COLUMN digest_retired_at, ALTER COLUMN content_digest SET NOT NULL;
COMMIT;
