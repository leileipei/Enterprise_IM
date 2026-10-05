BEGIN;

LOCK TABLE conversations IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM conversations WHERE kind = 'group') THEN
        RAISE EXCEPTION 'cannot roll back group membership schema while group conversations exist';
    END IF;
END;
$$;

DROP TABLE conversation_membership_intervals;
DROP FUNCTION protect_group_membership_interval();
DROP TRIGGER conversations_stable_kind_and_group_origin ON conversations;
DROP FUNCTION protect_conversation_kind_and_group_origin();

ALTER TABLE conversations
    DROP CONSTRAINT conversations_tenant_id_kind_unique,
    DROP CONSTRAINT conversations_group_creator_organization_fk,
    DROP CONSTRAINT conversations_group_creator_user_membership_fk,
    DROP CONSTRAINT conversations_group_creator_membership_organization_fk,
    DROP CONSTRAINT conversations_shape_check,
    DROP CONSTRAINT conversations_kind_check,
    DROP COLUMN group_name,
    DROP COLUMN group_creator_membership_id,
    DROP COLUMN group_creator_organization_id,
    DROP COLUMN group_creator_legal_entity_id,
    ADD CONSTRAINT conversations_kind_check CHECK (kind = 'direct'),
    ADD CONSTRAINT conversations_check CHECK (
        direct_user_low_id IS NOT NULL AND direct_user_high_id IS NOT NULL
        AND direct_low_membership_id IS NOT NULL AND direct_high_membership_id IS NOT NULL
        AND direct_user_low_id < direct_user_high_id
        AND created_by_user_id IN (direct_user_low_id, direct_user_high_id)
    );

ALTER TABLE organizations DROP CONSTRAINT organizations_tenant_id_legal_unique;

COMMIT;
