BEGIN;

ALTER TABLE conversation_membership_intervals ADD CONSTRAINT receipt_interval_owner_unique
    UNIQUE (tenant_id,conversation_id,user_id,id);

CREATE TABLE device_delivery_cursors (
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    device_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    scanned_seq bigint NOT NULL DEFAULT 0 CHECK (scanned_seq >= 0),
    delivered_seq bigint NOT NULL DEFAULT 0 CHECK (delivered_seq BETWEEN 0 AND scanned_seq),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id,user_id,device_id,conversation_id,acting_membership_id),
    FOREIGN KEY (tenant_id,user_id,device_id) REFERENCES user_devices(tenant_id,user_id,id),
    FOREIGN KEY (tenant_id,user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
    FOREIGN KEY (tenant_id,conversation_id) REFERENCES conversations(tenant_id,id)
);
CREATE TABLE conversation_user_read_cursors (
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    read_seq bigint NOT NULL DEFAULT 0 CHECK (read_seq >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant_id,user_id,conversation_id),
    FOREIGN KEY (tenant_id,user_id) REFERENCES users(tenant_id,id),
    FOREIGN KEY (tenant_id,conversation_id) REFERENCES conversations(tenant_id,id)
);
CREATE TABLE device_sync_proofs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    user_id uuid NOT NULL,
    device_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    after_seq bigint NOT NULL CHECK (after_seq >= 0),
    next_after_seq bigint NOT NULL CHECK (next_after_seq >= after_seq),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    confirmed_at timestamptz,
    CHECK (expires_at = issued_at + interval '300 seconds'),
    CHECK (confirmed_at IS NULL OR (confirmed_at >= issued_at AND confirmed_at < expires_at)),
    UNIQUE (tenant_id,id,conversation_id,user_id,device_id,acting_membership_id),
    FOREIGN KEY (tenant_id,user_id,device_id) REFERENCES user_devices(tenant_id,user_id,id),
    FOREIGN KEY (tenant_id,user_id,acting_membership_id) REFERENCES user_organizations(tenant_id,user_id,id),
    FOREIGN KEY (tenant_id,conversation_id) REFERENCES conversations(tenant_id,id)
);
CREATE INDEX device_sync_proofs_expiry ON device_sync_proofs(expires_at,id);
CREATE INDEX device_sync_proofs_device_quota ON device_sync_proofs(tenant_id,user_id,device_id,expires_at)
    WHERE confirmed_at IS NULL;

CREATE TABLE device_sync_proof_items (
    tenant_id uuid NOT NULL,
    proof_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    user_id uuid NOT NULL,
    device_id uuid NOT NULL,
    acting_membership_id uuid NOT NULL,
    seq bigint NOT NULL CHECK (seq > 0),
    message_id uuid NOT NULL,
    recipient_interval_id uuid,
    PRIMARY KEY (tenant_id,proof_id,seq),
    FOREIGN KEY (tenant_id,proof_id,conversation_id,user_id,device_id,acting_membership_id)
        REFERENCES device_sync_proofs(tenant_id,id,conversation_id,user_id,device_id,acting_membership_id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,conversation_id,seq,message_id) REFERENCES messages(tenant_id,conversation_id,seq,id),
    FOREIGN KEY (tenant_id,conversation_id,user_id,recipient_interval_id)
        REFERENCES conversation_membership_intervals(tenant_id,conversation_id,user_id,id)
);

CREATE TABLE message_device_receipts (
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    seq bigint NOT NULL,
    user_id uuid NOT NULL,
    device_id uuid NOT NULL,
    delivered_at timestamptz NOT NULL,
    read_at timestamptz,
    CHECK (read_at IS NULL OR read_at >= delivered_at),
    PRIMARY KEY (tenant_id,conversation_id,seq,user_id,device_id),
    FOREIGN KEY (tenant_id,conversation_id,seq) REFERENCES messages(tenant_id,conversation_id,seq),
    FOREIGN KEY (tenant_id,user_id,device_id) REFERENCES user_devices(tenant_id,user_id,id)
);
CREATE TABLE message_user_receipts (
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    seq bigint NOT NULL,
    user_id uuid NOT NULL,
    delivered_at timestamptz NOT NULL,
    read_at timestamptz,
    CHECK (read_at IS NULL OR read_at >= delivered_at),
    PRIMARY KEY (tenant_id,conversation_id,seq,user_id),
    FOREIGN KEY (tenant_id,conversation_id,seq) REFERENCES messages(tenant_id,conversation_id,seq),
    FOREIGN KEY (tenant_id,user_id) REFERENCES users(tenant_id,id)
);
CREATE INDEX message_user_receipts_read_lookup ON message_user_receipts(tenant_id,user_id,conversation_id,seq)
    WHERE read_at IS NOT NULL;
