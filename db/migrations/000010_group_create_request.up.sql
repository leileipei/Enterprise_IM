BEGIN;

ALTER TABLE conversations
    ADD COLUMN group_create_request_id uuid,
    ADD COLUMN group_create_request_digest bytea,
    ADD CONSTRAINT conversations_group_create_request_shape_check CHECK (
        (group_create_request_id IS NULL AND group_create_request_digest IS NULL)
        OR (kind = 'group' AND group_create_request_id IS NOT NULL
            AND group_create_request_digest IS NOT NULL
            AND octet_length(group_create_request_digest) = 32)
    );

CREATE UNIQUE INDEX conversations_group_create_request_unique
ON conversations (tenant_id, created_by_user_id, group_create_request_id)
WHERE kind = 'group' AND group_create_request_id IS NOT NULL;

CREATE FUNCTION protect_group_create_request() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.group_create_request_id IS DISTINCT FROM NEW.group_create_request_id
       OR OLD.group_create_request_digest IS DISTINCT FROM NEW.group_create_request_digest THEN
        RAISE EXCEPTION 'group creation request identity cannot change' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversations_group_create_request_stable
BEFORE UPDATE OF group_create_request_id, group_create_request_digest ON conversations
FOR EACH ROW EXECUTE FUNCTION protect_group_create_request();

COMMIT;
