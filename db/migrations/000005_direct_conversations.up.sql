BEGIN;

ALTER TABLE user_organizations
    ADD CONSTRAINT user_organizations_tenant_user_membership_unique UNIQUE (tenant_id, user_id, id);

CREATE TABLE conversations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id),
    kind text NOT NULL DEFAULT 'direct' CHECK (kind = 'direct'),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'policy_blocked', 'ended')),
    direct_user_low_id uuid,
    direct_user_high_id uuid,
    direct_low_membership_id uuid,
    direct_high_membership_id uuid,
    last_seq bigint NOT NULL DEFAULT 0 CHECK (last_seq >= 0),
    last_policy_version bigint NOT NULL DEFAULT 0 CHECK (last_policy_version >= 0),
    created_by_user_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    CHECK (direct_user_low_id IS NOT NULL AND direct_user_high_id IS NOT NULL
        AND direct_low_membership_id IS NOT NULL AND direct_high_membership_id IS NOT NULL
        AND direct_user_low_id < direct_user_high_id
        AND created_by_user_id IN (direct_user_low_id, direct_user_high_id)),
    FOREIGN KEY (tenant_id, created_by_user_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, direct_user_low_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, direct_user_high_id) REFERENCES users(tenant_id, id),
    FOREIGN KEY (tenant_id, direct_user_low_id, direct_low_membership_id)
        REFERENCES user_organizations(tenant_id, user_id, id),
    FOREIGN KEY (tenant_id, direct_user_high_id, direct_high_membership_id)
        REFERENCES user_organizations(tenant_id, user_id, id)
);

CREATE UNIQUE INDEX conversations_direct_pair_unique
ON conversations (tenant_id, direct_user_low_id, direct_user_high_id) WHERE kind = 'direct';

CREATE INDEX conversations_tenant_updated ON conversations (tenant_id, updated_at DESC);

COMMIT;
