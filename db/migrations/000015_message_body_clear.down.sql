BEGIN;

LOCK TABLE messages, message_body_clear_batches IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM messages WHERE body_cleared_at IS NOT NULL)
        OR EXISTS (SELECT 1 FROM message_body_clear_batches) THEN
        RAISE EXCEPTION 'cannot roll back message body clearing with existing history'
            USING ERRCODE = '23514';
    END IF;
END $$;

DROP TABLE message_body_clear_batches;
DROP FUNCTION reject_message_body_clear_batch_change();
DROP INDEX messages_body_clear_candidates;
DROP TRIGGER messages_body_clear_one_way ON messages;
DROP FUNCTION guard_message_body_clear();
ALTER TABLE messages DROP CONSTRAINT messages_body_clear_state,
    DROP COLUMN body_cleared_at, ALTER COLUMN text_body SET NOT NULL;

COMMIT;