CREATE INDEX message_device_receipts_own_delivered ON message_device_receipts(tenant_id,user_id,device_id,conversation_id,seq);

CREATE TABLE conversation_receipt_versions (
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    PRIMARY KEY (tenant_id,conversation_id),
    FOREIGN KEY (tenant_id,conversation_id) REFERENCES conversations(tenant_id,id)
);
CREATE TABLE receipt_outbox_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    actor_user_id uuid NOT NULL,
    event_type text NOT NULL DEFAULT 'receipt_changed' CHECK (event_type = 'receipt_changed'),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','published')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_at timestamptz,
    CHECK ((status='pending' AND published_at IS NULL) OR (status='published' AND published_at IS NOT NULL)),
    CHECK (published_at IS NULL OR published_at >= created_at),
    UNIQUE (tenant_id,conversation_id,revision),
    UNIQUE (tenant_id,id),
    FOREIGN KEY (tenant_id,conversation_id) REFERENCES conversations(tenant_id,id),
    FOREIGN KEY (tenant_id,actor_user_id) REFERENCES users(tenant_id,id)
);
CREATE INDEX receipt_outbox_pending_retry ON receipt_outbox_events(next_retry_at,created_at,id) WHERE status='pending';
CREATE TABLE receipt_outbox_targets (
    tenant_id uuid NOT NULL,
    event_id uuid NOT NULL,
    target_user_id uuid NOT NULL,
    PRIMARY KEY (tenant_id,event_id,target_user_id),
    FOREIGN KEY (tenant_id,event_id) REFERENCES receipt_outbox_events(tenant_id,id),
    FOREIGN KEY (tenant_id,target_user_id) REFERENCES users(tenant_id,id)
);

CREATE FUNCTION protect_receipt_fact() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'receipt facts cannot be deleted' USING ERRCODE='23514';
    END IF;
    IF (to_jsonb(NEW)-'read_at') IS DISTINCT FROM (to_jsonb(OLD)-'read_at')
       OR (OLD.read_at IS NOT NULL AND NEW.read_at IS DISTINCT FROM OLD.read_at) THEN
        RAISE EXCEPTION 'receipt identity and established facts are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER message_device_receipts_protected BEFORE UPDATE OR DELETE ON message_device_receipts
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_fact();
CREATE TRIGGER message_user_receipts_protected BEFORE UPDATE OR DELETE ON message_user_receipts
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_fact();

CREATE FUNCTION protect_receipt_progress() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_row jsonb; new_row jsonb; field text;
BEGIN
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'receipt progress cannot be reset' USING ERRCODE='23514'; END IF;
    old_row:=to_jsonb(OLD); new_row:=to_jsonb(NEW);
    IF (old_row-ARRAY['scanned_seq','delivered_seq','read_seq','revision','updated_at'])
       IS DISTINCT FROM (new_row-ARRAY['scanned_seq','delivered_seq','read_seq','revision','updated_at']) THEN
        RAISE EXCEPTION 'receipt progress scope is immutable' USING ERRCODE='23514';
    END IF;
    FOREACH field IN ARRAY ARRAY['scanned_seq','delivered_seq','read_seq','revision'] LOOP
        IF (new_row->>field)::bigint < (old_row->>field)::bigint THEN
            RAISE EXCEPTION 'receipt progress cannot decrease' USING ERRCODE='23514';
        END IF;
    END LOOP;
    RETURN NEW;
END $$;
CREATE TRIGGER device_delivery_progress_protected BEFORE UPDATE OR DELETE ON device_delivery_cursors
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_progress();
CREATE TRIGGER conversation_user_read_progress_protected BEFORE UPDATE OR DELETE ON conversation_user_read_cursors
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_progress();
CREATE TRIGGER conversation_receipt_revision_protected BEFORE UPDATE OR DELETE ON conversation_receipt_versions
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_progress();

