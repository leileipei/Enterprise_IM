BEGIN;

CREATE INDEX conversations_direct_low_inbox ON conversations
    (tenant_id, direct_user_low_id, direct_low_membership_id, updated_at DESC, id DESC)
    WHERE kind='direct' AND status='active';

CREATE INDEX conversations_direct_high_inbox ON conversations
    (tenant_id, direct_user_high_id, direct_high_membership_id, updated_at DESC, id DESC)
    WHERE kind='direct' AND status='active';

COMMIT;
