BEGIN;

LOCK TABLE group_invitation_requests IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM group_invitation_requests) THEN
        RAISE EXCEPTION 'cannot roll back recorded group invitations';
    END IF;
END;
$$;

DROP TABLE group_invitation_requests;

ALTER TABLE conversation_membership_intervals
    DROP CONSTRAINT conversation_intervals_invite_origin_unique;

ALTER TABLE user_organizations DROP CONSTRAINT user_organizations_tenant_id_unique;

COMMIT;