CREATE FUNCTION protect_sync_proof() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW)-'confirmed_at') IS DISTINCT FROM (to_jsonb(OLD)-'confirmed_at')
       OR (OLD.confirmed_at IS NOT NULL AND NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at) THEN
        RAISE EXCEPTION 'sync proof scope is immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER device_sync_proof_immutable BEFORE UPDATE ON device_sync_proofs
    FOR EACH ROW EXECUTE FUNCTION protect_sync_proof();

CREATE FUNCTION protect_sync_proof_item() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE page_after bigint; page_next bigint; kind text; sender uuid; recipient uuid; interval_start bigint; interval_end bigint;
BEGIN
    IF TG_OP='UPDATE' THEN RAISE EXCEPTION 'sync proof items are immutable' USING ERRCODE='23514'; END IF;
    IF TG_OP='DELETE' THEN
        IF EXISTS(SELECT 1 FROM device_sync_proofs WHERE tenant_id=OLD.tenant_id AND id=OLD.proof_id) THEN
            RAISE EXCEPTION 'sync proof items must be removed with their proof' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    SELECT after_seq,next_after_seq INTO page_after,page_next FROM device_sync_proofs
        WHERE tenant_id=NEW.tenant_id AND id=NEW.proof_id;
    SELECT c.kind,m.sender_user_id,m.recipient_user_id INTO kind,sender,recipient FROM messages m
        JOIN conversations c ON c.tenant_id=m.tenant_id AND c.id=m.conversation_id
        WHERE m.tenant_id=NEW.tenant_id AND m.conversation_id=NEW.conversation_id AND m.seq=NEW.seq AND m.id=NEW.message_id;
    IF NEW.seq <= page_after OR NEW.seq > page_next OR sender=NEW.user_id
       OR (kind='direct' AND (NEW.recipient_interval_id IS NOT NULL OR recipient IS DISTINCT FROM NEW.user_id))
       OR (kind='group' AND NEW.recipient_interval_id IS NULL) THEN
        RAISE EXCEPTION 'sync proof item is not a page recipient' USING ERRCODE='23514';
    END IF;
    IF kind='group' THEN
        SELECT join_seq,leave_seq INTO interval_start,interval_end FROM conversation_membership_intervals
            WHERE tenant_id=NEW.tenant_id AND conversation_id=NEW.conversation_id AND user_id=NEW.user_id AND id=NEW.recipient_interval_id;
        IF NEW.seq < interval_start OR (interval_end IS NOT NULL AND NEW.seq > interval_end) THEN
            RAISE EXCEPTION 'sync proof item is outside recipient interval' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER device_sync_proof_item_protected BEFORE INSERT OR UPDATE OR DELETE ON device_sync_proof_items
    FOR EACH ROW EXECUTE FUNCTION protect_sync_proof_item();

CREATE FUNCTION protect_receipt_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='DELETE' THEN RAISE EXCEPTION 'receipt event evidence cannot be deleted' USING ERRCODE='23514'; END IF;
    IF TG_TABLE_NAME='receipt_outbox_targets' THEN
        RAISE EXCEPTION 'receipt targets are immutable' USING ERRCODE='23514';
    END IF;
    IF (to_jsonb(OLD)-ARRAY['status','attempt_count','next_retry_at','published_at'])
       IS DISTINCT FROM (to_jsonb(NEW)-ARRAY['status','attempt_count','next_retry_at','published_at'])
       OR NEW.attempt_count < OLD.attempt_count
       OR (OLD.status='published' AND (NEW.status IS DISTINCT FROM OLD.status OR NEW.published_at IS DISTINCT FROM OLD.published_at)) THEN
        RAISE EXCEPTION 'receipt event scope and publication cannot regress' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER receipt_outbox_event_protected BEFORE UPDATE OR DELETE ON receipt_outbox_events
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_outbox();
CREATE TRIGGER receipt_outbox_target_protected BEFORE UPDATE OR DELETE ON receipt_outbox_targets
    FOR EACH ROW EXECUTE FUNCTION protect_receipt_outbox();

COMMIT;
