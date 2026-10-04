BEGIN;

ALTER TABLE messages
 ADD COLUMN message_type text NOT NULL DEFAULT 'text' CHECK(message_type IN ('text','file')),
 DROP CONSTRAINT messages_text_body_check,
 ADD CONSTRAINT messages_typed_body CHECK(text_body IS NULL OR
   (message_type='text' AND octet_length(text_body) BETWEEN 1 AND 16384 AND length(btrim(text_body))>0) OR
   (message_type='file' AND octet_length(text_body)<=16384)),
 ADD CONSTRAINT messages_attachment_origin UNIQUE(tenant_id,conversation_id,sender_user_id,sender_membership_id,id);
ALTER TABLE file_objects ADD CONSTRAINT file_objects_attachment_origin
 UNIQUE(tenant_id,conversation_id,uploader_user_id,uploader_membership_id,id);

CREATE TABLE message_attachments (
 tenant_id uuid NOT NULL,
 conversation_id uuid NOT NULL,
 message_id uuid NOT NULL,
 sender_user_id uuid NOT NULL,
 sender_membership_id uuid NOT NULL,
 file_id uuid NOT NULL,
 sealed_sha256 bytea,
 fingerprint_retired_at timestamptz,
 PRIMARY KEY(tenant_id,message_id),
 UNIQUE(tenant_id,file_id),
 FOREIGN KEY(tenant_id,conversation_id,sender_user_id,sender_membership_id,message_id)
 REFERENCES messages(tenant_id,conversation_id,sender_user_id,sender_membership_id,id),
 FOREIGN KEY(tenant_id,conversation_id,sender_user_id,sender_membership_id,file_id)
 REFERENCES file_objects(tenant_id,conversation_id,uploader_user_id,uploader_membership_id,id),
 CHECK((sealed_sha256 IS NOT NULL AND octet_length(sealed_sha256)=32 AND fingerprint_retired_at IS NULL)
 OR (sealed_sha256 IS NULL AND fingerprint_retired_at IS NOT NULL AND isfinite(fingerprint_retired_at)))
);

CREATE FUNCTION guard_message_type() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.message_type IS DISTINCT FROM OLD.message_type THEN
  RAISE EXCEPTION 'message type is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER messages_type_immutable BEFORE UPDATE ON messages
 FOR EACH ROW EXECUTE FUNCTION guard_message_type();

CREATE FUNCTION guard_message_attachment_binding() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE f file_objects%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN
  RAISE EXCEPTION 'attachment evidence cannot be deleted' USING ERRCODE='23514';
 ELSIF TG_OP='UPDATE' THEN
  IF OLD.fingerprint_retired_at IS NOT NULL THEN
   IF to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
    RAISE EXCEPTION 'retired fingerprint cannot be rewritten' USING ERRCODE='23514';
   END IF;
  ELSIF to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
   IF NEW.sealed_sha256 IS NOT NULL OR NEW.fingerprint_retired_at IS NULL OR
      (to_jsonb(NEW)-ARRAY['sealed_sha256','fingerprint_retired_at']) IS DISTINCT FROM
      (to_jsonb(OLD)-ARRAY['sealed_sha256','fingerprint_retired_at']) THEN
    RAISE EXCEPTION 'attachment origin and fingerprint are immutable' USING ERRCODE='23514';
   END IF;
  END IF;
  RETURN NEW;
 END IF;
 IF NEW.fingerprint_retired_at IS NOT NULL THEN
  RAISE EXCEPTION 'new attachment cannot be retired' USING ERRCODE='23514';
 END IF;
 -- Match the send transaction's conversation -> file order even for direct SQL.
 PERFORM id FROM conversations WHERE tenant_id=NEW.tenant_id AND id=NEW.conversation_id FOR UPDATE;
 IF NOT FOUND THEN RAISE EXCEPTION 'attachment conversation unavailable' USING ERRCODE='23514'; END IF;
 SELECT * INTO f FROM file_objects WHERE tenant_id=NEW.tenant_id AND id=NEW.file_id FOR UPDATE;
 IF NOT FOUND OR ROW(f.conversation_id,f.uploader_user_id,f.uploader_membership_id)
   IS DISTINCT FROM ROW(NEW.conversation_id,NEW.sender_user_id,NEW.sender_membership_id)
   OR f.state<>'ready' OR f.scan_job_id IS NULL OR f.scan_engine IS NULL OR f.scan_definition_version IS NULL
   OR f.scanned_at IS NULL OR f.sha256 IS NULL OR f.scan_sha256 IS DISTINCT FROM f.sha256
   OR NEW.sealed_sha256 IS DISTINCT FROM f.sha256 THEN
  RAISE EXCEPTION 'attachment requires original ready file and sealed scan fingerprint' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER message_attachments_guard BEFORE INSERT OR UPDATE OR DELETE ON message_attachments
 FOR EACH ROW EXECUTE FUNCTION guard_message_attachment_binding();

CREATE FUNCTION check_message_attachment_state() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 tid uuid; mid uuid; kind text; m_stamp timestamptz; i_stamp timestamptz; a_stamp timestamptz;
 m_digest bytea; i_digest bytea; fp bytea; linked integer; key_found boolean;
BEGIN
 IF TG_OP='DELETE' THEN
  tid:=OLD.tenant_id;
  IF TG_TABLE_NAME='messages' THEN mid:=OLD.id; ELSE mid:=OLD.message_id; END IF;
 ELSE
  tid:=NEW.tenant_id;
  IF TG_TABLE_NAME='messages' THEN mid:=NEW.id; ELSE mid:=NEW.message_id; END IF;
 END IF;
 SELECT message_type,digest_retired_at,content_digest INTO kind,m_stamp,m_digest FROM messages WHERE tenant_id=tid AND id=mid;
 SELECT count(*) INTO linked FROM message_attachments WHERE tenant_id=tid AND message_id=mid;
 IF kind IS DISTINCT FROM 'file' THEN
  IF linked<>0 THEN RAISE EXCEPTION 'only file messages can have attachments' USING ERRCODE='23514'; END IF;
  RETURN NULL;
 END IF;
 IF linked<>1 THEN RAISE EXCEPTION 'file message requires exactly one attachment' USING ERRCODE='23514'; END IF;
 SELECT digest_retired_at,content_digest INTO i_stamp,i_digest FROM message_idempotency WHERE tenant_id=tid AND message_id=mid;
 key_found:=FOUND;
 SELECT fingerprint_retired_at,sealed_sha256 INTO a_stamp,fp FROM message_attachments WHERE tenant_id=tid AND message_id=mid;
 IF NOT key_found OR m_stamp IS DISTINCT FROM i_stamp OR m_stamp IS DISTINCT FROM a_stamp
  OR m_digest IS DISTINCT FROM i_digest OR
  (m_stamp IS NULL AND (m_digest IS NULL OR fp IS NULL)) OR
  (m_stamp IS NOT NULL AND (m_digest IS NOT NULL OR fp IS NOT NULL)) THEN
  RAISE EXCEPTION 'file message requires matching message, idempotency and fingerprint states' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER messages_attachment_pair AFTER INSERT OR UPDATE OR DELETE ON messages
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_message_attachment_state();
CREATE CONSTRAINT TRIGGER idempotency_attachment_pair AFTER INSERT OR UPDATE OR DELETE ON message_idempotency
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_message_attachment_state();
CREATE CONSTRAINT TRIGGER attachments_message_pair AFTER INSERT OR UPDATE OR DELETE ON message_attachments
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_message_attachment_state();

COMMIT;
