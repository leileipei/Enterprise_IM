BEGIN;

ALTER TABLE messages
    DROP CONSTRAINT messages_recipient_organization_fk,
    DROP CONSTRAINT messages_sender_organization_fk,
    DROP CONSTRAINT messages_recipient_membership_fk,
    DROP CONSTRAINT messages_distinct_users,
    DROP CONSTRAINT messages_recipient_complete,
    DROP COLUMN recipient_membership_id,
    DROP COLUMN recipient_user_id,
    DROP COLUMN sender_organization_id,
    DROP COLUMN recipient_organization_id;

COMMIT;
