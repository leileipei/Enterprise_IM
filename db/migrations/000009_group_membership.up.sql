BEGIN;

ALTER TABLE organizations
    ADD CONSTRAINT organizations_tenant_id_legal_unique
    UNIQUE (tenant_id, id, legal_entity_id);

ALTER TABLE conversations
    DROP CONSTRAINT conversations_kind_check,
    DROP CONSTRAINT conversations_check,
    ADD COLUMN group_name text,
    ADD COLUMN group_creator_membership_id uuid,
    ADD COLUMN group_creator_organization_id uuid,
    ADD COLUMN group_creator_legal_entity_id uuid,
    ADD CONSTRAINT conversations_kind_check CHECK (kind IN ('direct', 'group')),
    ADD CONSTRAINT conversations_shape_check CHECK (
        (kind = 'direct'
         AND direct_user_low_id IS NOT NULL AND direct_user_high_id IS NOT NULL
         AND direct_low_membership_id IS NOT NULL AND direct_high_membership_id IS NOT NULL
         AND direct_user_low_id < direct_user_high_id
         AND created_by_user_id IN (direct_user_low_id, direct_user_high_id)
         AND group_name IS NULL AND group_creator_membership_id IS NULL
         AND group_creator_organization_id IS NULL
         AND group_creator_legal_entity_id IS NULL)
        OR
        (kind = 'group'
         AND direct_user_low_id IS NULL AND direct_user_high_id IS NULL
         AND direct_low_membership_id IS NULL AND direct_high_membership_id IS NULL
         AND group_name IS NOT NULL AND length(btrim(group_name)) >= 1
         AND length(group_name) <= 120
         AND group_creator_membership_id IS NOT NULL
         AND group_creator_organization_id IS NOT NULL
         AND group_creator_legal_entity_id IS NOT NULL)
    ),
    ADD CONSTRAINT conversations_group_creator_organization_fk
    FOREIGN KEY (tenant_id, group_creator_organization_id, group_creator_legal_entity_id)
    REFERENCES organizations (tenant_id, id, legal_entity_id),
    ADD CONSTRAINT conversations_group_creator_user_membership_fk
    FOREIGN KEY (tenant_id, created_by_user_id, group_creator_membership_id)
    REFERENCES user_organizations (tenant_id, user_id, id),
    ADD CONSTRAINT conversations_group_creator_membership_organization_fk
    FOREIGN KEY (tenant_id, group_creator_membership_id, group_creator_organization_id)
    REFERENCES user_organizations (tenant_id, id, organization_id),
    ADD CONSTRAINT conversations_tenant_id_kind_unique UNIQUE (tenant_id, id, kind);

CREATE FUNCTION protect_conversation_kind_and_group_origin() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.kind IS DISTINCT FROM NEW.kind THEN
        RAISE EXCEPTION 'conversation kind cannot change' USING ERRCODE = '23514';
    END IF;
    IF OLD.kind = 'group' AND
       (OLD.created_by_user_id, OLD.group_creator_membership_id,
        OLD.group_creator_organization_id, OLD.group_creator_legal_entity_id)
       IS DISTINCT FROM
       (NEW.created_by_user_id, NEW.group_creator_membership_id,
        NEW.group_creator_organization_id, NEW.group_creator_legal_entity_id) THEN
        RAISE EXCEPTION 'group creation origin cannot change' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversations_stable_kind_and_group_origin
BEFORE UPDATE ON conversations
FOR EACH ROW EXECUTE FUNCTION protect_conversation_kind_and_group_origin();

CREATE TABLE conversation_membership_intervals (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    conversation_kind text NOT NULL DEFAULT 'group' CHECK (conversation_kind = 'group'),
    user_id uuid NOT NULL,
    source_membership_id uuid NOT NULL,
    source_organization_id uuid NOT NULL,
    source_legal_entity_id uuid NOT NULL,
    role text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'left', 'removed')),
    join_seq bigint NOT NULL CHECK (join_seq >= 1),
    leave_seq bigint CHECK (leave_seq >= join_seq - 1 AND leave_seq < 9223372036854775807),
    joined_policy_version bigint NOT NULL CHECK (joined_policy_version >= 0),
    joined_at timestamptz NOT NULL DEFAULT now(),
    left_at timestamptz,
    CHECK ((status = 'active' AND leave_seq IS NULL AND left_at IS NULL)
        OR (status <> 'active' AND leave_seq IS NOT NULL AND left_at IS NOT NULL
            AND left_at >= joined_at)),
    FOREIGN KEY (tenant_id, conversation_id, conversation_kind)
        REFERENCES conversations (tenant_id, id, kind),
    FOREIGN KEY (tenant_id, user_id, source_membership_id)
        REFERENCES user_organizations (tenant_id, user_id, id),
    FOREIGN KEY (tenant_id, source_membership_id, source_organization_id)
        REFERENCES user_organizations (tenant_id, id, organization_id),
    FOREIGN KEY (tenant_id, source_organization_id, source_legal_entity_id)
        REFERENCES organizations (tenant_id, id, legal_entity_id),
    EXCLUDE USING gist (
        tenant_id WITH =,
        conversation_id WITH =,
        user_id WITH =,
        int8range(join_seq, leave_seq + 1, '[)') WITH &&
    )
);

CREATE FUNCTION protect_group_membership_interval() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'group membership history cannot be deleted' USING ERRCODE = '23514';
    END IF;
    IF OLD.status <> 'active' THEN
        RAISE EXCEPTION 'closed group membership interval cannot be changed' USING ERRCODE = '23514';
    END IF;
    IF (OLD.id, OLD.tenant_id, OLD.conversation_id, OLD.conversation_kind,
        OLD.user_id, OLD.source_membership_id, OLD.source_organization_id,
        OLD.source_legal_entity_id, OLD.join_seq, OLD.joined_policy_version, OLD.joined_at)
       IS DISTINCT FROM
       (NEW.id, NEW.tenant_id, NEW.conversation_id, NEW.conversation_kind,
        NEW.user_id, NEW.source_membership_id, NEW.source_organization_id,
        NEW.source_legal_entity_id, NEW.join_seq, NEW.joined_policy_version, NEW.joined_at) THEN
        RAISE EXCEPTION 'group membership origin cannot be changed' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER conversation_membership_history_protected
BEFORE UPDATE OR DELETE ON conversation_membership_intervals
FOR EACH ROW EXECUTE FUNCTION protect_group_membership_interval();

CREATE UNIQUE INDEX conversation_membership_active_user_unique
    ON conversation_membership_intervals (tenant_id, conversation_id, user_id)
    WHERE status = 'active';

CREATE UNIQUE INDEX conversation_membership_active_owner_unique
    ON conversation_membership_intervals (tenant_id, conversation_id)
    WHERE status = 'active' AND role = 'owner';

CREATE INDEX conversation_membership_conversation_lookup
    ON conversation_membership_intervals (tenant_id, conversation_id, status);

CREATE INDEX conversation_membership_user_lookup
    ON conversation_membership_intervals (tenant_id, user_id, conversation_id);

COMMIT;
