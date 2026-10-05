BEGIN;
LOCK TABLE messages, message_attachments IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM messages WHERE message_type='file') OR EXISTS(SELECT 1 FROM message_attachments) THEN
  RAISE EXCEPTION 'cannot roll back file messages with existing attachment evidence' USING ERRCODE='23514';
 END IF;
END $$;
DROP TRIGGER messages_attachment_pair ON messages;
DROP TRIGGER idempotency_attachment_pair ON message_idempotency;
DROP TABLE message_attachments;
DROP FUNCTION check_message_attachment_state();
DROP FUNCTION guard_message_attachment_binding();
DROP TRIGGER messages_type_immutable ON messages;
DROP FUNCTION guard_message_type();
ALTER TABLE messages DROP CONSTRAINT messages_attachment_origin,
 DROP CONSTRAINT messages_typed_body, DROP COLUMN message_type,
 ADD CONSTRAINT messages_text_body_check CHECK(octet_length(text_body) BETWEEN 1 AND 16384 AND length(btrim(text_body))>0);
ALTER TABLE file_objects DROP CONSTRAINT file_objects_attachment_origin;
COMMIT;
