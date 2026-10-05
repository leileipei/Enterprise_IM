BEGIN;

LOCK TABLE conversations IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM conversations WHERE group_create_request_id IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back group creation requests while request records exist';
    END IF;
END;
$$;

DROP TRIGGER conversations_group_create_request_stable ON conversations;
DROP FUNCTION protect_group_create_request();
DROP INDEX conversations_group_create_request_unique;

ALTER TABLE conversations
    DROP CONSTRAINT conversations_group_create_request_shape_check,
    DROP COLUMN group_create_request_id,
    DROP COLUMN group_create_request_digest;

COMMIT;
