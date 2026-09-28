BEGIN;

-- Old rows cannot be attributed to a historical recipient membership safely.
-- They remain NULL and the pull API returns a redacted sequence placeholder.
ALTER TABLE messages
    ADD COLUMN recipient_user_id uuid,
    ADD COLUMN recipient_membership_id uuid,
    ADD CONSTRAINT messages_recipient_complete CHECK
        ((recipient_user_id IS NULL AND recipient_membership_id IS NULL)
          OR (recipient_user_id IS NOT NULL AND recipient_membership_id IS NOT NULL)),
    ADD CONSTRAINT messages_distinct_users CHECK
        (recipient_user_id IS NULL OR recipient_user_id <> sender_user_id),
    ADD CONSTRAINT messages_recipient_membership_fk
        FOREIGN KEY (tenant_id, recipient_user_id, recipient_membership_id)
        REFERENCES user_organizations(tenant_id, user_id, id);

COMMIT;
