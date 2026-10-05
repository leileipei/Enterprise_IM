BEGIN;

ALTER TABLE messages ALTER COLUMN content_digest DROP NOT NULL,
    ADD COLUMN digest_retired_at timestamptz,
    ADD CONSTRAINT messages_digest_state CHECK (
        (content_digest IS NOT NULL AND octet_length(content_digest)=32 AND digest_retired_at IS NULL)
        OR (content_digest IS NULL AND digest_retired_at IS NOT NULL
            AND text_body IS NULL AND body_cleared_at IS NOT NULL AND digest_retired_at>=body_cleared_at));
ALTER TABLE message_idempotency ALTER COLUMN content_digest DROP NOT NULL,
    ADD COLUMN digest_retired_at timestamptz,
    ADD CONSTRAINT idempotency_digest_state CHECK (
        (content_digest IS NOT NULL AND octet_length(content_digest)=32 AND digest_retired_at IS NULL)
        OR (content_digest IS NULL AND digest_retired_at IS NOT NULL AND digest_retired_at>=expires_at));

CREATE FUNCTION guard_message_digest_retirement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.digest_retired_at IS NOT NULL THEN
            RAISE EXCEPTION 'new rows cannot be retired' USING ERRCODE='23514';
        END IF;
        RETURN NEW;
    ELSIF TG_OP='DELETE' THEN
        IF OLD.digest_retired_at IS NOT NULL THEN
            RAISE EXCEPTION 'retired rows cannot be deleted' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.digest_retired_at IS NOT NULL AND to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
        RAISE EXCEPTION 'retired rows cannot be rewritten' USING ERRCODE='23514';
    END IF;
    IF NEW.digest_retired_at IS NOT NULL AND
       (to_jsonb(NEW)-ARRAY['content_digest','digest_retired_at']) IS DISTINCT FROM
       (to_jsonb(OLD)-ARRAY['content_digest','digest_retired_at']) THEN
        RAISE EXCEPTION 'retirement cannot rewrite original metadata' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER messages_digest_guard BEFORE INSERT OR UPDATE OR DELETE ON messages
    FOR EACH ROW EXECUTE FUNCTION guard_message_digest_retirement();
CREATE TRIGGER idempotency_digest_guard BEFORE INSERT OR UPDATE OR DELETE ON message_idempotency
    FOR EACH ROW EXECUTE FUNCTION guard_message_digest_retirement();

CREATE FUNCTION check_message_digest_pair() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    tid uuid;
    mid uuid;
    cid uuid;
    uid uuid;
    clientid uuid;
    m_stamp timestamptz;
    i_stamp timestamptz;
    m_exists boolean;
    i_exists boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        tid:=OLD.tenant_id;
        cid:=OLD.conversation_id; uid:=OLD.sender_user_id; clientid:=OLD.client_msg_id;
        IF TG_TABLE_NAME='messages' THEN mid:=OLD.id; ELSE mid:=OLD.message_id; END IF;
    ELSE
        tid:=NEW.tenant_id;
        cid:=NEW.conversation_id; uid:=NEW.sender_user_id; clientid:=NEW.client_msg_id;
        IF TG_TABLE_NAME='messages' THEN mid:=NEW.id; ELSE mid:=NEW.message_id; END IF;
    END IF;
    SELECT digest_retired_at INTO m_stamp FROM messages WHERE tenant_id=tid AND id=mid;
    m_exists:=FOUND;
    SELECT digest_retired_at INTO i_stamp FROM message_idempotency
        WHERE tenant_id=tid AND conversation_id=cid AND sender_user_id=uid AND client_msg_id=clientid AND message_id=mid;
    i_exists:=FOUND;
    IF m_stamp IS NOT NULL OR i_stamp IS NOT NULL THEN
        IF NOT m_exists OR NOT i_exists OR m_stamp IS DISTINCT FROM i_stamp THEN
            RAISE EXCEPTION 'digest retirement requires a matching pair' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER messages_digest_pair AFTER INSERT OR UPDATE OR DELETE ON messages
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_message_digest_pair();
CREATE CONSTRAINT TRIGGER idempotency_digest_pair AFTER INSERT OR UPDATE OR DELETE ON message_idempotency
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_message_digest_pair();

CREATE INDEX message_digest_retirement_candidates
    ON message_idempotency(tenant_id,conversation_id,expires_at,message_id) WHERE digest_retired_at IS NULL;
CREATE TABLE message_digest_retirement_batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    retired_at timestamptz NOT NULL,
    retired_count integer NOT NULL CHECK(retired_count BETWEEN 1 AND 1000),
    first_seq bigint NOT NULL CHECK(first_seq>0),
    last_seq bigint NOT NULL CHECK(last_seq>=first_seq),
    min_expires_at timestamptz NOT NULL,
    max_expires_at timestamptz NOT NULL,
    CHECK(min_expires_at<=max_expires_at AND max_expires_at<=retired_at),
    FOREIGN KEY(tenant_id,conversation_id) REFERENCES conversations(tenant_id,id)
);
CREATE INDEX message_digest_retirement_batches_conversation
    ON message_digest_retirement_batches(tenant_id,conversation_id,retired_at,id);
CREATE TRIGGER message_digest_retirement_batches_immutable
    BEFORE UPDATE OR DELETE ON message_digest_retirement_batches
    FOR EACH ROW EXECUTE FUNCTION reject_message_body_clear_batch_change();

COMMIT;
